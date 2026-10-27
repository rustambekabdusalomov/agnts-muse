// Package memledger is an in-memory budget.Store for tests, examples, the
// demo CLI and single-process deployments. Every operation takes one mutex,
// so it is linearizable; it keeps no history beyond what Store needs.
package memledger

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/rustambekabdusalomov/agnts-muse/budget"
	"github.com/rustambekabdusalomov/agnts-muse/money"
)

// Ledger is an in-memory budget.Store. The zero value is not usable; call New.
type Ledger struct {
	now func() time.Time

	mu       sync.Mutex
	accounts map[budget.AccountID]*account
	holds    map[budget.HoldID]*hold
	keys     map[string]budget.HoldID
	deposits map[string]budget.Balance
	receipts map[budget.HoldID]budget.Receipt
	seq      int64
}

type account struct{ total, held money.USD }

type hold struct {
	budget.Hold
	ttl time.Duration
}

var _ budget.Store = (*Ledger)(nil)

// Option configures a Ledger.
type Option func(*Ledger)

// WithClock sets the clock holds expire by (time.Now by default).
func WithClock(now func() time.Time) Option { return func(l *Ledger) { l.now = now } }

// New returns an empty ledger.
func New(opts ...Option) *Ledger {
	l := &Ledger{
		now:      time.Now,
		accounts: map[budget.AccountID]*account{},
		holds:    map[budget.HoldID]*hold{},
		keys:     map[string]budget.HoldID{},
		deposits: map[string]budget.Balance{},
		receipts: map[budget.HoldID]budget.Receipt{},
	}
	for _, o := range opts {
		o(l)
	}
	return l
}

func (l *Ledger) acct(id budget.AccountID) *account {
	a := l.accounts[id]
	if a == nil {
		a = &account{}
		l.accounts[id] = a
	}
	return a
}

func (a *account) balance() budget.Balance {
	return budget.Balance{Total: a.total, Held: a.held, Available: a.total - a.held}
}

