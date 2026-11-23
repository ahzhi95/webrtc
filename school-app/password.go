package main

import (
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/smtp"
	"os"
	"strings"
	"time"
)

// Forgot password (忘记密码).
// With an email address on the account and a mail server configured, the user gets a one-time link
// valid for 30 minutes (only a hash of the token is stored). Otherwise the request goes to the
// school's admins, who set a temporary password and pass it on by phone or WhatsApp.
// The response never says whether an account exists.

const resetTTL = 30 * time.Minute

type Mailer struct {
	Host, Port, User, Pass, From string
}

func mailerFromEnv() *Mailer {
	m := &Mailer{Host: os.Getenv("SMTP_HOST"), Port: os.Getenv("SMTP_PORT"), User: os.Getenv("SMTP_USERNAME"),
		Pass: os.Getenv("SMTP_PASSWORD"), From: os.Getenv("SMTP_FROM")}
	if m.Host == "" || m.From == "" {
		return nil
	}
	if m.Port == "" {
		m.Port = "587"
	}
	return m
}

func (m *Mailer) Send(to, subject, body string) error {
	msg := "From: " + m.From + "\r\nTo: " + to + "\r\nSubject: =?UTF-8?B?" + b64(subject) + "?=\r\n" +
		"MIME-Version: 1.0\r\nContent-Type: text/plain; charset=UTF-8\r\nContent-Transfer-Encoding: base64\r\n\r\n" + b64(body)
	addr := net.JoinHostPort(m.Host, m.Port)
	var auth smtp.Auth
	if m.User != "" {
		auth = smtp.PlainAuth("", m.User, m.Pass, m.Host)
	}
	from := m.From
	if i := strings.LastIndex(from, "<"); i >= 0 {
		from = strings.TrimSuffix(from[i+1:], ">")
	}
	if m.Port == "465" { // implicit TLS
		conn, err := tls.Dial("tcp", addr, &tls.Config{ServerName: m.Host})
		if err != nil {
			return err
		}
		c, err := smtp.NewClient(conn, m.Host)
		if err != nil {
			return err
		}
		defer c.Close()
		if auth != nil {
			if err := c.Auth(auth); err != nil {
				return err
			}
		}
		if err := c.Mail(from); err != nil {
			return err
		}
		if err := c.Rcpt(to); err != nil {
			return err
		}
		wc, err := c.Data()
		if err != nil {
			return err
		}
		if _, err := wc.Write([]byte(msg)); err != nil {
			return err
		}
		if err := wc.Close(); err != nil {
			return err
		}
		return c.Quit()
	}
	return smtp.SendMail(addr, auth, from, []string{to}, []byte(msg)) // uses STARTTLS when offered
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func tokenHash(t string) string {
	h := sha256.Sum256([]byte(t))
	return hex.EncodeToString(h[:])
}

// publicURL: where links in emails point. Set -public-url in production; otherwise taken from the request.
func (a *App) publicURL(r *http.Request) string {
	if a.baseURL != "" {
		return strings.TrimRight(a.baseURL, "/")
	}
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" || a.secure {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// findAccount looks an account up by username, phone number or email.
func (a *App) findAccount(id string) (userID int64, email string, ok bool) {
	id = strings.TrimSpace(id)
	if id == "" {
		return 0, "", false
	}
	err := a.db.QueryRow(`SELECT id, email FROM users WHERE username = ? AND disabled = 0`, strings.ToLower(id)).Scan(&userID, &email)
	if err == nil {
		return userID, email, true
	}
	if strings.Contains(id, "@") {
		if a.db.QueryRow(`SELECT id, email FROM users WHERE lower(email) = lower(?) AND disabled = 0 ORDER BY id LIMIT 1`, id).Scan(&userID, &email) == nil {
			return userID, email, true
		}
		return 0, "", false
	}
	key := phoneKey(id)
	if len(key) < 9 {
		return 0, "", false
	}
	rows, err := a.db.Query(`SELECT id, email, phone FROM users WHERE phone != '' AND disabled = 0`)
	if err != nil {
		return 0, "", false
	}
	defer rows.Close()
	for rows.Next() {
		var phone string
		if rows.Scan(&userID, &email, &phone) == nil && phoneKey(phone) == key {
			return userID, email, true
		}
	}
	return 0, "", false
}

func (a *App) handleForgotPassword(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-Requested-With") != "schoolapp" {
		httpError(w, http.StatusForbidden, "bad request origin")
		return
	}
	var req struct {
		Identifier string `json:"identifier"`
		Contact    string `json:"contact"` // how the school can reach them, optional
	}
	if !readJSON(w, r, &req) {
		return
	}
	// Limit attempts per identifier and per network address.
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	// A whole school may share one address on its Wi-Fi, so the per-address limit is higher.
	limits := map[string]int{"forgot:" + strings.ToLower(strings.TrimSpace(req.Identifier)): 8, "forgot-ip:" + ip: 30}
	for k, max := range limits {
		if !a.limiter.allowN(k, max) {
			httpError(w, http.StatusTooManyRequests, "too many attempts, try again in 15 minutes")
			return
		}
		a.limiter.fail(k) // every request counts
	}
	answer := map[string]bool{"ok": true}
	userID, email, found := a.findAccount(req.Identifier)
	if !found {
		writeJSON(w, answer) // same answer as success
		return
	}
	if email != "" && a.mailer != nil {
		token := randomToken(32)
		if _, err := a.db.Exec(`INSERT INTO password_resets (user_id, token_hash, expires_at, created_at) VALUES (?,?,?,?)`,
			userID, tokenHash(token), time.Now().Add(resetTTL).Unix(), a.nowStr()); err != nil {
			serverError(w, err)
			return
		}
		link := a.publicURL(r) + "/#reset=" + token
		body := "您好，\n\n请点下面的链接重设 Lovely House App 的密码（30 分钟内有效，只能用一次）：\n" + link +
			"\n\n如果不是您本人要求的，请忽略这封邮件，您的密码不会改变。\n\n" +
			"Hello,\n\nUse this link to reset your Lovely House app password (valid for 30 minutes, one use only):\n" + link +
			"\n\nIf you did not ask for this, ignore this email; your password stays the same.\n"
		go func() {
			if err := a.mailer.Send(email, "重设密码 / Reset your password", body); err != nil {
				log.Printf("reset email to user %d failed: %v", userID, err)
			}
		}()
		writeJSON(w, answer)
		return
	}
	// No email: ask the school. One open request per account is enough.
	var open int
	_ = a.db.QueryRow(`SELECT COUNT(*) FROM reset_requests WHERE user_id = ? AND status = 'open'`, userID).Scan(&open)
	if open == 0 {
		contact := strings.TrimSpace(req.Contact)
		if len(contact) > 100 {
			contact = contact[:100]
		}
		if _, err := a.db.Exec(`INSERT INTO reset_requests (user_id, contact, status, created_at) VALUES (?,?,'open',?)`,
			userID, contact, a.nowStr()); err != nil {
			serverError(w, err)
			return
		}
		go a.notifyResetRequest(userID)
	}
	writeJSON(w, answer)
}

// notifyResetRequest pushes "X asked for a password reset" to the admins who manage that account.
func (a *App) notifyResetRequest(userID int64) {
	var name string
	_ = a.db.QueryRow(`SELECT name FROM users WHERE id = ?`, userID).Scan(&name)
	rows, err := a.db.Query(`SELECT id, username, name, phone, email, role, COALESCE(campus_id,0) FROM users WHERE role = 'admin' AND disabled = 0`)
	if err != nil {
		logError(err)
		return
	}
	var admins []User
	for rows.Next() {
		var u User
		if rows.Scan(&u.ID, &u.Username, &u.Name, &u.Phone, &u.Email, &u.Role, &u.CampusID) == nil {
			admins = append(admins, u)
		}
	}
	rows.Close()
	for i := range admins {
		if a.userInScope(&admins[i], userID) {
			a.notifier.deliver(admins[i].ID, 0, "密码重设请求 / Password reset request",
				name+" 忘记密码，请协助重设。"+name+" forgot their password.", "/#overview", fmt.Sprintf("reset-%d", userID), "")
		}
	}
}

func (a *App) handleResetPasswordWithToken(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-Requested-With") != "schoolapp" {
		httpError(w, http.StatusForbidden, "bad request origin")
		return
	}
	var req struct{ Token, Password string }
	if !readJSON(w, r, &req) {
		return
	}
	if len(req.Password) < 6 {
		httpError(w, http.StatusBadRequest, "password must be at least 6 characters")
		return
	}
	var id, userID int64
	err := a.db.QueryRow(`SELECT id, user_id FROM password_resets WHERE token_hash = ? AND used_at = '' AND expires_at > ?`,
		tokenHash(req.Token), time.Now().Unix()).Scan(&id, &userID)
	if err != nil {
		httpError(w, http.StatusBadRequest, "link expired or invalid")
		return
	}
	if err := a.setPassword(userID, req.Password); err != nil {
		serverError(w, err)
		return
	}
	_, _ = a.db.Exec(`UPDATE password_resets SET used_at = ? WHERE user_id = ? AND used_at = ''`, a.nowStr(), userID) // all older links too
	_, _ = a.db.Exec(`DELETE FROM sessions WHERE user_id = ?`, userID)
	_, _ = a.db.Exec(`UPDATE reset_requests SET status = 'done', handled_at = ? WHERE user_id = ? AND status = 'open'`, a.nowStr(), userID)
	okJSON(w)
}

