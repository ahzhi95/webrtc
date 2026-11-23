package main

import (
	"database/sql"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"

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

func okJSON(w http.ResponseWriter) { writeJSON(w, map[string]bool{"ok": true}) }

// ================= Access rules =================

func (a *App) classCampus(classID int64) int64 {
	var c int64
	_ = a.db.QueryRow(`SELECT COALESCE(campus_id,0) FROM classes WHERE id = ?`, classID).Scan(&c)
	return c
}

func (a *App) studentCampus(studentID int64) int64 {
	var c int64
	_ = a.db.QueryRow(`SELECT COALESCE(c.campus_id,0) FROM students s LEFT JOIN classes c ON c.id = s.class_id WHERE s.id = ?`, studentID).Scan(&c)
	return c
}

// parentOwns reports whether the parent is linked to the student.
func (a *App) parentOwns(parentID, studentID int64) bool {
	var one int
	return a.db.QueryRow(`SELECT 1 FROM parent_students WHERE parent_id = ? AND student_id = ?`, parentID, studentID).Scan(&one) == nil
}

// canAccessStudent: parents their own children; staff the students of their campus (or all, when not tied to one).
func (a *App) canAccessStudent(u *User, studentID int64) bool {
	if u.Role == "parent" {
		return a.parentOwns(u.ID, studentID)
	}
	var exists int
	if a.db.QueryRow(`SELECT 1 FROM students WHERE id = ?`, studentID).Scan(&exists) != nil {
		return false
	}
	return scope(u) == 0 || a.studentCampus(studentID) == scope(u)
}

func (a *App) canAccessClass(u *User, classID int64) bool {
	var exists int
	if a.db.QueryRow(`SELECT 1 FROM classes WHERE id = ?`, classID).Scan(&exists) != nil {
		return false
	}
	return scope(u) == 0 || a.classCampus(classID) == scope(u)
}

// userInScope: may this admin manage that account? Campus admins manage accounts of their campus,
// and parents of children in their campus.
func (a *App) userInScope(u *User, targetID int64) bool {
	var role string
	var campus int64
	if a.db.QueryRow(`SELECT role, COALESCE(campus_id,0) FROM users WHERE id = ?`, targetID).Scan(&role, &campus) != nil {
		return false
	}
	if scope(u) == 0 {
		return true
	}
	if campus == scope(u) {
		return true
	}
	if role == "parent" {
		var one int
		return a.db.QueryRow(`SELECT 1 FROM parent_students ps JOIN students s ON s.id = ps.student_id
			JOIN classes c ON c.id = s.class_id WHERE ps.parent_id = ? AND c.campus_id = ? LIMIT 1`, targetID, scope(u)).Scan(&one) == nil
	}
	return false
}

// ================= Campuses =================

type Campus struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Classes  int    `json:"classes"`
	Students int    `json:"students"`
}

func (a *App) handleListCampuses(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.Query(`SELECT cp.id, cp.name,
		(SELECT COUNT(*) FROM classes c WHERE c.campus_id = cp.id),
		(SELECT COUNT(*) FROM students s JOIN classes c ON c.id = s.class_id WHERE c.campus_id = cp.id)
		FROM campuses cp WHERE (? = 0 OR cp.id = ?) ORDER BY cp.name`, scope(currentUser(r)), scope(currentUser(r)))
	if err != nil {
		serverError(w, err)
		return
	}
	defer rows.Close()
	list := []Campus{}
	for rows.Next() {
		var c Campus
		if err := rows.Scan(&c.ID, &c.Name, &c.Classes, &c.Students); err != nil {
			serverError(w, err)
			return
		}
		list = append(list, c)
	}
	writeJSON(w, list)
}

