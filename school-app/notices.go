package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Announcements (通告) go to all campuses, one campus, or one class. Admins can attach images and PDFs,
// schedule them, edit, retract, and see who has read them. When a notice is published, every parent
// it is addressed to becomes a recipient and gets a push notification and a message.

const (
	maxNoticeFiles    = 5
	maxNoticeFileSize = 10 << 20
)

type NoticeFile struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
	Size int64  `json:"size"`
}

type Announcement struct {
	ID         int64        `json:"id"`
	Title      string       `json:"title"`
	Body       string       `json:"body"`
	CampusID   int64        `json:"campus_id"`
	CampusName string       `json:"campus_name"`
	ClassID    int64        `json:"class_id"`
	ClassName  string       `json:"class_name"`
	AuthorID   int64        `json:"author_id"`
	AuthorName string       `json:"author_name"`
	CreatedAt  string       `json:"created_at"`
	PublishAt  string       `json:"publish_at"`
	Published  bool         `json:"published"`
	Retracted  string       `json:"retracted_at"`
	EditedAt   string       `json:"edited_at"`
	Files      []NoticeFile `json:"files"`
	Read       bool         `json:"read"`       // parents: have I read it
	Recipients int          `json:"recipients"` // admins: read statistics
	ReadCount  int          `json:"read_count"`
	AskReply   bool         `json:"ask_reply"` // parents answer per child: attending or not
	// Parents: which of my children it is about, and my answers (student id → "yes" / "no").
	Children []int64          `json:"children,omitempty"`
	Replies  map[int64]string `json:"replies,omitempty"`
	// Admins: how many answered yes / no.
	ReplyYes int `json:"reply_yes"`
	ReplyNo  int `json:"reply_no"`
}

// aboutChildren: which of these children a notice for (campus, class) concerns.
func aboutChildren(kids []Student, campus, class int64) []int64 {
	out := []int64{}
	for _, k := range kids {
		if (class != 0 && k.ClassID == class) || (class == 0 && (campus == 0 || k.CampusID == campus)) {
			out = append(out, k.ID)
		}
	}
	return out
}

// noticeTarget returns the audience of a notice.
func (a *App) noticeTarget(id int64) (campus, class int64, askReply bool, err error) {
	err = a.db.QueryRow(`SELECT COALESCE(n.campus_id, c.campus_id, 0), COALESCE(n.class_id,0), n.ask_reply
		FROM announcements n LEFT JOIN classes c ON c.id = n.class_id WHERE n.id = ?`, id).Scan(&campus, &class, &askReply)
	return
}

func (a *App) noticeDir() string { return filepath.Join(a.uploadDir, "notices") }

// noticeCampus: the campus a notice belongs to (0 = all campuses).
func (a *App) noticeCampus(id int64) (int64, bool) {
	var c int64
	err := a.db.QueryRow(`SELECT COALESCE(n.campus_id, c.campus_id, 0) FROM announcements n LEFT JOIN classes c ON c.id = n.class_id WHERE n.id = ?`, id).Scan(&c)
	return c, err == nil
}

func (a *App) canManageNotice(u *User, id int64) bool {
	c, ok := a.noticeCampus(id)
	return ok && u.Role == "admin" && (scope(u) == 0 || c == scope(u))
}

// parentVisible: SQL condition (on announcements n) for notices a parent may read; takes the parent id twice.
const parentVisible = `n.published = 1 AND n.retracted_at = '' AND ((n.campus_id IS NULL AND n.class_id IS NULL)
	OR n.campus_id IN (SELECT c2.campus_id FROM parent_students ps JOIN students s ON s.id = ps.student_id
		JOIN classes c2 ON c2.id = s.class_id WHERE ps.parent_id = ?)
	OR n.class_id IN (SELECT s.class_id FROM parent_students ps JOIN students s ON s.id = ps.student_id WHERE ps.parent_id = ?))`

func (a *App) canReadNotice(u *User, id int64) bool {
	if u.Role == "parent" {
		var one int
		return a.db.QueryRow(`SELECT 1 FROM announcements n WHERE n.id = ? AND `+parentVisible, id, u.ID, u.ID).Scan(&one) == nil
	}
	c, ok := a.noticeCampus(id)
	if !ok {
		return false
	}
	if u.Role == "teacher" {
		var pub int
		var retracted string
		_ = a.db.QueryRow(`SELECT published, retracted_at FROM announcements WHERE id = ?`, id).Scan(&pub, &retracted)
		if pub == 0 || retracted != "" {
			return false
		}
	}
	return scope(u) == 0 || c == 0 || c == scope(u)
}

