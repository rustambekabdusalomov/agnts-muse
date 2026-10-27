package muse

import (
	"errors"
	"fmt"
	"time"

	"github.com/rustambekabdusalomov/agnts-muse/money"
)

// Run errors. A Run that ends early still returns its Result (what was
// done, spent and charged) alongside the error.
var (
	// ErrBudget: the payers ran out of money, or a cap was reached, and
	// the run was stopped. It wraps budget.ErrInsufficient or
	// budget.ErrCapReached.
	ErrBudget = errors.New("muse: budget exhausted")
	// ErrRunLimit: the run reached its own spending limit.
	ErrRunLimit = errors.New("muse: run limit reached")
	// ErrAuth: Muse rejected the credential.
	ErrAuth = errors.New("muse: authentication required")
	// ErrStepLimit: Muse stopped at its step limit.
	ErrStepLimit = errors.New("muse: step limit reached")
	// ErrExited: the Muse process ended before the turn did.
	ErrExited = errors.New("muse: process exited")
	// ErrInvalid: the request itself is wrong.
	ErrInvalid = errors.New("muse: invalid request")
)

// RateLimitError: the account's Muse allowance is used up until Until.
type RateLimitError struct {
	Kind  string // the provider's limit kind
	Until time.Time
}

func (e *RateLimitError) Error() string {
	if e.Until.IsZero() {
		return "muse: rate limited"
	}
	return fmt.Sprintf("muse: rate limited until %s", e.Until.Format(time.RFC3339))
}

// TurnError is any other failed turn.
type TurnError struct {
	Kind      string
	Message   string
	Retryable bool
}

func (e *TurnError) Error() string {
	return fmt.Sprintf("muse: turn failed (%s): %s", e.Kind, e.Message)
}

// BudgetError explains a budget stop: what was spent and held, and the
// ledger's reason.
type BudgetError struct {
	Spent, Held money.USD
	Err         error // ErrBudget or ErrRunLimit, joined with the ledger's error
}

func (e *BudgetError) Error() string {
	return fmt.Sprintf("%v (spent %v, held %v)", e.Err, e.Spent, e.Held)
}

func (e *BudgetError) Unwrap() error { return e.Err }
