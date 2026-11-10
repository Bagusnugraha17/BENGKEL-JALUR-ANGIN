package api

import (
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/jackc/pgx/v5"
)

func (a *API) creditExtensionRoutes(r fiber.Router) {
	r.Get("/loan-products/extended", require("admin"), func(c *fiber.Ctx) error {
		return a.rows(c, `SELECT id,name,min_amount,max_amount,min_tenor,max_tenor,annual_rate_bps,COALESCE(interest_method_v2,interest_method) interest_method,penalty_daily_amount,penalty_daily_bps,configured,active,max_ltv_bps FROM loan_products ORDER BY id`)
	})
	r.Post("/loan-applications/:id/assign", require("admin"), a.assignLoanApplication)
	r.Get("/loan-applications/:id/documents", a.listApplicationDocuments)
	r.Post("/loan-applications/:id/collaterals", require("member"), a.createCollateral)
	r.Get("/loan-applications/:id/collaterals", a.listApplicationCollaterals)
	r.Post("/loan-collaterals/:id/review", require("committee", "admin"), a.reviewCollateral)
	r.Get("/early-repayment/policy", require("admin"), func(c *fiber.Ctx) error { return a.rows(c, `SELECT * FROM early_repayment_policies WHERE id=1`) })
	r.Put("/early-repayment/policy", require("admin"), a.updateEarlyRepaymentPolicy)
	r.Post("/loans/:id/early-repayment/quote", require("member"), a.quoteEarlyRepayment)
	r.Post("/loans/:id/early-repayments", require("member"), a.createEarlyRepayment)
	r.Get("/early-repayments", a.listEarlyRepayments)
	r.Post("/early-repayments/:id/verify", require("teller", "admin"), a.verifyEarlyRepayment)
}

func (a *API) assignLoanApplication(c *fiber.Ctx) error {
	var b struct {
		CommitteeUserID int64  `json:"committee_user_id"`
		Reason          string `json:"reason"`
	}
	if c.BodyParser(&b) != nil || b.CommitteeUserID <= 0 || strings.TrimSpace(b.Reason) == "" {
		return fail(c, 400, "invalid_assignment", "Petugas komite dan alasan penugasan wajib diisi")
	}
	tx, err := a.DB.Begin(c.Context())
	if err != nil {
		return err
	}
	defer tx.Rollback(c.Context())
	var appStatus string
	if err = tx.QueryRow(c.Context(), `SELECT status FROM loan_applications WHERE id=$1 FOR UPDATE`, id(c)).Scan(&appStatus); err == pgx.ErrNoRows {
		return fiber.ErrNotFound
	} else if err != nil {
		return err
	}
	var roleName string
	var active bool
	if err = tx.QueryRow(c.Context(), `SELECT role,active FROM users WHERE id=$1`, b.CommitteeUserID).Scan(&roleName, &active); err != nil {
		return fiber.ErrNotFound
	}
	if roleName != "committee" || !active {
		return fail(c, 422, "invalid_committee_user", "Akun harus merupakan komite kredit yang aktif")
	}
	if _, err = tx.Exec(c.Context(), `UPDATE loan_application_assignments SET unassigned_at=now() WHERE application_id=$1 AND unassigned_at IS NULL`, id(c)); err != nil {
		return err
	}
	var assignmentID int64
	err = tx.QueryRow(c.Context(), `INSERT INTO loan_application_assignments(application_id,committee_user_id,assigned_by,reason) VALUES($1,$2,$3,$4) RETURNING id`, id(c), b.CommitteeUserID, actor(c), strings.TrimSpace(b.Reason)).Scan(&assignmentID)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(c.Context(), `INSERT INTO notifications(user_id,title,message) VALUES($1,'Pengajuan kredit ditugaskan',$2)`, b.CommitteeUserID, fmt.Sprintf("Pengajuan #%d ditugaskan kepada Anda untuk ditinjau.", id(c))); err != nil {
		return err
	}
	if err = audit(c.Context(), tx, actor(c), "loan_application_assigned", "loan_application_assignments", assignmentID, nil, b); err != nil {
		return err
	}
	if err = tx.Commit(c.Context()); err != nil {
		return err
	}
	return ok(c, fiber.Map{"assignment_id": assignmentID, "application_id": id(c), "committee_user_id": b.CommitteeUserID})
}

