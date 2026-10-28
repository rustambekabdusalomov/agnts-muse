package muse_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	muse "github.com/rustambekabdusalomov/agnts-muse"
	"github.com/rustambekabdusalomov/agnts-muse/guard"
	"github.com/rustambekabdusalomov/agnts-muse/model"
	"github.com/rustambekabdusalomov/agnts-muse/msp"
	"github.com/rustambekabdusalomov/agnts-muse/msp/msptest"
)

// A person following a run talks to it (Request.Steer) and decides the tool
// calls the guard leaves to them (Request.Ask).

// waitSent waits until method was sent n times (scripted notes run on the
// fake's side, so they wait for the engine's reaction).
func waitSent(h *msptest.Host, method string, n int) {
	for i := 0; i < 200 && len(h.Sent(method)) < n; i++ {
		time.Sleep(5 * time.Millisecond)
	}
}

func approval(id, command string) func(*msptest.Host) {
	return func(h *msptest.Host) {
		_ = h.Notify(msp.NoteApprovalRequested, msp.ApprovalRequested{SessionID: "s", ApprovalID: id, TurnID: "t", ToolName: "bash",
			Subject: msp.ApprovalSubject{Kind: "shell", Command: command},
			AvailableChoices: []msp.ApprovalChoice{
				{ChoiceID: "once", Decision: msp.DecisionApproved, Scope: "once"},
				{ChoiceID: "deny", Decision: msp.DecisionDenied, AcceptsFeedback: true},
			},
			CurrentRequirementID: msp.RequirementRef{ApprovalID: id}})
	}
}

func decisions(t *testing.T, h *msptest.Host) []msp.ApprovalDecideParams {
	t.Helper()
	sent := h.Sent(msp.MethodApprovalDecide)
	out := make([]msp.ApprovalDecideParams, 0, len(sent))
	for _, raw := range sent {
		var d msp.ApprovalDecideParams
		if err := json.Unmarshal(raw, &d); err != nil {
			t.Fatal(err)
		}
		out = append(out, d)
	}
	return out
}

func steers(t *testing.T, h *msptest.Host) []string {
	t.Helper()
	sent := h.Sent(msp.MethodTurnSteer)
	out := make([]string, 0, len(sent))
	for _, raw := range sent {
		var p msp.TurnSteerParams
		if err := json.Unmarshal(raw, &p); err != nil {
			t.Fatal(err)
		}
		out = append(out, p.Input[0].Text)
	}
	return out
}

func asking() guard.Policy { return guard.Chain{guard.Defaults(), guard.AskBeforeInstalls()} }

func TestPersonSteersTheTurn(t *testing.T) {
	s := &scripted{}
	s.notes = []func(*msptest.Host){
		usage(model.Spark, 1000, 100),
		func(h *msptest.Host) { waitSent(h, msp.MethodTurnSteer, 1) },
		complete,
	}
	msgs := make(chan string, 1)
	msgs <- "show the seconds too"
	on, events := collect()
	res, err := muse.New(muse.Config{}).Run(ctxT(t), muse.Request{Prompt: "timer", Model: model.Spark, Steer: msgs}, s, on)
	if err != nil {
		t.Fatal(err)
	}
	if got := steers(t, s.host); len(got) != 1 || !strings.Contains(got[0], "show the seconds too") {
		t.Fatalf("steers = %q", got)
	}
	if res.Steered {
		t.Fatal("a person's message counted as a budget steer")
	}
	found := false
	for _, e := range events() {
		found = found || e.Kind == muse.EventSteer && e.Reason == "person"
	}
	if !found {
		t.Fatal("no steer event for the person's message")
	}
}

func TestPersonAllowsAHeldCall(t *testing.T) {
	asked := make(chan muse.Approval, 1)
	answer := make(chan bool, 1)
	s := &scripted{}
	s.notes = []func(*msptest.Host){
		approval("a1", "npm install left-pad"),
		func(h *msptest.Host) {
			<-asked
			answer <- true
			waitSent(h, msp.MethodTurnSteer, 1) // told it may go on
		},
		approval("a2", "npm install left-pad"), // Muse runs it again
		func(h *msptest.Host) { waitSent(h, msp.MethodApprovalDecide, 2) },
		complete,
	}
	on, events := collect()
	res, err := muse.New(muse.Config{Guard: asking()}).Run(ctxT(t), muse.Request{Prompt: "add a dependency", Model: model.Spark,
		Ask: func(a muse.Approval) <-chan bool { asked <- a; return answer }}, s, on)
	if err != nil {
		t.Fatal(err)
	}
	d := decisions(t, s.host)
	if len(d) != 2 || d[0].ChoiceID != "deny" || d[0].Feedback == nil || !strings.Contains(*d[0].Feedback, "they have been asked") || d[1].ChoiceID != "once" {
		t.Fatalf("decisions = %+v", d)
	}
	if got := steers(t, s.host); len(got) != 1 || !strings.Contains(got[0], "The person allowed: npm install left-pad") {
		t.Fatalf("steers = %q", got)
	}
	var seen []string
	for _, e := range events() {
		if e.Kind == muse.EventApproval {
			a := e.Approval
			seen = append(seen, map[bool]string{true: "pending", false: map[bool]string{true: "allowed", false: "denied"}[a.Allow]}[a.Pending])
			if a.Rule != "ask-new-dependency" || !a.Ask || a.Command != "npm install left-pad" {
				t.Fatalf("approval = %+v", a)
			}
		}
	}
	if strings.Join(seen, " ") != "pending allowed allowed" || res.Denied != 0 {
		t.Fatalf("approvals = %v, denied %d", seen, res.Denied)
	}
}

func TestNobodyAnswersDenies(t *testing.T) {
	s := &scripted{}
	s.notes = []func(*msptest.Host){
		approval("a1", "pip install requests"),
		func(h *msptest.Host) { waitSent(h, msp.MethodTurnSteer, 1) },
		complete,
	}
	res, err := muse.New(muse.Config{Guard: asking(), AskTimeout: 30 * time.Millisecond}).Run(ctxT(t),
		muse.Request{Prompt: "x", Model: model.Spark, Ask: func(muse.Approval) <-chan bool { return make(chan bool) }}, s, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := steers(t, s.host); len(got) != 1 || !strings.Contains(got[0], "Nobody answered in time") || res.Denied != 1 {
		t.Fatalf("steers = %q, denied %d", got, res.Denied)
	}
}

func TestAskWithoutAnyoneDenies(t *testing.T) {
	s := &scripted{}
	s.notes = []func(*msptest.Host){
		approval("a1", "cargo add serde"),
		func(h *msptest.Host) { waitSent(h, msp.MethodApprovalDecide, 1) },
		complete,
	}
	res, err := muse.New(muse.Config{Guard: asking()}).Run(ctxT(t), muse.Request{Prompt: "x", Model: model.Spark}, s, nil)
	if err != nil {
		t.Fatal(err)
	}
	d := decisions(t, s.host)
	if len(d) != 1 || d[0].ChoiceID != "deny" || !strings.Contains(*d[0].Feedback, "nobody can be asked") || res.Denied != 1 {
		t.Fatalf("decisions = %+v, denied %d", d, res.Denied)
	}
}
