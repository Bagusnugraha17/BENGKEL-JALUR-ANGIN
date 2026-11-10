package api

import (
	"bytes"
	"context"
	"encoding/csv"
	"fmt"
	"github.com/gofiber/fiber/v2"
	"github.com/jackc/pgx/v5"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"time"
)

type reportQuerier interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

func reportRows(ctx context.Context, q reportQuerier, sql string, args ...any) ([]map[string]any, error) {
	r, e := q.Query(ctx, sql, args...)
	if e != nil {
		return nil, e
	}
	defer r.Close()
	out := []map[string]any{}
	for r.Next() {
		vals, e := r.Values()
		if e != nil {
			return nil, e
		}
		m := map[string]any{}
		for i, f := range r.FieldDescriptions() {
			m[f.Name] = vals[i]
		}
		out = append(out, m)
	}
	return out, r.Err()
}
func csvSafe(s string) string {
	trimmed := strings.TrimLeft(s, " \t\r\n")
	if (len(s) > 0 && strings.ContainsRune("\t\r\n", rune(s[0]))) || (len(trimmed) > 0 && strings.ContainsRune("=+-@", rune(trimmed[0]))) {
		return "'" + s
	}
	return s
}

// Largest remainders preserve every rupiah; ties use member order, deterministically.
func allocate(total int64, weights []int64) []int64 {
	out := make([]int64, len(weights))
	sum := new(big.Int)
	for _, w := range weights {
		if w > 0 {
			sum.Add(sum, big.NewInt(w))
		}
	}
	if sum.Sign() == 0 || total <= 0 {
		return out
	}
	type remainder struct {
		i int
		r *big.Int
	}
	remainders := []remainder{}
	var used int64
	for i, w := range weights {
		if w <= 0 {
			continue
		}
		n := new(big.Int).Mul(big.NewInt(total), big.NewInt(w))
		q, r := new(big.Int), new(big.Int)
		q.QuoRem(n, sum, r)
		out[i] = q.Int64()
		used += out[i]
		remainders = append(remainders, remainder{i, r})
	}
	sort.SliceStable(remainders, func(i, j int) bool { return remainders[i].r.Cmp(remainders[j].r) > 0 })
	for i := int64(0); i < total-used; i++ {
		out[remainders[i%int64(len(remainders))].i]++
	}
	return out
}
func reportNow() time.Time { loc, _ := time.LoadLocation("Asia/Makassar"); return time.Now().In(loc) }
func reportDates(c *fiber.Ctx) (time.Time, time.Time, error) {
	loc, _ := time.LoadLocation("Asia/Makassar")
	now := time.Now().In(loc)
	from := c.Query("from", fmt.Sprintf("%d-01-01", now.Year()))
	to := c.Query("to", now.Format("2006-01-02"))
	a, e := time.ParseInLocation("2006-01-02", from, loc)
	if e != nil {
		return a, a, e
	}
	b, e := time.ParseInLocation("2006-01-02", to, loc)
	if e != nil || b.Before(a) {
		return a, b, fmt.Errorf("Rentang tanggal tidak valid")
	}
	return a, b.AddDate(0, 0, 1), nil
}
func (a *API) reporting(r fiber.Router) {
	r.Get("/settings", require("admin"), func(c *fiber.Ctx) error {
		x, e := reportRows(c.Context(), a.DB, "SELECT * FROM settings WHERE id=1")
		if e != nil {
			return e
		}
		return ok(c, x[0])
	})
	r.Patch("/settings", require("admin"), a.updateSettings)
	r.Post("/expenses", require("admin"), a.createExpense)
	r.Get("/reports", require("admin"), a.report)
	r.Get("/reports/export", require("admin"), a.exportReport)
	r.Get("/shu-periods", func(c *fiber.Ctx) error {
		if role(c) != "member" && role(c) != "admin" {
			return fiber.ErrForbidden
		}
		q := "SELECT p.*,COALESCE((SELECT sum(d.total)::bigint FROM shu_distributions d WHERE d.period_id=p.id),0) AS allocated_amount,p.distributable-COALESCE((SELECT sum(d.total)::bigint FROM shu_distributions d WHERE d.period_id=p.id),0) AS retained_amount FROM shu_periods p"
		if role(c) == "member" {
			q += " WHERE status IN ('finalized','distributed')"
		}
		x, e := reportRows(c.Context(), a.DB, q+" ORDER BY year DESC")
		if e != nil {
			return e
		}
		return ok(c, x)
	})
	r.Post("/shu-periods", require("admin"), a.createSHU)
	r.Get("/shu-periods/:id", require("member", "admin"), a.readSHU)
	for _, action := range []string{"calculate", "review", "finalize", "distribute"} {
		action := action
		r.Post("/shu-periods/:id/"+action, require("admin"), func(c *fiber.Ctx) error { return a.changeSHU(c, action) })
	}
}
func (a *API) updateSettings(c *fiber.Ctx) error {
	var v struct {
		Configured bool   `json:"shu_configured"`
		Capital    int    `json:"shu_capital_bps"`
		Business   int    `json:"shu_business_bps"`
		Basis      string `json:"shu_basis"`
	}
	if c.BodyParser(&v) != nil || v.Capital < 0 || v.Business < 0 || v.Capital > 10000 || v.Business > 10000 || (v.Configured && (v.Capital+v.Business != 10000 || v.Basis != "closing_balance_paid_interest")) {
		return fail(c, 400, "validation", "Bobot SHU harus berjumlah 10000 bps dan basis saldo akhir/bunga dibayar")
	}
	ctx := c.Context()
	tx, e := a.DB.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	before, e := reportRows(ctx, tx, "SELECT * FROM settings WHERE id=1 FOR UPDATE")
	if e != nil {
		return e
	}
	_, e = tx.Exec(ctx, "UPDATE settings SET shu_configured=$1,shu_capital_bps=$2,shu_business_bps=$3,shu_basis=$4 WHERE id=1", v.Configured, v.Capital, v.Business, v.Basis)
	if e != nil {
		return e
	}
	if e = audit(ctx, tx, actor(c), "update", "settings", 1, before, v); e != nil {
		return e
	}
	if e = tx.Commit(ctx); e != nil {
		return e
	}
	return ok(c, v)
}
func (a *API) createExpense(c *fiber.Ctx) error {
	var v struct {
		Amount      int64  `json:"amount"`
		Description string `json:"description"`
		Date        string `json:"date"`
		Reference   string `json:"reference"`
	}
	if c.BodyParser(&v) != nil || v.Amount <= 0 || v.Amount > 1000000000000 || strings.TrimSpace(v.Description) == "" || strings.TrimSpace(v.Reference) == "" {
		return fail(c, 400, "validation", "Nominal, keterangan dan referensi wajib valid")
	}
	date, e := time.Parse("2006-01-02", v.Date)
	if e != nil || date.After(time.Now()) {
		return fail(c, 400, "validation", "Tanggal biaya tidak valid")
	}
	ctx := c.Context()
	tx, e := a.DB.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	_, e = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(792002)")
	if e != nil {
		return e
	}
	var closed bool
	if e = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM shu_periods WHERE year=$1 AND status<>'draft')", date.Year()).Scan(&closed); e != nil {
		return e
	}
	if closed {
		return fail(c, 409, "closed_period", "Periode sudah ditutup untuk perhitungan SHU")
	}
	var id int64
	if e = tx.QueryRow(ctx, "INSERT INTO expenses(amount,description,date,reference) VALUES($1,$2,$3,$4) RETURNING id", v.Amount, v.Description, date, v.Reference).Scan(&id); e != nil {
		return e
	}
	if _, e = tx.Exec(ctx, "UPDATE shu_periods SET capital_bps=NULL,business_bps=NULL WHERE year=$1 AND status='draft'", date.Year()); e != nil {
		return e
	}
	if _, e = tx.Exec(ctx, "DELETE FROM shu_distributions WHERE period_id IN (SELECT id FROM shu_periods WHERE year=$1 AND status='draft')", date.Year()); e != nil {
		return e
	}
	if e = journal(ctx, tx, "expense:"+strconv.FormatInt(id, 10), "expenses", "cash", v.Amount, 0, v.Description); e != nil {
		return e
	}
	if e = audit(ctx, tx, actor(c), "create", "expenses", id, nil, v); e != nil {
		return e
	}
	if e = tx.Commit(ctx); e != nil {
		return e
	}
	return ok(c, fiber.Map{"id": id})
}
func (a *API) report(c *fiber.Ctx) error {
	from, to, e := reportDates(c)
	if e != nil {
		return fail(c, 400, "validation", e.Error())
	}
	ctx := c.Context()
	tx, e := a.DB.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	out := fiber.Map{}
	queries := map[string]string{
		"expenses":  "SELECT * FROM expenses WHERE date >= $1::date AND date < $2::date ORDER BY id",
		"ledger":    "SELECT * FROM ledger WHERE created_at >= $1 AND created_at < $2 ORDER BY id",
		"savings":   "SELECT st.*,u.name AS member_name,t.name AS type_name FROM savings_transactions st JOIN members m ON m.id=st.member_id JOIN users u ON u.id=m.user_id JOIN savings_types t ON t.id=st.type_id WHERE st.created_at >= $1 AND st.created_at < $2 ORDER BY st.id",
		"loans":     "SELECT l.*,u.name AS member_name,COALESCE((SELECT sum(i.principal+i.interest+i.penalty) FROM installments i WHERE i.loan_id=l.id AND i.status='unpaid'),0)::bigint outstanding FROM loans l JOIN members m ON m.id=l.member_id JOIN users u ON u.id=m.user_id WHERE disbursed_at >= $1 AND disbursed_at < $2 ORDER BY l.id",
		"payments":  "SELECT p.*,l.member_id,u.name AS member_name FROM payments p JOIN installments i ON i.id=p.installment_id JOIN loans l ON l.id=i.loan_id JOIN members m ON m.id=l.member_id JOIN users u ON u.id=m.user_id WHERE p.created_at >= $1 AND p.created_at < $2 ORDER BY p.id",
		"deposits":  "SELECT d.*,u.name AS member_name,p.name AS product_name FROM deposits d JOIN members m ON m.id=d.member_id JOIN users u ON u.id=m.user_id JOIN deposit_products p ON p.id=d.product_id WHERE d.created_at >= $1 AND d.created_at < $2 ORDER BY d.id",
		"members":   "SELECT m.id,m.number,m.status,m.created_at,u.name FROM members m JOIN users u ON u.id=m.user_id WHERE m.created_at >= $1 AND m.created_at < $2 ORDER BY m.id",
		"penalties": "SELECT i.*,l.member_id,(i.principal+i.interest+i.penalty) total,u.name AS member_name FROM installments i JOIN loans l ON l.id=i.loan_id JOIN members m ON m.id=l.member_id JOIN users u ON u.id=m.user_id WHERE i.due_date >= $1::date AND i.due_date < $2::date AND i.penalty>0 ORDER BY i.id",
		"shu":       "SELECT * FROM shu_periods WHERE year>=extract(year FROM $1::date) AND year<=extract(year FROM ($2::date-1)) ORDER BY year"}
	for k, q := range queries {
		x, e := reportRows(ctx, tx, q, from, to)
		if e != nil {
			return e
		}
		out[k] = x
	}
	s, e := reportRows(ctx, tx, `SELECT COALESCE(sum(credit-debit) FILTER(WHERE account='interest_income'),0)::bigint AS interest_income,COALESCE(sum(credit-debit) FILTER(WHERE account='penalty_income'),0)::bigint AS penalty_income,COALESCE(sum(debit-credit) FILTER(WHERE account='expenses'),0)::bigint AS expenses,COALESCE(sum(debit),0)::bigint AS total_debit,COALESCE(sum(credit),0)::bigint AS total_credit FROM ledger WHERE created_at >= $1 AND created_at < $2`, from, to)
	if e != nil {
		return e
	}
	var cost int64
	if e = tx.QueryRow(ctx, "SELECT COALESCE(sum(amount),0)::bigint FROM expenses WHERE date >= $1::date AND date < $2::date", from, to).Scan(&cost); e != nil {
		return e
	}
	s[0]["expenses"] = cost
	var depositPrincipal, depositCount, loanOutstanding int64
	if e = tx.QueryRow(ctx, `SELECT COALESCE(sum(principal),0)::bigint,count(*) FROM deposits WHERE status='active' AND created_at<$1`, to).Scan(&depositPrincipal, &depositCount); e != nil {
		return e
	}
	if e = tx.QueryRow(ctx, `SELECT COALESCE(sum(i.principal+i.interest+i.penalty),0)::bigint FROM installments i JOIN loans l ON l.id=i.loan_id WHERE i.status='unpaid'`).Scan(&loanOutstanding); e != nil {
		return e
	}
	s[0]["active_deposit_principal"] = depositPrincipal
	s[0]["active_deposit_count"] = depositCount
	s[0]["loan_outstanding_all_periods"] = loanOutstanding
	out["summary"] = s[0]
	if e = tx.Commit(ctx); e != nil {
		return e
	}
	return ok(c, out)
}
func (a *API) exportReport(c *fiber.Ctx) error {
	if c.Query("format", "csv") != "csv" {
		return fail(c, 400, "validation", "Format yang tersedia: csv")
	}
	from, to, e := reportDates(c)
	if e != nil {
		return fail(c, 400, "validation", e.Error())
	}
	rows, e := reportRows(c.Context(), a.DB, "SELECT id,reference,account,debit,credit,COALESCE(member_id,0) AS member_id,description,created_at FROM ledger WHERE created_at >= $1 AND created_at < $2 ORDER BY id", from, to)
	if e != nil {
		return e
	}
	var b bytes.Buffer
	w := csv.NewWriter(&b)
	keys := []string{"id", "reference", "account", "debit", "credit", "member_id", "description", "created_at"}
	w.Write(keys)
	for _, row := range rows {
		line := make([]string, len(keys))
		for i, k := range keys {
			line[i] = csvSafe(fmt.Sprint(row[k]))
		}
		w.Write(line)
	}
	w.Flush()
	if e = w.Error(); e != nil {
		return e
	}
	c.Set("Content-Type", "text/csv; charset=utf-8")
	c.Set("Content-Disposition", "attachment; filename=koplink-ledger.csv")
	return c.Send(b.Bytes())
}
func (a *API) createSHU(c *fiber.Ctx) error {
	var v struct {
		Year int `json:"year"`
	}
	if c.BodyParser(&v) != nil || v.Year < 2000 || v.Year >= reportNow().Year() {
		return fail(c, 400, "validation", "SHU hanya untuk tahun yang telah berakhir")
	}
	ctx := c.Context()
	tx, e := a.DB.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	var id int64
	if e = tx.QueryRow(ctx, "INSERT INTO shu_periods(year) VALUES($1) RETURNING id", v.Year).Scan(&id); e != nil {
		return e
	}
	if e = audit(ctx, tx, actor(c), "create", "shu_periods", id, nil, v); e != nil {
		return e
	}
	if e = tx.Commit(ctx); e != nil {
		return e
	}
	return ok(c, fiber.Map{"id": id, "year": v.Year, "status": "draft"})
}
func (a *API) readSHU(c *fiber.Ctx) error {
	id, e := strconv.ParseInt(c.Params("id"), 10, 64)
	if e != nil {
		return fail(c, 400, "validation", "ID tidak valid")
	}
	ctx := c.Context()
	p, e := reportRows(ctx, a.DB, "SELECT p.*,COALESCE((SELECT sum(d.total)::bigint FROM shu_distributions d WHERE d.period_id=p.id),0) AS allocated_amount,p.distributable-COALESCE((SELECT sum(d.total)::bigint FROM shu_distributions d WHERE d.period_id=p.id),0) AS retained_amount FROM shu_periods p WHERE p.id=$1", id)
	if e != nil {
		return e
	}
	if len(p) == 0 {
		return fail(c, 404, "not_found", "Periode tidak ditemukan")
	}
	if role(c) == "member" && p[0]["status"] != "finalized" && p[0]["status"] != "distributed" {
		return fail(c, 403, "forbidden", "Periode belum dipublikasikan")
	}
	q := "SELECT d.*,u.name AS member_name FROM shu_distributions d JOIN members m ON m.id=d.member_id JOIN users u ON u.id=m.user_id WHERE d.period_id=$1"
	args := []any{id}
	if role(c) == "member" {
		q += " AND d.member_id=$2"
		args = append(args, member(c))
	}
	d, e := reportRows(ctx, a.DB, q+" ORDER BY d.member_id", args...)
	if e != nil {
		return e
	}
	return ok(c, fiber.Map{"period": p[0], "distributions": d})
}
func (a *API) changeSHU(c *fiber.Ctx, action string) error {
	id, e := strconv.ParseInt(c.Params("id"), 10, 64)
	if e != nil {
		return fail(c, 400, "validation", "ID tidak valid")
	}
	ctx := c.Context()
	tx, e := a.DB.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	if _, e = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(792002)"); e != nil {
		return e
	}
	var year int
	var status string
	var calculated *int
	e = tx.QueryRow(ctx, "SELECT year,status,capital_bps FROM shu_periods WHERE id=$1 FOR UPDATE", id).Scan(&year, &status, &calculated)
	if e == pgx.ErrNoRows {
		return fail(c, 404, "not_found", "Periode tidak ditemukan")
	}
	if e != nil {
		return e
	}
	expected := map[string]string{"calculate": "draft", "review": "draft", "finalize": "reviewed", "distribute": "finalized"}
	if status != expected[action] {
		return fail(c, 409, "invalid_state", "Tahap SHU tidak sesuai atau aksi sudah dilakukan")
	}
	if action == "calculate" {
		if year >= reportNow().Year() {
			return fail(c, 409, "open_period", "Tahun belum berakhir")
		}
		var configured bool
		var capital, business int
		var basis string
		if e = tx.QueryRow(ctx, "SELECT shu_configured,shu_capital_bps,shu_business_bps,shu_basis FROM settings WHERE id=1 FOR SHARE").Scan(&configured, &capital, &business, &basis); e != nil {
			return e
		}
		if !configured || capital+business != 10000 || basis != "closing_balance_paid_interest" {
			return fail(c, 409, "unconfigured", "Konfigurasi SHU belum ditetapkan")
		}
		loc, _ := time.LoadLocation("Asia/Makassar")
		start := time.Date(year, 1, 1, 0, 0, 0, 0, loc)
		end := start.AddDate(1, 0, 0)
		var income, costs int64
		if e = tx.QueryRow(ctx, "SELECT COALESCE(sum(credit-debit),0)::bigint FROM ledger WHERE account IN ('interest_income','penalty_income') AND created_at >= $1 AND created_at < $2", start, end).Scan(&income); e != nil {
			return e
		}
		if e = tx.QueryRow(ctx, "SELECT COALESCE(sum(amount),0)::bigint FROM expenses WHERE date >= $1::date AND date < $2::date", start, end).Scan(&costs); e != nil {
			return e
		}
		available := income - costs
		if available < 0 {
			available = 0
		}
		rr, e := tx.Query(ctx, `SELECT m.id,COALESCE((SELECT greatest(sum(credit-debit),0)::bigint FROM ledger WHERE member_id=m.id AND account='savings' AND created_at<$2),0),COALESCE((SELECT sum(i.interest)::bigint FROM payments p JOIN installments i ON i.id=p.installment_id JOIN loans l ON l.id=i.loan_id WHERE l.member_id=m.id AND p.status='approved' AND p.verified_at >= $1 AND p.verified_at<$2),0) FROM members m WHERE m.created_at<$2 ORDER BY m.id`, start, end)
		if e != nil {
			return e
		}
		ids, cw, bw := []int64{}, []int64{}, []int64{}
		for rr.Next() {
			var m, cap, bus int64
			if e = rr.Scan(&m, &cap, &bus); e != nil {
				rr.Close()
				return e
			}
			ids = append(ids, m)
			cw = append(cw, cap)
			bw = append(bw, bus)
		}
		e = rr.Err()
		rr.Close()
		if e != nil {
			return e
		}
		capPool := new(big.Int).Quo(new(big.Int).Mul(big.NewInt(available), big.NewInt(int64(capital))), big.NewInt(10000)).Int64()
		ca, ba := allocate(capPool, cw), allocate(available-capPool, bw)
		if _, e = tx.Exec(ctx, "DELETE FROM shu_distributions WHERE period_id=$1", id); e != nil {
			return e
		}
		for i, m := range ids {
			if _, e = tx.Exec(ctx, "INSERT INTO shu_distributions(period_id,member_id,capital_weight,business_weight,capital_amount,business_amount,total) VALUES($1,$2,$3,$4,$5,$6,$7)", id, m, cw[i], bw[i], ca[i], ba[i], ca[i]+ba[i]); e != nil {
				return e
			}
		}
		if _, e = tx.Exec(ctx, "UPDATE shu_periods SET income=$2,expenses=$3,distributable=$4,capital_bps=$5,business_bps=$6 WHERE id=$1", id, income, costs, available, capital, business); e != nil {
			return e
		}
	} else if action == "review" {
		if calculated == nil {
			return fail(c, 409, "not_calculated", "Hitung SHU sebelum meninjau")
		}
		if _, e = tx.Exec(ctx, "UPDATE shu_periods SET status='reviewed',reviewed_by=$2 WHERE id=$1", id, actor(c)); e != nil {
			return e
		}
	} else if action == "finalize" {
		if _, e = tx.Exec(ctx, "UPDATE shu_periods SET status='finalized',finalized_by=$2 WHERE id=$1", id, actor(c)); e != nil {
			return e
		}
	} else {
		rr, e := tx.Query(ctx, "SELECT d.member_id,d.total,m.user_id FROM shu_distributions d JOIN members m ON m.id=d.member_id WHERE d.period_id=$1 ORDER BY d.member_id", id)
		if e != nil {
			return e
		}
		type payout struct{ member, total, user int64 }
		payouts := []payout{}
		for rr.Next() {
			var p payout
			if e = rr.Scan(&p.member, &p.total, &p.user); e != nil {
				rr.Close()
				return e
			}
			payouts = append(payouts, p)
		}
		e = rr.Err()
		rr.Close()
		if e != nil {
			return e
		}
		for _, p := range payouts {
			if p.total <= 0 {
				continue
			}
			if e = journal(ctx, tx, fmt.Sprintf("shu:%d:%d", id, p.member), "shu_distribution", "cash", p.total, p.member, fmt.Sprintf("Pembagian tunai SHU tahun %d", year)); e != nil {
				return e
			}
			if _, e = tx.Exec(ctx, "INSERT INTO notifications(user_id,title,message) VALUES($1,$2,$3)", p.user, "Pembagian SHU", fmt.Sprintf("SHU tahun %d sejumlah Rp %d telah dicatat sebagai pembayaran tunai. Hubungi petugas untuk bukti penerimaan.", year, p.total)); e != nil {
				return e
			}
		}
		if _, e = tx.Exec(ctx, "UPDATE shu_periods SET status='distributed' WHERE id=$1", id); e != nil {
			return e
		}
	}
	if e = audit(ctx, tx, actor(c), action, "shu_periods", id, fiber.Map{"status": status}, fiber.Map{"action": action}); e != nil {
		return e
	}
	if e = tx.Commit(ctx); e != nil {
		return e
	}
	return a.readSHU(c)
}
