package pgledger_test

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rustambekabdusalomov/agnts-muse/budget"
	"github.com/rustambekabdusalomov/agnts-muse/budget/ledgertest"
	"github.com/rustambekabdusalomov/agnts-muse/ledger/pgledger"
	"github.com/rustambekabdusalomov/agnts-muse/money"
)

// The tests need a database they may wipe:
//
//	AGNTS_MUSE_TEST_DATABASE_URL=postgres://user:pass@localhost:5432/agnts_muse_test go test ./...
func pool(t testing.TB) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("AGNTS_MUSE_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("AGNTS_MUSE_TEST_DATABASE_URL is not set")
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 32
	p, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

func reset(t testing.TB, p *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	if _, err := p.Exec(ctx, `DROP TABLE IF EXISTS muse_entries, muse_hold_legs, muse_holds, muse_accounts`); err != nil {
		t.Fatal(err)
	}
	if err := pgledger.New(p).Migrate(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestConformance(t *testing.T) {
	p := pool(t)
	ledgertest.Run(t, func(t *testing.T) (budget.Store, *ledgertest.Clock) {
		reset(t, p)
		c := ledgertest.NewClock()
		return pgledger.New(p, pgledger.WithClock(c.Now)), c
	})
}

func TestMigrateIsIdempotent(t *testing.T) {
	p := pool(t)
	reset(t, p)
	if err := pgledger.New(p).Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestDatabaseRefusesOverspend writes around the ledger, as a buggy or
// hostile client might, and checks the constraint stops it.
func TestDatabaseRefusesOverspend(t *testing.T) {
	p := pool(t)
	reset(t, p)
	ctx := context.Background()
	l := pgledger.New(p)
	if _, err := l.Deposit(ctx, "ws", money.Dollar, "d"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Exec(ctx, `UPDATE muse_accounts SET held = held + $1 WHERE id = 'ws'`, int64(2*money.Dollar)); err == nil {
		t.Fatal("the database let an account reserve more than it has")
	}
}

func BenchmarkHoldCommit(b *testing.B) {
	p := pool(b)
	reset(b, p)
	ctx := context.Background()
	l := pgledger.New(p)
	const accounts = 64
	for i := range accounts {
		if _, err := l.Deposit(ctx, budget.AccountID(fmt.Sprintf("ws:%d", i)), money.USD(1<<50), fmt.Sprint("seed", i)); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportAllocs()
	var n atomic.Int64
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			i := n.Add(1)
			acct := budget.AccountID(fmt.Sprintf("ws:%d", i%accounts))
			h, err := l.Hold(ctx, budget.HoldRequest{Key: fmt.Sprint("b", i), Payers: []budget.AccountID{acct}, Amount: money.Cent})
			if err != nil {
				b.Fatal(err)
			}
			if _, err := l.Commit(ctx, h.ID, money.Cent/2); err != nil {
				b.Fatal(err)
			}
		}
	})
}
