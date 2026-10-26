# Conversation runtime investigation — 26 October 2026

Status: investigation concluded at Kareem’s request, **not a delivered conversation API**. No production launch,
PTY, prompt, HTTP route, database schema or shared contract has changed.

Ticket: `2026-10-26-0804-add-polling-conversation-api-for-codex-claude-and-opencode`.
The initial requirement targets existing terminal sessions. After the Claude
provenance probe, Kareem selected: “Investigate an opt-in structured-runtime
session mode for genuine chat; preserve current terminal sessions.”
After reviewing the resulting tradeoff, he chose: “Finish this ticket as an
investigation; retain findings and let the architect scope the implementation
separately.”

## Existing terminal owners

| Installed provider | Measured result | Consequence |
| --- | --- | --- |
| Claude Code 2.1.310 / Sonnet 5.5 | Startup prompt is `turnOrigin: human`; asyncRewake input is `turnOrigin: task_notification`. Both receive replies. | A notification cannot be exposed as a genuine user message. Structured input requires a different process mode. |
| Codex 0.164.0 / gpt-5.6-sol | External queue records `UserMessage.client_id`; replies and `task_complete` have the same explicit turn ID. Busy queued input starts only after the first turn completes. | A promising existing-owner send path. This probe does not remove the previously observed Esc stall or crash-after-dispatch duplicate window. |
| OpenCode 1.17.18 / deepseek-v4-flash | Idle `prompt_async` yields a native user message and completed assistant with matching `parentID`. Posting during `sleep 12` retains the tool result but displaces the first requested final reply. | Sending only after an idle read still has a race with terminal input. This cannot promise strict subsequent-turn queueing alongside an independent TUI submitter. |

Claude session: `9b021265-12f2-4d1d-bccb-ba6c25e7ea3d`. External input UUID:
`0fc9bb6a-a065-4d06-83d3-22d3688499c8`, origin `task_notification`.

Codex idle session: `01a17746-6dc3-7022-a195-385321ea6c23`. Client ID
`chat-probe-X001` maps to queue turn `01a17746-ac2a-7193-b06c-16fa9cea9ce4`,
final `ACK-X001` and a matching `task_complete`. Busy probe: first turn
`01a17748-eb90-77e2-9b67-5b4d761b5eda` completed at 08:18:12.234Z;
queue turn `01a17749-de8b-7921-bdf0-e7dfa019b6ac` started at
08:18:12.238Z and completed with `SECOND-X003`. `FIRST-X002` was preserved.

OpenCode session: `ses_e88bb5aa1ffeHv6olDxydOi9SY`. Busy user ID
`msg_17744c6f600022c3b03908ca42` requested `FIRST-O002`; follow-up ID
`msg_17744cf0000082a8ec081f9640` requested `SECOND-O003`. The final assistant
parent is the latter; no `FIRST-O002` final was recorded.

## Opt-in mode: proposed boundary

Introduce an explicit session interaction mode, defaulting to the existing
terminal behavior. A new `chat` mode would use one daemon-owned structured
provider process/connection and no independently submitting provider TUI.
Existing sessions retain their creation-time mode and terminal ownership.
Do not convert active sessions or call resume beside a running owner. An
explicit future stopped-session migration could be investigated separately.

- Claude: persistent `--print --input-format stream-json --output-format
  stream-json --verbose --replay-user-messages`; retain profile/login,
  additive instructions, MCP configuration and tools. Send one user UUID at
  a time; wait for its result before dispatching the next submission.
- Codex: daemon-owned `codex app-server` stdio connection; `thread/start`,
  `turn/start`, native item/turn IDs, `turn/completed`; `thread/resume` only
  after the previous owner is confirmed stopped.
- OpenCode: daemon-owned loopback `opencode serve`; native message IDs and
  parent-linked replies; serialize submissions until the active turn ends.
  Attaching an independent TUI that can submit would invalidate this guarantee.

