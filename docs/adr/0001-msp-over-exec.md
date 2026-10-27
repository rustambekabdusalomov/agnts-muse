# 1. Drive Muse Code through MSP, not `muse exec`

Status: accepted · Oct 27, 2026

## Context

Muse Code has two headless surfaces. `muse exec --json` runs one prompt and prints an event log. `muse serve` speaks MSP: JSON-RPC 2.0 over stdio, with 70 methods and 45 notifications, all described by a JSON Schema the binary exports.

We recorded both on the same task. `exec` emits a single `run.usage` record **when the run ends**. MSP emits `session/tokenUsage` **after every model call**, and it also accepts commands while a turn runs: `turn/steer`, `turn/interrupt`, `session/setModel` and `approval/decide`.

## Decision

The engine drives `muse serve`. The `msp` package is a typed client. Its message types are hand-written for the subset we use and tested field by field against the exported schema (`testdata/msp.schema.json`, fingerprint `sha256:7fddaf64…`).

## Consequences

- Budgets are enforced during a run, not reconciled after it.
- Approvals can be decided by policy. We confirmed on a real session that a veto sent while Muse's LLM judge is still reviewing wins (`resolvedBy: user`, `policyResult: deny`).
- MSP is a larger surface that can change. The schema test and the fingerprint warning make changes loud.
- Every command needs a UUIDv7 `commandId`. `clientInfo.name` must match `^[a-z0-9_]+$`, and the `initialized` notification must follow the `initialize` reply. These were learned by probing and are encoded in the client.