func (a *API) applicationAccess(c *fiber.Ctx, applicationID int64) (bool, error) {
	if role(c) == "admin" {
		return true, nil
	}
	if role(c) == "member" {
		var ok bool
		err := a.DB.QueryRow(c.Context(), `SELECT EXISTS(SELECT 1 FROM loan_applications WHERE id=$1 AND member_id=$2)`, applicationID, member(c)).Scan(&ok)
		return ok, err
	}
	if role(c) == "committee" {
		return a.isAssignedCommittee(c, applicationID)
	}
	return false, nil
}

func (a *API) listApplicationDocuments(c *fiber.Ctx) error {
	allowed, err := a.applicationAccess(c, id(c))
	if err != nil {
		return err
	}
	if !allowed {
		return fail(c, 403, "forbidden", "Tidak berhak melihat dokumen pengajuan ini")
	}
	return a.documentRows(c, `SELECT DISTINCT d.id,d.kind,d.filename,d.mime_type,d.size,d.review_status,d.review_reason,d.created_at FROM documents d LEFT JOIN loan_application_documents lad ON lad.document_id=d.id WHERE d.application_id=$1 OR lad.application_id=$1 ORDER BY d.created_at DESC`, id(c))
}

func (a *API) listApplicationCollaterals(c *fiber.Ctx) error {
	allowed, err := a.applicationAccess(c, id(c))
	if err != nil {
		return err
	}
	if !allowed {
		return fail(c, 403, "forbidden", "Tidak berhak melihat agunan pengajuan ini")
	}
	return a.rows(c, `SELECT c.id,c.application_id,c.type,c.details,c.estimated_value,c.appraised_value,c.status,c.review_reason,c.created_at,COALESCE((SELECT json_agg(json_build_object('id',d.id,'kind',d.kind,'filename',d.filename,'review_status',d.review_status)) FROM collateral_documents cd JOIN documents d ON d.id=cd.document_id WHERE cd.collateral_id=c.id),'[]'::json) documents FROM loan_collaterals c WHERE c.application_id=$1 ORDER BY c.id`, id(c))
}

func (a *API) createCollateral(c *fiber.Ctx) error {
	var b struct {
		Type           string         `json:"type"`
		Details        map[string]any `json:"details"`
		EstimatedValue int64          `json:"estimated_value"`
		DocumentIDs    []int64        `json:"document_ids"`
	}
	if c.BodyParser(&b) != nil || !contains([]string{"savings", "deposit", "vehicle", "property"}, b.Type) || b.EstimatedValue <= 0 || b.EstimatedValue > 1000000000000 {
		return fiber.ErrBadRequest
	}
	if b.Details == nil {
		b.Details = map[string]any{}
	}
	if err := validateCollateralDetails(b.Type, b.Details); err != nil {
		return fail(c, 400, "invalid_collateral_details", err.Error())
	}
	var owner int64
	var amount int64
	var productID int64
	err := a.DB.QueryRow(c.Context(), `SELECT member_id,amount,product_id FROM loan_applications WHERE id=$1`, id(c)).Scan(&owner, &amount, &productID)
	if err == pgx.ErrNoRows {
		return fiber.ErrNotFound
	}
	if err != nil {
		return err
	}
	if owner != member(c) {
		return fiber.ErrForbidden
	}
	if len(b.DocumentIDs) == 0 && (b.Type == "vehicle" || b.Type == "property") {
		return fail(c, 422, "collateral_documents_required", "Dokumen agunan wajib dilampirkan")
	}
	tx, err := a.DB.Begin(c.Context())
	if err != nil {
		return err
	}
	defer tx.Rollback(c.Context())
	var cid int64
	err = tx.QueryRow(c.Context(), `INSERT INTO loan_collaterals(application_id,type,details,estimated_value) VALUES($1,$2,$3,$4) RETURNING id`, id(c), b.Type, b.Details, b.EstimatedValue).Scan(&cid)
	if err != nil {
		return err
	}
	seen := map[int64]bool{}
	for _, did := range b.DocumentIDs {
		if did <= 0 || seen[did] {
			return fail(c, 400, "invalid_documents", "ID dokumen duplikat atau tidak valid")
		}
		seen[did] = true
		var docID int64
		if err = tx.QueryRow(c.Context(), `SELECT id FROM documents WHERE id=$1 AND user_id=$2 AND application_id IS NULL`, did, actor(c)).Scan(&docID); err != nil {
			return fail(c, 403, "invalid_documents", "Dokumen agunan harus milik anggota dan belum digunakan")
		}
		if _, err = tx.Exec(c.Context(), `INSERT INTO collateral_documents(collateral_id,document_id) VALUES($1,$2)`, cid, did); err != nil {
			return err
		}
	}
	if err = audit(c.Context(), tx, actor(c), "collateral_submitted", "loan_collaterals", cid, nil, fiber.Map{"type": b.Type, "estimated_value": b.EstimatedValue, "document_ids": b.DocumentIDs}); err != nil {
		return err
	}
	if err = tx.Commit(c.Context()); err != nil {
		return err
	}
	return ok(c, fiber.Map{"id": cid, "status": "pending"})
}

