package muse

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
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

// run is the state of one Run.
type run struct {
	e      *Engine
	req    Request
	ctx    context.Context
	on     func(Event)
	router route.Policy
	guard  guard.Policy
	redact credential.Redactor
	meter  *meter.Meter
	client *msp.Client

	model     model.ID
	sessionID string
	turnID    string

	// budget
	hold      *budget.Hold
	held      money.USD
	fundsLow  bool
	fundsErr  error
	nextCall  money.USD // forecast of the next model call
	steered   bool
	stopped   StopReason
	stopErr   error
	interrupt time.Time

	// routing and guard
	failedTools int
	escalations int
	decided     map[string]bool
	denied      int

	// asking a person (Request.Ask): what is being asked about, what they
	// allowed (by command or path), and their answers as they come
	asking  map[string]bool
	allowed map[string]bool
	answers chan answer
	quit    chan struct{} // closed when the run ends

	final    string
	lastText string
	// answered is set when the current turn had an agent message; a turn
	// that ends without one is continued (Config.Continues).
	answered  bool
	continues int
}

// Run executes req and returns when Muse's turn ends, the budget stops it,
// or ctx is cancelled. The Result is filled in every case, including the
// cost charged; the error says why the run did not complete.
func (e *Engine) Run(ctx context.Context, req Request, ex Executor, onEvent func(Event)) (Result, error) {
	if err := req.validate(e.cfg.Ledger != nil); err != nil {
		return Result{}, err
	}
	if onEvent == nil {
		onEvent = func(Event) {}
	}
	r := &run{e: e, req: req, ctx: ctx, on: onEvent, router: e.cfg.Router, guard: e.cfg.Guard,
		meter: meter.New(e.cfg.Catalog), decided: map[string]bool{},
		asking: map[string]bool{}, allowed: map[string]bool{}, answers: make(chan answer), quit: make(chan struct{})}
	defer close(r.quit)
	if req.Router != nil {
		r.router = req.Router
	}
	if req.Guard != nil {
		r.guard = req.Guard
	}
	if req.Credential != nil {
		r.redact = credential.NewRedactor(req.Credential.Secrets()...)
	}
	r.model = req.Model
	if r.model == "" {
		r.model = r.router.Initial(req.Complexity)
	}
	start := time.Now()
	res, err := r.execute(ex)
	res.Duration = time.Since(start)
	return res, err
}

func (r *run) execute(ex Executor) (res Result, err error) {
	if r.req.Budget != nil {
		if err := r.openHold(); err != nil {
			return Result{Model: r.model}, err
		}
	}
	// Whatever happens next, charge what was metered, once.
	defer func() {
		res.Model, res.SessionID, res.TurnID = r.model, r.sessionID, r.turnID
		res.Usage = r.meter.Snapshot()
		res.Stopped, res.Steered, res.Escalated, res.Denied = r.stopped, r.steered, r.escalations > 0, r.denied
		if res.Text == "" {
			res.Text = r.redact.Redact(firstNonEmpty(r.final, r.lastText))
		}
		if rec, serr := r.settle(); serr != nil {
			err = errors.Join(err, serr)
		} else {
			res.Receipt = rec
		}
	}()

	cmd, err := r.e.command(r.req)
	if err != nil {
		return res, err
	}
	procCtx, cancelProc := context.WithCancel(context.WithoutCancel(r.ctx))
	defer cancelProc()
	proc, err := ex.Start(procCtx, cmd)
	if err != nil {
		return res, err
	}
	var opts []msp.Option
	if r.e.cfg.Trace != nil {
		opts = append(opts, msp.WithTrace(r.e.cfg.Trace))
	}
	r.client = msp.NewClient(proc, opts...)
	defer func() {
		r.leave()
		_ = r.client.Close()
		stopProcess(proc, 5*time.Second)
	}()

	init, err := r.client.Initialize(r.ctx, msp.ClientInfo{Name: r.e.cfg.ClientName, Version: Version})
	if err != nil {
		return res, fmt.Errorf("muse: initialize: %w", err)
	}
	res.MuseVersion = init.ServerInfo.Version
	if init.Schema.Fingerprint != msp.SchemaFingerprint {
		r.emit(Event{Kind: EventWarning, Reason: "Muse speaks MSP schema " + init.Schema.Fingerprint + "; agnts-muse was checked against " + msp.SchemaFingerprint})
	}
	if err := r.openSession(); err != nil {
		return res, err
	}
	if err := r.startTurn(); err != nil {
		return res, err
	}
	r.emit(Event{Kind: EventStarted})
	done, err := r.loop()
	res.Terminal = done.Terminal
	return res, err
}

