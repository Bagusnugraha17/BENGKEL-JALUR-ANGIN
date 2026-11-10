package api

import (
	"bytes"
	"context"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/jackc/pgx/v5"
)

type depositQuote struct {
	Principal      int64 `json:"principal"`
	Tenor          int   `json:"tenor_months"`
	AnnualRateBPS  int64 `json:"annual_rate_bps"`
	BonusRateBPS   int64 `json:"bonus_rate_bps"`
	GrossInterest  int64 `json:"gross_interest"`
	TaxBPS         int64 `json:"tax_bps"`
	TaxAmount      int64 `json:"tax_amount"`
	NetInterest    int64 `json:"net_interest"`
	MaturityAmount int64 `json:"maturity_amount"`
}

func (a *API) depositRoutes(r fiber.Router) {
	r.Get("/deposit-products", func(c *fiber.Ctx) error {
		if role(c) == "admin" {
			return a.rows(c, `SELECT * FROM deposit_products ORDER BY tenor_months,id`)
		}
		if role(c) == "committee" {
			return fail(c, 403, "forbidden", "Komite tidak mengelola produk deposito")
		}
		return a.rows(c, `SELECT id,name,tenor_months,annual_rate_bps,min_amount,configured,active FROM deposit_products WHERE active=true AND configured=true ORDER BY tenor_months`)
	})
	r.Post("/deposit-products", require("admin"), a.saveDepositProduct)
	r.Patch("/deposit-products/:id", require("admin"), a.saveDepositProduct)
	r.Get("/financial-rate-settings", require("admin"), func(c *fiber.Ctx) error {
		return a.rows(c, `SELECT id,savings_interest_bps,interest_tax_bps,updated_by,updated_at FROM financial_rate_settings WHERE id=1`)
	})
	r.Put("/financial-rate-settings", require("admin"), a.updateFinancialRates)
	r.Post("/deposits/simulate", require("member"), a.simulateDeposit)
	r.Get("/deposits", func(c *fiber.Ctx) error {
		switch role(c) {
		case "member":
			return a.rows(c, `SELECT d.id,d.member_id,d.product_id,p.name product_name,p.tenor_months,d.principal,d.gross_interest,d.tax_amount,d.net_interest,d.aro_type,d.status,d.reference,d.opened_at,d.maturity_date,d.created_at FROM deposits d JOIN deposit_products p ON p.id=d.product_id WHERE d.member_id=$1 ORDER BY d.id DESC`, member(c))
		case "teller":
			return a.rows(c, `SELECT d.id,d.member_id,u.name member_name,p.name product_name,p.tenor_months,d.principal,d.status,d.reference,d.maturity_date,d.created_at FROM deposits d JOIN members m ON m.id=d.member_id JOIN users u ON u.id=m.user_id JOIN deposit_products p ON p.id=d.product_id WHERE d.status IN ('pending','active') ORDER BY d.id DESC`)
		case "admin":
			return a.rows(c, `SELECT d.id,d.member_id,u.name member_name,p.name product_name,p.tenor_months,d.principal,d.gross_interest,d.tax_amount,d.net_interest,d.aro_type,d.status,d.reference,d.maturity_date,d.created_at FROM deposits d JOIN members m ON m.id=d.member_id JOIN users u ON u.id=m.user_id JOIN deposit_products p ON p.id=d.product_id ORDER BY d.id DESC`)
		default:
			return fail(c, 403, "forbidden", "Akses deposito tidak diizinkan")
		}
	})
	r.Post("/deposits", require("member"), a.createDeposit)
	r.Post("/deposits/:id/activate", require("teller", "admin"), a.activateDeposit)
	r.Post("/deposits/:id/maturity", require("teller", "admin"), a.processDepositMaturity)
	r.Get("/deposits/:id/certificate", a.depositCertificate)
}

