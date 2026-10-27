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
	return Charge{Model: id, Usage: u, Cost: m.total - prev, Total: m.total, Calls: m.calls, Estimated: m.estimated}
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
