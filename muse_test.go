package muse_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	muse "github.com/rustambekabdusalomov/agnts-muse"
	"github.com/rustambekabdusalomov/agnts-muse/budget"
	"github.com/rustambekabdusalomov/agnts-muse/budget/memledger"
	"github.com/rustambekabdusalomov/agnts-muse/credential"
	"github.com/rustambekabdusalomov/agnts-muse/meter"
	"github.com/rustambekabdusalomov/agnts-muse/model"
	"github.com/rustambekabdusalomov/agnts-muse/money"
	"github.com/rustambekabdusalomov/agnts-muse/msp"
	"github.com/rustambekabdusalomov/agnts-muse/msp/msptest"
)

const (
	recordedSession = "01a17c50-763a-7dd2-b0f3-0a6fde9529e7"
	recordedTurn    = "01a17c50-7715-79b7-b835-f5c4a541b8f5"
	guardSession    = "01a17c5e-3daf-7ea1-858f-88f4936fa938"
)

// fakeMuse is a `muse serve` that replays a recorded turn. It pauses after
// each model call so the engine's reaction (steer, interrupt) lands before
// the next one, like real calls that take seconds.
type fakeMuse struct {
	t       *testing.T
	rec     msptest.Recording
	session string // the recorded session id to rewrite
	turn    string
	pause   time.Duration

	host        *msptest.Host
	interrupted atomic.Bool
	started     chan struct{}
	cmd         muse.Command
	mu          sync.Mutex
}

func newFake(t *testing.T, path, session string) *fakeMuse {
	t.Helper()
	rec, err := msptest.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return &fakeMuse{t: t, rec: rec.Turn(), session: session, pause: 30 * time.Millisecond, started: make(chan struct{})}
}

// Start implements muse.Executor.
func (f *fakeMuse) Start(_ context.Context, cmd muse.Command) (muse.Process, error) {
	f.mu.Lock()
	f.cmd = cmd
	f.mu.Unlock()
	h, conn := msptest.New()
	f.host = h
	h.Handle(msp.MethodSessionStart, func(json.RawMessage) (any, error) {
		return msp.SessionResult{Session: msp.Session{SessionID: "s-test", Status: "idle"}}, nil
	})
	h.Handle(msp.MethodSessionLeave, func(json.RawMessage) (any, error) { return msp.CommandAck{Status: "accepted"}, nil })
	h.Handle(msp.MethodTurnSteer, func(json.RawMessage) (any, error) { return msp.CommandAck{Status: "accepted"}, nil })
	h.Handle(msp.MethodSessionSetModel, func(json.RawMessage) (any, error) { return msp.CommandAck{Status: "accepted"}, nil })
	h.Handle(msp.MethodApprovalDecide, func(json.RawMessage) (any, error) { return msp.CommandAck{Status: "accepted"}, nil })
	h.Handle(msp.MethodTurnInterrupt, func(json.RawMessage) (any, error) {
		f.interrupted.Store(true)
		return msp.CommandAck{Status: "accepted", TurnID: "t-test"}, nil
	})
	h.Handle(msp.MethodTurnStart, func(json.RawMessage) (any, error) {
		go f.replay()
		return msp.TurnStartResult{TurnID: "t-test", Disposition: "started", StartedNewTurn: true}, nil
	})
	return &fakeProcess{ReadWriteCloser: conn, host: h}, nil
}

func (f *fakeMuse) replay() {
	turn := f.turn
	if turn == "" {
		turn = recordedTurn
	}
	rec := f.rec.Rewrite(f.session, "s-test").Rewrite(turn, "t-test")
	for _, n := range rec {
		if f.interrupted.Load() {
			_ = f.host.Notify(msp.NoteTurnCompleted, msp.TurnCompleted{SessionID: "s-test", TurnID: "t-test", Terminal: msp.TerminalCancelled, Reason: "interrupted"})
			return
		}
		if err := f.host.NotifyRaw(n); err != nil {
			return
		}
		if n.Method == msp.NoteTokenUsage || n.Method == msp.NoteApprovalReviewing {
			time.Sleep(f.pause)
		}
	}
}

