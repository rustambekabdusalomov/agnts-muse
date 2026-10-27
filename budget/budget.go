// Package budget reserves and charges money for model runs so that
// concurrent runs can never spend more than their payers have.
//
// It works like a card payment. Before a run, [Ledger.Hold] reserves an
// amount across an ordered list of payers (say, a user's free credits and
// then a workspace's bought credits) and against any number of caps (a
// per-month platform limit). While the run streams usage, [Ledger.Extend]
// grows the reservation; when it cannot, the caller stops the run. At the
// end, [Ledger.Commit] charges the exact cost and releases the rest. A run
// can never be charged more than it held, so a payer's balance can never
// go below zero, however many runs share it.
//
// Every operation is idempotent where it matters: a Hold is keyed by the
// caller's key, a Commit of a committed hold returns the same receipt, and
// a Deposit is keyed too. Holds expire, so a crashed process cannot keep
// money reserved forever: [Store.Reap] releases them.
//
// [memledger] is an in-memory implementation and [ledgertest] the
// conformance suite every implementation must pass; the PostgreSQL one is
// the nested module ledger/pgledger.
package budget

import (
	"context"
	"errors"
	"time"

	"github.com/rustambekabdusalomov/agnts-muse/money"
)

// AccountID names an account: "user:42:free", "ws:7:paid", "cap:2026-10".
type AccountID string

// HoldID identifies a hold.
type HoldID string

// State is where a hold is in its life.
type State string

// Hold states. Only an open hold can be extended, committed or released.
const (
	StateOpen      State = "open"
	StateCommitted State = "committed"
	StateReleased  State = "released"
	StateExpired   State = "expired"
)

// DefaultTTL is how long a hold lives without an Extend.
const DefaultTTL = 30 * time.Minute

// Errors. Implementations wrap them; test with errors.Is.
var (
	// ErrInsufficient: the payers together cannot cover the amount.
	ErrInsufficient = errors.New("budget: insufficient funds")
	// ErrCapReached: a cap the hold counts against has no room left.
	ErrCapReached = errors.New("budget: cap reached")
	// ErrNotFound: no such hold or account.
	ErrNotFound = errors.New("budget: not found")
	// ErrClosed: the hold is no longer open (committed, released, expired).
	ErrClosed = errors.New("budget: hold is closed")
	// ErrInvalid: the request itself is wrong (no payers, amount ≤ 0, …).
	ErrInvalid = errors.New("budget: invalid request")
)

// HoldRequest asks for a reservation.
type HoldRequest struct {
	// Key makes Hold idempotent: a second Hold with the same key returns
	// the first hold instead of reserving again. Required.
	Key string
	// Payers are charged in order: the first pays as much as it has, the
	// next the rest, and so on.
	Payers []AccountID
	// Caps are accounts every hold counts against in full without paying
	// from them, such as a platform's monthly limit. Each must have room
	// for the whole amount.
	Caps []AccountID
	// Amount to reserve; must be positive.
	Amount money.USD
	// TTL is how long the hold lives without an Extend (DefaultTTL if 0).
	TTL time.Duration
}

// Validate reports whether r is well formed.
func (r HoldRequest) Validate() error {
	switch {
	case r.Key == "":
		return errors.Join(ErrInvalid, errors.New("budget: hold needs a key"))
	case len(r.Payers) == 0:
		return errors.Join(ErrInvalid, errors.New("budget: hold needs a payer"))
	case r.Amount <= 0:
		return errors.Join(ErrInvalid, errors.New("budget: hold amount must be positive"))
	case r.TTL < 0:
		return errors.Join(ErrInvalid, errors.New("budget: negative TTL"))
	}
	seen := map[AccountID]bool{}
	for _, a := range append(append([]AccountID{}, r.Payers...), r.Caps...) {
		if a == "" || seen[a] {
			return errors.Join(ErrInvalid, errors.New("budget: accounts must be distinct and named"))
		}
		seen[a] = true
	}
	return nil
}

