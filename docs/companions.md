# Companions

Companions are persistent agent sessions with their own repository, started by the daemon with every new
architect session and owned by that exact session until they stop. Types: shared `domain/companion.go`
(TypeScript `domain/companion.ts`). Runtime: `internal/sessionruntime/companions*.go`, `actions.go`.

## Configuration

`hiveryn.yaml`:

```yaml
companions:
  browser:            # name: repo-key grammar; how the architect addresses it
    repo: chrome-task # a key of repos; its machine is where the companion runs
    variant: claude-opus
```

The config loader (and so the workspace check) validates the name, that `repo` is a key of `repos` and
that `variant` is present. The variant's existence, agent (claude or codex only) and machine match
(`Config.CompanionVariant`) are checked when the architect session starts its companions; a mismatch is a
named `start_failed` outcome, not a config load error. Edits apply to later architect sessions.

## Lifecycle

- **Startup.** When an architect session's run becomes live (`CreateRun`, every launch path), the daemon
  writes one `companions` record per configured entry (`starting`; the session's configuration snapshot)
  and launches each in the background through the ordinary `CreateRun` path, so local and remote
  (SSH/tmux, authenticated gateway, frozen variant snapshot, restore) are the ticket worker's own. A
  session that already has records (a relaunch after a failed run) starts none. Instructions are
  `prompts/companion/SYSTEM.md` (the approved text, byte-identical); the first message is
  `Session started: <RFC3339 UTC>` and nothing else. One `companion_startup` agent message to
  `session:<architect session>` names every outcome.
- **Failed start.** A failed launch is cleaned up: local terminals killed, remote termination confirmed
  over SSH, the session removed → `start_failed`. Cleanup that cannot be confirmed keeps the session →
  `stop_failed` (visible, blocks the conclusion).
- **Status.** `starting`, `running`, `shutdown_requested`, `stopping`, `stopped`, `stop_failed`,
  `start_failed`, `failed`. Settled (no longer blocking the conclusion): `stopped`, `start_failed`,
  `failed`.
- **Process failure.** A local companion whose process ends is resumed like any session; a failed resume
  or restore → `failed` plus a `companion_failed` message. A remote SSH disconnect is a connection event
  (reattach), never a relaunch.
- **Restart.** `ReconcileCompanions` runs before restore: `starting` with a running session → `running`,
  otherwise cleaned up → `start_failed` (and the startup report is sent if it was not); a recorded handoff
  (`completing` shutdown) is terminated instead of restored. `ReconcileActions` resolves orphaned action
  requests as `interrupted`.
- **Conclusion.** `concludeArchitectSession` (pre-check before the approval and again in its Exec) is a
  `ConflictError` while any owned companion is not settled. On conclusion the remains of settled
  companions are removed; records go with the architect session (FK cascade).

## Messaging

Durable via the agent-message outbox, addressed `session:<id>` to the exact counterpart. `accepted` =
committed outbox row (never delivery or reading). Ownership is derived from the calling session; a
reply's target is derived from the referenced message, which must be the right kind and addressed to
the caller (anything else is the same `NotFound`).

| Tool (role) | Route | Kind → recipient |
|---|---|---|
| `sendMessageToArchitect(message)` (companion) | `POST /api/sessions/{id}/companion/messages` | `companion_message` → owning architect session |
| `replyToArchitectMessage(message_id, message)` (companion) | `POST /api/sessions/{id}/companion/replies` | `companion_message` (`in_reply_to`) → architect; `message_id` must be an `architect_message` sent to this companion |
| `sendMessageToCompanion(companion, message)` (architect) | `POST /api/sessions/{id}/companions/{name}/messages` | `architect_message` → companion; only `running` companions accept assignments |
| `replyToCompanionMessage(message_id, message)` (architect) | `POST /api/sessions/{id}/companions/replies` | `architect_message` (`in_reply_to`) → the companion that sent `message_id` |

Request/response: `CompanionMessageRequest`, `CompanionReplyRequest` → `CompanionMessageResult
{message_id, in_reply_to?, status: "accepted"}`. Bodies name the `message_id` and the reply tool.
Uncommitted agent messages found at restart were never confirmed to the sender and are failed, not sent.

The outbox delivers to remote Claude (waiter through the attachment's reverse tunnel; requires a
connected attachment) and remote Codex (`codex app-server` over the session's SSH master) and confirms
receipts from the mirrored remote record (and the remote Codex queue). Remote OpenCode/chat recipients
are retained with the reason.

## requestAction

`POST /api/sessions/{id}/actions` (companion only; blocks ≤ 1 h) with `RequestActionRequest{title,
description}` → `RequestActionResult`: `outcome` `accepted` | `denied` | `replied` (+`message`), or
`error_code` + `error`, which the MCP tool returns as a tool error. Order: phone alert first (failure →
`ACTION_REQUEST_NOTIFICATION_FAILED`, nothing raised), then the durable `action_request`/`required`
session event, then the wait. Single resolution: the user's decision, expiry (`ACTION_REQUEST_TIMEOUT`,
the approved text), shutdown request / session end / call ended (`ACTION_REQUEST_CANCELLED`, the
shutdown case directs to `completeCompanionShutdown`), daemon stop or restart (`ACTION_REQUEST_INTERRUPTED`).
No auto-acceptance, no architect escalation; the id never reaches the agent.

Admission and withdrawal are decided under one lock: a shutdown request, session end, failed process or
daemon stop closes the session to new requests and withdraws the raised ones. A request whose phone alert
was still in flight when its session closed is never admitted (the call returns the withdrawal error; the
alert already went out, but no decidable request exists), and one admitted but not yet raised resolves
itself right after its `required` event, so a `resolved` event never precedes its `required` event.

Desktop: `GET /api/sessions/{id}/actions` → `{actions: ActionRequest[]}` (pending);
`POST /api/sessions/{id}/actions/{actionID}/decision` with `ActionDecisionRequest{decision:
accept|deny|reply, message (reply only)}` → the resolved `ActionRequest`. A decision on a request that
is already resolved — including the retry of a decision that won but whose response was lost — is 409
`ACTION_REQUEST_RESOLVED` whose `details.action` is the recorded request with its authoritative `status`
(`accepted`/`denied`/`replied` with `message`/`decided_at`, or `expired`/`cancelled`/`interrupted` with
`reason`); nothing is decided again. An unknown id or another session's id is `NotFound`: resolved
requests are retained in memory only briefly past their expiry and not across a restart, so a 404 means
the outcome is not known here — never that nothing was authorized. Events: `action_request` `required` (raw
`action_id, title, description, origin, created_at, expires_at`) and `resolved` (raw `action_id, status,
message?, reason?`); `companion_changed` on the architect stream.

## Shutdown

- `requestCompanionShutdown(companion, request_id)` (architect) → `POST /api/sessions/{id}/companions/shutdown`
  with `CompanionShutdownRequest`. `request_id` must be a UUID (stored canonical lower case). Records
  `companion_shutdowns` bound to the architect session and companion session and moves the companion to
  `shutdown_requested` atomically, then commits the `companion_shutdown_requested` instruction (begun
  first; a crash in between is settled against the record). Pending action requests are withdrawn.
  Same id + same target → recorded acceptance (and completes an earlier failed commit); same id, other
  target → `ConflictError`; other id for a companion already shutting down → `ConflictError` naming the
  recorded id.
- `completeCompanionShutdown(request_id, handoff)` (companion) → `POST /api/sessions/{id}/companion/shutdown-completion`.
  Only the request recorded for this companion session and its owning architect session is accepted
  (anything else `NotFound`). Records the handoff (`completing`, companion `stopping`) and returns
  `accepted`. The stop waits for the acknowledgement of that very completion: the successful response
  for the bound `request_id`, written and flushed to the last transport the daemon observes, for at most
  `companionAckWait` (30 s), then `companionStopGrace` (2 s) for the hop it cannot observe (tunnel or
  stdio pipe to the agent). Remote: the per-session gateway records, per `/mcp` HTTP request, the
  `request_id` of a completion the daemon accepted while serving it (request-context recorder in
  `internal/mcp`), and releases the stop only after that request's 200 response was written and flushed
  without a write error while its caller was still connected. Local: the API handler releases it after
  the successful completion response to the stdio MCP server was flushed — this proves only the API →
  stdio-MCP hop. Other requests of the same companion (another tool call, `tools/list`), error responses,
  a refused or foreign `request_id`, or the other transport's hop release nothing. If the transport
  disappears (caller disconnected, write failed) nothing is acknowledged: after the bounded wait the stop
  proceeds and both the log and the architect's `companion_shutdown_outcome` say the acknowledgement was
  not observed written, so the companion may not have received it. It then ends the session (remote
  termination confirmed first) and sends `companion_shutdown_outcome` (request id, handoff, actual
  outcome) to the architect. Limits: receipt by the agent itself is not observable, and a response
  written into a connection whose peer vanished before the server noticed can still count as written;
  after a daemon
  restart a recorded handoff is terminated without waiting, and the outcome says whether its
  acknowledgement arrived is not known.
  Same id + handoff → recorded acceptance; changed handoff → `ConflictError`.
- Unconfirmed termination → `stop_failed` (session kept, conclusion blocked, outcome message says so).
  The only further route is the user's `POST /api/sessions/{id}/discard` on that companion session, which
  retries confirmed termination (also removes a `failed` companion); a running companion is never
  discarded. No automatic force-stop.

## Desktop contract

- `GET /api/architects/{key}/companions` → `CompanionList{architect_session_id, companions}` for the
  running architect session; each `Companion` carries the record plus `shutdown`, `main_terminal_id`,
  `connection`, `agent_status`, `pending_actions`. `GET /api/architects/status` includes the same
  `companions` per architect; companion sessions are not listed among worker `sessions`.
- Companion sessions are ordinary sessions (`session_type: companion`, `context_id` = name,
  `owner_session_id`): terminals, tabs, events, terminal workdirs (companion repo first) and the
  conversation API work by session id. `session_started`/`session_ended` and `companion_changed` are
  published on the architect stream. Their right-pane tabs come from the `companion` entry of
  `tabs.yaml`, defaulting to the activity log (`event-log`); the desktop shows no session without a tab.

## MCP

Companion servers (stdio `HIVERYN_SESSION_TYPE=companion`, or the per-session remote gateway) register
exactly `sendMessageToArchitect`, `replyToArchitectMessage`, `requestAction`,
`completeCompanionShutdown`. Architects additionally get `sendMessageToCompanion`,
`replyToCompanionMessage`, `requestCompanionShutdown` and read-only `readContext`/`listContext` (so a
remote companion repository's AGENTS.md is readable as `<repoKey>:AGENTS.md`). There is no
`listCompanions`. Remote companions get no remote-worker instruction suffix: the approved text is used
unchanged.