func (a *App) handleListAnnouncements(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	q := `SELECT n.id, n.title, n.body, COALESCE(n.campus_id, c.campus_id, 0), COALESCE(cp.name,''), COALESCE(n.class_id,0), COALESCE(c.name,''),
		COALESCE(n.author_id,0), COALESCE(au.name,''), n.created_at, n.publish_at, n.published, n.retracted_at, n.edited_at,
		(SELECT COUNT(*) FROM notice_recipients nr WHERE nr.notice_id = n.id),
		(SELECT COUNT(*) FROM notice_recipients nr WHERE nr.notice_id = n.id AND nr.read_at != ''),
		COALESCE((SELECT nr.read_at != '' FROM notice_recipients nr WHERE nr.notice_id = n.id AND nr.user_id = ?), 1),
		n.ask_reply,
		(SELECT COUNT(*) FROM notice_replies x WHERE x.notice_id = n.id AND x.answer = 'yes'),
		(SELECT COUNT(*) FROM notice_replies x WHERE x.notice_id = n.id AND x.answer = 'no')
		FROM announcements n LEFT JOIN classes c ON c.id = n.class_id
		LEFT JOIN campuses cp ON cp.id = COALESCE(n.campus_id, c.campus_id) LEFT JOIN users au ON au.id = n.author_id`
	args := []any{u.ID}
	switch {
	case u.Role == "parent":
		q += ` WHERE ` + parentVisible
		args = append(args, u.ID, u.ID)
	case u.Role == "teacher":
		q += ` WHERE n.published = 1 AND n.retracted_at = '' AND (? = 0 OR (n.campus_id IS NULL AND n.class_id IS NULL) OR n.campus_id = ? OR c.campus_id = ?)`
		args = append(args, scope(u), scope(u), scope(u))
	case scope(u) != 0:
		q += ` WHERE (n.campus_id IS NULL AND n.class_id IS NULL) OR n.campus_id = ? OR c.campus_id = ?`
		args = append(args, scope(u), scope(u))
	}
	// Scheduled notices first (by when they go out), then newest published.
	rows, err := a.db.Query(q+` ORDER BY n.published, CASE WHEN n.published = 0 THEN n.publish_at END, n.created_at DESC LIMIT 200`, args...)
	if err != nil {
		serverError(w, err)
		return
	}
	list := []Announcement{}
	index := map[int64]int{}
	for rows.Next() {
		var n Announcement
		if err := rows.Scan(&n.ID, &n.Title, &n.Body, &n.CampusID, &n.CampusName, &n.ClassID, &n.ClassName, &n.AuthorID, &n.AuthorName,
			&n.CreatedAt, &n.PublishAt, &n.Published, &n.Retracted, &n.EditedAt, &n.Recipients, &n.ReadCount, &n.Read, &n.AskReply, &n.ReplyYes, &n.ReplyNo); err != nil {
			rows.Close()
			serverError(w, err)
			return
		}
		n.Files = []NoticeFile{}
		if u.Role != "admin" {
			n.Recipients, n.ReadCount, n.ReplyYes, n.ReplyNo = 0, 0, 0, 0
		}
		index[n.ID] = len(list)
		list = append(list, n)
	}
	rows.Close()
	if u.Role == "parent" {
		kids, err := a.queryStudents(`WHERE s.id IN (SELECT student_id FROM parent_students WHERE parent_id = ?)`, u.ID)
		if err != nil {
			serverError(w, err)
			return
		}
		for i := range list {
			list[i].Children = aboutChildren(kids, list[i].CampusID, list[i].ClassID)
		}
		rr, err := a.db.Query(`SELECT notice_id, student_id, answer FROM notice_replies
			WHERE student_id IN (SELECT student_id FROM parent_students WHERE parent_id = ?)`, u.ID)
		if err != nil {
			serverError(w, err)
			return
		}
		for rr.Next() {
			var nid, sid int64
			var ans string
			if rr.Scan(&nid, &sid, &ans) == nil {
				if i, ok := index[nid]; ok {
					if list[i].Replies == nil {
						list[i].Replies = map[int64]string{}
					}
					list[i].Replies[sid] = ans
				}
			}
		}
		rr.Close()
	}
	files, err := a.db.Query(`SELECT id, notice_id, orig_name, file_type, size_bytes FROM notice_attachments ORDER BY id`)
	if err != nil {
		serverError(w, err)
		return
	}
	defer files.Close()
	for files.Next() {
		var f NoticeFile
		var nid int64
		if files.Scan(&f.ID, &nid, &f.Name, &f.Type, &f.Size) == nil {
			if i, ok := index[nid]; ok {
				list[i].Files = append(list[i].Files, f)
			}
		}
	}
	writeJSON(w, list)
}