func (a *App) handleSaveCampus(w http.ResponseWriter, r *http.Request) {
	if !isTopAdmin(currentUser(r)) {
		httpError(w, http.StatusForbidden, "permission denied")
		return
	}
	var req struct{ Name string }
	if !readJSON(w, r, &req) {
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		httpError(w, http.StatusBadRequest, "name is required")
		return
	}
	var err error
	if id, ok := pathID(r, "id"); ok {
		_, err = a.db.Exec(`UPDATE campuses SET name = ? WHERE id = ?`, req.Name, id)
	} else {
		_, err = a.db.Exec(`INSERT INTO campuses (name) VALUES (?)`, req.Name)
	}
	if isUnique(err) {
		httpError(w, http.StatusConflict, "already exists")
		return
	}
	if err != nil {
		serverError(w, err)
		return
	}
	okJSON(w)
}

func (a *App) handleDeleteCampus(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok || !isTopAdmin(currentUser(r)) {
		httpError(w, http.StatusForbidden, "permission denied")
		return
	}
	var n int
	_ = a.db.QueryRow(`SELECT COUNT(*) FROM classes WHERE campus_id = ?`, id).Scan(&n)
	if n > 0 {
		httpError(w, http.StatusConflict, "campus still has classes")
		return
	}
	if _, err := a.db.Exec(`DELETE FROM campuses WHERE id = ?`, id); err != nil {
		serverError(w, err)
		return
	}
	okJSON(w)
}

// ================= Classes =================

type Class struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	CampusID    int64  `json:"campus_id"`
	CampusName  string `json:"campus_name"`
	TeacherID   int64  `json:"teacher_id"`
	TeacherName string `json:"teacher_name"`
	Students    int    `json:"students"`
}

func (a *App) listClasses(u *User) ([]Class, error) {
	rows, err := a.db.Query(`SELECT c.id, c.name, COALESCE(c.campus_id,0), COALESCE(cp.name,''), COALESCE(c.teacher_id,0), COALESCE(t.name,''),
		(SELECT COUNT(*) FROM students s WHERE s.class_id = c.id)
		FROM classes c LEFT JOIN users t ON t.id = c.teacher_id LEFT JOIN campuses cp ON cp.id = c.campus_id
		WHERE (? = 0 OR c.campus_id = ?) ORDER BY cp.name, c.name`, scope(u), scope(u))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []Class{}
	for rows.Next() {
		var c Class
		if err := rows.Scan(&c.ID, &c.Name, &c.CampusID, &c.CampusName, &c.TeacherID, &c.TeacherName, &c.Students); err != nil {
			return nil, err
		}
		list = append(list, c)
	}
	return list, rows.Err()
}

func (a *App) handleListClasses(w http.ResponseWriter, r *http.Request) {
	list, err := a.listClasses(currentUser(r))
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, list)
}

