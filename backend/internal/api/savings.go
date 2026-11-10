package api

import (
	"fmt"
	"github.com/gofiber/fiber/v2"
	"strings"
	"time"
)

func (a *API) savingsRoutes(r fiber.Router) {
	r.Get("/savings-types", func(c *fiber.Ctx) error {
		if role(c) == "committee" {
			return fiber.ErrForbidden
		}
		if role(c) == "admin" {
			return a.rows(c, "SELECT * FROM savings_types ORDER BY id")
		}
		return a.rows(c, "SELECT id,code,name,withdrawable,minimum_balance,required_amount,active FROM savings_types WHERE active=true ORDER BY id")
	})
	r.Post("/savings-types", require("admin"), a.saveType)
	r.Patch("/savings-types/:id", require("admin"), a.saveType)
	r.Get("/savings", func(c *fiber.Ctx) error {
		switch role(c) {
		case "member":
			return a.rows(c, `SELECT s.id,s.member_id,s.type_id,s.balance,t.name type_name FROM savings s JOIN savings_types t ON t.id=s.type_id WHERE s.member_id=$1 ORDER BY s.id`, member(c))
		case "teller", "admin":
			return a.rows(c, `SELECT s.id,s.member_id,s.type_id,s.balance,u.name member_name,t.name type_name FROM savings s JOIN members m ON m.id=s.member_id JOIN users u ON u.id=m.user_id JOIN savings_types t ON t.id=s.type_id ORDER BY s.id`)
		case "committee":
			return a.rows(c, `SELECT DISTINCT s.id,s.member_id,s.type_id,s.balance,t.name type_name FROM savings s JOIN savings_types t ON t.id=s.type_id JOIN loan_application_assignments a ON a.unassigned_at IS NULL JOIN loan_applications x ON x.id=a.application_id AND x.member_id=s.member_id WHERE a.committee_user_id=$1 ORDER BY s.id`, actor(c))
		default:
			return fiber.ErrForbidden
		}
	})
	r.Get("/savings-transactions", func(c *fiber.Ctx) error {
		switch role(c) {
		case "member":
			return a.rows(c, `SELECT s.id,s.member_id,s.type_id,s.kind,s.amount,s.status,s.note,s.period,s.reference,s.reason,s.created_at,t.name type_name FROM savings_transactions s JOIN savings_types t ON t.id=s.type_id WHERE s.member_id=$1 ORDER BY s.id DESC`, member(c))
		case "teller", "admin":
			return a.rows(c, `SELECT s.*,u.name member_name,t.name type_name FROM savings_transactions s JOIN members m ON m.id=s.member_id JOIN users u ON u.id=m.user_id JOIN savings_types t ON t.id=s.type_id ORDER BY s.id DESC`)
		default:
			return fiber.ErrForbidden
		}
	})
	r.Post("/savings-transactions", require("member", "admin", "teller"), a.createSaving)
	r.Post("/savings-transactions/:id/verify", require("teller", "admin"), a.verifySaving)
}