func (r *run) openSession() error {
	if r.req.SessionID != "" {
		var s msp.SessionResult
		if err := r.client.Call(r.ctx, msp.MethodSessionResume, msp.SessionResumeParams{CommandID: msp.NewCommandID(), SessionID: r.req.SessionID}, &s); err != nil {
			return fmt.Errorf("muse: resume session %s: %w", r.req.SessionID, err)
		}
		r.sessionID = s.Session.SessionID
		if s.Session.ModelID == nil || model.ID(*s.Session.ModelID) != r.model {
			return r.setModel(r.model)
		}
		return nil
	}
	var s msp.SessionResult
	err := r.client.Call(r.ctx, msp.MethodSessionStart, msp.SessionStartParams{CommandID: msp.NewCommandID(),
		WorkspaceRoot: r.req.Workdir, WorkspaceRoots: r.roots(), ModelID: string(r.model), ApprovalMode: r.e.cfg.ApprovalMode}, &s)
	if err != nil {
		return fmt.Errorf("muse: start session: %w", err)
	}
	r.sessionID = s.Session.SessionID
	return nil
}

func (r *run) startTurn() error {
	input := []msp.InputPart{msp.Text(r.prompt())}
	for _, img := range r.req.Images {
		input = append(input, msp.InputPart{Type: "image", MediaType: img.MediaType, Base64Data: base64.StdEncoding.EncodeToString(img.Data)})
	}
	return r.turn(input)
}

// continueTurn starts another turn in the session after one that ended
// with neither a tool call nor an answer.
func (r *run) continueTurn() error {
	return r.turn([]msp.InputPart{msp.Text(continuePrompt)})
}

// continuePrompt asks the model to carry on after an empty last step.
const continuePrompt = "[Continue] Your last step ended without a tool call or an answer, so the task is not done. " +
	"Carry on from where you are: call the tools you need to make the changes, then finish with a short summary of what you changed and checked."

func (r *run) turn(input []msp.InputPart) error {
	r.answered = false
	var t msp.TurnStartResult
	if err := r.client.Call(r.ctx, msp.MethodTurnStart, msp.TurnStartParams{CommandID: msp.NewCommandID(), SessionID: r.sessionID,
		Input: input, ReasoningEffort: r.req.Effort}, &t); err != nil {
		return fmt.Errorf("muse: start turn: %w", err)
	}
	r.turnID = t.TurnID
	return nil
}

// prompt adds the budget to the prompt when the run announces it.
func (r *run) prompt() string {
	b := r.req.Budget
	if !r.req.AnnounceBudget || b == nil || b.Limit <= 0 {
		return r.req.Prompt
	}
	return r.req.Prompt + fmt.Sprintf("\n\n[Budget] This task has a hard budget of %v of model usage on %s; the run is stopped when it is spent. "+
		"Plan to finish well within it: read only the files you need, make focused changes, run the most relevant checks, and stop when done.",
		b.Limit, r.model)
}

// loop handles notifications until our turn completes, and brings a
// person's messages and answers into the turn as they come.
func (r *run) loop() (msp.TurnCompleted, error) {
	var killAt <-chan time.Time
	ctxDone := r.ctx.Done()
	steer := r.req.Steer
	for {
		select {
		case msg, ok := <-steer:
			if !ok {
				steer = nil
				continue
			}
			if msg = strings.TrimSpace(msg); msg != "" && r.stopped == StopNone {
				r.steerWith("[Message from the person following this task] "+msg, "person")
			}
		case a := <-r.answers:
			r.onAnswer(a)
		case n, ok := <-r.client.Notifications():
			if !ok {
				err := r.client.Err()
				if r.stopped != StopNone {
					return msp.TurnCompleted{Terminal: msp.TerminalCancelled}, r.stopError()
				}
				return msp.TurnCompleted{}, fmt.Errorf("%w before the turn ended: %w", ErrExited, err)
			}
			if done, ok := r.handle(n); ok {
				if r.shouldContinue(done) {
					r.continues++
					r.emit(Event{Kind: EventContinue, Reason: fmt.Sprintf("the turn ended without an answer; continuing (%d of %d)", r.continues, r.e.cfg.Continues)})
					if err := r.continueTurn(); err != nil {
						return done, err
					}
					continue
				}
				return done, r.turnError(done)
			}
		case <-ctxDone:
			ctxDone = nil
			r.stop(StopContext, r.ctx.Err(), "the caller cancelled the run")
		case <-killAt:
			_ = r.client.Close() // the process is stopped by the deferred stopProcess
			killAt = nil
		}
		if !r.interrupt.IsZero() && killAt == nil {
			killAt = time.After(r.e.cfg.InterruptGrace)
		}
	}
}