The client still uses ordinary HTTP and polling. Native structured transports
are implementation details, not a new public streaming surface. Provider
native permission control remains a separate requirement: preserving a
permission policy may block work, and that block must be visible. Silently
changing a profile to bypass or deny all permissions is not an acceptable fix.

### Suggested client contract, not implemented

Existing session reads would add interaction mode and explicit capabilities
(history, send, native-permission support, execution location and limitations).
Remote SSH sends remain unsupported. Existing terminal sessions could expose
history without advertising unsupported send capability.

`GET /api/sessions/{id}/conversation?cursor=...&limit=...` returns normalized
items, stable IDs/order, origin, available timestamps and status, plus a revision
and continuation cursor. Persist normalized history and revision changes so
compaction/resume cannot delete already observed history. Separate snapshots
from changes so an updated old item cannot be missed by forward pagination.
Expose tool activity without hidden reasoning or provider instructions.

`POST /api/sessions/{id}/messages` accepts `{request_id, text}` and promptly
returns a durable submission receipt. Same ID + payload replays it; conflicting
reuse returns a conflict. Poll a submission resource for queued, started,
completed, failed and visible blocked/uncertain conditions. Never infer success
from transport acceptance or a random later assistant message.

Persist the dispatch reservation before native handoff. Reconcile native IDs
before any retry; an ambiguous send must not be blindly replayed. Client
disconnect does not cancel work. Session conclusion explicitly settles queued
submissions; it must not launch a replacement process. Expose pending Hiveryn
questions/intents and reuse their existing answer routes. Chat text is never
an answer to an approval or question.

### Integration work the new mode entails

This exceeds adding HTTP handlers to existing TUI sessions. The daemon currently
announces a live run when it has a `main_terminal_id`; launch, recovery and
unexpected-exit handling are PTY-owned. A chat mode needs a separate owned
process lifecycle, explicit launch readiness, persisted mode/run identity,
restore/discard/conclusion handling, and compatibility with consumers that
currently expect a main terminal. Adapters must preserve launch configuration
without copying the CLI instruction/config synthesis into the daemon.

The ordinary notification outbox, long-running Hiveryn MCP questions, native
permission waits and provider transport failure need integration tests in the
new mode. The small probes below do not establish those product behaviors.

## Structured probe evidence

Claude session `2b7559f8-35c9-4626-bd82-f5f431bf9b8e` completed a synthetic
`sleep 8` turn, a subsequent message, and a third message after process stop
and resume. Native user origin was `sdk`, not `task_notification`. Result
`user_message_uuid`/`user_message_uuids` matched the submitted UUIDs. This is
genuine SDK user input; it should be labelled honestly rather than rewritten
as terminal-origin `human`.

A separate Claude session `e2f98ab1-9931-45e4-b09f-19b3b295d2fd` invoked the
synthetic `probe_echo` MCP tool in three turns including resume. Tools and
existing subscription authentication work with structured input in this probe.

Codex structured session `01a1774a-d364-77a3-9802-ea87d503c9f5` completed
`sleep 8`, the next message, then a resumed message under one owner at a time.
Explicit completion IDs: `01a1774a-ddb2-7891-b059-4fd1274e113e`,
`01a1774b-33cb-7e11-9cb2-1f1f4777dc26`, and
`01a1774b-3f6d-78d1-b3d7-00cec831d490`.

OpenCode initially completed two structured turns, but after restart its
refreshed provider catalog rejected `deepseek/deepseek-v4-flash` with
`ProviderModelNotFoundError`. The POST returned 204 and the user message was
persisted without an assistant reply. This is a native failure case, not a
successful resume. A bounded repeat pins the bundled model catalog to separate
transport recovery from external catalog drift; it passed all three turns in session `ses_e88b225f3ffe6FeStFmnZYLvTu` with `OPENCODE_DISABLE_MODELS_FETCH=true`. A separate pinned-catalog MCP session `ses_e88b22190ffecUUM7U2RBERIPh` also invoked the echo tool in all three turns including restart. This pin is probe-local, not a proposed global configuration change.

