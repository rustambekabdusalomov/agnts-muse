package quote

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	muse "github.com/rustambekabdusalomov/agnts-muse"
	"github.com/rustambekabdusalomov/agnts-muse/model"
	"github.com/rustambekabdusalomov/agnts-muse/money"
	"github.com/rustambekabdusalomov/agnts-muse/msp"
	"github.com/rustambekabdusalomov/agnts-muse/msp/msptest"
	"github.com/rustambekabdusalomov/agnts-muse/route"
)

func TestParse(t *testing.T) {
	for _, tc := range []struct {
		text string
		ok   bool
	}{
		{`{"complexity":"simple","calls":{"low":3,"expected":5,"high":9},"files":["a.go"],"reasons":["one file"]}`, true},
		{"Here is my estimate:\n```json\n{\"complexity\":\"complex\",\"calls\":{\"expected\":20}}\n```", true},
		{"I cannot tell.", false},
		{`{"complexity":"simple"}`, false},
		{`{"calls": nope}`, false},
	} {
		_, err := parse(tc.text)
		if (err == nil) != tc.ok || err != nil && !errors.Is(err, ErrNoEstimate) {
			t.Errorf("parse(%q) = %v", tc.text, err)
		}
	}
}

func TestPricing(t *testing.T) {
	p := DefaultPricing[model.TierSpark]
	if p.Cost(0) != 0 || p.Cost(1) != 4*money.Cent || p.Cost(5) != 8*money.Cent {
		t.Fatalf("costs %v %v %v", p.Cost(0), p.Cost(1), p.Cost(5))
	}
}

type fake struct{ answer string }

func (f fake) Start(context.Context, muse.Command) (muse.Process, error) {
	h, conn := msptest.New()
	h.Handle(msp.MethodSessionStart, func(json.RawMessage) (any, error) {
		return msp.SessionResult{Session: msp.Session{SessionID: "s"}}, nil
	})
	h.Handle(msp.MethodSessionLeave, func(json.RawMessage) (any, error) { return msp.CommandAck{}, nil })
	h.Handle(msp.MethodTurnStart, func(p json.RawMessage) (any, error) {
		var tp msp.TurnStartParams
		_ = json.Unmarshal(p, &tp)
		go func() {
			if !strings.Contains(tp.Input[0].Text, "estimating, not doing") || tp.ReasoningEffort != "low" {
				_ = h.Notify(msp.NoteTurnCompleted, msp.TurnCompleted{SessionID: "s", TurnID: "t", Terminal: msp.TerminalFailed,
					Error: &msp.TurnError{Kind: "configError", Message: "wrong quote prompt"}})
				return
			}
			spark := string(model.Spark)
			_ = h.Notify(msp.NoteTokenUsage, msp.TokenUsage{SessionID: "s", TurnID: "t", ModelID: &spark, Usage: msp.TokenCounts{InputTokens: 20000, OutputTokens: 200}})
			_ = h.Notify(msp.NoteItemCompleted, msp.ItemEvent{SessionID: "s", Item: msp.Item{Kind: msp.ItemAgentMessage, Status: "completed", Text: f.answer, MessagePhase: "finalAnswer"}})
			_ = h.Notify(msp.NoteTurnCompleted, msp.TurnCompleted{SessionID: "s", TurnID: "t", Terminal: msp.TerminalCompleted})
		}()
		return msp.TurnStartResult{TurnID: "t"}, nil
	})
	return proc{conn, h}, nil
}

type proc struct {
	io.ReadWriteCloser
	h *msptest.Host
}

func (p proc) Wait() error { return nil }
func (p proc) Kill() error { return p.h.Close() }

func TestEstimate(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ex := fake{answer: `{"complexity":"complex","calls":{"low":10,"expected":18,"high":30},"files":["billing/payme.go"],"reasons":["touches payments"]}`}
	q, err := Estimate(ctx, muse.New(muse.Config{}), ex, Request{Task: "Add refunds to Payme", Workdir: "/w"})
	if err != nil {
		t.Fatal(err)
	}
	wonder := DefaultPricing[model.TierWonder]
	if q.Complexity != route.Complex || q.Model != model.Wonder || q.Expected != wonder.Cost(18) || q.High != wonder.Cost(30) {
		t.Fatalf("quote = %+v", q)
	}
	if q.Cost <= 0 || len(q.Files) != 1 {
		t.Fatalf("quote cost %v files %v", q.Cost, q.Files)
	}
	if _, err := Estimate(ctx, muse.New(muse.Config{}), fake{answer: "no idea"}, Request{Task: "x"}); !errors.Is(err, ErrNoEstimate) {
		t.Fatalf("err = %v", err)
	}
}
