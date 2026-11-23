package main

import (
	"net/http"
	"strings"
)

// Announcements go to all campuses, one campus, or one class.

type Announcement struct {
	ID         int64  `json:"id"`
	Title      string `json:"title"`
	Body       string `json:"body"`
	CampusID   int64  `json:"campus_id"`
	CampusName string `json:"campus_name"`
	ClassID    int64  `json:"class_id"`
	ClassName  string `json:"class_name"`
	AuthorID   int64  `json:"author_id"`
	AuthorName string `json:"author_name"`
	CreatedAt  string `json:"created_at"`
}

func (a *App) handleListAnnouncements(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	q := `SELECT n.id, n.title, n.body, COALESCE(n.campus_id, c.campus_id, 0), COALESCE(cp.name,''), COALESCE(n.class_id,0), COALESCE(c.name,''),
		COALESCE(n.author_id,0), COALESCE(u.name,''), n.created_at
		FROM announcements n LEFT JOIN classes c ON c.id = n.class_id
		LEFT JOIN campuses cp ON cp.id = COALESCE(n.campus_id, c.campus_id) LEFT JOIN users u ON u.id = n.author_id`
	var args []any
	switch {
	case u.Role == "parent":
		// Notices for everyone, for a campus one of their children attends, or for a child's class.
		q += ` WHERE (n.campus_id IS NULL AND n.class_id IS NULL)
			OR n.campus_id IN (SELECT c2.campus_id FROM parent_students ps JOIN students s ON s.id = ps.student_id
				JOIN classes c2 ON c2.id = s.class_id WHERE ps.parent_id = ?)
			OR n.class_id IN (SELECT s.class_id FROM parent_students ps JOIN students s ON s.id = ps.student_id WHERE ps.parent_id = ?)`
		args = append(args, u.ID, u.ID)
	case scope(u) != 0:
		q += ` WHERE (n.campus_id IS NULL AND n.class_id IS NULL) OR n.campus_id = ? OR c.campus_id = ?`
		args = append(args, scope(u), scope(u))
	}
	rows, err := a.db.Query(q+` ORDER BY n.created_at DESC LIMIT 200`, args...)
	if err != nil {
		serverError(w, err)
		return
	}
	defer rows.Close()
	list := []Announcement{}
	for rows.Next() {
		var n Announcement
		if err := rows.Scan(&n.ID, &n.Title, &n.Body, &n.CampusID, &n.CampusName, &n.ClassID, &n.ClassName,
			&n.AuthorID, &n.AuthorName, &n.CreatedAt); err != nil {
			serverError(w, err)
			return
		}
		list = append(list, n)
	}
	writeJSON(w, list)
}

func (a *App) handleCreateAnnouncement(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	var req struct {
		Title    string `json:"title"`
		Body     string `json:"body"`
		CampusID int64  `json:"campus_id"`
		ClassID  int64  `json:"class_id"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	req.Title, req.Body = strings.TrimSpace(req.Title), strings.TrimSpace(req.Body)
	if req.Title == "" || req.Body == "" {
		httpError(w, http.StatusBadRequest, "title and content are required")
		return
	}
	if req.ClassID != 0 {
		if !a.canAccessClass(u, req.ClassID) {
			httpError(w, http.StatusForbidden, "permission denied")
			return
		}
		req.CampusID = 0 // the class already says which campus
	} else if scope(u) != 0 {
		req.CampusID = scope(u) // campus staff cannot post to every campus
	} else if !a.validCampus(req.CampusID) {
		httpError(w, http.StatusBadRequest, "invalid campus")
		return
	}
	if _, err := a.db.Exec(`INSERT INTO announcements (title, body, campus_id, class_id, author_id, created_at) VALUES (?,?,?,?,?,?)`,
		req.Title, req.Body, nullInt(req.CampusID), nullInt(req.ClassID), u.ID, a.nowStr()); err != nil {
		serverError(w, err)
		return
	}
	okJSON(w)
}

func (a *App) handleDeleteAnnouncement(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		httpError(w, http.StatusBadRequest, "invalid id")
		return
	}
	u := currentUser(r)
	q, args := `DELETE FROM announcements WHERE id = ?`, []any{id}
	switch {
	case u.Role != "admin": // teachers may only delete their own notices
		q += ` AND author_id = ?`
		args = append(args, u.ID)
	case scope(u) != 0: // campus admins: notices of their campus
		q += ` AND (campus_id = ? OR class_id IN (SELECT id FROM classes WHERE campus_id = ?))`
		args = append(args, scope(u), scope(u))
	}
	res, err := a.db.Exec(q, args...)
	if err != nil {
		serverError(w, err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		httpError(w, http.StatusForbidden, "permission denied")
		return
	}
	okJSON(w)
}
