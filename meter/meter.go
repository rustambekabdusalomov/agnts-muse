// Package meter turns a stream of per-call token usage into charges.
//
// A Meter keeps the exact running cost (picodollars) and reports each call
// as the change in its rounded-up total, so the charges of a run always
// add up to exactly the rounded total: no call's rounding is lost or paid
// twice, however many calls there are.
package meter

import (
	"sync"

	"github.com/rustambekabdusalomov/agnts-muse/model"
	"github.com/rustambekabdusalomov/agnts-muse/money"
)

// Charge is what one model call added.
type Charge struct {
	Model model.ID
	Usage model.Usage
	// Cost is this call's share of the rounded total; the Costs of every
	// Charge of a Meter sum to Total.
	Cost  money.USD
	Total money.USD
	Calls int
	// Estimated is true when any call so far was priced without a catalog
	// entry (see model.Catalog.Cost).
	Estimated bool
}

// Meter accumulates cost. It is safe for concurrent use.
type Meter struct {
	cat *model.Catalog

	mu        sync.Mutex
	exact     money.Exact
	total     money.USD
	usage     model.Usage
	byModel   map[model.ID]model.Usage
	calls     int
	estimated bool
	last      model.Usage // the latest call, for Forecast
	prevInput int64       // input of the call before the latest
}

// New returns a Meter pricing with cat (model.Default when nil).
func New(cat *model.Catalog) *Meter {
	if cat == nil {
		cat = model.Default
	}
	return &Meter{cat: cat, byModel: map[model.ID]model.Usage{}}
}

// Add records one model call.
func (m *Meter) Add(id model.ID, u model.Usage) Charge {
	cost, est := m.cat.Cost(id, u)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.exact += cost
	prev := m.total
	m.total = m.exact.Ceil()
	m.usage = m.usage.Add(u)
	m.byModel[id] = m.byModel[id].Add(u)
	m.calls++
	m.estimated = m.estimated || est
	m.prevInput, m.last = m.last.Input, u
	return Charge{Model: id, Usage: u, Cost: m.total - prev, Total: m.total, Calls: m.calls, Estimated: m.estimated}
}

// Forecast estimates the next model call on id, for deciding whether a run
// can afford it. An agent's calls resend the whole conversation, and Muse
// caches it: the next call reads the last call's input from cache, adds
// what the conversation grew by since (tool output and the reply), and
// writes about as much as calls have on average. Growth is the last
// increase in input, doubled for margin; before there are two calls it is
// estimated from the reply. Zero before the first call.
func (m *Meter) Forecast(id model.ID) money.USD {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.calls == 0 {
		return 0
	}
	growth := 2 * m.last.Output
	if m.prevInput > 0 {
		growth = 2 * max(m.last.Input-m.prevInput, 0)
	}
	next := model.Usage{Input: m.last.Input + growth, Cached: m.last.Input, Output: m.usage.Output / int64(m.calls)}
	cost, _ := m.cat.Cost(id, next)
	return cost.Ceil()
}

// Total is the rounded-up cost so far.
func (m *Meter) Total() money.USD {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.total
}

// Snapshot is a Meter's state at one moment.
type Snapshot struct {
	Total     money.USD
	Usage     model.Usage
	ByModel   map[model.ID]model.Usage
	Calls     int
	Estimated bool
}

// Snapshot returns the current totals.
func (m *Meter) Snapshot() Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	by := make(map[model.ID]model.Usage, len(m.byModel))
	for k, v := range m.byModel {
		by[k] = v
	}
	return Snapshot{Total: m.total, Usage: m.usage, ByModel: by, Calls: m.calls, Estimated: m.estimated}
}
