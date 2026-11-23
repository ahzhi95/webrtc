// School app for kindergartens with several campuses: arrival/departure, announcements, fees and receipts, photos.
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
	mailer    *Mailer // nil when no mail server is configured
	baseURL   string  // public address used in emailed links
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
	publicURL := flag.String("public-url", "", "public address of the app, e.g. https://school.example.com (used in password reset emails)")
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
		limiter: newLoginLimiter(), currency: *currency, mailer: mailerFromEnv(), baseURL: *publicURL,
	}
	if err := ensureAdmin(db, os.Getenv("ADMIN_PASSWORD")); err != nil {
		log.Fatal(err)
	}
	app.notifier, err = newNotifier(db, *pushContact)
	if err != nil {
		log.Fatal(err)
	}
	app.notifier.loc = loc
	if app.mailer != nil {
		log.Printf("邮件已设置 / Email configured (%s) — password reset links will be emailed", app.mailer.Host)
	} else {
		log.Printf("邮件未设置 / No email server — password reset requests go to the school admins")
	}
	if *demo {
		if err := seedDemo(app); err != nil {
			log.Fatal(err)
		}
	}
	go app.cleanSessions()
	go app.publishDue()

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
	mux.Handle("PUT /api/me", a.auth(a.handleUpdateMe))
	mux.Handle("GET /api/messages", a.auth(a.handleListMessages))
	mux.Handle("POST /api/messages/read", a.auth(a.handleReadMessages))
	mux.Handle("GET /api/messages/unread", a.auth(a.handleUnreadCount))
	mux.HandleFunc("POST /api/password/forgot", a.handleForgotPassword)
	mux.HandleFunc("POST /api/password/reset", a.handleResetPasswordWithToken)
	mux.Handle("GET /api/password/requests", a.auth(a.handleListResetRequests, "admin"))
	mux.Handle("POST /api/password/requests/{id}", a.auth(a.handleResolveResetRequest, "admin"))

	// School setup
	mux.Handle("GET /api/campuses", a.auth(a.handleListCampuses, "admin", "teacher"))
	mux.Handle("POST /api/campuses", a.auth(a.handleSaveCampus, "admin"))
	mux.Handle("PUT /api/campuses/{id}", a.auth(a.handleSaveCampus, "admin"))
	mux.Handle("DELETE /api/campuses/{id}", a.auth(a.handleDeleteCampus, "admin"))
	mux.Handle("GET /api/classes", a.auth(a.handleListClasses, "admin", "teacher"))
	mux.Handle("POST /api/classes", a.auth(a.handleSaveClass, "admin"))
	mux.Handle("PUT /api/classes/{id}", a.auth(a.handleSaveClass, "admin"))
	mux.Handle("DELETE /api/classes/{id}", a.auth(a.handleDeleteClass, "admin"))
	mux.Handle("GET /api/users", a.auth(a.handleListUsers, "admin"))
	mux.Handle("POST /api/users", a.auth(a.handleCreateUser, "admin"))
	mux.Handle("PUT /api/users/{id}", a.auth(a.handleUpdateUser, "admin"))
	mux.Handle("POST /api/users/{id}/disabled", a.auth(a.handleSetDisabled, "admin"))
	mux.Handle("DELETE /api/users/{id}", a.auth(a.handleDeleteUser, "admin"))
	mux.Handle("POST /api/users/{id}/password", a.auth(a.handleResetPassword, "admin"))
	mux.Handle("GET /api/students", a.auth(a.handleListStudents, "admin", "teacher"))
	mux.Handle("POST /api/students", a.auth(a.handleSaveStudent, "admin"))
	mux.Handle("GET /api/students/{id}", a.auth(a.handleStudentDetail))
	mux.Handle("PUT /api/students/{id}", a.auth(a.handleSaveStudent, "admin"))
	mux.Handle("DELETE /api/students/{id}", a.auth(a.handleDeleteStudent, "admin"))
	mux.Handle("POST /api/students/{id}/parents", a.auth(a.handleLinkParent, "admin"))
	mux.Handle("DELETE /api/students/{id}/parents/{pid}", a.auth(a.handleUnlinkParent, "admin"))
	mux.Handle("POST /api/students/{id}/qr/reset", a.auth(a.handleResetQR, "admin"))
	mux.Handle("GET /api/import/template.xlsx", a.auth(a.handleImportTemplate, "admin"))
	mux.Handle("POST /api/import/students", a.auth(a.handleImportStudents, "admin"))
	mux.Handle("GET /api/students/{id}/qr.png", a.auth(a.handleStudentQR, "admin", "teacher"))

	// Arrival / departure
	mux.Handle("POST /api/attendance/scan", a.auth(a.handleScan, "admin", "teacher"))
	mux.Handle("POST /api/attendance/mark", a.auth(a.handleMark, "admin", "teacher"))
	mux.Handle("PUT /api/attendance/{id}/{date}", a.auth(a.handleEditAttendance, "admin", "teacher"))
	mux.Handle("GET /api/attendance", a.auth(a.handleDailyAttendance, "admin", "teacher"))
	mux.Handle("GET /api/attendance/student/{id}", a.auth(a.handleStudentAttendance))

	// Announcements
	mux.Handle("GET /api/announcements", a.auth(a.handleListAnnouncements))
	// Only admins post notices; teachers take roll call and share photos.
	mux.Handle("POST /api/announcements", a.auth(a.handleCreateAnnouncement, "admin"))
	mux.Handle("DELETE /api/announcements/{id}", a.auth(a.handleDeleteAnnouncement, "admin"))
	mux.Handle("PUT /api/announcements/{id}", a.auth(a.handleUpdateAnnouncement, "admin"))
	mux.Handle("POST /api/announcements/{id}/retract", a.auth(a.handleRetractAnnouncement, "admin"))
	mux.Handle("GET /api/announcements/{id}/reads", a.auth(a.handleNoticeReads, "admin"))
	mux.Handle("GET /api/announcements/{id}/files/{fid}", a.auth(a.handleNoticeFile))
	mux.Handle("POST /api/announcements/read", a.auth(a.handleMarkNoticesRead, "parent"))
	mux.Handle("GET /api/overview", a.auth(a.handleOverview, "admin"))

	// Fees
	mux.Handle("GET /api/fee-items", a.auth(a.handleListFeeItems, "admin"))
	mux.Handle("POST /api/fee-items", a.auth(a.handleCreateFeeItem, "admin"))
	mux.Handle("PUT /api/fee-items/{id}", a.auth(a.handleUpdateFeeItem, "admin"))
	mux.Handle("DELETE /api/fee-items/{id}", a.auth(a.handleDeleteFeeItem, "admin"))
	mux.Handle("GET /api/fee-items/{id}/students", a.auth(a.handleFeeItemStudents, "admin"))
	mux.Handle("PUT /api/student-fees/{id}", a.auth(a.handleUpdateStudentFee, "admin"))
	mux.Handle("GET /api/fees", a.auth(a.handleListFees, "admin", "parent"))
	mux.Handle("GET /api/official-receipts", a.auth(a.handleListOfficial, "admin", "parent"))
	mux.Handle("GET /api/official-receipts/{id}", a.auth(a.handleGetOfficial, "admin", "parent"))

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
	mux.Handle("GET /api/export/receipts", a.auth(a.handleExportReceipts, "admin"))
	mux.Handle("GET /api/export/logs", a.auth(a.handleExportLogs, "admin"))

	// Suggestions and feedback
	mux.Handle("POST /api/feedback", a.auth(a.handleCreateFeedback, "parent", "teacher"))
	mux.Handle("GET /api/feedback", a.auth(a.handleListFeedback))
	mux.Handle("GET /api/feedback/{id}/images/{iid}", a.auth(a.handleFeedbackImage))
	mux.Handle("POST /api/feedback/{id}/reply", a.auth(a.handleReplyFeedback, "admin"))

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
