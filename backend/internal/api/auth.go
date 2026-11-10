package api

import (
	"errors"
	"mime/multipart"
	"net/mail"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gofiber/fiber/v2"
	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"golang.org/x/crypto/bcrypt"
)

func (a *API) login(c *fiber.Ctx) error {
	var b struct{ Email, Password string }
	if c.BodyParser(&b) != nil {
		return fiber.ErrBadRequest
	}
	var uid int64
	var hash string
	identity := strings.TrimSpace(b.Email)
	if a.DB.QueryRow(c.Context(), `SELECT u.id,u.password_hash FROM users u LEFT JOIN members m ON m.user_id=u.id WHERE (LOWER(u.email)=LOWER($1) OR LOWER(COALESCE(u.username,''))=LOWER($1) OR UPPER(COALESCE(m.number,''))=UPPER($1)) AND u.active LIMIT 1`, identity).Scan(&uid, &hash) != nil || bcrypt.CompareHashAndPassword([]byte(hash), []byte(b.Password)) != nil {
		return fail(c, 401, "invalid_credentials", "Email/NPA atau kata sandi salah")
	}
	sid := randomID()
	exp := time.Now().Add(8 * time.Hour)
	_, e := a.DB.Exec(c.Context(), "INSERT INTO sessions(id,user_id,expires_at) VALUES($1,$2,$3)", sid, uid, exp)
	if e != nil {
		return e
	}
	token, e := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"sub": uid, "jti": sid, "exp": exp.Unix(), "iat": time.Now().Unix()}).SignedString(a.Secret)
	if e != nil {
		return e
	}
	u, e := a.user(c, uid)
	if e != nil {
		return e
	}
	return ok(c, fiber.Map{"token": token, "user": u})
}
func (a *API) user(c *fiber.Ctx, uid int64) (fiber.Map, error) {
	var name, email, r, username string
	var mid *int64
	var memberStatus, memberNumber *string
	e := a.DB.QueryRow(c.Context(), `SELECT u.name,u.email,u.role,COALESCE(u.username,''),m.id,m.status,m.number FROM users u LEFT JOIN members m ON m.user_id=u.id WHERE u.id=$1`, uid).Scan(&name, &email, &r, &username, &mid, &memberStatus, &memberNumber)
	return fiber.Map{"id": uid, "name": name, "email": email, "username": username, "role": r, "member_id": mid, "member_status": memberStatus, "member_number": memberNumber}, e
}
func (a *API) authenticate(c *fiber.Ctx) error {
	h := c.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") {
		return fiber.ErrUnauthorized
	}
	t, e := jwt.Parse(strings.TrimPrefix(h, "Bearer "), func(t *jwt.Token) (any, error) { return a.Secret, nil }, jwt.WithValidMethods([]string{"HS256"}), jwt.WithExpirationRequired())
	if e != nil || !t.Valid {
		return fiber.ErrUnauthorized
	}
	cl := t.Claims.(jwt.MapClaims)
	sid, _ := cl["jti"].(string)
	var uid int64
	var mid *int64
	var r string
	e = a.DB.QueryRow(c.Context(), `SELECT u.id,u.role,m.id FROM sessions s JOIN users u ON u.id=s.user_id LEFT JOIN members m ON m.user_id=u.id WHERE s.id=$1 AND NOT s.revoked AND s.expires_at>now() AND u.active`, sid).Scan(&uid, &r, &mid)
	if e != nil {
		return fiber.ErrUnauthorized
	}
	c.Locals("user_id", uid)
	c.Locals("role", r)
	c.Locals("session_id", sid)
	if mid != nil {
		c.Locals("member_id", *mid)
	}
	return c.Next()
}
func validPassword(s string) bool { return utf8.RuneCountInString(s) >= 10 && len(s) <= 72 }
func validUsername(s string) bool {
	if len(s) < 4 || len(s) > 32 {
		return false
	}
	for _, r := range s {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

func (a *API) register(c *fiber.Ctx) error {
	var b struct {
		Name              string `json:"name"`
		Email             string `json:"email"`
		Username          string `json:"username"`
		Password          string `json:"password"`
		NIK               string `json:"nik"`
		PlaceOfBirth      string `json:"place_of_birth"`
		BirthDate         string `json:"birth_date"`
		Gender            string `json:"gender"`
		Address           string `json:"address"`
		CityRegency       string `json:"city_regency"`
		PostalCode        string `json:"postal_code"`
		Phone             string `json:"phone"`
		Occupation        string `json:"occupation"`
		MonthlyIncome     int64  `json:"monthly_income"`
		CoopUnit          string `json:"coop_unit"`
		ReferralCode      string `json:"referral_code"`
		MembershipPurpose string `json:"membership_purpose"`
		AdditionalNotes   string `json:"additional_notes"`
		TransactionPIN    string `json:"transaction_pin"`
		ConsentMembership bool   `json:"consent_membership"`
		ConsentFinal      bool   `json:"consent_final"`
	}
	var identityHeader, photoHeader, supportingHeader *multipart.FileHeader
	if strings.HasPrefix(strings.ToLower(c.Get("Content-Type")), "multipart/form-data") {
		b.Name = c.FormValue("name")
		b.Email = c.FormValue("email")
		b.Username = c.FormValue("username")
		b.Password = c.FormValue("password")
		b.NIK = c.FormValue("nik")
		b.PlaceOfBirth = c.FormValue("place_of_birth")
		b.BirthDate = c.FormValue("birth_date")
		b.Gender = c.FormValue("gender")
		b.Address = c.FormValue("address")
		b.CityRegency = c.FormValue("city_regency")
		b.PostalCode = c.FormValue("postal_code")
		b.Phone = c.FormValue("phone")
		b.Occupation = c.FormValue("occupation")
		income, parseErr := strconv.ParseInt(strings.TrimSpace(c.FormValue("monthly_income")), 10, 64)
		if parseErr != nil {
			return fail(c, 400, "invalid_income", "Penghasilan bulanan harus berupa angka nol atau lebih")
		}
		b.MonthlyIncome = income
		b.CoopUnit = c.FormValue("coop_unit")
		b.ReferralCode = c.FormValue("referral_code")
		b.MembershipPurpose = c.FormValue("membership_purpose")
		b.AdditionalNotes = c.FormValue("additional_notes")
		b.TransactionPIN = c.FormValue("transaction_pin")
		b.ConsentMembership = c.FormValue("consent_membership") == "true"
		b.ConsentFinal = c.FormValue("consent_final") == "true"
		identityHeader, _ = c.FormFile("identity_document")
		photoHeader, _ = c.FormFile("photo_document")
		supportingHeader, _ = c.FormFile("supporting_document")
	} else if c.BodyParser(&b) != nil {
		return fiber.ErrBadRequest
	}

	b.Name = strings.TrimSpace(b.Name)
	b.Email = strings.ToLower(strings.TrimSpace(b.Email))
	b.Username = strings.ToLower(strings.TrimSpace(b.Username))
	b.NIK = strings.TrimSpace(b.NIK)
	b.PlaceOfBirth = strings.TrimSpace(b.PlaceOfBirth)
	b.Gender = strings.TrimSpace(b.Gender)
	b.Address = strings.TrimSpace(b.Address)
	b.CityRegency = strings.TrimSpace(b.CityRegency)
	b.PostalCode = strings.TrimSpace(b.PostalCode)
	b.Phone = strings.TrimSpace(b.Phone)
	b.Occupation = strings.TrimSpace(b.Occupation)
	b.CoopUnit = strings.TrimSpace(b.CoopUnit)
	b.ReferralCode = strings.TrimSpace(b.ReferralCode)
	b.MembershipPurpose = strings.TrimSpace(b.MembershipPurpose)
	b.AdditionalNotes = strings.TrimSpace(b.AdditionalNotes)
	b.TransactionPIN = strings.TrimSpace(b.TransactionPIN)
	addr, emailErr := mail.ParseAddress(b.Email)
	if b.Name == "" || len(b.Name) > 120 || emailErr != nil || addr.Address != b.Email || !validUsername(b.Username) || !validPassword(b.Password) || len(b.NIK) != 16 || b.PlaceOfBirth == "" || len(b.PlaceOfBirth) > 120 || b.Address == "" || len(b.Address) > 500 || b.CityRegency == "" || len(b.CityRegency) > 120 || len(b.PostalCode) != 5 || b.Phone == "" || len(b.Phone) > 32 || b.Occupation == "" || len(b.Occupation) > 120 || b.MonthlyIncome < 0 || b.CoopUnit == "" || len(b.CoopUnit) > 120 || b.MembershipPurpose == "" || len(b.MembershipPurpose) > 300 || len(b.AdditionalNotes) > 1000 || (b.Gender != "male" && b.Gender != "female") || len(b.TransactionPIN) != 6 || !b.ConsentMembership || !b.ConsentFinal {
		return fail(c, 400, "invalid_input", "Lengkapi identitas, kontak, informasi keanggotaan, persetujuan, username, kata sandi, dan PIN transaksi dengan format yang benar")
	}
	for _, r := range b.NIK {
		if r < '0' || r > '9' {
			return fail(c, 400, "invalid_input", "NIK harus terdiri dari 16 digit angka")
		}
	}
	for _, r := range b.PostalCode {
		if r < '0' || r > '9' {
			return fail(c, 400, "invalid_input", "Kode pos harus terdiri dari 5 digit angka")
		}
	}
	for _, r := range b.TransactionPIN {
		if r < '0' || r > '9' {
			return fail(c, 400, "invalid_pin", "PIN transaksi harus terdiri dari 6 digit angka")
		}
	}
	if _, e := time.Parse("2006-01-02", b.BirthDate); e != nil {
		return fail(c, 400, "invalid_birth_date", "Tanggal lahir wajib diisi dengan format yang benar")
	}
	if identityHeader == nil || photoHeader == nil {
		return fail(c, 400, "required_documents", "KTP/identitas resmi dan pasfoto wajib diunggah")
	}
	var exists bool
	if e := a.DB.QueryRow(c.Context(), `SELECT EXISTS(SELECT 1 FROM users WHERE LOWER(email)=LOWER($1)) OR EXISTS(SELECT 1 FROM users WHERE LOWER(username)=LOWER($2)) OR EXISTS(SELECT 1 FROM members WHERE nik=$3)`, b.Email, b.Username, b.NIK).Scan(&exists); e != nil {
		return e
	}
	if exists {
		return fail(c, 409, "registration_conflict", "Email, username, atau NIK sudah terdaftar")
	}
	passwordHash, e := bcrypt.GenerateFromPassword([]byte(b.Password), 12)
	if e != nil {
		return e
	}
	pinHash, e := bcrypt.GenerateFromPassword([]byte(b.TransactionPIN), 12)
	if e != nil {
		return e
	}
	type storedDocument struct {
		kind, storageName, filename, contentType string
		size                                     int64
		cleanup                                  func()
	}
	stored := make([]storedDocument, 0, 3)
	cleanupStored := func() {
		for _, doc := range stored {
			if doc.cleanup != nil {
				doc.cleanup()
			}
		}
	}
	for _, item := range []struct {
		kind     string
		header   *multipart.FileHeader
		optional bool
	}{
		{"identity", identityHeader, false}, {"profile_photo", photoHeader, false}, {"supporting", supportingHeader, true},
	} {
		if item.header == nil {
			if item.optional {
				continue
			}
			cleanupStored()
			return fail(c, 400, "required_documents", "KTP/identitas resmi dan pasfoto wajib diunggah")
		}
		storage, filename, contentType, size, cleanup, err := a.persistValidatedDocument(item.header)
		if err != nil {
			cleanupStored()
			var invalid invalidDocumentError
			if errors.As(err, &invalid) {
				return fail(c, 400, "invalid_document", invalid.Error())
			}
			return err
		}
		if item.kind == "profile_photo" && contentType != "image/jpeg" && contentType != "image/png" {
			cleanup()
			cleanupStored()
			return fail(c, 400, "invalid_photo", "Pasfoto harus berupa gambar JPG atau PNG")
		}
		stored = append(stored, storedDocument{kind: item.kind, storageName: storage, filename: filename, contentType: contentType, size: size, cleanup: cleanup})
	}
	committed := false
	defer func() {
		if !committed {
			cleanupStored()
		}
	}()
	tx, e := a.DB.Begin(c.Context())
	if e != nil {
		return e
	}
	defer tx.Rollback(c.Context())
	var uid, mid int64
	e = tx.QueryRow(c.Context(), `INSERT INTO users(name,email,username,password_hash,role) VALUES($1,$2,$3,$4,'member') RETURNING id`, b.Name, b.Email, b.Username, string(passwordHash)).Scan(&uid)
	if e != nil {
		var pgErr *pgconn.PgError
		if errors.As(e, &pgErr) && pgErr.Code == "23505" {
			return fail(c, 409, "registration_conflict", "Email, username, atau NIK sudah terdaftar")
		}
		return e
	}
	e = tx.QueryRow(c.Context(), `INSERT INTO members(user_id,number,nik,address,phone,occupation,birth_date,birth_place,gender,city_regency,postal_code,monthly_income,coop_unit,referral_code,membership_purpose,additional_notes,transaction_pin_hash) VALUES($1,NULL,$2,$3,$4,$5,$6::date,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16) RETURNING id`, uid, b.NIK, b.Address, b.Phone, b.Occupation, b.BirthDate, b.PlaceOfBirth, b.Gender, b.CityRegency, b.PostalCode, b.MonthlyIncome, b.CoopUnit, b.ReferralCode, b.MembershipPurpose, b.AdditionalNotes, string(pinHash)).Scan(&mid)
	if e != nil {
		return e
	}
	documentIDs := make([]int64, 0, len(stored))
	for _, doc := range stored {
		var docID int64
		e = tx.QueryRow(c.Context(), `INSERT INTO documents(user_id,kind,filename,path,mime_type,size) VALUES($1,$2,$3,$4,$5,$6) RETURNING id`, uid, doc.kind, doc.filename, doc.storageName, doc.contentType, doc.size).Scan(&docID)
		if e != nil {
			return e
		}
		documentIDs = append(documentIDs, docID)
	}
	if e = audit(c.Context(), tx, uid, "register", "members", mid, nil, fiber.Map{"status": "pending", "username": b.Username, "document_ids": documentIDs}); e != nil {
		return e
	}
	if e = tx.Commit(c.Context()); e != nil {
		return e
	}
	committed = true
	return ok(c, fiber.Map{"id": uid, "member_id": mid, "username": b.Username, "document_ids": documentIDs, "status": "pending"})
}
func (a *API) createStaffUser(c *fiber.Ctx) error {
	var b struct {
		Name     string `json:"name"`
		Email    string `json:"email"`
		Password string `json:"password"`
		Role     string `json:"role"`
	}
	if c.BodyParser(&b) != nil {
		return fiber.ErrBadRequest
	}
	b.Name = strings.TrimSpace(b.Name)
	b.Email = strings.ToLower(strings.TrimSpace(b.Email))
	addr, emailErr := mail.ParseAddress(b.Email)
	if b.Name == "" || len(b.Name) > 120 || emailErr != nil || addr.Address != b.Email || !validPassword(b.Password) || !contains([]string{"admin", "teller", "committee"}, b.Role) {
		return fail(c, 400, "invalid_input", "Nama, email valid, peran admin/teller/committee, dan sandi 10–72 karakter wajib")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(b.Password), 12)
	if err != nil {
		return err
	}
	tx, err := a.DB.Begin(c.Context())
	if err != nil {
		return err
	}
	defer tx.Rollback(c.Context())
	var uid int64
	if err = tx.QueryRow(c.Context(), `INSERT INTO users(name,email,password_hash,role) VALUES($1,$2,$3,$4) RETURNING id`, b.Name, b.Email, string(hash), b.Role).Scan(&uid); err != nil {
		return err
	}
	if err = audit(c.Context(), tx, actor(c), "staff_user_created", "users", uid, nil, fiber.Map{"name": b.Name, "email": b.Email, "role": b.Role}); err != nil {
		return err
	}
	if err = tx.Commit(c.Context()); err != nil {
		return err
	}
	return ok(c, fiber.Map{"id": uid, "name": b.Name, "email": b.Email, "role": b.Role, "active": true})
}

func (a *API) resetUserPassword(c *fiber.Ctx) error {
	if id(c) == actor(c) {
		return fail(c, 400, "self_reset", "Gunakan menu Profil untuk mengganti kata sandi akun sendiri")
	}
	var b struct {
		NewPassword string `json:"new_password"`
	}
	if c.BodyParser(&b) != nil || !validPassword(b.NewPassword) {
		return fail(c, 400, "invalid_password", "Kata sandi baru harus 10–72 karakter UTF-8")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(b.NewPassword), 12)
	if err != nil {
		return err
	}
	tx, err := a.DB.Begin(c.Context())
	if err != nil {
		return err
	}
	defer tx.Rollback(c.Context())
	var uid int64
	if err = tx.QueryRow(c.Context(), "SELECT id FROM users WHERE id=$1 FOR UPDATE", id(c)).Scan(&uid); err != nil {
		return fiber.ErrNotFound
	}
	if _, err = tx.Exec(c.Context(), "UPDATE users SET password_hash=$1 WHERE id=$2", string(hash), uid); err != nil {
		return err
	}
	if _, err = tx.Exec(c.Context(), "UPDATE sessions SET revoked=true WHERE user_id=$1", uid); err != nil {
		return err
	}
	if err = audit(c.Context(), tx, actor(c), "user_password_reset", "users", uid, nil, fiber.Map{"sessions_revoked": true}); err != nil {
		return err
	}
	if err = tx.Commit(c.Context()); err != nil {
		return err
	}
	return ok(c, fiber.Map{"password_reset": true, "sessions_revoked": true})
}

func (a *API) authRoutes(r fiber.Router) {
	r.Get("/auth/me", func(c *fiber.Ctx) error {
		u, e := a.user(c, actor(c))
		if e != nil {
			return e
		}
		return ok(c, u)
	})
	r.Post("/auth/logout", func(c *fiber.Ctx) error {
		_, e := a.DB.Exec(c.Context(), "UPDATE sessions SET revoked=true WHERE id=$1", c.Locals("session_id"))
		if e != nil {
			return e
		}
		return ok(c, fiber.Map{"logged_out": true})
	})
	r.Patch("/auth/password", a.password)
	r.Patch("/auth/profile", a.profile)
	r.Get("/roles", require("admin"), func(c *fiber.Ctx) error { return ok(c, []string{"admin", "teller", "committee", "member"}) })
	r.Get("/members", func(c *fiber.Ctx) error {
		switch role(c) {
		case "admin":
			return a.rows(c, `SELECT m.id,m.number,m.phone,m.occupation,m.status,m.reason,m.created_at,u.name,u.email,CASE WHEN length(m.nik)>=4 THEN repeat('•',length(m.nik)-4)||right(m.nik,4) ELSE '••••' END nik_masked FROM members m JOIN users u ON u.id=m.user_id ORDER BY m.id`)
		case "member":
			return a.rows(c, `SELECT m.id,m.user_id,m.number,m.address,m.phone,m.occupation,m.birth_date,m.status,m.reason,m.created_at,m.birth_place,m.gender,m.city_regency,m.postal_code,m.monthly_income,m.coop_unit,m.membership_purpose,m.additional_notes,u.name,u.email FROM members m JOIN users u ON u.id=m.user_id WHERE m.id=$1`, member(c))
		case "teller":
			return a.rows(c, `SELECT DISTINCT m.id,m.number,m.phone,m.status,u.name FROM members m JOIN users u ON u.id=m.user_id WHERE EXISTS(SELECT 1 FROM savings_transactions s WHERE s.member_id=m.id AND s.status='pending') OR EXISTS(SELECT 1 FROM loans l JOIN loan_applications x ON x.id=l.application_id WHERE l.member_id=m.id AND x.status='approved') OR EXISTS(SELECT 1 FROM payments p JOIN installments i ON i.id=p.installment_id JOIN loans l ON l.id=i.loan_id WHERE l.member_id=m.id AND p.status='pending') ORDER BY m.id`)
		case "committee":
			return a.rows(c, `SELECT DISTINCT m.id,m.number,u.name,x.id application_id,x.status application_status FROM loan_application_assignments a JOIN loan_applications x ON x.id=a.application_id JOIN members m ON m.id=x.member_id JOIN users u ON u.id=m.user_id WHERE a.committee_user_id=$1 AND a.unassigned_at IS NULL ORDER BY m.id`, actor(c))
		default:
			return fiber.ErrForbidden
		}
	})
	r.Get("/members/:id/profile", require("admin"), func(c *fiber.Ctx) error {
		rows, err := a.readMaps(c, `SELECT m.id,m.number,m.nik,m.npwp,m.address,m.domicile_address,m.phone,m.occupation,m.birth_place,m.birth_date,m.gender,m.marital_status,m.city_regency,m.postal_code,m.monthly_income,m.coop_unit,m.bank_name,m.bank_account_number,m.bank_account_holder,m.emergency_contact_name,m.emergency_contact_phone,m.emergency_contact_relationship,m.heir_name,m.heir_phone,m.membership_purpose,m.additional_notes,m.status,m.reason,u.name,u.email FROM members m JOIN users u ON u.id=m.user_id WHERE m.id=$1`, id(c))
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return fiber.ErrNotFound
		}
		tx, err := a.DB.Begin(c.Context())
		if err != nil {
			return err
		}
		defer tx.Rollback(c.Context())
		if err = audit(c.Context(), tx, actor(c), "member_profile_viewed", "members", id(c), nil, fiber.Map{"scope": "admin_profile_detail"}); err != nil {
			return err
		}
		if err = tx.Commit(c.Context()); err != nil {
			return err
		}
		return ok(c, rows[0])
	})
	r.Patch("/members/:id", require("admin"), a.updateMember)
	r.Get("/users", require("admin"), func(c *fiber.Ctx) error {
		return a.rows(c, "SELECT id,name,email,username,role,active,created_at FROM users ORDER BY id")
	})
	r.Post("/users", require("admin"), a.createStaffUser)
	r.Patch("/users/:id", require("admin"), a.updateUser)
	r.Patch("/users/:id/password", require("admin"), a.resetUserPassword)
}
func (a *API) password(c *fiber.Ctx) error {
	var b struct {
		Old string `json:"old_password"`
		New string `json:"new_password"`
	}
	if c.BodyParser(&b) != nil || !validPassword(b.New) {
		return fiber.ErrBadRequest
	}
	var hash string
	if a.DB.QueryRow(c.Context(), "SELECT password_hash FROM users WHERE id=$1", actor(c)).Scan(&hash) != nil || bcrypt.CompareHashAndPassword([]byte(hash), []byte(b.Old)) != nil {
		return fiber.ErrUnauthorized
	}
	h, _ := bcrypt.GenerateFromPassword([]byte(b.New), 12)
	tx, e := a.DB.Begin(c.Context())
	if e != nil {
		return e
	}
	defer tx.Rollback(c.Context())
	if _, e = tx.Exec(c.Context(), "UPDATE users SET password_hash=$1 WHERE id=$2", string(h), actor(c)); e != nil {
		return e
	}
	if _, e = tx.Exec(c.Context(), "UPDATE sessions SET revoked=true WHERE user_id=$1", actor(c)); e != nil {
		return e
	}
	if e = audit(c.Context(), tx, actor(c), "password_changed", "users", actor(c), nil, nil); e != nil {
		return e
	}
	if e = tx.Commit(c.Context()); e != nil {
		return e
	}
	return ok(c, fiber.Map{"reauthenticate": true})
}
func (a *API) profile(c *fiber.Ctx) error {
	var b struct {
		Name       string `json:"name"`
		Phone      string `json:"phone"`
		Address    string `json:"address"`
		Occupation string `json:"occupation"`
	}
	if c.BodyParser(&b) != nil {
		return fiber.ErrBadRequest
	}
	b.Name = strings.TrimSpace(b.Name)
	if b.Name == "" || len(b.Name) > 120 || len(b.Address) > 500 || len(b.Phone) > 32 || len(b.Occupation) > 120 {
		return fail(c, 400, "invalid_profile", "Nama wajib diisi; panjang data profil tidak valid")
	}
	tx, e := a.DB.Begin(c.Context())
	if e != nil {
		return e
	}
	defer tx.Rollback(c.Context())
	if _, e = tx.Exec(c.Context(), "UPDATE users SET name=$1 WHERE id=$2", b.Name, actor(c)); e != nil {
		return e
	}
	// Staff accounts are not member records. Never try to edit member columns
	// for admins, tellers, or committee users.
	if role(c) == "member" {
		if _, e = tx.Exec(c.Context(), "UPDATE members SET phone=$1,address=$2,occupation=$3 WHERE user_id=$4", strings.TrimSpace(b.Phone), strings.TrimSpace(b.Address), strings.TrimSpace(b.Occupation), actor(c)); e != nil {
			return e
		}
	}
	if e = audit(c.Context(), tx, actor(c), "profile_updated", "users", actor(c), nil, fiber.Map{"name": b.Name, "member_fields_updated": role(c) == "member"}); e != nil {
		return e
	}
	if e = tx.Commit(c.Context()); e != nil {
		return e
	}
	u, e := a.user(c, actor(c))
	if e != nil {
		return e
	}
	return ok(c, u)
}

func (a *API) updateMember(c *fiber.Ctx) error {
	var b struct{ Status, Reason string }
	if c.BodyParser(&b) != nil || !contains([]string{"active", "pending", "rejected", "inactive"}, b.Status) {
		return fiber.ErrBadRequest
	}
	tx, e := a.DB.Begin(c.Context())
	if e != nil {
		return e
	}
	defer tx.Rollback(c.Context())
	var old string
	e = tx.QueryRow(c.Context(), "SELECT status FROM members WHERE id=$1 FOR UPDATE", id(c)).Scan(&old)
	if e != nil {
		return fiber.ErrNotFound
	}
	if b.Status == "active" {
		var complete bool
		if e = tx.QueryRow(c.Context(), `SELECT address<>'' AND phone<>'' AND EXISTS(SELECT 1 FROM documents d WHERE d.user_id=m.user_id AND d.kind='identity') AND EXISTS(SELECT 1 FROM documents d WHERE d.user_id=m.user_id AND d.kind='profile_photo') FROM members m WHERE m.id=$1`, id(c)).Scan(&complete); e != nil {
			return e
		}
		if !complete {
			return fail(c, 409, "kyc_incomplete", "Alamat, telepon, dokumen identitas, dan pasfoto wajib sebelum aktivasi")
		}
	}
	_, e = tx.Exec(c.Context(), "UPDATE members SET status=$1,reason=$2,number=CASE WHEN $1='active' THEN COALESCE(number,'KOP-'||lpad(id::text,6,'0')) ELSE number END WHERE id=$3", b.Status, b.Reason, id(c))
	if e != nil {
		return e
	}
	if e = audit(c.Context(), tx, actor(c), "status_changed", "members", id(c), old, b); e != nil {
		return e
	}
	_, e = tx.Exec(c.Context(), "INSERT INTO notifications(user_id,title,message) SELECT user_id,'Status anggota',$1 FROM members WHERE id=$2", b.Status+": "+b.Reason, id(c))
	if e != nil {
		return e
	}
	if e = tx.Commit(c.Context()); e != nil {
		return e
	}
	return ok(c, b)
}
func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
func (a *API) updateUser(c *fiber.Ctx) error {
	var b struct {
		Role   string
		Active *bool
	}
	if c.BodyParser(&b) != nil || b.Role != "" && !contains([]string{"admin", "committee", "teller"}, b.Role) {
		return fiber.ErrBadRequest
	}
	if id(c) == actor(c) {
		return fail(c, 400, "self_change", "Tidak dapat mengubah akses diri sendiri")
	}
	tx, e := a.DB.Begin(c.Context())
	if e != nil {
		return e
	}
	defer tx.Rollback(c.Context())
	var targetRole string
	var hasMemberRecord bool
	if e = tx.QueryRow(c.Context(), `SELECT u.role,EXISTS(SELECT 1 FROM members m WHERE m.user_id=u.id) FROM users u WHERE u.id=$1 FOR UPDATE`, id(c)).Scan(&targetRole, &hasMemberRecord); e != nil {
		return fiber.ErrNotFound
	}
	if b.Role != "" && (targetRole == "member" || hasMemberRecord) {
		return fail(c, 409, "member_role_locked", "Peran akun anggota tidak dapat diubah dari menu ini; buat akun staf terpisah")
	}
	res, e := tx.Exec(c.Context(), `UPDATE users SET role=COALESCE(NULLIF($1,''),role),active=COALESCE($2,active) WHERE id=$3`, b.Role, b.Active, id(c))
	if e != nil {
		return e
	}
	if res.RowsAffected() == 0 {
		return fiber.ErrNotFound
	}
	if _, e = tx.Exec(c.Context(), "UPDATE sessions SET revoked=true WHERE user_id=$1", id(c)); e != nil {
		return e
	}
	if e = audit(c.Context(), tx, actor(c), "access_changed", "users", id(c), nil, b); e != nil {
		return e
	}
	if e = tx.Commit(c.Context()); e != nil {
		return e
	}
	return ok(c, b)
}