func validateCollateralDetails(kind string, d map[string]any) error {
	text := func(k string) string {
		v, ok := d[k]
		if !ok || v == nil {
			return ""
		}
		return strings.TrimSpace(fmt.Sprint(v))
	}
	positive := func(k string) bool {
		v, ok := d[k]
		if !ok {
			return false
		}
		n, err := strconv.ParseFloat(fmt.Sprint(v), 64)
		return err == nil && n > 0
	}
	switch kind {
	case "vehicle":
		for _, k := range []string{"bpkb_number", "plate_number", "brand"} {
			if text(k) == "" {
				return fmt.Errorf("detail kendaraan %s wajib diisi", k)
			}
		}
		year, err := strconv.Atoi(text("year"))
		if err != nil || year < 1900 || year > time.Now().Year()+1 {
			return fmt.Errorf("tahun kendaraan tidak valid")
		}
	case "property":
		for _, k := range []string{"certificate_number", "location"} {
			if text(k) == "" {
				return fmt.Errorf("detail properti %s wajib diisi", k)
			}
		}
		if !(positive("area_m2") || positive("area_sqm")) || !positive("njop") {
			return fmt.Errorf("luas properti dan NJOP harus lebih dari nol")
		}
	case "savings", "deposit":
		if !positive("asset_id") || !positive("locked_amount") {
			return fmt.Errorf("asset_id dan locked_amount wajib diisi")
		}
	}
	return nil
}

