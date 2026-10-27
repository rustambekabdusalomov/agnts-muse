# 3. Integer money with exact token pricing

Status: accepted · Oct 27, 2026

## Context

Model prices are per million tokens ($1.25/M), so one token costs 1.25 µ$. Floats drift when summed over thousands of calls, and rounding each call up overcharges: a thousand one-token calls would bill 2000 µ$ instead of 1250 µ$.

## Decision

- `money.USD` is int64 micro-dollars.
- `money.Rate` is µ$ per million tokens, which is numerically picodollars per token.
- `Rate × tokens` is `money.Exact` (picodollars), with no rounding.
- A meter sums Exact, rounds up once, and reports each call as the change in the rounded total.

## Consequences

- Per-call charges always sum exactly to the total. `TestRoundingIsNotPaidPerCall` and `TestChargesSumToTotal` check it.
- Prices parse from decimal strings without passing through a float (`money.Parse`, which is fuzzed).
- An int64 of picodollars holds about $9.2M, far more than any single run. Totals across runs stay in µ$.
