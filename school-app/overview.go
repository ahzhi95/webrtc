package main

import (
	"net/http"
	"strconv"
)

// The admin's at-a-glance page (学校概况): today's arrivals per class and the state of fees,
// for each campus the admin manages.

type ClassToday struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Teacher  string `json:"teacher"`
	Students int    `json:"students"`
	Present  int    `json:"present"`
	Late     int    `json:"late"`
	Left     int    `json:"left"`
}

type CampusOverview struct {
	ID                  int64        `json:"id"`
	Name                string       `json:"name"`
	Students            int          `json:"students"`
	Present             int          `json:"present"`
	Late                int          `json:"late"`
	Left                int          `json:"left"`
	Classes             []ClassToday `json:"classes"`
	PendingReceipts     int          `json:"pending_receipts"`
	UnpaidCount         int          `json:"unpaid_count"`
	UnpaidCents         int64        `json:"unpaid_cents"`
	OverdueCount        int          `json:"overdue_count"`
	OverdueCents        int64        `json:"overdue_cents"`
	CollectedMonthCents int64        `json:"collected_month_cents"`
}

func (a *App) handleOverview(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	today := a.now().Format("2006-01-02")
	month := today[:7]
	only := scope(u)
	if only == 0 {
		only, _ = strconv.ParseInt(r.URL.Query().Get("campus_id"), 10, 64)
	}
	rows, err := a.db.Query(`SELECT id, name FROM campuses WHERE (? = 0 OR id = ?) ORDER BY name`, only, only)
	if err != nil {
		serverError(w, err)
		return
	}
	list := []CampusOverview{}
	for rows.Next() {
		var c CampusOverview
		if err := rows.Scan(&c.ID, &c.Name); err != nil {
			rows.Close()
			serverError(w, err)
			return
		}
		c.Classes = []ClassToday{}
		list = append(list, c)
	}
	rows.Close()

	for i := range list {
		c := &list[i]
		crows, err := a.db.Query(`SELECT cl.id, cl.name, COALESCE(t.name,''), COUNT(s.id),
			COALESCE(SUM(at.checkin_at != ''),0), COALESCE(SUM(at.checkin_at != '' AND at.late),0), COALESCE(SUM(at.checkout_at != ''),0)
			FROM classes cl LEFT JOIN users t ON t.id = cl.teacher_id LEFT JOIN students s ON s.class_id = cl.id
			LEFT JOIN attendance at ON at.student_id = s.id AND at.date = ?
			WHERE cl.campus_id = ? GROUP BY cl.id ORDER BY cl.name`, today, c.ID)
		if err != nil {
			serverError(w, err)
			return
		}
		for crows.Next() {
			var k ClassToday
			if err := crows.Scan(&k.ID, &k.Name, &k.Teacher, &k.Students, &k.Present, &k.Late, &k.Left); err != nil {
				crows.Close()
				serverError(w, err)
				return
			}
			c.Classes = append(c.Classes, k)
			c.Students += k.Students
			c.Present += k.Present
			c.Late += k.Late
			c.Left += k.Left
		}
		crows.Close()

		const inCampus = `sf.student_id IN (SELECT s.id FROM students s JOIN classes cl ON cl.id = s.class_id WHERE cl.campus_id = ?)`
		err = a.db.QueryRow(`SELECT
			COALESCE(SUM(sf.status = 'unpaid'),0), COALESCE(SUM(CASE WHEN sf.status = 'unpaid' THEN sf.amount_cents END),0),
			COALESCE(SUM(sf.status = 'unpaid' AND f.due_date != '' AND f.due_date < ?),0),
			COALESCE(SUM(CASE WHEN sf.status = 'unpaid' AND f.due_date != '' AND f.due_date < ? THEN sf.amount_cents END),0),
			COALESCE(SUM(CASE WHEN sf.status = 'paid' AND sf.paid_at LIKE ? THEN sf.amount_cents END),0)
			FROM student_fees sf JOIN fee_items f ON f.id = sf.fee_item_id WHERE `+inCampus, today, today, month+"%", c.ID).
			Scan(&c.UnpaidCount, &c.UnpaidCents, &c.OverdueCount, &c.OverdueCents, &c.CollectedMonthCents)
		if err != nil {
			serverError(w, err)
			return
		}
		if err := a.db.QueryRow(`SELECT COUNT(*) FROM receipts rc JOIN students s ON s.id = rc.student_id
			JOIN classes cl ON cl.id = s.class_id WHERE rc.status = 'pending' AND cl.campus_id = ?`, c.ID).Scan(&c.PendingReceipts); err != nil {
			serverError(w, err)
			return
		}
	}
	writeJSON(w, map[string]any{"date": today, "campuses": list})
}
