package api

import (
	"fmt"
	"github.com/gofiber/fiber/v2"
	"github.com/jackc/pgx/v5"
	"koplink/internal/finance"
	"strings"
	"time"
)

func (a *API) loanRoutes(r fiber.Router) {
	r.Get("/loan-products", func(c *fiber.Ctx) error {
		if role(c) == "admin" {
			return a.rows(c, `SELECT id,name,min_amount,max_amount,min_tenor,max_tenor,annual_rate_bps,COALESCE(interest_method_v2,interest_method) interest_method,penalty_daily_amount,penalty_daily_bps,configured,active,max_ltv_bps FROM loan_products ORDER BY id`)
		}
		if role(c) == "committee" {
			return fail(c, 403, "forbidden", "Komite tidak mengelola produk pinjaman")
		}
		return a.rows(c, "SELECT id,name,min_amount,max_amount,min_tenor,max_tenor,annual_rate_bps,COALESCE(interest_method_v2,interest_method) interest_method,configured,active FROM loan_products WHERE active=true AND configured=true ORDER BY id")
	})
	r.Post("/loan-products", require("admin"), a.saveProduct)
	r.Patch("/loan-products/:id", require("admin"), a.saveProduct)
	r.Get("/loan-applications", func(c *fiber.Ctx) error {
		switch role(c) {
		case "member":
			return a.rows(c, `SELECT x.*,u.name member_name,p.name product_name FROM loan_applications x JOIN members m ON m.id=x.member_id JOIN users u ON u.id=m.user_id JOIN loan_products p ON p.id=x.product_id WHERE x.member_id=$1 ORDER BY x.id DESC`, member(c))
		case "committee":
			return a.rows(c, `SELECT x.*,u.name member_name,p.name product_name,a.assigned_at FROM loan_application_assignments a JOIN loan_applications x ON x.id=a.application_id JOIN members m ON m.id=x.member_id JOIN users u ON u.id=m.user_id JOIN loan_products p ON p.id=x.product_id WHERE a.committee_user_id=$1 AND a.unassigned_at IS NULL ORDER BY x.id DESC`, actor(c))
		case "teller":
			return a.rows(c, `SELECT x.id,x.member_id,u.name member_name,p.name product_name,x.amount,x.tenor,x.status,x.created_at,x.decision_reason FROM loan_applications x JOIN members m ON m.id=x.member_id JOIN users u ON u.id=m.user_id JOIN loan_products p ON p.id=x.product_id WHERE x.status='approved' ORDER BY x.id DESC`)
		case "admin":
			return a.rows(c, `SELECT x.*,u.name member_name,p.name product_name,ass.committee_user_id,cu.name committee_name FROM loan_applications x JOIN members m ON m.id=x.member_id JOIN users u ON u.id=m.user_id JOIN loan_products p ON p.id=x.product_id LEFT JOIN LATERAL (SELECT committee_user_id FROM loan_application_assignments WHERE application_id=x.id AND unassigned_at IS NULL ORDER BY assigned_at DESC LIMIT 1) ass ON true LEFT JOIN users cu ON cu.id=ass.committee_user_id ORDER BY x.id DESC`)
		default:
			return fiber.ErrForbidden
		}
	})
	r.Post("/loan-applications", require("member"), a.applyLoan)
	r.Post("/loan-applications/secured", require("member"), a.applySecuredLoan)
	r.Post("/loan-applications/:id/survey", require("committee", "admin"), a.surveyLoan)
	r.Post("/loan-applications/:id/decision", require("committee", "admin"), a.decideLoan)
	r.Post("/loan-applications/:id/disburse", require("teller", "admin"), a.disburse)
	r.Get("/loans", func(c *fiber.Ctx) error {
		switch role(c) {
		case "member":
			return a.rows(c, `SELECT l.id,l.application_id,l.amount,l.status,l.annual_rate_bps,l.interest_method,l.reference,l.disbursed_at,COALESCE((SELECT sum(principal+interest+penalty) FROM installments WHERE loan_id=l.id AND status='unpaid'),0)::bigint outstanding FROM loans l WHERE l.member_id=$1 ORDER BY l.id DESC`, member(c))
		case "admin":
			return a.rows(c, `SELECT l.*,u.name member_name,COALESCE((SELECT sum(principal+interest+penalty) FROM installments WHERE loan_id=l.id AND status='unpaid'),0)::bigint outstanding FROM loans l JOIN members m ON m.id=l.member_id JOIN users u ON u.id=m.user_id ORDER BY l.id DESC`)
		case "committee":
			return a.rows(c, `SELECT l.id,l.application_id,l.member_id,l.amount,l.status,l.annual_rate_bps,l.interest_method,l.disbursed_at,COALESCE((SELECT sum(principal+interest+penalty) FROM installments WHERE loan_id=l.id AND status='unpaid'),0)::bigint outstanding FROM loans l JOIN loan_application_assignments a ON a.application_id=l.application_id WHERE a.committee_user_id=$1 AND a.unassigned_at IS NULL ORDER BY l.id DESC`, actor(c))
		default:
			return fail(c, 403, "forbidden", "Teller menggunakan antrean pencairan, bukan portofolio pinjaman")
		}
	})
	r.Get("/installments", a.installments)
	r.Get("/penalties", require("admin", "committee"), a.installments)
	r.Get("/payments", func(c *fiber.Ctx) error {
		switch role(c) {
		case "member":
			return a.rows(c, `SELECT p.*,l.member_id FROM payments p JOIN installments i ON i.id=p.installment_id JOIN loans l ON l.id=i.loan_id WHERE l.member_id=$1 ORDER BY p.id DESC`, member(c))
		case "teller", "admin":
			return a.rows(c, `SELECT p.*,l.member_id,u.name member_name FROM payments p JOIN installments i ON i.id=p.installment_id JOIN loans l ON l.id=i.loan_id JOIN members m ON m.id=l.member_id JOIN users u ON u.id=m.user_id ORDER BY p.id DESC`)
		default:
			return fiber.ErrForbidden
		}
	})
	r.Post("/payments", require("member"), a.createPayment)
	r.Post("/payments/:id/verify", require("teller", "admin"), a.verifyPayment)
	r.Post("/scheduler/run", require("admin"), func(c *fiber.Ctx) error {
		n, e := a.runScheduler(actor(c))
		if e != nil {
			return e
		}
		return ok(c, fiber.Map{"updated": n})
	})
}

