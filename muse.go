// Package muse runs Meta's Muse Code under a hard, live-enforced budget.
//
// An Engine drives `muse serve` through MSP, Muse Code's session protocol,
// rather than scraping a one-shot CLI: it sees the cost of every model
// call as it happens (session/tokenUsage), so it can reserve money ahead
// of the spend, tell Muse to wrap up when the budget runs low
// (turn/steer), stop it at the limit (turn/interrupt), move a stuck
// session to a stronger model without losing context (session/setModel),
// and decide tool approvals with the caller's own rules (approval/decide).
//
//	eng := muse.New(muse.Config{Ledger: ledger})
//	res, err := eng.Run(ctx, muse.Request{
//		Prompt:  "Fix the failing test in ./billing",
//		Workdir: "/workspace",
//		Budget: &muse.Budget{Key: "run-42", Payers: []budget.AccountID{"user:7:free", "ws:3:paid"},
//			Limit: 50 * money.Cent},
//	}, muse.LocalExecutor{}, func(e muse.Event) { log.Println(e.Kind, e.Text) })
//
// The pieces are usable alone: money (integer amounts), model (catalog and
// prices), meter (usage to charges), budget (holds and the ledger), msp
// (the protocol client), route (model escalation), guard (approvals) and
// credential (API keys and Meta's device sign-in).
package muse

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/rustambekabdusalomov/agnts-muse/budget"
	"github.com/rustambekabdusalomov/agnts-muse/credential"
	"github.com/rustambekabdusalomov/agnts-muse/guard"
	"github.com/rustambekabdusalomov/agnts-muse/meter"
	"github.com/rustambekabdusalomov/agnts-muse/model"
	"github.com/rustambekabdusalomov/agnts-muse/money"
	"github.com/rustambekabdusalomov/agnts-muse/msp"
	"github.com/rustambekabdusalomov/agnts-muse/route"
)

// Version is this module's version, sent to Muse as the client version.
const Version = "0.1.0"

// Config is an Engine's fixed setup. The zero value works: it runs `muse`
// from PATH, prices with model.Default, routes with route.Default, guards
// with guard.Defaults and enforces no budget.
type Config struct {
	// Binary is the Muse Code executable ("muse").
	Binary string
	// ServeArgs are extra arguments for `muse serve`.
	ServeArgs []string
	// Catalog prices model calls (model.Default).
	Catalog *model.Catalog
	// Ledger reserves and charges budgets; nil runs without enforcement
	// (usage is still metered and reported).
	Ledger budget.Ledger
	// Router picks and escalates models (route.Default()).
	Router route.Policy
	// Guard decides tool approvals (guard.Defaults()).
	Guard guard.Policy
	// Observer sees every event of every run.
	Observer Observer
	// ClientName identifies this client to Muse; ^[a-z0-9_]+$.
	ClientName string
	// Step is how far a hold stays ahead of the spend (10¢): each model
	// call that brings the spend within Step of the hold extends it.
	Step money.USD
	// SteerAt is the share of a run's limit at which Muse is told to wrap
	// up (0.8). Set it ≥ 1 to never steer.
	SteerAt float64
	// ApprovalMode is the session's MSP approval mode (promptUnmatched).
	ApprovalMode string
	// InterruptGrace is how long an interrupted turn may take to stop
	// before the process is killed (15s).
	InterruptGrace time.Duration
	// AskTimeout is how long a person has to answer about a tool call the
	// guard left to them (5m); no answer denies it.
	AskTimeout time.Duration
	// Trace receives every protocol line, for recording sessions.
	Trace func(dir byte, line []byte)
}

// Engine runs Muse sessions. It is safe for concurrent use; each Run
// starts its own `muse serve` process.
type Engine struct{ cfg Config }