type noticeForm struct {
	Title, Body       string
	CampusID, ClassID int64
	PublishAt         time.Time // zero = now
	Remove            []int64
	AskReply          bool
}

// readNoticeForm parses the multipart form shared by create and edit.
func (a *App) readNoticeForm(w http.ResponseWriter, r *http.Request) (*noticeForm, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxNoticeFiles*maxNoticeFileSize+(1<<20))
	if err := r.ParseMultipartForm(4 << 20); err != nil {
		httpError(w, http.StatusBadRequest, "file too large (max 10 MB)")
		return nil, false
	}
	f := &noticeForm{Title: strings.TrimSpace(r.FormValue("title")), Body: strings.TrimSpace(r.FormValue("body"))}
	f.CampusID, _ = strconv.ParseInt(r.FormValue("campus_id"), 10, 64)
	f.ClassID, _ = strconv.ParseInt(r.FormValue("class_id"), 10, 64)
	f.AskReply = r.FormValue("ask_reply") == "1"
	if f.Title == "" || f.Body == "" {
		httpError(w, http.StatusBadRequest, "title and content are required")
		return nil, false
	}
	if v := r.FormValue("publish_at"); v != "" {
		t, err := time.ParseInLocation("2006-01-02T15:04", v, a.loc)
		if err != nil {
			httpError(w, http.StatusBadRequest, "invalid date")
			return nil, false
		}
		if t.After(a.now()) {
			f.PublishAt = t
		}
	}
	for _, v := range r.MultipartForm.Value["remove_file"] {
		if id, err := strconv.ParseInt(v, 10, 64); err == nil {
			f.Remove = append(f.Remove, id)
		}
	}
	return f, true
}

// checkAudience applies the campus rules: campus admins only reach their own campus.
func (a *App) checkAudience(w http.ResponseWriter, u *User, f *noticeForm) bool {
	switch {
	case f.ClassID != 0:
		if !a.canAccessClass(u, f.ClassID) {
			httpError(w, http.StatusForbidden, "permission denied")
			return false
		}
		f.CampusID = 0 // the class already says which campus
	case scope(u) != 0:
		f.CampusID = scope(u)
	case !a.validCampus(f.CampusID):
		httpError(w, http.StatusBadRequest, "invalid campus")
		return false
	}
	return true
}

type savedFile struct {
	name, ctype, orig string
	size              int64
}

