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
)

// Suggestions and feedback (建议反馈). Parents and teachers send a message with a type, text, optional
// photos and contact details to one school; that school's admins reply and mark it in progress / resolved,
// and the sender gets the reply in their messages.

var feedbackCategories = map[string]bool{"suggestion": true, "problem": true, "complaint": true, "other": true}
var feedbackStatuses = map[string]bool{"new": true, "in_progress": true, "resolved": true}

const maxFeedbackImages = 3

type Feedback struct {
	ID         int64   `json:"id"`
	UserName   string  `json:"user_name"`
	UserRole   string  `json:"user_role"`
	UserPhone  string  `json:"user_phone"`
	Children   string  `json:"children"`
	CampusID   int64   `json:"campus_id"`
	CampusName string  `json:"campus_name"`
	Category   string  `json:"category"`
	Body       string  `json:"body"`
	Contact    string  `json:"contact"`
	Status     string  `json:"status"`
	Reply      string  `json:"reply"`
	HandledBy  string  `json:"handled_by"`
	HandledAt  string  `json:"handled_at"`
	CreatedAt  string  `json:"created_at"`
	Images     []int64 `json:"images"`
}

func (a *App) feedbackDir() string { return filepath.Join(a.uploadDir, "feedback") }

// userCampuses: the schools a user belongs to (a parent's children's schools, or a teacher's school).
func (a *App) userCampuses(u *User) []int64 {
	if u.Role != "parent" {
		if u.CampusID != 0 {
			return []int64{u.CampusID}
		}
		return nil
	}
	rows, err := a.db.Query(`SELECT DISTINCT c.campus_id FROM parent_students ps JOIN students s ON s.id = ps.student_id
		JOIN classes c ON c.id = s.class_id WHERE ps.parent_id = ? AND c.campus_id IS NOT NULL`, u.ID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			out = append(out, id)
		}
	}
	return out
}

