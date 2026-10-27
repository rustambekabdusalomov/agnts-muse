package route

import (
	"testing"

	"github.com/rustambekabdusalomov/agnts-muse/model"
	"github.com/rustambekabdusalomov/agnts-muse/money"
)

func TestEscalate(t *testing.T) {
	p := Default()
	for _, tc := range []struct {
		name string
		s    Signal
		want bool
	}{
		{"healthy", Signal{Model: model.Spark, Calls: 3, Remaining: money.Dollar}, false},
		{"stuck on tools", Signal{Model: model.Spark, FailedTools: 3, Remaining: money.Dollar}, true},
		{"long and failing", Signal{Model: model.Spark, Calls: 25, FailedTools: 1, Remaining: money.Dollar}, true},
		{"long but fine", Signal{Model: model.Spark, Calls: 40, Remaining: money.Dollar}, false},
		{"already strong", Signal{Model: model.Wonder, FailedTools: 9, Remaining: money.Dollar}, false},
		{"unknown model", Signal{Model: "muse-next", FailedTools: 9, Remaining: money.Dollar}, false},
		{"escalated once", Signal{Model: model.Spark, FailedTools: 9, Remaining: money.Dollar, Escalations: 1}, false},
		{"budget too low", Signal{Model: model.Spark, FailedTools: 9, Remaining: 10 * money.Cent}, false},
		{"no budget", Signal{Model: model.Spark, FailedTools: 3, Remaining: -1}, true},
	} {
		d, ok := p.Next(tc.s)
		if ok != tc.want || ok && (d.To != model.Wonder || d.Reason == "") {
			t.Errorf("%s: Next = %+v, %v; want %v", tc.name, d, ok, tc.want)
		}
	}
}

func TestInitial(t *testing.T) {
	if Default().Initial(Simple) != model.Spark || Default().Initial(Complex) != model.Wonder {
		t.Fatal("initial models")
	}
	if Fixed(model.Wonder).Initial(Simple) != model.Wonder {
		t.Fatal("fixed")
	}
	if _, ok := Fixed(model.Spark).Next(Signal{FailedTools: 99}); ok {
		t.Fatal("fixed never changes")
	}
}