// handle processes one notification; ok is true when our turn completed.
func (r *run) handle(n msp.Notification) (msp.TurnCompleted, bool) {
	switch n.Method {
	case msp.NoteTokenUsage:
		if u, err := msp.Decode[msp.TokenUsage](n); err == nil && u.SessionID == r.sessionID {
			r.onUsage(u)
		}
	case msp.NoteItemDelta:
		if d, err := msp.Decode[msp.ItemDelta](n); err == nil && d.SessionID == r.sessionID && (d.Field == "" || d.Field == "text") {
			r.emit(Event{Kind: EventDelta, Text: d.Delta})
		}
	case msp.NoteItemStarted:
		if it, err := msp.Decode[msp.ItemEvent](n); err == nil && it.SessionID == r.sessionID && it.Item.Kind == msp.ItemToolCall {
			r.emit(Event{Kind: EventToolStart, Tool: it.Item.Tool, Args: it.Item.Args})
		}
	case msp.NoteItemCompleted:
		if it, err := msp.Decode[msp.ItemEvent](n); err == nil && it.SessionID == r.sessionID {
			r.onItem(it.Item)
		}
	case msp.NoteApprovalRequested, msp.NoteApprovalReviewing:
		if a, err := msp.Decode[msp.ApprovalRequested](n); err == nil && a.SessionID == r.sessionID {
			r.onApproval(a, n.Method == msp.NoteApprovalRequested)
		}
	case msp.NoteUsageChanged:
		if u, err := msp.Decode[msp.SubscriptionUsage](n); err == nil {
			r.emit(Event{Kind: EventAllowance, Allowance: &u})
		}
	case msp.NoteTurnRetry, msp.NoteTurnModelRetry:
		if rs, err := msp.Decode[msp.RetryScheduled](n); err == nil && rs.SessionID == r.sessionID {
			r.emit(Event{Kind: EventRetry, Reason: fmt.Sprintf("attempt %d/%d in %dms: %s %s", rs.Attempt, rs.MaxAttempts, rs.RetryDelayMs, rs.ErrorKind, rs.Reason)})
		}
	case msp.NoteTurnCompleted:
		if c, err := msp.Decode[msp.TurnCompleted](n); err == nil && c.SessionID == r.sessionID && c.TurnID == r.turnID {
			return c, true
		}
	}
	return msp.TurnCompleted{}, false
}

// shouldContinue reports a turn that completed without any agent message:
// the model's last step had neither a tool call nor an answer (Muse Spark
// Open on Baseten sometimes returns only its reasoning), so the task was
// left unfinished, not done.
func (r *run) shouldContinue(c msp.TurnCompleted) bool {
	return c.Terminal == msp.TerminalCompleted && !r.answered && r.stopped == StopNone &&
		r.continues < r.e.cfg.Continues && r.ctx.Err() == nil
}

func (r *run) onItem(it msp.Item) {
	switch it.Kind {
	case msp.ItemAgentMessage:
		if it.Text == "" {
			return
		}
		r.answered = true
		final := it.MessagePhase == "finalAnswer"
		r.lastText = it.Text
		if final {
			r.final = it.Text
		}
		r.emit(Event{Kind: EventMessage, Text: it.Text, Final: final})
	case msp.ItemToolCall:
		failed := it.Status != msp.ItemCompleted
		if failed {
			r.failedTools++
		} else {
			r.failedTools = 0
		}
		out := it.VisibleOutput
		if out == "" {
			out = it.FailureReason
		}
		r.emit(Event{Kind: EventToolEnd, Tool: it.Tool, Args: it.Args, Output: out, Failed: failed})
		r.route()
	}
}

