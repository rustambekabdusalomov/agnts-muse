package muse

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/rustambekabdusalomov/agnts-muse/budget"
	"github.com/rustambekabdusalomov/agnts-muse/credential"
	"github.com/rustambekabdusalomov/agnts-muse/model"
	"github.com/rustambekabdusalomov/agnts-muse/route"
)

// Provider is another place a run's model calls can go: a host's
// credential and the one model the run uses there.
type Provider struct {
	// Name is for people, events and budget keys ("baseten").
	Name       string
	Credential credential.Credential
	// Model is what runs use on the provider. They never change model
	// there (route.Fixed): a host serves its own models only.
	Model model.ID
}

// Mode is which provider a Failover's runs use.
type Mode string

// Modes.
const (
	// ModeAuto runs on the request's own credential, and on the fallback
	// while that is out (ProviderOut).
	ModeAuto Mode = "auto"
	// ModePrimary runs on the request's own credential only.
	ModePrimary Mode = "primary"
	// ModeFallback runs on the fallback only.
	ModeFallback Mode = "fallback"
)

// ParseMode reads a Mode; "" is ModeAuto.
func ParseMode(s string) (Mode, error) {
	switch m := Mode(strings.ToLower(strings.TrimSpace(s))); m {
	case "":
		return ModeAuto, nil
	case ModeAuto, ModePrimary, ModeFallback:
		return m, nil
	}
	return "", fmt.Errorf("%w: mode %q (want auto, primary or fallback)", ErrInvalid, s)
}

// Failover runs requests on their own credential (the primary) and moves
// them to a fallback provider when the primary's account is out: its
// allowance or credit is used up, or its key is refused.
//
// A run that fails over mid-task continues where it stopped: the fallback
// resumes the same session, with everything Muse learned so far, on its
// own model (MSP keeps sessions on disk, so this works across providers).
// It spends what is left of the run's limit, under its own budget key,
// and within the budget's FallbackLimit and FallbackCaps: what a person
// may spend on the fallback can be limited apart from the primary, and
// FallbackPayers can pay for it instead of the run's own payers.
//
// A request with PreferFallback goes the other way: it starts on the
// fallback, and moves to its own credential, in the same session, when the
// fallback is out or its budget there is spent.
//
// After the primary runs out, a Failover sends runs straight to the
// fallback until the primary's allowance resets (or for Cooldown when
// Muse does not say when), so they do not each fail first. Use one
// Failover per primary account: that state is the account's.
//
// The zero value is not usable; set Engine and Fallback.
type Failover struct {
	Engine   *Engine
	Fallback Provider
	// Mode is read at the start of every run, so an operator can change
	// it while the server runs (nil: ModeAuto).
	Mode func() Mode
	// Cooldown is how long the primary is skipped after it ran out
	// without a reset time (15m).
	Cooldown time.Duration
	// Out says whether a run's error means its account is out (ProviderOut).
	Out func(error) bool
	// Now is the clock (time.Now).
	Now func() time.Time

	mu        sync.Mutex
	downUntil time.Time
	downWhy   string
}

// ProviderOut reports whether err means the account a run used can make
// no more model calls for now: its allowance is used up (RateLimitError),
// it was refused (ErrAuth), or the provider would not bill it. Overloads
// and server errors are not: Muse retries those itself, and moving would
// spend the fallback's money on a passing problem. The run's own budget
// (ErrBudget, ErrRunLimit) is not either: the payer is out, not the account.
func ProviderOut(err error) bool {
	var rl *RateLimitError
	if errors.As(err, &rl) || errors.Is(err, ErrAuth) {
		return true
	}
	var te *TurnError
	if !errors.As(err, &te) {
		return false
	}
	m := strings.ToLower(te.Kind + " " + te.Message)
	for _, w := range []string{"billing", "credit", "quota", "payment", "insufficient_funds", "insufficient funds"} {
		if strings.Contains(m, w) {
			return true
		}
	}
	return false
}

