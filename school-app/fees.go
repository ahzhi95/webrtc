package main

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Fee items (收费项目) are created by an admin — monthly tuition, event fees such as a graduation
// ceremony, half-yearly materials/photocopy fees — and billed to a campus, a class or chosen students.
// Each student gets a student_fees row with its own amount and status:
//   unpaid → pending (parent uploaded a receipt) → paid (receipt approved, or marked paid for cash)
//   waived: the school decided this student does not pay.

var feeCategories = map[string]bool{"monthly": true, "event": true, "misc": true, "other": true}

type FeeItem struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Category    string `json:"category"`
	AmountCents int64  `json:"amount_cents"`
	DueDate     string `json:"due_date"`
	CampusID    int64  `json:"campus_id"`
	CampusName  string `json:"campus_name"`
	ClassID     int64  `json:"class_id"`
	ClassName   string `json:"class_name"`
	CreatedAt   string `json:"created_at"`
	RemindedAt  string `json:"reminded_at"`
	RemindCount int    `json:"remind_count"`
	NeedsRemind bool   `json:"needs_remind"` // unpaid and past the due date, or a month old when there is no due date
	Total       int    `json:"total"`
	Paid        int    `json:"paid"`
	Pending     int    `json:"pending"`
	Unpaid      int    `json:"unpaid"`
	Waived      int    `json:"waived"`
	PaidCents   int64  `json:"paid_cents"`
	DueCents    int64  `json:"due_cents"`
}

// feeItemCampus: the campus an item belongs to (from its class when it targets one).
func (a *App) feeItemCampus(id int64) (int64, bool) {
	var campus int64
	err := a.db.QueryRow(`SELECT COALESCE(f.campus_id, c.campus_id, 0) FROM fee_items f LEFT JOIN classes c ON c.id = f.class_id WHERE f.id = ?`, id).Scan(&campus)
	return campus, err == nil
}

func (a *App) canAccessFeeItem(u *User, id int64) bool {
	campus, ok := a.feeItemCampus(id)
	return ok && (scope(u) == 0 || campus == scope(u))
}

func (a *App) handleListFeeItems(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	q := `SELECT f.id, f.name, f.category, f.amount_cents, f.due_date, COALESCE(f.campus_id, c.campus_id, 0), COALESCE(cp.name,''),
		COALESCE(f.class_id,0), COALESCE(c.name,''), f.created_at, f.reminded_at, f.remind_count,
		COUNT(sf.id), COALESCE(SUM(sf.status='paid'),0), COALESCE(SUM(sf.status='pending'),0), COALESCE(SUM(sf.status='unpaid'),0),
		COALESCE(SUM(sf.status='waived'),0),
		COALESCE(SUM(CASE WHEN sf.status='paid' THEN sf.amount_cents END),0),
		COALESCE(SUM(CASE WHEN sf.status IN ('unpaid','pending') THEN sf.amount_cents END),0)
		FROM fee_items f LEFT JOIN classes c ON c.id = f.class_id LEFT JOIN campuses cp ON cp.id = COALESCE(f.campus_id, c.campus_id)
		LEFT JOIN student_fees sf ON sf.fee_item_id = f.id
		WHERE (? = 0 OR COALESCE(f.campus_id, c.campus_id) = ?)`
	args := []any{scope(u), scope(u)}
	// Compare as a number: the COALESCE expression has no column type for SQLite to convert text to.
	if cp, _ := strconv.ParseInt(r.URL.Query().Get("campus_id"), 10, 64); cp > 0 {
		q += ` AND COALESCE(f.campus_id, c.campus_id) = ?`
		args = append(args, cp)
	}
	rows, err := a.db.Query(q+` GROUP BY f.id ORDER BY f.due_date DESC, f.id DESC LIMIT 300`, args...)
	if err != nil {
		serverError(w, err)
		return
	}
	defer rows.Close()
	list := []FeeItem{}
	for rows.Next() {
		var f FeeItem
		if err := rows.Scan(&f.ID, &f.Name, &f.Category, &f.AmountCents, &f.DueDate, &f.CampusID, &f.CampusName, &f.ClassID, &f.ClassName,
			&f.CreatedAt, &f.RemindedAt, &f.RemindCount, &f.Total, &f.Paid, &f.Pending, &f.Unpaid, &f.Waived, &f.PaidCents, &f.DueCents); err != nil {
			serverError(w, err)
			return
		}
		f.NeedsRemind = f.Unpaid > 0 && a.feeIsLate(f.DueDate, f.CreatedAt)
		list = append(list, f)
	}
	writeJSON(w, list)
}

