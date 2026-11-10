package api

import (
	"context"
	"github.com/gofiber/fiber/v2"
	"github.com/jackc/pgx/v5"
	"koplink/internal/finance"
	"time"
)

func (a *API) RunScheduler() (int, error) { return a.runScheduler(0) }
func (a *API) runScheduler(actorID int64) (int, error) {
	ctx := context.Background()
	tx, e := a.DB.Begin(ctx)
	if e != nil {
		return 0, e
	}
	defer tx.Rollback(ctx)
	rs, e := tx.Query(ctx, `SELECT i.id,i.principal,i.due_date,l.penalty_daily_amount,l.penalty_daily_bps FROM installments i JOIN loans l ON l.id=i.loan_id WHERE i.status='unpaid' AND i.due_date<(now() AT TIME ZONE 'Asia/Makassar')::date FOR UPDATE OF i`)
	if e != nil {
		return 0, e
	}
	type item struct {
		id, p, pa, pb int64
		due           time.Time
	}
	items := []item{}
	for rs.Next() {
		var i item
		if e = rs.Scan(&i.id, &i.p, &i.due, &i.pa, &i.pb); e != nil {
			rs.Close()
			return 0, e
		}
		items = append(items, i)
	}
	rs.Close()
	if e = rs.Err(); e != nil {
		return 0, e
	}
	loc, _ := time.LoadLocation("Asia/Makassar")
	for _, i := range items {
		pen := finance.Penalty(i.p, i.pa, i.pb, localDueDate(i.due, loc), time.Now().In(loc))
		if _, e = tx.Exec(ctx, "UPDATE installments SET penalty=$1 WHERE id=$2", pen, i.id); e != nil {
			return 0, e
		}
	}
	_, e = tx.Exec(ctx, `INSERT INTO notifications(user_id,title,message,event_key)
      SELECT m.user_id,CASE WHEN i.due_date<(now() AT TIME ZONE 'Asia/Makassar')::date THEN 'Angsuran terlambat' ELSE 'Pengingat jatuh tempo' END,
      'Angsuran #'||i.number::text||' jatuh tempo '||i.due_date::text||'. Periksa jumlah terbaru sebelum membayar.',
      'installment:'||i.id::text||':'||(now() AT TIME ZONE 'Asia/Makassar')::date::text
      FROM installments i JOIN loans l ON l.id=i.loan_id JOIN members m ON m.id=l.member_id
      WHERE i.status='unpaid' AND (i.due_date<(now() AT TIME ZONE 'Asia/Makassar')::date OR i.due_date IN ((now() AT TIME ZONE 'Asia/Makassar')::date+1,(now() AT TIME ZONE 'Asia/Makassar')::date+3))
      ON CONFLICT(event_key) DO NOTHING`)
	if e != nil {
		return 0, e
	}
	if e = audit(ctx, tx, actorID, "penalties_recalculated", "installments", 0, nil, fiber.Map{"count": len(items)}); e != nil {
		return 0, e
	}
	return len(items), tx.Commit(ctx)
}

