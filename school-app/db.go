package main

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"log"
	"strings"

	"golang.org/x/crypto/bcrypt"
	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS users (
	id            INTEGER PRIMARY KEY,
	username      TEXT NOT NULL UNIQUE,
	password_hash TEXT NOT NULL,
	name          TEXT NOT NULL,
	phone         TEXT NOT NULL DEFAULT '',
	role          TEXT NOT NULL CHECK (role IN ('admin','teacher','parent')),
	created_at    TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS sessions (
	token      TEXT PRIMARY KEY,
	user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	expires_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS classes (
	id         INTEGER PRIMARY KEY,
	name       TEXT NOT NULL UNIQUE,
	teacher_id INTEGER REFERENCES users(id) ON DELETE SET NULL
);

CREATE TABLE IF NOT EXISTS students (
	id         INTEGER PRIMARY KEY,
	name       TEXT NOT NULL,
	student_no TEXT NOT NULL UNIQUE,
	class_id   INTEGER REFERENCES classes(id) ON DELETE SET NULL,
	qr_token   TEXT NOT NULL UNIQUE
);

CREATE TABLE IF NOT EXISTS parent_students (
	parent_id  INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	student_id INTEGER NOT NULL REFERENCES students(id) ON DELETE CASCADE,
	PRIMARY KEY (parent_id, student_id)
);

CREATE TABLE IF NOT EXISTS attendance (
	id         INTEGER PRIMARY KEY,
	student_id INTEGER NOT NULL REFERENCES students(id) ON DELETE CASCADE,
	date       TEXT NOT NULL,
	checkin_at TEXT NOT NULL,
	late       INTEGER NOT NULL DEFAULT 0,
	method     TEXT NOT NULL DEFAULT 'qr',
	scanned_by INTEGER REFERENCES users(id) ON DELETE SET NULL,
	UNIQUE (student_id, date)
);

CREATE TABLE IF NOT EXISTS announcements (
	id         INTEGER PRIMARY KEY,
	title      TEXT NOT NULL,
	body       TEXT NOT NULL,
	class_id   INTEGER REFERENCES classes(id) ON DELETE CASCADE,
	author_id  INTEGER REFERENCES users(id) ON DELETE SET NULL,
	created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS receipts (
	id           INTEGER PRIMARY KEY,
	student_id   INTEGER NOT NULL REFERENCES students(id) ON DELETE CASCADE,
	parent_id    INTEGER REFERENCES users(id) ON DELETE SET NULL,
	description  TEXT NOT NULL,
	amount_cents INTEGER NOT NULL,
	file_name    TEXT NOT NULL,
	file_type    TEXT NOT NULL,
	status       TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','approved','rejected')),
	review_note  TEXT NOT NULL DEFAULT '',
	reviewed_by  INTEGER REFERENCES users(id) ON DELETE SET NULL,
	reviewed_at  TEXT,
	created_at   TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS settings (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS push_subscriptions (
	id         INTEGER PRIMARY KEY,
	user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	endpoint   TEXT NOT NULL UNIQUE,
	p256dh     TEXT NOT NULL,
	auth       TEXT NOT NULL,
	created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS notification_log (
	id         INTEGER PRIMARY KEY,
	user_id    INTEGER REFERENCES users(id) ON DELETE SET NULL,
	student_id INTEGER REFERENCES students(id) ON DELETE SET NULL,
	channel    TEXT NOT NULL,
	ok         INTEGER NOT NULL,
	detail     TEXT NOT NULL,
	created_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_attendance_date ON attendance(date);
CREATE INDEX IF NOT EXISTS idx_receipts_status ON receipts(status);
`

func openDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	// SQLite handles one writer at a time; a single connection keeps things simple and safe.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("create schema: %w", err)
	}
	// Columns added after the first version; ignore "duplicate column" on databases that have them.
	for _, stmt := range []string{
		`ALTER TABLE users ADD COLUMN notify_push INTEGER NOT NULL DEFAULT 1`,
		`ALTER TABLE users ADD COLUMN notify_whatsapp INTEGER NOT NULL DEFAULT 1`,
	} {
		if _, err := db.Exec(stmt); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			return nil, fmt.Errorf("migrate: %w", err)
		}
	}
	return db, nil
}

func randomToken(nBytes int) string {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func hashPassword(pw string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	return string(h), err
}

func createUser(db *sql.DB, username, password, name, phone, role string) (int64, error) {
	h, err := hashPassword(password)
	if err != nil {
		return 0, err
	}
	res, err := db.Exec(`INSERT INTO users (username, password_hash, name, phone, role) VALUES (?,?,?,?,?)`,
		username, h, name, phone, role)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ensureAdmin creates the first admin account when the database is empty.
func ensureAdmin(db *sql.DB, password string) error {
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	generated := password == ""
	if generated {
		password = randomToken(6)
	}
	if _, err := createUser(db, "admin", password, "管理员 Admin", "", "admin"); err != nil {
		return err
	}
	if generated {
		log.Printf("==============================================")
		log.Printf(" 已创建管理员账号 / Admin account created")
		log.Printf("   用户名 username: admin")
		log.Printf("   密码   password: %s", password)
		log.Printf(" 请登录后立即修改密码 / Change it after login")
		log.Printf("==============================================")
	}
	return nil
}

// seedDemo fills an empty school with sample data so the app can be tried out.
func seedDemo(app *App) error {
	db := app.db
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM classes`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	tid, err := createUser(db, "teacher", "teacher123", "陈老师 Ms. Tan", "", "teacher")
	if err != nil {
		return err
	}
	pid, err := createUser(db, "parent", "parent123", "林先生 Mr. Lim", "012-3456789", "parent")
	if err != nil {
		return err
	}
	res, err := db.Exec(`INSERT INTO classes (name, teacher_id) VALUES ('一年级A班 1A', ?)`, tid)
	if err != nil {
		return err
	}
	c1, _ := res.LastInsertId()
	res, err = db.Exec(`INSERT INTO classes (name) VALUES ('二年级B班 2B')`)
	if err != nil {
		return err
	}
	c2, _ := res.LastInsertId()

	students := []struct {
		name, no string
		class    int64
	}{
		{"林小明 Lim Xiao Ming", "S1001", c1},
		{"林小红 Lim Xiao Hong", "S2001", c2},
		{"王大伟 Wong Da Wei", "S1002", c1},
		{"李美玲 Lee Mei Ling", "S1003", c1},
	}
	for i, s := range students {
		res, err := db.Exec(`INSERT INTO students (name, student_no, class_id, qr_token) VALUES (?,?,?,?)`,
			s.name, s.no, s.class, randomToken(12))
		if err != nil {
			return err
		}
		if i < 2 { // the demo parent has two children
			sid, _ := res.LastInsertId()
			if _, err := db.Exec(`INSERT INTO parent_students VALUES (?,?)`, pid, sid); err != nil {
				return err
			}
		}
	}
	_, err = db.Exec(`INSERT INTO announcements (title, body, class_id, author_id, created_at)
		VALUES ('欢迎使用 / Welcome', '学校通知会在这里发布。School announcements will appear here.', NULL, ?, ?)`, tid, app.nowStr())
	if err != nil {
		return err
	}
	log.Printf("已加入示范数据 / Demo data added: teacher/teacher123, parent/parent123")
	return nil
}
