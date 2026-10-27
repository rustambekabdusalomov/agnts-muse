// Package route decides which Muse model a run uses, and when to move a
// running session to a stronger one.
//
// Muse lets a client change a session's model between model calls
// (MSP session/setModel) without losing its context, so escalation is not
// a retry: the session that got stuck on Spark continues on Wonder with
// everything it learned. A Policy sees the run's signals after every model
// call and tool result and returns at most one decision per change.
package route

import (
	"fmt"

	"github.com/rustambekabdusalomov/agnts-muse/model"
	"github.com/rustambekabdusalomov/agnts-muse/money"
)

// Signal is what a policy knows about a running session.
type Signal struct {
	Model model.ID
	// Calls is the number of model calls so far.
	Calls int
	// FailedTools is the number of tool calls in a row that failed (a
	// non-zero exit, an edit that did not apply); a success resets it.
	FailedTools int
	// Spent and Remaining are the run's spend and what its budget still
	// allows (Remaining < 0 when the run has no budget).
	Spent, Remaining money.USD
	// Escalations is how many times this run already changed model.
	Escalations int
}

// Decision is a model change.
type Decision struct {
	To     model.ID
	Reason string
}

// Policy decides model changes.
type Policy interface {
	// Initial picks the model for a run of the given complexity.
	Initial(c Complexity) model.ID
	// Next returns a change, or ok false to keep the model.
	Next(s Signal) (d Decision, ok bool)
}

// Complexity is how hard a task looks before it runs.
type Complexity int

// Complexities.
const (
	Simple Complexity = iota
	Complex
)

// Escalate is the default policy: start on Spark, or on Wonder for complex
// work, and move a stuck Spark session to Wonder once, if the budget can
// afford Wonder's prices for a while.
type Escalate struct {
	Catalog *model.Catalog
	// Cheap and Strong are the two models (Spark and Wonder by default).
	Cheap, Strong model.ID
	// AfterFailedTools escalates after this many tool failures in a row.
	AfterFailedTools int
	// AfterCalls escalates when a session has made this many model calls
	// and is still failing tools (0 turns it off).
	AfterCalls int
	// MinRemaining is the budget a run must have left to escalate; below
	// it, finishing on the cheap model beats being interrupted on the
	// strong one.
	MinRemaining money.USD
}

// Default returns the default escalation policy.
func Default() *Escalate {
	return &Escalate{Catalog: model.Default, Cheap: model.Spark, Strong: model.Wonder,
		AfterFailedTools: 3, AfterCalls: 25, MinRemaining: 25 * money.Cent}
}

// Initial implements Policy.
func (e *Escalate) Initial(c Complexity) model.ID {
	if c == Complex {
		return e.Strong
	}
	return e.Cheap
}

// Next implements Policy.
func (e *Escalate) Next(s Signal) (Decision, bool) {
	if s.Escalations > 0 || e.tier(s.Model) >= e.tier(e.Strong) {
		return Decision{}, false
	}
	if s.Remaining >= 0 && s.Remaining < e.MinRemaining {
		return Decision{}, false
	}
	switch {
	case e.AfterFailedTools > 0 && s.FailedTools >= e.AfterFailedTools:
		return Decision{To: e.Strong, Reason: fmt.Sprintf("%d tool calls failed in a row on %s", s.FailedTools, s.Model)}, true
	case e.AfterCalls > 0 && s.Calls >= e.AfterCalls && s.FailedTools > 0:
		return Decision{To: e.Strong, Reason: fmt.Sprintf("%d model calls on %s and still failing", s.Calls, s.Model)}, true
	}
	return Decision{}, false
}

func (e *Escalate) tier(id model.ID) model.Tier {
	cat := e.Catalog
	if cat == nil {
		cat = model.Default
	}
	if m, ok := cat.Lookup(id); ok {
		return m.Tier
	}
	return model.TierWonder // unknown: treat as strong, never escalate from it
}

// Fixed never changes model.
type Fixed model.ID

// Initial implements Policy.
func (f Fixed) Initial(Complexity) model.ID { return model.ID(f) }

// Next implements Policy.
func (Fixed) Next(Signal) (Decision, bool) { return Decision{}, false }