type ResetRequest struct {
	ID        int64  `json:"id"`
	UserID    int64  `json:"user_id"`
	Name      string `json:"name"`
	Username  string `json:"username"`
	Role      string `json:"role"`
	Phone     string `json:"phone"`
	Contact   string `json:"contact"`
	Children  string `json:"children"`
	CreatedAt string `json:"created_at"`
}

func (a *App) handleListResetRequests(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	rows, err := a.db.Query(`SELECT rr.id, us.id, us.name, us.username, us.role, us.phone, rr.contact, rr.created_at,
		COALESCE((SELECT group_concat(s.name, '、') FROM parent_students ps JOIN students s ON s.id = ps.student_id WHERE ps.parent_id = us.id), '')
		FROM reset_requests rr JOIN users us ON us.id = rr.user_id WHERE rr.status = 'open' ORDER BY rr.id`)
	if err != nil {
		serverError(w, err)
		return
	}
	var all []ResetRequest
	for rows.Next() {
		var x ResetRequest
		if err := rows.Scan(&x.ID, &x.UserID, &x.Name, &x.Username, &x.Role, &x.Phone, &x.Contact, &x.CreatedAt, &x.Children); err != nil {
			rows.Close()
			serverError(w, err)
			return
		}
		all = append(all, x)
	}
	rows.Close()
	list := []ResetRequest{}
	for _, x := range all {
		if a.userInScope(u, x.UserID) {
			list = append(list, x)
		}
	}
	writeJSON(w, list)
}

