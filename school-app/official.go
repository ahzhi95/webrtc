package main

import (
	"database/sql"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// Official receipts (正式收据) are issued by the school once a payment is confirmed:
// when an admin approves an uploaded proof of payment, or records a cash payment.
// Each campus numbers its own receipts per year, e.g. TTM-2026-0001. A receipt is never deleted:
// if the payment is later undone it is voided (作废) with a reason, so the numbering has no gaps.
// The PDF is drawn on the parent's phone from this data (phones have Chinese fonts), see web/fees.js.

type OfficialReceiptItem struct {
	Description string `json:"description"`
	AmountCents int64  `json:"amount_cents"`
}

type OfficialReceipt struct {
	ID          int64                 `json:"id"`
	Number      string                `json:"number"`
	StudentID   int64                 `json:"student_id"`
	StudentName string                `json:"student_name"`
	StudentNo   string                `json:"student_no"`
	ClassName   string                `json:"class_name"`
	PayerName   string                `json:"payer_name"`
	Method      string                `json:"method"`
	TotalCents  int64                 `json:"total_cents"`
	IssuedAt    string                `json:"issued_at"`
	IssuedBy    string                `json:"issued_by"`
	VoidedAt    string                `json:"voided_at"`
	VoidReason  string                `json:"void_reason"`
	Items       []OfficialReceiptItem `json:"items"`
	School      CampusInfo            `json:"school"`
}

// CampusInfo is what a receipt prints about the school.
type CampusInfo struct {
	Name          string `json:"name"`
	RegNo         string `json:"reg_no"`
	Address       string `json:"address"`
	Phone         string `json:"phone"`
	Email         string `json:"email"`
	ReceiptPrefix string `json:"receipt_prefix"`
}

// defaultPrefix: initials of the campus name, "Tadika Taman Merak" → "TTM".
func defaultPrefix(name string) string {
	var b strings.Builder
	for _, w := range strings.Fields(name) {
		r := []rune(w)[0]
		if r < 128 {
			b.WriteRune(r)
		}
	}
	p := strings.ToUpper(b.String())
	if p == "" {
		p = "R"
	}
	return p
}

type receiptLine struct {
	studentFeeID int64 // 0 for a payment that is not one of the listed fees
	desc         string
	cents        int64
}

// issueReceipt creates a numbered receipt inside the caller's transaction.
func (a *App) issueReceipt(tx *sql.Tx, studentID, payerID, uploadID int64, method string, issuedBy int64, lines []receiptLine) (int64, error) {
	var campusID int64
	var campusName, prefix string
	err := tx.QueryRow(`SELECT COALESCE(cp.id,0), COALESCE(cp.name,''), COALESCE(cp.receipt_prefix,'') FROM students s
		LEFT JOIN classes c ON c.id = s.class_id LEFT JOIN campuses cp ON cp.id = c.campus_id WHERE s.id = ?`, studentID).
		Scan(&campusID, &campusName, &prefix)
	if err != nil {
		return 0, err
	}
	if prefix == "" {
		prefix = defaultPrefix(campusName)
	}
	year := a.now().Year()
	// Next number for this campus and year.
	if _, err := tx.Exec(`INSERT INTO receipt_counters (campus_id, year, last) VALUES (?,?,1)
		ON CONFLICT(campus_id, year) DO UPDATE SET last = last + 1`, campusID, year); err != nil {
		return 0, err
	}
	var seq int
	if err := tx.QueryRow(`SELECT last FROM receipt_counters WHERE campus_id = ? AND year = ?`, campusID, year).Scan(&seq); err != nil {
		return 0, err
	}
	number := fmt.Sprintf("%s-%d-%04d", prefix, year, seq)
	var total int64
	for _, l := range lines {
		total += l.cents
	}
	res, err := tx.Exec(`INSERT INTO official_receipts (number, campus_id, student_id, payer_id, upload_id, method, total_cents, issued_at, issued_by)
		VALUES (?,?,?,?,?,?,?,?,?)`, number, nullInt(campusID), studentID, nullInt(payerID), nullInt(uploadID), method, total, a.nowStr(), issuedBy)
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	for _, l := range lines {
		if _, err := tx.Exec(`INSERT INTO official_receipt_items (official_receipt_id, student_fee_id, description, amount_cents) VALUES (?,?,?,?)`,
			id, nullInt(l.studentFeeID), l.desc, l.cents); err != nil {
			return 0, err
		}
	}
	return id, nil
}

// voidReceipts voids the active receipts matching where (a condition on official_receipts r).
func (a *App) voidReceipts(tx *sql.Tx, by int64, reason, where string, args ...any) error {
	_, err := tx.Exec(`UPDATE official_receipts SET voided_at = ?, void_reason = ?, voided_by = ?
		WHERE voided_at = '' AND id IN (SELECT r.id FROM official_receipts r WHERE `+where+`)`,
		append([]any{a.nowStr(), reason, by}, args...)...)
	return err
}

// issueForUpload: receipt for an approved proof of payment, listing the fees it paid.
func (a *App) issueForUpload(tx *sql.Tx, uploadID, issuedBy int64) error {
	var exists int
	if tx.QueryRow(`SELECT 1 FROM official_receipts WHERE upload_id = ? AND voided_at = ''`, uploadID).Scan(&exists) == nil {
		return nil // already issued
	}
	var studentID, parentID, amount int64
	var desc string
	if err := tx.QueryRow(`SELECT student_id, COALESCE(parent_id,0), amount_cents, description FROM receipts WHERE id = ?`, uploadID).
		Scan(&studentID, &parentID, &amount, &desc); err != nil {
		return err
	}
	rows, err := tx.Query(`SELECT sf.id, f.name, sf.amount_cents FROM receipt_fees rf JOIN student_fees sf ON sf.id = rf.student_fee_id
		JOIN fee_items f ON f.id = sf.fee_item_id WHERE rf.receipt_id = ? AND sf.status = 'paid' ORDER BY f.due_date, f.id`, uploadID)
	if err != nil {
		return err
	}
	var lines []receiptLine
	for rows.Next() {
		var l receiptLine
		if err := rows.Scan(&l.studentFeeID, &l.desc, &l.cents); err != nil {
			rows.Close()
			return err
		}
		lines = append(lines, l)
	}
	rows.Close()
	if len(lines) == 0 { // a payment that was not one of the listed fees
		lines = []receiptLine{{desc: desc, cents: amount}}
	}
	_, err = a.issueReceipt(tx, studentID, parentID, uploadID, "transfer", issuedBy, lines)
	return err
}

// ---------- HTTP ----------

const officialSelect = `SELECT r.id, r.number, s.id, s.name, s.student_no, COALESCE(c.name,''), COALESCE(p.name,''), r.method, r.total_cents,
	r.issued_at, COALESCE(ib.name,''), r.voided_at, r.void_reason,
	COALESCE(cp.name,''), COALESCE(cp.reg_no,''), COALESCE(cp.address,''), COALESCE(cp.phone,''), COALESCE(cp.email,''), COALESCE(cp.receipt_prefix,'')
	FROM official_receipts r JOIN students s ON s.id = r.student_id LEFT JOIN classes c ON c.id = s.class_id
	LEFT JOIN campuses cp ON cp.id = r.campus_id LEFT JOIN users p ON p.id = r.payer_id LEFT JOIN users ib ON ib.id = r.issued_by `

func (a *App) queryOfficial(where string, args ...any) ([]OfficialReceipt, error) {
	rows, err := a.db.Query(officialSelect+where, args...)
	if err != nil {
		return nil, err
	}
	list := []OfficialReceipt{}
	index := map[int64]int{}
	for rows.Next() {
		var x OfficialReceipt
		if err := rows.Scan(&x.ID, &x.Number, &x.StudentID, &x.StudentName, &x.StudentNo, &x.ClassName, &x.PayerName, &x.Method, &x.TotalCents,
			&x.IssuedAt, &x.IssuedBy, &x.VoidedAt, &x.VoidReason,
			&x.School.Name, &x.School.RegNo, &x.School.Address, &x.School.Phone, &x.School.Email, &x.School.ReceiptPrefix); err != nil {
			rows.Close()
			return nil, err
		}
		x.Items = []OfficialReceiptItem{}
		index[x.ID] = len(list)
		list = append(list, x)
	}
	rows.Close()
	for i := range list {
		items, err := a.db.Query(`SELECT description, amount_cents FROM official_receipt_items WHERE official_receipt_id = ? ORDER BY rowid`, list[i].ID)
		if err != nil {
			return nil, err
		}
		for items.Next() {
			var it OfficialReceiptItem
			if err := items.Scan(&it.Description, &it.AmountCents); err != nil {
				items.Close()
				return nil, err
			}
			list[i].Items = append(list[i].Items, it)
		}
		items.Close()
	}
	return list, nil
}

// handleListOfficial: receipts of one student (?student_id), or all of a parent's children.
func (a *App) handleListOfficial(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	var list []OfficialReceipt
	var err error
	if sid, _ := strconv.ParseInt(r.URL.Query().Get("student_id"), 10, 64); sid > 0 {
		if !a.canAccessStudent(u, sid) {
			httpError(w, http.StatusForbidden, "permission denied")
			return
		}
		list, err = a.queryOfficial(`WHERE r.student_id = ? ORDER BY r.id DESC`, sid)
	} else if u.Role == "parent" {
		list, err = a.queryOfficial(`WHERE r.student_id IN (SELECT student_id FROM parent_students WHERE parent_id = ?) ORDER BY r.id DESC LIMIT 200`, u.ID)
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

func (a *App) handleGetOfficial(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		httpError(w, http.StatusBadRequest, "invalid id")
		return
	}
	list, err := a.queryOfficial(`WHERE r.id = ?`, id)
	if err != nil {
		serverError(w, err)
		return
	}
	if len(list) == 0 || !a.canAccessStudent(currentUser(r), list[0].StudentID) {
		httpError(w, http.StatusNotFound, "not found")
		return
	}
	writeJSON(w, list[0])
}

// receiptIDsForUploads maps proof-of-payment uploads to the official receipt issued for them.
func (a *App) receiptIDsForUploads(ids []int64) map[int64][2]string {
	out := map[int64][2]string{}
	if len(ids) == 0 {
		return out
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := a.db.Query(`SELECT upload_id, id, number FROM official_receipts WHERE voided_at = '' AND upload_id IN (`+ph+`)`, args...)
	if err != nil {
		logError(err)
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var up, id int64
		var num string
		if rows.Scan(&up, &id, &num) == nil {
			out[up] = [2]string{strconv.FormatInt(id, 10), num}
		}
	}
	return out
}
