package budget

import (
	"errors"
	"testing"

	"github.com/rustambekabdusalomov/agnts-muse/money"
)

func TestSplit(t *testing.T) {
	for _, tc := range []struct {
		avail  []money.USD
		amount money.USD
		want   []money.USD
		ok     bool
	}{
		{[]money.USD{20, 100}, 50, []money.USD{20, 30}, true},
		{[]money.USD{0, 100}, 50, []money.USD{0, 50}, true},
		{[]money.USD{-5, 100}, 50, []money.USD{0, 50}, true}, // a negative balance gives nothing
		{[]money.USD{20, 10}, 50, []money.USD{20, 10}, false},
		{[]money.USD{60}, 50, []money.USD{50}, true},
	} {
		got, ok := Split(tc.avail, tc.amount)
		if ok != tc.ok || len(got) != len(tc.want) {
			t.Fatalf("Split(%v, %v) = %v, %v", tc.avail, tc.amount, got, ok)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("Split(%v, %v) = %v, want %v", tc.avail, tc.amount, got, tc.want)
			}
		}
	}
}

func TestSettle(t *testing.T) {
	legs := []Leg{{Account: "free", Held: 20}, {Account: "paid", Held: 30}, {Account: "cap", Cap: true, Held: 50}}
	out, charged, unbilled := Settle(legs, 35)
	if charged != 35 || unbilled != 0 || out[0].Charged != 20 || out[1].Charged != 15 || out[2].Charged != 35 {
		t.Fatalf("Settle(35) = %+v, %v, %v", out, charged, unbilled)
	}
	out, charged, unbilled = Settle(legs, 70)
	if charged != 50 || unbilled != 20 || out[1].Charged != 30 || out[2].Charged != 50 {
		t.Fatalf("Settle(70) = %+v, %v, %v", out, charged, unbilled)
	}
	if legs[0].Charged != 0 {
		t.Fatal("Settle changed its input")
	}
}

func TestValidate(t *testing.T) {
	ok := HoldRequest{Key: "k", Payers: []AccountID{"a"}, Caps: []AccountID{"c"}, Amount: 1}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	bad := ok
	bad.TTL = -1
	if err := bad.Validate(); !errors.Is(err, ErrInvalid) {
		t.Fatalf("negative TTL: %v", err)
	}
}
