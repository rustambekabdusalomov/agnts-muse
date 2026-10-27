package muse_test

import (
	"context"
	"errors"
	"fmt"
	"log"

	muse "github.com/rustambekabdusalomov/agnts-muse"
	"github.com/rustambekabdusalomov/agnts-muse/budget"
	"github.com/rustambekabdusalomov/agnts-muse/budget/memledger"
	"github.com/rustambekabdusalomov/agnts-muse/money"
	"github.com/rustambekabdusalomov/agnts-muse/route"
)

// A run that pays a person's free credits first, then their workspace's,
// is stopped inside a 50¢ limit, and reports every model call as it is
// charged.
func ExampleEngine_Run() {
	ctx := context.Background()
	ledger := memledger.New()
	ledger.Deposit(ctx, "user:42:free", 20*money.Cent, "admin-grant-7")
	ledger.Deposit(ctx, "ws:7:paid", 5*money.Dollar, "payme-991")

	eng := muse.New(muse.Config{Ledger: ledger})
	res, err := eng.Run(ctx, muse.Request{
		Prompt:         "Fix the failing test in ./billing",
		Workdir:        "/workspace",
		Complexity:     route.Simple,
		AnnounceBudget: true,
		Budget: &muse.Budget{Key: "run-1842", Payers: []budget.AccountID{"user:42:free", "ws:7:paid"},
			Limit: 50 * money.Cent},
	}, muse.LocalExecutor{}, func(e muse.Event) {
		if e.Kind == muse.EventUsage {
			fmt.Printf("call %d: %v (total %v, held %v)\n", e.Charge.Calls, e.Charge.Cost, e.Charge.Total, e.Held)
		}
	})
	var rl *muse.RateLimitError
	switch {
	case errors.Is(err, muse.ErrRunLimit), errors.Is(err, muse.ErrBudget):
		fmt.Println("stopped inside the budget; charged", res.Receipt.Charged)
	case errors.As(err, &rl):
		fmt.Println("the account is at its limit until", rl.Until)
	case err != nil:
		log.Fatal(err)
	default:
		fmt.Println(res.Text, "for", res.Cost())
	}
}
