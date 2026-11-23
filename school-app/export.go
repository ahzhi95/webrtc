package main

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/xuri/excelize/v2"
)

// Export of payment receipts (凭证导出) for accounting: a spreadsheet, or a ZIP with every receipt file
// plus the spreadsheet. Filters: campus, class, upload date range, review status. Every export is logged
// (who, when, what), because the files contain parents' payment details.

type exportRow struct {
	Receipt
	StudentNo string
	FileName  string
}

type exportFilter struct {
	CampusID, ClassID int64
	From, To, Status  string
}

func (a *App) readExportFilter(r *http.Request) (exportFilter, string) {
	q := r.URL.Query()
	var f exportFilter
	fmt.Sscan(q.Get("campus_id"), &f.CampusID)
	fmt.Sscan(q.Get("class_id"), &f.ClassID)
	f.From, f.To, f.Status = q.Get("from"), q.Get("to"), q.Get("status")
	for _, d := range []string{f.From, f.To} {
		if d != "" {
			if _, err := time.Parse("2006-01-02", d); err != nil {
				return f, "invalid date"
			}
		}
	}
	if f.Status != "" && f.Status != "pending" && f.Status != "approved" && f.Status != "rejected" {
		return f, "invalid status"
	}
	return f, ""
}

func (a *App) exportRows(u *User, f exportFilter) ([]exportRow, error) {
	q := `SELECT rc.id, s.name, s.student_no, COALESCE(c.name,''), COALESCE(cp.name,''), COALESCE(p.name,''), COALESCE(p.phone,''),
		rc.description, rc.amount_cents, rc.file_type, rc.file_name, rc.status, rc.review_note, COALESCE(rv.name,''),
		COALESCE(rc.reviewed_at,''), rc.created_at,
		COALESCE((SELECT group_concat(fi.name, '、') FROM receipt_fees rf JOIN student_fees sf ON sf.id = rf.student_fee_id
			JOIN fee_items fi ON fi.id = sf.fee_item_id WHERE rf.receipt_id = rc.id), ''),
		COALESCE((SELECT o.number FROM official_receipts o WHERE o.upload_id = rc.id AND o.voided_at = '' LIMIT 1), '')
		FROM receipts rc JOIN students s ON s.id = rc.student_id LEFT JOIN classes c ON c.id = s.class_id
		LEFT JOIN campuses cp ON cp.id = c.campus_id LEFT JOIN users p ON p.id = rc.parent_id LEFT JOIN users rv ON rv.id = rc.reviewed_by
		WHERE (? = 0 OR c.campus_id = ?)`
	args := []any{scope(u), scope(u)}
	if f.CampusID != 0 {
		q += ` AND c.campus_id = ?`
		args = append(args, f.CampusID)
	}
	if f.ClassID != 0 {
		q += ` AND s.class_id = ?`
		args = append(args, f.ClassID)
	}
	if f.From != "" {
		q += ` AND substr(rc.created_at, 1, 10) >= ?`
		args = append(args, f.From)
	}
	if f.To != "" {
		q += ` AND substr(rc.created_at, 1, 10) <= ?`
		args = append(args, f.To)
	}
	if f.Status != "" {
		q += ` AND rc.status = ?`
		args = append(args, f.Status)
	}
	rows, err := a.db.Query(q+` ORDER BY rc.created_at`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []exportRow
	for rows.Next() {
		var x exportRow
		var fees string
		if err := rows.Scan(&x.ID, &x.StudentName, &x.StudentNo, &x.ClassName, &x.CampusName, &x.ParentName, &x.ParentPhone,
			&x.Description, &x.AmountCents, &x.FileType, &x.FileName, &x.Status, &x.ReviewNote, &x.ReviewedBy, &x.ReviewedAt, &x.CreatedAt,
			&fees, &x.OfficialNo); err != nil {
			return nil, err
		}
		if fees != "" {
			x.Fees = []ReceiptFee{{Name: fees}}
		}
		list = append(list, x)
	}
	return list, rows.Err()
}

var statusLabel = map[string]string{"pending": "待确认 Pending", "approved": "已确认 Approved", "rejected": "已退回 Rejected"}

func (a *App) receiptsWorkbook(list []exportRow, f exportFilter) *excelize.File {
	x := excelize.NewFile()
	sh := "凭证 Receipts"
	_ = x.SetSheetName("Sheet1", sh)
	_ = x.SetSheetRow(sh, "A1", &[]any{"编号 ID", "上传日期 Uploaded", "校区 Campus", "班级 Class", "学生 Student", "学号 No.", "家长 Parent",
		"电话 Phone", "说明 Description", "收费项目 Fees", "金额 Amount (" + a.currency + ")", "状态 Status", "审核人 Reviewed by",
		"审核时间 Reviewed at", "退回原因 Reason", "正式收据 Receipt no."})
	var total, approved float64
	for i, r := range list {
		fees := ""
		if len(r.Fees) > 0 {
			fees = r.Fees[0].Name
		}
		amount := float64(r.AmountCents) / 100
		total += amount
		if r.Status == "approved" {
			approved += amount
		}
		_ = x.SetSheetRow(sh, fmt.Sprintf("A%d", i+2), &[]any{r.ID, r.CreatedAt[:10], r.CampusName, r.ClassName, r.StudentName, r.StudentNo,
			r.ParentName, r.ParentPhone, r.Description, fees, amount, statusLabel[r.Status], r.ReviewedBy, dateTimeStr(r.ReviewedAt),
			r.ReviewNote, r.OfficialNo})
	}
	n := len(list) + 3
	_ = x.SetSheetRow(sh, fmt.Sprintf("J%d", n), &[]any{"合计 Total", total})
	_ = x.SetSheetRow(sh, fmt.Sprintf("J%d", n+1), &[]any{"已确认 Approved", approved})
	filters := fmt.Sprintf("筛选 Filters: %s ~ %s, %s", orDash(f.From), orDash(f.To), orDash(statusLabel[f.Status]))
	_ = x.SetCellValue(sh, fmt.Sprintf("A%d", n+3), filters)
	money, _ := x.NewStyle(&excelize.Style{NumFmt: 4}) // #,##0.00
	_ = x.SetColStyle(sh, "K", money)
	bold, _ := x.NewStyle(&excelize.Style{Font: &excelize.Font{Bold: true}, Fill: excelize.Fill{Type: "pattern", Pattern: 1, Color: []string{"DCEFE8"}}})
	_ = x.SetRowStyle(sh, 1, 1, bold)
	_ = x.SetColWidth(sh, "A", "P", 16)
	_ = x.SetColWidth(sh, "I", "J", 30)
	_ = x.SetPanes(sh, &excelize.Panes{Freeze: true, YSplit: 1, TopLeftCell: "A2", ActivePane: "bottomLeft"})
	_ = x.AutoFilter(sh, fmt.Sprintf("A1:P%d", len(list)+1), nil)
	return x
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func dateTimeStr(rfc string) string {
	if len(rfc) >= 16 {
		return strings.Replace(rfc[:16], "T", " ", 1)
	}
	return rfc
}

// safeName keeps a file name readable but valid on Windows and macOS.
func safeName(s string) string {
	return strings.Map(func(r rune) rune {
		if strings.ContainsRune(`\/:*?"<>|`, r) || r < 32 {
			return '_'
		}
		return r
	}, strings.TrimSpace(s))
}

func (a *App) handleExportReceipts(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	f, msg := a.readExportFilter(r)
	if msg != "" {
		httpError(w, http.StatusBadRequest, msg)
		return
	}
	if f.ClassID != 0 && !a.canAccessClass(u, f.ClassID) {
		httpError(w, http.StatusForbidden, "permission denied")
		return
	}
	list, err := a.exportRows(u, f)
	if err != nil {
		serverError(w, err)
		return
	}
	format := r.URL.Query().Get("format")
	if format != "zip" {
		format = "xlsx"
	}
	filters, _ := json.Marshal(f)
	_, _ = a.db.Exec(`INSERT INTO export_logs (user_id, kind, filters, count, created_at) VALUES (?,?,?,?,?)`,
		u.ID, "receipts-"+format, string(filters), len(list), a.nowStr())

	stamp := a.now().Format("20060102-1504")
	book := a.receiptsWorkbook(list, f)
	defer book.Close()
	w.Header().Set("Cache-Control", "no-store")
	if format == "xlsx" {
		w.Header().Set("Content-Type", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="receipts-%s.xlsx"`, stamp))
		_ = book.Write(w)
		return
	}
	// ZIP: one folder per campus, each file named date_student_amount_status_id.
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="receipts-%s.zip"`, stamp))
	zw := zip.NewWriter(w)
	defer zw.Close()
	if iw, err := zw.Create(fmt.Sprintf("receipts-%s.xlsx", stamp)); err == nil {
		_ = book.Write(iw)
	}
	for _, x := range list {
		src, err := os.Open(filepath.Join(a.uploadDir, filepath.Base(x.FileName)))
		if err != nil {
			logError(fmt.Errorf("export receipt %d: %w", x.ID, err))
			continue
		}
		name := fmt.Sprintf("%s/%s_%s_%s%s_%s_%d%s", safeName(orDash(x.CampusName)), x.CreatedAt[:10], safeName(x.StudentName),
			a.currency, centsStr(x.AmountCents), x.Status, x.ID, filepath.Ext(x.FileName))
		fw, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate, Modified: a.now()})
		if err == nil {
			_, _ = io.Copy(fw, src)
		}
		src.Close()
	}
}

// handleExportLogs: recent exports (who, when, which filters, how many records).
func (a *App) handleExportLogs(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	rows, err := a.db.Query(`SELECT l.id, COALESCE(us.name,''), COALESCE(us.campus_id,0), l.kind, l.filters, l.count, l.created_at
		FROM export_logs l LEFT JOIN users us ON us.id = l.user_id ORDER BY l.id DESC LIMIT 100`)
	if err != nil {
		serverError(w, err)
		return
	}
	defer rows.Close()
	type entry struct {
		ID        int64  `json:"id"`
		User      string `json:"user"`
		Kind      string `json:"kind"`
		Filters   string `json:"filters"`
		Count     int    `json:"count"`
		CreatedAt string `json:"created_at"`
	}
	list := []entry{}
	for rows.Next() {
		var e entry
		var campus int64
		if err := rows.Scan(&e.ID, &e.User, &campus, &e.Kind, &e.Filters, &e.Count, &e.CreatedAt); err != nil {
			serverError(w, err)
			return
		}
		if scope(u) != 0 && campus != scope(u) { // campus admins see their own school's exports
			continue
		}
		list = append(list, e)
	}
	writeJSON(w, list)
}
