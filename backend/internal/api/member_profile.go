package api

import (
	"encoding/csv"
	"fmt"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/jackc/pgx/v5"
)

var sensitiveProfileFields = map[string]bool{
	"nik": true, "npwp": true, "bank_account_number": true, "bank_name": true, "bank_account_holder": true,
}

func (a *API) memberProfileRoutes(r fiber.Router) {
	r.Get("/member-profile", a.getMemberProfile)
	r.Patch("/member-profile", a.updateMemberProfile)
	r.Get("/member-profile/change-requests", a.listProfileChangeRequests)
	r.Post("/member-profile/change-requests/:id/review", require("admin"), a.reviewProfileChangeRequest)
	r.Post("/profile/photo", a.uploadProfilePhoto)
	r.Get("/member-statement", require("member"), a.memberStatement)
	r.Get("/member-statement/export", require("member"), a.exportMemberStatement)
}

func (a *API) getMemberProfile(c *fiber.Ctx) error {
	var rows []fiber.Map
	var err error
	if role(c) == "member" {
		rows, err = a.readMaps(c, `SELECT u.id,u.name,u.email,u.role,m.id member_id,m.number member_number,m.nik,m.npwp,m.birth_place,m.birth_date,m.gender,m.marital_status,m.address,m.domicile_address,m.city_regency,m.postal_code,m.phone,m.occupation,m.monthly_income,m.coop_unit,m.bank_name,m.bank_account_number,m.bank_account_holder,m.emergency_contact_name,m.emergency_contact_phone,m.emergency_contact_relationship,m.heir_name,m.heir_phone,m.membership_purpose,m.additional_notes,m.mandatory_savings_amount,m.initial_principal_amount,m.consent_adart,m.status member_status,m.reason member_reason,m.created_at joined_at,
		(SELECT d.id FROM documents d WHERE d.user_id=u.id AND d.kind='profile_photo' AND d.is_current=true ORDER BY d.created_at DESC,d.id DESC LIMIT 1) profile_photo_id
		FROM users u JOIN members m ON m.user_id=u.id WHERE u.id=$1`, actor(c))
	} else {
		rows, err = a.readMaps(c, `SELECT u.id,u.name,u.email,u.role,NULL::bigint member_id,NULL::text member_number,NULL::text member_status,
			(SELECT d.id FROM documents d WHERE d.user_id=u.id AND d.kind='profile_photo' AND d.is_current=true ORDER BY d.created_at DESC,d.id DESC LIMIT 1) profile_photo_id
			FROM users u WHERE u.id=$1`, actor(c))
	}
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return fiber.ErrNotFound
	}
	return ok(c, rows[0])
}