type fakeProcess struct {
	io.ReadWriteCloser
	host *msptest.Host
}

func (p *fakeProcess) Wait() error { return nil }
func (p *fakeProcess) Kill() error { return p.host.Close() }

func collect() (func(muse.Event), func() []muse.Event) {
	var mu sync.Mutex
	var evs []muse.Event
	return func(e muse.Event) {
			mu.Lock()
			evs = append(evs, e)
			mu.Unlock()
		}, func() []muse.Event {
			mu.Lock()
			defer mu.Unlock()
			return append([]muse.Event(nil), evs...)
		}
}

func count(evs []muse.Event, k muse.EventKind) int {
	n := 0
	for _, e := range evs {
		if e.Kind == k {
			n++
		}
	}
	return n
}

// cumulativeCosts is the running total after each recorded model call.
func cumulativeCosts(t *testing.T) []money.USD {
	t.Helper()
	rec, err := msptest.Load("testdata/msp-session.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	m := meter.New(nil)
	var out []money.USD
	for _, n := range rec {
		if n.Method == msp.NoteTokenUsage {
			u, _ := msp.Decode[msp.TokenUsage](n)
			out = append(out, m.Add(model.ID(*u.ModelID), model.Usage{Input: u.Usage.InputTokens, Cached: u.Usage.CachedTokens,
				Output: u.Usage.OutputTokens, Reasoning: u.Usage.ReasoningTokens}).Total)
		}
	}
	return out
}

// recordedCost is what the recorded five calls cost on Spark.
func recordedCost(t *testing.T) money.USD {
	c := cumulativeCosts(t)
	return c[len(c)-1]
}

func ctxT(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestRunReplaysRecordedSession(t *testing.T) {
	f := newFake(t, "testdata/msp-session.jsonl", recordedSession)
	f.pause = 0
	on, events := collect()
	res, err := muse.New(muse.Config{}).Run(ctxT(t), muse.Request{Prompt: "vet and tag", Workdir: "/workspace", Model: model.Spark}, f, on)
	if err != nil {
		t.Fatal(err)
	}
	if res.Terminal != msp.TerminalCompleted || res.SessionID != "s-test" || res.Text != "Vet failed without a module and I appended the marker." {
		t.Fatalf("result = %+v", res)
	}
	evs := events()
	if count(evs, muse.EventUsage) != 5 || res.Usage.Calls != 5 || res.Cost() != recordedCost(t) {
		t.Fatalf("usage events %d, calls %d, cost %v (want %v)", count(evs, muse.EventUsage), res.Usage.Calls, res.Cost(), recordedCost(t))
	}
	if count(evs, muse.EventToolEnd) != 4 || count(evs, muse.EventMessage) < 2 || evs[0].Kind != muse.EventStarted {
		t.Fatalf("events: %d tool ends, %d messages, first %s", count(evs, muse.EventToolEnd), count(evs, muse.EventMessage), evs[0].Kind)
	}
	if res.Receipt != nil || res.Stopped != muse.StopNone {
		t.Fatalf("no budget, yet receipt %+v stop %q", res.Receipt, res.Stopped)
	}
	// The session was opened in the workspace on the requested model with
	// the client asking for approvals, and released at the end.
	var start msp.SessionStartParams
	_ = json.Unmarshal(f.host.Sent(msp.MethodSessionStart)[0], &start)
	if start.WorkspaceRoot != "/workspace" || start.ModelID != string(model.Spark) || start.ApprovalMode != msp.ApprovalPromptUnmatched {
		t.Fatalf("session/start = %+v", start)
	}
	if len(f.host.Sent(msp.MethodSessionLeave)) != 1 {
		t.Fatal("session not released")
	}
}

func TestRunChargesTheLedger(t *testing.T) {
	ctx := ctxT(t)
	l := memledger.New()
	l.Deposit(ctx, "user:1:free", 2*money.Cent, "grant")
	l.Deposit(ctx, "ws:1:paid", money.Dollar, "pay")
	f := newFake(t, "testdata/msp-session.jsonl", recordedSession)
	on, events := collect()
	res, err := muse.New(muse.Config{Ledger: l}).Run(ctx, muse.Request{Prompt: "p", Model: model.Spark,
		Budget: &muse.Budget{Key: "run-1", Payers: []budget.AccountID{"user:1:free", "ws:1:paid"}, Limit: 50 * money.Cent}}, f, on)
	if err != nil {
		t.Fatal(err)
	}
	cost := recordedCost(t)
	if res.Receipt == nil || res.Receipt.Charged != cost || res.Receipt.Unbilled != 0 {
		t.Fatalf("receipt = %+v, want %v charged", res.Receipt, cost)
	}
	free, _ := l.Balance(ctx, "user:1:free")
	paid, _ := l.Balance(ctx, "ws:1:paid")
	if free.Total != 0 || paid.Total != money.Dollar-(cost-2*money.Cent) || paid.Held != 0 {
		t.Fatalf("free %+v paid %+v: free credits must go first", free, paid)
	}
	if count(events(), muse.EventSettled) != 1 || res.Steered {
		t.Fatalf("settled events %d steered %v", count(events(), muse.EventSettled), res.Steered)
	}
	// The hold stayed ahead of the spend after every call.
	for _, e := range events() {
		if e.Kind == muse.EventUsage && e.Charge.Total > e.Held {
			t.Fatalf("spent %v with only %v held", e.Charge.Total, e.Held)
		}
	}
}

func TestRunStopsAtItsLimit(t *testing.T) {
	ctx := ctxT(t)
	l := memledger.New()
	l.Deposit(ctx, "ws", money.Dollar, "pay")
	f := newFake(t, "testdata/msp-session.jsonl", recordedSession)
	on, events := collect()
	// Stop after the third call: a fourth like it would pass the limit.
	// The steer comes first, at 80% of it, after the second call.
	cum := cumulativeCosts(t)
	limit := cum[2] + (cum[2]-cum[1])/2
	res, err := muse.New(muse.Config{Ledger: l}).Run(ctx, muse.Request{Prompt: "p", Model: model.Spark, AnnounceBudget: true,
		Budget: &muse.Budget{Key: "run-limit", Payers: []budget.AccountID{"ws"}, Limit: limit}}, f, on)
	if !errors.Is(err, muse.ErrRunLimit) {
		t.Fatalf("err = %v, want ErrRunLimit", err)
	}
	var be *muse.BudgetError
	if !errors.As(err, &be) || be.Spent > limit {
		t.Fatalf("budget error = %#v", err)
	}
	if res.Stopped != muse.StopRunLimit || res.Terminal != msp.TerminalCancelled || !res.Steered {
		t.Fatalf("result = %+v", res)
	}
	if res.Receipt.Charged > limit || res.Receipt.Charged != res.Cost() {
		t.Fatalf("charged %v (cost %v), limit %v", res.Receipt.Charged, res.Cost(), limit)
	}
	if len(f.host.Sent(msp.MethodTurnSteer)) != 1 || len(f.host.Sent(msp.MethodTurnInterrupt)) != 1 {
		t.Fatalf("steers %d interrupts %d", len(f.host.Sent(msp.MethodTurnSteer)), len(f.host.Sent(msp.MethodTurnInterrupt)))
	}
	var turn msp.TurnStartParams
	_ = json.Unmarshal(f.host.Sent(msp.MethodTurnStart)[0], &turn)
	if res.Usage.Calls != 3 {
		t.Fatalf("stopped after %d calls, want 3", res.Usage.Calls)
	}
	if !strings.Contains(turn.Input[0].Text, "[Budget] This task has a hard budget of "+limit.String()) {
		t.Fatalf("budget not announced: %q", turn.Input[0].Text)
	}
	if count(events(), muse.EventInterrupt) != 1 || count(events(), muse.EventSteer) != 1 {
		t.Fatal("missing steer or interrupt event")
	}
}

func TestRunStopsWhenFundsRunOut(t *testing.T) {
	ctx := ctxT(t)
	l := memledger.New()
	l.Deposit(ctx, "ws", 5*money.Cent, "pay") // less than the 20¢ first hold
	f := newFake(t, "testdata/msp-session.jsonl", recordedSession)
	res, err := muse.New(muse.Config{Ledger: l}).Run(ctx, muse.Request{Prompt: "p", Model: model.Spark,
		Budget: &muse.Budget{Key: "run-poor", Payers: []budget.AccountID{"ws"}}}, f, nil)
	if !errors.Is(err, muse.ErrBudget) || !errors.Is(err, budget.ErrInsufficient) {
		t.Fatalf("err = %v, want ErrBudget wrapping ErrInsufficient", err)
	}
	b, _ := l.Balance(ctx, "ws")
	if b.Total < 0 || b.Held != 0 || res.Stopped != muse.StopBudget {
		t.Fatalf("balance %+v, stopped %q", b, res.Stopped)
	}
	if res.Receipt.Charged+res.Receipt.Unbilled != res.Cost() {
		t.Fatalf("receipt %+v does not account for cost %v", res.Receipt, res.Cost())
	}
}

func TestRunRefusedWithoutFunds(t *testing.T) {
	l := memledger.New()
	f := newFake(t, "testdata/msp-session.jsonl", recordedSession)
	_, err := muse.New(muse.Config{Ledger: l}).Run(ctxT(t), muse.Request{Prompt: "p",
		Budget: &muse.Budget{Key: "k", Payers: []budget.AccountID{"ws:empty"}}}, f, nil)
	if !errors.Is(err, muse.ErrBudget) {
		t.Fatalf("err = %v, want ErrBudget", err)
	}
	if f.host != nil {
		t.Fatal("muse was started without money")
	}
}

func TestGuardDeniesPush(t *testing.T) {
	f := newFake(t, "testdata/msp-guard.jsonl", guardSession)
	f.turn = turnOf(t, f.rec)
	on, events := collect()
	res, err := muse.New(muse.Config{}).Run(ctxT(t), muse.Request{Prompt: "push", Model: model.Spark}, f, on)
	if err != nil {
		t.Fatal(err)
	}
	decides := f.host.Sent(msp.MethodApprovalDecide)
	if len(decides) != 1 {
		t.Fatalf("approval/decide sent %d times", len(decides))
	}
	var d msp.ApprovalDecideParams
	_ = json.Unmarshal(decides[0], &d)
	if d.ChoiceID != "abort" || d.Feedback == nil || !strings.Contains(*d.Feedback, "no-push") {
		t.Fatalf("decide = %+v", d)
	}
	if res.Denied != 1 {
		t.Fatalf("denied = %d", res.Denied)
	}
	for _, e := range events() {
		if e.Kind == muse.EventApproval && (e.Approval.Allow || e.Approval.Rule != "no-push" || !e.Approval.Applied) {
			t.Fatalf("approval event = %+v", e.Approval)
		}
	}
}

func turnOf(t *testing.T, rec msptest.Recording) string {
	for _, n := range rec {
		if n.Method == msp.NoteTurnStarted {
			s, _ := msp.Decode[msp.TurnStarted](n)
			return s.TurnID
		}
	}
	t.Fatal("no turn in recording")
	return ""
}

// scripted is a fake Muse that sends a fixed list of notifications.
type scripted struct {
	notes []func(*msptest.Host)
	host  *msptest.Host
}

func (s *scripted) Start(context.Context, muse.Command) (muse.Process, error) {
	h, conn := msptest.New()
	s.host = h
	ack := func(json.RawMessage) (any, error) { return msp.CommandAck{Status: "accepted"}, nil }
	h.Handle(msp.MethodSessionStart, func(json.RawMessage) (any, error) {
		return msp.SessionResult{Session: msp.Session{SessionID: "s"}}, nil
	})
	for _, m := range []string{msp.MethodSessionLeave, msp.MethodSessionSetModel, msp.MethodTurnInterrupt, msp.MethodTurnSteer, msp.MethodApprovalDecide} {
		h.Handle(m, ack)
	}
	h.Handle(msp.MethodTurnStart, func(json.RawMessage) (any, error) {
		go func() {
			for _, n := range s.notes {
				n(h)
				time.Sleep(5 * time.Millisecond)
			}
		}()
		return msp.TurnStartResult{TurnID: "t"}, nil
	})
	return &fakeProcess{ReadWriteCloser: conn, host: h}, nil
}

func tool(status, output string) func(*msptest.Host) {
	return func(h *msptest.Host) {
		_ = h.Notify(msp.NoteItemCompleted, msp.ItemEvent{SessionID: "s", Item: msp.Item{ItemID: "i", Kind: msp.ItemToolCall, Status: status, Tool: "bash", VisibleOutput: output}})
	}
}

func usage(id model.ID, in, out int64) func(*msptest.Host) {
	return func(h *msptest.Host) {
		m := string(id)
		_ = h.Notify(msp.NoteTokenUsage, msp.TokenUsage{SessionID: "s", TurnID: "t", ModelID: &m, Usage: msp.TokenCounts{InputTokens: in, OutputTokens: out}})
	}
}

func complete(h *msptest.Host) {
	_ = h.Notify(msp.NoteTurnCompleted, msp.TurnCompleted{SessionID: "s", TurnID: "t", Terminal: msp.TerminalCompleted})
}

func TestEscalatesStuckSession(t *testing.T) {
	s := &scripted{notes: []func(*msptest.Host){
		usage(model.Spark, 1000, 10), tool("failed", "exit 1"),
		usage(model.Spark, 1000, 10), tool("failed", "exit 1"),
		usage(model.Spark, 1000, 10), tool("failed", "exit 1"),
		usage(model.Wonder, 1000, 10), tool("completed", "ok"),
		complete,
	}}
	on, events := collect()
	res, err := muse.New(muse.Config{}).Run(ctxT(t), muse.Request{Prompt: "p"}, s, on)
	if err != nil {
		t.Fatal(err)
	}
	sm := s.host.Sent(msp.MethodSessionSetModel)
	if len(sm) != 1 || !strings.Contains(string(sm[0]), string(model.Wonder)) || !res.Escalated || res.Model != model.Wonder {
		t.Fatalf("setModel %s, result %+v", sm, res)
	}
	if res.Usage.ByModel[model.Wonder].Input != 1000 || res.Usage.ByModel[model.Spark].Input != 3000 {
		t.Fatalf("by model = %+v", res.Usage.ByModel)
	}
	if count(events(), muse.EventEscalate) != 1 {
		t.Fatal("no escalate event")
	}
}

func TestTurnErrors(t *testing.T) {
	for _, tc := range []struct {
		c    msp.TurnCompleted
		want func(error) bool
	}{
		{msp.TurnCompleted{Terminal: msp.TerminalFailed, Error: &msp.TurnError{Kind: "authRequired", Message: "sign in"}},
			func(err error) bool { return errors.Is(err, muse.ErrAuth) }},
		{msp.TurnCompleted{Terminal: msp.TerminalFailed, Error: &msp.TurnError{Kind: "modelError", Quota: &msp.Quota{LimitKind: "window", ResetsAtMs: 1_800_000_000_000}}},
			func(err error) bool {
				var rl *muse.RateLimitError
				return errors.As(err, &rl) && rl.Until.Equal(time.UnixMilli(1_800_000_000_000))
			}},
		{msp.TurnCompleted{Terminal: msp.TerminalFailed, Error: &msp.TurnError{Kind: "stepLimit"}},
			func(err error) bool { return errors.Is(err, muse.ErrStepLimit) }},
		{msp.TurnCompleted{Terminal: msp.TerminalFailed, Error: &msp.TurnError{Kind: "modelError", Message: "boom"}},
			func(err error) bool { var te *muse.TurnError; return errors.As(err, &te) && te.Kind == "modelError" }},
	} {
		c := tc.c
		c.SessionID, c.TurnID = "s", "t"
		s := &scripted{notes: []func(*msptest.Host){func(h *msptest.Host) { _ = h.Notify(msp.NoteTurnCompleted, c) }}}
		_, err := muse.New(muse.Config{}).Run(ctxT(t), muse.Request{Prompt: "p"}, s, nil)
		if !tc.want(err) {
			t.Errorf("%+v: err = %v", c.Error, err)
		}
	}
}

func TestProcessExitStillSettles(t *testing.T) {
	ctx := ctxT(t)
	l := memledger.New()
	l.Deposit(ctx, "ws", money.Dollar, "pay")
	s := &scripted{}
	s.notes = []func(*msptest.Host){usage(model.Spark, 100_000, 1000), func(h *msptest.Host) { _ = h.Close() }}
	res, err := muse.New(muse.Config{Ledger: l}).Run(ctx, muse.Request{Prompt: "p",
		Budget: &muse.Budget{Key: "k", Payers: []budget.AccountID{"ws"}}}, s, nil)
	if !errors.Is(err, muse.ErrExited) {
		t.Fatalf("err = %v, want ErrExited", err)
	}
	b, _ := l.Balance(ctx, "ws")
	if res.Receipt == nil || res.Receipt.Charged != res.Cost() || b.Held != 0 || b.Total != money.Dollar-res.Cost() {
		t.Fatalf("receipt %+v balance %+v cost %v", res.Receipt, b, res.Cost())
	}
}

func TestCancelInterrupts(t *testing.T) {
	ctx, cancel := context.WithCancel(ctxT(t))
	s := &scripted{}
	s.notes = []func(*msptest.Host){usage(model.Spark, 10, 1), func(*msptest.Host) { cancel() }, func(h *msptest.Host) {
		if _, err := h.WaitFor(context.Background(), msp.MethodTurnInterrupt); err == nil {
			_ = h.Notify(msp.NoteTurnCompleted, msp.TurnCompleted{SessionID: "s", TurnID: "t", Terminal: msp.TerminalCancelled})
		}
	}}
	res, err := muse.New(muse.Config{}).Run(ctx, muse.Request{Prompt: "p"}, s, nil)
	if !errors.Is(err, context.Canceled) || res.Stopped != muse.StopContext {
		t.Fatalf("err = %v, stopped %q", err, res.Stopped)
	}
}

func TestSecretsAreRedacted(t *testing.T) {
	key, _ := credential.APIKey("LLM_0123456789abcdefSECRET")
	s := &scripted{notes: []func(*msptest.Host){tool("completed", "META_API_KEY=LLM_0123456789abcdefSECRET"), complete}}
	on, events := collect()
	_, err := muse.New(muse.Config{}).Run(ctxT(t), muse.Request{Prompt: "p", Credential: &key}, s, on)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events() {
		if strings.Contains(e.Output, "SECRET") {
			t.Fatalf("secret leaked in %s event: %q", e.Kind, e.Output)
		}
	}
}

func TestCredentialReachesTheProcess(t *testing.T) {
	login, _ := credential.Login(credential.Token{Access: "access-token-123"}, "")
	f := newFake(t, "testdata/msp-session.jsonl", recordedSession)
	f.pause = 0
	_, err := muse.New(muse.Config{}).Run(ctxT(t), muse.Request{Prompt: "p", Credential: &login, AuthPath: "/home/agent/.config/muse/auth.json", Ephemeral: true}, f, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	cmd := f.cmd
	f.mu.Unlock()
	if strings.Join(cmd.Args, " ") != `serve --no-session-log -c run.workflow_trigger_mode="off"` || len(cmd.Files) != 1 || cmd.Files[0].Path != "/home/agent/.config/muse/auth.json" {
		t.Fatalf("command = %+v", cmd)
	}
	if !strings.Contains(strings.Join(cmd.Env, " "), "MUSE_AUTH_PATH=/home/agent/.config/muse/auth.json") {
		t.Fatalf("env = %v", cmd.Env)
	}
}

func TestInvalidRequests(t *testing.T) {
	e := muse.New(muse.Config{})
	for _, req := range []muse.Request{
		{},
		{Prompt: "p", Budget: &muse.Budget{Key: "k", Payers: []budget.AccountID{"a"}}}, // no ledger
	} {
		if _, err := e.Run(context.Background(), req, &scripted{}, nil); !errors.Is(err, muse.ErrInvalid) {
			t.Errorf("Run(%+v) = %v, want ErrInvalid", req, err)
		}
	}
}

// TestManyRunsShareOneBudget runs sessions concurrently against one small
// balance: none may push it below zero, and every cent is accounted for.
func TestManyRunsShareOneBudget(t *testing.T) {
	ctx := ctxT(t)
	l := memledger.New()
	l.Deposit(ctx, "ws", 10*money.Cent, "pay")
	eng := muse.New(muse.Config{Ledger: l, Step: 2 * money.Cent})
	var wg sync.WaitGroup
	var charged, unbilled atomic.Int64
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f := newFake(t, "testdata/msp-session.jsonl", recordedSession)
			f.pause = 5 * time.Millisecond
			res, _ := eng.Run(ctx, muse.Request{Prompt: "p", Model: model.Spark,
				Budget: &muse.Budget{Key: "run-" + string(rune('a'+i)), Payers: []budget.AccountID{"ws"}}}, f, nil)
			if res.Receipt != nil {
				charged.Add(int64(res.Receipt.Charged))
				unbilled.Add(int64(res.Receipt.Unbilled))
			}
		}()
	}
	wg.Wait()
	b, _ := l.Balance(ctx, "ws")
	if b.Total < 0 || b.Held != 0 || money.USD(charged.Load()) != 10*money.Cent-b.Total {
		t.Fatalf("balance %+v, charged %v", b, money.USD(charged.Load()))
	}
	t.Logf("8 runs on a 10¢ budget: charged %v, unbilled %v, left %v", money.USD(charged.Load()), money.USD(unbilled.Load()), b.Total)
}

func TestMaxCallsSteersThenStops(t *testing.T) {
	s := &scripted{}
	s.notes = []func(*msptest.Host){
		usage(model.Spark, 1000, 10), usage(model.Spark, 1000, 10), usage(model.Spark, 1000, 10),
		usage(model.Spark, 1000, 10), usage(model.Spark, 1000, 10), usage(model.Spark, 1000, 10),
		func(h *msptest.Host) {
			if _, err := h.WaitFor(context.Background(), msp.MethodTurnInterrupt); err == nil {
				_ = h.Notify(msp.NoteTurnCompleted, msp.TurnCompleted{SessionID: "s", TurnID: "t", Terminal: msp.TerminalCancelled})
			}
		},
	}
	on, events := collect()
	res, err := muse.New(muse.Config{}).Run(ctxT(t), muse.Request{Prompt: "p", MaxCalls: 5}, s, on)
	if !errors.Is(err, muse.ErrStepLimit) || res.Stopped != muse.StopSteps || !res.Steered {
		t.Fatalf("err = %v, result %+v", err, res)
	}
	var steer string
	for _, e := range events() {
		if e.Kind == muse.EventSteer {
			steer = e.Reason
		}
	}
	if steer != "steps" || len(s.host.Sent(msp.MethodTurnInterrupt)) != 1 || len(s.host.Sent(msp.MethodTurnSteer)) != 1 {
		t.Fatalf("steer %q, interrupts %d", steer, len(s.host.Sent(msp.MethodTurnInterrupt)))
	}
}

// TestNoBackgroundWorkflows keeps Muse from handing a run's work to a
// background workflow, which would report back only after the run ended;
// Config.Workflows lets a caller that waits for them turn it back on.
func TestNoBackgroundWorkflows(t *testing.T) {
	for _, tc := range []struct {
		cfg  muse.Config
		want bool
	}{{muse.Config{}, true}, {muse.Config{Workflows: true}, false}} {
		f := newFake(t, "testdata/msp-session.jsonl", recordedSession)
		f.pause = 0
		if _, err := muse.New(tc.cfg).Run(ctxT(t), muse.Request{Prompt: "p"}, f, nil); err != nil {
			t.Fatal(err)
		}
		f.mu.Lock()
		args := strings.Join(f.cmd.Args, " ")
		f.mu.Unlock()
		if got := strings.Contains(args, `run.workflow_trigger_mode="off"`); got != tc.want {
			t.Errorf("Workflows=%v: args %q", tc.cfg.Workflows, args)
		}
	}
}