// Deposit implements budget.Funds.
func (l *Ledger) Deposit(ctx context.Context, id budget.AccountID, amount money.USD, key string) (budget.Balance, error) {
	if err := ctx.Err(); err != nil {
		return budget.Balance{}, err
	}
	if id == "" || key == "" {
		return budget.Balance{}, fmt.Errorf("%w: deposit needs an account and a key", budget.ErrInvalid)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if b, ok := l.deposits[key]; ok {
		return b, nil
	}
	a := l.acct(id)
	if amount < 0 && a.total+amount < a.held {
		return a.balance(), fmt.Errorf("%w: %s has %v available", budget.ErrInsufficient, id, a.total-a.held)
	}
	a.total += amount
	b := a.balance()
	l.deposits[key] = b
	return b, nil
}

// Balance implements budget.Funds.
func (l *Ledger) Balance(ctx context.Context, id budget.AccountID) (budget.Balance, error) {
	if err := ctx.Err(); err != nil {
		return budget.Balance{}, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if a, ok := l.accounts[id]; ok {
		return a.balance(), nil
	}
	return budget.Balance{}, nil
}

// Hold implements budget.Ledger.
func (l *Ledger) Hold(ctx context.Context, req budget.HoldRequest) (budget.Hold, error) {
	if err := ctx.Err(); err != nil {
		return budget.Hold{}, err
	}
	if err := req.Validate(); err != nil {
		return budget.Hold{}, err
	}
	ttl := req.TTL
	if ttl == 0 {
		ttl = budget.DefaultTTL
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if id, ok := l.keys[req.Key]; ok {
		return l.holds[id].copy(), nil
	}
	legs, err := l.reserve(req.Payers, req.Caps, req.Amount)
	if err != nil {
		return budget.Hold{}, err
	}
	l.seq++
	h := &hold{Hold: budget.Hold{
		ID: budget.HoldID("h" + strconv.FormatInt(l.seq, 10)), Key: req.Key, State: budget.StateOpen,
		Amount: req.Amount, Legs: legs, ExpiresAt: l.now().Add(ttl),
	}, ttl: ttl}
	l.holds[h.ID] = h
	l.keys[h.Key] = h.ID
	return h.copy(), nil
}

// reserve allocates amount across payers in order and against every cap,
// all or nothing.
func (l *Ledger) reserve(payers, caps []budget.AccountID, amount money.USD) ([]budget.Leg, error) {
	avail := make([]money.USD, len(payers))
	for i, p := range payers {
		avail[i] = l.acct(p).balance().Available
	}
	parts, ok := budget.Split(avail, amount)
	if !ok {
		return nil, fmt.Errorf("%w: need %v", budget.ErrInsufficient, amount)
	}
	for _, c := range caps {
		if l.acct(c).balance().Available < amount {
			return nil, fmt.Errorf("%w: %s", budget.ErrCapReached, c)
		}
	}
	legs := make([]budget.Leg, 0, len(payers)+len(caps))
	for i, p := range payers {
		l.acct(p).held += parts[i]
		legs = append(legs, budget.Leg{Account: p, Held: parts[i]})
	}
	for _, c := range caps {
		l.acct(c).held += amount
		legs = append(legs, budget.Leg{Account: c, Cap: true, Held: amount})
	}
	return legs, nil
}

// Extend implements budget.Ledger.
func (l *Ledger) Extend(ctx context.Context, id budget.HoldID, by money.USD) (budget.Hold, error) {
	if err := ctx.Err(); err != nil {
		return budget.Hold{}, err
	}
	if by <= 0 {
		return budget.Hold{}, fmt.Errorf("%w: extend by %v", budget.ErrInvalid, by)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	h, err := l.open(id)
	if err != nil {
		return budget.Hold{}, err
	}
	var payers, caps []budget.AccountID
	for _, leg := range h.Legs {
		if leg.Cap {
			caps = append(caps, leg.Account)
		} else {
			payers = append(payers, leg.Account)
		}
	}
	more, err := l.reserve(payers, caps, by)
	if err != nil {
		return h.copy(), err
	}
	for i := range h.Legs {
		h.Legs[i].Held += more[i].Held // reserve keeps the same leg order
	}
	h.Amount += by
	h.ExpiresAt = l.now().Add(h.ttl)
	return h.copy(), nil
}

// Commit implements budget.Ledger.
func (l *Ledger) Commit(ctx context.Context, id budget.HoldID, actual money.USD) (budget.Receipt, error) {
	if err := ctx.Err(); err != nil {
		return budget.Receipt{}, err
	}
	if actual < 0 {
		return budget.Receipt{}, fmt.Errorf("%w: commit %v", budget.ErrInvalid, actual)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if r, ok := l.receipts[id]; ok {
		return r, nil
	}
	h, err := l.open(id)
	if err != nil {
		return budget.Receipt{}, err
	}
	legs, charged, unbilled := budget.Settle(h.Legs, actual)
	for _, leg := range legs {
		a := l.acct(leg.Account)
		a.held -= leg.Held
		a.total -= leg.Charged
	}
	h.Legs, h.State = legs, budget.StateCommitted
	r := budget.Receipt{HoldID: id, Key: h.Key, Charged: charged, Released: h.Amount - charged, Unbilled: unbilled,
		Legs: append([]budget.Leg(nil), legs...)}
	l.receipts[id] = r
	return r, nil
}

// Release implements budget.Ledger.
func (l *Ledger) Release(ctx context.Context, id budget.HoldID) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	h, ok := l.holds[id]
	switch {
	case !ok:
		return fmt.Errorf("%w: hold %s", budget.ErrNotFound, id)
	case h.State == budget.StateReleased || h.State == budget.StateExpired:
		return nil
	case h.State != budget.StateOpen:
		return fmt.Errorf("%w: hold %s is %s", budget.ErrClosed, id, h.State)
	}
	l.unhold(h, budget.StateReleased)
	return nil
}

// Reap implements budget.Store.
func (l *Ledger) Reap(ctx context.Context, now time.Time) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, h := range l.holds {
		if h.State == budget.StateOpen && !now.Before(h.ExpiresAt) {
			l.unhold(h, budget.StateExpired)
			n++
		}
	}
	return n, nil
}

func (l *Ledger) unhold(h *hold, s budget.State) {
	for _, leg := range h.Legs {
		l.acct(leg.Account).held -= leg.Held
	}
	h.State = s
}

// open returns an open, unexpired hold. An expired hold is reaped here, so
// expiry does not depend on Reap being called.
func (l *Ledger) open(id budget.HoldID) (*hold, error) {
	h, ok := l.holds[id]
	if !ok {
		return nil, fmt.Errorf("%w: hold %s", budget.ErrNotFound, id)
	}
	if h.State == budget.StateOpen && !l.now().Before(h.ExpiresAt) {
		l.unhold(h, budget.StateExpired)
	}
	if h.State != budget.StateOpen {
		return nil, fmt.Errorf("%w: hold %s is %s", budget.ErrClosed, id, h.State)
	}
	return h, nil
}

func (h *hold) copy() budget.Hold {
	c := h.Hold
	c.Legs = append([]budget.Leg(nil), h.Legs...)
	return c
}