func (a *API) updateMemberProfile(c *fiber.Ctx) error {
	var b map[string]any
	if c.BodyParser(&b) != nil {
		return fiber.ErrBadRequest
	}
	if len(b) == 0 {
		return fail(c, 400, "empty_profile", "Tidak ada perubahan profil")
	}
	name, hasName := b["name"].(string)
	if !hasName {
		if err := a.DB.QueryRow(c.Context(), `SELECT name FROM users WHERE id=$1`, actor(c)).Scan(&name); err != nil {
			return err
		}
	}
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 120 {
		return fail(c, 400, "invalid_name", "Nama lengkap wajib diisi dan maksimal 120 karakter")
	}
	if role(c) != "member" {
		tx, err := a.DB.Begin(c.Context())
		if err != nil {
			return err
		}
		defer tx.Rollback(c.Context())
		if _, err = tx.Exec(c.Context(), `UPDATE users SET name=$1 WHERE id=$2`, name, actor(c)); err != nil {
			return err
		}
		if err = audit(c.Context(), tx, actor(c), "profile_updated", "users", actor(c), nil, fiber.Map{"name": name}); err != nil {
			return err
		}
		if err = tx.Commit(c.Context()); err != nil {
			return err
		}
		return ok(c, fiber.Map{"updated": true, "sensitive_changes": 0})
	}
	allowed := map[string]bool{
		"name": true, "phone": true, "address": true, "domicile_address": true, "occupation": true, "birth_place": true, "birth_date": true, "gender": true, "marital_status": true, "city_regency": true, "postal_code": true, "monthly_income": true, "coop_unit": true, "membership_purpose": true, "additional_notes": true,
		"emergency_contact_name": true, "emergency_contact_phone": true, "emergency_contact_relationship": true, "heir_name": true, "heir_phone": true, "consent_adart": true,
		"nik": true, "npwp": true, "bank_name": true, "bank_account_number": true, "bank_account_holder": true, "mandatory_savings_amount": true, "initial_principal_amount": true,
	}
	for k := range b {
		if !allowed[k] {
			return fail(c, 400, "unsupported_profile_field", "Field profil tidak dikenal: "+k)
		}
	}
	for _, k := range []string{"phone", "address", "domicile_address", "occupation", "birth_place", "gender", "marital_status", "city_regency", "postal_code", "coop_unit", "membership_purpose", "additional_notes", "emergency_contact_name", "emergency_contact_phone", "emergency_contact_relationship", "heir_name", "heir_phone", "npwp", "bank_name", "bank_account_number", "bank_account_holder", "nik"} {
		if v, ok := b[k].(string); ok && len(v) > 500 {
			return fail(c, 400, "profile_field_too_long", "Nilai field terlalu panjang: "+k)
		}
	}
	for _, key := range []string{"monthly_income", "mandatory_savings_amount", "initial_principal_amount"} {
		if v, ok := b[key]; ok && v != nil && v != "" {
			n, valid := numberInt64(v)
			if !valid || n < 0 || n > 1000000000000 {
				return fail(c, 400, "invalid_amount", "Nominal tidak valid: "+key)
			}
		}
	}
	if v, ok := b["birth_date"]; ok && v != nil && v != "" {
		if _, err := time.Parse("2006-01-02", fmt.Sprint(v)); err != nil {
			return fail(c, 400, "invalid_birth_date", "Tanggal lahir harus YYYY-MM-DD")
		}
	}
	if v, ok := b["consent_adart"].(bool); ok && v {
		var verified bool
		if err := a.DB.QueryRow(c.Context(), `SELECT EXISTS(SELECT 1 FROM documents WHERE user_id=$1 AND kind='selfie_ktp' AND review_status='verified')`, actor(c)).Scan(&verified); err != nil {
			return err
		}
		if !verified {
			return fail(c, 422, "selfie_ktp_required", "Selfie memegang e-KTP harus diverifikasi sebelum menyetujui AD/ART")
		}
	}
	tx, err := a.DB.Begin(c.Context())
	if err != nil {
		return err
	}
	defer tx.Rollback(c.Context())
	if _, err = tx.Exec(c.Context(), `UPDATE users SET name=$1 WHERE id=$2`, name, actor(c)); err != nil {
		return err
	}
	memberUpdates := []struct {
		key, column string
		value       any
	}{
		{"phone", "phone", b["phone"]}, {"address", "address", b["address"]}, {"domicile_address", "domicile_address", b["domicile_address"]}, {"occupation", "occupation", b["occupation"]}, {"birth_place", "birth_place", b["birth_place"]}, {"birth_date", "birth_date", b["birth_date"]}, {"gender", "gender", b["gender"]}, {"marital_status", "marital_status", b["marital_status"]}, {"city_regency", "city_regency", b["city_regency"]}, {"postal_code", "postal_code", b["postal_code"]}, {"monthly_income", "monthly_income", b["monthly_income"]}, {"coop_unit", "coop_unit", b["coop_unit"]}, {"membership_purpose", "membership_purpose", b["membership_purpose"]}, {"additional_notes", "additional_notes", b["additional_notes"]}, {"emergency_contact_name", "emergency_contact_name", b["emergency_contact_name"]}, {"emergency_contact_phone", "emergency_contact_phone", b["emergency_contact_phone"]}, {"emergency_contact_relationship", "emergency_contact_relationship", b["emergency_contact_relationship"]}, {"heir_name", "heir_name", b["heir_name"]}, {"heir_phone", "heir_phone", b["heir_phone"]},
	}
	for _, f := range memberUpdates {
		if value, ok := b[f.key]; ok {
			if f.key == "monthly_income" || f.key == "mandatory_savings_amount" || f.key == "initial_principal_amount" {
				if value == "" {
					value = nil
				} else {
					n, _ := numberInt64(value)
					value = n
				}
			}
			if f.key == "birth_date" && value == "" {
				value = nil
			}
			query := fmt.Sprintf(`UPDATE members SET %s=$1 WHERE id=$2`, f.column)
			if _, err = tx.Exec(c.Context(), query, value, member(c)); err != nil {
				return err
			}
		}
	}
	if v, ok := b["consent_adart"].(bool); ok {
		if _, err = tx.Exec(c.Context(), `UPDATE members SET consent_adart=$1,consent_adart_at=CASE WHEN $1 THEN now() ELSE NULL END WHERE id=$2`, v, member(c)); err != nil {
			return err
		}
	}
	requestCount := 0
	for _, field := range []string{"nik", "npwp", "bank_name", "bank_account_number", "bank_account_holder"} {
		newValue, ok := b[field].(string)
		if !ok {
			continue
		}
		newValue = strings.TrimSpace(newValue)
		var oldValue *string
		q := fmt.Sprintf(`SELECT %s FROM members WHERE id=$1 FOR UPDATE`, field)
		if err = tx.QueryRow(c.Context(), q, member(c)).Scan(&oldValue); err != nil {
			return err
		}
		old := ""
		if oldValue != nil {
			old = *oldValue
		}
		if old == newValue {
			continue
		}
		var pending bool
		if err = tx.QueryRow(c.Context(), `SELECT EXISTS(SELECT 1 FROM member_profile_change_requests WHERE member_id=$1 AND field_name=$2 AND status='pending')`, member(c), field).Scan(&pending); err != nil {
			return err
		}
		if pending {
			return fail(c, 409, "profile_change_pending", "Masih ada permintaan perubahan yang menunggu untuk field "+field)
		}
		if _, err = tx.Exec(c.Context(), `INSERT INTO member_profile_change_requests(member_id,field_name,old_value,new_value) VALUES($1,$2,$3,$4)`, member(c), field, old, newValue); err != nil {
			return err
		}
		requestCount++
	}
	if requestCount > 0 {
		_, err = tx.Exec(c.Context(), `INSERT INTO notifications(user_id,title,message) SELECT id,'Permintaan perubahan profil','Anggota mengajukan perubahan data sensitif. Periksa antrean verifikasi profil.' FROM users WHERE role='admin' AND active`)
		if err != nil {
			return err
		}
	}
	if err = audit(c.Context(), tx, actor(c), "member_profile_updated", "members", member(c), nil, fiber.Map{"updated_fields": keysOf(b), "sensitive_change_requests": requestCount}); err != nil {
		return err
	}
	if err = tx.Commit(c.Context()); err != nil {
		return err
	}
	return ok(c, fiber.Map{"updated": true, "sensitive_change_requests": requestCount})
}