// handleResolveResetRequest: "reset" sets a random temporary password and returns it once; "dismiss" closes the request.
func (a *App) handleResolveResetRequest(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	id, ok := pathID(r, "id")
	var userID int64
	if !ok || a.db.QueryRow(`SELECT user_id FROM reset_requests WHERE id = ? AND status = 'open'`, id).Scan(&userID) != nil {
		httpError(w, http.StatusNotFound, "not found")
		return
	}
	if !a.userInScope(u, userID) {
		httpError(w, http.StatusForbidden, "permission denied")
		return
	}
	var req struct{ Action string }
	if !readJSON(w, r, &req) {
		return
	}
	resp := map[string]string{}
	switch req.Action {
	case "reset":
		pw := randomPassword()
		if err := a.setPassword(userID, pw); err != nil {
			serverError(w, err)
			return
		}
		_, _ = a.db.Exec(`DELETE FROM sessions WHERE user_id = ?`, userID)
		resp["password"] = pw
	case "dismiss":
	default:
		httpError(w, http.StatusBadRequest, "invalid request")
		return
	}
	if _, err := a.db.Exec(`UPDATE reset_requests SET status = ?, handled_by = ?, handled_at = ? WHERE user_id = ? AND status = 'open'`,
		map[string]string{"reset": "done", "dismiss": "dismissed"}[req.Action], u.ID, a.nowStr(), userID); err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, resp)
}

// handleUpdateMe lets everyone correct their own name, phone and email (e.g. to use email password resets).
func (a *App) handleUpdateMe(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	var req struct{ Name, Phone, Email string }
	if !readJSON(w, r, &req) {
		return
	}
	req.Name, req.Phone, req.Email = strings.TrimSpace(req.Name), strings.TrimSpace(req.Phone), strings.TrimSpace(req.Email)
	if req.Name == "" || len(req.Name) > 100 {
		httpError(w, http.StatusBadRequest, "name is required")
		return
	}
	if req.Email != "" && (!strings.Contains(req.Email, "@") || len(req.Email) > 200) {
		httpError(w, http.StatusBadRequest, "invalid email")
		return
	}
	if _, err := a.db.Exec(`UPDATE users SET name = ?, phone = ?, email = ? WHERE id = ?`, req.Name, req.Phone, req.Email, u.ID); err != nil {
		serverError(w, err)
		return
	}
	okJSON(w)
}
