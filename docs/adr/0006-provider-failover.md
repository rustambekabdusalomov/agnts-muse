# 6. Provider failover: continue the session elsewhere

Status: accepted · Oct 29, 2026

## Context

agnts runs on its own Meta Model API key for everyone who works on credits. When that account's allowance or credit runs out, every run fails until it resets, sometimes days later. The person's credits are still there, but nothing can spend them.

Muse Spark Open, Meta's open-weights Spark, is also served by other hosts on an OpenAI-compatible API (Baseten for the hackathon). Verified with Muse Code 1.4.14 on Oct 29, 2026:

- Muse sends `META_API_KEY` to another host only when the host is pinned in the `endpoint_transport` setting with `auth = "bearer"`. A `--base-url` flag alone gets "Meta bearer withheld: base_url is off the sanctioned front door". `muse serve -c endpoint_transport=…` pins it for one process without touching the user's settings.
- With that pin, MSP works unchanged on Baseten: tool calls, approvals, `session/tokenUsage`, `turn/completed`.
- A session that failed on Meta with a quota error (`rate limited until …`) was resumed on Baseten with `session/resume` and remembered what it had been told. Sessions are stored on disk by Muse, not by the provider.
- Muse classifies provider failures (`rate_limit`, `overloaded`, `authentication_failed`, `billing_error`, `server_error`, …) and retries the passing ones itself.

## Decision

- `credential.Hosted(baseURL, key)` is an API key for another host. `Credential.Args` adds the `endpoint_transport` pin to `muse serve`, so the key only ever goes to the host it was given for.
- `muse.Failover` wraps an Engine with one fallback `Provider` (credential and model) and a `Mode` read at every run: `auto`, `primary` or `fallback`. An operator can switch it live.
- In `auto`, a run whose primary fails with `ProviderOut` (a quota, a refused key, a billing error) continues on the fallback:
  - It **resumes the same session** with a "continue where it stopped" prompt that repeats the task, so work already done is kept. Ephemeral sessions start afresh.
  - It spends **what is left of the run's limit**, under its own budget key (`key:provider`). The primary's hold was already settled, and reusing its key would find a finished hold.
  - It stays on one model (`route.Fixed`). The host serves only that model, so escalation to Wonder is off there.
- What a person may spend on the fallback is limited apart from the primary. `Budget.FallbackCaps` are ledger caps that only the fallback's spend counts against (a person's monthly allowance there, the host's credit). `Budget.FallbackLimit` caps the run there (< 0: nothing, so the run does not move and fails with `ErrFallbackLimit` beside the primary's error). Fallback holds always end in `:provider`, so their spend can be counted apart.
- After the primary runs out, it is skipped until the quota's reset time, or for `Cooldown` (15 minutes) when Muse does not give one. Later runs then do not each pay a failed attempt first. `Reset` ends that early.
- Overloads, server errors and the run's own budget (`ErrBudget`, `ErrRunLimit`) do not fail over. Muse retries the first two itself, and the last means the payer is out, not the account.
- `Result.Fallback` and `Result.Primary` say where a run went. `Usage` covers both attempts, and each attempt has its own receipt.
- Prices: `model.SparkOpen` is in the default catalog at Baseten's list price, the same as Spark on Meta. A person pays the same either way. `Catalog.WithPrices` overrides it.

## Consequences

- A Meta outage of the platform account no longer stops paid work. Runs keep going on the fallback at the same price per token.
- The fallback has one model. A task that would have escalated to Wonder finishes on Spark Open instead.
- Failover is decided per account. One `Failover` serves one primary account: agnts uses it for runs on its own key, not for people's own Meta accounts, which Meta bills and limits for them.
- `ProviderOut` reads billing failures from the turn error's text. Muse does not give them a typed kind over MSP. If Muse changes that text, a billing failure stops failing over. A quota or auth failure still does, because those are typed.