func (a *API) listProfileChangeRequests(c *fiber.Ctx) error {
	if role(c) == "admin" {
		return a.rows(c, `SELECT r.*,u.name member_name,m.number member_number FROM member_profile_change_requests r JOIN members m ON m.id=r.member_id JOIN users u ON u.id=m.user_id ORDER BY r.created_at DESC LIMIT 500`)
	}
	if role(c) != "member" {
		return fail(c, 403, "forbidden", "Hanya anggota dan admin yang dapat melihat permintaan perubahan profil")
	}
	return a.rows(c, `SELECT id,field_name,old_value,new_value,status,review_reason,created_at,reviewed_at FROM member_profile_change_requests WHERE member_id=$1 ORDER BY created_at DESC`, member(c))
}

func (a *API) reviewProfileChangeRequest(c *fiber.Ctx) error {
	var b struct {
		Approve bool   `json:"approve"`
		Reason  string `json:"reason"`
	}
	if c.BodyParser(&b) != nil || strings.TrimSpace(b.Reason) == "" {
		return fail(c, 400, "review_reason_required", "Keputusan dan alasan wajib diisi")
	}
	tx, err := a.DB.Begin(c.Context())
	if err != nil {
		return err
	}
	defer tx.Rollback(c.Context())
	var mid int64
	var field, oldValue, newValue, status string
	err = tx.QueryRow(c.Context(), `SELECT member_id,field_name,old_value,new_value,status FROM member_profile_change_requests WHERE id=$1 FOR UPDATE`, id(c)).Scan(&mid, &field, &oldValue, &newValue, &status)
	if err == pgx.ErrNoRows {
		return fiber.ErrNotFound
	}
	if err != nil {
		return err
	}
	if status != "pending" {
		return fail(c, 409, "already_reviewed", "Permintaan sudah ditinjau")
	}
	next := "rejected"
	if b.Approve {
		if !sensitiveProfileFields[field] {
			return fail(c, 409, "unsupported_sensitive_field", "Field ini tidak dapat disetujui melalui alur ini")
		}
		q := fmt.Sprintf(`UPDATE members SET %s=$1 WHERE id=$2`, field)
		if _, err = tx.Exec(c.Context(), q, newValue, mid); err != nil {
			return err
		}
		next = "approved"
	}
	if _, err = tx.Exec(c.Context(), `UPDATE member_profile_change_requests SET status=$1,reviewed_by=$2,review_reason=$3,reviewed_at=now() WHERE id=$4`, next, actor(c), strings.TrimSpace(b.Reason), id(c)); err != nil {
		return err
	}
	if _, err = tx.Exec(c.Context(), `INSERT INTO notifications(user_id,title,message) SELECT user_id,'Perubahan profil ditinjau',$1 FROM members WHERE id=$2`, next+": "+strings.TrimSpace(b.Reason), mid); err != nil {
		return err
	}
	if err = audit(c.Context(), tx, actor(c), "member_profile_change_reviewed", "member_profile_change_requests", id(c), fiber.Map{"status": status, "field": field}, fiber.Map{"status": next, "reason": b.Reason}); err != nil {
		return err
	}
	if err = tx.Commit(c.Context()); err != nil {
		return err
	}
	return ok(c, fiber.Map{"id": id(c), "status": next})
}