func (a *App) handleSaveClass(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	var req struct {
		Name      string `json:"name"`
		CampusID  int64  `json:"campus_id"`
		TeacherID int64  `json:"teacher_id"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if scope(u) != 0 {
		req.CampusID = scope(u)
	}
	var exists int
	if req.Name == "" || a.db.QueryRow(`SELECT 1 FROM campuses WHERE id = ?`, req.CampusID).Scan(&exists) != nil {
		httpError(w, http.StatusBadRequest, "name and campus are required")
		return
	}
	var err error
	if id, ok := pathID(r, "id"); ok {
		if !a.canAccessClass(u, id) {
			httpError(w, http.StatusForbidden, "permission denied")
			return
		}
		_, err = a.db.Exec(`UPDATE classes SET name = ?, campus_id = ?, teacher_id = ? WHERE id = ?`, req.Name, req.CampusID, nullInt(req.TeacherID), id)
	} else {
		_, err = a.db.Exec(`INSERT INTO classes (name, campus_id, teacher_id) VALUES (?,?,?)`, req.Name, req.CampusID, nullInt(req.TeacherID))
	}
	if isUnique(err) {
		httpError(w, http.StatusConflict, "already exists")
		return
	}
	if err != nil {
		serverError(w, err)
		return
	}
	okJSON(w)
}

func (a *App) handleDeleteClass(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok || !a.canAccessClass(currentUser(r), id) {
		httpError(w, http.StatusForbidden, "permission denied")
		return
	}
	if _, err := a.db.Exec(`DELETE FROM classes WHERE id = ?`, id); err != nil {
		serverError(w, err)
		return
	}
	okJSON(w)
}

// ================= Users =================

var usernameRe = regexp.MustCompile(`^[a-z0-9._-]{3,32}$`)

type UserRow struct {
	User
	Children []PhotoStudent `json:"children,omitempty"`
}

func (a *App) handleListUsers(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	q := `SELECT u.id, u.username, u.name, u.phone, u.email, u.role, COALESCE(u.campus_id,0), COALESCE(cp.name,''), u.disabled
		FROM users u LEFT JOIN campuses cp ON cp.id = u.campus_id WHERE 1=1`
	var args []any
	if role := r.URL.Query().Get("role"); role != "" {
		q += ` AND u.role = ?`
		args = append(args, role)
	}
	if s := scope(u); s != 0 {
		q += ` AND (u.campus_id = ? OR (u.role = 'parent' AND u.id IN (SELECT ps.parent_id FROM parent_students ps
			JOIN students st ON st.id = ps.student_id JOIN classes c ON c.id = st.class_id WHERE c.campus_id = ?)))`
		args = append(args, s, s)
	}
	rows, err := a.db.Query(q+` ORDER BY u.role, u.name`, args...)
	if err != nil {
		serverError(w, err)
		return
	}
	list := []UserRow{}
	index := map[int64]int{}
	for rows.Next() {
		var x UserRow
		if err := rows.Scan(&x.ID, &x.Username, &x.Name, &x.Phone, &x.Email, &x.Role, &x.CampusID, &x.CampusName, &x.Disabled); err != nil {
			rows.Close()
			serverError(w, err)
			return
		}
		index[x.ID] = len(list)
		list = append(list, x)
	}
	rows.Close()
	// Parents: which children they are linked to.
	rows, err = a.db.Query(`SELECT ps.parent_id, s.id, s.name FROM parent_students ps JOIN students s ON s.id = ps.student_id ORDER BY s.name`)
	if err != nil {
		serverError(w, err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var pid int64
		var c PhotoStudent
		if rows.Scan(&pid, &c.ID, &c.Name) == nil {
			if i, ok := index[pid]; ok {
				list[i].Children = append(list[i].Children, c)
			}
		}
	}
	writeJSON(w, list)
}

type userReq struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Name     string `json:"name"`
	Phone    string `json:"phone"`
	Email    string `json:"email"`
	Role     string `json:"role"`
	CampusID int64  `json:"campus_id"`
}

func (a *App) validCampus(id int64) bool {
	if id == 0 {
		return true
	}
	var one int
	return a.db.QueryRow(`SELECT 1 FROM campuses WHERE id = ?`, id).Scan(&one) == nil
}

func (a *App) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	var req userReq
	if !readJSON(w, r, &req) {
		return
	}
	req.Username = strings.ToLower(strings.TrimSpace(req.Username))
	req.Name = strings.TrimSpace(req.Name)
	if scope(u) != 0 {
		req.CampusID = scope(u) // campus admins can only add accounts to their own campus
	}
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
	case !a.validCampus(req.CampusID):
		httpError(w, http.StatusBadRequest, "invalid campus")
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
	if _, err := a.db.Exec(`UPDATE users SET email = ?, campus_id = ? WHERE id = ?`, strings.TrimSpace(req.Email), nullInt(req.CampusID), id); err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, map[string]int64{"id": id})
}

func (a *App) handleUpdateUser(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	id, ok := pathID(r, "id")
	if !ok || !a.userInScope(u, id) {
		httpError(w, http.StatusForbidden, "permission denied")
		return
	}
	var req userReq
	if !readJSON(w, r, &req) {
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		httpError(w, http.StatusBadRequest, "name is required")
		return
	}
	q := `UPDATE users SET name = ?, phone = ?, email = ? WHERE id = ?`
	args := []any{req.Name, strings.TrimSpace(req.Phone), strings.TrimSpace(req.Email), id}
	if isTopAdmin(u) { // only the top-level admin moves accounts between campuses
		if !a.validCampus(req.CampusID) {
			httpError(w, http.StatusBadRequest, "invalid campus")
			return
		}
		if id == u.ID && req.CampusID != 0 {
			httpError(w, http.StatusBadRequest, "cannot limit your own account to one campus")
			return
		}
		q = `UPDATE users SET name = ?, phone = ?, email = ?, campus_id = ? WHERE id = ?`
		args = []any{req.Name, strings.TrimSpace(req.Phone), strings.TrimSpace(req.Email), nullInt(req.CampusID), id}
	}
	if _, err := a.db.Exec(q, args...); err != nil {
		serverError(w, err)
		return
	}
	okJSON(w)
}

func (a *App) handleSetDisabled(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	id, ok := pathID(r, "id")
	if !ok || !a.userInScope(u, id) {
		httpError(w, http.StatusForbidden, "permission denied")
		return
	}
	if id == u.ID {
		httpError(w, http.StatusBadRequest, "cannot disable yourself")
		return
	}
	var req struct {
		Disabled bool `json:"disabled"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if _, err := a.db.Exec(`UPDATE users SET disabled = ? WHERE id = ?`, req.Disabled, id); err != nil {
		serverError(w, err)
		return
	}
	if req.Disabled {
		_, _ = a.db.Exec(`DELETE FROM sessions WHERE user_id = ?`, id)
		_, _ = a.db.Exec(`DELETE FROM push_subscriptions WHERE user_id = ?`, id)
	}
	okJSON(w)
}

func (a *App) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	id, ok := pathID(r, "id")
	if !ok || !a.userInScope(u, id) {
		httpError(w, http.StatusForbidden, "permission denied")
		return
	}
	if id == u.ID {
		httpError(w, http.StatusBadRequest, "cannot delete yourself")
		return
	}
	if _, err := a.db.Exec(`DELETE FROM users WHERE id = ?`, id); err != nil {
		serverError(w, err)
		return
	}
	okJSON(w)
}

