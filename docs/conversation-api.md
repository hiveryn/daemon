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
internal queue entry; user input follows it. A worker is moved to progress before
that queue starts executing, so its readTicket/conclusion calls see correct state.

Existing `GET /api/sessions` and `GET /api/sessions/SESSION_ID` include:

```json
{
  "interaction_mode": "chat",
  "conversation": {
    "history": true,
    "send": true,
    "native_permissions": false,
    "execution_location": "local",
    "limitation": "Native permission controls are unavailable; permission requests block the queue"
  }
}
```

`send` requires a running local chat session. It describes admission to the
queue, not an assurance that the provider can currently complete work. Inspect
submission receipts for waits/errors. Existing terminal/question/intent desktop
flows remain; chat sessions have a labelled status placeholder and session event
subscription, without a fake main terminal or a desktop chat UI.

## Submit and poll

```http
POST /api/sessions/SESSION_ID/messages
Content-Type: application/json

{"request_id":"client-unique-123","text":"Explain the fixture."}
```

HTTP 202 returns a durable receipt with `id`, `session_id`, `request_id`,
`sequence`, `origin`, `text`, `status`, `created_at`, `updated_at`, and optional
`native_turn_id`/`error`. Poll
`GET /api/sessions/SESSION_ID/submissions/SUBMISSION_ID`.

- `queued`: accepted and waiting its turn.
- `started`: dispatch reserved durably before handoff; native execution may have begun.
- `blocked`: native permission/request requires a control unavailable in V1.
- `uncertain`: delivery/completion cannot be established. No automatic replay.
- `completed`: the matching native turn completed. Individual tool items may still have failed.
- `failed`: explicit native turn failure, or session ended before completion.

Exact same request ID/text replays the existing receipt, including after session
conclusion/discard. Reuse with different text/origin is HTTP 409. Request IDs are
1–200 characters; text is nonblank and at most 1 MiB. Concurrent callers are
ordered by SQLite admission sequence, not client clocks. A single owner dispatches
one native turn at a time. Closing an HTTP connection never cancels an accepted
submission. If an earlier entry is blocked/uncertain, queued receipts report that
entry's ID and reason in `error`; clients may poll it too. Internal kickoff text
is redacted in receipt reads.

This is bounded evidence, not exactly-once execution: a crash can happen after
native acceptance but before a native turn ID or result is persisted. On resume,
explicit native turn/parent/user-UUID lineage is consulted before settling the
in-flight entry. Inconclusive results remain uncertain and hold the queue. No
resubmit/interrupt/force-resolve API is provided in V1. Discard/conclude through
the existing lifecycle operations when the session cannot continue safely.

## Read history and old-item updates

```http
GET /api/sessions/SESSION_ID/conversation?after_revision=0&limit=100
```

The page contains `items`, `snapshot_revision`, `next_revision`, `has_more`.
Each item has stable `id`, `sequence`, `revision`, `submission_id`, native turn ID
when known, `role`, `origin`, `status`, available timestamp, text and tool name.
Roles are `user`, `assistant`, `tool`, `system`; origins are `user`, `kickoff`,
`notification`. Native SDK user messages are not relabelled as terminal human
input. Internal notifications are explicitly labelled even when the provider's
structured transport represents their input as a user message.

This endpoint is an append-only **version feed**, not a list of just new messages:
upsert items by `id`, retain the greatest revision, display by `sequence`.
To continue a page, send `after_revision=next_revision` and the same
`snapshot_revision`. Once `has_more` is false, the next poll starts at that
snapshot revision and omits `snapshot_revision`. A change to an old item is a
new revision. Provider compaction/resume never removes observed history. Page
size is at most 200. Already-observed history and submission receipts survive
session deletion; unknown/unsupported session history returns an explicit error.
Retention pruning is not implemented in V1.

Only visible message text/tool output is normalized. Hidden reasoning, raw
provider payloads, launch instructions and kickoff user echoes are excluded.
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
outbox notifications join the same durable chat queue under a distinct origin;
the outbox reservation names that receipt before admission. Completed native
turn evidence acknowledges delivery, not task review.

Native permission UI is deferred. Policies remain configured: Claude stdio
control requests, Codex server approval requests and OpenCode permission waits
surface as blocked without being answered. Unsupported profile flags are errors,
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
discard. Fixture success is not native acceptance. See
[the native acceptance record](conversation-api-verification.md) for installed
provider evidence and limitations. No UI build, live restart, package release or
deployment is part of this delivery.

