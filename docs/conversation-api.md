# Local conversation API V1

The same polling API serves two kinds of session. Ordinary laptop sessions
(terminal mode, launched normally from the desktop) are read and continued
through the provider's own route into their running TUI conversation — see
[Existing terminal sessions](#existing-terminal-sessions-laptop-continuity).
New sessions may instead opt into `interaction_mode: "chat"`. Omitting it retains the
existing terminal mode, including existing records. Architect/ticket ownership,
workspace validation, variants, repository snapshots and single-session rules
are unchanged. Interaction mode is immutable for the life of the session; runs
use that stored mode and their frozen profile/scope. No active TUI is converted
or attached as another submitter.

The daemon owns Claude's structured CLI, Codex app-server or a private loopback
OpenCode server. These provider transports are internal. Public clients use
ordinary HTTP and polling; no streaming/authentication/gateway is added. Listener
exposure is unchanged. Remote SSH chat is rejected.

## Create, launch and discover

All responses use the existing `{data,error,logs,commands,meta}` envelope.

```http
POST /api/sessions
Content-Type: application/json

{"session_type":"ticket","architect_key":"example","ticket_id":"TICKET_ID","interaction_mode":"chat","workflows":[]}
```

Use an actual backlog ticket in an isolated project. Architects omit `ticket_id`.
Launch separately with `POST /api/sessions/SESSION_ID/runs` and
`{"profile_name":"CONFIGURED_VARIANT"}`. Readiness means the structured transport
initialized and its native session identity was persisted. A chat launch returns
`run` without a terminal ID or WebSocket URL. The unchanged kickoff is the first
entry of the chat owner's internal queue (kickoff and outbox notifications only);
user input is never queued and is refused as busy until the kickoff turn ends.
A worker is moved to progress before the kickoff runs, so its readTicket/conclusion
calls see correct state.

Existing `GET /api/sessions` and `GET /api/sessions/SESSION_ID` include:

```json
{
  "interaction_mode": "chat",
  "conversation": {
    "history": true,
    "send": true,
    "native_permissions": false,
    "execution_location": "local",
    "limitation": "Native permission controls are unavailable; a permission request holds the chat"
  }
}
```

`send` requires a running local chat session. It says the route exists, not that
the agent is idle right now: a send is answered synchronously (below). Existing terminal/question/intent desktop
flows remain; chat sessions have a labelled status placeholder and session event
subscription, without a fake main terminal or a desktop chat UI.

## Send (synchronous delivery)

```http
POST /api/sessions/SESSION_ID/messages
Content-Type: application/json

{"request_id":"client-unique-123","text":"Explain the fixture."}
```

Hiveryn does not queue user input. The request records a durable identity and
attempts delivery to the existing agent before answering, within a bound of
**20 seconds** (`submitDeliveryBound`). It never waits for the agent's reply.

- **HTTP 200** — the provider accepted the input. `data` is the receipt, with
  `accepted_at` and `acceptance` naming the evidence (see below). Codex's own
  native queue counts as acceptance, so a busy Codex session still answers 200.
- **Non-success** — the standard error envelope with a stable `error.code`, the
  original reason in `error.message` and the durable receipt (when one exists) in
  `error.details.submission`:

| `error.code` | HTTP | Meaning |
| --- | --- | --- |
| `SESSION_BUSY` | 409 | The agent is working, another input is being handed over, earlier input has not reached the conversation yet, or (chat) an entry holds the chat |
| `SESSION_NOT_RUNNING` | 409 | No running run |
| `SESSION_ENDING` | 409 | Conclusion/discard in progress |
| `SESSION_ARCHIVED` | 409 | The session ended; no receipt is created |
| `SEND_UNAVAILABLE` | 409 | The route cannot take input now (no native id yet, no Claude waiter, OpenCode server unknown, chat owner not ready) |
| `SEND_UNSUPPORTED` | 422 | Remote SSH session or an agent without an input route |
| `DELIVERY_REJECTED` | 502 | The provider refused it, or a known failure before the hand-off (reservation, preparation, deadline before the hand-off) |
| `DELIVERY_INTERRUPTED` | — | (receipts only) the daemon stopped before the hand-off |
| `QUEUED_BEFORE_CUTOVER` | — | (receipts only) waiting in the former Hiveryn queue at the cutover; never sent |
| `DELIVERY_UNKNOWN` | 504 | A hand-off was attempted and acceptance could not be established |

Every code except `DELIVERY_UNKNOWN` is **definitive**: nothing reached the
provider and the input is never delivered later (not after idle, not after a
restart). `DELIVERY_UNKNOWN` is ambiguous: the input may be in the conversation.
It is never resent; the daemon reconciles it from native evidence (the native
record, Codex's queue, the chat turn) and the receipt moves to `accepted`,
`running` or a turn outcome, or settles `uncertain`.

**Retry versus reconcile.** A `request_id` names one delivery decision.

- Same `request_id` and text: replays the recorded outcome (200, the same
  refusal, or `DELIVERY_UNKNOWN` until reconciled). It never sends again. Use it
  after a lost response, a reconnect or a restart, and to check an unknown send.
  A request whose attempt is still in progress waits for it, so concurrent
  duplicates get one outcome and one hand-off.
- To send a definitively refused message again, use a **new** `request_id`.
  Never do that for `DELIVERY_UNKNOWN`: it may already be in the conversation.
- Same `request_id` with different text is HTTP 409 `CONFLICT`.

The attempt runs detached from the HTTP request: a client that disconnects,
times out or retries does not cut a hand-off in half. Session teardown waits for
an attempt in progress (bounded) before settling it. Request IDs are 1–200
characters; text is nonblank and at most 1 MiB.

### Receipts

A receipt is returned only by `POST /messages`: as `data` on 200, or in
`error.details.submission` with a refusal. It has `id`, `session_id`,
`request_id`, `sequence`, `origin`, `text`, `status`, `native_turn_id`,
`error_code`, `error`, `accepted_at`, `acceptance`, `created_at`, `updated_at`.
There is no receipt read or list endpoint. The API assumes one connected client
at a time: that client keeps its own request IDs and unsent/unknown text (across
reloads too) and reconciles by repeating the same POST, which returns the
receipt's current state. Follow the turn itself in `GET /conversation`. Another
client does not discover sends that never entered the conversation history.

| `status` | Meaning |
| --- | --- |
| `delivering` | The synchronous attempt is in progress |
| `rejected` | Definitively not sent (`error_code`); final |
| `unknown` | Acceptance not established (`DELIVERY_UNKNOWN`); being reconciled, never resent |
| `accepted` | The provider accepted it (Codex native queue, OpenCode server); not yet seen running |
| `running` | The input is in the native conversation; its turn has not ended |
| `blocked` | (chat) a native permission request needs a control V1 does not provide |
| `completed` | The matching native turn completed. Individual tool items may still have failed |
| `failed` | Explicit native turn failure/interruption, or the session ended before completion |
| `uncertain` | Settled without the evidence: delivery (when `accepted_at` is absent) or completion cannot be established; not resent |
| `queued` | (chat internal entries only) kickoff/notification waiting in the chat owner's queue |

`accepted_at`/`acceptance` record acceptance and stay set through later turn
states: delivery acceptance is separate from turn completion. Acceptance evidence
per route: Claude terminal `claude_native_record` (the input in Claude's own
transcript — the asyncRewake waiter taking it is not enough), Codex terminal
`codex_thread_queue` (`thread/queue/add` answered), OpenCode terminal
`opencode_prompt_async` (the TUI server answered 2xx), chat `claude_stream_output`,
`codex_turn_start` or `opencode_prompt_async` (the structured transport's first
output for the input), and `native_record` when an unknown input was later found
in the record (or for receipts migrated at the cutover). A chat kickoff receipt
carries the launch text recorded when it was queued; that alone asserts no
acceptance (its acceptance fields follow the turn like any other entry).

This is bounded evidence, not exactly-once execution: a provider can accept input
whose response is lost. Such input is `unknown`, never replayed. A chat whose
head entry is `blocked`, `unknown` or `uncertain` stays held: later sends are
`SESSION_BUSY` naming that entry. No resubmit/interrupt/force-resolve API is
provided in V1. Discard/conclude through the existing lifecycle operations when
the session cannot continue safely.

**Cutover (migration 0020).** User input still `queued` in the former Hiveryn
queue becomes `rejected` with `QUEUED_BEFORE_CUTOVER` (text and history kept,
never dispatched); former `started` input with a native turn becomes `running`,
other `started` input `unknown`. Settled receipts with native turn evidence get
an approximate `accepted_at` (the hand-off or admission time). At startup any
attempt the previous daemon left `delivering` becomes `rejected`
(`DELIVERY_INTERRUPTED`, never handed over) or `unknown` (a hand-off was reserved).

## Read history and old-item updates

```http
GET /api/sessions/SESSION_ID/conversation?after_revision=0&limit=100
```

The page contains `items`, `snapshot_revision`, `next_revision`, `has_more`.
Each item has stable `id`, `sequence`, `revision`, `submission_id`, native turn ID
when known, `role`, `origin`, `status`, available timestamp, text and tool name.
Roles are `user`, `assistant`, `tool`, `system`; origins are `user`, `kickoff`,
`notification` (every item of a turn carries its input's origin). User items carry
`via`: `terminal`, `api`, `notification` or `kickoff`. Native SDK user messages
are not relabelled as terminal human input. Internal notifications are explicitly
labelled even when the provider's structured transport represents their input as
a user message.

**Kickoff.** The launch message is conversation content: the user item with
`origin`/`via` `kickoff` is the provider's own record of it (terminal: the native
transcript/rollout/database entry; chat: the transport's echo of the queued
launch text), verbatim and complete, including the Overview and selected
workflows injected into a worker's first message. Its replies and tools follow in
the same turn. It appears once per launch (a stable native id; a later run of the
same session that launches again records its own kickoff). Separate role
instructions (SYSTEM/developer prompts), hidden reasoning, hook plumbing and
credentials are never part of it. Nothing is reconstructed: a session whose
record (or, for chat, stored history) has no kickoff entry shows none.

This endpoint is an append-only **version feed**, not a list of just new messages:
upsert items by `id`, retain the greatest revision, display by `sequence`.
To continue a page, send `after_revision=next_revision` and the same
`snapshot_revision`. Once `has_more` is false, the next poll starts at that
snapshot revision and omits `snapshot_revision`. A change to an old item is a
new revision. Provider compaction/resume never removes observed history. Page
size is at most 200. Already-observed history survives session deletion and stays
readable by the session's known ID; unknown/unsupported session history returns an explicit error.
Retention pruning is not implemented in V1.

**Order.** Three separate things: `revision` is the feed cursor (discovery
order, for incremental synchronization only); `sequence` is the display
position within the session; `native_turn_id`/`submission_id` are turn
identity. Display order follows the native record's order, never timestamps
(missing, equal or skewed times do not reorder anything). The daemon writes
each native read (terminal: the whole record; chat: each transport event as it arrives, or a
turn's recovered events) in
record order: items already shown in that order keep their `sequence`; an item
seen for the first time — or stored out of record order, such as a launch
message first persisted after its replies — is placed directly after its native
predecessor, or before its first native successor, or else at the end. Moving
an item is a new revision of it, and each write is one transaction, so an
incremental client and a fresh read converge on the same order. Sequences are
gapped and only comparable within one session; when a gap runs out the session
is renumbered (every moved item is a new revision). Order items by `sequence`,
then `id`. Items of one native turn are contiguous only as far as the record
itself places them; clients group adjacent items by turn identity and must not
merge turns merely because an identifier is missing.

Only visible message text/tool output is normalized. Hidden reasoning, raw
provider payloads and role instructions (SYSTEM/developer prompts) are excluded.
Tool failures retain their own status separately from the enclosing turn.
Conversation records have the same local trust boundary as other daemon state;
tools can of course produce sensitive user-requested output.

## Questions, approvals and notifications

`GET /api/sessions/SESSION_ID/controls` returns the durable question/intent event
history under `events`. Reduce it by `question_id`/`intent_id`: `required` adds an
entry, `resolving` keeps an intent open, and `resolved` removes it. These events
are excluded from the ordinary session event ring.

Use the existing operations:

```http
POST /api/sessions/SESSION_ID/questions/QUESTION_ID/answer
{"answer":"The chosen answer"}

POST /api/sessions/SESSION_ID/intents/INTENT_ID/approve
{}

POST /api/sessions/SESSION_ID/intents/INTENT_ID/deny
{"reason":"Reason for denial"}
```

Chat messages never answer questions or approve tools. Hiveryn MCP tools and
phone notification acceptance use the existing daemon paths. Agent-message
outbox notifications to a chat session enter the chat owner's internal queue
under a distinct origin (the outbox reservation names that receipt before
admission); they are the only entries besides the kickoff that wait there.
Completed native turn evidence acknowledges delivery, not task review.

Native permission UI is deferred. Policies remain configured: Claude stdio
control requests, Codex server approval requests and OpenCode permission waits
surface as blocked (holding the chat) without being answered. Unsupported profile flags are errors,
not silently discarded. Codex chat forwards app-server configuration/feature
flags; TUI-only raw flags are rejected. OpenCode accepts its agent selector;
other TUI-only raw args are rejected. Model, additive instructions, writable
scope, MCP/timeouts, login environment and supported permission settings are
synthesized in agentruntime. No global provider setup hooks are installed for
chat. OpenCode model-catalog changes can yield 204 followed by `session.error`;
that original error is retained and is never called successful completion.

## Ownership and verification

Owned process groups inherit an exclusive session lock and a small EOF watchdog.
Daemon death closes the watchdog pipe and terminates that group; restart must
obtain the lock before starting another owner. Unexpected provider exit resumes
only the stored native identity/scope/profile; an ID-less chat resume is refused.
Normal daemon shutdown stops its chat owners; startup restores them. Conclusion
and discard settle open submissions and prevent late native events resurrecting
them. Neither clients disconnecting nor switching desktop tabs stop the owner.

Run the executable smoke flow from the daemon checkout:

```sh
python3 scripts/conversation-smoke.py
# Optional retained disposable state and evidence:
python3 scripts/conversation-smoke.py --keep
```

It builds into a temporary directory and uses a deterministic provider fixture,
private state/workspace/ports and a loopback notification sink. It does not read
authentication or target the live daemon. It exercises create/list/read/send,
concurrent admission, replay/conflict, pagination, question/answer, restart and
discard, including a single synchronous acceptance among concurrent sends and
the others refused as busy. Fixture success is not native acceptance. See
[the native acceptance record](conversation-api-verification.md) for installed
provider evidence and limitations. No UI build, live restart, package release or
deployment is part of this delivery.

## Ended sessions

An ended (concluded or discarded) session leaves `GET /api/sessions`; there is
no archive listing. The daemon keeps a retention record of the session, the
unfinished receipt disposition and final item revisions, committed together
before active records are deleted. A client that knows the ID still reads its
retained history with `GET /conversation`; exact prior request-ID replay still
returns the recorded outcome without dispatching again; new input is refused as
`SESSION_ARCHIVED`, and the session cannot be launched again.

Unfinished tool/permission items become `interrupted`; their names/output remain.
Existing native errors and successful results remain unchanged. Late native
output cannot resurrect ended activity. A conclusion tool interrupted by its own
session end is not claimed to have returned native success. Tool names are
retained across result revisions. Claude string and text-block user echoes share
stable UUID-based IDs; internal origins remain labelled; hidden thinking stays
excluded. Retention pruning is not implemented.

## Existing terminal sessions (laptop continuity)

Architects and workers opened normally on the laptop are terminal-mode
sessions: the daemon owns their PTY, native session id, hook state and the
provider's own message route. The same endpoints read and send into those
sessions — the same native conversation the laptop TUI shows — without a
second conversation owner, a takeover, a structured pane or a changed launch.
The browser client stays connect-only.

`GET /api/sessions/SESSION_ID` reports, for a local terminal session:

```json
"conversation": {"history": true, "send": true, "native_permissions": false,
  "execution_location": "local", "input_route": "codex_thread_queue",
  "limitation": "Queued in Codex's durable thread queue ..."}
```

`history` requires a known native session id (first hook event); `send` also
requires a running run (and, for OpenCode, the TUI server endpoint minted by
this daemon process). Remote SSH sessions advertise neither. `limitation` is
always the route's explicit caveat.

| `input_route` | Mechanism | Native provenance of API input |
| --- | --- | --- |
| `claude_async_rewake` | The session's asyncRewake Stop/SessionStart waiter, only while idle | `task_notification`. Claude Code wraps it for the model as a hook notification, and the TUI shows a collapsed "Stop hook feedback" entry followed by the reply. The model answered and acted on such input as the user's request in acceptance, but this is not native human input |
| `codex_thread_queue` | Durable thread queue, `clientUserMessageId` = submission id | Genuine user message, turn trigger `queue` |
| `opencode_prompt_async` | TUI server `prompt_async` with a daemon-chosen id, only while the server reports idle, under the agent/model of the latest user turn | `prompt` (same as typed input) |

None of these touch the TUI composer: a laptop draft stays intact and is
submitted when the laptop presses Enter. For Claude and OpenCode the daemon
briefly holds keystrokes to the session's main terminal around the hand-off
(re-checks idle after 300 ms, releases on the agent's working signal, at most
3 s), so a laptop Enter at the same instant runs as the next turn instead of
racing into the same idle slot. Codex needs no hold: its queue waits for the
running turn.

History is read from the native record (Claude transcript, Codex rollout
segments, OpenCode database) into the same version feed on each read and while
input is open. User items carry `via` (`terminal` typed in the TUI, `api`,
`notification` for the outbox, or `kickoff`) and the unmodified `native_origin`.
API items show the submitted text and `submission_id`; items of that native turn
carry it too. The launch message (the kickoff, with a worker's injected Overview
and workflows) is shown as recorded with `via`/`origin` `kickoff`; developer
instructions, hidden reasoning and hook plumbing are never read. An unreadable
record is an error, never an empty page.

Sends are synchronous with the same codes, replay/conflict rules and receipts
as above. Per route:

- **Claude** — refused `SESSION_BUSY` unless the agent is idle, `SEND_UNAVAILABLE`
  without an armed waiter. The daemon holds laptop keystrokes, re-checks idle
  after 300 ms, hands the input to the waiter and then waits for it in Claude's
  transcript: found → `running` (200); not found within the bound →
  `DELIVERY_UNKNOWN`. A waiter that leaves before taking it is `SEND_UNAVAILABLE`
  (nothing was sent).
- **Codex** — `thread/queue/add` answered → `accepted` (200), also while the
  agent is busy; it runs as its own native turn once idle. An error answer, or a
  failure before the request was sent, is `DELIVERY_REJECTED`; a lost response or
  timeout is `DELIVERY_UNKNOWN` (the queue and rollout are checked afterwards).
- **OpenCode** — the TUI server's own status must say idle (`SESSION_BUSY`
  otherwise; re-checked under the hold); `prompt_async` 2xx → `accepted` (200).
  A 4xx or a failure before the POST is `DELIVERY_REJECTED`; a transport
  failure or 5xx is `DELIVERY_UNKNOWN` (the database is checked afterwards).

For Claude and OpenCode, earlier input to the session (an API send or an outbox
notification) that has not yet reached the native record makes a send
`SESSION_BUSY`. One native hand-off per session is in progress at a time, shared
with the outbox dispatcher; a send that finds it taken is `SESSION_BUSY`, and the
outbox waits for a send. Notifications to the session also wait while its API
input is unsettled.

After acceptance, receipts follow native evidence: `running` once the input is
in the record, then `completed` (explicit native completion of the turn holding
it) or `failed` (interrupted/failed turn). `uncertain`, never resent: not found
in a record that moved past the hand-off after the agent went idle; the provider
process was restarted during the turn; OpenCode merged later laptop input into
the turn so its final reply belongs to that later input; or the session ended
before completion (or acceptance) was observed. An `uncertain` terminal input
does not hold later input. A daemon restart restores the TUI sessions; open
receipts are reconciled from the record.

Questions, approvals and notifications are unchanged: answer through
`/controls` operations; chat text never answers or approves. Worker
conclusions reach the architect through the outbox and appear in its history
as `via: notification`. Ended terminal sessions retain the history observed up
to a final sync at session end (see [Ended sessions](#ended-sessions)).

Known limits: Claude provenance as above; OpenCode laptop input sent while a
turn runs merges into it (native behaviour, reported as above); Codex holds
its queue after an Esc-interrupted turn until the next laptop turn completes;
a tool interrupted by a provider crash can stay `running` in the native
record; no remote SSH, streaming or authentication. Localhost is not a phone
network path. Acceptance evidence: [laptop continuity](laptop-conversation-continuity.md).