func (a *API) saveDepositProduct(c *fiber.Ctx) error {
	var b struct {
		Name       string `json:"name"`
		Tenor      int    `json:"tenor_months"`
		Rate       *int64 `json:"annual_rate_bps"`
		MinAmount  int64  `json:"min_amount"`
		Configured bool   `json:"configured"`
		Active     bool   `json:"active"`
	}
	if c.BodyParser(&b) != nil || strings.TrimSpace(b.Name) == "" || !contains([]string{"1", "3", "6", "12", "24"}, fmt.Sprint(b.Tenor)) || b.MinAmount <= 0 || b.MinAmount > 1000000000000 {
		return fail(c, 400, "invalid_product", "Nama, tenor 1/3/6/12/24 bulan dan nominal minimum wajib valid")
	}
	if b.Rate != nil && (*b.Rate < 0 || *b.Rate > 100000) {
		return fail(c, 400, "invalid_rate", "Bunga tahunan tidak valid")
	}
	if b.Configured && b.Rate == nil {
		return fail(c, 400, "rate_required", "Bunga wajib diisi sebelum produk dikonfigurasi")
	}
	if b.Active && !b.Configured {
		return fail(c, 400, "product_not_configured", "Produk aktif harus dikonfigurasi terlebih dahulu")
	}
	tx, err := a.DB.Begin(c.Context())
	if err != nil {
		return err
	}
	defer tx.Rollback(c.Context())
	productID := id(c)
	if productID == 0 {
		err = tx.QueryRow(c.Context(), `INSERT INTO deposit_products(name,tenor_months,annual_rate_bps,min_amount,configured,active) VALUES($1,$2,$3,$4,$5,$6) RETURNING id`, strings.TrimSpace(b.Name), b.Tenor, b.Rate, b.MinAmount, b.Configured, b.Active).Scan(&productID)
	} else {
		_, err = tx.Exec(c.Context(), `UPDATE deposit_products SET name=$1,tenor_months=$2,annual_rate_bps=$3,min_amount=$4,configured=$5,active=$6,updated_at=now() WHERE id=$7`, strings.TrimSpace(b.Name), b.Tenor, b.Rate, b.MinAmount, b.Configured, b.Active, productID)
	}
	if err != nil {
		return err
	}
	if err = audit(c.Context(), tx, actor(c), "deposit_product_configured", "deposit_products", productID, nil, b); err != nil {
		return err
	}
	if err = tx.Commit(c.Context()); err != nil {
		return err
	}
	return ok(c, fiber.Map{"id": productID})
}

func (a *API) updateFinancialRates(c *fiber.Ctx) error {
	var b struct {
		SavingsInterest *int `json:"savings_interest_bps"`
		Tax             *int `json:"interest_tax_bps"`
	}
	if c.BodyParser(&b) != nil || b.SavingsInterest != nil && (*b.SavingsInterest < 0 || *b.SavingsInterest > 100000) || b.Tax != nil && (*b.Tax < 0 || *b.Tax > 10000) {
		return fail(c, 400, "invalid_rate", "Konfigurasi bunga atau pajak tidak valid")
	}
	tx, err := a.DB.Begin(c.Context())
	if err != nil {
		return err
	}
	defer tx.Rollback(c.Context())
	_, err = tx.Exec(c.Context(), `UPDATE financial_rate_settings SET savings_interest_bps=$1,interest_tax_bps=$2,updated_by=$3,updated_at=now() WHERE id=1`, b.SavingsInterest, b.Tax, actor(c))
	if err != nil {
		return err
	}
	if err = audit(c.Context(), tx, actor(c), "financial_rates_updated", "financial_rate_settings", 1, nil, b); err != nil {
		return err
	}
	if err = tx.Commit(c.Context()); err != nil {
		return err
	}
	return ok(c, fiber.Map{"updated": true})
}