// OutUntil reports until when the primary is skipped, and why (zero when
// it is not).
func (f *Failover) OutUntil() (time.Time, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.now().Before(f.downUntil) {
		return time.Time{}, ""
	}
	return f.downUntil, f.downWhy
}

// Reset tries the primary again from the next run, such as after its
// account was topped up.
func (f *Failover) Reset() {
	f.mu.Lock()
	f.downUntil, f.downWhy = time.Time{}, ""
	f.mu.Unlock()
}

// Run is Engine.Run with failover. The Result's Fallback and Earlier say
// where the run went.
func (f *Failover) Run(ctx context.Context, req Request, ex Executor, onEvent func(Event)) (Result, error) {
	if f.Engine == nil || f.Fallback.Name == "" || f.Fallback.Model == "" {
		return Result{}, fmt.Errorf("%w: a failover needs an engine and a fallback with a name and a model", ErrInvalid)
	}
	if onEvent == nil {
		onEvent = func(Event) {}
	}
	mode := ModeAuto
	if f.Mode != nil {
		mode = f.Mode()
	}
	switch mode {
	case ModePrimary:
		return f.Engine.Run(ctx, req, ex, onEvent)
	case ModeFallback:
		return f.onFallback(ctx, req, ex, onEvent, "runs use "+f.Fallback.Name)
	}
	if req.PreferFallback && !fallbackClosed(req.Budget) {
		res, err := f.onFallback(ctx, req, ex, onEvent, "the run starts on "+f.Fallback.Name)
		if err == nil || ctx.Err() != nil || !f.out(err) && !errors.Is(err, ErrBudget) && !errors.Is(err, ErrRunLimit) {
			return res, err
		}
		return f.carryOn(ctx, req, res, err, ex, onEvent, false)
	}
	if until, why := f.OutUntil(); !until.IsZero() {
		return f.onFallback(ctx, req, ex, onEvent, fmt.Sprintf("the primary is out until %s (%s)", until.Format(time.RFC3339), why))
	}
	res, err := f.Engine.Run(ctx, req, ex, onEvent)
	if err == nil || ctx.Err() != nil || !f.out(err) {
		return res, err
	}
	f.markOut(err)
	if fallbackClosed(req.Budget) {
		return res, errors.Join(err, ErrFallbackLimit)
	}
	return f.carryOn(ctx, req, res, err, ex, onEvent, true)
}

func (f *Failover) out(err error) bool {
	if f.Out != nil {
		return f.Out(err)
	}
	return ProviderOut(err)
}

// fallbackClosed reports a budget that allows nothing on the fallback.
func fallbackClosed(b *Budget) bool { return b != nil && b.FallbackLimit < 0 }

// carryOn continues a run that stopped on one provider (res, err) on the
// other, in the same session. When both bill the same payers, the run's
// limit covers both attempts; when the fallback has payers of its own,
// neither attempt counts against the other's.
func (f *Failover) carryOn(ctx context.Context, req Request, res Result, err error, ex Executor, onEvent func(Event), toFallback bool) (Result, error) {
	next := req
	if b := req.Budget; b != nil && b.Limit > 0 && len(b.FallbackPayers) == 0 {
		nb := *b
		if nb.Limit = b.Limit - res.Cost(); nb.Limit <= 0 {
			return res, err // the run spent its limit
		}
		next.Budget = &nb
	}
	if res.SessionID != "" && !req.Ephemeral {
		next.SessionID, next.Prompt = res.SessionID, resumePrompt(req.Prompt)
	}
	var nres Result
	var nerr error
	if toFallback {
		nres, nerr = f.onFallback(ctx, next, ex, onEvent, "the primary ran out: "+err.Error())
	} else {
		nres, nerr = f.onPrimary(ctx, next, ex, onEvent, f.Fallback.Name+" stopped: "+err.Error())
	}
	earlier := res
	nres.Earlier = &earlier
	if nres.Fallback == "" {
		nres.Fallback = res.Fallback
	}
	nres.Usage = res.Usage.Add(nres.Usage)
	nres.Duration += res.Duration
	nres.Denied += res.Denied
	nres.Steered = nres.Steered || res.Steered
	return nres, nerr
}

