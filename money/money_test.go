package money

import (
	"errors"
	"testing"
)

func TestString(t *testing.T) {
	for _, tc := range []struct {
		in   USD
		want string
	}{
		{0, "$0.00"},
		{40 * Cent, "$0.40"},
		{Dollar + 234567, "$1.234567"},
		{-5 * Cent, "-$0.05"},
		{1, "$0.000001"},
		{12 * Dollar, "$12.00"},
	} {
		if got := tc.in.String(); got != tc.want {
			t.Errorf("%d.String() = %q, want %q", int64(tc.in), got, tc.want)
		}
	}
}

func TestParse(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want USD
		err  bool
	}{
		{"0.5", 50 * Cent, false},
		{"$12.25", 12*Dollar + 25*Cent, false},
		{"-$0.000001", -1, false},
		{".75", 75 * Cent, false},
		{"20", 20 * Dollar, false},
		{"1.0000001", 0, true},
		{"", 0, true},
		{"$", 0, true},
		{"1.-5", 0, true},
		{"abc", 0, true},
		{"99999999999999999999", 0, true},
	} {
		got, err := Parse(tc.in)
		if tc.err {
			if !errors.Is(err, ErrSyntax) {
				t.Errorf("Parse(%q) error = %v, want ErrSyntax", tc.in, err)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("Parse(%q) = %v, %v; want %v", tc.in, got, err, tc.want)
		}
	}
}

func TestParseRoundTrip(t *testing.T) {
	for _, u := range []USD{0, 1, 99, Cent, 123456789, -42 * Cent} {
		got, err := Parse(u.String())
		if err != nil || got != u {
			t.Errorf("Parse(%q) = %v, %v; want %v", u.String(), got, err, u)
		}
	}
}

func TestRateCostIsExact(t *testing.T) {
	spark := PerMillion(1.25)
	// One token at $1.25/M is 1.25 micro-dollars: not representable in
	// USD, exact in Exact, and rounded up only once.
	if got := spark.Cost(1); got != 1_250_000 {
		t.Fatalf("Cost(1) = %d, want 1250000 pico", got)
	}
	if got := spark.Cost(1).Ceil(); got != 2 {
		t.Fatalf("Ceil = %d, want 2", got)
	}
	// A million calls of one token each, summed exactly, cost $1.25.
	var sum Exact
	for range 1_000_000 {
		sum += spark.Cost(1)
	}
	if got := sum.Ceil(); got != Dollars(1.25) {
		t.Fatalf("sum = %v, want $1.25", got)
	}
}

func TestCeil(t *testing.T) {
	for _, tc := range []struct {
		in   Exact
		want USD
	}{{0, 0}, {1, 1}, {999_999, 1}, {1_000_000, 1}, {1_000_001, 2}, {-1_000_000, -1}} {
		if got := tc.in.Ceil(); got != tc.want {
			t.Errorf("Exact(%d).Ceil() = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func FuzzParse(f *testing.F) {
	for _, s := range []string{"0.5", "$1.25", "-$3", ".1", "1e5", "--1"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		u, err := Parse(s)
		if err != nil {
			return
		}
		back, err := Parse(u.String())
		if err != nil || back != u {
			t.Fatalf("round trip %q → %v → %v, %v", s, u, back, err)
		}
	})
}
