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

## Cross-client history and archive verification (2026-10-26)

Historical record. The receipt list/read and archived-chat discovery endpoints
it verified were removed on 2026-10-28 for the single-client API (see
[Receipts](conversation-api.md#receipts) and [Ended sessions](conversation-api.md#ended-sessions));
retention, replay and archived-send refusal remain.

The follow-up delivery adds migration 0018, receipt discovery and explicit archived
chat descriptors. Focused regressions cover stable string/block Claude user IDs,
foreign-echo/hidden-thinking exclusion, tool-name retention and native tool errors,
receipt pagination/redaction/held queue context, empty retained histories, archive
idempotency, late output suppression and refusal to launch/admit archived chats.
The executable smoke also verifies archive discovery after daemon restart and
request-ID replay after discard. Evidence: ignored
`daemon/runs/conversation-smoke-xb575dam/evidence.json`.

Real Chrome acceptance ran against isolated source-built daemon state, with fresh
browser storage contexts and a local notification sink. Provider versions:
Claude Code 2.1.310 (sonnet, read-only work login reuse), Codex 0.164.0
(gpt-5.6-sol), OpenCode 1.17.18 (deepseek/deepseek-v4-flash).

- Claude message sent from the UI appeared as a native user item in a fresh
  browser; hidden kickoff echoes remained excluded. Completed tool revisions
  retained names.
- A real askQuestion held the Claude turn while a second client submitted input.
  The fresh browser discovered that queued receipt and answered Beta; the queue
  then drained normally.
- A native conclusion intent was denied in the UI. Repeating the same payload
  replayed the denial; a new explicit request with a distinct summary was approved.
  The worker became a retained concluded archive, rediscoverable after reload,
  with a disabled composer and interrupted conclusion tool (no invented success).
- The Codex architect received and completed the labelled conclusion notification.
- OpenCode UI input and its polled reply passed. Mobile archive navigation at
  390×844 had no horizontal overflow; desktop checks used 1280×900.
- Separate deterministic browser probes exercised blocked and uncertain dispatch,
  cross-client held receipts, no automatic replay, and discarded archive discovery.
  These failure probes are fixtures, not native permission-control acceptance.

Concise screenshots, synthetic inputs/results and scripts are retained in
`chat-ui-poc/runs/cross-client-evidence/`. Native state:
`chat-ui-poc/runs/isolated-1793009689`; fixture state:
`chat-ui-poc/runs/isolated-1793009925`. The manifests identify base commits; native
checks exercised the working implementation before commits. Subsequent changes
only narrowed interruption to tool/permission items, recognized native in-progress
spellings and reduced receipt-list database queries; regression checks were rerun.
Owned daemons, provider processes and UI servers were stopped, and copied
credentials removed. No live restart, deployment or global settings changes.

Reproduce final hands-on startup from `chat-ui-poc` with the delivered sibling
checkouts (daemon pins the delivered agentruntime dependency):

```sh
pnpm install --frozen-lockfile
python3 scripts/isolated-daemon.py --providers claude,codex,opencode --port 49410
# Separate terminal, also in chat-ui-poc:
HIVERYN_DAEMON_URL=http://127.0.0.1:49410 pnpm dev --port 3217
```

Open http://127.0.0.1:3217. Use `--providers fixture` to avoid authentication/model
calls. Ctrl-C both terminals when finished. Final hands-on acceptance remains
Kareem's; pre-migration deleted metadata cannot be backfilled and native permission
controls remain outside V1.
