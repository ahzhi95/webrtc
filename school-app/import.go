package main

import (
	"bytes"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/csv"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/xuri/excelize/v2"
)

// Bulk import of students and their parents from Excel (.xlsx) or CSV.
// Step 1 (?dry_run=1) checks every row and shows what would happen; step 2 imports, all or nothing.
// Parents are matched by phone number, so a family with children in two schools keeps one account.
// New parent accounts get a username (their phone number) and a random first password, returned
// once as an Excel sheet for the school to hand out.

var importHeaders = []string{
	"学号 Student No.*", "学生姓名 Student name*", "校区 Campus", "班级 Class*",
	"出生日期 Date of birth (YYYY-MM-DD)", "性别 Gender (男/女)", "过敏/健康备注 Health notes",
	"紧急联系人 Emergency contact", "紧急联系电话 Emergency phone", "备注 Notes",
	"家长1姓名 Parent 1 name", "家长1电话 Parent 1 phone", "家长1邮箱 Parent 1 email",
	"家长2姓名 Parent 2 name", "家长2电话 Parent 2 phone", "家长2邮箱 Parent 2 email",
}

const maxImportRows = 3000

type importParent struct {
	Name     string `json:"name"`
	Phone    string `json:"phone"`
	Email    string `json:"email"`
	Action   string `json:"action"` // new | existing
	existing int64
}

type importRow struct {
	Row       int            `json:"row"`
	StudentNo string         `json:"student_no"`
	Name      string         `json:"name"`
	Campus    string         `json:"campus"`
	Class     string         `json:"class"`
	Action    string         `json:"action"` // new | update | error
	Errors    []string       `json:"errors"`
	NewClass  bool           `json:"new_class"`
	Parents   []importParent `json:"parents"`
	profile   Profile
	campusID  int64
	studentID int64
}

type newAccount struct {
	Name, Phone, Username, Password string
	Children                        []string
}

// phoneKey normalises a Malaysian phone number for matching: digits only, +60 / 60 → 0.
func phoneKey(p string) string {
	var b strings.Builder
	for _, r := range p {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	d := b.String()
	if strings.HasPrefix(d, "60") && len(d) >= 10 {
		d = "0" + d[2:]
	}
	return d
}

func randomPassword() string {
	const chars = "abcdefghjkmnpqrstuvwxyz23456789" // no look-alikes such as l/1, o/0
	b := make([]byte, 8)
	for i := range b {
		n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(chars))))
		b[i] = chars[n.Int64()]
	}
	return string(b)
}

// parseDate accepts 2021-03-14, 2021/03/14, 14/03/2021 (day first, as written in Malaysia) and Excel serial numbers.
func parseDate(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", true
	}
	for _, f := range []string{"2006-01-02", "2006/01/02", "2/1/2006", "02/01/2006", "2-1-2006", "02-01-2006", "2.1.2006"} {
		if t, err := time.Parse(f, s); err == nil {
			return t.Format("2006-01-02"), true
		}
	}
	if n, err := strconv.ParseFloat(s, 64); err == nil && n > 1000 && n < 80000 {
		t := time.Date(1899, 12, 30, 0, 0, 0, 0, time.UTC).AddDate(0, 0, int(n))
		return t.Format("2006-01-02"), true
	}
	return "", false
}

func readImportFile(name string, data []byte) ([][]string, error) {
	if strings.HasSuffix(strings.ToLower(name), ".csv") {
		data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
		r := csv.NewReader(bytes.NewReader(data))
		r.FieldsPerRecord = -1
		return r.ReadAll()
	}
	f, err := excelize.OpenReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("not an Excel or CSV file")
	}
	defer f.Close()
	return f.GetRows(f.GetSheetName(0))
}

func cell(row []string, i int) string {
	if i < len(row) {
		return strings.TrimSpace(row[i])
	}
	return ""
}

