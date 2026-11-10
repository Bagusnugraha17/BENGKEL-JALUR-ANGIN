// Package finance contains deterministic integer-Rupiah calculations. It never
// reads mutable policy or wall-clock time: callers pass the frozen loan policy.
package finance

import (
	"errors"
	"math"
	"math/big"
	"time"
)

const MaxAmount int64 = 1000000000000
const MaxAnnualRateBPS int64 = 100000
const MaxTenor = 120

type Installment struct {
	Number    int       `json:"number"`
	DueDate   time.Time `json:"due_date"`
	Principal int64     `json:"principal"`
	Interest  int64     `json:"interest"`
	Penalty   int64     `json:"penalty"`
	Total     int64     `json:"total"`
}

// Schedule divides principal into equal integer portions, allocating the
// remainder to the final installment. Due dates keep the original anchor day,
// clamping independently for each month, so January 31 -> February 28 -> March 31.
func Schedule(amount int64, tenor int, rate int64, method string, start time.Time) ([]Installment, error) {
	if amount <= 0 || amount > MaxAmount || tenor < 1 || tenor > MaxTenor || amount < int64(tenor) || rate < 0 || rate > MaxAnnualRateBPS || (method != "flat" && method != "declining" && method != "annuity") {
		return nil, errors.New("invalid loan amount, tenor, annual rate, or interest method")
	}
	rows := make([]Installment, 0, tenor)
	base := amount / int64(tenor)
	remaining := amount
	annuityPayment := int64(0)
	if method == "annuity" {
		if rate == 0 {
			annuityPayment = (amount + int64(tenor) - 1) / int64(tenor)
		} else {
			monthly := float64(rate) / 120000.0
			power := math.Pow(1+monthly, float64(tenor))
			payment := float64(amount) * monthly * power / (power - 1)
			if math.IsInf(payment, 0) || math.IsNaN(payment) || payment > float64(math.MaxInt64) {
				return nil, errors.New("annuity payment overflow")
			}
			annuityPayment = int64(math.Round(payment))
		}
	}
	for n := 1; n <= tenor; n++ {
		principal := base
		interestBase := amount
		if method == "declining" {
			interestBase = remaining
		}
		interest := roundRatio(interestBase, rate, 120000)
		if method == "annuity" {
			principal = annuityPayment - interest
			if n == tenor || principal > remaining {
				principal = remaining
			}
			if principal <= 0 {
				return nil, errors.New("annuity principal amortization is not positive")
			}
		}
		if n == tenor {
			principal = remaining
		}
		rows = append(rows, Installment{Number: n, DueDate: AddMonthsClamped(start, n), Principal: principal, Interest: interest, Total: principal + interest})
		remaining -= principal
	}
	return rows, nil
}

// AddMonthsClamped returns midnight in the input location on the same civil day
// in the requested month, or that month's last day if the day does not exist.
func AddMonthsClamped(start time.Time, months int) time.Time {
	first := time.Date(start.Year(), start.Month()+time.Month(months), 1, 0, 0, 0, 0, start.Location())
	last := first.AddDate(0, 1, -1).Day()
	day := start.Day()
	if day > last {
		day = last
	}
	return time.Date(first.Year(), first.Month(), day, 0, 0, 0, 0, start.Location())
}

// Penalty returns the total penalty as of now, not an incremental accrual. The
// due day is exempt; the first following civil day incurs one day's penalty.
// Fixed daily amount takes precedence over basis points; policy validation at
// configuration time should reject both nonzero. Repeating this function with
// the same inputs cannot compound or double-count penalties.
func Penalty(principal, dailyAmount, dailyBPS int64, due, now time.Time) int64 {
	if principal <= 0 || dailyAmount < 0 || dailyBPS < 0 {
		return 0
	}
	now = now.In(due.Location())
	dueDay := time.Date(due.Year(), due.Month(), due.Day(), 0, 0, 0, 0, time.UTC)
	nowDay := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	days := (nowDay.Unix() - dueDay.Unix()) / 86400
	if days <= 0 {
		return 0
	}
	daily := dailyAmount
	if daily == 0 {
		daily = roundRatio(principal, dailyBPS, 10000)
	}
	return roundRatio(daily, days, 1)
}

// roundRatio computes a*b/d with positive half-up rounding. big.Int avoids
// intermediate overflow; saturation prevents wraparound on hostile inputs.
func roundRatio(a, b, d int64) int64 {
	if a < 0 || b < 0 || d <= 0 {
		return 0
	}
	var value, other big.Int
	value.SetInt64(a)
	other.SetInt64(b)
	value.Mul(&value, &other)
	other.SetInt64(d / 2)
	value.Add(&value, &other)
	other.SetInt64(d)
	value.Quo(&value, &other)
	if !value.IsInt64() {
		return math.MaxInt64
	}
	return value.Int64()
}
