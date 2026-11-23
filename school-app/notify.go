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
	"sync"
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
	loc        *time.Location // school time zone for stored timestamps

	mu      sync.Mutex
	pending map[string][]attEntry // "parentID/mode" → arrivals or departures waiting to be sent together
}

func (n *Notifier) nowStr() string {
	if n.loc == nil {
		return time.Now().Format(time.RFC3339)
	}
	return time.Now().In(n.loc).Format(time.RFC3339)
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

// Arrivals and departures are held for a few seconds and sent per parent, so brothers and sisters
// scanned together give one notification ("林小红、林小明 已到园") instead of several.
const attendanceBatchDelay = 15 * time.Second

type attEntry struct {
	studentID int64
	name, tm  string
	late      bool
}

// notifyAttendance tells the student's parents about an arrival or departure. It runs in the
// background so the scanner or roll call gets its answer immediately.
func (n *Notifier) notifyAttendance(s Student, mode string, at time.Time, late bool) {
	rows, err := n.db.Query(`SELECT u.id FROM parent_students ps JOIN users u ON u.id = ps.parent_id
		WHERE ps.student_id = ? AND u.disabled = 0`, s.ID)
	if err != nil {
		logError(err)
		return
	}
	var parents []int64
	for rows.Next() {
		var pid int64
		if rows.Scan(&pid) == nil {
			parents = append(parents, pid)
		}
	}
	rows.Close()
	e := attEntry{studentID: s.ID, name: s.Name, tm: at.Format("15:04"), late: late}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.pending == nil {
		n.pending = map[string][]attEntry{}
	}
	for _, pid := range parents {
		key := fmt.Sprintf("%d/%s", pid, mode)
		if _, waiting := n.pending[key]; !waiting {
			pid := pid
			time.AfterFunc(attendanceBatchDelay, func() { n.flushAttendance(key, pid, mode) })
		}
		n.pending[key] = append(n.pending[key], e)
	}
}

func (n *Notifier) flushAttendance(key string, parentID int64, mode string) {
	n.mu.Lock()
	list := n.pending[key]
	delete(n.pending, key)
	n.mu.Unlock()
	if len(list) == 0 {
		return
	}
	title, body := attendanceMessage(mode, list)
	sid, tag := list[0].studentID, fmt.Sprintf("att-%s-%d", mode, list[0].studentID)
	if len(list) > 1 {
		sid, tag = 0, fmt.Sprintf("att-%s-family-%d", mode, parentID)
	}
	n.deliver(parentID, sid, title, body, "/#home", tag, "")
}

// attendanceMessage: one child gives the usual sentence; several children are listed together,
// with one time when they all came (or left) at the same minute.
func attendanceMessage(mode string, list []attEntry) (title, body string) {
	title = map[string]string{modeIn: "到园通知 / Arrived", modeOut: "离园通知 / Left school"}[mode]
	status := func(e attEntry) (string, string) {
		if e.late {
			return "迟到", "late"
		}
		return "准时", "on time"
	}
	if len(list) == 1 {
		e := list[0]
		if mode == modeIn {
			zh, en := status(e)
			return title, fmt.Sprintf("%s 已于 %s 到园（%s）\n%s arrived at %s (%s)", e.name, e.tm, zh, e.name, e.tm, en)
		}
		return title, fmt.Sprintf("%s 已于 %s 离园\n%s left at %s", e.name, e.tm, e.name, e.tm)
	}
	same := true // same minute and, for arrivals, all on time or all late
	names := make([]string, len(list))
	for i, e := range list {
		names[i] = e.name
		same = same && e.tm == list[0].tm && (mode == modeOut || e.late == list[0].late)
	}
	if same {
		zhAll, enAll := strings.Join(names, "、"), strings.Join(names, ", ")
		if mode == modeIn {
			zh, en := status(list[0])
			return title, fmt.Sprintf("%s 已于 %s 到园（%s）\n%s arrived at %s (%s)", zhAll, list[0].tm, zh, enAll, list[0].tm, en)
		}
		return title, fmt.Sprintf("%s 已于 %s 离园\n%s left at %s", zhAll, list[0].tm, enAll, list[0].tm)
	}
	var zh, en []string
	for _, e := range list {
		if mode == modeIn {
			z, x := status(e)
			zh = append(zh, fmt.Sprintf("%s %s（%s）", e.name, e.tm, z))
			en = append(en, fmt.Sprintf("%s %s (%s)", e.name, e.tm, x))
		} else {
			zh = append(zh, e.name+" "+e.tm)
			en = append(en, e.name+" "+e.tm)
		}
	}
	verbZh, verbEn := "已到园", "arrived"
	if mode == modeOut {
		verbZh, verbEn = "已离园", "left school"
	}
	return title, strings.Join(zh, "、") + " " + verbZh + "\n" + strings.ToUpper(verbEn[:1]) + verbEn[1:] + ": " + strings.Join(en, ", ")
}

// notifyParents sends one push to each parent of the given students. body receives the names of
// that parent's own children among them, so nobody learns about other families.
func (n *Notifier) notifyParents(studentIDs []int64, title string, body func(childNames string) string, url, tag string) {
	names := map[int64][]string{}
	var order []int64
	firstStudent := map[int64]int64{}
	for _, sid := range studentIDs {
		rows, err := n.db.Query(`SELECT u.id, s.name FROM parent_students ps JOIN users u ON u.id = ps.parent_id
			JOIN students s ON s.id = ps.student_id WHERE ps.student_id = ? AND u.disabled = 0`, sid)
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
		sid := firstStudent[pid]
		if len(studentIDs) > 1 {
			sid = 0
		}
		n.deliver(pid, sid, title, body(strings.Join(names[pid], "、")), url, tag, "")
	}
}

// deliver puts a message in the user's message center (消息) and pushes it to their phones.
// ref ("notice:12") lets a message be withdrawn later, e.g. when a notice is retracted.
func (n *Notifier) deliver(userID, studentID int64, title, body, url, tag, ref string) {
	if _, err := n.db.Exec(`INSERT INTO messages (user_id, title, body, url, ref, created_at) VALUES (?,?,?,?,?,?)`,
		userID, title, body, url, ref, n.nowStr()); err != nil {
		logError(err)
	}
	var push bool
	if n.db.QueryRow(`SELECT notify_push FROM users WHERE id = ?`, userID).Scan(&push) != nil || !push {
		return
	}
	payload, _ := json.Marshal(map[string]string{"title": title, "body": body, "url": url, "tag": tag})
	n.sendPushToUser(userID, studentID, payload)
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
		VALUES (?,?,'push',?,?,?)`, userID, nullInt(studentID), ok, detail, n.nowStr())
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