// saveNoticeFiles stores the uploaded attachments. Images are re-encoded (drops EXIF/GPS); PDFs are kept as they are.
func (a *App) saveNoticeFiles(r *http.Request, already int) ([]savedFile, string) {
	files := r.MultipartForm.File["file"]
	if already+len(files) > maxNoticeFiles {
		return nil, fmt.Sprintf("at most %d attachments", maxNoticeFiles)
	}
	if err := os.MkdirAll(a.noticeDir(), 0o750); err != nil {
		return nil, "server error"
	}
	var out []savedFile
	cleanup := func() {
		for _, f := range out {
			os.Remove(filepath.Join(a.noticeDir(), f.name))
		}
	}
	for _, fh := range files {
		if fh.Size > maxNoticeFileSize {
			cleanup()
			return nil, "file too large (max 10 MB)"
		}
		src, err := fh.Open()
		if err != nil {
			cleanup()
			return nil, "server error"
		}
		data, err := io.ReadAll(io.LimitReader(src, maxNoticeFileSize+1))
		src.Close()
		if err != nil {
			cleanup()
			return nil, "server error"
		}
		ctype := http.DetectContentType(data)
		if i := strings.IndexByte(ctype, ';'); i >= 0 {
			ctype = ctype[:i]
		}
		var ext string
		switch {
		case ctype == "application/pdf":
			ext = ".pdf"
		case strings.HasPrefix(ctype, "image/"):
			full, _, err := processPhoto(bytes.NewReader(data))
			if err != nil {
				cleanup()
				return nil, "only images or PDF files"
			}
			data, ctype, ext = full, "image/jpeg", ".jpg"
		default:
			cleanup()
			return nil, "only images or PDF files"
		}
		name := randomToken(16) + ext
		if err := os.WriteFile(filepath.Join(a.noticeDir(), name), data, 0o640); err != nil {
			cleanup()
			return nil, "server error"
		}
		orig := filepath.Base(fh.Filename)
		if ext == ".jpg" && !strings.HasSuffix(strings.ToLower(orig), ".jpg") && !strings.HasSuffix(strings.ToLower(orig), ".jpeg") {
			orig = strings.TrimSuffix(orig, filepath.Ext(orig)) + ".jpg"
		}
		if utf8.RuneCountInString(orig) > 100 {
			orig = string([]rune(orig)[:100])
		}
		out = append(out, savedFile{name, ctype, orig, int64(len(data))})
	}
	return out, ""
}

func (a *App) handleCreateAnnouncement(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	f, ok := a.readNoticeForm(w, r)
	if !ok {
		return
	}
	defer r.MultipartForm.RemoveAll()
	if !a.checkAudience(w, u, f) {
		return
	}
	saved, msg := a.saveNoticeFiles(r, 0)
	if msg != "" {
		httpError(w, http.StatusBadRequest, msg)
		return
	}
	publishAt, published := a.nowStr(), true
	if !f.PublishAt.IsZero() {
		publishAt, published = f.PublishAt.Format(time.RFC3339), false
	}
	res, err := a.db.Exec(`INSERT INTO announcements (title, body, campus_id, class_id, author_id, created_at, publish_at, published, ask_reply)
		VALUES (?,?,?,?,?,?,?,0,?)`, f.Title, f.Body, nullInt(f.CampusID), nullInt(f.ClassID), u.ID, publishAt, publishAt, f.AskReply)
	if err != nil {
		serverError(w, err)
		return
	}
	id, _ := res.LastInsertId()
	for _, s := range saved {
		if _, err := a.db.Exec(`INSERT INTO notice_attachments (notice_id, file_name, file_type, orig_name, size_bytes) VALUES (?,?,?,?,?)`,
			id, s.name, s.ctype, s.orig, s.size); err != nil {
			serverError(w, err)
			return
		}
	}
	if published {
		a.publishNotice(id)
	}
	writeJSON(w, map[string]any{"id": id, "scheduled": !published})
}