// planImport checks every row and decides what to do, without changing anything.
func (a *App) planImport(u *User, rows [][]string) ([]importRow, map[string]int, error) {
	// Campuses the user may import into.
	campuses := map[string]int64{}
	var only int64
	crows, err := a.db.Query(`SELECT id, name FROM campuses WHERE (? = 0 OR id = ?)`, scope(u), scope(u))
	if err != nil {
		return nil, nil, err
	}
	for crows.Next() {
		var id int64
		var name string
		if crows.Scan(&id, &name) == nil {
			campuses[strings.ToLower(name)] = id
			only = id
		}
	}
	crows.Close()
	if len(campuses) != 1 {
		only = 0
	}
	// Existing classes and parents.
	classes := map[string]int64{} // "campus|name" -> id
	cl, err := a.db.Query(`SELECT id, COALESCE(campus_id,0), name FROM classes`)
	if err != nil {
		return nil, nil, err
	}
	for cl.Next() {
		var id, cp int64
		var name string
		if cl.Scan(&id, &cp, &name) == nil {
			classes[fmt.Sprintf("%d|%s", cp, strings.ToLower(name))] = id
		}
	}
	cl.Close()
	parents := map[string]int64{}
	pr, err := a.db.Query(`SELECT id, phone FROM users WHERE role = 'parent'`)
	if err != nil {
		return nil, nil, err
	}
	for pr.Next() {
		var id int64
		var phone string
		if pr.Scan(&id, &phone) == nil && phoneKey(phone) != "" {
			parents[phoneKey(phone)] = id
		}
	}
	pr.Close()

	plan := []importRow{}
	seenNo := map[string]int{}
	newClasses := map[string]bool{}
	newParents := map[string]bool{}
	sum := map[string]int{}
	for i, r := range rows {
		if i == 0 { // header row
			continue
		}
		empty := true
		for _, c := range r {
			if strings.TrimSpace(c) != "" {
				empty = false
			}
		}
		if empty || strings.HasPrefix(cell(r, 0), "例") || strings.HasPrefix(strings.ToLower(cell(r, 0)), "e.g") {
			continue // blank or the template's example row
		}
		x := importRow{Row: i + 1, StudentNo: cell(r, 0), Name: cell(r, 1), Campus: cell(r, 2), Class: cell(r, 3), Errors: []string{}, Parents: []importParent{}}
		bad := func(msg string) { x.Errors = append(x.Errors, msg) }
		if x.StudentNo == "" {
			bad("缺少学号 missing student no.")
		} else if prev, dup := seenNo[x.StudentNo]; dup {
			bad(fmt.Sprintf("学号和第 %d 行重复 duplicate of row %d", prev, prev))
		} else {
			seenNo[x.StudentNo] = x.Row
		}
		if x.Name == "" {
			bad("缺少学生姓名 missing name")
		}
		// Campus
		switch {
		case scope(u) != 0:
			x.campusID = scope(u)
		case x.Campus != "":
			x.campusID = campuses[strings.ToLower(x.Campus)]
			if x.campusID == 0 {
				bad("找不到校区 unknown campus: " + x.Campus)
			}
		case only != 0:
			x.campusID = only
		default:
			bad("请填写校区 campus is required")
		}
		if x.Class == "" {
			bad("缺少班级 missing class")
		} else if x.campusID != 0 {
			key := fmt.Sprintf("%d|%s", x.campusID, strings.ToLower(x.Class))
			if classes[key] == 0 {
				x.NewClass = true
				if !newClasses[key] {
					newClasses[key] = true
					sum["new_classes"]++
				}
			}
		}
		// Profile
		var ok bool
		if x.profile.BirthDate, ok = parseDate(cell(r, 4)); !ok {
			bad("出生日期格式不对 invalid date: " + cell(r, 4))
		}
		switch g := strings.ToUpper(cell(r, 5)); g {
		case "":
		case "男", "M", "MALE", "BOY", "L", "LELAKI":
			x.profile.Gender = "M"
		case "女", "F", "FEMALE", "GIRL", "P", "PEREMPUAN":
			x.profile.Gender = "F"
		default:
			bad("性别请填 男 或 女 gender must be 男/女")
		}
		x.profile.HealthNotes, x.profile.EmergencyName, x.profile.EmergencyPhone, x.profile.Notes = cell(r, 6), cell(r, 7), cell(r, 8), cell(r, 9)
		// Existing student?
		if x.StudentNo != "" {
			var sid int64
			if a.db.QueryRow(`SELECT id FROM students WHERE student_no = ?`, x.StudentNo).Scan(&sid) == nil {
				if !a.canAccessStudent(u, sid) {
					bad("学号已被其他学校使用 student no. belongs to another school")
				}
				x.studentID = sid
			}
		}
		// Parents
		for p := 0; p < 2; p++ {
			name, phone, email := cell(r, 10+3*p), cell(r, 11+3*p), cell(r, 12+3*p)
			if name == "" && phone == "" && email == "" {
				continue
			}
			ip := importParent{Name: name, Phone: phone, Email: email}
			key := phoneKey(phone)
			switch {
			case len(key) < 9:
				bad(fmt.Sprintf("家长%d 电话不对（用电话号码建立账号）parent %d phone is required", p+1, p+1))
				continue
			case parents[key] != 0:
				ip.Action, ip.existing = "existing", parents[key]
			default:
				if name == "" {
					bad(fmt.Sprintf("缺少家长%d姓名 parent %d name is required", p+1, p+1))
					continue
				}
				ip.Action = "new"
				if !newParents[key] {
					newParents[key] = true
					sum["new_parents"]++
				}
			}
			if ip.Action == "existing" {
				sum["linked_parents"]++
			}
			x.Parents = append(x.Parents, ip)
		}
		switch {
		case len(x.Errors) > 0:
			x.Action = "error"
		case x.studentID != 0:
			x.Action = "update"
		default:
			x.Action = "new"
		}
		sum[x.Action]++
		plan = append(plan, x)
		if len(plan) > maxImportRows {
			return nil, nil, fmt.Errorf("too many rows (max %d)", maxImportRows)
		}
	}
	return plan, sum, nil
}