func (a *API) quoteDeposit(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, memberID, productID, principal int64) (depositQuote, error) {
	var quote depositQuote
	quote.Principal = principal
	var configured, active bool
	var baseRate *int64
	var tenor int
	var minAmount int64
	err := q.QueryRow(ctx, `SELECT tenor_months,annual_rate_bps,min_amount,configured,active FROM deposit_products WHERE id=$1`, productID).Scan(&tenor, &baseRate, &minAmount, &configured, &active)
	if err != nil {
		return quote, err
	}
	if !configured || !active || baseRate == nil {
		return quote, fmt.Errorf("produk deposito belum dikonfigurasi atau tidak aktif")
	}
	if principal < minAmount || principal > 1000000000000 {
		return quote, fmt.Errorf("nominal deposito di bawah minimum atau di atas batas sistem")
	}
	var tax *int64
	err = q.QueryRow(ctx, `SELECT interest_tax_bps FROM financial_rate_settings WHERE id=1`).Scan(&tax)
	if err != nil {
		return quote, err
	}
	if tax == nil {
		return quote, fmt.Errorf("pajak bunga deposito belum dikonfigurasi")
	}
	var bonus int64
	err = q.QueryRow(ctx, `SELECT p.deposit_bonus_bps FROM member_tiers t JOIN member_tier_policies p ON p.tier=t.tier WHERE t.member_id=$1`, memberID).Scan(&bonus)
	if err == pgx.ErrNoRows {
		bonus = 0
	} else if err != nil {
		return quote, err
	}
	annual := *baseRate + bonus
	if annual > 100000 {
		return quote, fmt.Errorf("total bunga melebihi batas sistem")
	}
	gross := roundMoney(principal, annual*int64(tenor), 120000)
	taxAmount := roundMoney(gross, *tax, 10000)
	net := gross - taxAmount
	quote.Tenor = tenor
	quote.AnnualRateBPS = *baseRate
	quote.BonusRateBPS = bonus
	quote.GrossInterest = gross
	quote.TaxBPS = *tax
	quote.TaxAmount = taxAmount
	quote.NetInterest = net
	quote.MaturityAmount = principal + net
	return quote, nil
}

func (a *API) simulateDeposit(c *fiber.Ctx) error {
	var b struct {
		ProductID int64 `json:"product_id"`
		Amount    int64 `json:"amount"`
	}
	if c.BodyParser(&b) != nil || b.ProductID <= 0 || b.Amount <= 0 {
		return fiber.ErrBadRequest
	}
	q, err := a.quoteDeposit(c.Context(), a.DB, member(c), b.ProductID, b.Amount)
	if err != nil {
		return fail(c, 422, "deposit_not_configured", err.Error())
	}
	return ok(c, q)
}

func (a *API) createDeposit(c *fiber.Ctx) error {
	var b struct {
		ProductID int64  `json:"product_id"`
		Amount    int64  `json:"amount"`
		AROType   string `json:"aro_type"`
		Reference string `json:"reference"`
	}
	if c.BodyParser(&b) != nil || b.ProductID <= 0 || b.Amount <= 0 || !contains([]string{"non_aro", "aro_principal", "aro_principal_interest"}, b.AROType) || len(b.Reference) > 120 {
		return fiber.ErrBadRequest
	}
	if err := a.active(c, member(c)); err != nil {
		return err
	}
	tx, err := a.DB.Begin(c.Context())
	if err != nil {
		return err
	}
	defer tx.Rollback(c.Context())
	quote, err := a.quoteDeposit(c.Context(), tx, member(c), b.ProductID, b.Amount)
	if err != nil {
		return fail(c, 422, "deposit_not_configured", err.Error())
	}
	if strings.TrimSpace(b.Reference) == "" {
		b.Reference = "DPT-" + randomID()[:24]
	}
	var depositID int64
	err = tx.QueryRow(c.Context(), `INSERT INTO deposits(member_id,product_id,principal,gross_interest,tax_amount,net_interest,aro_type,reference) VALUES($1,$2,$3,$4,$5,$6,$7,$8) RETURNING id`, member(c), b.ProductID, b.Amount, quote.GrossInterest, quote.TaxAmount, quote.NetInterest, b.AROType, b.Reference).Scan(&depositID)
	if err != nil {
		return err
	}
	if err = audit(c.Context(), tx, actor(c), "deposit_submitted", "deposits", depositID, nil, fiber.Map{"principal": b.Amount, "product_id": b.ProductID, "aro_type": b.AROType}); err != nil {
		return err
	}
	if _, err = tx.Exec(c.Context(), `INSERT INTO notifications(user_id,title,message) SELECT id,'Permohonan deposito baru','Terdapat pembukaan deposito yang menunggu pemeriksaan teller.' FROM users WHERE role='teller' AND active`); err != nil {
		return err
	}
	if err = tx.Commit(c.Context()); err != nil {
		return err
	}
	return ok(c, fiber.Map{"id": depositID, "status": "pending", "quote": quote})
}