// Leg is one account's part in a hold.
type Leg struct {
	Account AccountID
	// Cap is true for a cap leg: it counts the hold, it does not pay.
	Cap     bool
	Held    money.USD
	Charged money.USD
}

// Hold is a reservation.
type Hold struct {
	ID        HoldID
	Key       string
	State     State
	Amount    money.USD // total reserved from the payers
	Legs      []Leg     // payers first, in order, then caps
	ExpiresAt time.Time
}

// Receipt is what a Commit charged.
type Receipt struct {
	HoldID  HoldID
	Key     string
	Charged money.USD // taken from the payers
	// Released is what the hold had reserved but the run did not use.
	Released money.USD
	// Unbilled is cost above the hold that nobody was charged for: the
	// last model call of a run that ran over. The ledger never takes it
	// from a payer; the caller decides whether to absorb or report it.
	Unbilled money.USD
	Legs     []Leg
}

// Balance is an account's money.
type Balance struct {
	Total     money.USD // deposited minus charged
	Held      money.USD // reserved by open holds
	Available money.USD // Total - Held
}

// Ledger reserves and charges.
type Ledger interface {
	// Hold reserves req.Amount, or fails with ErrInsufficient or
	// ErrCapReached without reserving anything.
	Hold(ctx context.Context, req HoldRequest) (Hold, error)
	// Extend adds by to an open hold, all or nothing, and renews its TTL.
	Extend(ctx context.Context, id HoldID, by money.USD) (Hold, error)
	// Commit charges actual (at most the held amount, see
	// Receipt.Unbilled) and releases the rest. Committing a committed hold
	// returns the original receipt.
	Commit(ctx context.Context, id HoldID, actual money.USD) (Receipt, error)
	// Release cancels an open hold. Releasing a released hold is a no-op.
	Release(ctx context.Context, id HoldID) error
}

// Funds moves money into and out of accounts.
type Funds interface {
	// Deposit adds amount (negative to withdraw) and returns the new
	// balance. It is idempotent by key. A withdrawal that would take the
	// account below what its open holds reserve fails with ErrInsufficient.
	Deposit(ctx context.Context, account AccountID, amount money.USD, key string) (Balance, error)
	// Balance returns an account's balance; an unknown account is empty.
	Balance(ctx context.Context, account AccountID) (Balance, error)
}

// Store is a complete ledger implementation.
type Store interface {
	Ledger
	Funds
	// Reap expires open holds whose TTL passed before now and returns how
	// many it released.
	Reap(ctx context.Context, now time.Time) (int, error)
}

// Split divides amount across available balances in order, each giving as
// much as it has. ok is false when they cannot cover it together.
// Implementations share it so every ledger allocates the same way.
func Split(available []money.USD, amount money.USD) (parts []money.USD, ok bool) {
	parts = make([]money.USD, len(available))
	left := amount
	for i, a := range available {
		take := money.Min(money.Max(a, 0), left)
		parts[i] = take
		left -= take
	}
	return parts, left == 0
}

// Settle divides a commit across a hold's legs: payer legs are charged in
// order up to what each held, caps are charged the same total. It returns
// the legs with Charged set and the amount above the hold (unbilled).
func Settle(legs []Leg, actual money.USD) (out []Leg, charged, unbilled money.USD) {
	out = append([]Leg(nil), legs...)
	var held money.USD
	for _, l := range out {
		if !l.Cap {
			held += l.Held
		}
	}
	charged = money.Min(money.Max(actual, 0), held)
	unbilled = money.Max(actual-charged, 0)
	left := charged
	for i := range out {
		if out[i].Cap {
			out[i].Charged = charged
			continue
		}
		out[i].Charged = money.Min(out[i].Held, left)
		left -= out[i].Charged
	}
	return out, charged, unbilled
}