// feeIsLate: past the due date, or — when the school set none — a month since the fee was billed.
func (a *App) feeIsLate(dueDate, createdAt string) bool {
	today := a.now().Format("2006-01-02")
	if dueDate != "" {
		return dueDate < today
	}
	return len(createdAt) >= 10 && createdAt[:10] <= a.now().AddDate(0, -1, 0).Format("2006-01-02")
}

const remindCooldown = 12 * time.Hour

// handleRemindFeeItem reminds the parents of every student who has not paid this fee yet (缴费提醒).
func (a *App) handleRemindFeeItem(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	id, ok := pathID(r, "id")
	if !ok || !a.canAccessFeeItem(u, id) {
		httpError(w, http.StatusForbidden, "permission denied")
		return
	}
	var name, last string
	var amount int64
	if err := a.db.QueryRow(`SELECT name, amount_cents, reminded_at FROM fee_items WHERE id = ?`, id).Scan(&name, &amount, &last); err != nil {
		serverError(w, err)
		return
	}
	if t, err := time.Parse(time.RFC3339, last); err == nil && time.Since(t) < remindCooldown {
		httpError(w, http.StatusConflict, "already reminded in the last 12 hours")
		return
	}
	rows, err := a.db.Query(`SELECT student_id FROM student_fees WHERE fee_item_id = ? AND status = 'unpaid'`, id)
	if err != nil {
		serverError(w, err)
		return
	}
	var students []int64
	for rows.Next() {
		var sid int64
		if rows.Scan(&sid) == nil {
			students = append(students, sid)
		}
	}
	rows.Close()
	if len(students) == 0 {
		httpError(w, http.StatusBadRequest, "everyone has paid")
		return
	}
	var parents int
	ph := strings.TrimSuffix(strings.Repeat("?,", len(students)), ",")
	args := make([]any, len(students))
	for i, s := range students {
		args[i] = s
	}
	_ = a.db.QueryRow(`SELECT COUNT(DISTINCT parent_id) FROM parent_students WHERE student_id IN (`+ph+`)`, args...).Scan(&parents)
	if _, err := a.db.Exec(`UPDATE fee_items SET reminded_at = ?, remind_count = remind_count + 1 WHERE id = ?`, a.nowStr(), id); err != nil {
		serverError(w, err)
		return
	}
	amountStr := a.currency + " " + centsStr(amount)
	go a.notifier.notifyParents(students, "缴费提醒 / Payment reminder", func(children string) string {
		return fmt.Sprintf("%s（%s）尚未缴付：%s。请在 App 缴费并上传付款凭证。\n%s for %s is still unpaid: %s. Please pay and upload the receipt in the app.",
			name, amountStr, children, name, children, amountStr)
	}, "/#receipts", fmt.Sprintf("remind-%d", id))
	writeJSON(w, map[string]int{"students": len(students), "parents": parents})
}

