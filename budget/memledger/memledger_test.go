package memledger_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/rustambekabdusalomov/agnts-muse/budget"
	"github.com/rustambekabdusalomov/agnts-muse/budget/ledgertest"
	"github.com/rustambekabdusalomov/agnts-muse/budget/memledger"
	"github.com/rustambekabdusalomov/agnts-muse/money"
)

func TestConformance(t *testing.T) {
	ledgertest.Run(t, func(t *testing.T) (budget.Store, *ledgertest.Clock) {
		c := ledgertest.NewClock()
		return memledger.New(memledger.WithClock(c.Now)), c
	})
}

func BenchmarkHoldCommit(b *testing.B) {
	ctx := context.Background()
	l := memledger.New()
	if _, err := l.Deposit(ctx, "ws", money.USD(1<<60), "seed"); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for n := 0; b.Loop(); n++ {
		h, err := l.Hold(ctx, budget.HoldRequest{Key: fmt.Sprintf("k%d", n), Payers: []budget.AccountID{"ws"}, Amount: money.Cent})
		if err != nil {
			b.Fatal(err)
		}
		if _, err := l.Commit(ctx, h.ID, money.Cent/2); err != nil {
			b.Fatal(err)
		}
	}
}

func ExampleLedger() {
	ctx := context.Background()
	l := memledger.New()
	l.Deposit(ctx, "user:42:free", 20*money.Cent, "admin-grant-1")
	l.Deposit(ctx, "ws:7:paid", 5*money.Dollar, "payme-991")

	// Reserve 50¢ for a run: free credits first, then the workspace.
	h, _ := l.Hold(ctx, budget.HoldRequest{Key: "run-1", Payers: []budget.AccountID{"user:42:free", "ws:7:paid"}, Amount: 50 * money.Cent})
	r, _ := l.Commit(ctx, h.ID, 32*money.Cent)
	fmt.Println("charged", r.Charged, "released", r.Released)
	free, _ := l.Balance(ctx, "user:42:free")
	paid, _ := l.Balance(ctx, "ws:7:paid")
	fmt.Println("free", free.Total, "paid", paid.Total)
	// Output:
	// charged $0.32 released $0.18
	// free $0.00 paid $4.88
}
