package api

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"

	"github.com/gofiber/fiber/v2"
	"github.com/jackc/pgx/v5"
)

const maxDocumentBytes = 5 * 1024 * 1024

type invalidDocumentError struct{ message string }

func (e invalidDocumentError) Error() string { return e.message }

func validateDocument(data []byte) (string, error) {
	if len(data) == 0 || len(data) > maxDocumentBytes {
		return "", errors.New("Ukuran dokumen harus 1 byte sampai 5 MB")
	}
	contentType := http.DetectContentType(data)
	switch contentType {
	case "application/pdf":
		if !bytes.HasPrefix(data, []byte("%PDF-")) || !bytes.HasSuffix(bytes.TrimSpace(data), []byte("%%EOF")) {
			return "", errors.New("Dokumen PDF tidak lengkap")
		}
	case "image/png", "image/jpeg":
		config, _, err := image.DecodeConfig(bytes.NewReader(data))
		if err != nil || config.Width <= 0 || config.Height <= 0 || int64(config.Width)*int64(config.Height) > 40000000 {
			return "", errors.New("Gambar tidak valid atau terlalu besar")
		}
	default:
		return "", errors.New("Hanya PDF, JPEG dan PNG yang diizinkan")
	}
	return contentType, nil
}
func documentFilename(name string) string {
	name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, name)
	if len(name) > 200 {
		name = "document" + filepath.Ext(name)
	}
	if name == "" || name == "." || name == ".." || name == "/" {
		return "document"
	}
	return name
}
func canReadDocument(r string, uid, owner int64) bool {
	return uid == owner || r == "admin"
}

func (a *API) documents(r fiber.Router) {
	r.Get("/documents", func(c *fiber.Ctx) error {
		switch role(c) {
		case "admin":
			return a.documentRows(c, `SELECT d.id,d.user_id,u.name AS owner_name,u.email AS owner_email,d.application_id,d.kind,d.filename,d.mime_type,d.size,d.review_status,d.review_reason,d.created_at FROM documents d JOIN users u ON u.id=d.user_id ORDER BY d.id DESC`)
		case "committee":
			return a.documentRows(c, `SELECT DISTINCT d.id,d.user_id,u.name AS owner_name,d.application_id,d.kind,d.filename,d.mime_type,d.size,d.review_status,d.review_reason,d.created_at FROM documents d JOIN users u ON u.id=d.user_id LEFT JOIN loan_application_documents lad ON lad.document_id=d.id LEFT JOIN loan_application_assignments laa ON laa.application_id=COALESCE(lad.application_id,d.application_id) AND laa.committee_user_id=$1 AND laa.unassigned_at IS NULL WHERE d.user_id=$1 OR laa.id IS NOT NULL ORDER BY d.id DESC`, actor(c))
		case "member":
			return a.documentRows(c, `SELECT id,application_id,kind,filename,mime_type,size,review_status,review_reason,created_at FROM documents WHERE user_id=$1 ORDER BY id DESC`, actor(c))
		default:
			return a.documentRows(c, `SELECT id,kind,filename,mime_type,size,created_at FROM documents WHERE user_id=$1 ORDER BY id DESC`, actor(c))
		}
	})
	r.Post("/documents", a.uploadDocument)
	r.Post("/documents/:id/review", require("admin"), a.reviewDocument)
	r.Get("/documents/:id/download", a.downloadDocument)
	r.Get("/documents/:id/preview", a.previewDocument)
	r.Get("/notifications", func(c *fiber.Ctx) error {
		return a.documentRows(c, `SELECT id,title,message,read,created_at FROM notifications WHERE user_id=$1 ORDER BY id DESC LIMIT 500`, actor(c))
	})
	r.Patch("/notifications/:id/read", func(c *fiber.Ctx) error {
		id, err := strconv.ParseInt(c.Params("id"), 10, 64)
		if err != nil || id <= 0 {
			return fail(c, 400, "invalid_id", "ID tidak valid")
		}
		result, err := a.DB.Exec(c.UserContext(), `UPDATE notifications SET read=true WHERE id=$1 AND user_id=$2`, id, actor(c))
		if err != nil {
			return err
		}
		if result.RowsAffected() == 0 {
			return fail(c, 404, "not_found", "Notifikasi tidak ditemukan")
		}
		return ok(c, fiber.Map{"id": id, "read": true})
	})
	r.Get("/audit-logs", require("admin"), func(c *fiber.Ctx) error {
		return a.documentRows(c, `SELECT a.id,a.actor_id,u.name actor_name,a.action,a.entity,a.entity_id,a."before",a."after",a.created_at FROM audit_logs a LEFT JOIN users u ON u.id=a.actor_id ORDER BY a.id DESC LIMIT 1000`)
	})
}

