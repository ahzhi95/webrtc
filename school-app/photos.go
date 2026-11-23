package main

import (
	"bytes"
	"fmt"
	"image"
	"image/jpeg"
	_ "image/png" // register PNG decoder
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp" // register WebP decoder
)

// Scene tags teachers choose when uploading (场景标签).
var photoTags = map[string]bool{"class": true, "outdoor": true, "art": true, "music": true, "meal": true,
	"festival": true, "birthday": true, "trip": true, "other": true}

// photoDay: the day a photo was taken (older photos without one use the upload day).
const photoDay = `COALESCE(NULLIF(p.taken_on, ''), substr(p.created_at, 1, 10))`

const (
	maxPhotosPerUpload = 30
	maxPhotoBytes      = 15 << 20 // per original file
	photoFullSize      = 1920     // longest side, pixels
	photoThumbSize     = 480
)

func (a *App) photoDir() string { return filepath.Join(a.uploadDir, "photos") }

// processPhoto decodes an uploaded image and re-encodes it as JPEG at two sizes.
// Re-encoding drops all metadata (EXIF), including the GPS location phones store in photos.
func processPhoto(r io.Reader) (full, thumb []byte, err error) {
	img, _, err := image.Decode(r)
	if err != nil {
		return nil, nil, fmt.Errorf("not a supported image")
	}
	if full, err = encodeScaled(img, photoFullSize, 85); err != nil {
		return nil, nil, err
	}
	if thumb, err = encodeScaled(img, photoThumbSize, 78); err != nil {
		return nil, nil, err
	}
	return full, thumb, nil
}

func encodeScaled(src image.Image, maxSide, quality int) ([]byte, error) {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w == 0 || h == 0 {
		return nil, fmt.Errorf("empty image")
	}
	if w > maxSide || h > maxSide {
		if w >= h {
			h, w = h*maxSide/w, maxSide
		} else {
			w, h = w*maxSide/h, maxSide
		}
	}
	dst := image.NewRGBA(image.Rect(0, 0, max(w, 1), max(h, 1)))
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, b, draw.Src, nil)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: quality}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ---------- upload ----------

func (a *App) handleUploadPhotos(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxPhotosPerUpload*maxPhotoBytes)
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		httpError(w, http.StatusBadRequest, "upload too large")
		return
	}
	defer r.MultipartForm.RemoveAll()

	// Which children are in the photos.
	var studentIDs []int64
	seen := map[int64]bool{}
	for _, v := range r.MultipartForm.Value["student_id"] {
		id, err := strconv.ParseInt(v, 10, 64)
		if err == nil && id > 0 && !seen[id] {
			seen[id] = true
			studentIDs = append(studentIDs, id)
		}
	}
	if len(studentIDs) == 0 {
		httpError(w, http.StatusBadRequest, "choose at least one student")
		return
	}
	for _, id := range studentIDs {
		if !a.canAccessStudent(currentUser(r), id) {
			httpError(w, http.StatusBadRequest, "student not found")
			return
		}
	}
	files := r.MultipartForm.File["photo"]
	if len(files) == 0 {
		httpError(w, http.StatusBadRequest, "please choose a file")
		return
	}
	if len(files) > maxPhotosPerUpload {
		httpError(w, http.StatusBadRequest, fmt.Sprintf("at most %d photos at a time", maxPhotosPerUpload))
		return
	}
	caption := strings.TrimSpace(r.FormValue("caption"))
	if len(caption) > 500 {
		caption = caption[:500]
	}
	tag := r.FormValue("tag")
	if !photoTags[tag] {
		tag = "other"
	}
	takenOn := a.now().Format("2006-01-02")
	if v := r.FormValue("taken_on"); v != "" {
		t, err := time.ParseInLocation("2006-01-02", v, a.loc)
		if err != nil || t.After(a.now()) {
			httpError(w, http.StatusBadRequest, "invalid date")
			return
		}
		takenOn = v
	}
	if err := os.MkdirAll(a.photoDir(), 0o750); err != nil {
		serverError(w, err)
		return
	}

	// Process every image first, so a bad file doesn't leave half an upload behind.
	type processed struct{ full, thumb []byte }
	var imgs []processed
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
		full, thumb, err := processPhoto(f)
		f.Close()
		if err != nil {
			httpError(w, http.StatusBadRequest, "only JPG, PNG or WEBP photos")
			return
		}
		imgs = append(imgs, processed{full, thumb})
	}

	u := currentUser(r)
	now := a.nowStr()
	var written []string
	cleanup := func() {
		for _, p := range written {
			os.Remove(p)
		}
	}
	tx, err := a.db.Begin()
	if err != nil {
		serverError(w, err)
		return
	}
	defer tx.Rollback()
	for _, im := range imgs {
		name := randomToken(16)
		for suffix, data := range map[string][]byte{".jpg": im.full, "_t.jpg": im.thumb} {
			p := filepath.Join(a.photoDir(), name+suffix)
			if err := os.WriteFile(p, data, 0o640); err != nil {
				cleanup()
				serverError(w, err)
				return
			}
			written = append(written, p)
		}
		res, err := tx.Exec(`INSERT INTO photos (file_name, caption, uploaded_by, created_at, tag, taken_on) VALUES (?,?,?,?,?,?)`,
			name, caption, u.ID, now, tag, takenOn)
		if err != nil {
			cleanup()
			serverError(w, err)
			return
		}
		pid, _ := res.LastInsertId()
		for _, sid := range studentIDs {
			if _, err := tx.Exec(`INSERT INTO photo_students (photo_id, student_id) VALUES (?,?)`, pid, sid); err != nil {
				cleanup()
				serverError(w, err)
				return
			}
		}
	}
	if err := tx.Commit(); err != nil {
		cleanup()
		serverError(w, err)
		return
	}
	go a.notifier.notifyPhotos(studentIDs, len(imgs))
	writeJSON(w, map[string]int{"uploaded": len(imgs)})
}

