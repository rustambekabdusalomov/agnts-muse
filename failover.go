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
// may spend on the fallback can be limited apart from the primary.
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

// Run is Engine.Run with failover. The Result's Fallback and Primary say
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
	if until, why := f.OutUntil(); !until.IsZero() {
		return f.onFallback(ctx, req, ex, onEvent, fmt.Sprintf("the primary is out until %s (%s)", until.Format(time.RFC3339), why))
	}
	res, err := f.Engine.Run(ctx, req, ex, onEvent)
	out := f.Out
	if out == nil {
		out = ProviderOut
	}
	if err == nil || ctx.Err() != nil || !out(err) {
		return res, err
	}
	f.markOut(err)
	next := req
	if b := req.Budget; b != nil {
		if b.FallbackLimit < 0 {
			return res, errors.Join(err, ErrFallbackLimit)
		}
		nb := *b
		if b.Limit > 0 {
			if nb.Limit = b.Limit - res.Cost(); nb.Limit <= 0 {
				return res, err // the run spent its limit on the primary
			}
		}
		next.Budget = &nb
	}
	if res.SessionID != "" && !req.Ephemeral {
		next.SessionID, next.Prompt = res.SessionID, resumePrompt(req.Prompt)
	}
	fres, ferr := f.onFallback(ctx, next, ex, onEvent, "the primary ran out: "+err.Error())
	primary := res
	fres.Primary = &primary
	fres.Usage = res.Usage.Add(fres.Usage)
	fres.Duration += res.Duration
	fres.Denied += res.Denied
	fres.Steered = fres.Steered || res.Steered
	return fres, ferr
}

// onFallback runs req on the fallback provider. Its budget gets its own
// key (ending ":" + the provider's name, so the fallback's spend can be
// told apart), the fallback's caps, and at most the fallback's limit.
func (f *Failover) onFallback(ctx context.Context, req Request, ex Executor, onEvent func(Event), why string) (Result, error) {
	p := f.Fallback
	if b := req.Budget; b != nil {
		if b.FallbackLimit < 0 {
			return Result{Fallback: p.Name}, fmt.Errorf("%w (%s)", ErrFallbackLimit, p.Name)
		}
		nb := *b
		nb.Key = b.Key + ":" + p.Name
		nb.Caps = append(append([]budget.AccountID(nil), b.Caps...), b.FallbackCaps...)
		if b.FallbackLimit > 0 && (nb.Limit <= 0 || b.FallbackLimit < nb.Limit) {
			nb.Limit = b.FallbackLimit
		}
		nb.FallbackCaps, nb.FallbackLimit = nil, 0
		req.Budget = &nb
	}
	req.Credential, req.Model, req.Router = &p.Credential, p.Model, route.Fixed(p.Model)
	e := Event{Kind: EventFailover, Time: f.now(), SessionID: req.SessionID, Model: p.Model,
		Reason: credential.NewRedactor(p.Credential.Secrets()...).Redact(why)}
	onEvent(e)
	if o := f.Engine.cfg.Observer; o != nil {
		o.Observe(ctx, e)
	}
	res, err := f.Engine.Run(ctx, req, ex, onEvent)
	res.Fallback = p.Name
	return res, err
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