// documentRows avoids exposing private local storage paths in API metadata.
func (a *API) documentRows(c *fiber.Ctx, query string, args ...any) error {
	rows, err := a.DB.Query(c.UserContext(), query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	result := []map[string]any{}
	fields := rows.FieldDescriptions()
	for rows.Next() {
		v, err := rows.Values()
		if err != nil {
			return err
		}
		m := map[string]any{}
		for i, f := range fields {
			m[f.Name] = v[i]
		}
		result = append(result, m)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return ok(c, result)
}

// persistValidatedDocument stores a validated upload under an opaque random
// name. The caller must invoke cleanup unless its database transaction commits.
func (a *API) persistValidatedDocument(header *multipart.FileHeader) (storageName, filename, contentType string, size int64, cleanup func(), err error) {
	if header == nil {
		return "", "", "", 0, nil, invalidDocumentError{"Dokumen identitas wajib diunggah"}
	}
	if header.Size > maxDocumentBytes {
		return "", "", "", 0, nil, invalidDocumentError{"Berkas maksimum 5 MB"}
	}
	input, err := header.Open()
	if err != nil {
		return "", "", "", 0, nil, err
	}
	data, readErr := io.ReadAll(io.LimitReader(input, maxDocumentBytes+1))
	closeErr := input.Close()
	if readErr != nil {
		return "", "", "", 0, nil, readErr
	}
	if closeErr != nil {
		return "", "", "", 0, nil, closeErr
	}
	if len(data) > maxDocumentBytes {
		return "", "", "", 0, nil, invalidDocumentError{"Berkas maksimum 5 MB"}
	}
	contentType, err = validateDocument(data)
	if err != nil {
		return "", "", "", 0, nil, invalidDocumentError{err.Error()}
	}
	dir := a.UploadDir
	if dir == "" {
		dir = "data/uploads"
	}
	if err = os.MkdirAll(dir, 0700); err != nil {
		return "", "", "", 0, nil, err
	}
	random := make([]byte, 24)
	if _, err = rand.Read(random); err != nil {
		return "", "", "", 0, nil, err
	}
	storageName = hex.EncodeToString(random)
	fullPath := filepath.Join(dir, storageName)
	file, err := os.OpenFile(fullPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return "", "", "", 0, nil, err
	}
	_, writeErr := file.Write(data)
	closeErr = file.Close()
	if writeErr != nil {
		_ = os.Remove(fullPath)
		return "", "", "", 0, nil, writeErr
	}
	if closeErr != nil {
		_ = os.Remove(fullPath)
		return "", "", "", 0, nil, closeErr
	}
	return storageName, documentFilename(header.Filename), contentType, int64(len(data)), func() { _ = os.Remove(fullPath) }, nil
}

func (a *API) uploadDocument(c *fiber.Ctx) error {
	kind := c.FormValue("kind")
	if !contains([]string{"identity", "profile_photo", "supporting", "loan", "payment", "selfie_ktp", "family_card", "npwp", "domicile", "bank_book", "salary_debit_authorization", "salary_slip", "business_proof", "bank_statement", "spouse_id", "marriage_certificate", "bpkb", "stnk", "vehicle_invoice", "vehicle_kir", "vehicle_photo", "property_certificate", "pbb", "property_photo", "collateral_photo"}, kind) {
		return fail(c, 400, "validation", "Jenis dokumen tidak valid")
	}
	var appID *int64
	if value := c.FormValue("application_id"); value != "" {
		id, err := strconv.ParseInt(value, 10, 64)
		if err != nil || id <= 0 {
			return fail(c, 400, "validation", "ID pengajuan tidak valid")
		}
		appID = &id
		var owner int64
		err = a.DB.QueryRow(c.UserContext(), `SELECT m.user_id FROM loan_applications l JOIN members m ON m.id=l.member_id WHERE l.id=$1`, id).Scan(&owner)
		if errors.Is(err, pgx.ErrNoRows) {
			return fail(c, 404, "not_found", "Pengajuan tidak ditemukan")
		}
		if err != nil {
			return err
		}
		allowed := canReadDocument(role(c), actor(c), owner)
		if role(c) == "committee" {
			allowed, err = a.isAssignedCommittee(c, id)
			if err != nil {
				return err
			}
		}
		if !allowed {
			return fail(c, 403, "forbidden", "Tidak berhak mengakses pengajuan")
		}
	}
	header, err := c.FormFile("file")
	if err != nil {
		return fail(c, 400, "validation", "Berkas wajib diunggah")
	}
	if header.Size > maxDocumentBytes {
		return fail(c, 413, "file_too_large", "Berkas maksimum 5 MB")
	}
	input, err := header.Open()
	if err != nil {
		return err
	}
	defer input.Close()
	data, err := io.ReadAll(io.LimitReader(input, maxDocumentBytes+1))
	if err != nil {
		return err
	}
	contentType, err := validateDocument(data)
	if err != nil {
		return fail(c, 400, "invalid_document", err.Error())
	}
	dir := a.UploadDir
	if dir == "" {
		dir = "data/uploads"
	}
	if err = os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	random := make([]byte, 24)
	if _, err = rand.Read(random); err != nil {
		return err
	}
	storageName := hex.EncodeToString(random)
	fullPath := filepath.Join(dir, storageName)
	file, err := os.OpenFile(fullPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			os.Remove(fullPath)
		}
	}()
	_, writeErr := file.Write(data)
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	tx, err := a.DB.Begin(c.UserContext())
	if err != nil {
		return err
	}
	defer tx.Rollback(c.UserContext())
	var id int64
	name := documentFilename(header.Filename)
	err = tx.QueryRow(c.UserContext(), `INSERT INTO documents(user_id,application_id,kind,filename,path,mime_type,size) VALUES($1,$2,$3,$4,$5,$6,$7) RETURNING id`, actor(c), appID, kind, name, storageName, contentType, len(data)).Scan(&id)
	if err != nil {
		return err
	}
	result := fiber.Map{"id": id, "user_id": actor(c), "application_id": appID, "kind": kind, "filename": name, "mime_type": contentType, "size": len(data)}
	if err = audit(c.UserContext(), tx, actor(c), "upload", "documents", id, nil, result); err != nil {
		return err
	}
	if err = tx.Commit(c.UserContext()); err != nil {
		return err
	}
	committed = true
	c.Status(201)
	return ok(c, result)
}
func (a *API) reviewDocument(c *fiber.Ctx) error {
	var b struct {
		Approve bool   `json:"approve"`
		Reason  string `json:"reason"`
	}
	if c.BodyParser(&b) != nil || strings.TrimSpace(b.Reason) == "" {
		return fail(c, 400, "review_reason_required", "Keputusan dan alasan pemeriksaan wajib diisi")
	}
	next := "rejected"
	if b.Approve {
		next = "verified"
	}
	tx, err := a.DB.Begin(c.Context())
	if err != nil {
		return err
	}
	defer tx.Rollback(c.Context())
	var owner int64
	var old string
	err = tx.QueryRow(c.Context(), `SELECT user_id,review_status FROM documents WHERE id=$1 FOR UPDATE`, id(c)).Scan(&owner, &old)
	if errors.Is(err, pgx.ErrNoRows) {
		return fiber.ErrNotFound
	}
	if err != nil {
		return err
	}
	if old != "pending" {
		return fail(c, 409, "document_already_reviewed", "Dokumen sudah pernah ditinjau")
	}
	if _, err = tx.Exec(c.Context(), `UPDATE documents SET review_status=$1,review_reason=$2,reviewed_by=$3,reviewed_at=now() WHERE id=$4`, next, strings.TrimSpace(b.Reason), actor(c), id(c)); err != nil {
		return err
	}
	if _, err = tx.Exec(c.Context(), `INSERT INTO notifications(user_id,title,message) VALUES($1,'Status dokumen diperbarui',$2)`, owner, next+": "+strings.TrimSpace(b.Reason)); err != nil {
		return err
	}
	if err = audit(c.Context(), tx, actor(c), "document_reviewed", "documents", id(c), fiber.Map{"review_status": old}, fiber.Map{"review_status": next, "reason": b.Reason}); err != nil {
		return err
	}
	if err = tx.Commit(c.Context()); err != nil {
		return err
	}
	return ok(c, fiber.Map{"id": id(c), "review_status": next})
}

func (a *API) downloadDocument(c *fiber.Ctx) error { return a.sendDocument(c, false) }
func (a *API) previewDocument(c *fiber.Ctx) error  { return a.sendDocument(c, true) }
func (a *API) sendDocument(c *fiber.Ctx, inline bool) error {
	docID, err := strconv.ParseInt(c.Params("id"), 10, 64)
	if err != nil || docID <= 0 {
		return fail(c, 400, "invalid_id", "ID tidak valid")
	}
	var owner int64
	var name, storageName, contentType string
	var appID *int64
	err = a.DB.QueryRow(c.UserContext(), `SELECT user_id,filename,path,mime_type,COALESCE(application_id,(SELECT application_id FROM loan_application_documents WHERE document_id=documents.id ORDER BY linked_at DESC LIMIT 1)) FROM documents WHERE id=$1`, docID).Scan(&owner, &name, &storageName, &contentType, &appID)
	if errors.Is(err, pgx.ErrNoRows) {
		return fail(c, 404, "not_found", "Dokumen tidak ditemukan")
	}
	if err != nil {
		return err
	}
	allowed := canReadDocument(role(c), actor(c), owner)
	if role(c) == "committee" && appID != nil {
		allowed, err = a.isAssignedCommittee(c, *appID)
		if err != nil {
			return err
		}
	}
	if !allowed {
		return fail(c, 403, "forbidden", "Tidak berhak mengakses dokumen")
	}
	if len(storageName) != 48 {
		return fmt.Errorf("invalid document storage key")
	}
	if _, err = hex.DecodeString(storageName); err != nil {
		return fmt.Errorf("invalid document storage key")
	}
	dir := a.UploadDir
	if dir == "" {
		dir = "data/uploads"
	}
	path := filepath.Join(dir, storageName)
	if _, err = os.Stat(path); os.IsNotExist(err) {
		return fail(c, 404, "not_found", "Berkas dokumen tidak tersedia")
	} else if err != nil {
		return err
	}
	c.Set("Content-Type", contentType)
	c.Set("X-Content-Type-Options", "nosniff")
	c.Set("Cache-Control", "private, no-store")
	if inline {
		c.Set("Content-Disposition", mime.FormatMediaType("inline", map[string]string{"filename": documentFilename(name)}))
	} else {
		c.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": documentFilename(name)}))
	}
	return c.SendFile(path, false)
}

func (a *API) isAssignedCommittee(c *fiber.Ctx, applicationID int64) (bool, error) {
	var assigned bool
	err := a.DB.QueryRow(c.Context(), `SELECT EXISTS(SELECT 1 FROM loan_application_assignments WHERE application_id=$1 AND committee_user_id=$2 AND unassigned_at IS NULL)`, applicationID, actor(c)).Scan(&assigned)
	return assigned, err
}
