package main

import (
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	qrcode "github.com/skip2/go-qrcode"
)

// QR codes on student cards contain this prefix followed by the student's secret token.
const qrPrefix = "SCHOOLAPP:"

func logError(err error) { log.Printf("error: %v", err) }

func pathID(r *http.Request, name string) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	return id, err == nil && id > 0
}

func isUnique(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint")
}

func nullInt(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

// ================= Classes =================

type Class struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	TeacherID   int64  `json:"teacher_id"`
	TeacherName string `json:"teacher_name"`
	Students    int    `json:"students"`
}

func (a *App) handleListClasses(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.Query(`SELECT c.id, c.name, COALESCE(c.teacher_id,0), COALESCE(u.name,''),
		(SELECT COUNT(*) FROM students s WHERE s.class_id = c.id)
		FROM classes c LEFT JOIN users u ON u.id = c.teacher_id ORDER BY c.name`)
	if err != nil {
		serverError(w, err)
		return
	}
	defer rows.Close()
	list := []Class{}
	for rows.Next() {
		var c Class
		if err := rows.Scan(&c.ID, &c.Name, &c.TeacherID, &c.TeacherName, &c.Students); err != nil {
			serverError(w, err)
			return
		}
		list = append(list, c)
	}
	writeJSON(w, list)
}

