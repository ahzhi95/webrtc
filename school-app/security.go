package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	qrcode "github.com/skip2/go-qrcode"
	"golang.org/x/crypto/bcrypt"
)

// Security features:
//   - sessions are stored as hashes, so a copied database cannot be used to log in
//   - session lifetime depends on the role (admins log in again more often)
//   - password rules: at least 8 characters, not a common password, not the username or phone number
//   - two-step verification (TOTP, RFC 6238) with one-time backup codes, for any account (recommended for admins)
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

// ---------- two-step verification (TOTP) ----------

const totpPeriod = 30

func totpCode(secret []byte, counter uint64) string {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], counter)
	mac := hmac.New(sha1.New, secret)
	mac.Write(msg[:])
	sum := mac.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	v := (uint32(sum[off])&0x7f)<<24 | uint32(sum[off+1])<<16 | uint32(sum[off+2])<<8 | uint32(sum[off+3])
	return fmt.Sprintf("%06d", v%1000000)
}

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// verifyTOTP accepts the current code and one period either side (clock drift), and refuses a
// code that was already used, so an overheard code cannot be replayed.
func (a *App) verifyTOTP(userID int64, code string) bool {
	code = strings.ReplaceAll(strings.TrimSpace(code), " ", "")
	if len(code) != 6 {
		return false
	}
	var secret string
	var last int64
	if a.db.QueryRow(`SELECT totp_secret, totp_last FROM users WHERE id = ?`, userID).Scan(&secret, &last) != nil || secret == "" {
		return false
	}
	key, err := b32.DecodeString(secret)
	if err != nil {
		return false
	}
	now := time.Now().Unix() / totpPeriod
	for _, c := range []int64{now - 1, now, now + 1} {
		if c > last && subtle.ConstantTimeCompare([]byte(totpCode(key, uint64(c))), []byte(code)) == 1 {
			_, _ = a.db.Exec(`UPDATE users SET totp_last = ? WHERE id = ?`, c, userID)
			return true
		}
	}
	return false
}

// useRecoveryCode spends one of the user's backup codes.
func (a *App) useRecoveryCode(userID int64, code string) bool {
	code = strings.ToUpper(strings.ReplaceAll(strings.ReplaceAll(strings.TrimSpace(code), "-", ""), " ", ""))
	if len(code) != 10 {
		return false
	}
	res, err := a.db.Exec(`UPDATE recovery_codes SET used_at = ? WHERE user_id = ? AND code_hash = ? AND used_at = ''`, a.nowStr(), userID, tokenHash(code))
	if err != nil {
		return false
	}
	n, _ := res.RowsAffected()
	return n == 1
}

func newRecoveryCodes() []string {
	const alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	codes := make([]string, 8)
	for i := range codes {
		b := make([]byte, 10)
		_, _ = rand.Read(b)
		for j := range b {
			b[j] = alphabet[int(b[j])%len(alphabet)]
		}
		codes[i] = string(b[:5]) + "-" + string(b[5:])
	}
	return codes
}

// handle2FASetup creates a new secret (not active yet) and returns it with a QR code for the authenticator app.
func (a *App) handle2FASetup(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	var enabled bool
	_ = a.db.QueryRow(`SELECT totp_enabled FROM users WHERE id = ?`, u.ID).Scan(&enabled)
	if enabled {
		httpError(w, http.StatusConflict, "two-step verification is already on")
		return
	}
	key := make([]byte, 20)
	_, _ = rand.Read(key)
	secret := b32.EncodeToString(key)
	if _, err := a.db.Exec(`UPDATE users SET totp_secret = ?, totp_last = 0 WHERE id = ?`, secret, u.ID); err != nil {
		serverError(w, err)
		return
	}
	label := url.PathEscape("Lovely House:" + u.Username)
	uri := fmt.Sprintf("otpauth://totp/%s?secret=%s&issuer=%s&algorithm=SHA1&digits=6&period=30", label, secret, url.QueryEscape("Lovely House"))
	png, err := qrcode.Encode(uri, qrcode.Medium, 320)
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, map[string]string{"secret": secret, "uri": uri, "qr": "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)})
}

// handle2FAEnable turns it on after the first correct code, and returns backup codes (shown once).
func (a *App) handle2FAEnable(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	var req struct{ Code string }
	if !readJSON(w, r, &req) {
		return
	}
	if !a.verifyTOTP(u.ID, req.Code) {
		httpError(w, http.StatusBadRequest, "wrong code")
		return
	}
	codes := newRecoveryCodes()
	tx, err := a.db.Begin()
	if err != nil {
		serverError(w, err)
		return
	}
	defer tx.Rollback()
	_, _ = tx.Exec(`DELETE FROM recovery_codes WHERE user_id = ?`, u.ID)
	for _, c := range codes {
		if _, err := tx.Exec(`INSERT INTO recovery_codes (user_id, code_hash) VALUES (?,?)`, u.ID, tokenHash(strings.ReplaceAll(c, "-", ""))); err != nil {
			serverError(w, err)
			return
		}
	}
	if _, err := tx.Exec(`UPDATE users SET totp_enabled = 1 WHERE id = ?`, u.ID); err != nil {
		serverError(w, err)
		return
	}
	if err := tx.Commit(); err != nil {
		serverError(w, err)
		return
	}
	a.audit(r, u.ID, u.Username, "2fa_on", "")
	writeJSON(w, map[string]any{"recovery_codes": codes})
}

func (a *App) handle2FADisable(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	var req struct{ Password, Code string }
	if !readJSON(w, r, &req) {
		return
	}
	var hash string
	_ = a.db.QueryRow(`SELECT password_hash FROM users WHERE id = ?`, u.ID).Scan(&hash)
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(req.Password)) != nil || !(a.verifyTOTP(u.ID, req.Code) || a.useRecoveryCode(u.ID, req.Code)) {
		httpError(w, http.StatusBadRequest, "wrong password or code")
		return
	}
	a.turnOff2FA(u.ID)
	a.audit(r, u.ID, u.Username, "2fa_off", "")
	okJSON(w)
}

