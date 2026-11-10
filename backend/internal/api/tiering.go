package api

import (
	"context"
	"fmt"
	"strings"

	"github.com/gofiber/fiber/v2"
	"github.com/jackc/pgx/v5"
)

func (a *API) tierRoutes(r fiber.Router) {
	r.Get("/member-tier", require("member"), func(c *fiber.Ctx) error {
		var exists bool
		if err := a.DB.QueryRow(c.Context(), `SELECT EXISTS(SELECT 1 FROM member_tiers WHERE member_id=$1)`, member(c)).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			if _, err := a.DB.Exec(c.Context(), `INSERT INTO member_tiers(member_id) VALUES($1) ON CONFLICT DO NOTHING`, member(c)); err != nil {
				return err
			}
		}
		var tier string
		var balance int64
		var current bool
		var minBalance, maxLoan int64
		var loanDiscount, depositBonus int32
		var priority string
		var waive bool
		var updated any
		err := a.DB.QueryRow(c.Context(), `SELECT t.tier,t.qualifying_balance,t.repayment_current,p.min_qualifying_balance,p.max_loan_amount,p.loan_discount_bps,p.deposit_bonus_bps,p.priority_label,p.waive_admin_fee,t.updated_at FROM member_tiers t JOIN member_tier_policies p ON p.tier=t.tier WHERE t.member_id=$1`, member(c)).Scan(&tier, &balance, &current, &minBalance, &maxLoan, &loanDiscount, &depositBonus, &priority, &waive, &updated)
		if err != nil {
			return err
		}
		policies, err := a.readMaps(c, `SELECT tier,min_qualifying_balance,max_loan_amount,loan_discount_bps,deposit_bonus_bps,priority_label,waive_admin_fee FROM member_tier_policies ORDER BY min_qualifying_balance`)
		if err != nil {
			return err
		}
		var next int64
		for _, p := range policies {
			v, _ := numberInt64(p["min_qualifying_balance"])
			if v > balance && (next == 0 || v < next) {
				next = v
			}
		}
		progress := int64(100)
		if next > 0 && balance < next {
			progress = (balance - minBalance) * 100 / (next - minBalance)
			if progress < 0 {
				progress = 0
			}
			if progress > 100 {
				progress = 100
			}
		}
		return ok(c, fiber.Map{"tier": tier, "qualifying_balance": balance, "repayment_current": current, "min_qualifying_balance": minBalance, "max_loan_amount": maxLoan, "loan_discount_bps": loanDiscount, "deposit_bonus_bps": depositBonus, "priority_label": priority, "waive_admin_fee": waive, "progress_percent": progress, "next_tier_threshold": next, "policies": policies, "updated_at": updated})
	})
	r.Get("/member-tier/history", require("member"), func(c *fiber.Ctx) error {
		return a.rows(c, `SELECT old_tier,new_tier,qualifying_balance,reason,created_at FROM member_tier_history WHERE member_id=$1 ORDER BY created_at DESC`, member(c))
	})
	r.Get("/member-tier/policies", require("admin"), func(c *fiber.Ctx) error {
		return a.rows(c, `SELECT * FROM member_tier_policies ORDER BY min_qualifying_balance`)
	})
	r.Put("/member-tier/policies/:tier", require("admin"), a.updateTierPolicy)
	r.Post("/member-tier/recalculate", require("admin"), a.recalculateTiers)
}

