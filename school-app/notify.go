package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
)

// ---------- configuration ----------

// WhatsAppConfig holds the WhatsApp Business Cloud API settings.
// Token and phone number ID come from environment variables because they are secrets.
type WhatsAppConfig struct {
	Token        string // WHATSAPP_TOKEN
	PhoneID      string // WHATSAPP_PHONE_ID
	Template     string // approved message template name
	TemplateLang string // e.g. zh_CN, en
	CountryCode  string // added to local numbers like 012-3456789, e.g. 60
	APIBase      string // https://graph.facebook.com/v21.0 (overridable for testing)
}

func (c WhatsAppConfig) Enabled() bool { return c.Token != "" && c.PhoneID != "" && c.Template != "" }

type Notifier struct {
	db         *sql.DB
	vapidPub   string
	vapidPriv  string
	subscriber string
	wa         WhatsAppConfig
	client     *http.Client
}

func newNotifier(db *sql.DB, subscriber string, wa WhatsAppConfig) (*Notifier, error) {
	n := &Notifier{
		db:         db,
		subscriber: strings.TrimPrefix(subscriber, "mailto:"), // the library adds mailto: itself
		wa:         wa,
		client:     &http.Client{Timeout: 15 * time.Second},
	}
	// VAPID keys identify this server to browser push services. Create them once and keep them,
	// otherwise every existing phone subscription stops working.
	err := db.QueryRow(`SELECT value FROM settings WHERE key = 'vapid_private'`).Scan(&n.vapidPriv)
	if err == sql.ErrNoRows {
		priv, pub, gerr := webpush.GenerateVAPIDKeys()
		if gerr != nil {
			return nil, gerr
		}
		if _, err := db.Exec(`INSERT INTO settings (key, value) VALUES ('vapid_private', ?), ('vapid_public', ?)`, priv, pub); err != nil {
			return nil, err
		}
		n.vapidPriv = priv
	} else if err != nil {
		return nil, err
	}
	if err := db.QueryRow(`SELECT value FROM settings WHERE key = 'vapid_public'`).Scan(&n.vapidPub); err != nil {
		return nil, err
	}
	if wa.Enabled() {
		log.Printf("WhatsApp 通知已启用 / WhatsApp notifications enabled (template %q)", wa.Template)
	} else {
		log.Printf("WhatsApp 未设置，只发送 App 通知 / WhatsApp not configured; app push only")
	}
	return n, nil
}

func whatsAppFromEnv(template, lang, country string) WhatsAppConfig {
	base := os.Getenv("WHATSAPP_API_BASE")
	if base == "" {
		base = "https://graph.facebook.com/v21.0"
	}
	return WhatsAppConfig{
		Token:        os.Getenv("WHATSAPP_TOKEN"),
		PhoneID:      os.Getenv("WHATSAPP_PHONE_ID"),
		Template:     template,
		TemplateLang: lang,
		CountryCode:  strings.TrimPrefix(country, "+"),
		APIBase:      strings.TrimRight(base, "/"),
	}
}

// ---------- check-in notification ----------

type parentContact struct {
	ID       int64
	Phone    string
	Push     bool
	WhatsApp bool
}

// notifyCheckin tells every parent of the student that the child arrived. It runs in the
// background so the scanner gets its answer immediately.
func (n *Notifier) notifyCheckin(s Student, checkinAt string, late bool) {
	rows, err := n.db.Query(`SELECT u.id, u.phone, u.notify_push, u.notify_whatsapp FROM parent_students ps
		JOIN users u ON u.id = ps.parent_id WHERE ps.student_id = ?`, s.ID)
	if err != nil {
		logError(err)
		return
	}
	var parents []parentContact
	for rows.Next() {
		var p parentContact
		if err := rows.Scan(&p.ID, &p.Phone, &p.Push, &p.WhatsApp); err == nil {
			parents = append(parents, p)
		}
	}
	rows.Close()

	tm := hhmm(checkinAt)
	statusZh, statusEn := "准时", "on time"
	if late {
		statusZh, statusEn = "迟到", "late"
	}
	payload, _ := json.Marshal(map[string]string{
		"title": "到校通知 / Arrived at school",
		"body":  fmt.Sprintf("%s 已于 %s 到校（%s）\n%s arrived at %s (%s)", s.Name, tm, statusZh, s.Name, tm, statusEn),
		"url":   "/#home",
		"tag":   fmt.Sprintf("checkin-%d", s.ID),
	})
	status := statusZh
	if strings.HasPrefix(n.wa.TemplateLang, "en") {
		status = statusEn
	}
	for _, p := range parents {
		if p.Push {
			n.sendPushToUser(p.ID, s.ID, payload)
		}
		if p.WhatsApp && n.wa.Enabled() && p.Phone != "" {
			err := n.sendWhatsApp(p.Phone, []string{s.Name, tm, status})
			n.record(p.ID, s.ID, "whatsapp", p.Phone, err)
		}
	}
}

