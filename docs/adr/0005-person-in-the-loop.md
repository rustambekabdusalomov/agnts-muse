# 5. A person in the loop: veto, ask, then steer

Status: accepted · Oct 28, 2026

## Context

agnts's chat has a Code mode: a person follows a ticket's run in a chat. Two things they should be able to do while Muse works:

1. Add to the request ("show the seconds too") without stopping the run.
2. Decide tool calls a policy should not decide alone, such as adding a dependency.

The guard answers `approval/reviewing` and `approval/requested` synchronously. A person takes seconds or minutes, and Muse's own LLM judge may approve a call while it is still reviewing. Blocking the run loop on a person would also stall metering and budget enforcement.

## Decision

- `Request.Steer` is a channel of the person's messages. Each one goes into the running turn with `turn/steer`. It does not count as the budget steer, which still comes at 80% of the limit.
- `guard.Verdict.Ask` lets a policy leave a call to a person (`Rule.Ask`, `guard.AskBeforeInstalls`). Denials still win over questions, in `Rules` and in `Chain`.
- When a call is left to a person, the engine vetoes it at once. A veto sent while the judge reviews wins (ADR 0001). The feedback tells Muse a person was asked and to go on with other work. The question goes out through `Request.Ask`, which returns a channel and never blocks.
- The answer comes back to the run loop. If allowed, the exact command (or path) goes on the run's allow-list, and Muse is steered to run it now. If denied, or not answered within `Config.AskTimeout` (5 minutes), Muse is steered not to run it.
- Without `Request.Ask`, a call left to a person is denied, and the feedback says nobody can be asked.

## Consequences

- The loop never waits on a person. Usage, holds and interrupts keep working while a question is open.
- Muse's judge cannot let an asked-about call through before the person answers.
- An allowed command runs on Muse's next attempt, not the original one, so it costs one more model call. That is the price of not blocking.
- An answer that arrives after the turn ended is reported (`EventApproval`). It changes nothing in that run.
