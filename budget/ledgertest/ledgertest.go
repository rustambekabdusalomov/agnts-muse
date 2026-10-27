// Package ledgertest is the conformance suite for budget.Store
// implementations. An implementation's tests call Run with a constructor
// that returns an empty store (and a clock the suite can move, for expiry):
//
//	func TestConformance(t *testing.T) {
//		ledgertest.Run(t, func(t *testing.T) (budget.Store, *ledgertest.Clock) {
//			c := ledgertest.NewClock()
//			return memledger.New(memledger.WithClock(c.Now)), c
//		})
//	}
package ledgertest

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rustambekabdusalomov/agnts-muse/budget"
	"github.com/rustambekabdusalomov/agnts-muse/money"
)

// Clock is a manual clock for hold expiry.
type Clock struct {
	mu sync.Mutex
	t  time.Time
}

// NewClock returns a clock at a fixed instant.
func NewClock() *Clock { return &Clock{t: time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)} }

// Now returns the clock's time.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

// Advance moves the clock forward.
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// New makes an empty store for one subtest.
type New func(t *testing.T) (budget.Store, *Clock)

// Run runs the suite.
func Run(t *testing.T, newStore New) {
	t.Helper()
	for _, tc := range []struct {
		name string
		fn   func(*testing.T, budget.Store, *Clock)
	}{
		{"HoldCommitRelease", holdCommitRelease},
		{"PayerOrder", payerOrder},
		{"Insufficient", insufficient},
		{"Caps", caps},
		{"Extend", extend},
		{"CommitOverHold", commitOverHold},
		{"Idempotency", idempotency},
		{"ClosedHolds", closedHolds},
		{"Expiry", expiry},
		{"Deposits", deposits},
		{"Invalid", invalid},
		{"ConcurrentNeverOverspends", concurrent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, c := newStore(t)
			tc.fn(t, s, c)
		})
	}
}

var ctx = context.Background()

func fund(t *testing.T, s budget.Store, id budget.AccountID, amount money.USD) {
	t.Helper()
	if _, err := s.Deposit(ctx, id, amount, "fund:"+string(id)+":"+amount.String()); err != nil {
		t.Fatalf("deposit %s: %v", id, err)
	}
}

func balance(t *testing.T, s budget.Store, id budget.AccountID) budget.Balance {
	t.Helper()
	b, err := s.Balance(ctx, id)
	if err != nil {
		t.Fatalf("balance %s: %v", id, err)
	}
	return b
}

func want(t *testing.T, s budget.Store, id budget.AccountID, total, held money.USD) {
	t.Helper()
	b := balance(t, s, id)
	if b.Total != total || b.Held != held || b.Available != total-held {
		t.Fatalf("%s = %+v, want total %v held %v", id, b, total, held)
	}
}

func holdOK(t *testing.T, s budget.Store, req budget.HoldRequest) budget.Hold {
	t.Helper()
	h, err := s.Hold(ctx, req)
	if err != nil {
		t.Fatalf("hold %+v: %v", req, err)
	}
	return h
}

const (
	user budget.AccountID = "user:1:free"
	ws   budget.AccountID = "ws:1:paid"
	capA budget.AccountID = "cap:global"
)

func holdCommitRelease(t *testing.T, s budget.Store, _ *Clock) {
	fund(t, s, ws, money.Dollar)
	h := holdOK(t, s, budget.HoldRequest{Key: "run-1", Payers: []budget.AccountID{ws}, Amount: 30 * money.Cent})
	if h.State != budget.StateOpen || h.Amount != 30*money.Cent || h.ID == "" {
		t.Fatalf("hold = %+v", h)
	}
	want(t, s, ws, money.Dollar, 30*money.Cent)
	r, err := s.Commit(ctx, h.ID, 12*money.Cent)
	if err != nil {
		t.Fatal(err)
	}
	if r.Charged != 12*money.Cent || r.Released != 18*money.Cent || r.Unbilled != 0 {
		t.Fatalf("receipt = %+v", r)
	}
	want(t, s, ws, 88*money.Cent, 0)

	h2 := holdOK(t, s, budget.HoldRequest{Key: "run-2", Payers: []budget.AccountID{ws}, Amount: 50 * money.Cent})
	if err := s.Release(ctx, h2.ID); err != nil {
		t.Fatal(err)
	}
	want(t, s, ws, 88*money.Cent, 0)
}