func (a *App) handleResetPassword(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok || !a.userInScope(currentUser(r), id) {
		httpError(w, http.StatusForbidden, "permission denied")
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
	okJSON(w)
}

// ================= Students =================

type Student struct {
	ID         int64    `json:"id"`
	Name       string   `json:"name"`
	StudentNo  string   `json:"student_no"`
	ClassID    int64    `json:"class_id"`
	ClassName  string   `json:"class_name"`
	CampusID   int64    `json:"campus_id"`
	CampusName string   `json:"campus_name"`
	Parents    []Parent `json:"parents,omitempty"`
}

type Parent struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Phone string `json:"phone"`
	Email string `json:"email"`
}

// Profile is the student's file (学生档案).
type Profile struct {
	BirthDate      string `json:"birth_date"`
	Gender         string `json:"gender"`
	HealthNotes    string `json:"health_notes"`
	EmergencyName  string `json:"emergency_name"`
	EmergencyPhone string `json:"emergency_phone"`
	Notes          string `json:"notes"`
}

const studentCols = `s.id, s.name, s.student_no, COALESCE(s.class_id,0), COALESCE(c.name,''), COALESCE(c.campus_id,0), COALESCE(cp.name,'')`
const studentFrom = ` FROM students s LEFT JOIN classes c ON c.id = s.class_id LEFT JOIN campuses cp ON cp.id = c.campus_id `

func scanStudent(rows interface{ Scan(...any) error }, s *Student) error {
	return rows.Scan(&s.ID, &s.Name, &s.StudentNo, &s.ClassID, &s.ClassName, &s.CampusID, &s.CampusName)
}