func (a *App) handleImportStudents(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	r.Body = http.MaxBytesReader(w, r.Body, 6<<20)
	if err := r.ParseMultipartForm(6 << 20); err != nil {
		httpError(w, http.StatusBadRequest, "file too large (max 5 MB)")
		return
	}
	defer r.MultipartForm.RemoveAll()
	file, hdr, err := r.FormFile("file")
	if err != nil {
		httpError(w, http.StatusBadRequest, "please choose a file")
		return
	}
	data, err := io.ReadAll(io.LimitReader(file, 5<<20+1))
	file.Close()
	if err != nil {
		serverError(w, err)
		return
	}
	rows, err := readImportFile(hdr.Filename, data)
	if err != nil {
		httpError(w, http.StatusBadRequest, "not an Excel or CSV file")
		return
	}
	plan, sum, err := a.planImport(u, rows)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	if r.URL.Query().Get("dry_run") == "1" || sum["error"] > 0 || len(plan) == 0 {
		writeJSON(w, map[string]any{"rows": plan, "summary": sum, "imported": false})
		return
	}

	// Import everything in one transaction: either all rows are saved or none.
	tx, err := a.db.Begin()
	if err != nil {
		serverError(w, err)
		return
	}
	defer tx.Rollback()
	classIDs := map[string]int64{}
	parentIDs := map[string]int64{}
	accounts := map[string]*newAccount{}
	var order []string
	classFor := func(campus int64, name string) (int64, error) {
		key := fmt.Sprintf("%d|%s", campus, strings.ToLower(name))
		if id, ok := classIDs[key]; ok {
			return id, nil
		}
		var id int64
		err := tx.QueryRow(`SELECT id FROM classes WHERE campus_id = ? AND lower(name) = lower(?)`, campus, name).Scan(&id)
		if err == sql.ErrNoRows {
			res, err2 := tx.Exec(`INSERT INTO classes (name, campus_id) VALUES (?,?)`, name, campus)
			if err2 != nil {
				return 0, err2
			}
			id, _ = res.LastInsertId()
		} else if err != nil {
			return 0, err
		}
		classIDs[key] = id
		return id, nil
	}
	keep := func(newV, oldV string) string { // blank cells keep what is already saved
		if newV == "" {
			return oldV
		}
		return newV
	}
	for _, x := range plan {
		cid, err := classFor(x.campusID, x.Class)
		if err != nil {
			serverError(w, err)
			return
		}
		sid := x.studentID
		p := x.profile
		if sid != 0 {
			var old Profile
			_ = tx.QueryRow(`SELECT birth_date, gender, health_notes, emergency_name, emergency_phone, notes FROM students WHERE id = ?`, sid).
				Scan(&old.BirthDate, &old.Gender, &old.HealthNotes, &old.EmergencyName, &old.EmergencyPhone, &old.Notes)
			if _, err := tx.Exec(`UPDATE students SET name = ?, class_id = ?, birth_date = ?, gender = ?, health_notes = ?,
				emergency_name = ?, emergency_phone = ?, notes = ? WHERE id = ?`, x.Name, cid,
				keep(p.BirthDate, old.BirthDate), keep(p.Gender, old.Gender), keep(p.HealthNotes, old.HealthNotes),
				keep(p.EmergencyName, old.EmergencyName), keep(p.EmergencyPhone, old.EmergencyPhone), keep(p.Notes, old.Notes), sid); err != nil {
				serverError(w, err)
				return
			}
		} else {
			res, err := tx.Exec(`INSERT INTO students (name, student_no, class_id, qr_token, birth_date, gender, health_notes,
				emergency_name, emergency_phone, notes) VALUES (?,?,?,?,?,?,?,?,?,?)`, x.Name, x.StudentNo, cid, randomToken(12),
				p.BirthDate, p.Gender, p.HealthNotes, p.EmergencyName, p.EmergencyPhone, p.Notes)
			if err != nil {
				serverError(w, err)
				return
			}
			sid, _ = res.LastInsertId()
		}
		for _, ip := range x.Parents {
			key := phoneKey(ip.Phone)
			pid := ip.existing
			if pid == 0 {
				pid = parentIDs[key]
			}
			if pid == 0 { // new parent account: username = phone number
				username := key
				for n := 2; ; n++ {
					var one int
					if tx.QueryRow(`SELECT 1 FROM users WHERE username = ?`, username).Scan(&one) == sql.ErrNoRows {
						break
					}
					username = fmt.Sprintf("%s-%d", key, n)
				}
				pw := randomPassword()
				hash, err := hashPassword(pw)
				if err != nil {
					serverError(w, err)
					return
				}
				res, err := tx.Exec(`INSERT INTO users (username, password_hash, name, phone, email, role, campus_id) VALUES (?,?,?,?,?,'parent',?)`,
					username, hash, ip.Name, ip.Phone, ip.Email, x.campusID)
				if err != nil {
					serverError(w, err)
					return
				}
				pid, _ = res.LastInsertId()
				parentIDs[key] = pid
				accounts[key] = &newAccount{Name: ip.Name, Phone: ip.Phone, Username: username, Password: pw}
				order = append(order, key)
			}
			if acc := accounts[key]; acc != nil {
				acc.Children = append(acc.Children, x.Name)
			}
			if _, err := tx.Exec(`INSERT OR IGNORE INTO parent_students (parent_id, student_id) VALUES (?,?)`, pid, sid); err != nil {
				serverError(w, err)
				return
			}
		}
	}
	if err := tx.Commit(); err != nil {
		serverError(w, err)
		return
	}

	// The new parents' first passwords, shown once, as an Excel sheet to print or send.
	var sheet string
	if len(order) > 0 {
		f := excelize.NewFile()
		sh := "家长账号 Accounts"
		_ = f.SetSheetName("Sheet1", sh)
		_ = f.SetSheetRow(sh, "A1", &[]any{"家长 Parent", "电话 Phone", "用户名 Username", "初始密码 First password", "孩子 Children"})
		for i, k := range order {
			acc := accounts[k]
			_ = f.SetSheetRow(sh, fmt.Sprintf("A%d", i+2), &[]any{acc.Name, acc.Phone, acc.Username, acc.Password, strings.Join(acc.Children, "、")})
		}
		_ = f.SetCellValue(sh, fmt.Sprintf("A%d", len(order)+3), "请家长第一次登录后修改密码。Parents should change the password after first login.")
		_ = f.SetColWidth(sh, "A", "E", 22)
		if bold, err := f.NewStyle(&excelize.Style{Font: &excelize.Font{Bold: true}}); err == nil {
			_ = f.SetRowStyle(sh, 1, 1, bold)
		}
		var buf bytes.Buffer
		if err := f.Write(&buf); err == nil {
			sheet = base64.StdEncoding.EncodeToString(buf.Bytes())
		}
		f.Close()
	}
	writeJSON(w, map[string]any{"rows": plan, "summary": sum, "imported": true, "new_accounts": len(order), "accounts_xlsx": sheet})
}

