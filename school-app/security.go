package main

import (
	"net"
	"net/http"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// Security features:
//   - sessions are stored as hashes, so a copied database cannot be used to log in
//   - session lifetime depends on the role (admins log in again more often)
//   - password rules: at least 8 characters, not a common password, not the username or phone number
//   - a security log of sign-ins, failed attempts and account changes

// ---------- sessions ----------

// sessionTTL by role: parents stay signed in, staff for a month, admins for a week.
func sessionTTLFor(role string) time.Duration {
	switch role {
	case "admin":
		return 7 * 24 * time.Hour
	case "teacher":
		return 30 * 24 * time.Hour
	default:
		return 90 * 24 * time.Hour
	}
}

// cookieName: over HTTPS the "__Host-" prefix makes the browser refuse the cookie unless it is
// Secure, host-only and for the whole site.
func (a *App) cookieName() string {
	if a.secure {
		return "__Host-sid"
	}
	return "sid"
}

func (a *App) sessionToken(r *http.Request) string {
	for _, name := range []string{"__Host-sid", "sid"} {
		if c, err := r.Cookie(name); err == nil && c.Value != "" {
			return c.Value
		}
	}
	return ""
}

func (a *App) startSession(w http.ResponseWriter, userID int64, role string) error {
	token := randomToken(32)
	expires := time.Now().Add(sessionTTLFor(role))
	if _, err := a.db.Exec(`INSERT INTO sessions (token, user_id, expires_at) VALUES (?,?,?)`, tokenHash(token), userID, expires.Unix()); err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name: a.cookieName(), Value: token, Path: "/", Expires: expires,
		HttpOnly: true, Secure: a.secure, SameSite: http.SameSiteLaxMode,
	})
	return nil
}

func (a *App) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: a.cookieName(), Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: a.secure, SameSite: http.SameSiteLaxMode})
}

// ---------- passwords ----------

// A short list of passwords people often choose; NIST SP 800-63B asks to refuse such passwords.
var commonPasswords = map[string]bool{}

func init() {
	for _, p := range strings.Fields(`12345678 123456789 1234567890 87654321 11111111 00000000 88888888 66666666 12341234 11223344
		password password1 password123 passw0rd p@ssw0rd qwerty123 qwertyui qwertyuiop asdfghjk asdfasdf zxcvbnm1 1q2w3e4r
		1qaz2wsx abcd1234 abc12345 aa123456 a1234567 iloveyou admin123 admin1234 administrator welcome1 welcome123 letmein1
		sunshine princess football baseball monkey123 dragon123 master123 superman batman123 trustno1 changeme
		school123 teacher1 teacher123 parent123 student1 kindergarten tadika123 malaysia sayangkamu sayang123 woaini520
		5201314 woaini1314 a5201314 abcdefgh lovelyhouse lovely123`) {
		commonPasswords[p] = true
	}
}

// passwordProblem returns "" when the password is acceptable, otherwise an error message.
func passwordProblem(pw, username, phone string) string {
	lower := strings.ToLower(pw)
	switch {
	case len([]rune(pw)) < 8:
		return "password must be at least 8 characters"
	case len(pw) > 128:
		return "password is too long"
	case commonPasswords[lower]:
		return "password is too common"
	case username != "" && strings.Contains(lower, strings.ToLower(username)) && len(username) >= 3:
		return "password must not contain the username"
	case phone != "" && len(phoneKey(phone)) >= 6 && strings.Contains(pw, phoneKey(phone)[len(phoneKey(phone))-6:]):
		return "password must not contain your phone number"
	}
	same := true
	for _, c := range lower {
		if c != rune(lower[0]) {
			same = false
		}
	}
	if same {
		return "password is too common"
	}
	return ""
}

func (a *App) passwordProblemFor(userID int64, pw string) string {
	var username, phone string
	_ = a.db.QueryRow(`SELECT username, phone FROM users WHERE id = ?`, userID).Scan(&username, &phone)
	return passwordProblem(pw, username, phone)
}

// dummyHash makes a login for an unknown username take as long as a real one.
var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("not-a-real-password"), bcryptCost)

// ---------- security log ----------

func clientIP(r *http.Request) string {
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		ip = r.RemoteAddr
	}
	return ip
}

// audit records a security event. Only the network address and a short browser description are kept.
func (a *App) audit(r *http.Request, userID int64, username, event, detail string) {
	ua := r.UserAgent()
	if len(ua) > 160 {
		ua = ua[:160]
	}
	_, _ = a.db.Exec(`INSERT INTO security_events (user_id, username, event, ip, user_agent, detail, created_at) VALUES (?,?,?,?,?,?,?)`,
		nullInt(userID), username, event, clientIP(r), ua, detail, a.nowStr())
}

func (a *App) handleSecurityEvents(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	rows, err := a.db.Query(`SELECT e.id, COALESCE(e.user_id,0), e.username, COALESCE(us.name,''), e.event, e.ip, e.user_agent, e.detail, e.created_at
		FROM security_events e LEFT JOIN users us ON us.id = e.user_id ORDER BY e.id DESC LIMIT 500`)
	if err != nil {
		serverError(w, err)
		return
	}
	defer rows.Close()
	type ev struct {
		ID        int64  `json:"id"`
		UserID    int64  `json:"user_id"`
		Username  string `json:"username"`
		Name      string `json:"name"`
		Event     string `json:"event"`
		IP        string `json:"ip"`
		UserAgent string `json:"user_agent"`
		Detail    string `json:"detail"`
		CreatedAt string `json:"created_at"`
	}
	var all []ev
	for rows.Next() {
		var e ev
		if err := rows.Scan(&e.ID, &e.UserID, &e.Username, &e.Name, &e.Event, &e.IP, &e.UserAgent, &e.Detail, &e.CreatedAt); err != nil {
			serverError(w, err)
			return
		}
		all = append(all, e)
	}
	rows.Close()
	list := []ev{}
	for _, e := range all { // campus admins see events of the accounts they manage
		if scope(u) == 0 || (e.UserID != 0 && (e.UserID == u.ID || a.userInScope(u, e.UserID))) {
			list = append(list, e)
		}
		if len(list) >= 200 {
			break
		}
	}
	writeJSON(w, list)
}