func (a *App) queryStudents(where string, args ...any) ([]Student, error) {
	rows, err := a.db.Query(`SELECT `+studentCols+studentFrom+where+` ORDER BY cp.name, c.name, s.name`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []Student{}
	for rows.Next() {
		var s Student
		if err := scanStudent(rows, &s); err != nil {
			return nil, err
		}
		list = append(list, s)
	}
	return list, rows.Err()
}

func (a *App) parentsOf(studentIDs []int64) (map[int64][]Parent, error) {
	out := map[int64][]Parent{}
	if len(studentIDs) == 0 {
		return out, nil
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(studentIDs)), ",")
	args := make([]any, len(studentIDs))
	for i, id := range studentIDs {
		args[i] = id
	}
	rows, err := a.db.Query(`SELECT ps.student_id, u.id, u.name, u.phone, u.email FROM parent_students ps
		JOIN users u ON u.id = ps.parent_id WHERE ps.student_id IN (`+ph+`) ORDER BY u.name`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var sid int64
		var p Parent
		if err := rows.Scan(&sid, &p.ID, &p.Name, &p.Phone, &p.Email); err != nil {
			return nil, err
		}
		out[sid] = append(out[sid], p)
	}
	return out, rows.Err()
}

func (a *App) handleListStudents(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	where, args := []string{"(? = 0 OR c.campus_id = ?)"}, []any{scope(u), scope(u)}
	if cid := r.URL.Query().Get("class_id"); cid != "" {
		where, args = append(where, "s.class_id = ?"), append(args, cid)
	}
	if cp := r.URL.Query().Get("campus_id"); cp != "" {
		where, args = append(where, "c.campus_id = ?"), append(args, cp)
	}
	list, err := a.queryStudents("WHERE "+strings.Join(where, " AND "), args...)
	if err != nil {
		serverError(w, err)
		return
	}
	// Staff see the parents' contact details (学生家长联系方式).
	ids := make([]int64, len(list))
	for i, s := range list {
		ids[i] = s.ID
	}
	parents, err := a.parentsOf(ids)
	if err != nil {
		serverError(w, err)
		return
	}
	for i := range list {
		list[i].Parents = parents[list[i].ID]
	}
	writeJSON(w, list)
}

// handleStudentDetail: the student's file, class and parents. Staff of the campus, or the child's own parents.
func (a *App) handleStudentDetail(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok || !a.canAccessStudent(currentUser(r), id) {
		httpError(w, http.StatusForbidden, "permission denied")
		return
	}
	var s Student
	var p Profile
	err := a.db.QueryRow(`SELECT `+studentCols+`, s.birth_date, s.gender, s.health_notes, s.emergency_name, s.emergency_phone, s.notes`+
		studentFrom+`WHERE s.id = ?`, id).Scan(&s.ID, &s.Name, &s.StudentNo, &s.ClassID, &s.ClassName, &s.CampusID, &s.CampusName,
		&p.BirthDate, &p.Gender, &p.HealthNotes, &p.EmergencyName, &p.EmergencyPhone, &p.Notes)
	if err != nil {
		serverError(w, err)
		return
	}
	parents, err := a.parentsOf([]int64{id})
	if err != nil {
		serverError(w, err)
		return
	}
	s.Parents = parents[id]
	var teacher string
	_ = a.db.QueryRow(`SELECT COALESCE(t.name,'') FROM classes c LEFT JOIN users t ON t.id = c.teacher_id WHERE c.id = ?`, s.ClassID).Scan(&teacher)
	writeJSON(w, map[string]any{"student": s, "profile": p, "class_teacher": teacher})
}

type studentReq struct {
	Name      string `json:"name"`
	StudentNo string `json:"student_no"`
	ClassID   int64  `json:"class_id"`
	Profile
}

