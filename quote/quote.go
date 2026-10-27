// Package quote asks Muse what a task will cost before it runs.
//
// A quote is a short, read-only Muse session: Muse may read the code but
// not change it (guard.ReadOnly), and answers with a JSON estimate of how
// many model calls the task needs and whether it is complex. Models judge
// effort far better than dollars, so the dollars come from Pricing, which
// a caller calibrates from its own history of runs.
package quote

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	muse "github.com/rustambekabdusalomov/agnts-muse"
	"github.com/rustambekabdusalomov/agnts-muse/guard"
	"github.com/rustambekabdusalomov/agnts-muse/model"
	"github.com/rustambekabdusalomov/agnts-muse/money"
	"github.com/rustambekabdusalomov/agnts-muse/route"
)

// Pricing turns model calls into money for one model: the first call of
// a session reads everything uncached, later calls mostly hit the cache.
type Pricing struct {
	First   money.USD
	PerCall money.USD
}

// Cost is the price of n calls.
func (p Pricing) Cost(n int) money.USD {
	if n <= 0 {
		return 0
	}
	return p.First + money.USD(n-1)*p.PerCall
}

// DefaultPricing is measured on recorded Muse Code sessions with about
// 25k tokens of context (Oct 2026 list prices).
var DefaultPricing = map[model.Tier]Pricing{
	model.TierSpark:  {First: 4 * money.Cent, PerCall: money.Cent},
	model.TierWonder: {First: 12 * money.Cent, PerCall: 3 * money.Cent},
}

// Quote is an estimate.
type Quote struct {
	Model      model.ID
	Complexity route.Complexity
	Calls      Range
	Low        money.USD
	Expected   money.USD
	High       money.USD
	Reasons    []string
	Files      []string
	// Cost is what asking for the quote cost.
	Cost money.USD
}

// Range is a low, expected and high count of model calls.
type Range struct{ Low, Expected, High int }

// Request is what to quote.
type Request struct {
	Task    string
	Workdir string
	// Model quotes on this model; "" lets the complexity decide (route).
	Model model.ID
	// Run carries the credential, budget and so on for the quote session
	// itself; Prompt, Workdir, Guard and Ephemeral are set here.
	Run     muse.Request
	Pricing map[model.Tier]Pricing
	Router  route.Policy
}

// ErrNoEstimate reports that Muse did not answer with an estimate.
var ErrNoEstimate = errors.New("quote: Muse gave no estimate")

const instructions = `You are estimating, not doing, a coding task. Read only what you need to judge its size; do not change anything.
Then answer with only this JSON object and nothing else:
{"complexity":"simple"|"complex","calls":{"low":N,"expected":N,"high":N},"files":["paths you expect to change"],"reasons":["one short line each"]}
"calls" is how many model calls (each read, edit, command or reply) a careful engineer-agent would need, including running the tests.
"complex" means design decisions, several modules, or unclear requirements.

Task:
`

// Estimate runs a read-only Muse session that sizes the task.
func Estimate(ctx context.Context, eng *muse.Engine, ex muse.Executor, req Request) (Quote, error) {
	run := req.Run
	run.Prompt = instructions + req.Task
	run.Workdir = req.Workdir
	run.Guard = guard.Chain{guard.Defaults(), guard.ReadOnly}
	run.Ephemeral = true
	if run.Model == "" {
		run.Model = model.Spark
	}
	if run.Effort == "" {
		run.Effort = "low"
	}
	res, err := eng.Run(ctx, run, ex, nil)
	q := Quote{Cost: res.Cost()}
	if err != nil {
		return q, fmt.Errorf("quote: %w", err)
	}
	est, err := parse(res.Text)
	if err != nil {
		return q, err
	}
	router := req.Router
	if router == nil {
		router = route.Default()
	}
	q.Complexity = route.Simple
	if est.Complexity == "complex" {
		q.Complexity = route.Complex
	}
	q.Model = req.Model
	if q.Model == "" {
		q.Model = router.Initial(q.Complexity)
	}
	q.Calls = Range{Low: est.Calls.Low, Expected: est.Calls.Expected, High: est.Calls.High}
	q.Calls.Low = max(q.Calls.Low, 1)
	q.Calls.Expected = max(q.Calls.Expected, q.Calls.Low)
	q.Calls.High = max(q.Calls.High, q.Calls.Expected)
	pricing := req.Pricing
	if pricing == nil {
		pricing = DefaultPricing
	}
	tier := model.TierWonder
	if m, ok := model.Default.Lookup(q.Model); ok {
		tier = m.Tier
	}
	p := pricing[tier]
	q.Low, q.Expected, q.High = p.Cost(q.Calls.Low), p.Cost(q.Calls.Expected), p.Cost(q.Calls.High)
	q.Reasons, q.Files = est.Reasons, est.Files
	return q, nil
}

type estimate struct {
	Complexity string `json:"complexity"`
	Calls      struct {
		Low      int `json:"low"`
		Expected int `json:"expected"`
		High     int `json:"high"`
	} `json:"calls"`
	Files   []string `json:"files"`
	Reasons []string `json:"reasons"`
}

// parse finds the JSON object in Muse's answer (which may be fenced or
// surrounded by a sentence despite the instructions).
func parse(text string) (estimate, error) {
	var e estimate
	start, end := strings.Index(text, "{"), strings.LastIndex(text, "}")
	if start < 0 || end <= start {
		return e, fmt.Errorf("%w: %q", ErrNoEstimate, truncate(text, 200))
	}
	if err := json.Unmarshal([]byte(text[start:end+1]), &e); err != nil {
		return e, fmt.Errorf("%w: %w", ErrNoEstimate, err)
	}
	if e.Calls.Expected <= 0 && e.Calls.High <= 0 {
		return e, fmt.Errorf("%w: no call counts", ErrNoEstimate)
	}
	return e, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