func (a *API) reviewCollateral(c *fiber.Ctx) error {
	var b struct {
		Approve        bool   `json:"approve"`
		AppraisedValue int64  `json:"appraised_value"`
		Reason         string `json:"reason"`
	}
	if c.BodyParser(&b) != nil || strings.TrimSpace(b.Reason) == "" {
		return fail(c, 400, "invalid_review", "Keputusan dan alasan wajib diisi")
	}
	tx, err := a.DB.Begin(c.Context())
	if err != nil {
		return err
	}
	defer tx.Rollback(c.Context())
	var appID, estimate, amount, productID int64
	var kind, status string
	err = tx.QueryRow(c.Context(), `SELECT c.application_id,c.type,c.estimated_value,c.status,x.amount,x.product_id FROM loan_collaterals c JOIN loan_applications x ON x.id=c.application_id WHERE c.id=$1 FOR UPDATE OF c`, id(c)).Scan(&appID, &kind, &estimate, &status, &amount, &productID)
	if err == pgx.ErrNoRows {
		return fiber.ErrNotFound
	}
	if err != nil {
		return err
	}
	if role(c) == "committee" {
		assigned, e := a.isAssignedCommittee(c, appID)
		if e != nil {
			return e
		}
		if !assigned {
			return fiber.ErrForbidden
		}
	}
	if status != "pending" {
		return fail(c, 409, "collateral_already_reviewed", "Agunan sudah ditinjau")
	}
	if b.Approve {
		if b.AppraisedValue <= 0 || b.AppraisedValue > estimate {
			return fail(c, 400, "invalid_appraisal", "Nilai taksiran disetujui harus lebih dari nol dan tidak melebihi estimasi awal")
		}
		var maxLTV *int32
		if err = tx.QueryRow(c.Context(), `SELECT max_ltv_bps FROM loan_products WHERE id=$1`, productID).Scan(&maxLTV); err != nil {
			return err
		}
		if maxLTV == nil {
			return fail(c, 422, "ltv_not_configured", "Batas LTV produk pinjaman belum dikonfigurasi")
		}
		ltv := roundMoney(amount, 10000, b.AppraisedValue)
		if ltv > int64(*maxLTV) {
			return fail(c, 422, "ltv_exceeded", "Nilai pinjaman melebihi batas LTV agunan")
		}
		var required int
		if kind == "vehicle" {
			err = tx.QueryRow(c.Context(), `SELECT count(*) FROM collateral_documents cd JOIN documents d ON d.id=cd.document_id WHERE cd.collateral_id=$1 AND d.review_status='verified' AND d.kind IN ('bpkb','stnk','vehicle_invoice','vehicle_kir','vehicle_photo','collateral_photo')`, id(c)).Scan(&required)
			if err != nil {
				return err
			}
			if required < 5 {
				return fail(c, 422, "collateral_documents_unverified", "BPKB, STNK, faktur, buku KIR, dan foto kendaraan wajib terverifikasi")
			}
		}
		if kind == "property" {
			err = tx.QueryRow(c.Context(), `SELECT count(*) FROM collateral_documents cd JOIN documents d ON d.id=cd.document_id WHERE cd.collateral_id=$1 AND d.review_status='verified' AND d.kind IN ('property_certificate','pbb','property_photo','collateral_photo')`, id(c)).Scan(&required)
			if err != nil {
				return err
			}
			if required < 3 {
				return fail(c, 422, "collateral_documents_unverified", "Sertifikat properti, SPPT PBB, dan foto properti wajib terverifikasi")
			}
		}
	}
	next := "rejected"
	if b.Approve {
		next = "approved"
	}
	_, err = tx.Exec(c.Context(), `UPDATE loan_collaterals SET status=$1,appraised_value=NULLIF($2,0),review_reason=$3,reviewed_by=$4,reviewed_at=now() WHERE id=$5`, next, b.AppraisedValue, strings.TrimSpace(b.Reason), actor(c), id(c))
	if err != nil {
		return err
	}
	if b.Approve && (kind == "savings" || kind == "deposit") {
		var mid, assetID, lockedAmount int64
		if err = tx.QueryRow(c.Context(), `SELECT member_id,(details->>'asset_id')::bigint,(details->>'locked_amount')::bigint FROM loan_collaterals WHERE id=$1`, id(c)).Scan(&mid, &assetID, &lockedAmount); err != nil {
			return err
		}
		if kind == "savings" {
			var assetOwner, balance int64
			if err = tx.QueryRow(c.Context(), `SELECT member_id,balance FROM savings WHERE id=$1 FOR UPDATE`, assetID).Scan(&assetOwner, &balance); err != nil {
				return fail(c, 422, "collateral_asset_not_found", "Rekening simpanan agunan tidak ditemukan")
			}
			if assetOwner != mid || lockedAmount <= 0 || balance < lockedAmount {
				return fail(c, 422, "collateral_asset_invalid", "Saldo simpanan tidak cukup untuk dijaminkan")
			}
		} else {
			var assetOwner, principal int64
			var assetStatus string
			if err = tx.QueryRow(c.Context(), `SELECT member_id,principal,status FROM deposits WHERE id=$1 FOR UPDATE`, assetID).Scan(&assetOwner, &principal, &assetStatus); err != nil {
				return fail(c, 422, "collateral_asset_not_found", "Deposito agunan tidak ditemukan")
			}
			if assetOwner != mid || assetStatus != "active" || principal < lockedAmount {
				return fail(c, 422, "collateral_asset_invalid", "Deposito tidak aktif atau nilainya tidak cukup untuk dijaminkan")
			}
		}
		assetType := kind
		if _, err = tx.Exec(c.Context(), `INSERT INTO collateral_locks(collateral_id,member_id,asset_type,asset_id,locked_amount) VALUES($1,$2,$3,$4,$5)`, id(c), mid, assetType, assetID, lockedAmount); err != nil {
			return err
		}
	}
	if err = audit(c.Context(), tx, actor(c), "collateral_reviewed", "loan_collaterals", id(c), fiber.Map{"status": status}, fiber.Map{"status": next, "appraised_value": b.AppraisedValue, "reason": b.Reason}); err != nil {
		return err
	}
	if err = tx.Commit(c.Context()); err != nil {
		return err
	}
	return ok(c, fiber.Map{"id": id(c), "status": next})
}

