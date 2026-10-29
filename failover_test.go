package muse_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	muse "github.com/rustambekabdusalomov/agnts-muse"
	"github.com/rustambekabdusalomov/agnts-muse/budget"
	"github.com/rustambekabdusalomov/agnts-muse/budget/memledger"
	"github.com/rustambekabdusalomov/agnts-muse/credential"
	"github.com/rustambekabdusalomov/agnts-muse/model"
	"github.com/rustambekabdusalomov/agnts-muse/money"
	"github.com/rustambekabdusalomov/agnts-muse/msp"
	"github.com/rustambekabdusalomov/agnts-muse/msp/msptest"
)

const hostedKey = "b10_test.hostedKeyForTests123"

// attempts is an executor whose every Start is the next scripted process;
// it records each command and session/resume.
type attempts struct {
	mu      sync.Mutex
	scripts []*scripted
	cmds    []muse.Command
	resumed []msp.SessionResumeParams
}

func (a *attempts) Start(ctx context.Context, cmd muse.Command) (muse.Process, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.cmds) == len(a.scripts) {
		return nil, errors.New("no more scripted processes")
	}
	s := a.scripts[len(a.cmds)]
	a.cmds = append(a.cmds, cmd)
	p, err := s.Start(ctx, cmd)
	s.host.Handle(msp.MethodSessionResume, func(raw json.RawMessage) (any, error) {
		var r msp.SessionResumeParams
		_ = json.Unmarshal(raw, &r)
		a.mu.Lock()
		a.resumed = append(a.resumed, r)
		a.mu.Unlock()
		return msp.SessionResult{Session: msp.Session{SessionID: r.SessionID}}, nil
	})
	return p, err
}

func (a *attempts) started() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.cmds)
}

func quotaOut(resetsAt time.Time) func(*msptest.Host) {
	return func(h *msptest.Host) {
		_ = h.Notify(msp.NoteTurnCompleted, msp.TurnCompleted{SessionID: "s", TurnID: "t", Terminal: msp.TerminalFailed,
			Error: &msp.TurnError{Kind: "modelError", Quota: &msp.Quota{LimitKind: "window", ResetsAtMs: resetsAt.UnixMilli()}}})
	}
}

func failed(kind, msg string) func(*msptest.Host) {
	return func(h *msptest.Host) {
		_ = h.Notify(msp.NoteTurnCompleted, msp.TurnCompleted{SessionID: "s", TurnID: "t", Terminal: msp.TerminalFailed,
			Error: &msp.TurnError{Kind: kind, Message: msg}})
	}
}

func baseten(t *testing.T) muse.Provider {
	t.Helper()
	c, err := credential.Hosted("https://inference.baseten.co/v1/", hostedKey, "")
	if err != nil {
		t.Fatal(err)
	}
	return muse.Provider{Name: "baseten", Credential: c, Model: model.SparkOpen}
}