func (a *App) turnOff2FA(userID int64) {
	_, _ = a.db.Exec(`UPDATE users SET totp_enabled = 0, totp_secret = '', totp_last = 0 WHERE id = ?`, userID)
	_, _ = a.db.Exec(`DELETE FROM recovery_codes WHERE user_id = ?`, userID)
}

// handleReset2FA: an admin turns off a lost authenticator for someone they manage (after checking who it is).
func (a *App) handleReset2FA(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	id, ok := pathID(r, "id")
	if !ok || !a.userInScope(u, id) || id == u.ID {
		httpError(w, http.StatusForbidden, "permission denied")
		return
	}
	var username string
	_ = a.db.QueryRow(`SELECT username FROM users WHERE id = ?`, id).Scan(&username)
	a.turnOff2FA(id)
	_, _ = a.db.Exec(`DELETE FROM sessions WHERE user_id = ?`, id)
	a.audit(r, id, username, "2fa_reset", "by "+u.Username)
	okJSON(w)
}

func (a *App) twoFactorStatus(userID int64) (enabled bool, codesLeft int) {
	_ = a.db.QueryRow(`SELECT totp_enabled FROM users WHERE id = ?`, userID).Scan(&enabled)
	_ = a.db.QueryRow(`SELECT COUNT(*) FROM recovery_codes WHERE user_id = ? AND used_at = ''`, userID).Scan(&codesLeft)
	return
}

// Pending second steps of sign-in: password accepted, code still needed (5 minutes, 5 tries).
type pendingLogin struct {
	userID   int64
	username string
	role     string
	expires  time.Time
	tries    int
}

type loginChallenges struct {
	mu sync.Mutex
	m  map[string]*pendingLogin
}

func (c *loginChallenges) add(p *pendingLogin) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	for k, v := range c.m {
		if now.After(v.expires) {
			delete(c.m, k)
		}
	}
	id := randomToken(24)
	c.m[id] = p
	return id
}

func (c *loginChallenges) get(id string) *pendingLogin {
	c.mu.Lock()
	defer c.mu.Unlock()
	p := c.m[id]
	if p == nil || time.Now().After(p.expires) || p.tries >= 5 {
		delete(c.m, id)
		return nil
	}
	p.tries++
	return p
}

func (c *loginChallenges) done(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.m, id)
}

func (a *App) handleLogin2FA(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-Requested-With") != "schoolapp" {
		httpError(w, http.StatusForbidden, "bad request origin")
		return
	}
	var req struct{ Challenge, Code string }
	if !readJSON(w, r, &req) {
		return
	}
	p := a.challenges.get(req.Challenge)
	if p == nil {
		httpError(w, http.StatusUnauthorized, "sign-in expired, please start again")
		return
	}
	viaBackup := false
	if !a.verifyTOTP(p.userID, req.Code) {
		if !a.useRecoveryCode(p.userID, req.Code) {
			a.audit(r, p.userID, p.username, "login_2fa_fail", "")
			httpError(w, http.StatusUnauthorized, "wrong code")
			return
		}
		viaBackup = true
	}
	a.challenges.done(req.Challenge)
	if err := a.startSession(w, p.userID, p.role); err != nil {
		serverError(w, err)
		return
	}
	detail := "2fa"
	if viaBackup {
		detail = "2fa backup code"
	}
	a.audit(r, p.userID, p.username, "login_ok", detail)
	okJSON(w)
}

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