## Cross-client receipt discovery and archived chats

`GET /api/sessions/SESSION_ID/submissions?after_sequence=0&limit=100`
returns `{submissions, next_sequence, has_more}`. Follow `next_sequence` while
`has_more` is true. Sequence is stable admission order (gaps are normal). Limits
are 1–200; omitted/zero uses 100. Negative cursors and limits over 200 are rejected.
This is a live receipt list, not a revision snapshot: restart at zero to refresh
old statuses, or poll known receipt IDs. New clients discover the same queue
without browser storage. Kickoff text is redacted and queued entries include the
blocking/uncertain head's reason. Discovering a receipt never sends or replays it.
Queued admission is not evidence of native execution or completion.

`GET /api/archived-chats?architect_key=PROJECT&after_sequence=0&limit=100`
returns `{chats, next_sequence, has_more}` in archive insertion order with the
same bounds. The optional project filter scopes discovery. Each descriptor has
`id` (the former session ID), `sequence`, `architect_key`, `session_type`,
`context_id`, `agent`, optional `model`, `profile_name`, `created_at`, `ended_at`
and `lifecycle` (`concluded`, `discarded`, or `interrupted` for administrative
termination). It contains no prompt, instructions, launch environment or native
credentials. Use its ID with the existing history and receipt endpoints.
Archives are read-only, cannot launch or admit new input, and do not participate
in ordinary active-session discovery or ownership. Exact prior request-ID replay
still returns its receipt; it does not dispatch again.

Archive descriptors, unfinished receipt disposition and final item revisions
are committed together before active records are deleted. Unfinished tool/permission
items become `interrupted`; their names/output remain. Existing native errors and
successful results remain unchanged. Late native output cannot resurrect archived
activity. A conclusion tool interrupted by its own session end is not claimed to
have returned native success. Tool names are retained across result revisions.
Claude string and text-block user echoes share stable UUID-based IDs; internal
origins remain labelled and kickoff echoes/hidden thinking stay excluded.

Migration 0018 adds retained archive discovery. Sessions already deleted before
that migration cannot be reliably backfilled: their metadata is gone. Their
previously observed history remains readable by known ID. No retention pruning
or new active-session ownership rules are introduced.

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
input is open. User items carry `via` (`terminal` typed in the TUI, `api`, or
`notification` for the outbox) and the unmodified `native_origin`. API items
show the submitted text and `submission_id`; items of that native turn carry
it too. The launch prompt (role/kickoff instructions, injected Overview and
workflows) is hidden; developer instructions, hidden reasoning and hook
plumbing are never read. An unreadable record is an error, never an empty
page.

Submissions use the same request-id replay/conflict rules and receipts, with
these terminal meanings:

- `queued` — waiting; `error` says why (agent busy, no Claude waiter armed yet,
  previous input not yet in the record, session not running).
- `started` — reserved durably (provider reference, hand-off time, receiving
  terminal) before the hand-off; `error` says whether it is queued natively or
  running in the native conversation.
- `completed` — explicit native completion of the turn containing the input.
- `failed` — native turn interrupted/failed, or the session ended before the
  hand-off.
- `uncertain` — never resent automatically: not found in a record that moved
  past the hand-off after the agent went idle; the provider process was
  restarted during the turn (the resumed process never completes it);
  OpenCode merged later laptop input into the turn so its final reply belongs
  to that later input; or the session ended before completion was observed.

Unlike the chat queue, an `uncertain` terminal input does not hold later
input (the laptop is not held either). One hand-off per session is in flight
at a time, shared with outbox notifications. A daemon restart restores the
TUI sessions; open receipts are reconciled from the record, and Claude/OpenCode
gates come from the resumed waiter and the server's own status.

Questions, approvals and notifications are unchanged: answer through
`/controls` operations; chat text never answers or approves. Worker
conclusions reach the architect through the outbox and appear in its history
as `via: notification`. Ended terminal sessions get an archive descriptor
(`interaction_mode: "terminal"`) and retain the history observed up to a
final sync at session end.

Known limits: Claude provenance as above; OpenCode laptop input sent while a
turn runs merges into it (native behaviour, reported as above); Codex holds
its queue after an Esc-interrupted turn until the next laptop turn completes;
a tool interrupted by a provider crash can stay `running` in the native
record; no remote SSH, streaming or authentication. Localhost is not a phone
network path. Acceptance evidence: [laptop continuity](laptop-conversation-continuity.md).