func TestFailoverContinuesTheSessionOnTheFallback(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	resets := now.Add(3 * time.Hour)
	ex := &attempts{scripts: []*scripted{
		{notes: []func(*msptest.Host){usage(model.Spark, 100_000, 1000), quotaOut(resets)}},
		{notes: []func(*msptest.Host){usage(model.SparkOpen, 100_000, 1000), complete}},
	}}
	l := memledger.New()
	if _, err := l.Deposit(context.Background(), "user:1", money.Dollar, "test"); err != nil {
		t.Fatal(err)
	}
	f := &muse.Failover{Engine: muse.New(muse.Config{Ledger: l}), Fallback: baseten(t), Now: func() time.Time { return now }}
	on, events := collect()
	primaryKey, _ := credential.APIKey("LLM_primary-key-0123456789")
	res, err := f.Run(ctxT(t), muse.Request{Prompt: "fix the bug", Credential: &primaryKey,
		Budget: &muse.Budget{Key: "run-1", Payers: []budget.AccountID{"user:1"}, Limit: 50 * money.Cent}}, ex, on)
	if err != nil {
		t.Fatal(err)
	}

	// The second process went to Baseten, with its key and the opt-in.
	if ex.started() != 2 {
		t.Fatalf("started %d processes, want 2", ex.started())
	}
	args, env := strings.Join(ex.cmds[1].Args, " "), strings.Join(ex.cmds[1].Env, " ")
	if !strings.Contains(args, `endpoint_transport={"base_url":"https://inference.baseten.co/v1","auth":"bearer"}`) ||
		!strings.Contains(env, "META_API_KEY="+hostedKey) || strings.Contains(strings.Join(ex.cmds[0].Args, " "), "endpoint_transport") {
		t.Fatalf("commands = %+v", ex.cmds)
	}
	// It resumed the primary's session and was told to continue.
	if len(ex.resumed) != 1 || ex.resumed[0].SessionID != "s" {
		t.Fatalf("resumed = %+v", ex.resumed)
	}
	if sent := ex.scripts[1].host.Sent(msp.MethodTurnStart); len(sent) != 1 || !strings.Contains(string(sent[0]), "[Continue]") ||
		!strings.Contains(string(sent[0]), "fix the bug") {
		t.Fatalf("turn/start = %s", sent)
	}
	if sm := ex.scripts[1].host.Sent(msp.MethodSessionSetModel); len(sm) != 1 || !strings.Contains(string(sm[0]), string(model.SparkOpen)) {
		t.Fatalf("setModel = %s", sm)
	}

	// The result covers both attempts, and each was charged under its own key.
	if res.Fallback != "baseten" || res.Primary == nil || res.Model != model.SparkOpen || res.Usage.Calls != 2 ||
		res.Usage.Total != res.Primary.Usage.Total*2 {
		t.Fatalf("result = %+v", res)
	}
	if res.Primary.Receipt == nil || res.Receipt == nil || res.Receipt.Key != "run-1:baseten" ||
		res.Primary.Receipt.Charged+res.Receipt.Charged != res.Usage.Total {
		t.Fatalf("receipts = %+v / %+v", res.Primary.Receipt, res.Receipt)
	}
	bal, _ := l.Balance(context.Background(), "user:1")
	if bal.Total != money.Dollar-res.Usage.Total {
		t.Fatalf("balance = %v, want %v", bal.Total, money.Dollar-res.Usage.Total)
	}
	if count(events(), muse.EventFailover) != 1 {
		t.Fatal("no failover event")
	}
	for _, e := range events() {
		if strings.Contains(e.Reason+e.Text, hostedKey) {
			t.Fatalf("event leaks the key: %+v", e)
		}
	}

	// The primary is skipped until its allowance resets.
	if until, _ := f.OutUntil(); !until.Equal(resets) {
		t.Fatalf("OutUntil = %v, want %v", until, resets)
	}
	ex.scripts = append(ex.scripts, &scripted{notes: []func(*msptest.Host){complete}})
	if res, err := f.Run(ctxT(t), muse.Request{Prompt: "next"}, ex, nil); err != nil || res.Fallback != "baseten" || res.Primary != nil {
		t.Fatalf("second run: %+v, %v", res, err)
	}
	if ex.started() != 3 {
		t.Fatalf("second run started %d processes, want 1", ex.started()-2)
	}
	now = resets
	if until, _ := f.OutUntil(); !until.IsZero() {
		t.Fatal("primary still out after its reset")
	}
}

func TestFailoverModes(t *testing.T) {
	for _, tc := range []struct {
		mode    muse.Mode
		scripts []*scripted
		started int
		want    string // the result's Fallback
		wantErr bool
	}{
		// Primary only: the quota error is the run's.
		{muse.ModePrimary, []*scripted{{notes: []func(*msptest.Host){quotaOut(time.Now().Add(time.Hour))}}}, 1, "", true},
		// Fallback only: never touches the primary.
		{muse.ModeFallback, []*scripted{{notes: []func(*msptest.Host){complete}}}, 1, "baseten", false},
	} {
		ex := &attempts{scripts: tc.scripts}
		f := &muse.Failover{Engine: muse.New(muse.Config{}), Fallback: baseten(t), Mode: func() muse.Mode { return tc.mode }}
		res, err := f.Run(ctxT(t), muse.Request{Prompt: "p"}, ex, nil)
		if (err != nil) != tc.wantErr || res.Fallback != tc.want || ex.started() != tc.started {
			t.Errorf("%s: result %+v, err %v, started %d", tc.mode, res, err, ex.started())
		}
	}
}

