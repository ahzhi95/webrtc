package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
)

// Notifier sends Web Push notifications to parents' phones (Android, iPhone home-screen app, desktop).
type Notifier struct {
	db         *sql.DB
	vapidPub   string
	vapidPriv  string
	subscriber string
	client     *http.Client
}

func newNotifier(db *sql.DB, subscriber string) (*Notifier, error) {
	n := &Notifier{
		db:         db,
		subscriber: strings.TrimPrefix(subscriber, "mailto:"), // the library adds mailto: itself
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
	return n, nil
}

// notifyAttendance tells the student's parents about an arrival or departure. It runs in the
// background so the scanner or roll call gets its answer immediately.
func (n *Notifier) notifyAttendance(s Student, mode string, at time.Time, late bool) {
	tm := at.Format("15:04")
	var body string
	if mode == modeIn {
		statusZh, statusEn := "准时", "on time"
		if late {
			statusZh, statusEn = "迟到", "late"
		}
		body = fmt.Sprintf("%s 已于 %s 到园（%s）\n%s arrived at %s (%s)", s.Name, tm, statusZh, s.Name, tm, statusEn)
	} else {
		body = fmt.Sprintf("%s 已于 %s 离园\n%s left at %s", s.Name, tm, s.Name, tm)
	}
	title := map[string]string{modeIn: "到园通知 / Arrived", modeOut: "离园通知 / Left school"}[mode]
	n.notifyParents([]int64{s.ID}, title, func(string) string { return body }, "/#home", fmt.Sprintf("att-%s-%d", mode, s.ID))
}

// notifyParents sends one push to each parent of the given students. body receives the names of
// that parent's own children among them, so nobody learns about other families.
func (n *Notifier) notifyParents(studentIDs []int64, title string, body func(childNames string) string, url, tag string) {
	names := map[int64][]string{}
	var order []int64
	firstStudent := map[int64]int64{}
	for _, sid := range studentIDs {
		rows, err := n.db.Query(`SELECT u.id, s.name FROM parent_students ps JOIN users u ON u.id = ps.parent_id
			JOIN students s ON s.id = ps.student_id WHERE ps.student_id = ? AND u.notify_push = 1 AND u.disabled = 0`, sid)
		if err != nil {
			logError(err)
			return
		}
		for rows.Next() {
			var pid int64
			var name string
			if rows.Scan(&pid, &name) == nil {
				if _, seen := names[pid]; !seen {
					order = append(order, pid)
					firstStudent[pid] = sid
				}
				names[pid] = append(names[pid], name)
			}
		}
		rows.Close()
	}
	for _, pid := range order {
		payload, _ := json.Marshal(map[string]string{"title": title, "body": body(strings.Join(names[pid], "、")), "url": url, "tag": tag})
		sid := firstStudent[pid]
		if len(studentIDs) > 1 {
			sid = 0
		}
		n.sendPushToUser(pid, sid, payload)
	}
}

func hhmm(rfc3339 string) string {
	if t, err := time.Parse(time.RFC3339, rfc3339); err == nil {
		return t.Format("15:04")
	}
	return rfc3339
}

func (n *Notifier) record(userID, studentID int64, target string, err error) {
	ok, detail := 1, target
	if err != nil {
		ok, detail = 0, target+": "+err.Error()
		log.Printf("push to user %d failed: %v", userID, err)
	}
	if len(detail) > 500 {
		detail = detail[:500]
	}
	_, _ = n.db.Exec(`INSERT INTO notification_log (user_id, student_id, channel, ok, detail, created_at)
		VALUES (?,?,'push',?,?,?)`, userID, nullInt(studentID), ok, detail, time.Now().Format(time.RFC3339))
}

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
		n.record(userID, studentID, hostOf(x.s.Endpoint), err)
	}
}

func hostOf(endpoint string) string {
	if u, err := url.Parse(endpoint); err == nil && u.Host != "" {
		return u.Host
	}
	return "?"
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

func (a *App) handleNotificationLog(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.Query(`SELECT l.id, COALESCE(u.name,''), COALESCE(s.name,''), l.ok, l.detail, l.created_at
		FROM notification_log l LEFT JOIN users u ON u.id = l.user_id LEFT JOIN students s ON s.id = l.student_id
		WHERE l.channel = 'push' AND (? = 0 OR l.student_id IN (SELECT st.id FROM students st JOIN classes c ON c.id = st.class_id WHERE c.campus_id = ?))
		ORDER BY l.id DESC LIMIT 200`, scope(currentUser(r)), scope(currentUser(r)))
	if err != nil {
		serverError(w, err)
		return
	}
	defer rows.Close()
	type entry struct {
		ID        int64  `json:"id"`
		User      string `json:"user"`
		Student   string `json:"student"`
		OK        bool   `json:"ok"`
		Detail    string `json:"detail"`
		CreatedAt string `json:"created_at"`
	}
	list := []entry{}
	for rows.Next() {
		var e entry
		if err := rows.Scan(&e.ID, &e.User, &e.Student, &e.OK, &e.Detail, &e.CreatedAt); err != nil {
			serverError(w, err)
			return
		}
		list = append(list, e)
	}
	var devices int
	_ = a.db.QueryRow(`SELECT COUNT(DISTINCT user_id) FROM push_subscriptions`).Scan(&devices)
	writeJSON(w, map[string]any{"users_with_push": devices, "entries": list})
}