func (a *App) handleCreateFeeItem(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	var req struct {
		Name       string  `json:"name"`
		Category   string  `json:"category"`
		Amount     string  `json:"amount"`
		DueDate    string  `json:"due_date"`
		CampusID   int64   `json:"campus_id"`
		ClassID    int64   `json:"class_id"`
		StudentIDs []int64 `json:"student_ids"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	amount, err := parseAmount(req.Amount)
	switch {
	case req.Name == "" || len(req.Name) > 200:
		httpError(w, http.StatusBadRequest, "name is required")
		return
	case !feeCategories[req.Category]:
		httpError(w, http.StatusBadRequest, "invalid category")
		return
	case err != nil:
		httpError(w, http.StatusBadRequest, "invalid amount")
		return
	}
	if req.DueDate != "" {
		if _, err := time.Parse("2006-01-02", req.DueDate); err != nil {
			httpError(w, http.StatusBadRequest, "invalid date")
			return
		}
	}
	// Who is billed: chosen students, else a class, else a whole campus.
	var targets []Student
	switch {
	case len(req.StudentIDs) > 0:
		for _, id := range req.StudentIDs {
			if !a.canAccessStudent(u, id) {
				httpError(w, http.StatusForbidden, "permission denied")
				return
			}
		}
		ph := strings.TrimSuffix(strings.Repeat("?,", len(req.StudentIDs)), ",")
		args := make([]any, len(req.StudentIDs))
		for i, id := range req.StudentIDs {
			args[i] = id
		}
		targets, err = a.queryStudents("WHERE s.id IN ("+ph+")", args...)
		if err == nil && len(targets) > 0 {
			req.CampusID, req.ClassID = targets[0].CampusID, 0
		}
	case req.ClassID != 0:
		if !a.canAccessClass(u, req.ClassID) {
			httpError(w, http.StatusForbidden, "permission denied")
			return
		}
		targets, err = a.queryStudents("WHERE s.class_id = ?", req.ClassID)
		req.CampusID = 0
	default:
		if scope(u) != 0 {
			req.CampusID = scope(u)
		}
		if req.CampusID == 0 || !a.validCampus(req.CampusID) {
			httpError(w, http.StatusBadRequest, "please choose a campus, class or students")
			return
		}
		targets, err = a.queryStudents("WHERE c.campus_id = ?", req.CampusID)
	}
	if err != nil {
		serverError(w, err)
		return
	}
	if len(targets) == 0 {
		httpError(w, http.StatusBadRequest, "no students to bill")
		return
	}
	tx, err := a.db.Begin()
	if err != nil {
		serverError(w, err)
		return
	}
	defer tx.Rollback()
	res, err := tx.Exec(`INSERT INTO fee_items (name, category, amount_cents, due_date, campus_id, class_id, created_by, created_at)
		VALUES (?,?,?,?,?,?,?,?)`, req.Name, req.Category, amount, req.DueDate, nullInt(req.CampusID), nullInt(req.ClassID), u.ID, a.nowStr())
	if err != nil {
		serverError(w, err)
		return
	}
	itemID, _ := res.LastInsertId()
	ids := make([]int64, 0, len(targets))
	for _, s := range targets {
		if _, err := tx.Exec(`INSERT INTO student_fees (fee_item_id, student_id, amount_cents) VALUES (?,?,?)`, itemID, s.ID, amount); err != nil {
			serverError(w, err)
			return
		}
		ids = append(ids, s.ID)
	}
	if err := tx.Commit(); err != nil {
		serverError(w, err)
		return
	}
	due := ""
	if req.DueDate != "" {
		due = "，截止 " + req.DueDate + " / due " + req.DueDate
	}
	body := fmt.Sprintf("%s：%s %s%s", req.Name, a.currency, centsStr(amount), due)
	go a.notifier.notifyParents(ids, "新的收费项目 / New fee", func(string) string { return body }, "/#receipts", "fee")
	writeJSON(w, map[string]any{"id": itemID, "students": len(ids)})
}

func centsStr(c int64) string { return fmt.Sprintf("%d.%02d", c/100, c%100) }

func (a *App) handleUpdateFeeItem(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok || !a.canAccessFeeItem(currentUser(r), id) {
		httpError(w, http.StatusForbidden, "permission denied")
		return
	}
	var req struct {
		Name    string `json:"name"`
		DueDate string `json:"due_date"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		httpError(w, http.StatusBadRequest, "name is required")
		return
	}
	if req.DueDate != "" {
		if _, err := time.Parse("2006-01-02", req.DueDate); err != nil {
			httpError(w, http.StatusBadRequest, "invalid date")
			return
		}
	}
	if _, err := a.db.Exec(`UPDATE fee_items SET name = ?, due_date = ? WHERE id = ?`, req.Name, req.DueDate, id); err != nil {
		serverError(w, err)
		return
	}
	okJSON(w)
}