func (a *API) activateDeposit(c *fiber.Ctx) error {
	tx, err := a.DB.Begin(c.Context())
	if err != nil {
		return err
	}
	defer tx.Rollback(c.Context())
	var mid, principal int64
	var status, reference string
	err = tx.QueryRow(c.Context(), `SELECT member_id,principal,status,reference FROM deposits WHERE id=$1 FOR UPDATE`, id(c)).Scan(&mid, &principal, &status, &reference)
	if err == pgx.ErrNoRows {
		return fiber.ErrNotFound
	}
	if err != nil {
		return err
	}
	if status != "pending" {
		return fail(c, 409, "deposit_not_pending", "Deposito tidak lagi menunggu aktivasi")
	}
	if err = a.active(c, mid); err != nil {
		return err
	}
	var tenor int
	err = tx.QueryRow(c.Context(), `SELECT tenor_months FROM deposit_products WHERE id=(SELECT product_id FROM deposits WHERE id=$1)`, id(c)).Scan(&tenor)
	if err != nil {
		return err
	}
	loc := makassarLocation()
	now := time.Now().In(loc)
	maturity := now.AddDate(0, tenor, 0)
	if _, err = tx.Exec(c.Context(), `UPDATE deposits SET status='active',opened_at=$1,maturity_date=$2 WHERE id=$3`, now, maturity, id(c)); err != nil {
		return err
	}
	if _, err = tx.Exec(c.Context(), `INSERT INTO deposit_transactions(deposit_id,kind,amount,reference,created_by) VALUES($1,'placement',$2,$3,$4)`, id(c), principal, "deposit-placement:"+fmt.Sprint(id(c)), actor(c)); err != nil {
		return err
	}
	if err = journal(c.Context(), tx, "deposit-placement:"+fmt.Sprint(id(c)), "cash", "deposit_liability", principal, mid, "Penempatan deposito"); err != nil {
		return err
	}
	if err = refreshMemberTier(c.Context(), tx, mid, "Aktivasi deposito"); err != nil {
		return err
	}
	if _, err = tx.Exec(c.Context(), `INSERT INTO notifications(user_id,title,message) SELECT user_id,'Deposito aktif',$1 FROM members WHERE id=$2`, "Deposito telah diaktifkan. Jatuh tempo: "+maturity.Format("02-01-2006"), mid); err != nil {
		return err
	}
	if err = audit(c.Context(), tx, actor(c), "deposit_activated", "deposits", id(c), fiber.Map{"status": status}, fiber.Map{"status": "active", "maturity_date": maturity}); err != nil {
		return err
	}
	if err = tx.Commit(c.Context()); err != nil {
		return err
	}
	return ok(c, fiber.Map{"id": id(c), "status": "active", "maturity_date": maturity})
}

