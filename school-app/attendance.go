package main

import (
	"database/sql"
	"net/http"
	"strings"
	"time"
)

// Arrival (到园) and departure (离园) records. One row per student per day.
// Teachers record them by scanning the student's QR card or by roll call on the class list.

const (
	modeIn  = "in"
	modeOut = "out"
)

type AttendanceResult struct {
	Student    Student `json:"student"`
	Mode       string  `json:"mode"`
	CheckinAt  string  `json:"checkin_at"`
	CheckoutAt string  `json:"checkout_at"`
	Late       bool    `json:"late"`
	Already    bool    `json:"already"` // this mode was already recorded today; nothing changed
}

// parseClock turns "HH:MM" on the given date into a time in the school's time zone.
func (a *App) parseClock(date, clock string) (time.Time, bool) {
	t, err := time.ParseInLocation("2006-01-02 15:04", date+" "+clock, a.loc)
	return t, err == nil
}

// record stores an arrival or departure for today. When at is zero the current time is used.
func (a *App) record(u *User, s Student, mode, method string, at time.Time) (AttendanceResult, error) {
	now := a.now()
	if at.IsZero() {
		at = now
	}
	date := now.Format("2006-01-02")
	out := AttendanceResult{Student: s, Mode: mode}
	ts := at.Format(time.RFC3339)
	var res sql.Result
	var err error
	if mode == modeIn {
		late := at.Format("15:04") > a.lateAfter
		res, err = a.db.Exec(`INSERT INTO attendance (student_id, date, checkin_at, late, method, scanned_by) VALUES (?,?,?,?,?,?)
			ON CONFLICT(student_id, date) DO UPDATE SET checkin_at = excluded.checkin_at, late = excluded.late,
			method = excluded.method, scanned_by = excluded.scanned_by WHERE attendance.checkin_at = ''`,
			s.ID, date, ts, late, method, u.ID)
	} else {
		res, err = a.db.Exec(`INSERT INTO attendance (student_id, date, checkin_at, checkout_at, checkout_method, checkout_by) VALUES (?,?,'',?,?,?)
			ON CONFLICT(student_id, date) DO UPDATE SET checkout_at = excluded.checkout_at,
			checkout_method = excluded.checkout_method, checkout_by = excluded.checkout_by WHERE attendance.checkout_at = ''`,
			s.ID, date, ts, method, u.ID)
	}
	if err != nil {
		return out, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		out.Already = true
	}
	if err := a.db.QueryRow(`SELECT checkin_at, checkout_at, late FROM attendance WHERE student_id = ? AND date = ?`, s.ID, date).
		Scan(&out.CheckinAt, &out.CheckoutAt, &out.Late); err != nil {
		return out, err
	}
	if !out.Already {
		go a.notifier.notifyAttendance(s, mode, at, out.Late)
	}
	return out, nil
}

func validMode(m string) bool { return m == modeIn || m == modeOut }

func (a *App) handleScan(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	var req struct{ Code, Mode string }
	if !readJSON(w, r, &req) {
		return
	}
	if req.Mode == "" {
		req.Mode = modeIn
	}
	token, ok := strings.CutPrefix(strings.TrimSpace(req.Code), qrPrefix)
	if !ok || token == "" || !validMode(req.Mode) {
		httpError(w, http.StatusBadRequest, "not a student card")
		return
	}
	list, err := a.queryStudents("WHERE s.qr_token = ?", token)
	if err != nil {
		serverError(w, err)
		return
	}
	if len(list) == 0 {
		httpError(w, http.StatusNotFound, "student not found")
		return
	}
	if !a.canAccessStudent(u, list[0].ID) {
		httpError(w, http.StatusForbidden, "student is in another campus")
		return
	}
	out, err := a.record(u, list[0], req.Mode, "qr", time.Time{})
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, map[string]any{"result": out, "siblings": a.siblings(u, list[0].ID, req.Mode)})
}

// Sibling is a brother or sister of a scanned child (they share a parent account), with whether
// today's arrival / departure is already recorded.
type Sibling struct {
	Student
	Done bool `json:"done"`
}