func (a *API) updateEarlyRepaymentPolicy(c *fiber.Ctx) error {
	var b struct {
		Configured bool `json:"configured"`
		Flat       int  `json:"flat_discount_bps"`
		Declining  int  `json:"declining_discount_bps"`
		Annuity    int  `json:"annuity_discount_bps"`
		Fee        int  `json:"fee_bps"`
	}
	if c.BodyParser(&b) != nil || b.Flat < 0 || b.Flat > 10000 || b.Declining < 0 || b.Declining > 10000 || b.Annuity < 0 || b.Annuity > 10000 || b.Fee < 0 || b.Fee > 10000 {
		return fail(c, 400, "invalid_policy", "Kebijakan pelunasan dipercepat tidak valid")
	}
	tx, err := a.DB.Begin(c.Context())
	if err != nil {
		return err
	}
	defer tx.Rollback(c.Context())
	_, err = tx.Exec(c.Context(), `UPDATE early_repayment_policies SET configured=$1,flat_discount_bps=$2,declining_discount_bps=$3,annuity_discount_bps=$4,fee_bps=$5,updated_by=$6,updated_at=now() WHERE id=1`, b.Configured, b.Flat, b.Declining, b.Annuity, b.Fee, actor(c))
	if err != nil {
		return err
	}
	if err = audit(c.Context(), tx, actor(c), "early_repayment_policy_updated", "early_repayment_policies", 1, nil, b); err != nil {
		return err
	}
	if err = tx.Commit(c.Context()); err != nil {
		return err
	}
	return ok(c, fiber.Map{"updated": true})
}

func (a *API) quoteEarlyRepayment(c *fiber.Ctx) error {
	var mid int64
	var method string
	var status string
	err := a.DB.QueryRow(c.Context(), `SELECT member_id,interest_method,status FROM loans WHERE id=$1`, id(c)).Scan(&mid, &method, &status)
	if err == pgx.ErrNoRows {
		return fiber.ErrNotFound
	}
	if err != nil {
		return err
	}
	if mid != member(c) {
		return fiber.ErrForbidden
	}
	if status != "active" {
		return fail(c, 409, "loan_not_active", "Pinjaman tidak aktif")
	}
	var configured bool
	var flat, declining, annuity, fee int64
	err = a.DB.QueryRow(c.Context(), `SELECT configured,flat_discount_bps,declining_discount_bps,annuity_discount_bps,fee_bps FROM early_repayment_policies WHERE id=1`).Scan(&configured, &flat, &declining, &annuity, &fee)
	if err != nil {
		return err
	}
	if !configured {
		return fail(c, 422, "policy_not_configured", "Kebijakan pelunasan dipercepat belum dikonfigurasi admin")
	}
	var principal, interest, penalty int64
	err = a.DB.QueryRow(c.Context(), `SELECT COALESCE(sum(i.principal) FILTER(WHERE i.status='unpaid'),0)::bigint,COALESCE(sum(i.interest) FILTER(WHERE i.status='unpaid'),0)::bigint,COALESCE(sum(i.penalty) FILTER(WHERE i.status='unpaid'),0)::bigint FROM installments i WHERE i.loan_id=$1`, id(c)).Scan(&principal, &interest, &penalty)
	if err != nil {
		return err
	}
	discountRate := flat
	if method == "declining" {
		discountRate = declining
	} else if method == "annuity" {
		discountRate = annuity
	}
	discount := roundMoney(interest, discountRate, 10000)
	if discount > interest {
		discount = interest
	}
	feeAmount := roundMoney(principal, fee, 10000)
	return ok(c, fiber.Map{"loan_id": id(c), "remaining_principal": principal, "remaining_interest": interest, "interest_discount": discount, "penalty": penalty, "fee": feeAmount, "payoff_amount": principal + interest - discount + penalty + feeAmount, "method": method})
}