func hhmm(rfc3339 string) string {
	if t, err := time.Parse(time.RFC3339, rfc3339); err == nil {
		return t.Format("15:04")
	}
	return rfc3339
}

func (n *Notifier) record(userID, studentID int64, channel, target string, err error) {
	ok, detail := 1, target
	if err != nil {
		ok, detail = 0, target+": "+err.Error()
		log.Printf("notify %s to user %d failed: %v", channel, userID, err)
	}
	if len(detail) > 500 {
		detail = detail[:500]
	}
	_, _ = n.db.Exec(`INSERT INTO notification_log (user_id, student_id, channel, ok, detail, created_at)
		VALUES (?,?,?,?,?,?)`, userID, nullInt(studentID), channel, ok, detail, time.Now().Format(time.RFC3339))
}

// ---------- web push ----------

func (n *Notifier) sendPushToUser(userID, studentID int64, payload []byte) {
	rows, err := n.db.Query(`SELECT id, endpoint, p256dh, auth FROM push_subscriptions WHERE user_id = ?`, userID)
	if err != nil {
		logError(err)
		return
	}
	type sub struct {
		id int64
		s  webpush.Subscription
	}
	var subs []sub
	for rows.Next() {
		var x sub
		if err := rows.Scan(&x.id, &x.s.Endpoint, &x.s.Keys.P256dh, &x.s.Keys.Auth); err == nil {
			subs = append(subs, x)
		}
	}
	rows.Close()

	for _, x := range subs {
		resp, err := webpush.SendNotification(payload, &x.s, &webpush.Options{
			HTTPClient:      n.client,
			Subscriber:      n.subscriber,
			VAPIDPublicKey:  n.vapidPub,
			VAPIDPrivateKey: n.vapidPriv,
			TTL:             6 * 3600,
			Urgency:         webpush.UrgencyHigh,
		})
		if err == nil {
			io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			switch {
			case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
				// The phone unsubscribed or the app was removed: forget this device.
				_, _ = n.db.Exec(`DELETE FROM push_subscriptions WHERE id = ?`, x.id)
				err = fmt.Errorf("device no longer subscribed (removed)")
			case resp.StatusCode >= 300:
				err = fmt.Errorf("push service returned %s", resp.Status)
			}
		}
		n.record(userID, studentID, "push", hostOf(x.s.Endpoint), err)
	}
}

func hostOf(endpoint string) string {
	if u, err := url.Parse(endpoint); err == nil && u.Host != "" {
		return u.Host
	}
	return "?"
}

// ---------- WhatsApp ----------

// normalizePhone turns "012-345 6789" into "60123456789" (international format, digits only).
func normalizePhone(phone, country string) string {
	var b strings.Builder
	for _, r := range phone {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	d := b.String()
	switch {
	case strings.HasPrefix(strings.TrimSpace(phone), "+"):
		return d
	case strings.HasPrefix(d, "00"):
		return d[2:]
	case strings.HasPrefix(d, "0") && country != "":
		return country + d[1:]
	default:
		return d
	}
}

func (n *Notifier) sendWhatsApp(phone string, params []string) error {
	to := normalizePhone(phone, n.wa.CountryCode)
	if len(to) < 8 {
		return fmt.Errorf("invalid phone number %q", phone)
	}
	var bodyParams []map[string]string
	for _, p := range params {
		bodyParams = append(bodyParams, map[string]string{"type": "text", "text": p})
	}
	msg := map[string]any{
		"messaging_product": "whatsapp",
		"to":                to,
		"type":              "template",
		"template": map[string]any{
			"name":       n.wa.Template,
			"language":   map[string]string{"code": n.wa.TemplateLang},
			"components": []any{map[string]any{"type": "body", "parameters": bodyParams}},
		},
	}
	body, _ := json.Marshal(msg)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.wa.APIBase+"/"+n.wa.PhoneID+"/messages", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+n.wa.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := n.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode >= 300 {
		var e struct {
			Error struct {
				Message string `json:"message"`
				Code    int    `json:"code"`
			} `json:"error"`
		}
		if json.Unmarshal(respBody, &e) == nil && e.Error.Message != "" {
			return fmt.Errorf("WhatsApp error %d: %s", e.Error.Code, e.Error.Message)
		}
		return fmt.Errorf("WhatsApp returned %s", resp.Status)
	}
	return nil
}

// ---------- HTTP handlers ----------

func (a *App) handlePushKey(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]string{"key": a.notifier.vapidPub})
}