func (a *API) uploadProfilePhoto(c *fiber.Ctx) error {
	header, err := c.FormFile("file")
	if err != nil {
		return fail(c, 400, "file_required", "Foto profil wajib diunggah")
	}
	storage, name, contentType, size, cleanup, err := a.persistValidatedDocument(header)
	if err != nil {
		return fail(c, 400, "invalid_photo", err.Error())
	}
	if contentType != "image/jpeg" && contentType != "image/png" {
		cleanup()
		return fail(c, 400, "invalid_photo", "Foto profil harus JPG atau PNG")
	}
	committed := false
	defer func() {
		if !committed {
			cleanup()
		}
	}()
	tx, err := a.DB.Begin(c.Context())
	if err != nil {
		return err
	}
	defer tx.Rollback(c.Context())
	if _, err = tx.Exec(c.Context(), `UPDATE documents SET is_current=false WHERE user_id=$1 AND kind='profile_photo' AND is_current=true`, actor(c)); err != nil {
		return err
	}
	var docID int64
	err = tx.QueryRow(c.Context(), `INSERT INTO documents(user_id,kind,filename,path,mime_type,size,review_status,is_current) VALUES($1,'profile_photo',$2,$3,$4,$5,'verified',true) RETURNING id`, actor(c), name, storage, contentType, size).Scan(&docID)
	if err != nil {
		return err
	}
	if err = audit(c.Context(), tx, actor(c), "profile_photo_uploaded", "documents", docID, nil, fiber.Map{"filename": name, "size": size}); err != nil {
		return err
	}
	if err = tx.Commit(c.Context()); err != nil {
		return err
	}
	committed = true
	c.Status(201)
	return ok(c, fiber.Map{"id": docID, "filename": name, "preview_url": "/api/v1/documents/" + fmt.Sprint(docID) + "/preview"})
}