func (a *API) dashboardRoutes(r fiber.Router) {
	r.Get("/dashboard", func(c *fiber.Ctx) error {
		out := fiber.Map{"role": role(c)}
		now := time.Now().In(makassarLocation())
		switch role(c) {
		case "member":
			mid := member(c)
			queries := map[string]string{
				"total_savings":        `SELECT COALESCE(sum(balance),0)::bigint FROM savings WHERE member_id=$1`,
				"active_loans":         `SELECT count(*) FROM loans WHERE member_id=$1 AND status='active'`,
				"outstanding":          `SELECT COALESCE(sum(i.principal+i.interest+i.penalty),0)::bigint FROM installments i JOIN loans l ON l.id=i.loan_id WHERE l.member_id=$1 AND i.status='unpaid'`,
				"total_overdue":        `SELECT COALESCE(sum(i.principal+i.interest+i.penalty),0)::bigint FROM installments i JOIN loans l ON l.id=i.loan_id WHERE l.member_id=$1 AND i.status='unpaid' AND i.due_date<(now() AT TIME ZONE 'Asia/Makassar')::date`,
				"pending_transactions": `SELECT count(*) FROM savings_transactions WHERE member_id=$1 AND status='pending'`,
				"pending_applications": `SELECT count(*) FROM loan_applications WHERE member_id=$1 AND status IN ('submitted','surveyed')`,
				"active_deposits":      `SELECT count(*) FROM deposits WHERE member_id=$1 AND status='active'`,
				"deposit_balance":      `SELECT COALESCE(sum(principal),0)::bigint FROM deposits WHERE member_id=$1 AND status='active'`,
			}
			for key, q := range queries {
				var n int64
				if e := a.DB.QueryRow(c.Context(), q, mid).Scan(&n); e != nil {
					return e
				}
				out[key] = n
			}
			var tier string
			var tierBalance int64
			var repaymentCurrent bool
			if e := a.DB.QueryRow(c.Context(), `SELECT tier,qualifying_balance,repayment_current FROM member_tiers WHERE member_id=$1`, mid).Scan(&tier, &tierBalance, &repaymentCurrent); e == nil {
				out["tier"] = fiber.Map{"tier": tier, "qualifying_balance": tierBalance, "repayment_current": repaymentCurrent}
			} else if e != pgx.ErrNoRows {
				return e
			}
			if e := a.dashboardRows(c, out, "savings_by_type", `SELECT t.name,COALESCE(sum(s.balance),0)::bigint value FROM savings_types t LEFT JOIN savings s ON s.type_id=t.id AND s.member_id=$1 WHERE t.active=true GROUP BY t.id ORDER BY t.id`, mid); e != nil {
				return e
			}
			if e := a.dashboardRows(c, out, "recent_transactions", `SELECT x.id,x.created_at,t.name type_name,x.kind,x.amount,x.status,x.reference FROM savings_transactions x JOIN savings_types t ON t.id=x.type_id WHERE x.member_id=$1 ORDER BY x.id DESC LIMIT 8`, mid); e != nil {
				return e
			}
			if e := a.dashboardRows(c, out, "next_installments", `SELECT i.id,i.loan_id,i.number,i.due_date,i.principal,i.interest,i.penalty,(i.principal+i.interest+i.penalty) total,i.status FROM installments i JOIN loans l ON l.id=i.loan_id WHERE l.member_id=$1 AND i.status='unpaid' ORDER BY i.due_date LIMIT 8`, mid); e != nil {
				return e
			}
			if e := a.dashboardRows(c, out, "my_applications", `SELECT x.id,x.amount,x.tenor,x.status,x.created_at,p.name product_name FROM loan_applications x JOIN loan_products p ON p.id=x.product_id WHERE x.member_id=$1 ORDER BY x.id DESC LIMIT 5`, mid); e != nil {
				return e
			}
		case "teller":
			queries := map[string]string{
				"pending_savings":   `SELECT count(*) FROM savings_transactions WHERE status='pending'`,
				"pending_payments":  `SELECT count(*) FROM payments WHERE status='pending'`,
				"loans_to_disburse": `SELECT count(*) FROM loan_applications WHERE status='approved'`,
				"today_incoming":    `SELECT COALESCE(sum(amount),0)::bigint FROM savings_transactions WHERE status='approved' AND kind='deposit' AND COALESCE(verified_at,created_at) >= $1`,
				"today_outgoing":    `SELECT COALESCE(sum(amount),0)::bigint FROM savings_transactions WHERE status='approved' AND kind='withdrawal' AND COALESCE(verified_at,created_at) >= $1`,
				"today_payments":    `SELECT COALESCE(sum(amount),0)::bigint FROM payments WHERE status='approved' AND verified_at >= $1`,
			}
			for key, q := range queries {
				var n int64
				if e := a.DB.QueryRow(c.Context(), q, now.Format(time.RFC3339)).Scan(&n); e != nil {
					return e
				}
				out[key] = n
			}
			if e := a.dashboardRows(c, out, "pending_savings_queue", `SELECT s.id,s.created_at,u.name member_name,t.name type_name,s.kind,s.amount,s.reference FROM savings_transactions s JOIN members m ON m.id=s.member_id JOIN users u ON u.id=m.user_id JOIN savings_types t ON t.id=s.type_id WHERE s.status='pending' ORDER BY s.created_at LIMIT 8`); e != nil {
				return e
			}
			if e := a.dashboardRows(c, out, "pending_payment_queue", `SELECT p.id,p.created_at,u.name member_name,p.installment_id,p.amount,p.reference FROM payments p JOIN installments i ON i.id=p.installment_id JOIN loans l ON l.id=i.loan_id JOIN members m ON m.id=l.member_id JOIN users u ON u.id=m.user_id WHERE p.status='pending' ORDER BY p.created_at LIMIT 8`); e != nil {
				return e
			}
			if e := a.dashboardRows(c, out, "disbursement_queue", `SELECT x.id,x.amount,x.tenor,u.name member_name,p.name product_name,x.created_at FROM loan_applications x JOIN members m ON m.id=x.member_id JOIN users u ON u.id=m.user_id JOIN loan_products p ON p.id=x.product_id WHERE x.status='approved' ORDER BY x.created_at LIMIT 8`); e != nil {
				return e
			}
			if e := a.dashboardRows(c, out, "recent_transactions", `SELECT id,COALESCE(verified_at,created_at) created_at,kind,amount,status,reference FROM savings_transactions WHERE verified_by=$1 ORDER BY id DESC LIMIT 8`, actor(c)); e != nil {
				return e
			}
		case "committee":
			queries := map[string]string{
				"assigned_cases":          `SELECT count(*) FROM loan_application_assignments WHERE committee_user_id=$1 AND unassigned_at IS NULL`,
				"cases_to_review":         `SELECT count(*) FROM loan_application_assignments a JOIN loan_applications x ON x.id=a.application_id WHERE a.committee_user_id=$1 AND a.unassigned_at IS NULL AND x.status IN ('submitted','surveyed')`,
				"cases_awaiting_decision": `SELECT count(*) FROM loan_application_assignments a JOIN loan_applications x ON x.id=a.application_id WHERE a.committee_user_id=$1 AND a.unassigned_at IS NULL AND x.status='surveyed'`,
				"assigned_overdue":        `SELECT count(*) FROM loan_application_assignments a JOIN loans l ON l.application_id=a.application_id JOIN installments i ON i.loan_id=l.id WHERE a.committee_user_id=$1 AND a.unassigned_at IS NULL AND i.status='unpaid' AND i.due_date<(now() AT TIME ZONE 'Asia/Makassar')::date`,
			}
			for key, q := range queries {
				var n int64
				if e := a.DB.QueryRow(c.Context(), q, actor(c)).Scan(&n); e != nil {
					return e
				}
				out[key] = n
			}
			if e := a.dashboardRows(c, out, "assigned_applications", `SELECT x.id,x.amount,x.tenor,x.status,x.created_at,x.purpose,u.name member_name,p.name product_name FROM loan_application_assignments a JOIN loan_applications x ON x.id=a.application_id JOIN members m ON m.id=x.member_id JOIN users u ON u.id=m.user_id JOIN loan_products p ON p.id=x.product_id WHERE a.committee_user_id=$1 AND a.unassigned_at IS NULL ORDER BY x.created_at DESC LIMIT 10`, actor(c)); e != nil {
				return e
			}
			if e := a.dashboardRows(c, out, "survey_queue", `SELECT x.id,x.amount,x.tenor,x.status,x.created_at,u.name member_name FROM loan_application_assignments a JOIN loan_applications x ON x.id=a.application_id JOIN members m ON m.id=x.member_id JOIN users u ON u.id=m.user_id WHERE a.committee_user_id=$1 AND a.unassigned_at IS NULL AND x.status IN ('submitted','surveyed') ORDER BY x.created_at LIMIT 8`, actor(c)); e != nil {
				return e
			}
		case "admin":
			queries := map[string]string{
				"total_members":        `SELECT count(*) FROM members`,
				"active_members":       `SELECT count(*) FROM members WHERE status='active'`,
				"pending_members":      `SELECT count(*) FROM members WHERE status='pending'`,
				"total_savings":        `SELECT COALESCE(sum(balance),0)::bigint FROM savings`,
				"active_deposits":      `SELECT count(*) FROM deposits WHERE status='active'`,
				"deposit_balance":      `SELECT COALESCE(sum(principal),0)::bigint FROM deposits WHERE status='active'`,
				"active_loans":         `SELECT count(*) FROM loans WHERE status='active'`,
				"outstanding":          `SELECT COALESCE(sum(i.principal+i.interest+i.penalty),0)::bigint FROM installments i JOIN loans l ON l.id=i.loan_id WHERE i.status='unpaid'`,
				"total_overdue":        `SELECT COALESCE(sum(i.principal+i.interest+i.penalty),0)::bigint FROM installments i JOIN loans l ON l.id=i.loan_id WHERE i.status='unpaid' AND i.due_date<(now() AT TIME ZONE 'Asia/Makassar')::date`,
				"pending_applications": `SELECT count(*) FROM loan_applications WHERE status IN ('submitted','surveyed')`,
				"pending_transactions": `SELECT count(*) FROM savings_transactions WHERE status='pending'`,
				"pending_payments":     `SELECT count(*) FROM payments WHERE status='pending'`,
				"unassigned_cases":     `SELECT count(*) FROM loan_applications x WHERE x.status IN ('submitted','surveyed') AND NOT EXISTS(SELECT 1 FROM loan_application_assignments a WHERE a.application_id=x.id AND a.unassigned_at IS NULL)`,
			}
			for key, q := range queries {
				var n int64
				if e := a.DB.QueryRow(c.Context(), q).Scan(&n); e != nil {
					return e
				}
				out[key] = n
			}
			if e := a.dashboardRows(c, out, "savings_by_type", `SELECT t.name,COALESCE(sum(s.balance),0)::bigint value FROM savings_types t LEFT JOIN savings s ON s.type_id=t.id GROUP BY t.id ORDER BY t.id`); e != nil {
				return e
			}
			if e := a.dashboardRows(c, out, "monthly_transactions", `SELECT to_char(COALESCE(verified_at,created_at) AT TIME ZONE 'Asia/Makassar','YYYY-MM') AS month,COALESCE(sum(amount) FILTER(WHERE kind='deposit'),0)::bigint deposits,COALESCE(sum(amount) FILTER(WHERE kind='withdrawal'),0)::bigint withdrawals FROM savings_transactions WHERE status='approved' GROUP BY 1 ORDER BY 1 DESC LIMIT 12`); e != nil {
				return e
			}
			if e := a.dashboardRows(c, out, "recent_transactions", `SELECT x.id,x.created_at,t.name type_name,x.kind,x.amount,x.status,u.name member_name FROM savings_transactions x JOIN savings_types t ON t.id=x.type_id JOIN members m ON m.id=x.member_id JOIN users u ON u.id=m.user_id ORDER BY x.id DESC LIMIT 8`); e != nil {
				return e
			}
			if e := a.dashboardRows(c, out, "next_installments", `SELECT i.id,i.loan_id,i.due_date,i.number,(i.principal+i.interest+i.penalty) total,i.status,u.name member_name FROM installments i JOIN loans l ON l.id=i.loan_id JOIN members m ON m.id=l.member_id JOIN users u ON u.id=m.user_id WHERE i.status='unpaid' ORDER BY i.due_date LIMIT 8`); e != nil {
				return e
			}
			if e := a.dashboardRows(c, out, "unassigned_applications", `SELECT x.id,x.amount,x.tenor,x.status,x.created_at,u.name member_name,p.name product_name FROM loan_applications x JOIN members m ON m.id=x.member_id JOIN users u ON u.id=m.user_id JOIN loan_products p ON p.id=x.product_id WHERE x.status IN ('submitted','surveyed') AND NOT EXISTS(SELECT 1 FROM loan_application_assignments a WHERE a.application_id=x.id AND a.unassigned_at IS NULL) ORDER BY x.created_at LIMIT 8`); e != nil {
				return e
			}
		default:
			return fail(c, 403, "role_not_supported", "Peran pengguna tidak dikenali")
		}
		return ok(c, out)
	})
}

func makassarLocation() *time.Location {
	loc, err := time.LoadLocation("Asia/Makassar")
	if err != nil {
		return time.FixedZone("WITA", 8*60*60)
	}
	return loc
}

func (a *API) dashboardRows(c *fiber.Ctx, out fiber.Map, key, query string, args ...any) error {
	rs, e := a.DB.Query(c.Context(), query, args...)
	if e != nil {
		return e
	}
	defer rs.Close()
	arr := []fiber.Map{}
	for rs.Next() {
		vals, e := rs.Values()
		if e != nil {
			return e
		}
		row := fiber.Map{}
		for i, f := range rs.FieldDescriptions() {
			row[f.Name] = vals[i]
		}
		arr = append(arr, row)
	}
	if e = rs.Err(); e != nil {
		return e
	}
	out[key] = arr
	return nil
}