// handleImportTemplate: an Excel file with the right columns, an example row and instructions.
func (a *App) handleImportTemplate(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	f := excelize.NewFile()
	defer f.Close()
	sh := "学生 Students"
	_ = f.SetSheetName("Sheet1", sh)
	header := make([]any, len(importHeaders))
	for i, h := range importHeaders {
		header[i] = h
	}
	_ = f.SetSheetRow(sh, "A1", &header)
	campus := ""
	if u.CampusName != "" {
		campus = u.CampusName
	}
	_ = f.SetSheetRow(sh, "A2", &[]any{"例 e.g. S1001", "林小明 Lim Xiao Ming", campus, "5岁 Melati", "2021-03-14", "男", "对花生过敏",
		"林太太", "013-9876543", "", "林先生 Mr. Lim", "012-3456789", "lim@example.com", "", "", ""})
	text, _ := f.NewStyle(&excelize.Style{NumFmt: 49}) // keep student no., dates and phones as typed
	_ = f.SetColStyle(sh, "A:P", text)
	bold, _ := f.NewStyle(&excelize.Style{Font: &excelize.Font{Bold: true}, Fill: excelize.Fill{Type: "pattern", Pattern: 1, Color: []string{"DCEFE8"}}, NumFmt: 49})
	_ = f.SetRowStyle(sh, 1, 1, bold)
	grey, _ := f.NewStyle(&excelize.Style{Font: &excelize.Font{Color: "888888", Italic: true}, NumFmt: 49})
	_ = f.SetRowStyle(sh, 2, 2, grey)
	_ = f.SetColWidth(sh, "A", "P", 18)
	_ = f.SetPanes(sh, &excelize.Panes{Freeze: true, YSplit: 1, TopLeftCell: "A2", ActivePane: "bottomLeft"})
	dv := excelize.NewDataValidation(true)
	dv.Sqref = "F2:F3001"
	_ = dv.SetDropList([]string{"男", "女"})
	_ = f.AddDataValidation(sh, dv)

	help := "说明 Instructions"
	_, _ = f.NewSheet(help)
	lines := []string{
		"1. 从第 3 行开始填写，每个学生一行。第 2 行是示范，可以删除或保留（会被忽略）。",
		"2. 带 * 的栏位必须填写：学号、学生姓名、班级。",
		"3. 校区：总管理员必须填写（要和系统里的校区名称一样）；学校管理员可以留空。",
		"4. 班级不存在时会自动建立。",
		"5. 学号已存在时会更新这个学生的资料；空白的栏位不会覆盖原有资料。",
		"6. 家长用电话号码配对：电话已有账号就直接绑定（例如兄弟姐妹在别的学校），没有就建立新账号。",
		"7. 新家长账号的用户名是电话号码，初始密码会在导入后给你一份 Excel，请发给家长并提醒他们修改密码。",
		"8. 出生日期格式：2021-03-14 或 14/03/2021。性别：男 / 女。",
		"9. 上传后会先显示预览，确认没有错误才会导入。",
		"",
		"1. Fill in from row 3, one student per row. Row 2 is an example and is ignored.",
		"2. Columns marked * are required: student no., name, class.",
		"3. Campus: required for the super admin (same name as in the app); school admins may leave it blank.",
		"4. Missing classes are created.",
		"5. An existing student no. updates that student; blank cells keep the saved values.",
		"6. Parents are matched by phone number: an existing account is linked, otherwise a new one is created.",
		"7. New parents log in with their phone number; their first passwords come back as an Excel file after the import.",
		"8. Date of birth: 2021-03-14 or 14/03/2021. Gender: 男 / 女.",
		"9. You will see a preview first; nothing is saved until you confirm.",
	}
	for i, l := range lines {
		_ = f.SetCellValue(help, fmt.Sprintf("A%d", i+1), l)
	}
	_ = f.SetColWidth(help, "A", "A", 110)
	w.Header().Set("Content-Type", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet")
	w.Header().Set("Content-Disposition", `attachment; filename="student-import-template.xlsx"`)
	_ = f.Write(w)
}