// siblings lists the other children of the same family that this staff member may record, so one
// scan at the gate can check in or out the whole family.
func (a *App) siblings(u *User, studentID int64, mode string) []Sibling {
	list, err := a.queryStudents(`WHERE s.id IN (SELECT ps2.student_id FROM parent_students ps1
		JOIN parent_students ps2 ON ps2.parent_id = ps1.parent_id WHERE ps1.student_id = ? AND ps2.student_id != ?)`, studentID, studentID)
	if err != nil {
		logError(err)
		return []Sibling{}
	}
	col := "checkin_at"
	if mode == modeOut {
		col = "checkout_at"
	}
	date := a.now().Format("2006-01-02")
	out := []Sibling{}
	for _, st := range list {
		if !a.canAccessStudent(u, st.ID) {
			continue
		}
		var v string
		_ = a.db.QueryRow(`SELECT `+col+` FROM attendance WHERE student_id = ? AND date = ?`, st.ID, date).Scan(&v)
		out = append(out, Sibling{Student: st, Done: v != ""})
	}
	return out
}

// handleMark records arrival/departure for one or many students: roll call (点名) on the class list,
// or a single student who forgot the card. Each entry may give its own time (HH:MM, today).
func (a *App) handleMark(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	var req struct {
		Mode         string `json:"mode"`
		WithSiblings bool   `json:"with_siblings"` // one child without a card: also list the brothers and sisters
		Entries      []struct {
			StudentID int64  `json:"student_id"`
			Time      string `json:"time"`
		} `json:"entries"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if !validMode(req.Mode) || len(req.Entries) == 0 || len(req.Entries) > 500 {
		httpError(w, http.StatusBadRequest, "invalid request")
		return
	}
	today := a.now().Format("2006-01-02")
	results := []AttendanceResult{}
	for _, e := range req.Entries {
		if !a.canAccessStudent(u, e.StudentID) {
			httpError(w, http.StatusForbidden, "permission denied")
			return
		}
		var at time.Time
		if e.Time != "" {
			t, ok := a.parseClock(today, e.Time)
			if !ok || t.After(a.now().Add(time.Minute)) {
				httpError(w, http.StatusBadRequest, "invalid time")
				return
			}
			at = t
		}
		list, err := a.queryStudents("WHERE s.id = ?", e.StudentID)
		if err != nil || len(list) == 0 {
			serverError(w, err)
			return
		}
		res, err := a.record(u, list[0], req.Mode, "manual", at)
		if err != nil {
			serverError(w, err)
			return
		}
		results = append(results, res)
	}
	recorded := 0
	for _, x := range results {
		if !x.Already {
			recorded++
		}
	}
	resp := map[string]any{"recorded": recorded, "already": len(results) - recorded, "results": results}
	if req.WithSiblings && len(results) == 1 {
		resp["siblings"] = a.siblings(u, results[0].Student.ID, req.Mode)
	}
	writeJSON(w, resp)
}

// handleEditAttendance corrects a submitted time. A reason is required and every change is kept (留痕).
func (a *App) handleEditAttendance(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	id, ok := pathID(r, "id")
	date := r.PathValue("date")
	if _, err := time.Parse("2006-01-02", date); !ok || err != nil {
		httpError(w, http.StatusBadRequest, "invalid request")
		return
	}
	if !a.canAccessStudent(u, id) {
		httpError(w, http.StatusForbidden, "permission denied")
		return
	}
	var req struct {
		Checkin  *string `json:"checkin"`  // "HH:MM", "" to clear, omitted = unchanged
		Checkout *string `json:"checkout"` // same
		Reason   string  `json:"reason"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	req.Reason = strings.TrimSpace(req.Reason)
	if req.Reason == "" {
		httpError(w, http.StatusBadRequest, "reason is required")
		return
	}
	var oldIn, oldOut string
	err := a.db.QueryRow(`SELECT checkin_at, checkout_at FROM attendance WHERE student_id = ? AND date = ?`, id, date).Scan(&oldIn, &oldOut)
	if err != nil && err != sql.ErrNoRows {
		serverError(w, err)
		return
	}
	toTS := func(clock *string, old string) (string, bool) {
		if clock == nil {
			return old, true
		}
		if *clock == "" {
			return "", true
		}
		t, ok := a.parseClock(date, *clock)
		if !ok || t.After(a.now().Add(time.Minute)) {
			return "", false
		}
		return t.Format(time.RFC3339), true
	}
	newIn, ok1 := toTS(req.Checkin, oldIn)
	newOut, ok2 := toTS(req.Checkout, oldOut)
	if !ok1 || !ok2 {
		httpError(w, http.StatusBadRequest, "invalid time")
		return
	}
	if newIn != "" && newOut != "" && newOut < newIn {
		httpError(w, http.StatusBadRequest, "departure is before arrival")
		return
	}
	if newIn == oldIn && newOut == oldOut {
		okJSON(w)
		return
	}
	late := newIn != "" && hhmm(newIn) > a.lateAfter
	tx, err := a.db.Begin()
	if err != nil {
		serverError(w, err)
		return
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO attendance (student_id, date, checkin_at, checkout_at, late, method, scanned_by, checkout_method, checkout_by)
		VALUES (?,?,?,?,?,'edit',?,'edit',?)
		ON CONFLICT(student_id, date) DO UPDATE SET checkin_at = excluded.checkin_at, checkout_at = excluded.checkout_at, late = excluded.late`,
		id, date, newIn, newOut, late, u.ID, u.ID); err != nil {
		serverError(w, err)
		return
	}
	for _, ch := range []struct{ field, old, new string }{{"checkin", oldIn, newIn}, {"checkout", oldOut, newOut}} {
		if ch.old == ch.new {
			continue
		}
		if _, err := tx.Exec(`INSERT INTO attendance_edits (student_id, date, field, old_value, new_value, reason, edited_by, edited_at)
			VALUES (?,?,?,?,?,?,?,?)`, id, date, ch.field, ch.old, ch.new, req.Reason, u.ID, a.nowStr()); err != nil {
			serverError(w, err)
			return
		}
	}
	// A day with neither time is no record at all.
	if _, err := tx.Exec(`DELETE FROM attendance WHERE student_id = ? AND date = ? AND checkin_at = '' AND checkout_at = ''`, id, date); err != nil {
		serverError(w, err)
		return
	}
	if err := tx.Commit(); err != nil {
		serverError(w, err)
		return
	}
	okJSON(w)
}

type AttendanceRow struct {
	Student
	CheckinAt  string `json:"checkin_at"`
	CheckoutAt string `json:"checkout_at"`
	Late       bool   `json:"late"`
	InMethod   string `json:"in_method"`
	OutMethod  string `json:"out_method"`
	InBy       string `json:"in_by"`
	OutBy      string `json:"out_by"`
	Edited     bool   `json:"edited"`
}

func (a *App) handleDailyAttendance(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	date := r.URL.Query().Get("date")
	if _, err := time.Parse("2006-01-02", date); err != nil {
		date = a.now().Format("2006-01-02")
	}
	q := `SELECT ` + studentCols + `, COALESCE(at.checkin_at,''), COALESCE(at.checkout_at,''), COALESCE(at.late,0),
		COALESCE(at.method,''), COALESCE(at.checkout_method,''), COALESCE(ib.name,''), COALESCE(ob.name,''),
		EXISTS (SELECT 1 FROM attendance_edits e WHERE e.student_id = s.id AND e.date = ?)` + studentFrom + `
		LEFT JOIN attendance at ON at.student_id = s.id AND at.date = ?
		LEFT JOIN users ib ON ib.id = at.scanned_by LEFT JOIN users ob ON ob.id = at.checkout_by
		WHERE (? = 0 OR c.campus_id = ?)`
	args := []any{date, date, scope(u), scope(u)}
	if cid := r.URL.Query().Get("class_id"); cid != "" {
		q += ` AND s.class_id = ?`
		args = append(args, cid)
	}
	if cp := r.URL.Query().Get("campus_id"); cp != "" {
		q += ` AND c.campus_id = ?`
		args = append(args, cp)
	}
	rows, err := a.db.Query(q+` ORDER BY cp.name, c.name, s.name`, args...)
	if err != nil {
		serverError(w, err)
		return
	}
	defer rows.Close()
	list := []AttendanceRow{}
	for rows.Next() {
		var x AttendanceRow
		if err := rows.Scan(&x.ID, &x.Name, &x.StudentNo, &x.ClassID, &x.ClassName, &x.CampusID, &x.CampusName,
			&x.CheckinAt, &x.CheckoutAt, &x.Late, &x.InMethod, &x.OutMethod, &x.InBy, &x.OutBy, &x.Edited); err != nil {
			serverError(w, err)
			return
		}
		list = append(list, x)
	}
	writeJSON(w, map[string]any{"date": date, "today": a.now().Format("2006-01-02"), "rows": list})
}

type AttendanceEdit struct {
	Date     string `json:"date"`
	Field    string `json:"field"`
	Old      string `json:"old"`
	New      string `json:"new"`
	Reason   string `json:"reason"`
	EditedBy string `json:"edited_by"`
	EditedAt string `json:"edited_at"`
}

// handleStudentAttendance: one student's records for a month (?month=YYYY-MM), with who recorded them and any corrections.
func (a *App) handleStudentAttendance(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok || !a.canAccessStudent(currentUser(r), id) {
		httpError(w, http.StatusForbidden, "permission denied")
		return
	}
	month := r.URL.Query().Get("month")
	if _, err := time.Parse("2006-01", month); err != nil {
		month = a.now().Format("2006-01")
	}
	rows, err := a.db.Query(`SELECT at.date, at.checkin_at, at.checkout_at, at.late, at.method, at.checkout_method,
		COALESCE(ib.name,''), COALESCE(ob.name,'')
		FROM attendance at LEFT JOIN users ib ON ib.id = at.scanned_by LEFT JOIN users ob ON ob.id = at.checkout_by
		WHERE at.student_id = ? AND at.date LIKE ? ORDER BY at.date DESC`, id, month+"-%")
	if err != nil {
		serverError(w, err)
		return
	}
	type rec struct {
		Date       string           `json:"date"`
		CheckinAt  string           `json:"checkin_at"`
		CheckoutAt string           `json:"checkout_at"`
		Late       bool             `json:"late"`
		InMethod   string           `json:"in_method"`
		OutMethod  string           `json:"out_method"`
		InBy       string           `json:"in_by"`
		OutBy      string           `json:"out_by"`
		Edits      []AttendanceEdit `json:"edits"`
	}
	list := []rec{}
	index := map[string]int{}
	for rows.Next() {
		var x rec
		if err := rows.Scan(&x.Date, &x.CheckinAt, &x.CheckoutAt, &x.Late, &x.InMethod, &x.OutMethod, &x.InBy, &x.OutBy); err != nil {
			rows.Close()
			serverError(w, err)
			return
		}
		x.Edits = []AttendanceEdit{}
		index[x.Date] = len(list)
		list = append(list, x)
	}
	rows.Close()
	rows, err = a.db.Query(`SELECT e.date, e.field, e.old_value, e.new_value, e.reason, COALESCE(u.name,''), e.edited_at
		FROM attendance_edits e LEFT JOIN users u ON u.id = e.edited_by
		WHERE e.student_id = ? AND e.date LIKE ? ORDER BY e.id`, id, month+"-%")
	if err != nil {
		serverError(w, err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var e AttendanceEdit
		if err := rows.Scan(&e.Date, &e.Field, &e.Old, &e.New, &e.Reason, &e.EditedBy, &e.EditedAt); err != nil {
			serverError(w, err)
			return
		}
		if i, ok := index[e.Date]; ok {
			list[i].Edits = append(list[i].Edits, e)
		} else { // the record was cleared; still show that it was
			list = append(list, rec{Date: e.Date, Edits: []AttendanceEdit{e}})
			index[e.Date] = len(list) - 1
		}
	}
	writeJSON(w, map[string]any{"month": month, "records": list})
}
