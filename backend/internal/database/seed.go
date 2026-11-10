package database

import (
	"context"
	"fmt"
	"net/mail"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"
	"koplink/internal/finance"
)

// Bootstrap creates the initial production administrator without changing an
// existing account. It does not configure financial policy or create demo data.
func Bootstrap(ctx context.Context, p *pgxpool.Pool, email, password string) error {
	email = strings.ToLower(strings.TrimSpace(email))
	addr, err := mail.ParseAddress(email)
	if err != nil || addr.Address != email {
		return fmt.Errorf("invalid INITIAL_ADMIN_EMAIL")
	}
	if len(password) < 10 || len(password) > 72 {
		return fmt.Errorf("INITIAL_ADMIN_PASSWORD must contain 10-72 bytes")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), 12)
	if err != nil {
		return err
	}
	tx, err := p.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(1482026)"); err != nil {
		return err
	}
	var existingRole string
	err = tx.QueryRow(ctx, "SELECT role FROM users WHERE email=$1", email).Scan(&existingRole)
	if err == nil {
		if existingRole != "admin" {
			return fmt.Errorf("INITIAL_ADMIN_EMAIL is already assigned to a non-admin account")
		}
		return tx.Commit(ctx)
	}
	if err != pgx.ErrNoRows {
		return err
	}
	var hasAdmin bool
	if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM users WHERE role='admin')").Scan(&hasAdmin); err != nil {
		return err
	}
	if hasAdmin {
		return fmt.Errorf("an administrator already exists; use authenticated user management")
	}
	var id int64
	if err = tx.QueryRow(ctx, "INSERT INTO users(name,email,password_hash,role) VALUES('Administrator',$1,$2,'admin') RETURNING id", email, string(hash)).Scan(&id); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO audit_logs(actor_id,action,entity,entity_id,"after") VALUES($1,'bootstrap','users',$1,'{"role":"admin"}')`, id); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Seed creates an explicitly fictitious demonstration dataset atomically. Its
// immutable audit marker makes repeat runs no-ops, including policy and balances.
func Seed(ctx context.Context, p *pgxpool.Pool, password string) error {
	if len(password) < 10 || len(password) > 72 {
		return fmt.Errorf("DEMO_PASSWORD must contain 10-72 bytes")
	}
	hash, e := bcrypt.GenerateFromPassword([]byte(password), 12)
	if e != nil {
		return e
	}
	tx, e := p.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	if _, e = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(1482027)"); e != nil {
		return e
	}
	var done bool
	if e = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM audit_logs WHERE action='demo_seed_v1')").Scan(&done); e != nil {
		return e
	}
	if done {
		return tx.Commit(ctx)
	}
	now := time.Now().In(time.FixedZone("Asia/Makassar", 8*3600))
	history := time.Date(now.Year()-1, time.January, 1, 9, 0, 0, 0, now.Location())
	userIDs := map[string]int64{}
	memberIDs := []int64{}
	for i, r := range []string{"admin", "teller", "committee", "member", "member2"} {
		role := r
		if r == "member2" {
			role = "member"
		}
		var uid int64
		e = tx.QueryRow(ctx, `INSERT INTO users(name,email,password_hash,role,created_at) VALUES($1,$2,$3,$4,$5) ON CONFLICT(email) DO UPDATE SET email=excluded.email RETURNING id`, "DEMO "+r, r+"@koplink.demo", string(hash), role, history).Scan(&uid)
		if e != nil {
			return e
		}
		userIDs[r] = uid
		if role == "member" {
			var mid int64
			e = tx.QueryRow(ctx, `INSERT INTO members(user_id,number,nik,address,phone,occupation,birth_date,status,created_at) VALUES($1,'DEMO-'||($1::bigint)::text,$2,'Alamat fiktif DEMO','080000000000','Pekerjaan fiktif DEMO','1995-01-01','active',$3) ON CONFLICT(user_id) DO UPDATE SET user_id=excluded.user_id RETURNING id`, uid, fmt.Sprintf("990000000000%04d", i), history).Scan(&mid)
			if e != nil {
				return e
			}
			memberIDs = append(memberIDs, mid)
		}
	}
	_, e = tx.Exec(ctx, `INSERT INTO savings_types(code,name,withdrawable,minimum_balance,required_amount) VALUES('principal','Simpanan pokok DEMO',false,0,100000),('mandatory','Simpanan wajib DEMO',false,0,50000),('voluntary','Simpanan sukarela DEMO',true,0,NULL) ON CONFLICT(code) DO NOTHING; INSERT INTO loan_products(name,min_amount,max_amount,min_tenor,max_tenor,annual_rate_bps,interest_method,penalty_daily_amount,penalty_daily_bps,configured) SELECT 'Pinjaman DEMO fiktif',100000,25000000,1,24,1200,'flat',1000,0,true WHERE NOT EXISTS(SELECT 1 FROM loan_products WHERE name='Pinjaman DEMO fiktif'); UPDATE settings SET shu_configured=true,shu_capital_bps=4000,shu_business_bps=6000 WHERE NOT shu_configured;`)
	if e != nil {
		return e
	}
	var product int64
	if e = tx.QueryRow(ctx, "SELECT id FROM loan_products WHERE name='Pinjaman DEMO fiktif' ORDER BY id LIMIT 1").Scan(&product); e != nil {
		return e
	}
	for idx, mid := range memberIDs {
		for _, entry := range []struct {
			code   string
			amount int64
			period any
		}{{"principal", 100000, "once"}, {"mandatory", 50000, history.Format("2006-01")}, {"voluntary", int64(12000000 - idx*2000000), nil}} {
			var tid, sid int64
			if e = tx.QueryRow(ctx, "SELECT id FROM savings_types WHERE code=$1", entry.code).Scan(&tid); e != nil {
				return e
			}
			ref := fmt.Sprintf("DEMO-SAVINGS-%d-%s", mid, entry.code)
			e = tx.QueryRow(ctx, `INSERT INTO savings_transactions(member_id,type_id,kind,amount,status,note,period,reference,verified_by,created_at) VALUES($1,$2,'deposit',$3,'approved','Saldo awal fiktif DEMO',$4,$5,$6,$7) RETURNING id`, mid, tid, entry.amount, entry.period, ref, userIDs["teller"], history).Scan(&sid)
			if e != nil {
				return e
			}
			if _, e = tx.Exec(ctx, `INSERT INTO savings(member_id,type_id,balance) VALUES($1,$2,$3) ON CONFLICT(member_id,type_id) DO UPDATE SET balance=savings.balance+excluded.balance`, mid, tid, entry.amount); e != nil {
				return e
			}
			if e = seedJournal(ctx, tx, fmt.Sprint("savings:", sid), "cash", "savings", entry.amount, mid, history); e != nil {
				return e
			}
		}
		disbursed := now.AddDate(0, -2, -10)
		amount := int64(6000000 - idx*3000000)
		var app, lid int64
		e = tx.QueryRow(ctx, `INSERT INTO loan_applications(member_id,product_id,amount,tenor,purpose,status,survey,recommendation,decision_reason,annual_rate_bps,interest_method,penalty_daily_amount,penalty_daily_bps,created_at) VALUES($1,$2,$3,6,'Modal usaha fiktif DEMO','disbursed','Survei DEMO: usaha fiktif, pendapatan dan kemampuan bayar disimulasikan','eligible','Disetujui untuk demonstrasi',1200,'flat',1000,0,$4) RETURNING id`, mid, product, amount, disbursed.AddDate(0, 0, -5)).Scan(&app)
		if e != nil {
			return e
		}
		e = tx.QueryRow(ctx, `INSERT INTO loans(application_id,member_id,amount,annual_rate_bps,interest_method,penalty_daily_amount,penalty_daily_bps,reference,disbursed_at) VALUES($1,$2,$3,1200,'flat',1000,0,$4,$5) RETURNING id`, app, mid, amount, fmt.Sprintf("DEMO-LOAN-%d", mid), disbursed).Scan(&lid)
		if e != nil {
			return e
		}
		if e = seedJournal(ctx, tx, fmt.Sprint("loan:", lid), "loan_receivable", "cash", amount, mid, disbursed); e != nil {
			return e
		}
		schedule, err := finance.Schedule(amount, 6, 1200, "flat", disbursed)
		if err != nil {
			return err
		}
		for _, inst := range schedule {
			status := "unpaid"
			var paidAt any
			penalty := finance.Penalty(inst.Principal, 1000, 0, inst.DueDate, now)
			if inst.Number == 1 {
				status = "paid"
				paidAt = inst.DueDate
				penalty = 0
			}
			var iid int64
			e = tx.QueryRow(ctx, `INSERT INTO installments(loan_id,number,due_date,principal,interest,penalty,status,paid_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8) RETURNING id`, lid, inst.Number, inst.DueDate, inst.Principal, inst.Interest, penalty, status, paidAt).Scan(&iid)
			if e != nil {
				return e
			}
			if inst.Number == 1 {
				var pid int64
				e = tx.QueryRow(ctx, `INSERT INTO payments(installment_id,amount,status,reference,created_at,verified_at) VALUES($1,$2,'approved',$3,$4,$4) RETURNING id`, iid, inst.Total, fmt.Sprintf("DEMO-PAYMENT-%d", mid), inst.DueDate).Scan(&pid)
				if e != nil {
					return e
				}
				if e = seedJournal(ctx, tx, fmt.Sprint("payment:", pid), "cash", "loan_receivable", inst.Principal, mid, inst.DueDate); e != nil {
					return e
				}
				if e = seedJournal(ctx, tx, fmt.Sprint("payment:", pid), "cash", "interest_income", inst.Interest, mid, inst.DueDate); e != nil {
					return e
				}
			}
		}
	}
	// A completed prior-year loan supplies actual paid interest for the draft
	// SHU period; every receipt has a matching balanced journal entry.
	historicalDate := time.Date(now.Year()-1, time.July, 15, 9, 0, 0, 0, now.Location())
	var historicalApp, historicalLoan int64
	e = tx.QueryRow(ctx, `INSERT INTO loan_applications(member_id,product_id,amount,tenor,purpose,status,survey,recommendation,decision_reason,annual_rate_bps,interest_method,penalty_daily_amount,penalty_daily_bps,created_at) VALUES($1,$2,1200000,3,'Riwayat pinjaman lunas fiktif DEMO untuk perhitungan SHU','disbursed','Survei historis fiktif DEMO','eligible','Persetujuan fiktif DEMO',1200,'flat',1000,0,$3) RETURNING id`, memberIDs[0], product, historicalDate.AddDate(0, 0, -5)).Scan(&historicalApp)
	if e != nil {
		return e
	}
	e = tx.QueryRow(ctx, `INSERT INTO loans(application_id,member_id,amount,status,annual_rate_bps,interest_method,penalty_daily_amount,penalty_daily_bps,reference,disbursed_at) VALUES($1,$2,1200000,'paid',1200,'flat',1000,0,'DEMO-HISTORICAL-LOAN',$3) RETURNING id`, historicalApp, memberIDs[0], historicalDate).Scan(&historicalLoan)
	if e != nil {
		return e
	}
	if e = seedJournal(ctx, tx, fmt.Sprint("loan:", historicalLoan), "loan_receivable", "cash", 1200000, memberIDs[0], historicalDate); e != nil {
		return e
	}
	historicalSchedule, err := finance.Schedule(1200000, 3, 1200, "flat", historicalDate)
	if err != nil {
		return err
	}
	for _, inst := range historicalSchedule {
		var iid, pid int64
		e = tx.QueryRow(ctx, `INSERT INTO installments(loan_id,number,due_date,principal,interest,penalty,status,paid_at) VALUES($1,$2,($3::timestamptz)::date,$4,$5,0,'paid',$3::timestamptz) RETURNING id`, historicalLoan, inst.Number, inst.DueDate, inst.Principal, inst.Interest).Scan(&iid)
		if e != nil {
			return e
		}
		e = tx.QueryRow(ctx, `INSERT INTO payments(installment_id,amount,status,reference,created_at,verified_at) VALUES($1,$2,'approved',$3,$4,$4) RETURNING id`, iid, inst.Total, fmt.Sprintf("DEMO-HISTORICAL-PAYMENT-%d", inst.Number), inst.DueDate).Scan(&pid)
		if e != nil {
			return e
		}
		if e = seedJournal(ctx, tx, fmt.Sprint("payment:", pid), "cash", "loan_receivable", inst.Principal, memberIDs[0], inst.DueDate); e != nil {
			return e
		}
		if e = seedJournal(ctx, tx, fmt.Sprint("payment:", pid), "cash", "interest_income", inst.Interest, memberIDs[0], inst.DueDate); e != nil {
			return e
		}
	}
	_, e = tx.Exec(ctx, `INSERT INTO loan_applications(member_id,product_id,amount,tenor,purpose,status,annual_rate_bps,interest_method,penalty_daily_amount,penalty_daily_bps) VALUES($1,$2,2000000,6,'Pengajuan baru fiktif DEMO untuk alur survei','submitted',1200,'flat',1000,0)`, memberIDs[1], product)
	if e != nil {
		return e
	}
	_, e = tx.Exec(ctx, `INSERT INTO savings_transactions(member_id,type_id,kind,amount,note,reference) SELECT $1,id,'deposit',250000,'Setoran menunggu verifikasi DEMO','DEMO-PENDING-DEPOSIT' FROM savings_types WHERE code='voluntary'`, memberIDs[0])
	if e != nil {
		return e
	}
	_, e = tx.Exec(ctx, `INSERT INTO shu_periods(year,status) VALUES($1,'draft') ON CONFLICT(year) DO NOTHING`, now.Year()-1)
	if e != nil {
		return e
	}
	for _, uid := range userIDs {
		_, e = tx.Exec(ctx, `INSERT INTO notifications(user_id,title,message) VALUES($1,'Selamat datang di DEMO KopLink','Semua nama, saldo, kebijakan, dan riwayat dalam seed ini adalah data fiktif untuk demonstrasi. Gunakan alur verifikasi manual untuk mencoba transaksi.')`, uid)
		if e != nil {
			return e
		}
	}
	_, e = tx.Exec(ctx, `INSERT INTO audit_logs(actor_id,action,entity,"after") VALUES($1,'demo_seed_v1','demo_seed','{"fictional":true,"description":"Atomic initial demonstration dataset with balanced ledger"}')`, userIDs["admin"])
	if e != nil {
		return e
	}
	return tx.Commit(ctx)
}

func seedJournal(ctx context.Context, tx pgx.Tx, ref, debit, credit string, amount, mid int64, date time.Time) error {
	if amount == 0 {
		return nil
	}
	_, e := tx.Exec(ctx, `INSERT INTO ledger(reference,account,debit,credit,member_id,description,created_at) VALUES($1,$2,$4,0,$5,'Transaksi fiktif DEMO',$6),($1,$3,0,$4,$5,'Transaksi fiktif DEMO',$6)`, ref, debit, credit, amount, mid, date)
	return e
}
