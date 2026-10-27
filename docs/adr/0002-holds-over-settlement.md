# 2. Reserve money before spending it

Status: accepted · Oct 27, 2026

## Context

Charging a run after it finishes lets concurrent runs overspend a shared balance: each one checks the balance, sees enough, and spends it. For a product that sells prepaid credits, and gives free credits to judges, that is a direct loss.

## Decision

Model a run like a card payment. `Hold` reserves an amount across ordered payers (free credits, then bought credits) and against caps (a platform's monthly limit). `Extend` grows the hold as usage streams in. `Commit` charges the exact cost (never more than was held) and releases the rest. `Release` cancels.

Holds carry idempotency keys and expire, and `Reap` returns money from crashed runs. The engine keeps the hold one step ahead of the spend and stops the run when the next call (forecast, cache-aware) would not fit.

## Consequences

- No interleaving of runs can take a payer below zero. `ledgertest` checks this with 100 goroutines and random holds, extends, commits and releases. In Postgres a `CHECK (total >= held)` constraint enforces it for any writer.
- A run can still cost slightly more than its hold if one model call is far larger than forecast. That excess is reported as `Receipt.Unbilled`, never taken from a payer, so the platform decides whether to absorb it.
- Every store must lock accounts in one global order. The conformance suite caught a deadlock in the first Postgres version, where commit locked accounts in leg order.