func (a *API) processDepositMaturity(c *fiber.Ctx) error {
	tx, err := a.DB.Begin(c.Context())
	if err != nil {
		return err
	}
	defer tx.Rollback(c.Context())
	var mid, productID, principal, gross, tax, net int64
	var status, aro string
	var maturity time.Time
	var reference string
	err = tx.QueryRow(c.Context(), `SELECT member_id,product_id,principal,gross_interest,tax_amount,net_interest,status,aro_type,maturity_date,reference FROM deposits WHERE id=$1 FOR UPDATE`, id(c)).Scan(&mid, &productID, &principal, &gross, &tax, &net, &status, &aro, &maturity, &reference)
	if err == pgx.ErrNoRows {
		return fiber.ErrNotFound
	}
	if err != nil {
		return err
	}
	if status != "active" {
		return fail(c, 409, "deposit_not_active", "Deposito tidak aktif")
	}
	var locked int64
	if err = tx.QueryRow(c.Context(), `SELECT COALESCE(sum(locked_amount),0)::bigint FROM collateral_locks WHERE asset_type='deposit' AND asset_id=$1 AND status='active'`, id(c)).Scan(&locked); err != nil {
		return err
	}
	if locked > 0 {
		return fail(c, 409, "deposit_locked_as_collateral", "Deposito sedang dijaminkan dan tidak dapat dicairkan sebelum pinjaman lunas")
	}
	loc := makassarLocation()
	now := time.Now().In(loc)
	if now.Before(localDueDate(maturity, loc)) {
		return fail(c, 409, "deposit_not_matured", "Deposito belum jatuh tempo")
	}
	if gross != net+tax {
		return fail(c, 409, "deposit_interest_mismatch", "Rincian bunga tidak seimbang; periksa konfigurasi")
	}
	var tenor int
	if err = tx.QueryRow(c.Context(), `SELECT tenor_months FROM deposit_products WHERE id=$1`, productID).Scan(&tenor); err != nil {
		return err
	}
	cycleRef := fmt.Sprintf("deposit-maturity:%d:%s", id(c), maturity.Format("20060102"))
	if aro == "non_aro" {
		if err = journal(c.Context(), tx, cycleRef+":principal", "deposit_liability", "cash", principal, mid, "Pembayaran pokok deposito jatuh tempo"); err != nil {
			return err
		}
		if err = journal(c.Context(), tx, cycleRef+":interest", "deposit_interest_expense", "cash", net, mid, "Pembayaran bunga bersih deposito"); err != nil {
			return err
		}
		if err = journal(c.Context(), tx, cycleRef+":tax", "deposit_interest_expense", "tax_payable", tax, mid, "Pajak bunga deposito"); err != nil {
			return err
		}
		if _, err = tx.Exec(c.Context(), `UPDATE deposits SET status='matured',paid_at=now() WHERE id=$1`, id(c)); err != nil {
			return err
		}
		if _, err = tx.Exec(c.Context(), `INSERT INTO deposit_transactions(deposit_id,kind,amount,reference,created_by) VALUES($1,'maturity_payout',$2,$3,$4)`, id(c), principal+net, cycleRef, actor(c)); err != nil {
			return err
		}
	} else {
		if err = journal(c.Context(), tx, cycleRef+":interest", "deposit_interest_expense", "deposit_liability", net, mid, "Bunga bersih deposito ARO"); err != nil {
			return err
		}
		if err = journal(c.Context(), tx, cycleRef+":tax", "deposit_interest_expense", "tax_payable", tax, mid, "Pajak bunga deposito ARO"); err != nil {
			return err
		}
		newPrincipal := principal
		if aro == "aro_principal_interest" {
			newPrincipal += net
		}
		quote, quoteErr := a.quoteDeposit(c.Context(), tx, mid, productID, newPrincipal)
		if quoteErr != nil {
			return fail(c, 422, "deposit_rollover_not_configured", quoteErr.Error())
		}
		newMaturity := now.AddDate(0, tenor, 0)
		if _, err = tx.Exec(c.Context(), `UPDATE deposits SET principal=$1,gross_interest=$2,tax_amount=$3,net_interest=$4,opened_at=$5,maturity_date=$6 WHERE id=$7`, newPrincipal, quote.GrossInterest, quote.TaxAmount, quote.NetInterest, now, newMaturity, id(c)); err != nil {
			return err
		}
		if _, err = tx.Exec(c.Context(), `INSERT INTO deposit_transactions(deposit_id,kind,amount,reference,created_by) VALUES($1,'interest_rollover',$2,$3,$4)`, id(c), net, cycleRef, actor(c)); err != nil {
			return err
		}
	}
	if err = refreshMemberTier(c.Context(), tx, mid, "Pemrosesan jatuh tempo deposito"); err != nil {
		return err
	}
	if err = audit(c.Context(), tx, actor(c), "deposit_maturity_processed", "deposits", id(c), fiber.Map{"status": status, "principal": principal, "gross_interest": gross, "tax_amount": tax, "net_interest": net, "aro_type": aro}, fiber.Map{"status": func() string {
		if aro == "non_aro" {
			return "matured"
		}
		return "active"
	}()}); err != nil {
		return err
	}
	if err = tx.Commit(c.Context()); err != nil {
		return err
	}
	return ok(c, fiber.Map{"id": id(c), "processed": true, "aro_type": aro})
}

