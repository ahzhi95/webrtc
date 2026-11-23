package main

import (
	"net/http"
	"strings"
)

// Information pages (关于我们): each school's introduction and contact details, plus the organisation's
// introduction, terms of use and privacy policy. Schools edit their own details; the top-level admin
// edits the three shared pages. Terms and privacy are readable before logging in.

var pageKeys = map[string]bool{"about": true, "terms": true, "privacy": true}

func (a *App) pageText(key string) string {
	var v string
	if a.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, "page_"+key).Scan(&v) == nil && strings.TrimSpace(v) != "" {
		return v
	}
	return defaultPages[key]
}

// handlePage: GET /api/pages/{key} — public for terms and privacy.
func (a *App) handlePage(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	if !pageKeys[key] {
		httpError(w, http.StatusNotFound, "not found")
		return
	}
	if key == "about" {
		if _, err := a.userFromRequest(r); err != nil {
			httpError(w, http.StatusUnauthorized, "not logged in")
			return
		}
	}
	var custom string
	_ = a.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, "page_"+key).Scan(&custom)
	writeJSON(w, map[string]any{"key": key, "text": a.pageText(key), "is_default": strings.TrimSpace(custom) == ""})
}

func (a *App) handleSavePage(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	if !pageKeys[key] || !isTopAdmin(currentUser(r)) {
		httpError(w, http.StatusForbidden, "permission denied")
		return
	}
	var req struct{ Text string }
	if !readJSON(w, r, &req) {
		return
	}
	if len(req.Text) > 100000 {
		httpError(w, http.StatusBadRequest, "text too long")
		return
	}
	if _, err := a.db.Exec(`INSERT INTO settings (key, value) VALUES (?,?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		"page_"+key, strings.TrimSpace(req.Text)); err != nil {
		serverError(w, err)
		return
	}
	okJSON(w)
}

// handleSchools: contact details of the schools this user belongs to (all schools for the top-level admin).
func (a *App) handleSchools(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	q := `SELECT id, name, reg_no, address, phone, email, receipt_prefix, about, whatsapp, hours FROM campuses`
	var args []any
	switch {
	case u.Role == "parent":
		q += ` WHERE id IN (SELECT c.campus_id FROM parent_students ps JOIN students s ON s.id = ps.student_id
			JOIN classes c ON c.id = s.class_id WHERE ps.parent_id = ?)`
		args = append(args, u.ID)
	case scope(u) != 0:
		q += ` WHERE id = ?`
		args = append(args, scope(u))
	}
	rows, err := a.db.Query(q+` ORDER BY name`, args...)
	if err != nil {
		serverError(w, err)
		return
	}
	defer rows.Close()
	type school struct {
		ID int64 `json:"id"`
		CampusInfo
		About    string `json:"about"`
		WhatsApp string `json:"whatsapp"`
		Hours    string `json:"hours"`
	}
	list := []school{}
	for rows.Next() {
		var s school
		if err := rows.Scan(&s.ID, &s.Name, &s.RegNo, &s.Address, &s.Phone, &s.Email, &s.ReceiptPrefix, &s.About, &s.WhatsApp, &s.Hours); err != nil {
			serverError(w, err)
			return
		}
		list = append(list, s)
	}
	writeJSON(w, list)
}

// Drafts for the school to review. Lines starting with "# " are headings and "- " are list items.
var defaultPages = map[string]string{
	"about": `# 关于我们 About us

我们是一所关心每一个孩子成长的幼儿园，提供安全、温暖、快乐的学习环境。
We are a kindergarten that cares about every child's growth, offering a safe, warm and joyful place to learn.

# App 功能 What this app does
- 到园 / 离园通知：孩子到园、离园时马上通知家长 Arrival and departure alerts
- 学校通告：重要消息不再错过 School announcements
- 生活照片：只有孩子自己的家长看得到 Daily photos, visible only to the child's own parents
- 缴费与正式收据：上传付款凭证，下载正式收据 Fees, payment proof and official receipts

（请管理员在“管理 → 校区”修改这段介绍。Admins can edit this text.）`,

	"terms": `# 用户协议 Terms of Use

（草稿，请学校审阅后修改 — Draft for the school to review）

# 1. 使用者 Who may use the app
本 App 只供本园的家长、监护人和职员使用。账号由学校建立，请勿把账号借给他人。
The app is for parents, guardians and staff of the kindergarten. Accounts are created by the school; do not share your account.

# 2. 账号安全 Your account
- 请妥善保管密码，第一次登录后请修改密码。Keep your password safe and change it after your first login.
- 如发现账号被他人使用，请立即通知学校。Tell the school at once if someone else uses your account.

# 3. 照片与资料 Photos and information
- 学校上传的照片只供家长私人保存，请勿在社交媒体公开分享有其他孩子的照片。Photos are for your family's private use; do not post photos showing other children on social media.
- 请提供正确的联系方式与孩子资料，以便学校在紧急时联系您。Keep your contact details and your child's information correct so the school can reach you in an emergency.

# 4. 缴费 Payments
上传付款凭证不代表已完成缴费；学校确认后才会开立正式收据。
Uploading proof of payment does not by itself complete the payment; the official receipt is issued after the school confirms it.

# 5. 服务变更 Changes
学校可能更新本 App 和本协议，重要的变更会通过 App 通知。
The school may update the app and these terms; important changes will be announced in the app.

# 6. 联系 Contact
如有疑问，请联系您孩子就读的学校（见“学校资料”）。
For questions, contact your child's school (see "School details").`,

	"privacy": `# 隐私政策 Privacy Policy

（草稿，依据马来西亚《2010年个人资料保护法》PDPA 编写，请学校审阅后修改 — Draft based on Malaysia's Personal Data Protection Act 2010, for the school to review）

# 1. 我们收集的资料 What we collect
- 家长 / 职员：姓名、电话、电邮、账号资料。Parents and staff: name, phone, email, account details.
- 孩子：姓名、学号、班级、出生日期、性别、健康 / 过敏备注、紧急联系人。Children: name, student number, class, date of birth, gender, health / allergy notes, emergency contact.
- 到园 / 离园时间与记录者。Arrival and departure times and who recorded them.
- 学校上传的生活照片。Photos taken by the school.
- 缴费资料与家长上传的付款凭证。Fees and the payment proofs parents upload.
- 家长提交的反馈。Feedback you send us.

# 2. 使用目的 Why we use it
用于照顾和接送孩子、与家长沟通、发送通知、处理学费，以及紧急时联系家长。我们不会把资料用于广告，也不会出售资料。
To care for children, manage arrival and pick-up, communicate with parents, send notices, handle fees, and contact you in an emergency. We do not use your data for advertising and never sell it.

# 3. 谁可以看到 Who can see it
- 家长只能看到自己孩子的资料与照片；合照只会给照片中孩子的家长。Parents only see their own children's records and photos; a group photo is shown only to the parents of the children in it.
- 老师只能看到所属学校的学生资料；缴费资料只限学校管理员。Teachers only see students of their own school; fee information is limited to school administrators.
- 除法律要求外，我们不会把资料交给第三方。We do not give your data to third parties unless the law requires it.

# 4. 保护措施 How we protect it
密码经过加密保存；照片上传时会删除位置等资料；资料存放在受保护的服务器并定期备份。
Passwords are stored encrypted; location data is removed from photos when uploaded; data is kept on a protected server and backed up regularly.

# 5. 保存期限 How long we keep it
孩子离园后，资料只会在法律或会计需要的期间内保存，之后删除。
After a child leaves, records are kept only as long as needed for legal or accounting purposes, then deleted.

# 6. 您的权利 Your rights
您可以要求查阅或更正您和孩子的个人资料，也可以撤回同意（但部分服务可能因此无法提供）。请联系学校办理。
You may ask to access or correct your and your child's personal data, or withdraw consent (some services may then be unavailable). Contact the school to do so.

# 7. 联系 Contact
个人资料相关事宜，请联系您孩子就读的学校（见“学校资料”）。
For personal data matters, contact your child's school (see "School details").`,
}