func payerOrder(t *testing.T, s budget.Store, _ *Clock) {
	fund(t, s, user, 20*money.Cent)
	fund(t, s, ws, money.Dollar)
	h := holdOK(t, s, budget.HoldRequest{Key: "k", Payers: []budget.AccountID{user, ws}, Amount: 50 * money.Cent})
	if h.Legs[0].Held != 20*money.Cent || h.Legs[1].Held != 30*money.Cent {
		t.Fatalf("legs = %+v, want free credits first", h.Legs)
	}
	// Spending 15¢ comes out of the free credits only.
	r, err := s.Commit(ctx, h.ID, 15*money.Cent)
	if err != nil {
		t.Fatal(err)
	}
	if r.Legs[0].Charged != 15*money.Cent || r.Legs[1].Charged != 0 {
		t.Fatalf("legs = %+v", r.Legs)
	}
	want(t, s, user, 5*money.Cent, 0)
	want(t, s, ws, money.Dollar, 0)
}

func insufficient(t *testing.T, s budget.Store, _ *Clock) {
	fund(t, s, user, 10*money.Cent)
	fund(t, s, ws, 10*money.Cent)
	_, err := s.Hold(ctx, budget.HoldRequest{Key: "k", Payers: []budget.AccountID{user, ws}, Amount: 21 * money.Cent})
	if !errors.Is(err, budget.ErrInsufficient) {
		t.Fatalf("err = %v, want ErrInsufficient", err)
	}
	// Nothing was reserved by the failed hold.
	want(t, s, user, 10*money.Cent, 0)
	want(t, s, ws, 10*money.Cent, 0)
	// An account nobody funded has nothing.
	if _, err := s.Hold(ctx, budget.HoldRequest{Key: "k2", Payers: []budget.AccountID{"ws:none"}, Amount: 1}); !errors.Is(err, budget.ErrInsufficient) {
		t.Fatalf("err = %v, want ErrInsufficient", err)
	}
}

func caps(t *testing.T, s budget.Store, _ *Clock) {
	fund(t, s, ws, 10*money.Dollar)
	fund(t, s, capA, money.Dollar)
	h := holdOK(t, s, budget.HoldRequest{Key: "a", Payers: []budget.AccountID{ws}, Caps: []budget.AccountID{capA}, Amount: 80 * money.Cent})
	if _, err := s.Hold(ctx, budget.HoldRequest{Key: "b", Payers: []budget.AccountID{ws}, Caps: []budget.AccountID{capA}, Amount: 30 * money.Cent}); !errors.Is(err, budget.ErrCapReached) {
		t.Fatalf("err = %v, want ErrCapReached", err)
	}
	want(t, s, ws, 10*money.Dollar, 80*money.Cent)
	if _, err := s.Commit(ctx, h.ID, 60*money.Cent); err != nil {
		t.Fatal(err)
	}
	// The cap counted the spend; the payer paid it.
	want(t, s, capA, 40*money.Cent, 0)
	want(t, s, ws, 9*money.Dollar+40*money.Cent, 0)
}

