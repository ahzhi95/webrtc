package main

import (
	"net/http"
	"strconv"
)

// Message center (消息): the list of notifications each user received, with unread counts.

type Message struct {
	ID        int64  `json:"id"`
	Title     string `json:"title"`
	Body      string `json:"body"`
	URL       string `json:"url"`
	CreatedAt string `json:"created_at"`
	Read      bool   `json:"read"`
}

func (a *App) unreadMessages(userID int64) int {
	var n int
	_ = a.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE user_id = ? AND read_at = ''`, userID).Scan(&n)
	return n
}

func (a *App) handleListMessages(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	before, err := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
	if err != nil || before <= 0 {
		before = 1 << 62
	}
	rows, err := a.db.Query(`SELECT id, title, body, url, created_at, read_at != '' FROM messages
		WHERE user_id = ? AND id < ? ORDER BY id DESC LIMIT 50`, u.ID, before)
	if err != nil {
		serverError(w, err)
		return
	}
	defer rows.Close()
	list := []Message{}
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.ID, &m.Title, &m.Body, &m.URL, &m.CreatedAt, &m.Read); err != nil {
			serverError(w, err)
			return
		}
		list = append(list, m)
	}
	writeJSON(w, map[string]any{"messages": list, "unread": a.unreadMessages(u.ID)})
}

func (a *App) handleUnreadCount(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]int{"unread": a.unreadMessages(currentUser(r).ID)})
}

// handleReadMessages marks one message (id) or all of them as read.
func (a *App) handleReadMessages(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	var req struct {
		ID  int64 `json:"id"`
		All bool  `json:"all"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	now := a.nowStr()
	var err error
	if req.All {
		_, err = a.db.Exec(`UPDATE messages SET read_at = ? WHERE user_id = ? AND read_at = ''`, now, u.ID)
	} else {
		_, err = a.db.Exec(`UPDATE messages SET read_at = ? WHERE user_id = ? AND id = ? AND read_at = ''`, now, u.ID, req.ID)
	}
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, map[string]int{"unread": a.unreadMessages(u.ID)})
}