func (a *App) handleCreateFeedback(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	r.Body = http.MaxBytesReader(w, r.Body, maxFeedbackImages*maxPhotoBytes+(1<<20))
	if err := r.ParseMultipartForm(4 << 20); err != nil {
		httpError(w, http.StatusBadRequest, "upload too large")
		return
	}
	defer r.MultipartForm.RemoveAll()
	category := r.FormValue("category")
	body := strings.TrimSpace(r.FormValue("body"))
	contact := strings.TrimSpace(r.FormValue("contact"))
	if !feedbackCategories[category] {
		httpError(w, http.StatusBadRequest, "invalid category")
		return
	}
	if body == "" || len(body) > 5000 || len(contact) > 200 {
		httpError(w, http.StatusBadRequest, "please describe the problem")
		return
	}
	// Which school it goes to: one the sender belongs to.
	mine := a.userCampuses(u)
	campus, _ := strconv.ParseInt(r.FormValue("campus_id"), 10, 64)
	ok := false
	for _, c := range mine {
		if c == campus {
			ok = true
		}
	}
	if !ok {
		if len(mine) == 0 {
			httpError(w, http.StatusBadRequest, "no school linked to this account")
			return
		}
		campus = mine[0]
	}
	files := r.MultipartForm.File["image"]
	if len(files) > maxFeedbackImages {
		httpError(w, http.StatusBadRequest, fmt.Sprintf("at most %d photos", maxFeedbackImages))
		return
	}
	var images [][]byte
	for _, fh := range files {
		if fh.Size > maxPhotoBytes {
			httpError(w, http.StatusBadRequest, "photo too large (max 15 MB)")
			return
		}
		f, err := fh.Open()
		if err != nil {
			serverError(w, err)
			return
		}
		data, _ := io.ReadAll(io.LimitReader(f, maxPhotoBytes))
		f.Close()
		full, _, err := processPhoto(bytes.NewReader(data))
		if err != nil {
			httpError(w, http.StatusBadRequest, "only JPG, PNG or WEBP photos")
			return
		}
		images = append(images, full)
	}
	res, err := a.db.Exec(`INSERT INTO feedback (user_id, campus_id, category, body, contact, status, created_at) VALUES (?,?,?,?,?,'new',?)`,
		u.ID, campus, category, body, contact, a.nowStr())
	if err != nil {
		serverError(w, err)
		return
	}
	id, _ := res.LastInsertId()
	if len(images) > 0 {
		if err := os.MkdirAll(a.feedbackDir(), 0o750); err != nil {
			serverError(w, err)
			return
		}
	}
	for _, img := range images {
		name := randomToken(16) + ".jpg"
		if err := os.WriteFile(filepath.Join(a.feedbackDir(), name), img, 0o640); err != nil {
			serverError(w, err)
			return
		}
		if _, err := a.db.Exec(`INSERT INTO feedback_images (feedback_id, file_name) VALUES (?,?)`, id, name); err != nil {
			serverError(w, err)
			return
		}
	}
	go a.notifyAdmins(campus, "新的反馈 / New feedback", u.Name+"："+truncate(body, 80), "/#manage")
	writeJSON(w, map[string]int64{"id": id})
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// notifyAdmins messages the admins of a campus and the top-level admins.
func (a *App) notifyAdmins(campus int64, title, body, url string) {
	rows, err := a.db.Query(`SELECT id FROM users WHERE role = 'admin' AND disabled = 0 AND (campus_id IS NULL OR campus_id = ?)`, campus)
	if err != nil {
		logError(err)
		return
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, id := range ids {
		a.notifier.deliver(id, 0, title, body, url, "feedback", "")
	}
}

func (a *App) handleListFeedback(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	q := `SELECT f.id, us.name, us.role, us.phone,
		COALESCE((SELECT group_concat(s.name, '、') FROM parent_students ps JOIN students s ON s.id = ps.student_id
			JOIN classes c ON c.id = s.class_id WHERE ps.parent_id = us.id AND c.campus_id = f.campus_id), ''),
		f.campus_id, COALESCE(cp.name,''), f.category, f.body, f.contact, f.status, f.reply, COALESCE(hb.name,''), f.handled_at, f.created_at
		FROM feedback f JOIN users us ON us.id = f.user_id LEFT JOIN campuses cp ON cp.id = f.campus_id LEFT JOIN users hb ON hb.id = f.handled_by`
	var args []any
	if u.Role == "admin" {
		q += ` WHERE (? = 0 OR f.campus_id = ?)`
		args = append(args, scope(u), scope(u))
		if st := r.URL.Query().Get("status"); feedbackStatuses[st] {
			q += ` AND f.status = ?`
			args = append(args, st)
		}
	} else {
		q += ` WHERE f.user_id = ?`
		args = append(args, u.ID)
	}
	rows, err := a.db.Query(q+` ORDER BY f.status = 'resolved', f.id DESC LIMIT 300`, args...)
	if err != nil {
		serverError(w, err)
		return
	}
	list := []Feedback{}
	index := map[int64]int{}
	for rows.Next() {
		var x Feedback
		if err := rows.Scan(&x.ID, &x.UserName, &x.UserRole, &x.UserPhone, &x.Children, &x.CampusID, &x.CampusName, &x.Category, &x.Body,
			&x.Contact, &x.Status, &x.Reply, &x.HandledBy, &x.HandledAt, &x.CreatedAt); err != nil {
			rows.Close()
			serverError(w, err)
			return
		}
		x.Images = []int64{}
		index[x.ID] = len(list)
		list = append(list, x)
	}
	rows.Close()
	imgs, err := a.db.Query(`SELECT id, feedback_id FROM feedback_images ORDER BY id`)
	if err == nil {
		for imgs.Next() {
			var id, fid int64
			if imgs.Scan(&id, &fid) == nil {
				if i, ok := index[fid]; ok {
					list[i].Images = append(list[i].Images, id)
				}
			}
		}
		imgs.Close()
	}
	writeJSON(w, list)
}

func (a *App) canSeeFeedback(u *User, id int64) bool {
	var owner, campus int64
	if a.db.QueryRow(`SELECT user_id, campus_id FROM feedback WHERE id = ?`, id).Scan(&owner, &campus) != nil {
		return false
	}
	if u.Role == "admin" {
		return scope(u) == 0 || scope(u) == campus
	}
	return owner == u.ID
}

func (a *App) handleFeedbackImage(w http.ResponseWriter, r *http.Request) {
	id, ok1 := pathID(r, "id")
	iid, ok2 := pathID(r, "iid")
	if !ok1 || !ok2 || !a.canSeeFeedback(currentUser(r), id) {
		httpError(w, http.StatusForbidden, "permission denied")
		return
	}
	var name string
	if err := a.db.QueryRow(`SELECT file_name FROM feedback_images WHERE id = ? AND feedback_id = ?`, iid, id).Scan(&name); err != nil {
		serverError(w, err)
		return
	}
	f, err := os.Open(filepath.Join(a.feedbackDir(), filepath.Base(name)))
	if err != nil {
		serverError(w, err)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "private, no-cache")
	fi, _ := f.Stat()
	http.ServeContent(w, r, "", fi.ModTime(), f)
}

// handleReplyFeedback: the school answers and/or changes the status; the sender gets a message.
func (a *App) handleReplyFeedback(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	id, ok := pathID(r, "id")
	if !ok || !a.canSeeFeedback(u, id) {
		httpError(w, http.StatusForbidden, "permission denied")
		return
	}
	var req struct {
		Status string `json:"status"`
		Reply  string `json:"reply"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	req.Reply = strings.TrimSpace(req.Reply)
	if !feedbackStatuses[req.Status] || len(req.Reply) > 5000 {
		httpError(w, http.StatusBadRequest, "invalid request")
		return
	}
	var owner int64
	var oldReply string
	_ = a.db.QueryRow(`SELECT user_id, reply FROM feedback WHERE id = ?`, id).Scan(&owner, &oldReply)
	if _, err := a.db.Exec(`UPDATE feedback SET status = ?, reply = ?, handled_by = ?, handled_at = ? WHERE id = ?`,
		req.Status, req.Reply, u.ID, a.nowStr(), id); err != nil {
		serverError(w, err)
		return
	}
	if req.Reply != "" && req.Reply != oldReply {
		status := map[string]string{"new": "", "in_progress": "（处理中 in progress）", "resolved": "（已解决 resolved）"}[req.Status]
		go a.notifier.deliver(owner, 0, "学校回复了你的反馈 / Reply to your feedback"+status, truncate(req.Reply, 120), "/#me", "feedback", "")
	}
	okJSON(w)
}