func TestFailoverKeepsOtherErrors(t *testing.T) {
	ex := &attempts{scripts: []*scripted{{notes: []func(*msptest.Host){failed("modelError", "upstream overloaded")}}}}
	f := &muse.Failover{Engine: muse.New(muse.Config{}), Fallback: baseten(t)}
	_, err := f.Run(ctxT(t), muse.Request{Prompt: "p"}, ex, nil)
	var te *muse.TurnError
	if !errors.As(err, &te) || ex.started() != 1 {
		t.Fatalf("err = %v, started %d", err, ex.started())
	}
	if until, _ := f.OutUntil(); !until.IsZero() {
		t.Fatal("an overload marked the primary out")
	}
}

func TestFailoverCooldownWithoutResetTime(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	ex := &attempts{scripts: []*scripted{
		{notes: []func(*msptest.Host){failed("modelError", "billing_error: credit balance is too low")}},
		{notes: []func(*msptest.Host){complete}},
	}}
	f := &muse.Failover{Engine: muse.New(muse.Config{}), Fallback: baseten(t), Cooldown: time.Minute, Now: func() time.Time { return now }}
	if _, err := f.Run(ctxT(t), muse.Request{Prompt: "p", Ephemeral: true}, ex, nil); err != nil {
		t.Fatal(err)
	}
	if until, why := f.OutUntil(); !until.Equal(now.Add(time.Minute)) || !strings.Contains(why, "billing") {
		t.Fatalf("OutUntil = %v %q", until, why)
	}
	// An ephemeral session cannot be resumed: the fallback starts afresh.
	if len(ex.resumed) != 0 || !strings.Contains(string(ex.scripts[1].host.Sent(msp.MethodTurnStart)[0]), `"p"`) {
		t.Fatalf("resumed %+v", ex.resumed)
	}
	f.Reset()
	if until, _ := f.OutUntil(); !until.IsZero() {
		t.Fatal("Reset kept the primary out")
	}
}

func TestFailoverStopsAtTheRunLimit(t *testing.T) {
	ex := &attempts{scripts: []*scripted{{notes: []func(*msptest.Host){usage(model.Spark, 4_000_000, 0), quotaOut(time.Now().Add(time.Hour))}}}}
	l := memledger.New()
	_, _ = l.Deposit(context.Background(), "user:1", 10*money.Dollar, "test")
	f := &muse.Failover{Engine: muse.New(muse.Config{Ledger: l}), Fallback: baseten(t)}
	// 4M input tokens on Spark cost $5, all of the run's limit.
	res, err := f.Run(ctxT(t), muse.Request{Prompt: "p", Budget: &muse.Budget{Key: "k", Payers: []budget.AccountID{"user:1"}, Limit: 5 * money.Dollar}}, ex, nil)
	if err == nil || res.Fallback != "" || ex.started() != 1 {
		t.Fatalf("result %+v, err %v, started %d", res, err, ex.started())
	}
}

func TestProviderOut(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{&muse.RateLimitError{Kind: "window"}, true},
		{errors.Join(muse.ErrAuth, errors.New("key revoked")), true},
		{&muse.TurnError{Kind: "modelError", Message: "billing_error: insufficient credit"}, true},
		{&muse.TurnError{Kind: "modelError", Message: "overloaded"}, false},
		{&muse.BudgetError{Err: muse.ErrBudget}, false},
		{muse.ErrStepLimit, false},
		{context.Canceled, false},
	} {
		if got := muse.ProviderOut(tc.err); got != tc.want {
			t.Errorf("ProviderOut(%v) = %v", tc.err, got)
		}
	}
}