// publishNotice makes a notice visible, records who it went to, and notifies them.
func (a *App) publishNotice(id int64) {
	res, err := a.db.Exec(`UPDATE announcements SET published = 1, created_at = ? WHERE id = ? AND published = 0 AND retracted_at = ''`, a.nowStr(), id)
	if err != nil {
		logError(err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return
	}
	var title, body string
	var campus, class int64
	_ = a.db.QueryRow(`SELECT title, body, COALESCE(campus_id,0), COALESCE(class_id,0) FROM announcements WHERE id = ?`, id).Scan(&title, &body, &campus, &class)
	q := `SELECT DISTINCT ps.parent_id FROM parent_students ps JOIN students s ON s.id = ps.student_id
		JOIN classes c ON c.id = s.class_id JOIN users u ON u.id = ps.parent_id WHERE u.disabled = 0`
	var args []any
	switch {
	case class != 0:
		q += ` AND s.class_id = ?`
		args = append(args, class)
	case campus != 0:
		q += ` AND c.campus_id = ?`
		args = append(args, campus)
	}
	rows, err := a.db.Query(q, args...)
	if err != nil {
		logError(err)
		return
	}
	var parents []int64
	for rows.Next() {
		var pid int64
		if rows.Scan(&pid) == nil {
			parents = append(parents, pid)
		}
	}
	rows.Close()
	preview := body
	if utf8.RuneCountInString(preview) > 120 {
		preview = string([]rune(preview)[:120]) + "…"
	}
	ref := fmt.Sprintf("notice:%d", id)
	for _, pid := range parents {
		_, _ = a.db.Exec(`INSERT OR IGNORE INTO notice_recipients (notice_id, user_id) VALUES (?,?)`, id, pid)
	}
	_, _, askReply, _ := a.noticeTarget(id)
	go func() {
		for _, pid := range parents {
			body := preview
			// For a family with several children, say which of them it is about.
			if kids, err := a.queryStudents(`WHERE s.id IN (SELECT student_id FROM parent_students WHERE parent_id = ?)`, pid); err == nil && len(kids) > 1 {
				about := aboutChildren(kids, campus, class)
				var names []string
				for _, k := range kids {
					for _, id := range about {
						if k.ID == id {
							names = append(names, k.Name)
						}
					}
				}
				if len(names) > 0 {
					body = "关于 About: " + strings.Join(names, "、") + "\n" + body
				}
			}
			if askReply {
				body += "\n请在 App 回复 · Please reply in the app"
			}
			a.notifier.deliver(pid, 0, "📢 "+title, body, "/#notices", ref, ref)
		}
	}()
}

// publishDue runs in the background and sends scheduled notices when their time comes.
func (a *App) publishDue() {
	for {
		rows, err := a.db.Query(`SELECT id FROM announcements WHERE published = 0 AND retracted_at = '' AND publish_at <= ?`, a.nowStr())
		if err == nil {
			var ids []int64
			for rows.Next() {
				var id int64
				if rows.Scan(&id) == nil {
					ids = append(ids, id)
				}
			}
			rows.Close()
			for _, id := range ids {
				a.publishNotice(id)
			}
		}
		time.Sleep(30 * time.Second)
	}
}

func (a *App) handleUpdateAnnouncement(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	id, ok := pathID(r, "id")
	if !ok || !a.canManageNotice(u, id) {
		httpError(w, http.StatusForbidden, "permission denied")
		return
	}
	f, ok := a.readNoticeForm(w, r)
	if !ok {
		return
	}
	defer r.MultipartForm.RemoveAll()
	var published bool
	var campus, class int64
	_ = a.db.QueryRow(`SELECT published, COALESCE(campus_id,0), COALESCE(class_id,0) FROM announcements WHERE id = ?`, id).Scan(&published, &campus, &class)
	// Removing attachments first, so the new ones fit within the limit.
	for _, fid := range f.Remove {
		var name string
		if a.db.QueryRow(`SELECT file_name FROM notice_attachments WHERE id = ? AND notice_id = ?`, fid, id).Scan(&name) == nil {
			_, _ = a.db.Exec(`DELETE FROM notice_attachments WHERE id = ?`, fid)
			os.Remove(filepath.Join(a.noticeDir(), filepath.Base(name)))
		}
	}
	var have int
	_ = a.db.QueryRow(`SELECT COUNT(*) FROM notice_attachments WHERE notice_id = ?`, id).Scan(&have)
	saved, msg := a.saveNoticeFiles(r, have)
	if msg != "" {
		httpError(w, http.StatusBadRequest, msg)
		return
	}
	for _, s := range saved {
		if _, err := a.db.Exec(`INSERT INTO notice_attachments (notice_id, file_name, file_type, orig_name, size_bytes) VALUES (?,?,?,?,?)`,
			id, s.name, s.ctype, s.orig, s.size); err != nil {
			serverError(w, err)
			return
		}
	}
	if published {
		// Already sent: the text and files can be corrected, the audience cannot change.
		_, err := a.db.Exec(`UPDATE announcements SET title = ?, body = ?, edited_at = ?, ask_reply = ? WHERE id = ?`, f.Title, f.Body, a.nowStr(), f.AskReply, id)
		if err != nil {
			serverError(w, err)
			return
		}
		okJSON(w)
		return
	}
	if !a.checkAudience(w, u, f) {
		return
	}
	publishAt := a.nowStr()
	if !f.PublishAt.IsZero() {
		publishAt = f.PublishAt.Format(time.RFC3339)
	}
	if _, err := a.db.Exec(`UPDATE announcements SET title = ?, body = ?, campus_id = ?, class_id = ?, publish_at = ?, created_at = ?, ask_reply = ? WHERE id = ?`,
		f.Title, f.Body, nullInt(f.CampusID), nullInt(f.ClassID), publishAt, publishAt, f.AskReply, id); err != nil {
		serverError(w, err)
		return
	}
	if f.PublishAt.IsZero() {
		a.publishNotice(id)
	}
	okJSON(w)
}

// handleRetractAnnouncement hides a notice from parents and takes it out of their message lists.
func (a *App) handleRetractAnnouncement(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	id, ok := pathID(r, "id")
	if !ok || !a.canManageNotice(u, id) {
		httpError(w, http.StatusForbidden, "permission denied")
		return
	}
	if _, err := a.db.Exec(`UPDATE announcements SET retracted_at = ? WHERE id = ? AND retracted_at = ''`, a.nowStr(), id); err != nil {
		serverError(w, err)
		return
	}
	_, _ = a.db.Exec(`DELETE FROM messages WHERE ref = ?`, fmt.Sprintf("notice:%d", id))
	okJSON(w)
}

func (a *App) handleDeleteAnnouncement(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	id, ok := pathID(r, "id")
	if !ok || !a.canManageNotice(u, id) {
		httpError(w, http.StatusForbidden, "permission denied")
		return
	}
	rows, err := a.db.Query(`SELECT file_name FROM notice_attachments WHERE notice_id = ?`, id)
	if err == nil {
		for rows.Next() {
			var name string
			if rows.Scan(&name) == nil {
				os.Remove(filepath.Join(a.noticeDir(), filepath.Base(name)))
			}
		}
		rows.Close()
	}
	if _, err := a.db.Exec(`DELETE FROM announcements WHERE id = ?`, id); err != nil {
		serverError(w, err)
		return
	}
	_, _ = a.db.Exec(`DELETE FROM messages WHERE ref = ?`, fmt.Sprintf("notice:%d", id))
	okJSON(w)
}

// handleMarkNoticesRead: the parent has seen these notices (called when the notices page is shown).
func (a *App) handleMarkNoticesRead(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	var req struct {
		IDs []int64 `json:"ids"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	now := a.nowStr()
	for _, id := range req.IDs {
		if _, err := a.db.Exec(`UPDATE notice_recipients SET read_at = ? WHERE notice_id = ? AND user_id = ? AND read_at = ''`, now, id, u.ID); err != nil {
			serverError(w, err)
			return
		}
		_, _ = a.db.Exec(`UPDATE messages SET read_at = ? WHERE user_id = ? AND ref = ? AND read_at = ''`, now, u.ID, fmt.Sprintf("notice:%d", id))
	}
	writeJSON(w, map[string]int{"unread": a.unreadMessages(u.ID)})
}

// handleNoticeReads: who the notice went to and who has read it (阅读统计).
func (a *App) handleNoticeReads(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	id, ok := pathID(r, "id")
	if !ok || !a.canManageNotice(u, id) {
		httpError(w, http.StatusForbidden, "permission denied")
		return
	}
	rows, err := a.db.Query(`SELECT us.name, us.phone, nr.read_at,
		COALESCE((SELECT group_concat(s.name, '、') FROM parent_students ps JOIN students s ON s.id = ps.student_id WHERE ps.parent_id = us.id), '')
		FROM notice_recipients nr JOIN users us ON us.id = nr.user_id WHERE nr.notice_id = ?
		ORDER BY nr.read_at != '', us.name`, id)
	if err != nil {
		serverError(w, err)
		return
	}
	defer rows.Close()
	type reader struct {
		Name     string `json:"name"`
		Phone    string `json:"phone"`
		ReadAt   string `json:"read_at"`
		Children string `json:"children"`
	}
	list := []reader{}
	for rows.Next() {
		var x reader
		if err := rows.Scan(&x.Name, &x.Phone, &x.ReadAt, &x.Children); err != nil {
			serverError(w, err)
			return
		}
		list = append(list, x)
	}
	writeJSON(w, list)
}

func (a *App) handleNoticeFile(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	id, ok1 := pathID(r, "id")
	fid, ok2 := pathID(r, "fid")
	if !ok1 || !ok2 || !a.canReadNotice(u, id) {
		httpError(w, http.StatusForbidden, "permission denied")
		return
	}
	var name, ctype, orig string
	if err := a.db.QueryRow(`SELECT file_name, file_type, orig_name FROM notice_attachments WHERE id = ? AND notice_id = ?`, fid, id).
		Scan(&name, &ctype, &orig); err != nil {
		serverError(w, err)
		return
	}
	f, err := os.Open(filepath.Join(a.noticeDir(), filepath.Base(name)))
	if err != nil {
		serverError(w, err)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Content-Disposition", fmt.Sprintf(`inline; filename="attachment%s"; filename*=UTF-8''%s`, filepath.Ext(name), urlEscape(orig)))
	w.Header().Set("Cache-Control", "private, no-cache")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; img-src 'self'; style-src 'unsafe-inline'")
	fi, _ := f.Stat()
	http.ServeContent(w, r, "", fi.ModTime(), f)
}

// urlEscape for the filename* header parameter (RFC 5987).
func urlEscape(s string) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || strings.IndexByte("-._~", c) >= 0 {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// handleNoticeReply: a parent answers for one child (attending or not). The answer can be changed.
func (a *App) handleNoticeReply(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	id, ok := pathID(r, "id")
	if !ok || !a.canReadNotice(u, id) {
		httpError(w, http.StatusForbidden, "permission denied")
		return
	}
	var req struct {
		StudentID int64  `json:"student_id"`
		Answer    string `json:"answer"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if req.Answer != "yes" && req.Answer != "no" {
		httpError(w, http.StatusBadRequest, "invalid request")
		return
	}
	campus, class, askReply, err := a.noticeTarget(id)
	if err != nil || !askReply || !a.parentOwns(u.ID, req.StudentID) {
		httpError(w, http.StatusForbidden, "permission denied")
		return
	}
	kids, err := a.queryStudents(`WHERE s.id = ?`, req.StudentID)
	if err != nil || len(kids) == 0 || len(aboutChildren(kids, campus, class)) == 0 {
		httpError(w, http.StatusForbidden, "permission denied")
		return
	}
	if _, err := a.db.Exec(`INSERT INTO notice_replies (notice_id, student_id, parent_id, answer, replied_at) VALUES (?,?,?,?,?)
		ON CONFLICT(notice_id, student_id) DO UPDATE SET answer = excluded.answer, parent_id = excluded.parent_id, replied_at = excluded.replied_at`,
		id, req.StudentID, u.ID, req.Answer, a.nowStr()); err != nil {
		serverError(w, err)
		return
	}
	okJSON(w)
}

// handleNoticeReplies: every child the notice is about, with the answer (or none yet).
func (a *App) handleNoticeReplies(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	id, ok := pathID(r, "id")
	if !ok || !a.canManageNotice(u, id) {
		httpError(w, http.StatusForbidden, "permission denied")
		return
	}
	campus, class, _, err := a.noticeTarget(id)
	if err != nil {
		serverError(w, err)
		return
	}
	where, args := "WHERE 1=1", []any{}
	switch {
	case class != 0:
		where, args = "WHERE s.class_id = ?", []any{class}
	case campus != 0:
		where, args = "WHERE c.campus_id = ?", []any{campus}
	}
	kids, err := a.queryStudents(where, args...)
	if err != nil {
		serverError(w, err)
		return
	}
	type reply struct {
		StudentID int64  `json:"student_id"`
		Name      string `json:"name"`
		ClassName string `json:"class_name"`
		Answer    string `json:"answer"`
		By        string `json:"by"`
		At        string `json:"at"`
	}
	answers := map[int64]reply{}
	rows, err := a.db.Query(`SELECT x.student_id, x.answer, COALESCE(p.name,''), x.replied_at FROM notice_replies x
		LEFT JOIN users p ON p.id = x.parent_id WHERE x.notice_id = ?`, id)
	if err != nil {
		serverError(w, err)
		return
	}
	for rows.Next() {
		var x reply
		if rows.Scan(&x.StudentID, &x.Answer, &x.By, &x.At) == nil {
			answers[x.StudentID] = x
		}
	}
	rows.Close()
	list := []reply{}
	for _, k := range kids {
		x := answers[k.ID]
		x.StudentID, x.Name, x.ClassName = k.ID, k.Name, k.ClassName
		list = append(list, x)
	}
	writeJSON(w, list)
}