func (a *API) createEarlyRepayment(c *fiber.Ctx) error {
	quote, err := a.quoteEarlyRepaymentInternal(c, id(c))
	if err != nil {
		return fail(c, 422, "quote_failed", err.Error())
	}
	ref := "ERP-" + randomID()[:24]
	var rid int64
	err = a.DB.QueryRow(c.Context(), `INSERT INTO early_repayments(loan_id,member_id,quoted_amount,reference) VALUES($1,$2,$3,$4) RETURNING id`, id(c), member(c), quote, ref).Scan(&rid)
	if err != nil {
		return err
	}
	return ok(c, fiber.Map{"id": rid, "status": "pending", "quoted_amount": quote, "reference": ref})
}
func (a *API) quoteEarlyRepaymentInternal(c *fiber.Ctx, loanID int64) (int64, error) {
	var mid int64
	var method, status string
	if err := a.DB.QueryRow(c.Context(), `SELECT member_id,interest_method,status FROM loans WHERE id=$1`, loanID).Scan(&mid, &method, &status); err != nil {
		return 0, err
	}
	if mid != member(c) || status != "active" {
		return 0, fmt.Errorf("pinjaman tidak dapat dilunasi oleh akun ini")
	}
	var configured bool
	var flat, declining, annuity, fee int64
	if err := a.DB.QueryRow(c.Context(), `SELECT configured,flat_discount_bps,declining_discount_bps,annuity_discount_bps,fee_bps FROM early_repayment_policies WHERE id=1`).Scan(&configured, &flat, &declining, &annuity, &fee); err != nil {
		return 0, err
	}
	if !configured {
		return 0, fmt.Errorf("kebijakan belum dikonfigurasi")
	}
	var p, i, pen int64
	if err := a.DB.QueryRow(c.Context(), `SELECT COALESCE(sum(principal) FILTER(WHERE status='unpaid'),0)::bigint,COALESCE(sum(interest) FILTER(WHERE status='unpaid'),0)::bigint,COALESCE(sum(penalty) FILTER(WHERE status='unpaid'),0)::bigint FROM installments WHERE loan_id=$1`, loanID).Scan(&p, &i, &pen); err != nil {
		return 0, err
	}
	rate := flat
	if method == "declining" {
		rate = declining
	} else if method == "annuity" {
		rate = annuity
	}
	d := roundMoney(i, rate, 10000)
	if d > i {
		d = i
	}
	return p + i - d + pen + roundMoney(p, fee, 10000), nil
}

func (a *API) listEarlyRepayments(c *fiber.Ctx) error {
	if role(c) == "member" {
		return a.rows(c, `SELECT e.id,e.loan_id,e.quoted_amount,e.status,e.reference,e.reason,e.created_at,e.reviewed_at FROM early_repayments e WHERE e.member_id=$1 ORDER BY e.id DESC`, member(c))
	}
	if role(c) != "admin" && role(c) != "teller" {
		return fiber.ErrForbidden
	}
	return a.rows(c, `SELECT e.id,e.loan_id,e.member_id,u.name member_name,e.quoted_amount,e.status,e.reference,e.reason,e.created_at,e.reviewed_at FROM early_repayments e JOIN members m ON m.id=e.member_id JOIN users u ON u.id=m.user_id ORDER BY e.id DESC`)
}
func (a *API) verifyEarlyRepayment(c *fiber.Ctx) error {
	var b struct {
		Approve bool   `json:"approve"`
		Reason  string `json:"reason"`
	}
	if c.BodyParser(&b) != nil || strings.TrimSpace(b.Reason) == "" {
		return fiber.ErrBadRequest
	}
	tx, err := a.DB.Begin(c.Context())
	if err != nil {
		return err
	}
	defer tx.Rollback(c.Context())
	var loanID, amount, mid int64
	var status string
	err = tx.QueryRow(c.Context(), `SELECT loan_id,quoted_amount,member_id,status FROM early_repayments WHERE id=$1 FOR UPDATE`, id(c)).Scan(&loanID, &amount, &mid, &status)
	if err == pgx.ErrNoRows {
		return fiber.ErrNotFound
	}
	if err != nil {
		return err
	}
	if status != "pending" {
		return fail(c, 409, "already_processed", "Permintaan sudah diproses")
	}
	next := "rejected"
	if b.Approve {
		next = "approved"
		if _, err = tx.Exec(c.Context(), `UPDATE early_repayments SET status=$1,reason=$2,reviewed_by=$3,reviewed_at=now() WHERE id=$4`, next, b.Reason, actor(c), id(c)); err != nil {
			return err
		}
		if err = audit(c.Context(), tx, actor(c), "early_repayment_reviewed", "early_repayments", id(c), fiber.Map{"status": status}, fiber.Map{"status": next, "amount": amount, "reason": b.Reason}); err != nil {
			return err
		}
		if err = tx.Commit(c.Context()); err != nil {
			return err
		}
		return ok(c, fiber.Map{"id": id(c), "status": next, "approved_amount": amount})
	}
	if _, err = tx.Exec(c.Context(), `UPDATE early_repayments SET status=$1,reason=$2,reviewed_by=$3,reviewed_at=now() WHERE id=$4`, next, b.Reason, actor(c), id(c)); err != nil {
		return err
	}
	if err = audit(c.Context(), tx, actor(c), "early_repayment_reviewed", "early_repayments", id(c), fiber.Map{"status": status}, fiber.Map{"status": next, "reason": b.Reason}); err != nil {
		return err
	}
	if err = tx.Commit(c.Context()); err != nil {
		return err
	}
	return ok(c, fiber.Map{"id": id(c), "status": next})
}

var _ = big.NewInt
