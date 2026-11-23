// School app: QR check-in, teacher announcements, and tuition receipt uploads.
package main

import (
	"database/sql"
	"embed"
	"flag"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

//go:embed web
var webFiles embed.FS

type App struct {
	db        *sql.DB
	uploadDir string
	loc       *time.Location
	lateAfter string // "HH:MM" local time; check-ins after this are marked late
	secure    bool   // set the Secure flag on cookies (when served over HTTPS)
	limiter   *loginLimiter
	currency  string // shown next to amounts, e.g. "RM"
	notifier  *Notifier
}

func (a *App) now() time.Time { return time.Now().In(a.loc) }

func (a *App) nowStr() string { return a.now().Format(time.RFC3339) }

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	dataDir := flag.String("data", "./data", "folder for the database and uploaded files")
	tz := flag.String("tz", "Asia/Kuala_Lumpur", "school time zone (IANA name)")
	late := flag.String("late", "07:30", "check-ins after this time (HH:MM) are marked late")
	currency := flag.String("currency", "RM", "currency label shown next to amounts")
	pushContact := flag.String("push-contact", "admin@example.com", "school e-mail or https URL sent to push services as a contact")
	demo := flag.Bool("demo", false, "add sample classes, students and accounts to an empty database")
	certFile := flag.String("tls-cert", "", "TLS certificate file (enables HTTPS)")
	keyFile := flag.String("tls-key", "", "TLS key file")
	behindHTTPS := flag.Bool("behind-https", false, "set when a reverse proxy (e.g. Caddy, Nginx) serves HTTPS in front of this app")
	flag.Parse()

	if _, err := time.Parse("15:04", *late); err != nil {
		log.Fatalf("invalid -late %q: use HH:MM", *late)
	}
	loc, err := time.LoadLocation(*tz)
	if err != nil {
		log.Fatalf("invalid -tz %q: %v", *tz, err)
	}
	uploadDir := filepath.Join(*dataDir, "uploads")
	if err := os.MkdirAll(uploadDir, 0o750); err != nil {
		log.Fatal(err)
	}
	db, err := openDB(filepath.Join(*dataDir, "school.db"))
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	app := &App{
		db: db, uploadDir: uploadDir, loc: loc, lateAfter: *late,
		secure:  *certFile != "" || *behindHTTPS,
		limiter: newLoginLimiter(), currency: *currency,
	}
	if err := ensureAdmin(db, os.Getenv("ADMIN_PASSWORD")); err != nil {
		log.Fatal(err)
	}
	app.notifier, err = newNotifier(db, *pushContact)
	if err != nil {
		log.Fatal(err)
	}
	if *demo {
		if err := seedDemo(app); err != nil {
			log.Fatal(err)
		}
	}
	go app.cleanSessions()

	srv := &http.Server{
		Addr:              *addr,
		Handler:           securityHeaders(app.routes()),
		ReadHeaderTimeout: 10 * time.Second,
	}
	if *certFile != "" {
		log.Printf("listening on https://%s", *addr)
		log.Fatal(srv.ListenAndServeTLS(*certFile, *keyFile))
	}
	log.Printf("listening on http://%s", *addr)
	log.Fatal(srv.ListenAndServe())
}

func (a *App) routes() http.Handler {
	mux := http.NewServeMux()

	static, _ := fs.Sub(webFiles, "web")
	mux.Handle("GET /", http.FileServer(http.FS(static)))

	// Accounts
	mux.HandleFunc("POST /api/login", a.handleLogin)
	mux.HandleFunc("POST /api/logout", a.handleLogout)
	mux.Handle("GET /api/me", a.auth(a.handleMe))
	mux.Handle("POST /api/me/password", a.auth(a.handleChangePassword))

	// School setup (admin)
	mux.Handle("GET /api/classes", a.auth(a.handleListClasses, "admin", "teacher"))
	mux.Handle("POST /api/classes", a.auth(a.handleCreateClass, "admin"))
	mux.Handle("DELETE /api/classes/{id}", a.auth(a.handleDeleteClass, "admin"))
	mux.Handle("GET /api/users", a.auth(a.handleListUsers, "admin"))
	mux.Handle("POST /api/users", a.auth(a.handleCreateUser, "admin"))
	mux.Handle("DELETE /api/users/{id}", a.auth(a.handleDeleteUser, "admin"))
	mux.Handle("POST /api/users/{id}/password", a.auth(a.handleResetPassword, "admin"))
	mux.Handle("GET /api/students", a.auth(a.handleListStudents, "admin", "teacher"))
	mux.Handle("POST /api/students", a.auth(a.handleCreateStudent, "admin"))
	mux.Handle("DELETE /api/students/{id}", a.auth(a.handleDeleteStudent, "admin"))
	mux.Handle("POST /api/students/{id}/parents", a.auth(a.handleLinkParent, "admin"))
	mux.Handle("DELETE /api/students/{id}/parents/{pid}", a.auth(a.handleUnlinkParent, "admin"))
	mux.Handle("POST /api/students/{id}/qr/reset", a.auth(a.handleResetQR, "admin"))
	mux.Handle("GET /api/students/{id}/qr.png", a.auth(a.handleStudentQR, "admin", "teacher"))

	// Attendance
	mux.Handle("POST /api/attendance/scan", a.auth(a.handleScan, "admin", "teacher"))
	mux.Handle("POST /api/attendance/manual", a.auth(a.handleManualCheckin, "admin", "teacher"))
	mux.Handle("GET /api/attendance", a.auth(a.handleDailyAttendance, "admin", "teacher"))
	mux.Handle("GET /api/attendance/student/{id}", a.auth(a.handleStudentAttendance))

	// Announcements
	mux.Handle("GET /api/announcements", a.auth(a.handleListAnnouncements))
	mux.Handle("POST /api/announcements", a.auth(a.handleCreateAnnouncement, "admin", "teacher"))
	mux.Handle("DELETE /api/announcements/{id}", a.auth(a.handleDeleteAnnouncement, "admin", "teacher"))

	// Notifications
	mux.Handle("GET /api/push/key", a.auth(a.handlePushKey))
	mux.Handle("POST /api/push/subscribe", a.auth(a.handlePushSubscribe))
	mux.Handle("POST /api/push/unsubscribe", a.auth(a.handlePushUnsubscribe))
	mux.Handle("POST /api/push/test", a.auth(a.handlePushTest))
	mux.Handle("GET /api/notifications/log", a.auth(a.handleNotificationLog, "admin"))

	// Student photos (parents only see photos of their own children)
	mux.Handle("GET /api/photos", a.auth(a.handleListPhotos))
	mux.Handle("POST /api/photos", a.auth(a.handleUploadPhotos, "admin", "teacher"))
	mux.Handle("GET /api/photos/{id}/image", a.auth(a.handlePhotoImage))
	mux.Handle("DELETE /api/photos/{id}", a.auth(a.handleDeletePhoto, "admin", "teacher"))

	// Tuition receipts
	mux.Handle("GET /api/receipts", a.auth(a.handleListReceipts, "admin", "parent"))
	mux.Handle("POST /api/receipts", a.auth(a.handleUploadReceipt, "parent"))
	mux.Handle("GET /api/receipts/{id}/file", a.auth(a.handleReceiptFile, "admin", "parent"))
	mux.Handle("POST /api/receipts/{id}/review", a.auth(a.handleReviewReceipt, "admin"))

	return mux
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' blob: data:; media-src 'self' blob:; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Permissions-Policy", "camera=(self), microphone=(), geolocation=()")
		next.ServeHTTP(w, r)
	})
}