// onUsage charges a model call and enforces the budget.
func (r *run) onUsage(u msp.TokenUsage) {
	id := r.model
	if u.ModelID != nil && *u.ModelID != "" {
		id = model.ID(*u.ModelID)
	}
	c := r.meter.Add(id, model.Usage{Input: u.Usage.InputTokens, Cached: u.Usage.CachedTokens,
		Output: u.Usage.OutputTokens, Reasoning: u.Usage.ReasoningTokens})
	r.nextCall = r.meter.Forecast(r.model)
	if r.hold != nil {
		r.extend(c.Total)
	}
	r.emit(Event{Kind: EventUsage, Charge: &c, Held: r.held, Model: id})
	if r.hold != nil {
		r.enforce(c.Total)
	}
	r.limitCalls(c.Calls)
	r.route()
}

// limitCalls steers the run at SteerAt of its MaxCalls and stops it there.
func (r *run) limitCalls(calls int) {
	maxCalls := r.req.MaxCalls
	if maxCalls <= 0 || r.stopped != StopNone {
		return
	}
	switch {
	case calls >= maxCalls:
		r.stop(StopSteps, ErrStepLimit, fmt.Sprintf("reached the cap of %d model calls", maxCalls))
	case !r.steered && float64(calls) >= r.e.cfg.SteerAt*float64(maxCalls):
		r.steered = true
		r.steerWith(fmt.Sprintf("[Step notice] You have used %d of the %d model calls this task may take. "+
			"Stop exploring: finish with what you have and give your answer now, in the format asked for.", calls, maxCalls), "steps")
	}
}

// extend keeps the hold Step ahead of spent; when the ledger refuses,
// the run is marked low on funds for enforce.
func (r *run) extend(spent money.USD) {
	b := r.req.Budget
	target := spent + r.e.cfg.Step
	if b.Limit > 0 {
		target = money.Min(target, b.Limit)
	}
	if target > r.held && !r.fundsLow {
		h, err := r.e.cfg.Ledger.Extend(context.WithoutCancel(r.ctx), r.hold.ID, target-r.held)
		switch {
		case err == nil:
			r.hold, r.held = &h, h.Amount
		case isFunds(err):
			r.fundsLow, r.fundsErr = true, err
		default:
			r.emit(Event{Kind: EventWarning, Reason: "budget ledger: " + err.Error()})
			r.fundsLow, r.fundsErr = true, err
		}
	}
}

// enforce steers or stops the run. A run is stopped when it could not
// afford the next call as the meter forecasts it, so it ends inside its
// budget rather than one call past it.
func (r *run) enforce(spent money.USD) {
	b := r.req.Budget
	next := spent + r.nextCall
	switch {
	case b.Limit > 0 && next > b.Limit:
		r.stop(StopRunLimit, ErrRunLimit, fmt.Sprintf("spent %v of the %v limit; another call would pass it", spent, b.Limit))
	case r.fundsLow && next > r.held:
		r.stop(StopBudget, errors.Join(ErrBudget, r.fundsErr), fmt.Sprintf("spent %v and no more money can be held", spent))
	case !r.steered && (r.fundsLow || b.Limit > 0 && float64(spent) >= r.e.cfg.SteerAt*float64(b.Limit)):
		r.steer(spent)
	}
}

// steer tells Muse, mid-turn, that the budget is running low.
func (r *run) steer(spent money.USD) {
	r.steered = true
	left := r.held - spent
	if l := r.req.Budget.Limit; l > 0 && l-spent < left {
		left = l - spent
	}
	r.steerWith(fmt.Sprintf("[Budget notice] %v of this task's budget is used and about %v is left. "+
		"Finish the change you are making, run the single most relevant check, and reply with a short summary. Do not start new exploration.",
		spent, left), "budget")
}