func (a *API) saveProduct(c *fiber.Ctx) error {
	var b struct {
		Name               string
		MinAmount          int64  `json:"min_amount"`
		MaxAmount          int64  `json:"max_amount"`
		MinTenor           int    `json:"min_tenor"`
		MaxTenor           int    `json:"max_tenor"`
		Rate               *int64 `json:"annual_rate_bps"`
		Method             string `json:"interest_method"`
		PenaltyAmount      *int64 `json:"penalty_daily_amount"`
		PenaltyBPS         *int64 `json:"penalty_daily_bps"`
		Configured, Active bool
		MaxLTVBPS          *int32 `json:"max_ltv_bps"`
	}
	if c.BodyParser(&b) != nil || b.Name == "" || b.MinAmount <= 0 || b.MaxAmount < b.MinAmount || b.MaxAmount > finance.MaxAmount || b.MinTenor < 1 || b.MaxTenor < b.MinTenor || b.MaxTenor > 120 {
		return fiber.ErrBadRequest
	}
	if b.Configured && (b.Rate == nil || *b.Rate < 0 || *b.Rate > finance.MaxAnnualRateBPS || !contains([]string{"flat", "declining", "annuity"}, b.Method) || b.PenaltyAmount == nil || b.PenaltyBPS == nil) {
		return fail(c, 400, "policy_missing", "Seluruh kebijakan bunga/denda wajib diatur")
	}
	if b.PenaltyAmount != nil && (*b.PenaltyAmount < 0 || *b.PenaltyAmount > 1000000000) || b.PenaltyBPS != nil && (*b.PenaltyBPS < 0 || *b.PenaltyBPS > 10000) || b.PenaltyAmount != nil && b.PenaltyBPS != nil && *b.PenaltyAmount > 0 && *b.PenaltyBPS > 0 || b.MaxLTVBPS != nil && (*b.MaxLTVBPS < 1 || *b.MaxLTVBPS > 10000) {
		return fiber.ErrBadRequest
	}
	tx, e := a.DB.Begin(c.Context())
	if e != nil {
		return e
	}
	defer tx.Rollback(c.Context())
	iid := id(c)
	args := []any{b.Name, b.MinAmount, b.MaxAmount, b.MinTenor, b.MaxTenor, b.Rate, b.Method, b.PenaltyAmount, b.PenaltyBPS, b.Configured, b.Active, b.MaxLTVBPS}
	if iid == 0 {
		e = tx.QueryRow(c.Context(), `INSERT INTO loan_products(name,min_amount,max_amount,min_tenor,max_tenor,annual_rate_bps,interest_method,interest_method_v2,penalty_daily_amount,penalty_daily_bps,configured,active,max_ltv_bps) VALUES($1,$2,$3,$4,$5,$6,CASE WHEN $7='annuity' THEN NULL ELSE NULLIF($7,'') END,NULLIF($7,''),$8,$9,$10,$11,$12) RETURNING id`, args...).Scan(&iid)
	} else {
		args = append(args, iid)
		_, e = tx.Exec(c.Context(), `UPDATE loan_products SET name=$1,min_amount=$2,max_amount=$3,min_tenor=$4,max_tenor=$5,annual_rate_bps=$6,interest_method=CASE WHEN $7='annuity' THEN NULL ELSE NULLIF($7,'') END,interest_method_v2=NULLIF($7,''),penalty_daily_amount=$8,penalty_daily_bps=$9,configured=$10,active=$11,max_ltv_bps=COALESCE($12,max_ltv_bps) WHERE id=$13`, args...)
	}
	if e != nil {
		return e
	}
	if e = audit(c.Context(), tx, actor(c), "configure", "loan_products", iid, nil, b); e != nil {
		return e
	}
	if e = tx.Commit(c.Context()); e != nil {
		return e
	}
	return ok(c, fiber.Map{"id": iid})
}
func (a *API) applyLoan(c *fiber.Ctx) error        { return a.applyLoanMode(c, false) }
func (a *API) applySecuredLoan(c *fiber.Ctx) error { return a.applyLoanMode(c, true) }
func (a *API) applyLoanMode(c *fiber.Ctx, secured bool) error {
	var b struct {
		ProductID int64 `json:"product_id"`
		Amount    int64
		Tenor     int
		Purpose   string
		Documents []int64 `json:"document_ids"`
	}
	if c.BodyParser(&b) != nil || b.Amount <= 0 || b.Amount > finance.MaxAmount || b.Tenor < 1 || b.Tenor > 120 || b.Amount < int64(b.Tenor) || b.Purpose == "" || (!secured && len(b.Documents) == 0) {
		return fiber.ErrBadRequest
	}
	tx, e := a.DB.Begin(c.Context())
	if e != nil {
		return e
	}
	defer tx.Rollback(c.Context())
	var memberStatus string
	if e = tx.QueryRow(c.Context(), "SELECT status FROM members WHERE id=$1 FOR UPDATE", member(c)).Scan(&memberStatus); e != nil {
		return fiber.ErrForbidden
	}
	if memberStatus != "active" {
		return fail(c, 403, "inactive_member", "Keanggotaan harus aktif")
	}
	var overdue bool
	var activeCount int
	if e = tx.QueryRow(c.Context(), `SELECT EXISTS(SELECT 1 FROM installments i JOIN loans l ON l.id=i.loan_id WHERE l.member_id=$1 AND i.status='unpaid' AND i.due_date<(now() AT TIME ZONE 'Asia/Makassar')::date), (SELECT count(*) FROM loans WHERE member_id=$1 AND status='active')`, member(c)).Scan(&overdue, &activeCount); e != nil {
		return e
	}
	if overdue {
		return fail(c, 422, "overdue_obligation", "Pengajuan diblokir karena masih ada angsuran lewat jatuh tempo")
	}
	var tier string
	var tierCap int64
	var discountBPS int64
	if e = tx.QueryRow(c.Context(), `SELECT t.tier,p.max_loan_amount,p.loan_discount_bps FROM member_tiers t JOIN member_tier_policies p ON p.tier=t.tier WHERE t.member_id=$1`, member(c)).Scan(&tier, &tierCap, &discountBPS); e == pgx.ErrNoRows {
		tier = "regular"
		tierCap = 25000000
		discountBPS = 0
	} else if e != nil {
		return e
	}
	if b.Amount > tierCap {
		return fail(c, 422, "tier_loan_limit", "Nominal pengajuan melebihi plafon pinjaman berdasarkan level anggota")
	}
	if tier == "vip" && !secured {
		return fail(c, 422, "secured_application_required", "Pengajuan VIP wajib menggunakan alur pengajuan secured dengan agunan")
	}
	if secured {
		var maxLTV *int32
		if e = tx.QueryRow(c.Context(), `SELECT max_ltv_bps FROM loan_products WHERE id=$1`, b.ProductID).Scan(&maxLTV); e != nil {
			return e
		}
		if maxLTV == nil {
			return fail(c, 422, "ltv_not_configured", "Batas LTV produk harus dikonfigurasi sebelum pengajuan secured")
		}
	}
	if secured {
		var marital string
		var npwp string
		if e = tx.QueryRow(c.Context(), `SELECT COALESCE(marital_status,''),COALESCE(npwp,'') FROM members WHERE id=$1`, member(c)).Scan(&marital, &npwp); e != nil {
			return e
		}
		var hasIdentity, hasIncome, hasBank, hasSpouseID, hasMarriage, hasNPWP bool
		if e = tx.QueryRow(c.Context(), `SELECT
		EXISTS(SELECT 1 FROM documents WHERE user_id=$1 AND kind='identity' AND review_status='verified'),
		EXISTS(SELECT 1 FROM documents WHERE user_id=$1 AND kind IN ('salary_slip','business_proof') AND review_status='verified'),
		EXISTS(SELECT 1 FROM documents WHERE user_id=$1 AND kind='bank_statement' AND review_status='verified'),
		EXISTS(SELECT 1 FROM documents WHERE user_id=$1 AND kind='spouse_id' AND review_status='verified'),
		EXISTS(SELECT 1 FROM documents WHERE user_id=$1 AND kind='marriage_certificate' AND review_status='verified'),
		EXISTS(SELECT 1 FROM documents WHERE user_id=$1 AND kind='npwp' AND review_status='verified')`, actor(c)).Scan(&hasIdentity, &hasIncome, &hasBank, &hasSpouseID, &hasMarriage, &hasNPWP); e != nil {
			return e
		}
		if !hasIdentity || !hasIncome || !hasBank {
			return fail(c, 422, "verified_documents_required", "KTP, slip gaji atau bukti usaha, dan rekening koran wajib terverifikasi di Document Vault")
		}
		if (strings.Contains(strings.ToLower(marital), "menikah") || strings.Contains(strings.ToLower(marital), "married")) && (!hasSpouseID || !hasMarriage) {
			return fail(c, 422, "spouse_documents_required", "KTP pasangan dan buku nikah wajib terverifikasi")
		}
		if b.Amount > 50000000 && (!hasNPWP || strings.TrimSpace(npwp) == "") {
			return fail(c, 422, "npwp_required", "NPWP dan dokumennya wajib terverifikasi untuk pinjaman di atas Rp50 juta")
		}
	}
	var lockedProduct int64
	if e = tx.QueryRow(c.Context(), "SELECT id FROM loan_products WHERE id=$1 AND configured AND active FOR SHARE", b.ProductID).Scan(&lockedProduct); e != nil {
		return fail(c, 400, "product_policy", "Produk belum aktif atau belum dikonfigurasi")
	}
	seen := map[int64]bool{}
	for _, did := range b.Documents {
		if did <= 0 || seen[did] {
			return fail(c, 400, "invalid_documents", "Dokumen tidak valid atau duplikat")
		}
		seen[did] = true
		var doc int64
		if e = tx.QueryRow(c.Context(), "SELECT id FROM documents WHERE id=$1 AND user_id=$2 AND application_id IS NULL AND kind='loan' FOR UPDATE", did, actor(c)).Scan(&doc); e != nil {
			return fail(c, 403, "invalid_documents", "Dokumen pinjaman harus milik pemohon dan belum digunakan")
		}
	}
	var iid int64
	e = tx.QueryRow(c.Context(), `INSERT INTO loan_applications(member_id,product_id,amount,tenor,purpose,annual_rate_bps,interest_method,penalty_daily_amount,penalty_daily_bps,require_collateral,tier_snapshot,rate_discount_bps)
		SELECT $1,id,$3,$4,$5,GREATEST(0,annual_rate_bps-$6),COALESCE(interest_method_v2,interest_method),penalty_daily_amount,penalty_daily_bps,$7,$8,$6 FROM loan_products WHERE id=$2 AND configured AND active AND $3 BETWEEN min_amount AND max_amount AND $4 BETWEEN min_tenor AND max_tenor RETURNING id`, member(c), b.ProductID, b.Amount, b.Tenor, b.Purpose, discountBPS, secured, tier).Scan(&iid)
	if e != nil {
		return fail(c, 400, "product_policy", "Produk belum diatur atau jumlah/tenor tidak sesuai")
	}
	for _, did := range b.Documents {
		res, e := tx.Exec(c.Context(), "UPDATE documents SET application_id=$1 WHERE id=$2 AND user_id=$3 AND application_id IS NULL AND kind='loan'", iid, did, actor(c))
		if e != nil {
			return e
		}
		if res.RowsAffected() != 1 {
			return fiber.NewError(403, "Dokumen tidak tersedia")
		}
		if _, e = tx.Exec(c.Context(), `INSERT INTO loan_application_documents(application_id,document_id) VALUES($1,$2) ON CONFLICT DO NOTHING`, iid, did); e != nil {
			return e
		}
	}
	if secured {
		if _, e = tx.Exec(c.Context(), `INSERT INTO loan_application_documents(application_id,document_id) SELECT $1,id FROM documents WHERE user_id=$2 AND review_status='verified' AND kind IN ('identity','selfie_ktp','salary_slip','business_proof','bank_statement','spouse_id','marriage_certificate','npwp') ON CONFLICT DO NOTHING`, iid, actor(c)); e != nil {
			return e
		}
	}
	if _, e = tx.Exec(c.Context(), "INSERT INTO notifications(user_id,title,message) VALUES($1,'Pengajuan pinjaman','Pengajuan telah diterima dan menunggu survei')", actor(c)); e != nil {
		return e
	}
	if e = audit(c.Context(), tx, actor(c), "submitted", "loan_applications", iid, nil, fiber.Map{"request": b, "active_loan_count": activeCount, "has_overdue": false}); e != nil {
		return e
	}
	if e = tx.Commit(c.Context()); e != nil {
		return e
	}
	return ok(c, fiber.Map{"id": iid, "status": "submitted", "active_loan_count": activeCount})
}
func (a *API) surveyLoan(c *fiber.Ctx) error {
	if role(c) == "committee" {
		allowed, err := a.isAssignedCommittee(c, id(c))
		if err != nil {
			return err
		}
		if !allowed {
			return fiber.ErrForbidden
		}
	}
	var requiresCollateral bool
	var approvedCollateral bool
	if err := a.DB.QueryRow(c.Context(), `SELECT require_collateral,EXISTS(SELECT 1 FROM loan_collaterals WHERE application_id=loan_applications.id AND status='approved') FROM loan_applications WHERE id=$1`, id(c)).Scan(&requiresCollateral, &approvedCollateral); err != nil {
		return fiber.ErrNotFound
	}
	if requiresCollateral && !approvedCollateral {
		return fail(c, 409, "collateral_not_approved", "Agunan harus diperiksa dan disetujui sebelum survei/analisis dilanjutkan")
	}
	var b struct{ Survey, Recommendation string }
	if c.BodyParser(&b) != nil || b.Survey == "" || !contains([]string{"eligible", "ineligible"}, b.Recommendation) {
		return fiber.ErrBadRequest
	}
	tx, e := a.DB.Begin(c.Context())
	if e != nil {
		return e
	}
	defer tx.Rollback(c.Context())
	res, e := tx.Exec(c.Context(), "UPDATE loan_applications SET survey=$1,recommendation=$2,status='surveyed' WHERE id=$3 AND status IN ('submitted','surveyed')", b.Survey, b.Recommendation, id(c))
	if e != nil {
		return e
	}
	if res.RowsAffected() != 1 {
		return fiber.NewError(409, "Status pengajuan tidak sesuai")
	}
	if e = audit(c.Context(), tx, actor(c), "survey", "loan_applications", id(c), nil, b); e != nil {
		return e
	}
	if e = tx.Commit(c.Context()); e != nil {
		return e
	}
	return ok(c, fiber.Map{"status": "surveyed"})
}
func (a *API) decideLoan(c *fiber.Ctx) error {
	if role(c) == "committee" {
		allowed, err := a.isAssignedCommittee(c, id(c))
		if err != nil {
			return err
		}
		if !allowed {
			return fiber.ErrForbidden
		}
	}
	var b struct {
		Approve bool
		Reason  string
	}
	if c.BodyParser(&b) != nil || b.Reason == "" {
		return fiber.ErrBadRequest
	}
	tx, e := a.DB.Begin(c.Context())
	if e != nil {
		return e
	}
	defer tx.Rollback(c.Context())
	status := "rejected"
	if b.Approve {
		status = "approved"
	}
	res, e := tx.Exec(c.Context(), "UPDATE loan_applications SET status=$1,decision_reason=$2 WHERE id=$3 AND status='surveyed' AND survey<>''", status, b.Reason, id(c))
	if e != nil {
		return e
	}
	if res.RowsAffected() != 1 {
		return fiber.NewError(409, "Survei harus lengkap dan pengajuan belum diputuskan")
	}
	if e = audit(c.Context(), tx, actor(c), "decision", "loan_applications", id(c), nil, b); e != nil {
		return e
	}
	if _, e = tx.Exec(c.Context(), "INSERT INTO notifications(user_id,title,message) SELECT m.user_id,'Keputusan pinjaman',$1 FROM loan_applications x JOIN members m ON m.id=x.member_id WHERE x.id=$2", status+": "+b.Reason, id(c)); e != nil {
		return e
	}
	if e = tx.Commit(c.Context()); e != nil {
		return e
	}
	return ok(c, fiber.Map{"status": status})
}
func (a *API) disburse(c *fiber.Ctx) error {
	var b struct{ Reference string }
	if c.BodyParser(&b) != nil || b.Reference == "" {
		return fiber.ErrBadRequest
	}
	tx, e := a.DB.Begin(c.Context())
	if e != nil {
		return e
	}
	defer tx.Rollback(c.Context())
	var mid, amount, rate, pa, pb int64
	var tenor int
	var method, status, tierSnapshot string
	e = tx.QueryRow(c.Context(), "SELECT member_id,amount,tenor,annual_rate_bps,interest_method,penalty_daily_amount,penalty_daily_bps,status,tier_snapshot FROM loan_applications WHERE id=$1 FOR UPDATE", id(c)).Scan(&mid, &amount, &tenor, &rate, &method, &pa, &pb, &status, &tierSnapshot)
	if e != nil {
		return fiber.ErrNotFound
	}
	if status != "approved" {
		return fiber.NewError(409, "Pinjaman belum disetujui atau sudah dicairkan")
	}
	var requiresCollateral, approvedCollateral bool
	if e = tx.QueryRow(c.Context(), `SELECT require_collateral,EXISTS(SELECT 1 FROM loan_collaterals WHERE application_id=loan_applications.id AND status='approved') FROM loan_applications WHERE id=$1`, id(c)).Scan(&requiresCollateral, &approvedCollateral); e != nil {
		return e
	}
	if requiresCollateral && !approvedCollateral {
		return fail(c, 409, "collateral_not_approved", "Pencairan diblokir karena agunan belum disetujui")
	}

	var ms string
	if e = tx.QueryRow(c.Context(), "SELECT status FROM members WHERE id=$1 FOR SHARE", mid).Scan(&ms); e != nil {
		return e
	}
	if ms != "active" {
		return fiber.NewError(409, "Anggota tidak aktif")
	}
	loc, _ := time.LoadLocation("Asia/Makassar")
	now := time.Now().In(loc)
	schedule, e := finance.Schedule(amount, tenor, rate, method, now)
	if e != nil {
		return e
	}
	var lid int64
	e = tx.QueryRow(c.Context(), `INSERT INTO loans(application_id,member_id,amount,annual_rate_bps,interest_method,penalty_daily_amount,penalty_daily_bps,reference,tier_snapshot) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING id`, id(c), mid, amount, rate, method, pa, pb, b.Reference, tierSnapshot).Scan(&lid)
	if e != nil {
		return e
	}
	for _, i := range schedule {
		_, e = tx.Exec(c.Context(), "INSERT INTO installments(loan_id,number,due_date,principal,interest) VALUES($1,$2,$3,$4,$5)", lid, i.Number, i.DueDate, i.Principal, i.Interest)
		if e != nil {
			return e
		}
	}
	if _, e = tx.Exec(c.Context(), "UPDATE loan_applications SET status='disbursed' WHERE id=$1", id(c)); e != nil {
		return e
	}
	if e = journal(c.Context(), tx, fmt.Sprint("loan:", lid), "loan_receivable", "cash", amount, mid, "Pencairan pinjaman"); e != nil {
		return e
	}
	if e = audit(c.Context(), tx, actor(c), "disburse", "loans", lid, nil, b); e != nil {
		return e
	}
	if e = tx.Commit(c.Context()); e != nil {
		return e
	}
	return ok(c, fiber.Map{"id": lid, "status": "active"})
}
func (a *API) installments(c *fiber.Ctx) error {
	loan := c.QueryInt("loan_id", 0)
	over := c.Path() == "/api/v1/penalties"
	switch role(c) {
	case "member":
		return a.rows(c, `SELECT i.*,l.member_id,(i.principal+i.interest+i.penalty) total FROM installments i JOIN loans l ON l.id=i.loan_id WHERE l.member_id=$1 AND ($2=0 OR i.loan_id=$2) AND (NOT $3 OR (i.status='unpaid' AND i.due_date<(now() AT TIME ZONE 'Asia/Makassar')::date)) ORDER BY i.due_date,i.id`, member(c), loan, over)
	case "admin":
		return a.rows(c, `SELECT i.*,l.member_id,u.name member_name,(i.principal+i.interest+i.penalty) total FROM installments i JOIN loans l ON l.id=i.loan_id JOIN members m ON m.id=l.member_id JOIN users u ON u.id=m.user_id WHERE ($1=0 OR i.loan_id=$1) AND (NOT $2 OR (i.status='unpaid' AND i.due_date<(now() AT TIME ZONE 'Asia/Makassar')::date)) ORDER BY i.due_date,i.id`, loan, over)
	case "committee":
		return a.rows(c, `SELECT i.*,l.member_id,(i.principal+i.interest+i.penalty) total FROM installments i JOIN loans l ON l.id=i.loan_id JOIN loan_application_assignments a ON a.application_id=l.application_id WHERE a.committee_user_id=$1 AND a.unassigned_at IS NULL AND ($2=0 OR i.loan_id=$2) AND (NOT $3 OR (i.status='unpaid' AND i.due_date<(now() AT TIME ZONE 'Asia/Makassar')::date) ) ORDER BY i.due_date,i.id`, actor(c), loan, over)
	default:
		return fail(c, 403, "forbidden", "Teller tidak memiliki akses ke seluruh jadwal angsuran")
	}
}
func updatePenalty(c *fiber.Ctx, tx pgx.Tx, iid int64) (mid, principal, interest, penalty int64, status string, err error) {
	var due time.Time
	var pa, pb int64
	err = tx.QueryRow(c.Context(), `SELECT l.member_id,i.principal,i.interest,i.penalty,i.status,i.due_date,l.penalty_daily_amount,l.penalty_daily_bps FROM installments i JOIN loans l ON l.id=i.loan_id WHERE i.id=$1 FOR UPDATE OF i`, iid).Scan(&mid, &principal, &interest, &penalty, &status, &due, &pa, &pb)
	if err != nil {
		return
	}
	if status == "unpaid" {
		loc, _ := time.LoadLocation("Asia/Makassar")
		due = localDueDate(due, loc)
		penalty = finance.Penalty(principal, pa, pb, due, time.Now().In(loc))
		_, err = tx.Exec(c.Context(), "UPDATE installments SET penalty=$1 WHERE id=$2", penalty, iid)
	}
	return
}
func (a *API) createPayment(c *fiber.Ctx) error {
	var b struct {
		InstallmentID int64 `json:"installment_id"`
		Reference     string
	}
	if c.BodyParser(&b) != nil || b.Reference == "" {
		return fiber.ErrBadRequest
	}
	tx, e := a.DB.Begin(c.Context())
	if e != nil {
		return e
	}
	defer tx.Rollback(c.Context())
	mid, p, i, pen, status, e := updatePenalty(c, tx, b.InstallmentID)
	if e != nil {
		return fiber.ErrNotFound
	}
	if role(c) == "member" && mid != member(c) {
		return fiber.ErrForbidden
	}
	var memberStatus string
	if e = tx.QueryRow(c.Context(), "SELECT status FROM members WHERE id=$1 FOR SHARE", mid).Scan(&memberStatus); e != nil {
		return e
	}
	if memberStatus != "active" {
		return fail(c, 403, "inactive_member", "Keanggotaan harus aktif untuk mengajukan pembayaran")
	}
	if status != "unpaid" {
		return fiber.NewError(409, "Angsuran sudah dibayar")
	}
	var pid int64
	e = tx.QueryRow(c.Context(), "INSERT INTO payments(installment_id,amount,reference) VALUES($1,$2,$3) RETURNING id", b.InstallmentID, p+i+pen, b.Reference).Scan(&pid)
	if e != nil {
		return e
	}
	if e = audit(c.Context(), tx, actor(c), "submitted", "payments", pid, nil, b); e != nil {
		return e
	}
	if e = tx.Commit(c.Context()); e != nil {
		return e
	}
	return ok(c, fiber.Map{"id": pid, "status": "pending", "amount": p + i + pen})
}
func (a *API) verifyPayment(c *fiber.Ctx) error {
	var b struct {
		Approve bool
		Reason  string
	}
	if c.BodyParser(&b) != nil {
		return fiber.ErrBadRequest
	}
	tx, e := a.DB.Begin(c.Context())
	if e != nil {
		return e
	}
	defer tx.Rollback(c.Context())
	var iid, submittedAmount int64
	var status string
	e = tx.QueryRow(c.Context(), "SELECT installment_id,status,amount FROM payments WHERE id=$1 FOR UPDATE", id(c)).Scan(&iid, &status, &submittedAmount)
	if e != nil {
		return fiber.ErrNotFound
	}
	if status != "pending" {
		return fiber.NewError(409, "Pembayaran sudah diproses")
	}
	var loanID int64
	if e = tx.QueryRow(c.Context(), "SELECT l.id FROM loans l JOIN installments i ON i.loan_id=l.id WHERE i.id=$1 FOR UPDATE OF l", iid).Scan(&loanID); e != nil {
		return e
	}
	mid, p, i, pen, istatus, e := updatePenalty(c, tx, iid)
	if e != nil {
		return e
	}
	next := "rejected"
	if b.Approve {
		if submittedAmount != p+i+pen {
			return fail(c, 409, "payment_amount_changed", "Denda berubah sejak pengajuan. Tolak pembayaran lama dan ajukan ulang dengan nominal terbaru.")
		}
		if istatus != "unpaid" {
			return fiber.NewError(409, "Angsuran telah dibayar")
		}
		if _, e = tx.Exec(c.Context(), "UPDATE installments SET status='paid',paid_at=now() WHERE id=$1", iid); e != nil {
			return e
		}
		ref := fmt.Sprint("payment:", id(c))
		for _, v := range []struct {
			account string
			amount  int64
		}{{"loan_receivable", p}, {"interest_income", i}, {"penalty_income", pen}} {
			if e = journal(c.Context(), tx, ref, "cash", v.account, v.amount, mid, "Pembayaran angsuran"); e != nil {
				return e
			}
		}
		if _, e = tx.Exec(c.Context(), `UPDATE loans SET status='paid' WHERE id=(SELECT loan_id FROM installments WHERE id=$1) AND NOT EXISTS(SELECT 1 FROM installments WHERE loan_id=loans.id AND status='unpaid')`, iid); e != nil {
			return e
		}
		var loanPaid bool
		if e = tx.QueryRow(c.Context(), `SELECT status='paid' FROM loans WHERE id=$1`, loanID).Scan(&loanPaid); e != nil {
			return e
		}
		if loanPaid {
			if _, e = tx.Exec(c.Context(), `UPDATE collateral_locks SET status='released',released_at=now() WHERE collateral_id IN (SELECT id FROM loan_collaterals WHERE application_id=(SELECT application_id FROM loans WHERE id=$1)) AND status='active'`, loanID); e != nil {
				return e
			}
			if _, e = tx.Exec(c.Context(), `UPDATE loan_collaterals SET status='released' WHERE application_id=(SELECT application_id FROM loans WHERE id=$1) AND status='approved'`, loanID); e != nil {
				return e
			}
		}
		next = "approved"
	}
	if _, e = tx.Exec(c.Context(), "UPDATE payments SET status=$1,reason=$2,amount=$3,verified_at=now() WHERE id=$4", next, b.Reason, p+i+pen, id(c)); e != nil {
		return e
	}
	if _, e = tx.Exec(c.Context(), "INSERT INTO notifications(user_id,title,message) SELECT user_id,'Verifikasi pembayaran',$1 FROM members WHERE id=$2", next+": "+b.Reason, mid); e != nil {
		return e
	}
	if e = audit(c.Context(), tx, actor(c), "verify", "payments", id(c), status, fiber.Map{"status": next, "amount": p + i + pen}); e != nil {
		return e
	}
	if e = tx.Commit(c.Context()); e != nil {
		return e
	}
	return ok(c, fiber.Map{"id": id(c), "status": next, "amount": p + i + pen})
}

// PostgreSQL DATE is a civil date, not an instant; preserve Y/M/D when assigning the business timezone.
func localDueDate(due time.Time, loc *time.Location) time.Time {
	return time.Date(due.Year(), due.Month(), due.Day(), 0, 0, 0, 0, loc)
}