// New returns an Engine.
func New(cfg Config) *Engine {
	if cfg.Binary == "" {
		cfg.Binary = "muse"
	}
	if cfg.Catalog == nil {
		cfg.Catalog = model.Default
	}
	if cfg.Router == nil {
		cfg.Router = route.Default()
	}
	if cfg.Guard == nil {
		cfg.Guard = guard.Defaults()
	}
	if cfg.ClientName == "" {
		cfg.ClientName = "agnts_muse"
	}
	if cfg.Step <= 0 {
		cfg.Step = 10 * money.Cent
	}
	if cfg.SteerAt == 0 {
		cfg.SteerAt = 0.8
	}
	if cfg.ApprovalMode == "" {
		cfg.ApprovalMode = msp.ApprovalPromptUnmatched
	}
	if cfg.InterruptGrace <= 0 {
		cfg.InterruptGrace = 15 * time.Second
	}
	if cfg.AskTimeout <= 0 {
		cfg.AskTimeout = 5 * time.Minute
	}
	return &Engine{cfg: cfg}
}

// Budget is what a run may spend and who pays.
type Budget struct {
	// Key identifies the run in the ledger; reusing it after a crash
	// finds the same hold instead of reserving twice.
	Key string
	// Payers pay in order (free credits, then bought credits).
	Payers []budget.AccountID
	// Caps are limits the spend counts against (a platform's month).
	Caps []budget.AccountID
	// Limit is the most this run may spend (0: whatever the payers have).
	Limit money.USD
	// Initial is the first hold (2 × Config.Step, at most Limit).
	Initial money.USD
	// TTL is how long the hold lives without a model call.
	TTL time.Duration
	// FallbackCaps count only what the run spends on a Failover's fallback
	// provider (a person's monthly allowance there), on top of Caps.
	FallbackCaps []budget.AccountID
	// FallbackLimit is the most the run may spend on the fallback: 0 is
	// whatever Limit allows, < 0 nothing (the run may not fail over).
	FallbackLimit money.USD
}

// Image is an image attached to the prompt.
type Image struct {
	MediaType string // image/png, image/jpeg
	Data      []byte
}

// Request is one run: a prompt in a workspace.
type Request struct {
	Prompt  string
	Images  []Image
	Workdir string
	// Model, or "" to let the router pick by Complexity.
	Model      model.ID
	Complexity route.Complexity
	// Effort is Muse's reasoning effort (minimal … max); "" is Muse's default.
	Effort string
	// SessionID resumes an earlier session instead of starting one.
	SessionID string
	// Ephemeral keeps no session log, so the session cannot be resumed.
	Ephemeral bool
	// Budget, or nil to run without one.
	Budget *Budget
	// AnnounceBudget tells Muse its budget in the prompt, so it plans to
	// fit it.
	AnnounceBudget bool
	// Credential, or nil to use whatever the executor's environment has.
	Credential *credential.Credential
	// AuthPath is where a login's credential file is written, as the
	// executor sees the filesystem.
	AuthPath string
	// Env is added to the process environment.
	Env []string
	// Guard and Router override the Engine's for this run.
	Guard  guard.Policy
	Router route.Policy
	// ExtraRoots are more directories Muse's file tools may use besides
	// Workdir, such as where the caller reads results back from.
	ExtraRoots []string
	// MaxCalls caps the run's model calls (0: no cap). Muse is told to
	// wrap up at Config.SteerAt of it and stopped at it (ErrStepLimit).
	MaxCalls int
	// Steer brings a person's messages while the run works; each goes
	// into the running turn (MSP turn/steer). Nil brings none.
	Steer <-chan string
	// Ask asks a person about a tool call the guard leaves to them
	// (guard.Verdict.Ask). It must return at once, with a channel that
	// gets their answer (true allows). Meanwhile the call is held back and
	// Muse goes on; once they allow it, Muse is told to run it and the
	// guard lets it through. No answer in Config.AskTimeout, or no Ask,
	// denies it.
	Ask func(Approval) <-chan bool
}

func (r Request) validate(hasLedger bool) error {
	switch {
	case r.Prompt == "" && len(r.Images) == 0:
		return fmt.Errorf("%w: empty prompt", ErrInvalid)
	case r.Budget != nil && !hasLedger:
		return fmt.Errorf("%w: a budget needs Config.Ledger", ErrInvalid)
	case r.Budget != nil && (r.Budget.Key == "" || len(r.Budget.Payers) == 0):
		return fmt.Errorf("%w: a budget needs a key and a payer", ErrInvalid)
	case r.Budget != nil && r.Budget.Limit < 0:
		return fmt.Errorf("%w: negative limit", ErrInvalid)
	}
	return nil
}

// StopReason says why a run ended before Muse finished.
type StopReason string