// ---------- list / view / delete ----------

type PhotoStudent struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

type Photo struct {
	ID         int64          `json:"id"`
	Caption    string         `json:"caption"`
	Tag        string         `json:"tag"`
	TakenOn    string         `json:"taken_on"`
	UploadedBy string         `json:"uploaded_by"`
	UploaderID int64          `json:"uploader_id"`
	CreatedAt  string         `json:"created_at"`
	Students   []PhotoStudent `json:"students"`
}

func (a *App) handleListPhotos(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	q := r.URL.Query()
	where, args := []string{"1=1"}, []any{}
	// Parents only ever see photos that include one of their own children.
	if u.Role == "parent" {
		where = append(where, `p.id IN (SELECT ps.photo_id FROM photo_students ps
			JOIN parent_students par ON par.student_id = ps.student_id WHERE par.parent_id = ?)`)
		args = append(args, u.ID)
	}
	if s := scope(u); s != 0 {
		where = append(where, `p.id IN (SELECT ps.photo_id FROM photo_students ps JOIN students st ON st.id = ps.student_id
			JOIN classes c ON c.id = st.class_id WHERE c.campus_id = ?)`)
		args = append(args, s)
	}
	if sid := q.Get("student_id"); sid != "" {
		where = append(where, `p.id IN (SELECT photo_id FROM photo_students WHERE student_id = ?)`)
		args = append(args, sid)
	}
	if tag := q.Get("tag"); photoTags[tag] {
		where, args = append(where, "p.tag = ?"), append(args, tag)
	}
	for _, d := range []struct{ key, op string }{{"from", ">="}, {"to", "<="}} {
		if v := q.Get(d.key); v != "" {
			if _, err := time.Parse("2006-01-02", v); err == nil {
				where, args = append(where, photoDay+" "+d.op+" ?"), append(args, v)
			}
		}
	}
	if cid := q.Get("class_id"); cid != "" && u.Role != "parent" {
		where = append(where, `p.id IN (SELECT ps.photo_id FROM photo_students ps JOIN students s ON s.id = ps.student_id WHERE s.class_id = ?)`)
		args = append(args, cid)
	}
	limit := 60
	if n, err := strconv.Atoi(q.Get("limit")); err == nil && n > 0 && n <= 200 {
		limit = n
	}
	where = append(where, "p.id < ?")
	before, err := strconv.ParseInt(q.Get("before"), 10, 64)
	if err != nil || before <= 0 {
		before = 1 << 62
	}
	args = append(args, before)

	// Newest day first (the day the photo was taken), then newest upload.
	rows, err := a.db.Query(`SELECT p.id, p.caption, p.tag, `+photoDay+`, COALESCE(u.name,''), COALESCE(p.uploaded_by,0), p.created_at FROM photos p
		LEFT JOIN users u ON u.id = p.uploaded_by WHERE `+strings.Join(where, " AND ")+
		fmt.Sprintf(` ORDER BY `+photoDay+` DESC, p.id DESC LIMIT %d`, limit), args...)
	if err != nil {
		serverError(w, err)
		return
	}
	list := []Photo{}
	index := map[int64]int{}
	for rows.Next() {
		var p Photo
		if err := rows.Scan(&p.ID, &p.Caption, &p.Tag, &p.TakenOn, &p.UploadedBy, &p.UploaderID, &p.CreatedAt); err != nil {
			rows.Close()
			serverError(w, err)
			return
		}
		p.Students = []PhotoStudent{}
		index[p.ID] = len(list)
		list = append(list, p)
	}
	rows.Close()
	if len(list) == 0 {
		writeJSON(w, list)
		return
	}

	// Attach the children in each photo. A parent only sees the names of their own children.
	tagQ := `SELECT ps.photo_id, s.id, s.name FROM photo_students ps JOIN students s ON s.id = ps.student_id
		WHERE ps.photo_id BETWEEN ? AND ?`
	minID, maxID := list[0].ID, list[0].ID
	for _, p := range list {
		minID, maxID = min(minID, p.ID), max(maxID, p.ID)
	}
	tagArgs := []any{minID, maxID}
	if u.Role == "parent" {
		tagQ += ` AND s.id IN (SELECT student_id FROM parent_students WHERE parent_id = ?)`
		tagArgs = append(tagArgs, u.ID)
	}
	rows, err = a.db.Query(tagQ+` ORDER BY s.name`, tagArgs...)
	if err != nil {
		serverError(w, err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var pid int64
		var s PhotoStudent
		if err := rows.Scan(&pid, &s.ID, &s.Name); err != nil {
			serverError(w, err)
			return
		}
		if i, ok := index[pid]; ok {
			list[i].Students = append(list[i].Students, s)
		}
	}
	writeJSON(w, list)
}

// canSeePhoto: staff see every photo; a parent only photos that include their child.
func (a *App) canSeePhoto(u *User, photoID int64) bool {
	if u.Role != "parent" {
		if scope(u) == 0 {
			return true
		}
		var one int
		return a.db.QueryRow(`SELECT 1 FROM photo_students ps JOIN students st ON st.id = ps.student_id
			JOIN classes c ON c.id = st.class_id WHERE ps.photo_id = ? AND c.campus_id = ? LIMIT 1`, photoID, scope(u)).Scan(&one) == nil
	}
	var one int
	return a.db.QueryRow(`SELECT 1 FROM photo_students ps JOIN parent_students par ON par.student_id = ps.student_id
		WHERE ps.photo_id = ? AND par.parent_id = ? LIMIT 1`, photoID, u.ID).Scan(&one) == nil
}

func (a *App) handlePhotoImage(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		httpError(w, http.StatusBadRequest, "invalid id")
		return
	}
	u := currentUser(r)
	var name, created string
	if err := a.db.QueryRow(`SELECT file_name, created_at FROM photos WHERE id = ?`, id).Scan(&name, &created); err != nil {
		serverError(w, err)
		return
	}
	if !a.canSeePhoto(u, id) {
		httpError(w, http.StatusForbidden, "permission denied")
		return
	}
	suffix := ".jpg"
	if r.URL.Query().Get("size") == "thumb" {
		suffix = "_t.jpg"
	}
	f, err := os.Open(filepath.Join(a.photoDir(), filepath.Base(name)+suffix))
	if err != nil {
		serverError(w, fmt.Errorf("open photo %d: %w", id, err))
		return
	}
	defer f.Close()
	mod, _ := time.Parse(time.RFC3339, created)
	w.Header().Set("Content-Type", "image/jpeg")
	// Revalidate every time, so a photo can't be shown from cache to someone who has since logged out.
	w.Header().Set("Cache-Control", "private, no-cache")
	if r.URL.Query().Get("download") == "1" {
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="photo-%d.jpg"`, id))
	}
	http.ServeContent(w, r, "", mod, f)
}

func (a *App) handleDeletePhoto(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		httpError(w, http.StatusBadRequest, "invalid id")
		return
	}
	u := currentUser(r)
	var name string
	var uploader int64
	if err := a.db.QueryRow(`SELECT file_name, COALESCE(uploaded_by,0) FROM photos WHERE id = ?`, id).Scan(&name, &uploader); err != nil {
		serverError(w, err)
		return
	}
	if (u.Role != "admin" && uploader != u.ID) || !a.canSeePhoto(u, id) {
		httpError(w, http.StatusForbidden, "permission denied")
		return
	}
	if _, err := a.db.Exec(`DELETE FROM photos WHERE id = ?`, id); err != nil {
		serverError(w, err)
		return
	}
	os.Remove(filepath.Join(a.photoDir(), filepath.Base(name)+".jpg"))
	os.Remove(filepath.Join(a.photoDir(), filepath.Base(name)+"_t.jpg"))
	writeJSON(w, map[string]bool{"ok": true})
}

// ---------- notification ----------

// Photos are often uploaded in several rounds, and brothers and sisters appear in different ones.
// New-photo notices are held for two minutes and sent per parent with a count for each child:
// "新照片：林小红 3 张、林小华 2 张".
const photoBatchDelay = 2 * time.Minute

type photoBatch struct {
	order  []string
	counts map[string]int
}

// notifyPhotos queues count new photos for each parent of the given students.
func (n *Notifier) notifyPhotos(studentIDs []int64, count int) {
	type kid struct {
		parent int64
		name   string
	}
	var found []kid
	for _, sid := range studentIDs {
		rows, err := n.db.Query(`SELECT u.id, s.name FROM parent_students ps JOIN users u ON u.id = ps.parent_id
			JOIN students s ON s.id = ps.student_id WHERE ps.student_id = ? AND u.disabled = 0`, sid)
		if err != nil {
			logError(err)
			return
		}
		for rows.Next() {
			var k kid
			if rows.Scan(&k.parent, &k.name) == nil {
				found = append(found, k)
			}
		}
		rows.Close()
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.photoPending == nil {
		n.photoPending = map[int64]*photoBatch{}
	}
	for _, k := range found {
		b := n.photoPending[k.parent]
		if b == nil {
			b = &photoBatch{counts: map[string]int{}}
			n.photoPending[k.parent] = b
			pid := k.parent
			time.AfterFunc(photoBatchDelay, func() { n.flushPhotos(pid) })
		}
		if _, seen := b.counts[k.name]; !seen {
			b.order = append(b.order, k.name)
		}
		b.counts[k.name] += count
	}
}

func (n *Notifier) flushPhotos(parentID int64) {
	n.mu.Lock()
	b := n.photoPending[parentID]
	delete(n.photoPending, parentID)
	n.mu.Unlock()
	if b == nil || len(b.order) == 0 {
		return
	}
	n.deliver(parentID, 0, "新照片 / New photos", photoMessage(b), "/#photos", "photos", "")
}

func photoMessage(b *photoBatch) string {
	if len(b.order) == 1 {
		name, c := b.order[0], b.counts[b.order[0]]
		return fmt.Sprintf("老师上传了 %d 张 %s 的照片\n%d new photo(s) of %s", c, name, c, name)
	}
	var zh, en []string
	for _, name := range b.order {
		zh = append(zh, fmt.Sprintf("%s %d 张", name, b.counts[name]))
		en = append(en, fmt.Sprintf("%s %d", name, b.counts[name]))
	}
	return "新照片：" + strings.Join(zh, "、") + "\nNew photos: " + strings.Join(en, ", ")
}