// steerWith sends msg into the running turn.
func (r *run) steerWith(msg, reason string) {
	err := r.client.Call(context.WithoutCancel(r.ctx), msp.MethodTurnSteer, msp.TurnSteerParams{CommandID: msp.NewCommandID(),
		SessionID: r.sessionID, ExpectedTurnID: r.turnID, Input: []msp.InputPart{msp.Text(msg)}}, nil)
	if err != nil {
		r.emit(Event{Kind: EventWarning, Reason: "steer: " + err.Error()})
		return
	}
	r.emit(Event{Kind: EventSteer, Text: msg, Reason: reason})
}

// stop interrupts the turn once.
func (r *run) stop(reason StopReason, err error, why string) {
	if r.stopped != StopNone {
		return
	}
	r.stopped, r.stopErr, r.interrupt = reason, err, time.Now()
	r.emit(Event{Kind: EventInterrupt, Reason: why})
	ierr := r.client.Call(context.WithoutCancel(r.ctx), msp.MethodTurnInterrupt,
		msp.TurnInterruptParams{CommandID: msp.NewCommandID(), SessionID: r.sessionID, TurnID: r.turnID}, nil)
	if ierr != nil && !errors.Is(ierr, msp.ErrTurnNotActive) {
		r.emit(Event{Kind: EventWarning, Reason: "interrupt: " + ierr.Error()})
		_ = r.client.Close()
	}
}

// route asks the router whether to change model.
func (r *run) route() {
	if r.stopped != StopNone {
		return
	}
	snap := r.meter.Snapshot()
	remaining := money.USD(-1)
	if b := r.req.Budget; b != nil {
		remaining = r.held - snap.Total
		if b.Limit > 0 {
			remaining = b.Limit - snap.Total
		}
		if r.fundsLow {
			remaining = money.Min(remaining, r.held-snap.Total)
		}
	}
	d, ok := r.router.Next(route.Signal{Model: r.model, Calls: snap.Calls, FailedTools: r.failedTools,
		Spent: snap.Total, Remaining: remaining, Escalations: r.escalations})
	if !ok || d.To == r.model {
		return
	}
	if err := r.setModel(d.To); err != nil {
		r.emit(Event{Kind: EventWarning, Reason: "escalate: " + err.Error()})
		r.escalations++ // do not retry every call
		return
	}
	r.escalations++
	r.failedTools = 0
	r.emit(Event{Kind: EventEscalate, Model: d.To, Reason: d.Reason})
}

func (r *run) setModel(id model.ID) error {
	err := r.client.Call(context.WithoutCancel(r.ctx), msp.MethodSessionSetModel, msp.SessionSetModelParams{CommandID: msp.NewCommandID(),
		SessionID: r.sessionID, Model: msp.ModelSelection{ModelID: string(id)}}, nil)
	if err == nil {
		r.model = id
	}
	return err
}

// onApproval applies the guard to a tool call Muse is about to run. While
// Muse's own judge is still reviewing it, only a denial is sent (the judge
// may still refuse what the guard allows); when Muse asks the client
// outright, the guard's answer is final. A call the guard leaves to a
// person is held back (a denial, which wins over the judge) while they are
// asked; their answer comes later (onAnswer).
func (r *run) onApproval(a msp.ApprovalRequested, asked bool) {
	key := fmt.Sprintf("%s/%d", a.ApprovalID, a.CurrentRequirementID.SourceIndex)
	if r.decided[key] {
		return
	}
	req := guard.Request{Tool: a.ToolName, Kind: a.Subject.Kind, Command: a.Subject.Command, Path: a.Subject.Path,
		Host: a.Subject.Host, Args: a.RawArgs}
	for _, st := range a.Subject.Stages {
		req.Argv = append(req.Argv, st.Argv)
	}
	subject := firstNonEmpty(a.Subject.Command, a.Subject.Path, a.ToolName)
	v := r.guard.Decide(req)
	byPerson := v.Ask && r.allowed[subject]
	if byPerson {
		v = guard.Verdict{Allow: true, Rule: v.Rule, Reason: "the person allowed it"}
	}
	if v.Allow && !asked {
		return // leave it to Muse's judge
	}
	r.decided[key] = true
	choice, ok := pickChoice(a.AvailableChoices, v.Allow)
	ap := &Approval{ID: a.ApprovalID, Tool: a.ToolName, Command: a.Subject.Command, Path: a.Subject.Path, Allow: v.Allow, Ask: v.Ask || byPerson,
		Rule: v.Rule, Reason: v.Reason}
	feedback := "Blocked by policy (" + v.Rule + "): " + v.Reason
	if v.Ask {
		if r.req.Ask == nil {
			ap.Ask, feedback = false, "Blocked by policy ("+v.Rule+"): "+v.Reason+", and a person must allow that; nobody can be asked in this run"
		} else {
			ap.Pending = true
			feedback = "A person must allow this first (" + v.Reason + "); they have been asked. Go on with other work; you will be told when they answer."
			if !r.asking[subject] {
				r.asking[subject] = true
				r.ask(subject, *ap)
			}
		}
	}
	if ok {
		params := msp.ApprovalDecideParams{CommandID: msp.NewCommandID(), SessionID: r.sessionID, ApprovalID: a.ApprovalID,
			ChoiceID: choice.ChoiceID, RequirementID: a.CurrentRequirementID}
		if !v.Allow && choice.AcceptsFeedback && v.Reason != "" {
			params.Feedback = &feedback
		}
		err := r.client.Call(context.WithoutCancel(r.ctx), msp.MethodApprovalDecide, params, nil)
		ap.Applied = err == nil
		if err != nil && !errors.Is(err, msp.ErrApprovalResolved) && !errors.Is(err, msp.ErrApprovalStale) {
			r.emit(Event{Kind: EventWarning, Reason: "approval: " + err.Error()})
		}
	}
	if !v.Allow && !ap.Pending {
		r.denied++
	}
	r.emit(Event{Kind: EventApproval, Tool: a.ToolName, Approval: ap})
}