func (req *studentReq) clean() {
	req.Name, req.StudentNo = strings.TrimSpace(req.Name), strings.TrimSpace(req.StudentNo)
	for _, f := range []*string{&req.BirthDate, &req.Gender, &req.HealthNotes, &req.EmergencyName, &req.EmergencyPhone, &req.Notes} {
		*f = strings.TrimSpace(*f)
		if len(*f) > 1000 {
			*f = (*f)[:1000]
		}
	}
}

func (a *App) handleSaveStudent(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	var req studentReq
	if !readJSON(w, r, &req) {
		return
	}
	req.clean()
	if req.Name == "" || req.StudentNo == "" {
		httpError(w, http.StatusBadRequest, "name and student number are required")
		return
	}
	if !a.canAccessClass(u, req.ClassID) {
		httpError(w, http.StatusBadRequest, "please choose a class")
		return
	}
	id, editing := pathID(r, "id")
	var err error
	if editing {
		if !a.canAccessStudent(u, id) {
			httpError(w, http.StatusForbidden, "permission denied")
			return
		}
		_, err = a.db.Exec(`UPDATE students SET name = ?, student_no = ?, class_id = ?, birth_date = ?, gender = ?,
			health_notes = ?, emergency_name = ?, emergency_phone = ?, notes = ? WHERE id = ?`,
			req.Name, req.StudentNo, req.ClassID, req.BirthDate, req.Gender, req.HealthNotes, req.EmergencyName, req.EmergencyPhone, req.Notes, id)
	} else {
		var res sql.Result
		res, err = a.db.Exec(`INSERT INTO students (name, student_no, class_id, qr_token, birth_date, gender, health_notes,
			emergency_name, emergency_phone, notes) VALUES (?,?,?,?,?,?,?,?,?,?)`,
			req.Name, req.StudentNo, req.ClassID, randomToken(12), req.BirthDate, req.Gender, req.HealthNotes, req.EmergencyName, req.EmergencyPhone, req.Notes)
		if err == nil {
			id, _ = res.LastInsertId()
		}
	}
	if isUnique(err) {
		httpError(w, http.StatusConflict, "student number already exists")
		return
	}
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, map[string]int64{"id": id})
}

func (a *App) handleDeleteStudent(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok || !a.canAccessStudent(currentUser(r), id) {
		httpError(w, http.StatusForbidden, "permission denied")
		return
	}
	if _, err := a.db.Exec(`DELETE FROM students WHERE id = ?`, id); err != nil {
		serverError(w, err)
		return
	}
	okJSON(w)
}

func (a *App) handleLinkParent(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok || !a.canAccessStudent(currentUser(r), id) {
		httpError(w, http.StatusForbidden, "permission denied")
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
	okJSON(w)
}

func (a *App) handleUnlinkParent(w http.ResponseWriter, r *http.Request) {
	id, ok1 := pathID(r, "id")
	pid, ok2 := pathID(r, "pid")
	if !ok1 || !ok2 || !a.canAccessStudent(currentUser(r), id) {
		httpError(w, http.StatusForbidden, "permission denied")
		return
	}
	if _, err := a.db.Exec(`DELETE FROM parent_students WHERE parent_id = ? AND student_id = ?`, pid, id); err != nil {
		serverError(w, err)
		return
	}
	okJSON(w)
}

// handleResetQR issues a new QR token, so a lost card stops working.
func (a *App) handleResetQR(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok || !a.canAccessStudent(currentUser(r), id) {
		httpError(w, http.StatusForbidden, "permission denied")
		return
	}
	if _, err := a.db.Exec(`UPDATE students SET qr_token = ? WHERE id = ?`, randomToken(12), id); err != nil {
		serverError(w, err)
		return
	}
	okJSON(w)
}

func (a *App) handleStudentQR(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok || !a.canAccessStudent(currentUser(r), id) {
		httpError(w, http.StatusForbidden, "permission denied")
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