type pushSubReq struct {
	Endpoint string `json:"endpoint"`
	Keys     struct {
		P256dh string `json:"p256dh"`
		Auth   string `json:"auth"`
	} `json:"keys"`
}

func (a *App) handlePushSubscribe(w http.ResponseWriter, r *http.Request) {
	var req pushSubReq
	if !readJSON(w, r, &req) {
		return
	}
	if !strings.HasPrefix(req.Endpoint, "https://") || req.Keys.P256dh == "" || req.Keys.Auth == "" || len(req.Endpoint) > 1000 {
		httpError(w, http.StatusBadRequest, "invalid subscription")
		return
	}
	// One device = one endpoint. If someone else logged in on this phone before, it now belongs to this user.
	_, err := a.db.Exec(`INSERT INTO push_subscriptions (user_id, endpoint, p256dh, auth, created_at) VALUES (?,?,?,?,?)
		ON CONFLICT(endpoint) DO UPDATE SET user_id = excluded.user_id, p256dh = excluded.p256dh, auth = excluded.auth`,
		currentUser(r).ID, req.Endpoint, req.Keys.P256dh, req.Keys.Auth, a.nowStr())
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (a *App) handlePushUnsubscribe(w http.ResponseWriter, r *http.Request) {
	var req pushSubReq
	if !readJSON(w, r, &req) {
		return
	}
	_, _ = a.db.Exec(`DELETE FROM push_subscriptions WHERE endpoint = ? AND user_id = ?`, req.Endpoint, currentUser(r).ID)
	writeJSON(w, map[string]bool{"ok": true})
}

func (a *App) handleNotifySettings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Push     bool `json:"push"`
		WhatsApp bool `json:"whatsapp"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if _, err := a.db.Exec(`UPDATE users SET notify_push = ?, notify_whatsapp = ? WHERE id = ?`, req.Push, req.WhatsApp, currentUser(r).ID); err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// handlePushTest sends a test notification to the current user's devices.
func (a *App) handlePushTest(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	var n int
	_ = a.db.QueryRow(`SELECT COUNT(*) FROM push_subscriptions WHERE user_id = ?`, u.ID).Scan(&n)
	if n == 0 {
		httpError(w, http.StatusBadRequest, "notifications not enabled on any device")
		return
	}
	payload, _ := json.Marshal(map[string]string{
		"title": "测试通知 / Test", "body": "App 通知设置成功 ✓ Notifications are working", "url": "/#me", "tag": "test",
	})
	a.notifier.sendPushToUser(u.ID, 0, payload)
	writeJSON(w, map[string]int{"devices": n})
}

// handleWhatsAppTest lets the admin check the WhatsApp setup by sending the template to a number.
func (a *App) handleWhatsAppTest(w http.ResponseWriter, r *http.Request) {
	if !a.notifier.wa.Enabled() {
		httpError(w, http.StatusBadRequest, "WhatsApp is not configured on the server")
		return
	}
	var req struct{ Phone string }
	if !readJSON(w, r, &req) {
		return
	}
	status := "准时"
	if strings.HasPrefix(a.notifier.wa.TemplateLang, "en") {
		status = "on time"
	}
	err := a.notifier.sendWhatsApp(req.Phone, []string{"测试学生 Test", a.now().Format("15:04"), status})
	a.notifier.record(currentUser(r).ID, 0, "whatsapp", req.Phone+" (test)", err)
	if err != nil {
		httpError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (a *App) handleNotificationLog(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.Query(`SELECT l.id, COALESCE(u.name,''), COALESCE(s.name,''), l.channel, l.ok, l.detail, l.created_at
		FROM notification_log l LEFT JOIN users u ON u.id = l.user_id LEFT JOIN students s ON s.id = l.student_id
		ORDER BY l.id DESC LIMIT 200`)
	if err != nil {
		serverError(w, err)
		return
	}
	defer rows.Close()
	type entry struct {
		ID        int64  `json:"id"`
		User      string `json:"user"`
		Student   string `json:"student"`
		Channel   string `json:"channel"`
		OK        bool   `json:"ok"`
		Detail    string `json:"detail"`
		CreatedAt string `json:"created_at"`
	}
	list := []entry{}
	for rows.Next() {
		var e entry
		if err := rows.Scan(&e.ID, &e.User, &e.Student, &e.Channel, &e.OK, &e.Detail, &e.CreatedAt); err != nil {
			serverError(w, err)
			return
		}
		list = append(list, e)
	}
	writeJSON(w, map[string]any{"whatsapp_enabled": a.notifier.wa.Enabled(), "entries": list})
}