// answer is a person's answer about a tool call (answered false: none came
// in time).
type answer struct {
	subject  string
	ap       Approval
	allow    bool
	answered bool
}

// ask puts a held-back tool call to the person, and brings their answer
// (or its absence after Config.AskTimeout) back to the loop.
func (r *run) ask(subject string, ap Approval) {
	ch := r.req.Ask(ap)
	go func() {
		t := time.NewTimer(r.e.cfg.AskTimeout)
		defer t.Stop()
		a := answer{subject: subject, ap: ap}
		select {
		case allow, ok := <-ch:
			a.allow, a.answered = ok && allow, ok
		case <-t.C:
		case <-r.quit:
			return
		}
		select {
		case r.answers <- a:
		case <-r.quit:
		}
	}()
}

// onAnswer tells Muse what the person decided about a held-back call.
// Allowed, the guard lets the same command or path through from now on.
func (r *run) onAnswer(a answer) {
	delete(r.asking, a.subject)
	var msg string
	switch {
	case a.allow:
		r.allowed[a.subject] = true
		msg = "[Approval] The person allowed: " + a.subject + "\nRun it now if you still need it."
	case a.answered:
		r.denied++
		msg = "[Approval] The person did not allow: " + a.subject + "\nDo not run it. Find another way, or say in your answer why it is needed."
	default:
		r.denied++
		msg = "[Approval] Nobody answered in time about: " + a.subject + "\nIt stays denied. Do not run it; find another way, or say in your answer why it is needed."
	}
	if r.stopped == StopNone {
		r.steerWith(msg, "approval")
	}
	ap := a.ap
	ap.Pending, ap.Allow = false, a.allow
	r.emit(Event{Kind: EventApproval, Tool: ap.Tool, Approval: &ap})
}

// pickChoice finds the one-time allow, or the denial, among Muse's choices.
func pickChoice(choices []msp.ApprovalChoice, allow bool) (msp.ApprovalChoice, bool) {
	for _, c := range choices {
		if allow && c.Decision == msp.DecisionApproved && c.Scope == "once" {
			return c, true
		}
		if !allow && (c.Decision == msp.DecisionDenied || c.Decision == "abort") {
			return c, true
		}
	}
	return msp.ApprovalChoice{}, false
}

// stopError is the error for a run stopped on purpose.
func (r *run) stopError() error {
	if r.stopped == StopContext || r.stopped == StopSteps {
		return r.stopErr
	}
	snap := r.meter.Snapshot()
	return &BudgetError{Spent: snap.Total, Held: r.held, Err: r.stopErr}
}

