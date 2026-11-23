package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const (
	sessionCookie = "sid"
	sessionTTL    = 30 * 24 * time.Hour
)

type User struct {
	ID         int64  `json:"id"`
	Username   string `json:"username"`
	Name       string `json:"name"`
	Phone      string `json:"phone"`
	Email      string `json:"email"`
	Role       string `json:"role"`
	CampusID   int64  `json:"campus_id"` // 0 = all campuses (top-level admin)
	CampusName string `json:"campus_name"`
	Disabled   bool   `json:"disabled"`
}

// scope is the campus a staff member is limited to; 0 means every campus.
// A teacher or admin with a campus only sees and manages that campus. Parents are scoped by their children instead.
func scope(u *User) int64 {
	if u.Role == "parent" {
		return 0
	}
	return u.CampusID
}

// isTopAdmin: an admin not tied to one campus (the owner).
func isTopAdmin(u *User) bool { return u.Role == "admin" && u.CampusID == 0 }

type ctxKey struct{}

func currentUser(r *http.Request) *User { return r.Context().Value(ctxKey{}).(*User) }

// auth wraps a handler so it requires a logged-in user, optionally with one of the given roles.
// Requests that change data must also carry the X-Requested-With header, which a page on
// another website cannot add; together with SameSite cookies this blocks CSRF.
func (a *App) auth(h http.HandlerFunc, roles ...string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Header.Get("X-Requested-With") != "schoolapp" {
			httpError(w, http.StatusForbidden, "bad request origin")
			return
		}
		u, err := a.userFromRequest(r)
		if err != nil {
			httpError(w, http.StatusUnauthorized, "not logged in")
			return
		}
		if len(roles) > 0 {
			ok := false
			for _, role := range roles {
				if u.Role == role {
					ok = true
				}
			}
			if !ok {
				httpError(w, http.StatusForbidden, "permission denied")
				return
			}
		}
		h(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, u)))
	})
}

func (a *App) userFromRequest(r *http.Request) (*User, error) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return nil, err
	}
	u := &User{}
	err = a.db.QueryRow(`SELECT u.id, u.username, u.name, u.phone, u.email, u.role, COALESCE(u.campus_id,0), COALESCE(cp.name,'')
		FROM sessions s JOIN users u ON u.id = s.user_id LEFT JOIN campuses cp ON cp.id = u.campus_id
		WHERE s.token = ? AND s.expires_at > ? AND u.disabled = 0`,
		c.Value, time.Now().Unix()).Scan(&u.ID, &u.Username, &u.Name, &u.Phone, &u.Email, &u.Role, &u.CampusID, &u.CampusName)
	if err != nil {
		return nil, err
	}
	return u, nil
}

func (a *App) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-Requested-With") != "schoolapp" {
		httpError(w, http.StatusForbidden, "bad request origin")
		return
	}
	var req struct{ Username, Password string }
	if !readJSON(w, r, &req) {
		return
	}
	username := strings.ToLower(strings.TrimSpace(req.Username))
	if !a.limiter.allow(username) {
		httpError(w, http.StatusTooManyRequests, "too many attempts, try again in 15 minutes")
		return
	}
	var id int64
	var hash string
	var disabled bool
	err := a.db.QueryRow(`SELECT id, password_hash, disabled FROM users WHERE username = ?`, username).Scan(&id, &hash, &disabled)
	if err == nil {
		err = bcrypt.CompareHashAndPassword([]byte(hash), []byte(req.Password))
	}
	if err != nil {
		a.limiter.fail(username)
		httpError(w, http.StatusUnauthorized, "wrong username or password")
		return
	}
	a.limiter.reset(username)
	if disabled {
		httpError(w, http.StatusForbidden, "account disabled")
		return
	}
	token := randomToken(32)
	expires := time.Now().Add(sessionTTL)
	if _, err := a.db.Exec(`INSERT INTO sessions (token, user_id, expires_at) VALUES (?,?,?)`, token, id, expires.Unix()); err != nil {
		serverError(w, err)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: token, Path: "/", Expires: expires,
		HttpOnly: true, Secure: a.secure, SameSite: http.SameSiteLaxMode,
	})
	writeJSON(w, map[string]bool{"ok": true})
}