## Verification boundaries and evidence hygiene

All probes use disposable config/data/work directories under ignored
`daemon/bin/conversation-probe/`, synthetic messages, and read-only copying of
authentication. Copied credential files are removed after each probe. Only
owned process groups are stopped. No live daemon is restarted, global hooks or
settings changed, or existing conversation resumed/interrupted. No public
listener is used. Terminal output is diagnostic only; native records establish
provenance, message identity and replies.

Initial Codex attempts failed at the probe's terminal handshake/trust setup;
those are not send failures. The first OpenCode attempt used a malformed
13-digit timestamp ID and caused repeated replies. It was stopped; the
corrected 12-digit probe is the evidence above. These harness failures must
not be counted as acceptance.

Not yet verified: daemon API durability/pagination/idempotency, concurrent HTTP
callers, live terminal drafts, daemon crash recovery, compaction, full native
permission control, Hiveryn MCP question/approval lifecycle in chat mode,
notification delivery in chat mode, every configured profile/plugin, or remote
chat. Host-serialized probes demonstrate native sequential turns, not a tested
durable product queue. There is no executable daemon HTTP smoke path yet because
no conversation API has been implemented.

Reference documentation: [Claude programmatic execution](https://code.claude.com/docs/en/headless),
[Codex app-server](https://learn.chatgpt.com/docs/app-server), and
[OpenCode server](https://opencode.ai/docs/server/). Native evidence above governs
installed-version claims; documentation alone does not prove attachment.

Codex MCP negative case: session `01a1774c-ab6b-7351-b221-3c5febe56143` exposed the echo tool, but `approvalPolicy: never` refused the unannotated tool (`MCP tool call requires approval, but approval policy is never`). The model turn completed with that error text; it was not successful tool execution. A repeat correctly annotates this synthetic pure echo tool as read-only/non-destructive/closed-world; no existing provider tool annotations or permission policy are changed.

Codex read-only MCP repeat passed: session `01a17750-c345-7223-9e2e-4a16892eff61` recorded three `mcpToolCall` items with status `completed` and echo results FIRST-MCP001, SECOND-MCP002 and RESUMED-MCP003. This verifies the tool route under its declared annotations; it does not authorize auto-approving arbitrary tools.

Credential-free [native evidence excerpts](evidence/conversation-runtime-2026-10-26.json) retain IDs, origins, turn outcomes and MCP statuses, plus SHA-256 hashes of the isolated source records. Hidden reasoning, account metadata and credentials are excluded. All probe processes were confirmed stopped and no copied auth.json/.credentials.json files remained after cleanup. Scratch scripts and full synthetic records remain locally under the Git-excluded probe directory for follow-up inspection.

## Recommended follow-up scope

The architect should scope an opt-in chat runtime plus the polling API as an
explicit new-session mode, with unchanged terminal defaults. Include process
readiness without a main terminal, mode persistence, serialized durable
submissions, native identity correlation, restart reconciliation and
conclusion/discard disposition. Define how native permission waits are surfaced
without changing profile policy, and verify Hiveryn MCP questions, approvals
and notification delivery in that mode. Include the original pagination,
compaction, concurrent-call, disconnect, failure and HTTP smoke-path acceptance
checks. The held chat UI ticket should wait for that implemented API contract.

Repository verification for this documentation-only result: `make vet`,
`make build`, `make lint`, `make tidy` with no module diff, and `TZ=UTC go test
./...` passed. The initial ambient-Europe/London test run failed a pre-existing
fixed-UTC ticket-ID assertion and a fake-SSH timing check; the SSH check passed
in isolation and the full suite passed in UTC. No source was changed to hide
these failures. Provider native probes are manual isolated evidence, not
ordinary automated test fixtures.
