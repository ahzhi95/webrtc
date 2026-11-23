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

	// The fees this receipt pays for: must be this child's and not already paid or waiting.
	var fees []StudentFee
	for _, v := range r.MultipartForm.Value["fee_id"] {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			continue
		}
		list, err := a.queryStudentFees(`WHERE sf.id = ? AND sf.student_id = ?`, id, studentID)
		if err != nil {
			serverError(w, err)
			return
		}
		if len(list) == 0 || list[0].Status != "unpaid" {
			httpError(w, http.StatusBadRequest, "fee is not open for payment")
			return
		}
		fees = append(fees, list[0])
	}

	desc := strings.TrimSpace(r.FormValue("description"))
	var feeTotal int64
	var names []string
	for _, f := range fees {
		feeTotal += f.AmountCents
		names = append(names, f.Name)
	}
	if desc == "" {
		desc = strings.Join(names, "、")
	}
	if desc == "" || len(desc) > 300 {
		httpError(w, http.StatusBadRequest, "description is required")
		return
	}
	amount := feeTotal
	if v := strings.TrimSpace(r.FormValue("amount")); v != "" || len(fees) == 0 {
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
	res, err := tx.Exec(`INSERT INTO receipts (student_id, parent_id, description, amount_cents, file_name, file_type, created_at)
		VALUES (?,?,?,?,?,?,?)`, studentID, u.ID, desc, amount, name, ctype, a.nowStr())
	if err != nil {
		os.Remove(out.Name())
		serverError(w, err)
		return
	}
	rid, _ := res.LastInsertId()
	for _, f := range fees {
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
		COALESCE(rc.reviewed_at,''), rc.created_at
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
			&x.Description, &x.AmountCents, &x.FileType, &x.Status, &x.ReviewNote, &x.ReviewedBy, &x.ReviewedAt, &x.CreatedAt); err != nil {
			rows.Close()
			serverError(w, err)
			return
		}
		x.Fees = []ReceiptFee{}
		index[x.ID] = len(list)
		list = append(list, x)
	}
	rows.Close()
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
	var desc string
	if !ok || a.db.QueryRow(`SELECT student_id, description FROM receipts WHERE id = ?`, id).Scan(&studentID, &desc) != nil ||
		!a.canAccessStudent(u, studentID) {
		httpError(w, http.StatusForbidden, "permission denied")
		return
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
	if _, err := tx.Exec(`UPDATE receipts SET status = ?, review_note = ?, reviewed_by = ?, reviewed_at = ? WHERE id = ?`,
		req.Status, req.Note, u.ID, a.nowStr(), id); err != nil {
		serverError(w, err)
		return
	}
	// The fees on this receipt follow its outcome.
	feeStatus, paidAt := map[string]string{"approved": "paid", "rejected": "unpaid", "pending": "pending"}[req.Status], ""
	if feeStatus == "paid" {
		paidAt = a.nowStr()
	}
	if _, err := tx.Exec(`UPDATE student_fees SET status = ?, paid_at = ?
		WHERE id IN (SELECT student_fee_id FROM receipt_fees WHERE receipt_id = ?) AND status != 'waived'`, feeStatus, paidAt, id); err != nil {
		serverError(w, err)
		return
	}
	if err := tx.Commit(); err != nil {
		serverError(w, err)
		return
	}
	if req.Status != "pending" {
		title, body := "缴费已确认 / Payment confirmed", "学校已确认收到："+desc
		if req.Status == "rejected" {
			title, body = "缴费凭证被退回 / Receipt returned", desc+"\n原因 Reason: "+req.Note
		}
		go a.notifier.notifyParents([]int64{studentID}, title, func(string) string { return body }, "/#receipts", fmt.Sprintf("receipt-%d", id))
	}
	okJSON(w)
}
