# Conversation API acceptance — 26 October 2026

This record covers the opt-in chat implementation, unlike the earlier
[transport investigation](conversation-runtime-investigation.md). Tests used
separate daemon binaries, state/databases, loopback ports, synthetic projects,
provider configuration/data locations, and read-only authentication copies. No
live daemon restart/build, existing conversation takeover, global hook/settings
change, package release or deployment was performed.

## Native daemon acceptance

Installed versions: Claude Code 2.1.310, Codex 0.164.0, OpenCode 1.17.18.
Synthetic variants explicitly used autonomous permissions for the MCP positive
cases. A separate non-autonomous Codex case exercised a real permission block.

| Provider | Observed through daemon HTTP and native transport |
| --- | --- |
| Codex / gpt-5.6-sol | Two queued messages completed in order with distinct native turn IDs; a third completed after daemon shutdown/restart. Real Hiveryn askQuestion was answered `Beta` through HTTP, notify reached the isolated notification sink, and concludeTicketSession moved the fixture ticket to done and settled its active submission. |
| Claude / Sonnet | Same sequential/resume and real Hiveryn MCP question/answer, notify and conclusion flow passed. Native user records retain SDK origin and caller UUIDs; result matching uses user_message_uuid. |
| OpenCode / deepseek-v4-flash | Same sequential/resume/MCP flow passed with parent-linked reply IDs. A separate architect + worker run survived SIGKILL of the isolated daemon: both owned provider process groups disappeared before restore, the worker resumed, concluded, and its outbox notification reached the architect through the shared chat queue with notification origin. |

The notification sink accepts the real daemon ntfy HTTP publication locally;
this does not claim a real phone push was delivered. The conclusion probe's
receipt is `failed: Session ended before submission completed`: successful
Hiveryn conclusion kills the submitting process, so fabricating a completed
native model turn would be wrong. Ticket done-state independently proves the
conclusion side effect.

Concise IDs, receipts and versions are retained in
[evidence JSON](evidence/conversation-api-2026-10-26.json). Full synthetic native
records and executable probe scripts are private Git-excluded scratch under
`daemon/bin/chat-acceptance/`; the useful runs are:

- `run-1793004967`: Codex MCP acceptance (also records the later primary-Claude auth failure).
- `run-1793005538`: Claude MCP acceptance using the existing work profile.
- `run-1793005147`: OpenCode MCP acceptance.
- `run-1793005574`: OpenCode architect/worker crash ownership and notification acceptance.
- `run-1793005812`: Codex native approval wait; discard settles queued input.
- `run-1793006047`: native OpenCode invalid-model failure after accepted input.

All run-owned daemon/provider processes were stopped; copied credential files
were deleted by probe cleanup. Evidence does not contain credentials, hidden
reasoning or full role/kickoff instructions.

## Negative evidence and limits

- Primary Claude auth passed an earlier queue/resume run, then reported
  `Failed to authenticate: OAuth session expired and could not be refreshed`.
  The architect explicitly authorized another already-authenticated profile.
  A read-only copy of Claude work credentials (over seven hours of access-token
  validity remaining) passed the full MCP run. The primary login was not
  refreshed or overwritten. This substitution is not evidence that the primary
  login is now usable.
- Codex `approval_policy="on-request"` produced a genuine pending native request
  exposed as blocked. It was not answered, auto-approved, or treated as chat
  text. The queue stayed held until discard. An attempted old
  `approval_policy="untrusted"` failed startup with Codex's original message
  that it is no longer supported; launch diagnostics now retain bounded,
  credential-redacted stderr instead of reporting only EOF.
- OpenCode positive probes pinned its bundled catalog in the isolated variant
  with `OPENCODE_DISABLE_MODELS_FETCH=true`, as in the investigation. The
  invalid-model probe accepted the user message, then emitted native
  `session.error` with `UnknownError` / `Model not found:
  missing-provider/nonexistent-model.`; the provider log classified it as
  `ProviderModelNotFoundError`. The API retained the original error, marked the
  head uncertain, and exposed its blocking reason on the queued receipt.
  A 204 is never completion. No global catalog setting changed.
- In-flight crash recovery is conservative. Explicit native terminal evidence
  settles an interrupted submission; missing identity, compacted-away evidence
  or partial delivery remains uncertain without blind replay. Full permission
  UI, mid-turn steering/interrupt, remote SSH chat and retention pruning remain
  outside V1. Native testing does not cover every arbitrary profile/plugin.

## Automated checks

Deterministic coverage exercises concurrent HTTP callers, same-ID replay and
conflict, client disconnection, busy queues, durable reservation/reopen,
uncertain handoff, conclusion/discard terminal dispositions, revision snapshot
pagination, old-item updates, retained history through missing provider history,
explicit native reconciliation, hidden-reasoning exclusion, unrelated completion
rejection, permission waits, and tool failure versus model completion.

`python3 scripts/conversation-smoke.py` passes the isolated executable
create/list/read/send/poll/answer/restart/discard contract flow. It uses a fixture,
not a real provider. Desktop discovery tests cover chat without a main terminal,
continued session event subscriptions and unaffected terminal discovery.

Repository checks and exact-commit CI results are recorded in the ticket
conclusion. During local full-suite testing, the existing fake-SSH startup timing
test failed once under parallel package load; the entire daemon suite passed
with `TZ=UTC go test -p 1 ./...`. No unrelated SSH or timezone test was changed.
