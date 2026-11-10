package finance

import (
	"testing"
	"time"
)

func TestScheduleFlatAndDeclining(t *testing.T) {
	start := time.Date(2024, 1, 31, 10, 0, 0, 0, time.FixedZone("WITA", 8*3600))
	for _, method := range []string{"flat", "declining"} {
		rows, err := Schedule(1000001, 3, 1200, method, start)
		if err != nil {
			t.Fatal(err)
		}
		var principal int64
		expectedInterest := []int64{10000, 10000, 10000}
		if method == "declining" {
			expectedInterest = []int64{10000, 6667, 3333}
		}
		for i, row := range rows {
			principal += row.Principal
			if row.Number != i+1 || row.Interest != expectedInterest[i] || row.Total != row.Principal+row.Interest {
				t.Fatalf("bad row %d: %+v", i, row)
			}
		}
		if principal != 1000001 || rows[2].Principal != 333335 {
			t.Fatalf("principal not conserved: %d %+v", principal, rows)
		}
		if rows[0].DueDate.Format("2006-01-02") != "2024-02-29" || rows[1].DueDate.Format("2006-01-02") != "2024-03-31" || rows[2].DueDate.Format("2006-01-02") != "2024-04-30" {
			t.Fatalf("month clamping drift: %+v", rows)
		}
	}
}

func TestHalfUpAndZeroInterest(t *testing.T) {
	rows, err := Schedule(5, 1, 12000, "flat", time.Now())
	if err != nil || rows[0].Interest != 1 {
		t.Fatalf("half up: %+v %v", rows, err)
	}
	rows, err = Schedule(120, 120, 0, "declining", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Principal != 1 || r.Interest != 0 {
			t.Fatalf("unexpected zero-rate row: %+v", r)
		}
	}
}

func TestScheduleBounds(t *testing.T) {
	for _, tc := range []struct {
		amount int64
		tenor  int
		rate   int64
		method string
	}{{0, 1, 1, "flat"}, {-1, 1, 1, "flat"}, {1000000000001, 1, 1, "flat"}, {100, 0, 1, "flat"}, {100, 121, 1, "flat"}, {100, 1, -1, "flat"}, {100, 1, 100001, "flat"}, {100, 1, 1, "unknown"}, {1, 2, 0, "flat"}} {
		if _, err := Schedule(tc.amount, tc.tenor, tc.rate, tc.method, time.Now()); err == nil {
			t.Fatalf("accepted invalid input: %+v", tc)
		}
	}
	rows, err := Schedule(1000000000000, 120, 100000, "declining", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var principal int64
	for _, r := range rows {
		principal += r.Principal
		if r.Total < 0 {
			t.Fatal("overflow")
		}
	}
	if principal != 1000000000000 {
		t.Fatal("lost principal")
	}
}

func TestPenaltyCivilDaysAndIdempotent(t *testing.T) {
	zone := time.FixedZone("WITA", 8*3600)
	due := time.Date(2025, 1, 10, 0, 0, 0, 0, zone)
	sameDay := time.Date(2025, 1, 10, 23, 59, 0, 0, zone)
	if got := Penalty(100000, 2000, 0, due, sameDay); got != 0 {
		t.Fatalf("penalty on due date: %d", got)
	}
	nextDay := time.Date(2025, 1, 11, 0, 1, 0, 0, zone)
	if got := Penalty(100000, 2000, 0, due, nextDay); got != 2000 {
		t.Fatalf("wrong daily fixed penalty: %d", got)
	}
	later := time.Date(2025, 1, 13, 12, 0, 0, 0, zone)
	for i := 0; i < 3; i++ {
		if got := Penalty(100001, 0, 50, due, later); got != 1500 {
			t.Fatalf("wrong percent/idempotency: %d", got)
		}
	}
	if got := Penalty(100000, 2000, 0, due, due.AddDate(0, 0, -1)); got != 0 {
		t.Fatalf("negative overdue: %d", got)
	}
}

func TestScheduleAnnuity(t *testing.T) {
	start := time.Date(2026, 1, 31, 0, 0, 0, 0, time.FixedZone("WITA", 8*3600))
	rows, err := Schedule(12000000, 12, 1200, "annuity", start)
	if err != nil {
		t.Fatal(err)
	}
	var principal int64
	var previousInterest int64 = 1 << 62
	var minPayment, maxPayment int64
	for i, row := range rows {
		principal += row.Principal
		if row.Number != i+1 || row.Total != row.Principal+row.Interest || row.Principal <= 0 {
			t.Fatalf("invalid annuity row: %+v", row)
		}
		if row.Interest >= previousInterest {
			t.Fatalf("annuity interest should decline: %+v", rows)
		}
		previousInterest = row.Interest
		if i == 0 || row.Total < minPayment {
			minPayment = row.Total
		}
		if row.Total > maxPayment {
			maxPayment = row.Total
		}
	}
	if principal != 12000000 {
		t.Fatalf("principal not conserved: %d", principal)
	}
	if maxPayment-minPayment > 2 {
		t.Fatalf("annuity payment should be approximately level: min=%d max=%d", minPayment, maxPayment)
	}
}

func TestScheduleAnnuityZeroRate(t *testing.T) {
	rows, err := Schedule(100, 4, 0, "annuity", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var principal int64
	for _, r := range rows {
		principal += r.Principal
		if r.Interest != 0 {
			t.Fatalf("unexpected interest: %+v", r)
		}
	}
	if principal != 100 {
		t.Fatalf("principal not conserved: %d", principal)
	}
}