func (a *API) depositCertificate(c *fiber.Ctx) error {
	var owner int64
	var name, reference, aro, status string
	var principal, gross, tax, net int64
	var opened, maturity *time.Time
	err := a.DB.QueryRow(c.Context(), `SELECT d.member_id,u.name,d.reference,d.aro_type,d.status,d.principal,d.gross_interest,d.tax_amount,d.net_interest,d.opened_at,d.maturity_date FROM deposits d JOIN members m ON m.id=d.member_id JOIN users u ON u.id=m.user_id WHERE d.id=$1`, id(c)).Scan(&owner, &name, &reference, &aro, &status, &principal, &gross, &tax, &net, &opened, &maturity)
	if err == pgx.ErrNoRows {
		return fiber.ErrNotFound
	}
	if err != nil {
		return err
	}
	if role(c) != "admin" && (role(c) != "member" || owner != member(c)) {
		return fail(c, 403, "forbidden", "Tidak berhak melihat E-Bilyet ini")
	}
	if status != "active" && status != "matured" {
		return fail(c, 409, "certificate_unavailable", "E-Bilyet tersedia setelah deposito aktif")
	}
	openedText := "-"
	maturityText := "-"
	if opened != nil {
		openedText = opened.In(makassarLocation()).Format("02-01-2006")
	}
	if maturity != nil {
		maturityText = maturity.Format("02-01-2006")
	}
	pdf := simplePDF([]string{"KOPLINK - E-BILYET DEPOSITO", "Pemilik: " + name, "Referensi: " + reference, "Pokok: Rp " + formatRupiah(principal), "Bunga bruto: Rp " + formatRupiah(gross), "Pajak: Rp " + formatRupiah(tax), "Bunga bersih: Rp " + formatRupiah(net), "Instruksi ARO: " + aro, "Tanggal pembukaan: " + openedText, "Jatuh tempo: " + maturityText, "Status: " + status, "Dokumen elektronik berdasarkan catatan deposito KopLink."})
	c.Set("Content-Type", "application/pdf")
	c.Set("Content-Disposition", fmt.Sprintf("attachment; filename=koplink-e-bilyet-%d.pdf", id(c)))
	return c.Send(pdf)
}

func roundMoney(a, b, d int64) int64 {
	if a < 0 || b < 0 || d <= 0 {
		return 0
	}
	x := new(big.Int).Mul(big.NewInt(a), big.NewInt(b))
	x.Add(x, big.NewInt(d/2))
	x.Div(x, big.NewInt(d))
	if !x.IsInt64() {
		return 0
	}
	return x.Int64()
}
func simplePDF(lines []string) []byte {
	var content bytes.Buffer
	content.WriteString("BT\n/F1 11 Tf\n50 790 Td\n15 TL\n")
	for _, line := range lines {
		safe := strings.NewReplacer("\\", "\\\\", "(", "\\(", ")", "\\)", "\r", " ", "\n", " ").Replace(line)
		content.WriteString("(" + safe + ") Tj\nT*\n")
	}
	content.WriteString("ET")
	stream := content.String()
	objects := []string{"<< /Type /Catalog /Pages 2 0 R >>", "<< /Type /Pages /Kids [3 0 R] /Count 1 >>", "<< /Type /Page /Parent 2 0 R /MediaBox [0 0 595 842] /Resources << /Font << /F1 4 0 R >> >> /Contents 5 0 R >>", "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>", fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(stream), stream)}
	var out bytes.Buffer
	out.WriteString("%PDF-1.4\n")
	offsets := []int{0}
	for i, obj := range objects {
		offsets = append(offsets, out.Len())
		fmt.Fprintf(&out, "%d 0 obj\n%s\nendobj\n", i+1, obj)
	}
	xref := out.Len()
	fmt.Fprintf(&out, "xref\n0 %d\n0000000000 65535 f \n", len(offsets))
	for _, off := range offsets[1:] {
		fmt.Fprintf(&out, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&out, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF", len(offsets), xref)
	return out.Bytes()
}

func formatRupiah(v int64) string {
	s := fmt.Sprintf("%d", v)
	if len(s) <= 3 {
		return s
	}
	var out strings.Builder
	for i, ch := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			out.WriteByte('.')
		}
		out.WriteRune(ch)
	}
	return out.String()
}