// turnError maps a completed turn to the run's error.
func (r *run) turnError(c msp.TurnCompleted) error {
	if r.stopped != StopNone && c.Terminal != msp.TerminalCompleted {
		return r.stopError()
	}
	switch c.Terminal {
	case msp.TerminalCompleted:
		return nil
	case msp.TerminalCancelled:
		return &TurnError{Kind: "cancelled", Message: firstNonEmpty(c.Reason, "the turn was cancelled")}
	}
	e := c.Error
	if e == nil {
		return &TurnError{Kind: "unknown", Message: firstNonEmpty(c.Reason, "the turn failed")}
	}
	switch {
	case e.Quota != nil:
		return &RateLimitError{Kind: e.Quota.LimitKind, Until: e.Quota.ResetsAt()}
	case e.Kind == "authRequired":
		return fmt.Errorf("%w: %s", ErrAuth, r.redact.Redact(e.Message))
	case e.Kind == "stepLimit":
		return ErrStepLimit
	}
	return &TurnError{Kind: e.Kind, Message: r.redact.Redact(e.Message), Retryable: e.Retryable}
}

// openHold reserves the first part of the budget.
func (r *run) openHold() error {
	b := r.req.Budget
	initial := b.Initial
	if initial <= 0 {
		initial = 2 * r.e.cfg.Step
	}
	if b.Limit > 0 {
		initial = money.Min(initial, b.Limit)
	}
	// A payer with less than the first hold can still afford a short run:
	// halve the hold down to a cent before giving up.
	h, err := r.e.cfg.Ledger.Hold(r.ctx, budget.HoldRequest{Key: b.Key, Payers: b.Payers, Caps: b.Caps, Amount: initial, TTL: b.TTL})
	for isFunds(err) && initial > money.Cent {
		initial = money.Max(initial/2, money.Cent)
		h, err = r.e.cfg.Ledger.Hold(r.ctx, budget.HoldRequest{Key: b.Key, Payers: b.Payers, Caps: b.Caps, Amount: initial, TTL: b.TTL})
	}
	if err != nil {
		if isFunds(err) {
			return &BudgetError{Err: errors.Join(ErrBudget, err)}
		}
		return fmt.Errorf("muse: hold budget: %w", err)
	}
	if h.State != budget.StateOpen {
		return fmt.Errorf("%w: budget key %q belongs to a finished run", ErrInvalid, b.Key)
	}
	r.hold, r.held = &h, h.Amount
	return nil
}

// settle commits the metered cost (or releases an unused hold).
func (r *run) settle() (*budget.Receipt, error) {
	if r.hold == nil {
		return nil, nil
	}
	ctx := context.WithoutCancel(r.ctx)
	total := r.meter.Total()
	rec, err := r.e.cfg.Ledger.Commit(ctx, r.hold.ID, total)
	if err != nil {
		return nil, fmt.Errorf("muse: settle %v: %w", total, err)
	}
	if rec.Unbilled > 0 {
		r.emit(Event{Kind: EventWarning, Reason: fmt.Sprintf("%v of the last call was over the hold and is unbilled", rec.Unbilled)})
	}
	r.emit(Event{Kind: EventSettled, Receipt: &rec})
	return &rec, nil
}

// leave releases the session on the host; it stays on disk for resume.
func (r *run) leave() {
	if r.sessionID == "" || r.client.Err() != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.ctx), 5*time.Second)
	defer cancel()
	_ = r.client.Call(ctx, msp.MethodSessionLeave, msp.SessionLeaveParams{CommandID: msp.NewCommandID(), SessionID: r.sessionID}, nil)
}

func (r *run) emit(e Event) {
	e.Time = time.Now()
	e.SessionID, e.TurnID = r.sessionID, r.turnID
	if e.Model == "" {
		e.Model = r.model
	}
	e.Text, e.Args, e.Output, e.Reason = r.redact.Redact(e.Text), r.redact.Redact(e.Args), r.redact.Redact(e.Output), r.redact.Redact(e.Reason)
	r.on(e)
	if o := r.e.cfg.Observer; o != nil {
		o.Observe(r.ctx, e)
	}
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// roots is the session's workspace roots: Workdir first, then ExtraRoots
// (nil without extras, so Muse uses Workdir alone).
func (r *run) roots() []string {
	if len(r.req.ExtraRoots) == 0 {
		return nil
	}
	return append([]string{r.req.Workdir}, r.req.ExtraRoots...)
}