func (a *API) updateTierPolicy(c *fiber.Ctx) error {
	var b struct {
		MinBalance   int64  `json:"min_qualifying_balance"`
		MaxLoan      int64  `json:"max_loan_amount"`
		LoanDiscount int    `json:"loan_discount_bps"`
		DepositBonus int    `json:"deposit_bonus_bps"`
		Priority     string `json:"priority_label"`
		Waive        bool   `json:"waive_admin_fee"`
	}
	if c.BodyParser(&b) != nil || b.MinBalance < 0 || b.MaxLoan <= 0 || b.MaxLoan > 1000000000000 || b.LoanDiscount < 0 || b.LoanDiscount > 10000 || b.DepositBonus < 0 || b.DepositBonus > 10000 || strings.TrimSpace(b.Priority) == "" {
		return fail(c, 400, "invalid_policy", "Kebijakan tier tidak valid")
	}
	tier := c.Params("tier")
	if !contains([]string{"regular", "platinum", "vip"}, tier) {
		return fail(c, 400, "invalid_tier", "Tier tidak dikenal")
	}
	tx, err := a.DB.Begin(c.Context())
	if err != nil {
		return err
	}
	defer tx.Rollback(c.Context())
	_, err = tx.Exec(c.Context(), `UPDATE member_tier_policies SET min_qualifying_balance=$1,max_loan_amount=$2,loan_discount_bps=$3,deposit_bonus_bps=$4,priority_label=$5,waive_admin_fee=$6,updated_at=now() WHERE tier=$7`, b.MinBalance, b.MaxLoan, b.LoanDiscount, b.DepositBonus, strings.TrimSpace(b.Priority), b.Waive, tier)
	if err != nil {
		return err
	}
	if err = audit(c.Context(), tx, actor(c), "tier_policy_updated", "member_tier_policies", 0, nil, b); err != nil {
		return err
	}
	if err = tx.Commit(c.Context()); err != nil {
		return err
	}
	return ok(c, fiber.Map{"tier": tier, "updated": true})
}

func (a *API) recalculateTiers(c *fiber.Ctx) error {
	rows, err := a.readMaps(c, `SELECT id FROM members ORDER BY id`)
	if err != nil {
		return err
	}
	updated := 0
	for _, row := range rows {
		mid, _ := numberInt64(row["id"])
		tx, e := a.DB.Begin(c.Context())
		if e != nil {
			return e
		}
		if e = refreshMemberTier(c.Context(), tx, mid, "Rekalkulasi berkala oleh admin"); e != nil {
			_ = tx.Rollback(c.Context())
			return e
		}
		if e = tx.Commit(c.Context()); e != nil {
			return e
		}
		updated++
	}
	return ok(c, fiber.Map{"recalculated": updated})
}

func refreshMemberTier(ctx context.Context, tx pgx.Tx, memberID int64, reason string) error {
	var balance int64
	err := tx.QueryRow(ctx, `SELECT (SELECT COALESCE(sum(balance),0) FROM savings WHERE member_id=$1)+(SELECT COALESCE(sum(principal),0) FROM deposits WHERE member_id=$1 AND status='active')`, memberID).Scan(&balance)
	if err != nil {
		return err
	}
	var current bool
	err = tx.QueryRow(ctx, `SELECT NOT EXISTS(SELECT 1 FROM installments i JOIN loans l ON l.id=i.loan_id WHERE l.member_id=$1 AND ((i.status='unpaid' AND i.due_date<(now() AT TIME ZONE 'Asia/Makassar')::date) OR (i.status='paid' AND i.paid_at IS NOT NULL AND i.paid_at::date>i.due_date)))`, memberID).Scan(&current)
	if err != nil {
		return err
	}
	var next string
	err = tx.QueryRow(ctx, `SELECT tier FROM member_tier_policies WHERE min_qualifying_balance<=$1 ORDER BY min_qualifying_balance DESC LIMIT 1`, balance).Scan(&next)
	if err != nil {
		return err
	}
	if !current {
		next = "regular"
	}
	var old string
	err = tx.QueryRow(ctx, `SELECT tier FROM member_tiers WHERE member_id=$1 FOR UPDATE`, memberID).Scan(&old)
	if err == pgx.ErrNoRows {
		_, err = tx.Exec(ctx, `INSERT INTO member_tiers(member_id,tier,qualifying_balance,repayment_current) VALUES($1,$2,$3,$4)`, memberID, next, balance, current)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO member_tier_history(member_id,old_tier,new_tier,qualifying_balance,reason) VALUES($1,NULL,$2,$3,$4)`, memberID, next, balance, reason)
		return err
	}
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE member_tiers SET tier=$1,qualifying_balance=$2,repayment_current=$3,updated_at=now() WHERE member_id=$4`, next, balance, current, memberID); err != nil {
		return err
	}
	if old != next {
		if _, err = tx.Exec(ctx, `INSERT INTO member_tier_history(member_id,old_tier,new_tier,qualifying_balance,reason) VALUES($1,$2,$3,$4,$5)`, memberID, old, next, balance, reason); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO notifications(user_id,title,message) SELECT user_id,'Level anggota diperbarui',$1 FROM members WHERE id=$2`, fmt.Sprintf("Level berubah dari %s menjadi %s", old, next), memberID); err != nil {
			return err
		}
	}
	return nil
}
