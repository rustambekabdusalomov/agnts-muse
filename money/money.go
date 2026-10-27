// Package money holds the integer amounts agnts-muse bills in.
//
// No money path in this module uses floating point. Budgets, holds and
// charges are [USD], an int64 count of micro-dollars. Model prices are
// [Rate], micro-dollars per million tokens, and their products are [Exact],
// picodollars, so a per-token price never rounds. Rounding happens once,
// when an [Exact] total becomes a [USD] charge, and always upwards: a
// platform never bills less than it paid.
package money

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// USD is an amount of US dollars in micro-dollars (1e-6 USD).
type USD int64

// Common amounts.
const (
	Microdollar USD = 1
	Cent        USD = 10_000
	Dollar      USD = 1_000_000
)

// Dollars converts a float amount, rounded to the nearest micro-dollar. It
// is for configuration and tests; money arithmetic never goes through it.
func Dollars(f float64) USD { return USD(math.Round(f * float64(Dollar))) }

// Float returns u in dollars, for display and metrics only.
func (u USD) Float() float64 { return float64(u) / float64(Dollar) }

// String formats u with at least two and at most six decimals: "$0.40",
// "$1.234567", "-$0.05".
func (u USD) String() string {
	sign := ""
	v := int64(u)
	if v < 0 {
		sign, v = "-", -v
	}
	frac := strings.TrimRight(fmt.Sprintf("%06d", v%int64(Dollar)), "0")
	for len(frac) < 2 {
		frac += "0"
	}
	return fmt.Sprintf("%s$%d.%s", sign, v/int64(Dollar), frac)
}

// ErrSyntax reports an amount Parse cannot read.
var ErrSyntax = errors.New("money: invalid amount")

// Parse reads a decimal dollar amount such as "0.5", "$12.25" or
// "-$0.000001" exactly, without going through a float.
func Parse(s string) (USD, error) {
	s = strings.TrimSpace(s)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(strings.TrimPrefix(s, "-"), "$")
	whole, frac, _ := strings.Cut(s, ".")
	if whole == "" && frac == "" || len(frac) > 6 || strings.ContainsAny(whole+frac, "+-") {
		return 0, fmt.Errorf("%w: %q", ErrSyntax, s)
	}
	if whole == "" {
		whole = "0"
	}
	w, err := strconv.ParseInt(whole, 10, 64)
	if err != nil || w > math.MaxInt64/int64(Dollar)-1 {
		return 0, fmt.Errorf("%w: %q", ErrSyntax, s)
	}
	var f int64
	if frac != "" {
		if f, err = strconv.ParseInt(frac+strings.Repeat("0", 6-len(frac)), 10, 64); err != nil {
			return 0, fmt.Errorf("%w: %q", ErrSyntax, s)
		}
	}
	v := USD(w*int64(Dollar) + f)
	if neg {
		v = -v
	}
	return v, nil
}

// Rate is a price per million tokens in micro-dollars: $1.25 per 1M tokens
// is Rate(1_250_000). Numerically it is also the price of one token in
// picodollars, which is why Cost is exact.
type Rate int64

// PerMillion converts a float dollars-per-million price, for configuration.
func PerMillion(dollars float64) Rate { return Rate(math.Round(dollars * float64(Dollar))) }

// Cost is the exact price of tokens at r.
func (r Rate) Cost(tokens int64) Exact { return Exact(int64(r) * tokens) }

// String formats r as dollars per million tokens: "$1.25/M".
func (r Rate) String() string { return USD(r).String() + "/M" }

// ParseRate reads a dollars-per-million price such as "1.25".
func ParseRate(s string) (Rate, error) {
	u, err := Parse(s)
	return Rate(u), err
}

// Exact is an amount in picodollars (1e-12 USD): the product of a [Rate]
// and a token count. An int64 holds about $9.2M of it, far above any one
// run; sums across runs are kept in [USD].
type Exact int64

// picoPerMicro is how many picodollars make a micro-dollar.
const picoPerMicro = 1_000_000

// Ceil rounds e up to the next micro-dollar.
func (e Exact) Ceil() USD {
	if e <= 0 {
		return USD(e / picoPerMicro)
	}
	return USD((e + picoPerMicro - 1) / picoPerMicro)
}

// Min returns the smaller of a and b.
func Min(a, b USD) USD {
	if a < b {
		return a
	}
	return b
}

// Max returns the larger of a and b.
func Max(a, b USD) USD {
	if a > b {
		return a
	}
	return b
}
