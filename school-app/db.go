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

-- Class names are unique within a campus (each school may have its own "5岁 Melati").
CREATE TABLE IF NOT EXISTS classes (
	id         INTEGER PRIMARY KEY,
	name       TEXT NOT NULL,
	teacher_id INTEGER REFERENCES users(id) ON DELETE SET NULL,
	campus_id  INTEGER REFERENCES campuses(id) ON DELETE SET NULL,
	UNIQUE (campus_id, name)
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

CREATE TABLE IF NOT EXISTS photos (
	id          INTEGER PRIMARY KEY,
	file_name   TEXT NOT NULL,
	caption     TEXT NOT NULL DEFAULT '',
	uploaded_by INTEGER REFERENCES users(id) ON DELETE SET NULL,
	created_at  TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS photo_students (
	photo_id   INTEGER NOT NULL REFERENCES photos(id) ON DELETE CASCADE,
	student_id INTEGER NOT NULL REFERENCES students(id) ON DELETE CASCADE,
	PRIMARY KEY (photo_id, student_id)
);
CREATE INDEX IF NOT EXISTS idx_photo_students_student ON photo_students(student_id);

CREATE TABLE IF NOT EXISTS campuses (
	id   INTEGER PRIMARY KEY,
	name TEXT NOT NULL UNIQUE
);

-- Every change to a submitted arrival/departure time, with the reason (audit trail).
CREATE TABLE IF NOT EXISTS attendance_edits (
	id         INTEGER PRIMARY KEY,
	student_id INTEGER NOT NULL REFERENCES students(id) ON DELETE CASCADE,
	date       TEXT NOT NULL,
	field      TEXT NOT NULL CHECK (field IN ('checkin','checkout')),
	old_value  TEXT NOT NULL,
	new_value  TEXT NOT NULL,
	reason     TEXT NOT NULL,
	edited_by  INTEGER REFERENCES users(id) ON DELETE SET NULL,
	edited_at  TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_attendance_edits ON attendance_edits(student_id, date);

-- Fees: an item (e.g. "Nov 2026 tuition") is billed to many students; each gets its own row.
CREATE TABLE IF NOT EXISTS fee_items (
	id           INTEGER PRIMARY KEY,
	name         TEXT NOT NULL,
	category     TEXT NOT NULL CHECK (category IN ('monthly','event','misc','other')),
	amount_cents INTEGER NOT NULL,
	due_date     TEXT NOT NULL DEFAULT '',
	campus_id    INTEGER REFERENCES campuses(id) ON DELETE SET NULL,
	class_id     INTEGER REFERENCES classes(id) ON DELETE SET NULL,
	created_by   INTEGER REFERENCES users(id) ON DELETE SET NULL,
	created_at   TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS student_fees (
	id           INTEGER PRIMARY KEY,
	fee_item_id  INTEGER NOT NULL REFERENCES fee_items(id) ON DELETE CASCADE,
	student_id   INTEGER NOT NULL REFERENCES students(id) ON DELETE CASCADE,
	amount_cents INTEGER NOT NULL,
	status       TEXT NOT NULL DEFAULT 'unpaid' CHECK (status IN ('unpaid','pending','paid','waived')),
	paid_at      TEXT NOT NULL DEFAULT '',
	note         TEXT NOT NULL DEFAULT '',
	UNIQUE (fee_item_id, student_id)
);
CREATE INDEX IF NOT EXISTS idx_student_fees_student ON student_fees(student_id);

CREATE TABLE IF NOT EXISTS receipt_fees (
	receipt_id     INTEGER NOT NULL REFERENCES receipts(id) ON DELETE CASCADE,
	student_fee_id INTEGER NOT NULL REFERENCES student_fees(id) ON DELETE CASCADE,
	PRIMARY KEY (receipt_id, student_fee_id)
);

CREATE TABLE IF NOT EXISTS receipt_counters (
	campus_id INTEGER NOT NULL,
	year      INTEGER NOT NULL,
	last      INTEGER NOT NULL,
	PRIMARY KEY (campus_id, year)
);

CREATE TABLE IF NOT EXISTS official_receipts (
	id          INTEGER PRIMARY KEY,
	number      TEXT NOT NULL UNIQUE,
	campus_id   INTEGER REFERENCES campuses(id) ON DELETE SET NULL,
	student_id  INTEGER NOT NULL REFERENCES students(id) ON DELETE CASCADE,
	payer_id    INTEGER REFERENCES users(id) ON DELETE SET NULL,
	upload_id   INTEGER REFERENCES receipts(id) ON DELETE SET NULL,
	method      TEXT NOT NULL,
	total_cents INTEGER NOT NULL,
	issued_at   TEXT NOT NULL,
	issued_by   INTEGER REFERENCES users(id) ON DELETE SET NULL,
	voided_at   TEXT NOT NULL DEFAULT '',
	void_reason TEXT NOT NULL DEFAULT '',
	voided_by   INTEGER REFERENCES users(id) ON DELETE SET NULL
);
CREATE INDEX IF NOT EXISTS idx_official_student ON official_receipts(student_id);

CREATE TABLE IF NOT EXISTS official_receipt_items (
	official_receipt_id INTEGER NOT NULL REFERENCES official_receipts(id) ON DELETE CASCADE,
	student_fee_id      INTEGER REFERENCES student_fees(id) ON DELETE SET NULL,
	description         TEXT NOT NULL,
	amount_cents        INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS password_resets (
	id         INTEGER PRIMARY KEY,
	user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	token_hash TEXT NOT NULL UNIQUE,
	expires_at INTEGER NOT NULL,
	used_at    TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS reset_requests (
	id         INTEGER PRIMARY KEY,
	user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	contact    TEXT NOT NULL DEFAULT '',
	status     TEXT NOT NULL CHECK (status IN ('open','done','dismissed')),
	created_at TEXT NOT NULL,
	handled_by INTEGER REFERENCES users(id) ON DELETE SET NULL,
	handled_at TEXT NOT NULL DEFAULT ''
);

-- Message center (消息): every notification a user was sent, read or not.
CREATE TABLE IF NOT EXISTS messages (
	id         INTEGER PRIMARY KEY,
	user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	title      TEXT NOT NULL,
	body       TEXT NOT NULL,
	url        TEXT NOT NULL DEFAULT '',
	ref        TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL,
	read_at    TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_messages_user ON messages(user_id, id);

CREATE TABLE IF NOT EXISTS notice_attachments (
	id         INTEGER PRIMARY KEY,
	notice_id  INTEGER NOT NULL REFERENCES announcements(id) ON DELETE CASCADE,
	file_name  TEXT NOT NULL,
	file_type  TEXT NOT NULL,
	orig_name  TEXT NOT NULL,
	size_bytes INTEGER NOT NULL
);

-- Who a published notice went to, and when each of them read it (阅读统计).
CREATE TABLE IF NOT EXISTS notice_recipients (
	notice_id INTEGER NOT NULL REFERENCES announcements(id) ON DELETE CASCADE,
	user_id   INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	read_at   TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (notice_id, user_id)
);

CREATE TABLE IF NOT EXISTS export_logs (
	id         INTEGER PRIMARY KEY,
	user_id    INTEGER REFERENCES users(id) ON DELETE SET NULL,
	kind       TEXT NOT NULL,
	filters    TEXT NOT NULL,
	count      INTEGER NOT NULL,
	created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS feedback (
	id         INTEGER PRIMARY KEY,
	user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	campus_id  INTEGER REFERENCES campuses(id) ON DELETE SET NULL,
	category   TEXT NOT NULL,
	body       TEXT NOT NULL,
	contact    TEXT NOT NULL DEFAULT '',
	status     TEXT NOT NULL CHECK (status IN ('new','in_progress','resolved')),
	reply      TEXT NOT NULL DEFAULT '',
	handled_by INTEGER REFERENCES users(id) ON DELETE SET NULL,
	handled_at TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS feedback_images (
	id          INTEGER PRIMARY KEY,
	feedback_id INTEGER NOT NULL REFERENCES feedback(id) ON DELETE CASCADE,
	file_name   TEXT NOT NULL
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
		`ALTER TABLE users ADD COLUMN email TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE users ADD COLUMN campus_id INTEGER REFERENCES campuses(id) ON DELETE SET NULL`,
		`ALTER TABLE users ADD COLUMN disabled INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE classes ADD COLUMN campus_id INTEGER REFERENCES campuses(id) ON DELETE SET NULL`,
		`ALTER TABLE announcements ADD COLUMN campus_id INTEGER REFERENCES campuses(id) ON DELETE CASCADE`,
		`ALTER TABLE attendance ADD COLUMN checkout_at TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE attendance ADD COLUMN checkout_method TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE attendance ADD COLUMN checkout_by INTEGER REFERENCES users(id) ON DELETE SET NULL`,
		`ALTER TABLE students ADD COLUMN birth_date TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE students ADD COLUMN gender TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE students ADD COLUMN health_notes TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE students ADD COLUMN emergency_name TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE students ADD COLUMN emergency_phone TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE students ADD COLUMN notes TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE campuses ADD COLUMN reg_no TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE campuses ADD COLUMN about TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE campuses ADD COLUMN whatsapp TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE campuses ADD COLUMN hours TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE photos ADD COLUMN tag TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE photos ADD COLUMN taken_on TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE fee_items ADD COLUMN reminded_at TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE fee_items ADD COLUMN remind_count INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE announcements ADD COLUMN publish_at TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE announcements ADD COLUMN published INTEGER NOT NULL DEFAULT 1`,
		`ALTER TABLE announcements ADD COLUMN retracted_at TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE announcements ADD COLUMN edited_at TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE campuses ADD COLUMN address TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE campuses ADD COLUMN phone TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE campuses ADD COLUMN email TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE campuses ADD COLUMN receipt_prefix TEXT NOT NULL DEFAULT ''`,
	} {
		if _, err := db.Exec(stmt); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			return nil, fmt.Errorf("migrate: %w", err)
		}
	}
	if err := migrateClassNames(db); err != nil {
		return nil, fmt.Errorf("migrate classes: %w", err)
	}
	// Databases from before campuses existed: put their classes into one campus the admin can rename.
	var orphan, campuses int
	_ = db.QueryRow(`SELECT COUNT(*) FROM classes WHERE campus_id IS NULL`).Scan(&orphan)
	_ = db.QueryRow(`SELECT COUNT(*) FROM campuses`).Scan(&campuses)
	if orphan > 0 && campuses == 0 {
		res, err := db.Exec(`INSERT INTO campuses (name) VALUES ('Main campus')`)
		if err != nil {
			return nil, fmt.Errorf("migrate campuses: %w", err)
		}
		id, _ := res.LastInsertId()
		if _, err := db.Exec(`UPDATE classes SET campus_id = ? WHERE campus_id IS NULL`, id); err != nil {
			return nil, fmt.Errorf("migrate campuses: %w", err)
		}
	}
	return db, nil
}

// migrateClassNames rebuilds the classes table of databases from before campuses, where class
// names had to be unique across all schools. SQLite cannot drop a constraint, so the table is
// copied into a new one (ids unchanged) with foreign key checks paused.
func migrateClassNames(db *sql.DB) error {
	var ddl string
	if err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'classes'`).Scan(&ddl); err != nil {
		return err
	}
	if !strings.Contains(ddl, "name       TEXT NOT NULL UNIQUE") && !strings.Contains(ddl, "name TEXT NOT NULL UNIQUE") {
		return nil
	}
	if _, err := db.Exec(`PRAGMA foreign_keys = OFF`); err != nil {
		return err
	}
	defer db.Exec(`PRAGMA foreign_keys = ON`)
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, stmt := range []string{
		`CREATE TABLE classes_new (
			id         INTEGER PRIMARY KEY,
			name       TEXT NOT NULL,
			teacher_id INTEGER REFERENCES users(id) ON DELETE SET NULL,
			campus_id  INTEGER REFERENCES campuses(id) ON DELETE SET NULL,
			UNIQUE (campus_id, name)
		)`,
		`INSERT INTO classes_new (id, name, teacher_id, campus_id) SELECT id, name, teacher_id, campus_id FROM classes`,
		`DROP TABLE classes`,
		`ALTER TABLE classes_new RENAME TO classes`,
	} {
		if _, err := tx.Exec(stmt); err != nil {
			return err
		}
	}
	return tx.Commit()
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
	var campusIDs []int64
	for _, name := range []string{"Tadika Taman Merak", "Tadika Merak Pintar", "Tadika Merak Cemerlang"} {
		res, err := db.Exec(`INSERT INTO campuses (name, receipt_prefix, address, phone) VALUES (?,?,?,?)`,
			name, defaultPrefix(name), "（示范地址 demo address）", "03-1234 5678")
		if err != nil {
			return err
		}
		id, _ := res.LastInsertId()
		campusIDs = append(campusIDs, id)
	}
	tid, err := createUser(db, "teacher", "teacher123", "Ms. Tan 陈老师", "011-2223333", "teacher")
	if err != nil {
		return err
	}
	if _, err := db.Exec(`UPDATE users SET campus_id = ?, email = 'tan@example.com' WHERE id = ?`, campusIDs[0], tid); err != nil {
		return err
	}
	pid, err := createUser(db, "parent", "parent123", "林先生 Mr. Lim", "012-3456789", "parent")
	if err != nil {
		return err
	}
	res, err := db.Exec(`INSERT INTO classes (name, teacher_id, campus_id) VALUES ('5岁 Melati', ?, ?)`, tid, campusIDs[0])
	if err != nil {
		return err
	}
	c1, _ := res.LastInsertId()
	res, err = db.Exec(`INSERT INTO classes (name, campus_id) VALUES ('6岁 Mawar', ?)`, campusIDs[1])
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
	_, _ = db.Exec(`UPDATE students SET birth_date = '2021-03-14', gender = 'M', health_notes = '对花生过敏 Peanut allergy',
		emergency_name = '林太太 Mrs. Lim', emergency_phone = '013-9876543' WHERE student_no = 'S1001'`)
	_, err = db.Exec(`INSERT INTO announcements (title, body, class_id, author_id, created_at)
		VALUES ('欢迎使用 / Welcome', '学校通知会在这里发布。School announcements will appear here.', NULL, ?, ?)`, tid, app.nowStr())
	if err != nil {
		return err
	}
	log.Printf("已加入示范数据 / Demo data added: teacher/teacher123, parent/parent123")
	return nil
}