func (a *App) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		_, _ = a.db.Exec(`DELETE FROM sessions WHERE token = ?`, c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: a.secure, SameSite: http.SameSiteLaxMode})
	writeJSON(w, map[string]bool{"ok": true})
}

func (a *App) handleMe(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	resp := map[string]any{"user": u, "late_after": a.lateAfter, "today": a.now().Format("2006-01-02"), "currency": a.currency, "time_zone": a.loc.String(), "unread": a.unreadMessages(u.ID)}
	if u.Role == "parent" {
		children, err := a.queryStudents(`WHERE s.id IN (SELECT student_id FROM parent_students WHERE parent_id = ?)`, u.ID)
		if err != nil {
			serverError(w, err)
			return
		}
		resp["children"] = children
	}
	writeJSON(w, resp)
}

func (a *App) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Old string `json:"old"`
		New string `json:"new"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if len(req.New) < 6 {
		httpError(w, http.StatusBadRequest, "password must be at least 6 characters")
		return
	}
	u := currentUser(r)
	var hash string
	if err := a.db.QueryRow(`SELECT password_hash FROM users WHERE id = ?`, u.ID).Scan(&hash); err != nil {
		serverError(w, err)
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(req.Old)) != nil {
		httpError(w, http.StatusBadRequest, "current password is wrong")
		return
	}
	if err := a.setPassword(u.ID, req.New); err != nil {
		serverError(w, err)
		return
	}
	// Sign out other devices, keep this one.
	c, _ := r.Cookie(sessionCookie)
	_, _ = a.db.Exec(`DELETE FROM sessions WHERE user_id = ? AND token != ?`, u.ID, c.Value)
	writeJSON(w, map[string]bool{"ok": true})
}

func (a *App) setPassword(userID int64, pw string) error {
	h, err := hashPassword(pw)
	if err != nil {
		return err
	}
	res, err := a.db.Exec(`UPDATE users SET password_hash = ? WHERE id = ?`, h, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (a *App) cleanSessions() {
	for {
		_, _ = a.db.Exec(`DELETE FROM sessions WHERE expires_at < ?`, time.Now().Unix())
		_, _ = a.db.Exec(`DELETE FROM messages WHERE created_at < ?`, a.now().AddDate(-1, 0, 0).Format(time.RFC3339))
		_, _ = a.db.Exec(`DELETE FROM password_resets WHERE expires_at < ?`, time.Now().Add(-24*time.Hour).Unix())
		time.Sleep(time.Hour)
	}
}

// loginLimiter locks a username for 15 minutes after 8 failed attempts.
type loginLimiter struct {
	mu    sync.Mutex
	fails map[string]*failRecord
}

type failRecord struct {
	count int
	until time.Time
}

func newLoginLimiter() *loginLimiter { return &loginLimiter{fails: map[string]*failRecord{}} }

func (l *loginLimiter) allow(user string) bool { return l.allowN(user, 8) }

// allowN: at most max attempts per key within 15 minutes.
func (l *loginLimiter) allowN(key string, max int) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	f := l.fails[key]
	return f == nil || f.count < max || time.Now().After(f.until)
}

func (l *loginLimiter) fail(user string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	f := l.fails[user]
	if f == nil || time.Now().After(f.until) { // window over: start counting again
		f = &failRecord{}
		l.fails[user] = f
	}
	f.count++
	f.until = time.Now().Add(15 * time.Minute)
}

func (l *loginLimiter) reset(user string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.fails, user)
}

// ---- small JSON helpers ----

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		httpError(w, http.StatusBadRequest, "invalid request")
		return false
	}
	return true
}

func httpError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func serverError(w http.ResponseWriter, err error) {
	if errors.Is(err, sql.ErrNoRows) {
		httpError(w, http.StatusNotFound, "not found")
		return
	}
	logError(err)
	httpError(w, http.StatusInternalServerError, "server error")
}
