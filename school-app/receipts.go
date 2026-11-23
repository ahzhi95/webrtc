package main

import (
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Payment receipts (缴费凭证): a parent uploads proof of payment, usually for one or more of the
// child's unpaid fees; the school approves (fees become paid) or rejects with a reason.

const maxUpload = 10 << 20 // 10 MB

var allowedTypes = map[string]string{
	"image/jpeg":      ".jpg",
	"image/png":       ".png",
	"image/webp":      ".webp",
	"application/pdf": ".pdf",
}

type ReceiptFee struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	AmountCents int64  `json:"amount_cents"`
}

type Receipt struct {
	ID          int64        `json:"id"`
	StudentID   int64        `json:"student_id"`
	StudentName string       `json:"student_name"`
	ClassName   string       `json:"class_name"`
	CampusName  string       `json:"campus_name"`
	ParentName  string       `json:"parent_name"`
	ParentPhone string       `json:"parent_phone"`
	Description string       `json:"description"`
	AmountCents int64        `json:"amount_cents"`
	FileType    string       `json:"file_type"`
	Status      string       `json:"status"`
	ReviewNote  string       `json:"review_note"`
	ReviewedBy  string       `json:"reviewed_by"`
	ReviewedAt  string       `json:"reviewed_at"`
	CreatedAt   string       `json:"created_at"`
	Fees        []ReceiptFee `json:"fees"`
	OfficialID  string       `json:"official_id"`
	OfficialNo  string       `json:"official_no"`
	Batch       string       `json:"batch,omitempty"`
	// Other children paid with the same transfer (shown to the school, which reviews them together).
	BatchWith  []string `json:"batch_with,omitempty"`
	BatchTotal int64    `json:"batch_total,omitempty"`
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

	// The fees this receipt pays for: the parent's own children's, not already paid or waiting.
	// One bank transfer may cover several brothers and sisters at the same school.
	var fees []StudentFee
	var campus int64
	for _, v := range r.MultipartForm.Value["fee_id"] {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			continue
		}
		list, err := a.queryStudentFees(`WHERE sf.id = ?`, id)
		if err != nil {
			serverError(w, err)
			return
		}
		if len(list) == 0 || !a.parentOwns(u.ID, list[0].StudentID) || (studentID != 0 && list[0].StudentID != studentID) {
			httpError(w, http.StatusForbidden, "permission denied")
			return
		}
		if list[0].Status != "unpaid" {
			httpError(w, http.StatusBadRequest, "fee is not open for payment")
			return
		}
		c, _ := a.feeItemCampus(list[0].FeeItemID)
		if campus != 0 && c != campus {
			httpError(w, http.StatusBadRequest, "one payment can only be for one school")
			return
		}
		campus = c
		fees = append(fees, list[0])
	}
	if len(fees) == 0 && !a.parentOwns(u.ID, studentID) {
		httpError(w, http.StatusForbidden, "permission denied")
		return
	}

	// Group by child, keeping the order the fees were ticked.
	type part struct {
		studentID int64
		fees      []StudentFee
	}
	var parts []*part
	byStudent := map[int64]*part{}
	for _, f := range fees {
		p := byStudent[f.StudentID]
		if p == nil {
			p = &part{studentID: f.StudentID}
			byStudent[f.StudentID] = p
			parts = append(parts, p)
		}
		p.fees = append(p.fees, f)
	}
	if len(parts) == 0 {
		parts = []*part{{studentID: studentID}}
	}

	note := strings.TrimSpace(r.FormValue("note")) // added to each child's description
	desc := strings.TrimSpace(r.FormValue("description"))
	describe := func(p *part) string {
		var names []string
		for _, f := range p.fees {
			names = append(names, f.Name)
		}
		d := desc
		if len(parts) > 1 || d == "" {
			d = strings.Join(names, "、")
		}
		if note != "" {
			d += " — " + note
		}
		return d
	}
	for _, p := range parts {
		if d := describe(p); d == "" || len(d) > 300 {
			httpError(w, http.StatusBadRequest, "description is required")
			return
		}
	}
	// A typed amount is only for a single child (e.g. a payment not on the list).
	var amount int64
	if v := strings.TrimSpace(r.FormValue("amount")); len(parts) == 1 && (v != "" || len(fees) == 0) {
		var err error
		if amount, err = parseAmount(v); err != nil {
			httpError(w, http.StatusBadRequest, "invalid amount")
			return
		}
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

	tx, err := a.db.Begin()
	if err != nil {
		os.Remove(out.Name())
		serverError(w, err)
		return
	}
	defer tx.Rollback()
	batch := ""
	if len(parts) > 1 {
		batch = randomToken(8)
	}
	for _, p := range parts {
		amt := amount
		if len(p.fees) > 0 && (amt == 0 || len(parts) > 1) {
			amt = 0
			for _, f := range p.fees {
				amt += f.AmountCents
			}
		}
		res, err := tx.Exec(`INSERT INTO receipts (student_id, parent_id, description, amount_cents, file_name, file_type, created_at, batch)
			VALUES (?,?,?,?,?,?,?,?)`, p.studentID, u.ID, describe(p), amt, name, ctype, a.nowStr(), batch)
		if err != nil {
			os.Remove(out.Name())
			serverError(w, err)
			return
		}
		rid, _ := res.LastInsertId()
		for _, f := range p.fees {
			if _, err := tx.Exec(`INSERT INTO receipt_fees (receipt_id, student_fee_id) VALUES (?,?)`, rid, f.ID); err != nil {
				os.Remove(out.Name())
				serverError(w, err)
				return
			}
			if _, err := tx.Exec(`UPDATE student_fees SET status = 'pending' WHERE id = ?`, f.ID); err != nil {
				os.Remove(out.Name())
				serverError(w, err)
				return
			}
		}
	}
	if err := tx.Commit(); err != nil {
		os.Remove(out.Name())
		serverError(w, err)
		return
	}
	okJSON(w)
}

