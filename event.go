package muse

import (
	"context"
	"log/slog"
	"time"

	"github.com/rustambekabdusalomov/agnts-muse/budget"
	"github.com/rustambekabdusalomov/agnts-muse/meter"
	"github.com/rustambekabdusalomov/agnts-muse/model"
	"github.com/rustambekabdusalomov/agnts-muse/money"
	"github.com/rustambekabdusalomov/agnts-muse/msp"
)

// EventKind classifies events.
type EventKind string

// Event kinds.
const (
	// EventStarted: the session and turn are running (SessionID, Model).
	EventStarted EventKind = "started"
	// EventDelta: streamed text of the agent's reply (Text).
	EventDelta EventKind = "delta"
	// EventMessage: a finished agent message (Text; Final for the answer).
	EventMessage EventKind = "message"
	// EventToolStart: a tool call began (Tool, Args).
	EventToolStart EventKind = "tool_start"
	// EventToolEnd: a tool call ended (Tool, Output, Failed).
	EventToolEnd EventKind = "tool_end"
	// EventUsage: a model call finished and was charged (Charge, Held).
	EventUsage EventKind = "usage"
	// EventSteer: Muse was told something mid-turn (Text): its budget or
	// calls are running low (Reason budget or steps), a person's message
	// (person), or a person's answer about a tool call (approval).
	EventSteer EventKind = "steer"
	// EventEscalate: the session moved to a stronger model (Model, Reason).
	EventEscalate EventKind = "escalate"
	// EventApproval: the guard decided a tool call, or a person was asked
	// about one (Pending) and answered (Approval).
	EventApproval EventKind = "approval"
	// EventRetry: Muse is retrying a failed model call (Reason).
	EventRetry EventKind = "retry"
	// EventAllowance: the account's subscription usage changed (Allowance).
	EventAllowance EventKind = "allowance"
	// EventInterrupt: the run is being stopped (Reason).
	EventInterrupt EventKind = "interrupt"
	// EventSettled: the run's cost was charged (Receipt).
	EventSettled EventKind = "settled"
	// EventFailover: a Failover put the run on another provider: the
	// fallback, or back on its own credential (Model, Reason).
	EventFailover EventKind = "failover"
	// EventContinue: a turn ended without any answer and the run goes on
	// in a new turn of the same session (Reason).
	EventContinue EventKind = "continue"
	// EventWarning: something worth logging that did not stop the run.
	EventWarning EventKind = "warning"
)

// Approval is a guard decision.
type Approval struct {
	// ID is Muse's id of the approval.
	ID      string
	Tool    string
	Command string
	Path    string
	Allow   bool
	// Ask says the guard left the call to a person. While Pending they
	// have not answered and the call is held back; after, Allow is their
	// answer (false when nobody answered in time).
	Ask     bool
	Pending bool
	Rule    string
	Reason  string
	// Applied is false when Muse had already resolved the call.
	Applied bool
}

// Event is something that happened in a run. Secrets of the run's
// credential are removed from every text field before it is emitted.
type Event struct {
	Kind      EventKind
	Time      time.Time
	SessionID string
	TurnID    string
	Model     model.ID
	Text      string
	Final     bool
	Tool      string
	Args      string
	Output    string
	Failed    bool
	Reason    string
	Charge    *meter.Charge
	Held      money.USD
	Approval  *Approval
	Allowance *msp.SubscriptionUsage
	Receipt   *budget.Receipt
}

// Observer receives every event of every run, for logs and metrics.
type Observer interface {
	Observe(ctx context.Context, e Event)
}

// SlogObserver logs events. Deltas are skipped: they are many and the
// finished message carries the same text.
type SlogObserver struct{ Logger *slog.Logger }

// Observe implements Observer.
func (o SlogObserver) Observe(ctx context.Context, e Event) {
	l := o.Logger
	if l == nil {
		l = slog.Default()
	}
	attrs := []slog.Attr{slog.String("session", e.SessionID), slog.String("model", string(e.Model))}
	level := slog.LevelDebug
	switch e.Kind {
	case EventDelta:
		return
	case EventUsage:
		if e.Charge != nil {
			attrs = append(attrs, slog.String("cost", e.Charge.Cost.String()), slog.String("total", e.Charge.Total.String()),
				slog.String("held", e.Held.String()), slog.Int64("input", e.Charge.Usage.Input), slog.Int64("cached", e.Charge.Usage.Cached),
				slog.Int64("output", e.Charge.Usage.Output))
		}
		level = slog.LevelInfo
	case EventSteer, EventEscalate, EventInterrupt:
		attrs = append(attrs, slog.String("reason", e.Reason))
		level = slog.LevelInfo
	case EventApproval:
		if a := e.Approval; a != nil {
			attrs = append(attrs, slog.String("tool", a.Tool), slog.Bool("allow", a.Allow), slog.String("rule", a.Rule))
			if !a.Allow {
				level = slog.LevelWarn
			}
		}
	case EventSettled:
		if r := e.Receipt; r != nil {
			attrs = append(attrs, slog.String("charged", r.Charged.String()), slog.String("released", r.Released.String()),
				slog.String("unbilled", r.Unbilled.String()))
		}
		level = slog.LevelInfo
	case EventWarning:
		attrs = append(attrs, slog.String("reason", e.Reason))
		level = slog.LevelWarn
	case EventToolEnd:
		attrs = append(attrs, slog.String("tool", e.Tool), slog.Bool("failed", e.Failed))
	}
	l.LogAttrs(ctx, level, "muse."+string(e.Kind), attrs...)
}