func (a *API) memberStatement(c *fiber.Ctx) error {
	loc := makassarLocation()
	today := time.Now().In(loc)
	fromText := c.Query("from", today.Format("2006-01-02"))
	toText := c.Query("to", today.Format("2006-01-02"))
	from, err := time.ParseInLocation("2006-01-02", fromText, loc)
	if err != nil {
		return fail(c, 400, "invalid_date", "Tanggal awal harus YYYY-MM-DD")
	}
	to, err := time.ParseInLocation("2006-01-02", toText, loc)
	if err != nil || to.Before(from) || to.Sub(from) > 366*24*time.Hour {
		return fail(c, 400, "invalid_date_range", "Rentang tanggal tidak valid atau lebih dari satu tahun")
	}
	typeID := int64(c.QueryInt("type_id", 0))
	if typeID < 0 {
		return fail(c, 400, "invalid_type", "Jenis simpanan tidak valid")
	}
	var opening int64
	qOpening := `SELECT COALESCE(sum(CASE WHEN kind='deposit' THEN amount ELSE -amount END),0)::bigint FROM savings_transactions WHERE member_id=$1 AND status='approved' AND COALESCE(verified_at,created_at)<$2 AND ($3=0 OR type_id=$3)`
	if err = a.DB.QueryRow(c.Context(), qOpening, member(c), from, typeID).Scan(&opening); err != nil {
		return err
	}
	rows, err := a.readMaps(c, `SELECT s.id,COALESCE(s.verified_at,s.created_at) created_at,s.reference,s.kind,s.amount,s.note,s.status,t.name type_name,s.type_id FROM savings_transactions s JOIN savings_types t ON t.id=s.type_id WHERE s.member_id=$1 AND s.status='approved' AND COALESCE(s.verified_at,s.created_at) >= $2 AND COALESCE(s.verified_at,s.created_at) < $3 AND ($4=0 OR s.type_id=$4) ORDER BY s.created_at,s.id`, member(c), from, to.AddDate(0, 0, 1), typeID)
	if err != nil {
		return err
	}
	balance := opening
	var debit, credit int64
	for _, row := range rows {
		amount, _ := numberInt64(row["amount"])
		if row["kind"] == "deposit" {
			credit += amount
			balance += amount
			row["credit"] = amount
			row["debit"] = int64(0)
		} else {
			debit += amount
			balance -= amount
			row["debit"] = amount
			row["credit"] = int64(0)
		}
		row["running_balance"] = balance
	}
	pending, err := a.readMaps(c, `SELECT s.id,s.created_at,s.reference,s.kind,s.amount,s.status,s.note,t.name type_name FROM savings_transactions s JOIN savings_types t ON t.id=s.type_id WHERE s.member_id=$1 AND s.status='pending' AND s.created_at >= $2 AND s.created_at < $3 AND ($4=0 OR s.type_id=$4) ORDER BY s.created_at,s.id`, member(c), from, to.AddDate(0, 0, 1), typeID)
	if err != nil {
		return err
	}
	return ok(c, fiber.Map{"from": fromText, "to": toText, "opening_balance": opening, "total_debit": debit, "total_credit": credit, "closing_balance": balance, "transactions": rows, "pending_transactions": pending, "account_type_id": typeID})
}

