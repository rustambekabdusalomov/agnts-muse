package meter

import (
	"sync"
	"testing"

	"github.com/rustambekabdusalomov/agnts-muse/model"
	"github.com/rustambekabdusalomov/agnts-muse/money"
)

// The five model calls of the recorded session (testdata/msp-session.jsonl).
var recorded = []model.Usage{
	{Input: 25426, Cached: 0, Output: 152, Reasoning: 65},
	{Input: 25693, Cached: 25329, Output: 119, Reasoning: 27},
	{Input: 26011, Cached: 25585, Output: 166, Reasoning: 113},
	{Input: 26224, Cached: 25969, Output: 92, Reasoning: 4},
	{Input: 26372, Cached: 26097, Output: 29, Reasoning: 8},
}

func TestChargesSumToTotal(t *testing.T) {
	m := New(nil)
	var sum money.USD
	var last Charge
	for _, u := range recorded {
		last = m.Add(model.Spark, u)
		sum += last.Cost
	}
	if sum != last.Total || last.Calls != 5 {
		t.Fatalf("sum of charges %v != total %v (calls %d)", sum, last.Total, last.Calls)
	}
	var exact money.Exact
	for _, u := range recorded {
		c, _ := model.Default.Cost(model.Spark, u)
		exact += c
	}
	if last.Total != exact.Ceil() {
		t.Fatalf("total %v, want %v", last.Total, exact.Ceil())
	}
	// Caching makes calls 2–5 much cheaper than the first.
	if last.Total > 3*money.Dollars(0.0325) {
		t.Fatalf("total %v: cached calls were not priced as cached", last.Total)
	}
}

func TestRoundingIsNotPaidPerCall(t *testing.T) {
	m := New(nil)
	// A one-token call costs 1.25 µ$; a thousand of them cost exactly
	// 1250 µ$, not 2000 µ$ (rounding each call up).
	for range 1000 {
		m.Add(model.Spark, model.Usage{Input: 1})
	}
	if got := m.Total(); got != 1250 {
		t.Fatalf("total = %d µ$, want 1250", got)
	}
}

func TestSnapshotByModel(t *testing.T) {
	m := New(nil)
	m.Add(model.Spark, recorded[0])
	m.Add(model.Wonder, recorded[1])
	s := m.Snapshot()
	if len(s.ByModel) != 2 || s.ByModel[model.Wonder] != recorded[1] || s.Estimated {
		t.Fatalf("snapshot = %+v", s)
	}
	m.Add("muse-unknown", recorded[2])
	if !m.Snapshot().Estimated {
		t.Fatal("an unknown model should mark the meter estimated")
	}
}

func TestConcurrentAdd(t *testing.T) {
	m := New(nil)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var sum money.USD
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for _, u := range recorded {
				c := m.Add(model.Spark, u)
				mu.Lock()
				sum += c.Cost
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if sum != m.Total() || m.Snapshot().Calls != 250 {
		t.Fatalf("sum %v total %v", sum, m.Total())
	}
}