func (a *App) handleCreateClass(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name      string `json:"name"`
		TeacherID int64  `json:"teacher_id"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		httpError(w, http.StatusBadRequest, "name is required")
		return
	}
	_, err := a.db.Exec(`INSERT INTO classes (name, teacher_id) VALUES (?,?)`, req.Name, nullInt(req.TeacherID))
	if isUnique(err) {
		httpError(w, http.StatusConflict, "already exists")
		return
	}
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (a *App) handleDeleteClass(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		httpError(w, http.StatusBadRequest, "invalid id")
		return
	}
	if _, err := a.db.Exec(`DELETE FROM classes WHERE id = ?`, id); err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// ================= Users =================

var usernameRe = regexp.MustCompile(`^[a-z0-9._-]{3,32}$`)

func (a *App) handleListUsers(w http.ResponseWriter, r *http.Request) {
	q := `SELECT id, username, name, phone, role FROM users`
	var args []any
	if role := r.URL.Query().Get("role"); role != "" {
		q += ` WHERE role = ?`
		args = append(args, role)
	}
	rows, err := a.db.Query(q+` ORDER BY role, name`, args...)
	if err != nil {
		serverError(w, err)
		return
	}
	defer rows.Close()
	list := []User{}
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Username, &u.Name, &u.Phone, &u.Role); err != nil {
			serverError(w, err)
			return
		}
		list = append(list, u)
	}
	writeJSON(w, list)
}

func (a *App) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username, Password, Name, Phone, Role string
	}
	if !readJSON(w, r, &req) {
		return
	}
	req.Username = strings.ToLower(strings.TrimSpace(req.Username))
	req.Name = strings.TrimSpace(req.Name)
	switch {
	case !usernameRe.MatchString(req.Username):
		httpError(w, http.StatusBadRequest, "username: 3-32 letters, numbers, . _ -")
		return
	case req.Name == "":
		httpError(w, http.StatusBadRequest, "name is required")
		return
	case len(req.Password) < 6:
		httpError(w, http.StatusBadRequest, "password must be at least 6 characters")
		return
	case req.Role != "admin" && req.Role != "teacher" && req.Role != "parent":
		httpError(w, http.StatusBadRequest, "invalid role")
		return
	}
	id, err := createUser(a.db, req.Username, req.Password, req.Name, strings.TrimSpace(req.Phone), req.Role)
	if isUnique(err) {
		httpError(w, http.StatusConflict, "username already exists")
		return
	}
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, map[string]int64{"id": id})
}

func (a *App) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		httpError(w, http.StatusBadRequest, "invalid id")
		return
	}
	if id == currentUser(r).ID {
		httpError(w, http.StatusBadRequest, "cannot delete yourself")
		return
	}
	if _, err := a.db.Exec(`DELETE FROM users WHERE id = ?`, id); err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (a *App) handleResetPassword(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		httpError(w, http.StatusBadRequest, "invalid id")
		return
	}
	var req struct{ Password string }
	if !readJSON(w, r, &req) {
		return
	}
	if len(req.Password) < 6 {
		httpError(w, http.StatusBadRequest, "password must be at least 6 characters")
		return
	}
	if err := a.setPassword(id, req.Password); err != nil {
		serverError(w, err)
		return
	}
	_, _ = a.db.Exec(`DELETE FROM sessions WHERE user_id = ?`, id)
	writeJSON(w, map[string]bool{"ok": true})
}

// ================= Students =================

type Student struct {
	ID        int64    `json:"id"`
	Name      string   `json:"name"`
	StudentNo string   `json:"student_no"`
	ClassID   int64    `json:"class_id"`
	ClassName string   `json:"class_name"`
	Parents   []Parent `json:"parents,omitempty"`
}

type Parent struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Phone string `json:"phone"`
}

func (a *App) queryStudents(where string, args ...any) ([]Student, error) {
	rows, err := a.db.Query(`SELECT s.id, s.name, s.student_no, COALESCE(s.class_id,0), COALESCE(c.name,'')
		FROM students s LEFT JOIN classes c ON c.id = s.class_id `+where+` ORDER BY c.name, s.name`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []Student{}
	for rows.Next() {
		var s Student
		if err := rows.Scan(&s.ID, &s.Name, &s.StudentNo, &s.ClassID, &s.ClassName); err != nil {
			return nil, err
		}
		list = append(list, s)
	}
	return list, rows.Err()
}

func (a *App) handleListStudents(w http.ResponseWriter, r *http.Request) {
	where, args := "", []any{}
	if cid := r.URL.Query().Get("class_id"); cid != "" {
		where, args = "WHERE s.class_id = ?", append(args, cid)
	}
	list, err := a.queryStudents(where, args...)
	if err != nil {
		serverError(w, err)
		return
	}
	if currentUser(r).Role == "admin" {
		rows, err := a.db.Query(`SELECT ps.student_id, u.id, u.name, u.phone FROM parent_students ps JOIN users u ON u.id = ps.parent_id`)
		if err != nil {
			serverError(w, err)
			return
		}
		defer rows.Close()
		parents := map[int64][]Parent{}
		for rows.Next() {
			var sid int64
			var p Parent
			if err := rows.Scan(&sid, &p.ID, &p.Name, &p.Phone); err != nil {
				serverError(w, err)
				return
			}
			parents[sid] = append(parents[sid], p)
		}
		for i := range list {
			list[i].Parents = parents[list[i].ID]
		}
	}
	writeJSON(w, list)
}

func (a *App) handleCreateStudent(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name      string `json:"name"`
		StudentNo string `json:"student_no"`
		ClassID   int64  `json:"class_id"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	req.Name, req.StudentNo = strings.TrimSpace(req.Name), strings.TrimSpace(req.StudentNo)
	if req.Name == "" || req.StudentNo == "" {
		httpError(w, http.StatusBadRequest, "name and student number are required")
		return
	}
	res, err := a.db.Exec(`INSERT INTO students (name, student_no, class_id, qr_token) VALUES (?,?,?,?)`,
		req.Name, req.StudentNo, nullInt(req.ClassID), randomToken(12))
	if isUnique(err) {
		httpError(w, http.StatusConflict, "student number already exists")
		return
	}
	if err != nil {
		serverError(w, err)
		return
	}
	id, _ := res.LastInsertId()
	writeJSON(w, map[string]int64{"id": id})
}