func (a *API) saveType(c *fiber.Ctx) error {
	var b struct {
		Code, Name   string
		Withdrawable bool
		Minimum      *int64 `json:"minimum_balance"`
		Required     *int64 `json:"required_amount"`
		Active       bool
	}
	if c.BodyParser(&b) != nil || b.Name == "" || !contains([]string{"principal", "mandatory", "voluntary"}, b.Code) || b.Minimum != nil && (*b.Minimum < 0 || *b.Minimum > 1000000000000) || b.Required != nil && (*b.Required <= 0 || *b.Required > 1000000000000) {
		return fiber.ErrBadRequest
	}
	tx, e := a.DB.Begin(c.Context())
	if e != nil {
		return e
	}
	defer tx.Rollback(c.Context())
	iid := id(c)
	if iid == 0 {
		e = tx.QueryRow(c.Context(), `INSERT INTO savings_types(code,name,withdrawable,minimum_balance,required_amount,active) VALUES($1,$2,$3,$4,$5,$6) RETURNING id`, b.Code, b.Name, b.Withdrawable, b.Minimum, b.Required, b.Active).Scan(&iid)
	} else {
		_, e = tx.Exec(c.Context(), `UPDATE savings_types SET code=$1,name=$2,withdrawable=$3,minimum_balance=$4,required_amount=$5,active=$6 WHERE id=$7`, b.Code, b.Name, b.Withdrawable, b.Minimum, b.Required, b.Active, iid)
	}
	if e != nil {
		return e
	}
	if e = audit(c.Context(), tx, actor(c), "configure", "savings_types", iid, nil, b); e != nil {
		return e
	}
	if e = tx.Commit(c.Context()); e != nil {
		return e
	}
	return ok(c, fiber.Map{"id": iid})
}
func (a *API) createSaving(c *fiber.Ctx) error {
	var b struct {
		MemberID                int64 `json:"member_id"`
		TypeID                  int64 `json:"type_id"`
		Kind                    string
		Amount                  int64
		Note, Period, Reference string
	}
	if c.BodyParser(&b) != nil || b.Amount <= 0 || b.Amount > 1000000000000 || !contains([]string{"deposit", "withdrawal"}, b.Kind) || strings.TrimSpace(b.Reference) == "" {
		return fiber.ErrBadRequest
	}
	if role(c) == "member" {
		if b.MemberID != 0 && b.MemberID != member(c) {
			return fiber.ErrForbidden
		}
		b.MemberID = member(c)
	}
	if e := a.active(c, b.MemberID); e != nil {
		return e
	}
	var code string
	var withdraw bool
	var minimum, required *int64
	e := a.DB.QueryRow(c.Context(), "SELECT code,withdrawable,minimum_balance,required_amount FROM savings_types WHERE id=$1 AND active", b.TypeID).Scan(&code, &withdraw, &minimum, &required)
	if e != nil {
		return fiber.ErrBadRequest
	}
	if b.Kind == "withdrawal" && (!withdraw || minimum == nil) {
		return fail(c, 400, "withdrawal_blocked", "Penarikan belum diizinkan atau batas minimum belum diatur")
	}
	if b.Kind == "withdrawal" {
		var balance int64
		if e := a.DB.QueryRow(c.Context(), "SELECT COALESCE((SELECT balance FROM savings WHERE member_id=$1 AND type_id=$2),0)", b.MemberID, b.TypeID).Scan(&balance); e != nil {
			return e
		}
		if balance-b.Amount < *minimum {
			return fail(c, 409, "insufficient_balance", "Saldo tidak mencukupi")
		}
	}
	if b.Kind == "deposit" {
		switch code {
		case "principal":
			if required == nil || b.Amount != *required {
				return fail(c, 400, "amount_policy", "Nominal simpanan pokok belum diatur atau tidak sesuai")
			}
			b.Period = "once"
		case "mandatory":
			if required == nil || b.Amount != *required {
				return fail(c, 400, "amount_policy", "Nominal wajib belum diatur atau tidak sesuai")
			}
			if _, e := time.Parse("2006-01", b.Period); e != nil {
				return fail(c, 400, "period_required", "Periode YYYY-MM wajib")
			}
		default:
			b.Period = ""
		}
	} else {
		b.Period = ""
	}
	tx, e := a.DB.Begin(c.Context())
	if e != nil {
		return e
	}
	defer tx.Rollback(c.Context())
	var tid int64
	e = tx.QueryRow(c.Context(), `INSERT INTO savings_transactions(member_id,type_id,kind,amount,note,period,reference) VALUES($1,$2,$3,$4,$5,NULLIF($6,''),$7) RETURNING id`, b.MemberID, b.TypeID, b.Kind, b.Amount, b.Note, b.Period, b.Reference).Scan(&tid)
	if e != nil {
		return e
	}
	if e = audit(c.Context(), tx, actor(c), "submitted", "savings_transactions", tid, nil, b); e != nil {
		return e
	}
	if e = tx.Commit(c.Context()); e != nil {
		return e
	}
	return ok(c, fiber.Map{"id": tid, "status": "pending"})
}
func (a *API) verifySaving(c *fiber.Ctx) error {
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
	var mid, typ, amount int64
	var kind, status, ref string
	e = tx.QueryRow(c.Context(), "SELECT member_id,type_id,amount,kind,status,reference FROM savings_transactions WHERE id=$1 FOR UPDATE", id(c)).Scan(&mid, &typ, &amount, &kind, &status, &ref)
	if e != nil {
		return fiber.ErrNotFound
	}
	if status != "pending" {
		return fail(c, 409, "already_processed", "Transaksi sudah diproses")
	}
	next := "rejected"
	if b.Approve {
		var memberStatus string
		if e = tx.QueryRow(c.Context(), "SELECT status FROM members WHERE id=$1 FOR SHARE", mid).Scan(&memberStatus); e != nil {
			return e
		}
		if memberStatus != "active" {
			return fiber.NewError(409, "Anggota tidak aktif")
		}
		var policyCode string
		var policyActive bool
		var required *int64
		if e = tx.QueryRow(c.Context(), "SELECT code,active,required_amount FROM savings_types WHERE id=$1 FOR SHARE", typ).Scan(&policyCode, &policyActive, &required); e != nil {
			return e
		}
		if !policyActive || (kind == "deposit" && (policyCode == "principal" || policyCode == "mandatory") && (required == nil || *required != amount)) {
			return fail(c, 409, "policy_changed", "Jenis simpanan tidak aktif atau kebijakan nominal berubah")
		}
		_, e = tx.Exec(c.Context(), "INSERT INTO savings(member_id,type_id) VALUES($1,$2) ON CONFLICT DO NOTHING", mid, typ)
		if e != nil {
			return e
		}
		var balance int64
		e = tx.QueryRow(c.Context(), "SELECT balance FROM savings WHERE member_id=$1 AND type_id=$2 FOR UPDATE", mid, typ).Scan(&balance)
		if e != nil {
			return e
		}
		delta := amount
		debit, credit := "cash", "savings"
		if kind == "withdrawal" {
			var min *int64
			var allowed bool
			e = tx.QueryRow(c.Context(), "SELECT withdrawable,minimum_balance FROM savings_types WHERE id=$1", typ).Scan(&allowed, &min)
			if e != nil {
				return e
			}
			var locked int64
			if e = tx.QueryRow(c.Context(), `SELECT COALESCE(sum(locked_amount),0)::bigint FROM collateral_locks WHERE asset_type='savings' AND asset_id=(SELECT id FROM savings WHERE member_id=$1 AND type_id=$2) AND status='active'`, mid, typ).Scan(&locked); e != nil {
				return e
			}
			if !allowed || min == nil || balance-amount < *min+locked {
				return fail(c, 409, "insufficient_balance", "Saldo tidak cukup, saldo minimum, atau saldo sedang dijaminkan")
			}
			delta = -amount
			debit, credit = "savings", "cash"
		}
		if _, e = tx.Exec(c.Context(), "UPDATE savings SET balance=balance+$1 WHERE member_id=$2 AND type_id=$3", delta, mid, typ); e != nil {
			return e
		}
		if e = journal(c.Context(), tx, "saving:"+fmt.Sprint(id(c)), debit, credit, amount, mid, kind); e != nil {
			return e
		}
		if e = refreshMemberTier(c.Context(), tx, mid, "Transaksi simpanan disetujui"); e != nil {
			return e
		}
		next = "approved"
	}
	if _, e = tx.Exec(c.Context(), "UPDATE savings_transactions SET status=$1,reason=$2,verified_by=$3,verified_at=CASE WHEN $1='approved' THEN now() ELSE NULL END WHERE id=$4", next, b.Reason, actor(c), id(c)); e != nil {
		return e
	}
	if _, e = tx.Exec(c.Context(), "INSERT INTO notifications(user_id,title,message) SELECT user_id,'Transaksi simpanan',$1 FROM members WHERE id=$2", next+": "+b.Reason, mid); e != nil {
		return e
	}
	if e = audit(c.Context(), tx, actor(c), "verify", "savings_transactions", id(c), status, fiber.Map{"status": next, "reason": b.Reason}); e != nil {
		return e
	}
	if e = tx.Commit(c.Context()); e != nil {
		return e
	}
	return ok(c, fiber.Map{"id": id(c), "status": next})
}