func TestParseMode(t *testing.T) {
	for in, want := range map[string]muse.Mode{"": muse.ModeAuto, "Fallback": muse.ModeFallback, " primary ": muse.ModePrimary} {
		if m, err := muse.ParseMode(in); err != nil || m != want {
			t.Errorf("ParseMode(%q) = %q, %v", in, m, err)
		}
	}
	if _, err := muse.ParseMode("both"); !errors.Is(err, muse.ErrInvalid) {
		t.Errorf("ParseMode(both) err = %v", err)
	}
}

// TestFallbackCapsCountOnlyTheFallback gives a person a Baseten allowance
// of 5¢: the 12.5¢ the run spent on the primary does not count against it,
// the 1.25¢ it spent on the fallback does.
func TestFallbackCapsCountOnlyTheFallback(t *testing.T) {
	ctx := context.Background()
	l := memledger.New()
	_, _ = l.Deposit(ctx, "user:1", money.Dollar, "test")
	_, _ = l.Deposit(ctx, "fcap:user:1", 5*money.Cent, "allowance")
	ex := &attempts{scripts: []*scripted{
		{notes: []func(*msptest.Host){usage(model.Spark, 100_000, 0), quotaOut(time.Now().Add(time.Hour))}},
		{notes: []func(*msptest.Host){usage(model.SparkOpen, 10_000, 0), complete}},
	}}
	f := &muse.Failover{Engine: muse.New(muse.Config{Ledger: l}), Fallback: baseten(t)}
	res, err := f.Run(ctxT(t), muse.Request{Prompt: "p", AnnounceBudget: true, Budget: &muse.Budget{Key: "k", Payers: []budget.AccountID{"user:1"},
		Limit: 50 * money.Cent, FallbackCaps: []budget.AccountID{"fcap:user:1"}, FallbackLimit: 3 * money.Cent}}, ex, nil)
	if err != nil || res.Fallback != "baseten" {
		t.Fatalf("result %+v, err %v", res, err)
	}
	onFallback := res.Usage.Total - res.Primary.Usage.Total
	if bal, _ := l.Balance(ctx, "fcap:user:1"); bal.Total != 5*money.Cent-onFallback {
		t.Fatalf("allowance left %v, want %v", bal.Total, 5*money.Cent-onFallback)
	}
	// The fallback's limit is the smaller of what is left (37.5¢) and
	// FallbackLimit (3¢); Muse is told that one.
	if sent := ex.scripts[1].host.Sent(msp.MethodTurnStart); !strings.Contains(string(sent[0]), "hard budget of $0.03") {
		t.Fatalf("turn/start = %s", sent)
	}
}

func TestNoFallbackBudget(t *testing.T) {
	l := memledger.New()
	_, _ = l.Deposit(context.Background(), "user:1", money.Dollar, "test")
	b := func() *muse.Budget {
		return &muse.Budget{Key: "k", Payers: []budget.AccountID{"user:1"}, FallbackLimit: -1}
	}
	// Auto: the primary's quota error stays the run's, and says why it did not move.
	ex := &attempts{scripts: []*scripted{{notes: []func(*msptest.Host){quotaOut(time.Now().Add(time.Hour))}}}}
	f := &muse.Failover{Engine: muse.New(muse.Config{Ledger: l}), Fallback: baseten(t)}
	_, err := f.Run(ctxT(t), muse.Request{Prompt: "p", Budget: b()}, ex, nil)
	var rl *muse.RateLimitError
	if !errors.Is(err, muse.ErrFallbackLimit) || !errors.As(err, &rl) || ex.started() != 1 {
		t.Fatalf("auto: err %v, started %d", err, ex.started())
	}
	// Fallback only: nothing starts.
	ex = &attempts{}
	f.Mode = func() muse.Mode { return muse.ModeFallback }
	if _, err := f.Run(ctxT(t), muse.Request{Prompt: "p", Budget: b()}, ex, nil); !errors.Is(err, muse.ErrFallbackLimit) || ex.started() != 0 {
		t.Fatalf("fallback: err %v, started %d", err, ex.started())
	}
}