func (a *App) handleDeleteStudent(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		httpError(w, http.StatusBadRequest, "invalid id")
		return
	}
	if _, err := a.db.Exec(`DELETE FROM students WHERE id = ?`, id); err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (a *App) handleLinkParent(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		httpError(w, http.StatusBadRequest, "invalid id")
		return
	}
	var req struct {
		ParentID int64 `json:"parent_id"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	var role string
	if err := a.db.QueryRow(`SELECT role FROM users WHERE id = ?`, req.ParentID).Scan(&role); err != nil || role != "parent" {
		httpError(w, http.StatusBadRequest, "not a parent account")
		return
	}
	if _, err := a.db.Exec(`INSERT OR IGNORE INTO parent_students (parent_id, student_id) VALUES (?,?)`, req.ParentID, id); err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (a *App) handleUnlinkParent(w http.ResponseWriter, r *http.Request) {
	id, ok1 := pathID(r, "id")
	pid, ok2 := pathID(r, "pid")
	if !ok1 || !ok2 {
		httpError(w, http.StatusBadRequest, "invalid id")
		return
	}
	if _, err := a.db.Exec(`DELETE FROM parent_students WHERE parent_id = ? AND student_id = ?`, pid, id); err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// handleResetQR issues a new QR token, so a lost card stops working.
func (a *App) handleResetQR(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		httpError(w, http.StatusBadRequest, "invalid id")
		return
	}
	if _, err := a.db.Exec(`UPDATE students SET qr_token = ? WHERE id = ?`, randomToken(12), id); err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (a *App) handleStudentQR(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		httpError(w, http.StatusBadRequest, "invalid id")
		return
	}
	var token string
	if err := a.db.QueryRow(`SELECT qr_token FROM students WHERE id = ?`, id).Scan(&token); err != nil {
		serverError(w, err)
		return
	}
	png, err := qrcode.Encode(qrPrefix+token, qrcode.Medium, 512)
	if err != nil {
		serverError(w, err)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "private, no-store")
	_, _ = w.Write(png)
}

// ================= Attendance =================

type CheckinResult struct {
	Student   Student `json:"student"`
	CheckinAt string  `json:"checkin_at"`
	Late      bool    `json:"late"`
	Already   bool    `json:"already"`
}

func (a *App) checkin(w http.ResponseWriter, r *http.Request, studentWhere string, arg any, method string) {
	list, err := a.queryStudents(studentWhere, arg)
	if err != nil {
		serverError(w, err)
		return
	}
	if len(list) == 0 {
		httpError(w, http.StatusNotFound, "student not found")
		return
	}
	s := list[0]
	now := a.now()
	date := now.Format("2006-01-02")
	late := now.Format("15:04") > a.lateAfter
	res, err := a.db.Exec(`INSERT OR IGNORE INTO attendance (student_id, date, checkin_at, late, method, scanned_by)
		VALUES (?,?,?,?,?,?)`, s.ID, date, now.Format(time.RFC3339), late, method, currentUser(r).ID)
	if err != nil {
		serverError(w, err)
		return
	}
	out := CheckinResult{Student: s}
	if n, _ := res.RowsAffected(); n == 0 {
		out.Already = true
	}
	if err := a.db.QueryRow(`SELECT checkin_at, late FROM attendance WHERE student_id = ? AND date = ?`, s.ID, date).
		Scan(&out.CheckinAt, &out.Late); err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, out)
	if !out.Already {
		go a.notifier.notifyCheckin(s, out.CheckinAt, out.Late)
	}
}

func (a *App) handleScan(w http.ResponseWriter, r *http.Request) {
	var req struct{ Code string }
	if !readJSON(w, r, &req) {
		return
	}
	token, ok := strings.CutPrefix(strings.TrimSpace(req.Code), qrPrefix)
	if !ok || token == "" {
		httpError(w, http.StatusBadRequest, "not a student card")
		return
	}
	a.checkin(w, r, "WHERE s.qr_token = ?", token, "qr")
}

// handleManualCheckin is for students who forgot their card.
func (a *App) handleManualCheckin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		StudentID int64 `json:"student_id"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	a.checkin(w, r, "WHERE s.id = ?", req.StudentID, "manual")
}

type AttendanceRow struct {
	Student
	Present   bool   `json:"present"`
	CheckinAt string `json:"checkin_at"`
	Late      bool   `json:"late"`
	Method    string `json:"method"`
}