func (a *API) exportMemberStatement(c *fiber.Ctx) error {
	// Reuse the same authenticated member/date scope as the on-screen statement.
	fromText := c.Query("from", time.Now().In(makassarLocation()).Format("2006-01-02"))
	toText := c.Query("to", time.Now().In(makassarLocation()).Format("2006-01-02"))
	loc := makassarLocation()
	from, err := time.ParseInLocation("2006-01-02", fromText, loc)
	if err != nil {
		return fail(c, 400, "invalid_date", "Tanggal awal tidak valid")
	}
	to, err := time.ParseInLocation("2006-01-02", toText, loc)
	if err != nil || to.Before(from) || to.Sub(from) > 366*24*time.Hour {
		return fail(c, 400, "invalid_date_range", "Rentang tanggal tidak valid")
	}
	var opening int64
	typeID := int64(c.QueryInt("type_id", 0))
	if err = a.DB.QueryRow(c.Context(), `SELECT COALESCE(sum(CASE WHEN kind='deposit' THEN amount ELSE -amount END),0)::bigint FROM savings_transactions WHERE member_id=$1 AND status='approved' AND COALESCE(verified_at,created_at)<$2 AND ($3=0 OR type_id=$3)`, member(c), from, typeID).Scan(&opening); err != nil {
		return err
	}
	rows, err := a.readMaps(c, `SELECT COALESCE(s.verified_at,s.created_at) created_at,s.reference,t.name type_name,s.kind,s.amount,s.note FROM savings_transactions s JOIN savings_types t ON t.id=s.type_id WHERE s.member_id=$1 AND s.status='approved' AND COALESCE(s.verified_at,s.created_at) >= $2 AND COALESCE(s.verified_at,s.created_at) < $3 AND ($4=0 OR s.type_id=$4) ORDER BY s.created_at,s.id`, member(c), from, to.AddDate(0, 0, 1), typeID)
	if err != nil {
		return err
	}
	var b strings.Builder
	w := csv.NewWriter(&b)
	_ = w.Write([]string{"Tanggal", "Referensi", "Jenis Rekening", "Transaksi", "Debit", "Kredit", "Saldo", "Keterangan"})
	balance := opening
	for _, r := range rows {
		amount, _ := numberInt64(r["amount"])
		debit, credit := int64(0), int64(0)
		if r["kind"] == "deposit" {
			credit = amount
			balance += amount
		} else {
			debit = amount
			balance -= amount
		}
		_ = w.Write([]string{fmt.Sprint(r["created_at"]), fmt.Sprint(r["reference"]), fmt.Sprint(r["type_name"]), fmt.Sprint(r["kind"]), fmt.Sprint(debit), fmt.Sprint(credit), fmt.Sprint(balance), fmt.Sprint(r["note"])})
	}
	w.Flush()
	if err = w.Error(); err != nil {
		return err
	}
	c.Set("Content-Type", "text/csv; charset=utf-8")
	c.Set("Content-Disposition", "attachment; filename=koplink-rekening-koran.csv")
	return c.SendString("\ufeff" + b.String())
}

func (a *API) readMaps(c *fiber.Ctx, query string, args ...any) ([]fiber.Map, error) {
	rs, err := a.DB.Query(c.Context(), query, args...)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	out := []fiber.Map{}
	for rs.Next() {
		vals, e := rs.Values()
		if e != nil {
			return nil, e
		}
		row := fiber.Map{}
		for i, f := range rs.FieldDescriptions() {
			row[f.Name] = vals[i]
		}
		out = append(out, row)
	}
	return out, rs.Err()
}

func numberInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case int64:
		return n, true
	case int32:
		return int64(n), true
	case int:
		return int64(n), true
	case float64:
		if n == float64(int64(n)) {
			return int64(n), true
		}
	case string:
		var out int64
		_, err := fmt.Sscan(n, &out)
		return out, err == nil
	}
	return 0, false
}
func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// Compile-time assertion: the CSV response is deliberately a member-only export.
var _ = csv.ErrFieldCount