// Stop reasons.
const (
	StopNone     StopReason = ""
	StopBudget   StopReason = "budget"    // the payers or a cap ran out
	StopRunLimit StopReason = "run_limit" // the run's own limit
	StopContext  StopReason = "context"   // the caller cancelled
	StopSteps    StopReason = "steps"     // the run's MaxCalls
)

// Result is what a run did.
type Result struct {
	SessionID string
	TurnID    string
	// Terminal is Muse's turn terminal: completed, failed or cancelled.
	Terminal string
	// Text is the final answer.
	Text string
	// Model is the model the run ended on.
	Model model.ID
	// Usage is the metered usage and cost.
	Usage meter.Snapshot
	// Receipt is what the ledger charged (nil without a budget).
	Receipt *budget.Receipt
	Stopped StopReason
	// Steered and Escalated report budget steering and model escalation.
	Steered     bool
	Escalated   bool
	Denied      int // tool calls the guard denied
	Duration    time.Duration
	MuseVersion string
	// Fallback names the provider the run ran on when a Failover moved it
	// off its own credential ("" when it did not).
	Fallback string
	// Primary is the attempt on the run's own credential before it failed
	// over (nil when it did not run there). Usage and Duration include it;
	// Receipt is the fallback's, Primary.Receipt the primary's.
	Primary *Result
}

// Cost is what the run cost (the metered total).
func (r Result) Cost() money.USD { return r.Usage.Total }

// Models lists the models Muse offers to cred.
func (e *Engine) Models(ctx context.Context, ex Executor, cred *credential.Credential, authPath string) ([]msp.ModelEntry, error) {
	var res msp.ModelListResult
	err := e.oneShot(ctx, ex, cred, authPath, func(c *msp.Client) error {
		return c.Call(ctx, msp.MethodModelList, struct{}{}, &res)
	})
	return res.Models, err
}

// Allowance reads the account's subscription usage without a model call.
func (e *Engine) Allowance(ctx context.Context, ex Executor, cred *credential.Credential, authPath string) (*msp.SubscriptionUsage, error) {
	var res msp.UsageReadResult
	err := e.oneShot(ctx, ex, cred, authPath, func(c *msp.Client) error {
		return c.Call(ctx, msp.MethodUsageRead, struct{}{}, &res)
	})
	return res.Usage, err
}

// oneShot starts `muse serve`, runs fn after the handshake and stops it.
func (e *Engine) oneShot(ctx context.Context, ex Executor, cred *credential.Credential, authPath string, fn func(*msp.Client) error) error {
	cmd, err := e.command(Request{Credential: cred, AuthPath: authPath, Ephemeral: true})
	if err != nil {
		return err
	}
	proc, err := ex.Start(ctx, cmd)
	if err != nil {
		return err
	}
	c := msp.NewClient(proc)
	defer func() {
		_ = c.Close()
		stopProcess(proc, 5*time.Second)
	}()
	if _, err := c.Initialize(ctx, msp.ClientInfo{Name: e.cfg.ClientName, Version: Version}); err != nil {
		return err
	}
	return fn(c)
}

func (e *Engine) command(req Request) (Command, error) {
	args := []string{"serve"}
	if req.Ephemeral {
		args = append(args, "--no-session-log")
	}
	if req.Credential != nil {
		args = append(args, req.Credential.Args()...)
	}
	args = append(args, e.cfg.ServeArgs...)
	cmd := Command{Path: e.cfg.Binary, Args: args, Dir: req.Workdir, Env: append([]string(nil), req.Env...)}
	if req.Credential != nil {
		env, files, err := req.Credential.Setup(req.AuthPath)
		if err != nil {
			return Command{}, err
		}
		cmd.Env = append(cmd.Env, env...)
		cmd.Files = files
	}
	return cmd, nil
}

// stopProcess waits for the process to exit after its stdin closed, and
// kills it if it does not within grace.
func stopProcess(p Process, grace time.Duration) {
	done := make(chan struct{})
	go func() {
		_ = p.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(grace):
		_ = p.Kill()
		<-done
	}
}

// isFunds reports a ledger refusal (as opposed to a ledger failure).
func isFunds(err error) bool {
	return errors.Is(err, budget.ErrInsufficient) || errors.Is(err, budget.ErrCapReached)
}