func (a *App) handleDailyAttendance(w http.ResponseWriter, r *http.Request) {
	date := r.URL.Query().Get("date")
	if _, err := time.Parse("2006-01-02", date); err != nil {
		date = a.now().Format("2006-01-02")
	}
	q := `SELECT s.id, s.name, s.student_no, COALESCE(s.class_id,0), COALESCE(c.name,''),
		a.id IS NOT NULL, COALESCE(a.checkin_at,''), COALESCE(a.late,0), COALESCE(a.method,'')
		FROM students s LEFT JOIN classes c ON c.id = s.class_id
		LEFT JOIN attendance a ON a.student_id = s.id AND a.date = ?`
	args := []any{date}
	if cid := r.URL.Query().Get("class_id"); cid != "" {
		q += ` WHERE s.class_id = ?`
		args = append(args, cid)
	}
	rows, err := a.db.Query(q+` ORDER BY c.name, s.name`, args...)
	if err != nil {
		serverError(w, err)
		return
	}
	defer rows.Close()
	list := []AttendanceRow{}
	for rows.Next() {
		var x AttendanceRow
		if err := rows.Scan(&x.ID, &x.Name, &x.StudentNo, &x.ClassID, &x.ClassName, &x.Present, &x.CheckinAt, &x.Late, &x.Method); err != nil {
			serverError(w, err)
			return
		}
		list = append(list, x)
	}
	writeJSON(w, map[string]any{"date": date, "rows": list})
}

// parentOwns reports whether the parent is linked to the student.
func (a *App) parentOwns(parentID, studentID int64) bool {
	var one int
	return a.db.QueryRow(`SELECT 1 FROM parent_students WHERE parent_id = ? AND student_id = ?`, parentID, studentID).Scan(&one) == nil
}

func (a *App) handleStudentAttendance(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		httpError(w, http.StatusBadRequest, "invalid id")
		return
	}
	u := currentUser(r)
	if u.Role == "parent" && !a.parentOwns(u.ID, id) {
		httpError(w, http.StatusForbidden, "permission denied")
		return
	}
	since := a.now().AddDate(0, 0, -60).Format("2006-01-02")
	rows, err := a.db.Query(`SELECT date, checkin_at, late, method FROM attendance
		WHERE student_id = ? AND date >= ? ORDER BY date DESC`, id, since)
	if err != nil {
		serverError(w, err)
		return
	}
	defer rows.Close()
	type rec struct {
		Date      string `json:"date"`
		CheckinAt string `json:"checkin_at"`
		Late      bool   `json:"late"`
		Method    string `json:"method"`
	}
	list := []rec{}
	for rows.Next() {
		var x rec
		if err := rows.Scan(&x.Date, &x.CheckinAt, &x.Late, &x.Method); err != nil {
			serverError(w, err)
			return
		}
		list = append(list, x)
	}
	writeJSON(w, list)
}

// ================= Announcements =================

type Announcement struct {
	ID         int64  `json:"id"`
	Title      string `json:"title"`
	Body       string `json:"body"`
	ClassID    int64  `json:"class_id"`
	ClassName  string `json:"class_name"`
	AuthorID   int64  `json:"author_id"`
	AuthorName string `json:"author_name"`
	CreatedAt  string `json:"created_at"`
}

func (a *App) handleListAnnouncements(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	q := `SELECT n.id, n.title, n.body, COALESCE(n.class_id,0), COALESCE(c.name,''),
		COALESCE(n.author_id,0), COALESCE(u.name,''), n.created_at
		FROM announcements n LEFT JOIN classes c ON c.id = n.class_id LEFT JOIN users u ON u.id = n.author_id`
	var args []any
	if u.Role == "parent" {
		// Parents see school-wide notices plus notices for their children's classes.
		q += ` WHERE n.class_id IS NULL OR n.class_id IN (SELECT s.class_id FROM students s
			JOIN parent_students ps ON ps.student_id = s.id WHERE ps.parent_id = ?)`
		args = append(args, u.ID)
	}
	rows, err := a.db.Query(q+` ORDER BY n.created_at DESC LIMIT 200`, args...)
	if err != nil {
		serverError(w, err)
		return
	}
	defer rows.Close()
	list := []Announcement{}
	for rows.Next() {
		var n Announcement
		if err := rows.Scan(&n.ID, &n.Title, &n.Body, &n.ClassID, &n.ClassName, &n.AuthorID, &n.AuthorName, &n.CreatedAt); err != nil {
			serverError(w, err)
			return
		}
		list = append(list, n)
	}
	writeJSON(w, list)
}

