# agnts-muse

**Run Meta's Muse Code under a hard, live-enforced budget.**

`agnts-muse` is a Go library that drives [Muse Code](https://dev.meta.ai) through **MSP**, its native session protocol (`muse serve`). Because it sees the cost of every model call as it happens, it can:

- reserve money before the spend,
- tell Muse mid-turn to wrap up when the budget runs low,
- stop it just before the call that would go over,
- move a stuck session to a stronger model without losing context,
- and decide every tool call against your own rules.

Concurrent runs sharing a balance can never push it below zero: not across goroutines, not across servers.

It is the engine behind [agnts](https://agnts.uz), a self-hosted AI engineer that takes Jira and Plane tickets, codes them in isolated containers, and ships nothing without approval. agnts lets teams pay for AI in local currency with prepaid credits. `agnts-muse` is what makes those credits safe to sell.

```text
$ muse-budget run --budget 0.05 --model spark "Fix the failing test, then add table-driven tests …"

  … Cheap fix plus new tests — scoping to the essentials to stay on budget.
  $ call 1   █████████████████░░░░░░░ +$0.035775 total $0.035775 / $0.05
  ▸ bash {"command":"ls -la"}
  $ call 2   ███████████████████░░░░░ +$0.005033 total $0.040808 / $0.05
  ⚑ steer told Muse the budget is running low; it should wrap up
  ▸ read_skill {"name":"bundled:durable-test-collateral"}
  $ call 3   ██████████████████████░░ +$0.006964 total $0.047772 / $0.05
  ■ interrupt spent $0.047772 of the $0.05 limit; another call would pass it

■ stopped at the budget: muse: run limit reached (spent $0.047772, held $0.05)
usage 3 calls · 87.1k in (56.9k cached) · 366 out · 20s
receipt charged $0.047772 · released $0.002228
```

*(A real run on Muse Code 1.4.13. Muse read its budget from the prompt and planned around it; the engine steered it at 80% and stopped it inside the limit.)*

## Why

An autonomous agent with an API key is an open-ended bill. Settling after the fact doesn't fix that: ten runs that each see $5 left will each spend $5. Hosted agent products therefore either cap usage crudely (per-day request counts) or absorb the overruns.

`agnts-muse` treats model spend like a card payment:

| Step | MSP / ledger call | What happens |
| --- | --- | --- |
| **Hold** | `Ledger.Hold` | Reserve a first slice before the turn starts. Free credits are taken first, then bought credits, and the hold counts against platform caps. |
| **Extend** | `session/tokenUsage` → `Ledger.Extend` | After every model call, charge it exactly and keep the hold one step ahead of the spend. |
| **Steer** | `turn/steer` | At 80% of the limit, tell Muse mid-turn to finish the current change and summarise. |
| **Interrupt** | `turn/interrupt` | When a cache-aware forecast says the next call would not fit, stop the turn before it starts. |
| **Commit** | `Ledger.Commit` | Charge the exact metered cost, idempotently. Release the rest. |

## What it uses in Muse

Most integrations wrap `muse exec` and parse its output once the run is over. MSP exposes far more. These are the parts `agnts-muse` uses, all verified against the schema the Muse binary exports (`muse schema generate-json-schema`):

| Capability | MSP | Used for |
| --- | --- | --- |
| Per-call usage | `session/tokenUsage` notification | Live metering and holds. `exec --json` reports usage only once, at the end. |
| Mid-turn steering | `turn/steer` | Budget-aware wrap-up without killing the turn |
| Priority interrupt | `turn/interrupt` | Stopping inside the budget |
| Model switch in a session | `session/setModel` | Escalating Spark → Wonder **with context kept**: no restart, no re-reading the repo |
| Client-side approvals | `approval/reviewing`, `approval/requested` → `approval/decide` | Policy rules veto tool calls. A veto sent during Muse's own LLM review wins (verified on a real `git push`). |
| A person in the loop | `turn/steer`, `approval/decide` | A person following the run talks to it mid-turn (`Request.Steer`), and decides the calls a rule leaves to them, such as adding a dependency (`Request.Ask`). The call is vetoed while they are asked, so Muse works on; when they allow it, Muse is told to run it and the guard lets it through ([ADR 0005](docs/adr/0005-person-in-the-loop.md)). |
| Model catalog | `model/list` | What the account can run |
| Subscription allowance | `usage/read`, `usage/changed` | Pausing an account at its limit |
| Images in a turn | `turn/start` input parts | Visual review of UI screenshots |

## Packages

```
agnts-muse/            package muse: Engine.Run, events, errors, LocalExecutor
├── money/             integer money: USD (µ$), Rate ($/M tokens), Exact (p$)
├── model/             catalog, versioned prices, Cost(usage)
├── meter/             usage → charges whose sum is always the rounded total; next-call forecast
├── budget/            Ledger and Funds interfaces, Split and Settle
│   ├── memledger/     in-memory store
│   └── ledgertest/    conformance suite every store must pass
├── ledger/pgledger/   PostgreSQL store (nested module: pgx stays out of the core)
├── msp/               MSP client: JSON-RPC 2.0 over stdio, typed messages checked against Muse's schema
│   └── msptest/       in-memory host that replays recorded sessions
├── route/             model escalation policy
├── guard/             tool-approval policy: default rules, read-only mode, rules that ask a person
├── credential/        API keys, Meta device sign-in (RFC 8628), Muse's credential file, redaction
├── quote/             read-only Muse session that sizes a task before it runs
└── cmd/muse-budget/   the CLI above
```

The core module imports only the standard library.

## Use

```go
ledger := memledger.New() // or pgledger.New(pool) for many servers
ledger.Deposit(ctx, "user:42:free", 20*money.Cent, "admin-grant-7")
ledger.Deposit(ctx, "ws:7:paid", 5*money.Dollar, "payme-txn-991")

eng := muse.New(muse.Config{Ledger: ledger, Observer: muse.SlogObserver{}})
res, err := eng.Run(ctx, muse.Request{
	Prompt:         "Fix the failing test in ./billing",
	Workdir:        "/workspace",
	Complexity:     route.Simple, // Spark first; escalates to Wonder if stuck
	AnnounceBudget: true,
	Budget: &muse.Budget{
		Key:    "run-1842",                                       // idempotent across crashes
		Payers: []budget.AccountID{"user:42:free", "ws:7:paid"}, // free credits first
		Caps:   []budget.AccountID{"cap:platform:2026-10"},       // platform-wide monthly cap
		Limit:  50 * money.Cent,
	},
}, muse.LocalExecutor{}, func(e muse.Event) {
	if e.Kind == muse.EventUsage {
		fmt.Println(e.Charge.Total, "of", e.Held, "held")
	}
})
switch {
case errors.Is(err, muse.ErrRunLimit), errors.Is(err, muse.ErrBudget):
	// stopped inside the budget; res.Receipt says what was charged
case err != nil:
	var rl *muse.RateLimitError
	if errors.As(err, &rl) { /* pause the account until rl.Until */ }
}
```

To run Muse somewhere else, implement `Executor`. agnts runs it with `docker exec -i` inside each run's container.

## Design notes

- **Money is integers** ([ADR 0003](docs/adr/0003-integer-money.md)). Budgets are µ$. Token prices are µ$ per million tokens, which is also picodollars per token, so per-call cost is exact. Rounding happens once, upward, and a meter's per-call charges always sum to its rounded total.
- **Holds, not settling afterwards** ([ADR 0002](docs/adr/0002-holds-over-settlement.md)). Every hold is one transaction that locks its accounts in id order. Postgres also enforces `CHECK (total >= held)`, so no code path, ours or anyone's, can overspend.
- **Stop before, not after.** Muse reports usage after each call, so a naive limit stops one call too late. The meter forecasts the next call cache-aware (it re-reads the cached context and adds only the growth) and stops when that forecast would not fit. The forecast is tested against real recorded calls: never below the actual cost, at most 3× above it.
- **MSP over `exec`** ([ADR 0001](docs/adr/0001-msp-over-exec.md)). Live usage, steering, interrupts and approvals exist only in the session protocol.
- **The schema is the contract.** `msp/schema_test.go` checks every field of every hand-written type against Muse's own JSON Schema snapshot, and the client warns when a server's schema fingerprint changes.
- **Real sessions as fixtures.** `testdata/` holds sanitized recordings of real `muse serve` sessions: a fix with 5 metered calls, and a guard veto of `git push`. `msptest` replays them, so engine tests run against Muse's real output without Muse or an account.
- **Nested modules** ([ADR 0004](docs/adr/0004-nested-modules.md)). `pgledger` brings in pgx; the core does not.

## Quality

| | |
| --- | --- |
| Tests | Unit, golden (recorded sessions), conformance (`ledgertest`, run against both stores), concurrency, fuzz (`msp`, `money`) |
| Race detector | All tests pass with `-race`, repeated 10× |
| Coverage | meter 100%, guard 97%, quote 96%, route 95%, memledger 93%, msp 89%, model 89% |
| CI | gofmt, vet, golangci-lint, `-race`, fuzz smoke, govulncheck, Postgres conformance |

**Benchmarks** (Apple M-series, Go 1.25):

| Benchmark | Result |
| --- | --- |
| memledger hold + commit | 1.4 µs/op, 11 allocs |
| pgledger hold + commit, 8 parallel, Postgres 18 in Docker | 3.5 ms/op (≈280 runs/s settled per DB; a run makes one hold and a few extends) |
| MSP notification throughput | ≈100k messages/s per session |
| Decoding one `session/tokenUsage` | 5 µs |

## Develop

```sh
go test -race ./...
# PostgreSQL store (needs a database it may wipe):
cd ledger/pgledger && AGNTS_MUSE_TEST_DATABASE_URL=postgres://… go test -race ./...
go run ./cmd/muse-budget run --budget 0.25 "describe this repository"
```

Re-record a fixture after a Muse upgrade with `muse.Config{Trace: …}` and update `testdata/msp.schema.json` with `muse schema generate-json-schema`. The schema test then lists every field that moved.