func (a *App) handleDeleteFeeItem(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok || !a.canAccessFeeItem(currentUser(r), id) {
		httpError(w, http.StatusForbidden, "permission denied")
		return
	}
	var n int
	_ = a.db.QueryRow(`SELECT COUNT(*) FROM student_fees WHERE fee_item_id = ? AND status IN ('paid','pending')`, id).Scan(&n)
	if n > 0 {
		httpError(w, http.StatusConflict, "fee already has payments")
		return
	}
	if _, err := a.db.Exec(`DELETE FROM fee_items WHERE id = ?`, id); err != nil {
		serverError(w, err)
		return
	}
	okJSON(w)
}

type StudentFee struct {
	ID          int64  `json:"id"`
	FeeItemID   int64  `json:"fee_item_id"`
	Name        string `json:"name"`
	Category    string `json:"category"`
	DueDate     string `json:"due_date"`
	StudentID   int64  `json:"student_id"`
	StudentName string `json:"student_name"`
	ClassName   string `json:"class_name"`
	AmountCents int64  `json:"amount_cents"`
	Status      string `json:"status"`
	PaidAt      string `json:"paid_at"`
	Note        string `json:"note"`
	ReceiptID   int64  `json:"receipt_id"` // official receipt, when paid
	ReceiptNo   string `json:"receipt_no"`
}

const studentFeeSelect = `SELECT sf.id, f.id, f.name, f.category, f.due_date, s.id, s.name, COALESCE(c.name,''),
	sf.amount_cents, sf.status, sf.paid_at, sf.note,
	COALESCE((SELECT r.id FROM official_receipt_items ri JOIN official_receipts r ON r.id = ri.official_receipt_id
		WHERE ri.student_fee_id = sf.id AND r.voided_at = '' ORDER BY r.id DESC LIMIT 1), 0),
	COALESCE((SELECT r.number FROM official_receipt_items ri JOIN official_receipts r ON r.id = ri.official_receipt_id
		WHERE ri.student_fee_id = sf.id AND r.voided_at = '' ORDER BY r.id DESC LIMIT 1), '')
	FROM student_fees sf JOIN fee_items f ON f.id = sf.fee_item_id JOIN students s ON s.id = sf.student_id
	LEFT JOIN classes c ON c.id = s.class_id `

func (a *App) queryStudentFees(where string, args ...any) ([]StudentFee, error) {
	rows, err := a.db.Query(studentFeeSelect+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []StudentFee{}
	for rows.Next() {
		var x StudentFee
		if err := rows.Scan(&x.ID, &x.FeeItemID, &x.Name, &x.Category, &x.DueDate, &x.StudentID, &x.StudentName, &x.ClassName,
			&x.AmountCents, &x.Status, &x.PaidAt, &x.Note, &x.ReceiptID, &x.ReceiptNo); err != nil {
			return nil, err
		}
		list = append(list, x)
	}
	return list, rows.Err()
}

// handleFeeItemStudents: every student billed for one item, with their status.
func (a *App) handleFeeItemStudents(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok || !a.canAccessFeeItem(currentUser(r), id) {
		httpError(w, http.StatusForbidden, "permission denied")
		return
	}
	list, err := a.queryStudentFees(`WHERE f.id = ? ORDER BY c.name, s.name`, id)
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, list)
}