func (a *App) handleCreateAnnouncement(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Title   string `json:"title"`
		Body    string `json:"body"`
		ClassID int64  `json:"class_id"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	req.Title, req.Body = strings.TrimSpace(req.Title), strings.TrimSpace(req.Body)
	if req.Title == "" || req.Body == "" {
		httpError(w, http.StatusBadRequest, "title and content are required")
		return
	}
	if _, err := a.db.Exec(`INSERT INTO announcements (title, body, class_id, author_id, created_at) VALUES (?,?,?,?,?)`,
		req.Title, req.Body, nullInt(req.ClassID), currentUser(r).ID, a.nowStr()); err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (a *App) handleDeleteAnnouncement(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		httpError(w, http.StatusBadRequest, "invalid id")
		return
	}
	u := currentUser(r)
	q, args := `DELETE FROM announcements WHERE id = ?`, []any{id}
	if u.Role != "admin" { // teachers may only delete their own notices
		q += ` AND author_id = ?`
		args = append(args, u.ID)
	}
	res, err := a.db.Exec(q, args...)
	if err != nil {
		serverError(w, err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		httpError(w, http.StatusForbidden, "permission denied")
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// ================= Tuition receipts =================

const maxUpload = 10 << 20 // 10 MB

var allowedTypes = map[string]string{
	"image/jpeg":      ".jpg",
	"image/png":       ".png",
	"image/webp":      ".webp",
	"application/pdf": ".pdf",
}

type Receipt struct {
	ID          int64  `json:"id"`
	StudentID   int64  `json:"student_id"`
	StudentName string `json:"student_name"`
	ClassName   string `json:"class_name"`
	ParentName  string `json:"parent_name"`
	ParentPhone string `json:"parent_phone"`
	Description string `json:"description"`
	AmountCents int64  `json:"amount_cents"`
	FileType    string `json:"file_type"`
	Status      string `json:"status"`
	ReviewNote  string `json:"review_note"`
	ReviewedBy  string `json:"reviewed_by"`
	ReviewedAt  string `json:"reviewed_at"`
	CreatedAt   string `json:"created_at"`
}

func parseAmount(s string) (int64, error) {
	s = strings.ReplaceAll(strings.TrimSpace(s), ",", "")
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f <= 0 || f > 1e8 || math.IsNaN(f) {
		return 0, errors.New("invalid amount")
	}
	return int64(math.Round(f * 100)), nil
}

func (a *App) handleUploadReceipt(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxUpload+(1<<20))
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		httpError(w, http.StatusBadRequest, "file too large (max 10 MB)")
		return
	}
	defer r.MultipartForm.RemoveAll()
	u := currentUser(r)
	studentID, _ := strconv.ParseInt(r.FormValue("student_id"), 10, 64)
	if !a.parentOwns(u.ID, studentID) {
		httpError(w, http.StatusForbidden, "permission denied")
		return
	}
	desc := strings.TrimSpace(r.FormValue("description"))
	if desc == "" || len(desc) > 200 {
		httpError(w, http.StatusBadRequest, "description is required")
		return
	}
	amount, err := parseAmount(r.FormValue("amount"))
	if err != nil {
		httpError(w, http.StatusBadRequest, "invalid amount")
		return
	}
	file, _, err := r.FormFile("file")
	if err != nil {
		httpError(w, http.StatusBadRequest, "please choose a file")
		return
	}
	defer file.Close()

	head := make([]byte, 512)
	n, _ := io.ReadFull(file, head)
	ctype := http.DetectContentType(head[:n])
	if i := strings.IndexByte(ctype, ';'); i >= 0 {
		ctype = ctype[:i]
	}
	ext, ok := allowedTypes[ctype]
	if !ok {
		httpError(w, http.StatusBadRequest, "only JPG, PNG, WEBP or PDF files")
		return
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		serverError(w, err)
		return
	}
	name := randomToken(16) + ext
	out, err := os.OpenFile(filepath.Join(a.uploadDir, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
	if err != nil {
		serverError(w, err)
		return
	}
	if _, err := io.Copy(out, io.LimitReader(file, maxUpload+1)); err != nil {
		out.Close()
		os.Remove(out.Name())
		serverError(w, err)
		return
	}
	if err := out.Close(); err != nil {
		serverError(w, err)
		return
	}
	if fi, _ := os.Stat(out.Name()); fi != nil && fi.Size() > maxUpload {
		os.Remove(out.Name())
		httpError(w, http.StatusBadRequest, "file too large (max 10 MB)")
		return
	}
	if _, err := a.db.Exec(`INSERT INTO receipts (student_id, parent_id, description, amount_cents, file_name, file_type, created_at)
		VALUES (?,?,?,?,?,?,?)`, studentID, u.ID, desc, amount, name, ctype, a.nowStr()); err != nil {
		os.Remove(out.Name())
		serverError(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (a *App) handleListReceipts(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	q := `SELECT rc.id, rc.student_id, s.name, COALESCE(c.name,''), COALESCE(p.name,''), COALESCE(p.phone,''),
		rc.description, rc.amount_cents, rc.file_type, rc.status, rc.review_note, COALESCE(rv.name,''),
		COALESCE(rc.reviewed_at,''), rc.created_at
		FROM receipts rc JOIN students s ON s.id = rc.student_id LEFT JOIN classes c ON c.id = s.class_id
		LEFT JOIN users p ON p.id = rc.parent_id LEFT JOIN users rv ON rv.id = rc.reviewed_by WHERE 1=1`
	var args []any
	if u.Role == "parent" {
		q += ` AND rc.student_id IN (SELECT student_id FROM parent_students WHERE parent_id = ?)`
		args = append(args, u.ID)
	}
	if st := r.URL.Query().Get("status"); st != "" {
		q += ` AND rc.status = ?`
		args = append(args, st)
	}
	rows, err := a.db.Query(q+` ORDER BY rc.created_at DESC LIMIT 500`, args...)
	if err != nil {
		serverError(w, err)
		return
	}
	defer rows.Close()
	list := []Receipt{}
	for rows.Next() {
		var x Receipt
		if err := rows.Scan(&x.ID, &x.StudentID, &x.StudentName, &x.ClassName, &x.ParentName, &x.ParentPhone,
			&x.Description, &x.AmountCents, &x.FileType, &x.Status, &x.ReviewNote, &x.ReviewedBy, &x.ReviewedAt, &x.CreatedAt); err != nil {
			serverError(w, err)
			return
		}
		list = append(list, x)
	}
	writeJSON(w, list)
}

func (a *App) handleReceiptFile(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		httpError(w, http.StatusBadRequest, "invalid id")
		return
	}
	var studentID int64
	var name, ctype string
	if err := a.db.QueryRow(`SELECT student_id, file_name, file_type FROM receipts WHERE id = ?`, id).
		Scan(&studentID, &name, &ctype); err != nil {
		serverError(w, err)
		return
	}
	u := currentUser(r)
	if u.Role == "parent" && !a.parentOwns(u.ID, studentID) {
		httpError(w, http.StatusForbidden, "permission denied")
		return
	}
	f, err := os.Open(filepath.Join(a.uploadDir, filepath.Base(name)))
	if err != nil {
		serverError(w, fmt.Errorf("open receipt %d: %w", id, err))
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Content-Disposition", fmt.Sprintf(`inline; filename="receipt-%d%s"`, id, filepath.Ext(name)))
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; img-src 'self'; style-src 'unsafe-inline'")
	_, _ = io.Copy(w, f)
}

func (a *App) handleReviewReceipt(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		httpError(w, http.StatusBadRequest, "invalid id")
		return
	}
	var req struct {
		Status string `json:"status"`
		Note   string `json:"note"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if req.Status != "approved" && req.Status != "rejected" && req.Status != "pending" {
		httpError(w, http.StatusBadRequest, "invalid status")
		return
	}
	res, err := a.db.Exec(`UPDATE receipts SET status = ?, review_note = ?, reviewed_by = ?, reviewed_at = ? WHERE id = ?`,
		req.Status, strings.TrimSpace(req.Note), currentUser(r).ID, a.nowStr(), id)
	if err != nil {
		serverError(w, err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		httpError(w, http.StatusNotFound, "not found")
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}
