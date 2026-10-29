package model

import (
	"errors"
	"testing"

	"github.com/rustambekabdusalomov/agnts-muse/money"
)

// The first model call of the recorded session (testdata/msp-session.jsonl).
var firstCall = Usage{Input: 25426, Cached: 0, Output: 152, Reasoning: 65}

func TestCostSpark(t *testing.T) {
	cost, est := Default.Cost(Spark, firstCall)
	// 25426 × $1.25/M + 152 × $4.25/M = $0.0317825 + $0.000646 = $0.0324285
	if est || cost.Ceil() != money.USD(32429) {
		t.Fatalf("cost = %v (estimated %v), want $0.032429", cost.Ceil(), est)
	}
}

func TestCostCachedIsCheaper(t *testing.T) {
	u := Usage{Input: 25693, Cached: 25329, Output: 119}
	cached, _ := Default.Cost(Spark, u)
	full, _ := Default.Cost(Spark, Usage{Input: u.Input, Output: u.Output})
	if cached >= full {
		t.Fatalf("cached %d should cost less than uncached %d", cached, full)
	}
}

func TestUnknownModelIsPricedHigh(t *testing.T) {
	known, _ := Default.Cost(Wonder, firstCall)
	unknown, est := Default.Cost("muse-next-2.0", firstCall)
	if !est || unknown < known {
		t.Fatalf("unknown = %d (estimated %v), want ≥ wonder %d and estimated", unknown, est, known)
	}
}

func TestResolve(t *testing.T) {
	for _, name := range []string{"spark", "SPARK", string(Spark)} {
		m, err := Default.Resolve(name)
		if err != nil || m.ID != Spark {
			t.Errorf("Resolve(%q) = %v, %v", name, m.ID, err)
		}
	}
	if _, err := Default.Resolve("llama"); !errors.Is(err, ErrUnknownModel) {
		t.Errorf("Resolve(llama) error = %v, want ErrUnknownModel", err)
	}
}

func TestModelsOrder(t *testing.T) {
	ms := Default.Models()
	if len(ms) != 5 || ms[0].Tier != TierSpark || ms[len(ms)-1].Tier != TierWonder {
		t.Fatalf("Models() = %+v", ms)
	}
}

func TestWithPrices(t *testing.T) {
	c, err := Default.WithPrices("muse-spark-1.3=2/0.5/8; muse-new-1.0=1/1/1")
	if err != nil {
		t.Fatal(err)
	}
	m, _ := c.Lookup(Spark)
	if m.Price.Input != money.PerMillion(2) || m.Tier != TierSpark || m.Alias != "spark" {
		t.Fatalf("spark = %+v", m)
	}
	n, ok := c.Lookup("muse-new-1.0")
	if !ok || n.Tier != TierWonder {
		t.Fatalf("new model = %+v, %v", n, ok)
	}
	if orig, _ := Default.Lookup(Spark); orig.Price.Input != money.PerMillion(1.25) {
		t.Fatal("WithPrices changed Default")
	}
	for _, bad := range []string{"spark", "x=1/2", "x=a/b/c", "x=-1/0/0"} {
		if _, err := Default.WithPrices(bad); err == nil {
			t.Errorf("WithPrices(%q) accepted", bad)
		}
	}
}