func (a *App) handleListReceipts(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	q := `SELECT rc.id, rc.student_id, s.name, COALESCE(c.name,''), COALESCE(cp.name,''), COALESCE(p.name,''), COALESCE(p.phone,''),
		rc.description, rc.amount_cents, rc.file_type, rc.status, rc.review_note, COALESCE(rv.name,''),
		COALESCE(rc.reviewed_at,''), rc.created_at, rc.batch
		FROM receipts rc JOIN students s ON s.id = rc.student_id LEFT JOIN classes c ON c.id = s.class_id
		LEFT JOIN campuses cp ON cp.id = c.campus_id
		LEFT JOIN users p ON p.id = rc.parent_id LEFT JOIN users rv ON rv.id = rc.reviewed_by WHERE 1=1`
	var args []any
	if u.Role == "parent" {
		q += ` AND rc.student_id IN (SELECT student_id FROM parent_students WHERE parent_id = ?)`
		args = append(args, u.ID)
	} else if scope(u) != 0 {
		q += ` AND c.campus_id = ?`
		args = append(args, scope(u))
	}
	if st := r.URL.Query().Get("status"); st != "" {
		q += ` AND rc.status = ?`
		args = append(args, st)
	}
	if cp := r.URL.Query().Get("campus_id"); cp != "" && u.Role != "parent" {
		q += ` AND c.campus_id = ?`
		args = append(args, cp)
	}
	rows, err := a.db.Query(q+` ORDER BY rc.created_at DESC LIMIT 500`, args...)
	if err != nil {
		serverError(w, err)
		return
	}
	list := []Receipt{}
	index := map[int64]int{}
	for rows.Next() {
		var x Receipt
		if err := rows.Scan(&x.ID, &x.StudentID, &x.StudentName, &x.ClassName, &x.CampusName, &x.ParentName, &x.ParentPhone,
			&x.Description, &x.AmountCents, &x.FileType, &x.Status, &x.ReviewNote, &x.ReviewedBy, &x.ReviewedAt, &x.CreatedAt, &x.Batch); err != nil {
			rows.Close()
			serverError(w, err)
			return
		}
		x.Fees = []ReceiptFee{}
		index[x.ID] = len(list)
		list = append(list, x)
	}
	rows.Close()
	ids := make([]int64, len(list))
	for i, x := range list {
		ids[i] = x.ID
	}
	for i, x := range list {
		if x.Batch == "" {
			continue
		}
		rows, err := a.db.Query(`SELECT r.id, s.name, r.amount_cents FROM receipts r JOIN students s ON s.id = r.student_id WHERE r.batch = ?`, x.Batch)
		if err != nil {
			serverError(w, err)
			return
		}
		for rows.Next() {
			var rid, amt int64
			var sn string
			if rows.Scan(&rid, &sn, &amt) == nil {
				list[i].BatchTotal += amt
				if rid != x.ID {
					list[i].BatchWith = append(list[i].BatchWith, sn)
				}
			}
		}
		rows.Close()
	}
	for up, v := range a.receiptIDsForUploads(ids) {
		list[index[up]].OfficialID, list[index[up]].OfficialNo = v[0], v[1]
	}
	if len(list) > 0 {
		rows, err := a.db.Query(`SELECT rf.receipt_id, sf.id, f.name, sf.amount_cents FROM receipt_fees rf
			JOIN student_fees sf ON sf.id = rf.student_fee_id JOIN fee_items f ON f.id = sf.fee_item_id
			WHERE rf.receipt_id BETWEEN ? AND ?`, list[len(list)-1].ID, list[0].ID)
		if err != nil {
			serverError(w, err)
			return
		}
		defer rows.Close()
		for rows.Next() {
			var rid int64
			var f ReceiptFee
			if rows.Scan(&rid, &f.ID, &f.Name, &f.AmountCents) == nil {
				if i, ok := index[rid]; ok {
					list[i].Fees = append(list[i].Fees, f)
				}
			}
		}
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
	if !a.canAccessStudent(currentUser(r), studentID) {
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
	u := currentUser(r)
	id, ok := pathID(r, "id")
	var studentID int64
	var desc, batch string
	if !ok || a.db.QueryRow(`SELECT student_id, description, batch FROM receipts WHERE id = ?`, id).Scan(&studentID, &desc, &batch) != nil ||
		!a.canAccessStudent(u, studentID) {
		httpError(w, http.StatusForbidden, "permission denied")
		return
	}
	// One transfer for brothers and sisters: the school confirms or returns it as a whole,
	// and each child still gets its own official receipt.
	ids, students, descs := []int64{id}, []int64{studentID}, []string{desc}
	if batch != "" {
		ids, students, descs = nil, nil, nil
		rows, err := a.db.Query(`SELECT r.id, r.student_id, r.description, s.name FROM receipts r JOIN students s ON s.id = r.student_id
			WHERE r.batch = ? ORDER BY r.id`, batch)
		if err != nil {
			serverError(w, err)
			return
		}
		for rows.Next() {
			var rid, sid int64
			var d, sn string
			if rows.Scan(&rid, &sid, &d, &sn) == nil {
				ids, students, descs = append(ids, rid), append(students, sid), append(descs, sn+"："+d)
			}
		}
		rows.Close()
		for _, sid := range students {
			if !a.canAccessStudent(u, sid) {
				httpError(w, http.StatusForbidden, "permission denied")
				return
			}
		}
		desc = strings.Join(descs, "；")
	}
	var req struct {
		Status string `json:"status"`
		Note   string `json:"note"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	req.Note = strings.TrimSpace(req.Note)
	if req.Status != "approved" && req.Status != "rejected" && req.Status != "pending" {
		httpError(w, http.StatusBadRequest, "invalid status")
		return
	}
	if req.Status == "rejected" && req.Note == "" {
		httpError(w, http.StatusBadRequest, "reason is required")
		return
	}
	tx, err := a.db.Begin()
	if err != nil {
		serverError(w, err)
		return
	}
	defer tx.Rollback()
	feeStatus, paidAt := map[string]string{"approved": "paid", "rejected": "unpaid", "pending": "pending"}[req.Status], ""
	if feeStatus == "paid" {
		paidAt = a.nowStr()
	}
	for _, rid := range ids {
		if _, err := tx.Exec(`UPDATE receipts SET status = ?, review_note = ?, reviewed_by = ?, reviewed_at = ? WHERE id = ?`,
			req.Status, req.Note, u.ID, a.nowStr(), rid); err != nil {
			serverError(w, err)
			return
		}
		// The fees on this receipt follow its outcome.
		if _, err := tx.Exec(`UPDATE student_fees SET status = ?, paid_at = ?
			WHERE id IN (SELECT student_fee_id FROM receipt_fees WHERE receipt_id = ?) AND status != 'waived'`, feeStatus, paidAt, rid); err != nil {
			serverError(w, err)
			return
		}
		// Approving issues the official receipt; undoing an approval voids it.
		if req.Status == "approved" {
			err = a.issueForUpload(tx, rid, u.ID)
		} else {
			err = a.voidReceipts(tx, u.ID, "付款凭证已改为"+map[string]string{"rejected": "退回", "pending": "待确认"}[req.Status]+" / payment proof "+req.Status,
				"r.upload_id = ?", rid)
		}
		if err != nil {
			serverError(w, err)
			return
		}
	}
	if err := tx.Commit(); err != nil {
		serverError(w, err)
		return
	}
	if req.Status != "pending" {
		title, body := "缴费已确认 / Payment confirmed", "学校已确认收到："+desc+"\n可以在 App 下载收据。Your receipt is ready to download."
		if req.Status == "rejected" {
			title, body = "缴费凭证被退回 / Receipt returned", desc+"\n原因 Reason: "+req.Note
		}
		go a.notifier.notifyParents(students, title, func(string) string { return body }, "/#receipts", fmt.Sprintf("receipt-%d", id))
	}
	okJSON(w)
}