// onFallback runs req on the fallback provider. Its budget gets its own
// key (ending ":" + the provider's name, so the fallback's spend can be
// told apart) and the fallback's caps and limit. With FallbackPayers the
// fallback bills them instead, under FallbackCaps alone.
func (f *Failover) onFallback(ctx context.Context, req Request, ex Executor, onEvent func(Event), why string) (Result, error) {
	p := f.Fallback
	if b := req.Budget; b != nil {
		if b.FallbackLimit < 0 {
			return Result{Fallback: p.Name}, fmt.Errorf("%w (%s)", ErrFallbackLimit, p.Name)
		}
		nb := *b
		nb.Key = b.Key + ":" + p.Name
		if len(b.FallbackPayers) > 0 {
			nb.Payers, nb.Caps, nb.Limit = append([]budget.AccountID(nil), b.FallbackPayers...), append([]budget.AccountID(nil), b.FallbackCaps...), b.FallbackLimit
		} else {
			nb.Caps = append(append([]budget.AccountID(nil), b.Caps...), b.FallbackCaps...)
			if b.FallbackLimit > 0 && (nb.Limit <= 0 || b.FallbackLimit < nb.Limit) {
				nb.Limit = b.FallbackLimit
			}
		}
		nb.FallbackPayers, nb.FallbackCaps, nb.FallbackLimit = nil, nil, 0
		req.Budget = &nb
	}
	req.Credential, req.Model, req.Router = &p.Credential, p.Model, route.Fixed(p.Model)
	f.emit(ctx, onEvent, Event{Kind: EventFailover, SessionID: req.SessionID, Model: p.Model,
		Reason: credential.NewRedactor(p.Credential.Secrets()...).Redact(why)})
	res, err := f.Engine.Run(ctx, req, ex, onEvent)
	res.Fallback = p.Name
	return res, err
}

// onPrimary runs req on its own credential after the fallback stopped.
func (f *Failover) onPrimary(ctx context.Context, req Request, ex Executor, onEvent func(Event), why string) (Result, error) {
	id := req.Model
	if id == "" {
		router := req.Router
		if router == nil {
			router = f.Engine.cfg.Router
		}
		id = router.Initial(req.Complexity)
		req.Model = id
	}
	f.emit(ctx, onEvent, Event{Kind: EventFailover, SessionID: req.SessionID, Model: id,
		Reason: credential.NewRedactor(f.Fallback.Credential.Secrets()...).Redact(why)})
	return f.Engine.Run(ctx, req, ex, onEvent)
}

func (f *Failover) emit(ctx context.Context, onEvent func(Event), e Event) {
	e.Time = f.now()
	onEvent(e)
	if o := f.Engine.cfg.Observer; o != nil {
		o.Observe(ctx, e)
	}
}

// markOut skips the primary until its allowance resets, or for Cooldown.
func (f *Failover) markOut(err error) {
	now := f.now()
	cool := f.Cooldown
	if cool <= 0 {
		cool = 15 * time.Minute
	}
	until := now.Add(cool)
	var rl *RateLimitError
	if errors.As(err, &rl) && rl.Until.After(now) {
		until = rl.Until
	}
	f.mu.Lock()
	f.downUntil, f.downWhy = until, err.Error()
	f.mu.Unlock()
}

func (f *Failover) now() time.Time {
	if f.Now != nil {
		return f.Now()
	}
	return time.Now()
}

// resumePrompt continues a task in its own session. The task is repeated:
// the primary may have failed before the session recorded it.
func resumePrompt(task string) string {
	return "[Continue] The previous attempt at this task was cut off by a provider limit and this session now continues on another provider. " +
		"Pick up where it stopped: check what is already done, do not redo it, and finish the task.\n\nThe task:\n" + task
}