func extend(t *testing.T, s budget.Store, c *Clock) {
	fund(t, s, user, 10*money.Cent)
	fund(t, s, ws, 30*money.Cent)
	fund(t, s, capA, money.Dollar)
	req := budget.HoldRequest{Key: "k", Payers: []budget.AccountID{user, ws}, Caps: []budget.AccountID{capA}, Amount: 5 * money.Cent, TTL: time.Minute}
	h := holdOK(t, s, req)
	c.Advance(50 * time.Second)
	h, err := s.Extend(ctx, h.ID, 20*money.Cent)
	if err != nil {
		t.Fatal(err)
	}
	if h.Amount != 25*money.Cent || h.Legs[0].Held != 10*money.Cent || h.Legs[1].Held != 15*money.Cent || h.Legs[2].Held != 25*money.Cent {
		t.Fatalf("hold = %+v", h)
	}
	// Extend renewed the TTL: 50s later the hold is still open.
	c.Advance(50 * time.Second)
	if _, err := s.Extend(ctx, h.ID, 30*money.Cent); !errors.Is(err, budget.ErrInsufficient) {
		t.Fatalf("err = %v, want ErrInsufficient (15¢ left)", err)
	}
	h, err = s.Extend(ctx, h.ID, 15*money.Cent)
	if err != nil || h.Amount != 40*money.Cent {
		t.Fatalf("hold = %+v, %v", h, err)
	}
	if _, err := s.Extend(ctx, h.ID, 0); !errors.Is(err, budget.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
}

func commitOverHold(t *testing.T, s budget.Store, _ *Clock) {
	fund(t, s, ws, money.Dollar)
	h := holdOK(t, s, budget.HoldRequest{Key: "k", Payers: []budget.AccountID{ws}, Amount: 10 * money.Cent})
	r, err := s.Commit(ctx, h.ID, 13*money.Cent)
	if err != nil {
		t.Fatal(err)
	}
	if r.Charged != 10*money.Cent || r.Unbilled != 3*money.Cent || r.Released != 0 {
		t.Fatalf("receipt = %+v", r)
	}
	want(t, s, ws, 90*money.Cent, 0)
}

func idempotency(t *testing.T, s budget.Store, _ *Clock) {
	fund(t, s, ws, money.Dollar)
	req := budget.HoldRequest{Key: "run-7", Payers: []budget.AccountID{ws}, Amount: 40 * money.Cent}
	a := holdOK(t, s, req)
	b := holdOK(t, s, req)
	if a.ID != b.ID {
		t.Fatalf("same key gave holds %s and %s", a.ID, b.ID)
	}
	want(t, s, ws, money.Dollar, 40*money.Cent)
	r1, err := s.Commit(ctx, a.ID, 25*money.Cent)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := s.Commit(ctx, a.ID, 99*money.Cent)
	if err != nil || r2.Charged != r1.Charged || r2.Released != r1.Released {
		t.Fatalf("second commit = %+v, %v; want %+v", r2, err, r1)
	}
	want(t, s, ws, 75*money.Cent, 0)
}

func closedHolds(t *testing.T, s budget.Store, _ *Clock) {
	fund(t, s, ws, money.Dollar)
	h := holdOK(t, s, budget.HoldRequest{Key: "k", Payers: []budget.AccountID{ws}, Amount: 10 * money.Cent})
	if _, err := s.Commit(ctx, h.ID, 5*money.Cent); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Extend(ctx, h.ID, money.Cent); !errors.Is(err, budget.ErrClosed) {
		t.Fatalf("extend committed: %v, want ErrClosed", err)
	}
	if err := s.Release(ctx, h.ID); !errors.Is(err, budget.ErrClosed) {
		t.Fatalf("release committed: %v, want ErrClosed", err)
	}
	r := holdOK(t, s, budget.HoldRequest{Key: "r", Payers: []budget.AccountID{ws}, Amount: 10 * money.Cent})
	if err := s.Release(ctx, r.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Release(ctx, r.ID); err != nil {
		t.Fatalf("second release: %v", err)
	}
	if _, err := s.Commit(ctx, r.ID, money.Cent); !errors.Is(err, budget.ErrClosed) {
		t.Fatalf("commit released: %v, want ErrClosed", err)
	}
	if _, err := s.Commit(ctx, "missing", 1); !errors.Is(err, budget.ErrNotFound) {
		t.Fatalf("commit missing: %v, want ErrNotFound", err)
	}
	want(t, s, ws, 95*money.Cent, 0)
}

func expiry(t *testing.T, s budget.Store, c *Clock) {
	fund(t, s, ws, money.Dollar)
	a := holdOK(t, s, budget.HoldRequest{Key: "a", Payers: []budget.AccountID{ws}, Amount: 30 * money.Cent, TTL: time.Minute})
	holdOK(t, s, budget.HoldRequest{Key: "b", Payers: []budget.AccountID{ws}, Amount: 30 * money.Cent, TTL: time.Hour})
	c.Advance(2 * time.Minute)
	n, err := s.Reap(ctx, c.Now())
	if err != nil || n != 1 {
		t.Fatalf("reap = %d, %v; want 1", n, err)
	}
	want(t, s, ws, money.Dollar, 30*money.Cent)
	if _, err := s.Commit(ctx, a.ID, money.Cent); !errors.Is(err, budget.ErrClosed) {
		t.Fatalf("commit expired: %v, want ErrClosed", err)
	}
	// An expired hold is closed even if nobody reaped it yet.
	d := holdOK(t, s, budget.HoldRequest{Key: "d", Payers: []budget.AccountID{ws}, Amount: 10 * money.Cent, TTL: time.Minute})
	c.Advance(2 * time.Minute)
	if _, err := s.Extend(ctx, d.ID, money.Cent); !errors.Is(err, budget.ErrClosed) {
		t.Fatalf("extend expired: %v, want ErrClosed", err)
	}
}

func deposits(t *testing.T, s budget.Store, _ *Clock) {
	b, err := s.Deposit(ctx, ws, money.Dollar, "pay-1")
	if err != nil || b.Total != money.Dollar {
		t.Fatalf("deposit = %+v, %v", b, err)
	}
	if b, _ = s.Deposit(ctx, ws, money.Dollar, "pay-1"); b.Total != money.Dollar {
		t.Fatalf("repeated deposit counted twice: %+v", b)
	}
	holdOK(t, s, budget.HoldRequest{Key: "k", Payers: []budget.AccountID{ws}, Amount: 70 * money.Cent})
	if _, err := s.Deposit(ctx, ws, -40*money.Cent, "refund-1"); !errors.Is(err, budget.ErrInsufficient) {
		t.Fatalf("withdrawal under holds: %v, want ErrInsufficient", err)
	}
	if b, err = s.Deposit(ctx, ws, -30*money.Cent, "refund-2"); err != nil || b.Total != 70*money.Cent {
		t.Fatalf("withdrawal = %+v, %v", b, err)
	}
	if b := balance(t, s, "ws:unknown"); b != (budget.Balance{}) {
		t.Fatalf("unknown account = %+v", b)
	}
}

func invalid(t *testing.T, s budget.Store, _ *Clock) {
	for _, req := range []budget.HoldRequest{
		{Payers: []budget.AccountID{ws}, Amount: 1},
		{Key: "k", Amount: 1},
		{Key: "k", Payers: []budget.AccountID{ws}},
		{Key: "k", Payers: []budget.AccountID{ws, ws}, Amount: 1},
		{Key: "k", Payers: []budget.AccountID{ws}, Caps: []budget.AccountID{ws}, Amount: 1},
	} {
		if _, err := s.Hold(ctx, req); !errors.Is(err, budget.ErrInvalid) {
			t.Errorf("Hold(%+v) = %v, want ErrInvalid", req, err)
		}
	}
}

// concurrent runs many goroutines that hold, extend and commit against one
// small budget at once, and checks that the payers' money adds up exactly
// and never goes below zero.
func concurrent(t *testing.T, s budget.Store, _ *Clock) {
	const workers = 100
	fund(t, s, user, 25*money.Cent)
	fund(t, s, ws, 75*money.Cent)
	fund(t, s, capA, 10*money.Dollar)
	var charged, unbilled atomic.Int64
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rng := rand.New(rand.NewPCG(uint64(i), 7)) //nolint:gosec // reproducible test schedule, not security
			h, err := s.Hold(ctx, budget.HoldRequest{Key: fmt.Sprintf("run-%d", i), Payers: []budget.AccountID{user, ws},
				Caps: []budget.AccountID{capA}, Amount: money.USD(rng.IntN(4)+1) * money.Cent})
			if errors.Is(err, budget.ErrInsufficient) {
				return
			}
			if err != nil {
				errs <- err
				return
			}
			for range rng.IntN(4) {
				if _, err := s.Extend(ctx, h.ID, money.USD(rng.IntN(3)+1)*money.Cent); err != nil && !errors.Is(err, budget.ErrInsufficient) {
					errs <- err
					return
				}
			}
			if rng.IntN(5) == 0 {
				if err := s.Release(ctx, h.ID); err != nil {
					errs <- err
				}
				return
			}
			r, err := s.Commit(ctx, h.ID, money.USD(rng.IntN(12))*money.Cent)
			if err != nil {
				errs <- err
				return
			}
			charged.Add(int64(r.Charged))
			unbilled.Add(int64(r.Unbilled))
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	u, w := balance(t, s, user), balance(t, s, ws)
	if u.Held != 0 || w.Held != 0 {
		t.Fatalf("holds left open: user %+v ws %+v", u, w)
	}
	if u.Total < 0 || w.Total < 0 {
		t.Fatalf("overspent: user %+v ws %+v", u, w)
	}
	spent := money.Dollar - u.Total - w.Total
	if spent != money.USD(charged.Load()) {
		t.Fatalf("payers lost %v but receipts charged %v", spent, money.USD(charged.Load()))
	}
	if c := balance(t, s, capA); c.Total != 10*money.Dollar-spent || c.Held != 0 {
		t.Fatalf("cap = %+v, want %v counted", c, spent)
	}
	t.Logf("charged %v, unbilled %v across %d runs", spent, money.USD(unbilled.Load()), workers)
}