// handleListFees: a parent's bills for their children (or, for staff, one student's bills).
func (a *App) handleListFees(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	var list []StudentFee
	var err error
	if sid, _ := strconv.ParseInt(r.URL.Query().Get("student_id"), 10, 64); sid > 0 {
		if !a.canAccessStudent(u, sid) {
			httpError(w, http.StatusForbidden, "permission denied")
			return
		}
		list, err = a.queryStudentFees(`WHERE s.id = ? ORDER BY f.due_date DESC, f.id DESC`, sid)
	} else if u.Role == "parent" {
		list, err = a.queryStudentFees(`WHERE s.id IN (SELECT student_id FROM parent_students WHERE parent_id = ?)
			ORDER BY (sf.status IN ('unpaid','pending')) DESC, f.due_date, f.id`, u.ID)
	} else {
		httpError(w, http.StatusBadRequest, "student_id is required")
		return
	}
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, list)
}

// handleUpdateStudentFee: adjust one student's amount (e.g. a sibling discount), mark paid by cash, or waive.
func (a *App) handleUpdateStudentFee(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	id, ok := pathID(r, "id")
	var studentID int64
	if !ok || a.db.QueryRow(`SELECT student_id FROM student_fees WHERE id = ?`, id).Scan(&studentID) != nil || !a.canAccessStudent(u, studentID) {
		httpError(w, http.StatusForbidden, "permission denied")
		return
	}
	var req struct {
		Amount string `json:"amount"`
		Status string `json:"status"`
		Note   string `json:"note"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if req.Amount != "" {
		amount, err := parseAmount(req.Amount)
		if err != nil {
			httpError(w, http.StatusBadRequest, "invalid amount")
			return
		}
		if _, err := a.db.Exec(`UPDATE student_fees SET amount_cents = ? WHERE id = ?`, amount, id); err != nil {
			serverError(w, err)
			return
		}
	}
	if req.Status != "" {
		if req.Status != "unpaid" && req.Status != "paid" && req.Status != "waived" {
			httpError(w, http.StatusBadRequest, "invalid status")
			return
		}
		var old string
		_ = a.db.QueryRow(`SELECT status FROM student_fees WHERE id = ?`, id).Scan(&old)
		paidAt := ""
		if req.Status == "paid" {
			paidAt = a.nowStr()
		}
		tx, err := a.db.Begin()
		if err != nil {
			serverError(w, err)
			return
		}
		defer tx.Rollback()
		if _, err := tx.Exec(`UPDATE student_fees SET status = ?, paid_at = ?, note = ? WHERE id = ?`,
			req.Status, paidAt, strings.TrimSpace(req.Note), id); err != nil {
			serverError(w, err)
			return
		}
		switch {
		case req.Status == "paid" && old != "paid":
			// Paid at the counter: issue a receipt for this fee.
			var name string
			var cents, payer int64
			_ = tx.QueryRow(`SELECT f.name, sf.amount_cents FROM student_fees sf JOIN fee_items f ON f.id = sf.fee_item_id WHERE sf.id = ?`, id).Scan(&name, &cents)
			_ = tx.QueryRow(`SELECT parent_id FROM parent_students WHERE student_id = ? ORDER BY parent_id LIMIT 1`, studentID).Scan(&payer)
			_, err = a.issueReceipt(tx, studentID, payer, 0, "cash", u.ID, []receiptLine{{studentFeeID: id, desc: name, cents: cents}})
		case req.Status != "paid" && old == "paid":
			err = a.voidReceipts(tx, u.ID, "付款已撤销 / payment undone",
				"r.id IN (SELECT official_receipt_id FROM official_receipt_items WHERE student_fee_id = ?)", id)
		}
		if err != nil {
			serverError(w, err)
			return
		}
		if err := tx.Commit(); err != nil {
			serverError(w, err)
			return
		}
	}
	okJSON(w)
}
