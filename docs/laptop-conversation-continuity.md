# Laptop ↔ phone continuity for existing Hiveryn sessions

27 October 2026, ticket `2026-10-27-0143-implement-existing-hiveryn-session-chat-continuity`
(supersedes the partial result of `2026-10-27-0125`).

**Delivered:** architects and workers opened normally in Hiveryn on the laptop
can be selected in the connect-only browser client, read, and continued in the
**same native conversation**; the laptop TUI then continues with the combined
context. Claude, Codex and OpenCode, both roles. No desktop change, no launch
change, no second conversation owner, no phone-created sessions. The API
contract is in [conversation-api.md](conversation-api.md#existing-terminal-sessions-laptop-continuity).

## Design

The daemon already owns each terminal session's PTY, native identity, hook
state and the provider's native message route (used by the agent-message
outbox). The conversation API uses exactly those:

- **History**: agentruntime `HistoryReader` reads the provider's own record
  read-only (Claude transcript, Codex rollout segments, OpenCode database) into
  the existing version feed. Inputs are classified by the daemon: typed in the
  TUI (`via: terminal`), API submission (`api`, correlated by Codex
  `client_id`, OpenCode message id or a header Claude carries in its hook text),
  outbox notification, or the launch message (`kickoff`, shown verbatim as
  recorded, injected Overview/workflows included). The provider's own
  provenance is kept unmodified in `native_origin`. Each read is written in
  record order, so display order follows the record: an item first seen late
  (an earlier daemon hid the kickoff) moves before its replies on the next read
  (`docs/conversation-api.md`, Order).
- **Send**: synchronous delivery through the provider route (28 October, below),
  one hand-off per session shared with the outbox, reservation before hand-off,
  completion only from the native turn end of the turn holding that input,
  never an automatic resend.
- **Coordination**: the routes never touch the TUI composer (drafts survive).
  Claude/OpenCode hand-offs happen only at idle (Claude waiter + Stop; OpenCode
  `GET /session/status`) under a ≤3 s hold of laptop keystrokes to that
  terminal; Codex's durable queue waits for the running turn itself.

### Decision: Claude provenance

Claude Code offers no local route for genuine typed-human input into a running
TUI. asyncRewake input is recorded as `turnOrigin: task_notification` and shown
to the model inside a "[SYSTEM NOTIFICATION - NOT USER INPUT]" wrapper; the TUI
shows "Stop hook feedback". Typing through the PTY with Claude's Ctrl+S prompt
stash would give `human` provenance but needs TUI modal/stash state the daemon
cannot verify. The architect (under Kareem's delegation) chose asyncRewake with
honest labelling (`via: api`, `native_origin: task_notification`) and no
PTY/stash injection. Acceptance showed the model answering and acting on these
inputs as the user's (conversation recall, authoring a backlog ticket, asking a
Hiveryn question first); that is evidence, not a guarantee. Approvals never go
through chat text.

## Acceptance (isolated, native)

Isolated daemon (built from this checkout), the **unchanged desktop** built
from its `scripts/isolated-workspace.config.ts` entry and driven through its
real launcher, ticket Spawn flow and xterm over CDP, and the chat-ui-poc in
Chromium at 390×844. Providers: Claude Code 2.1.311 (Sonnet), Codex 0.164.0
(gpt-5.6-sol), OpenCode 1.19.4 (DeepSeek), read-only credential copies,
isolated config roots outside `$HOME`. Results:

| Check | Claude | Codex | OpenCode |
| --- | --- | --- | --- |
| Architect launched from desktop launcher: laptop → phone → laptop recall | pass | pass | pass |
| Worker spawned from desktop ticket: laptop → phone → laptop recall | pass | pass | pass |
| Phone refresh keeps history and receipts | pass | pass | pass |
| Nonempty laptop draft during phone send: draft intact, then submitted | pass | pass | pass |
| Phone input while laptop turn busy: waits, then own turn | pass | pass (native queue) | pass |
| Laptop and phone input at the same idle moment: both own turns | pass (hold) | pass | pass (hold, 2 runs) |
| Laptop input during a phone turn | native `queued_command` in turn; completed | steered into turn; completed | merged by OpenCode → `uncertain`, explained |
| Hiveryn question answered from phone; conclusion approved from phone | pass | pass | pass |
| Worker conclusion notification in architect history (`via: notification`) | pass (`task_notification`) | pass (`queue`) | pass (`prompt`) |
| Daemon restart: in-flight receipt reconciled, nothing resent | pass | — | pass |
| Provider process killed mid-turn: same native session resumed, cut turn `uncertain`, next input continues with context | resumed, context kept | — | pass |

Found and fixed during acceptance: OpenCode `prompt_async` ran under the
default agent instead of the architect's (now carries the latest user turn's
agent/model); Claude prefixes hook text with its own label (correlation and
notification matching tolerate it); the dispatcher must use the in-memory live
main terminal (the hold and restart detection were inert with the SQLite row);
chat-style teardown had marked handed terminal input `failed`.

Evidence (screenshots, histories, receipts, log) is retained under ignored
`chat-ui-poc/runs/2026-10-27-0143-terminal-continuity/`, with a
credential-free summary in [evidence/terminal-continuity-2026-10-27.json](evidence/terminal-continuity-2026-10-27.json).
Lab tooling: `scripts/continuity_lab.py`.

Side effect: the first lab OpenCode TUI ran OpenCode's default auto-update and
replaced the global binary 1.17.18 → 1.19.4 (Kareem notified; Kareem's config
has no `autoupdate` setting, so his own sessions would have done the same). The
lab now sets `OPENCODE_DISABLE_AUTOUPDATE`.

## 28 October: synchronous delivery

Ticket `2026-10-28-0115-make-chat-message-delivery-synchronous-without-a-hiveryn-queue`
replaced the Hiveryn-side queue: a send is now answered with provider
acceptance, a definitive refusal (for example `SESSION_BUSY`) or
`DELIVERY_UNKNOWN` ([contract](conversation-api.md#send-synchronous-delivery)).
This supersedes the "phone input while laptop turn busy: waits, then own turn"
rows above for Claude and OpenCode: a busy send is now refused at once and never
delivered later; Codex still accepts it into its own native queue.

Isolated native lab (`scripts/continuity_lab.py`, Claude Code 2.1.312, Codex
0.164.0, OpenCode 1.19.4), terminal-mode architects, laptop input through the
desktop terminal WebSocket:

| Check | Claude | Codex | OpenCode |
| --- | --- | --- | --- |
| Send while the kickoff runs | 409 `SESSION_BUSY`, 0.0 s | 200 `codex_thread_queue`, 0.09 s, then completed | 409 `SESSION_BUSY`, 0.06 s |
| Idle send of a long task | 200 `claude_native_record`, 0.64 s | 200 `codex_thread_queue`, 0.08 s | 200 `opencode_prompt_async`, 0.33 s |
| That turn completed (from the POST) | 4.7 s | 7.1 s | 4.4 s |
| Send while that turn runs | 409 `SESSION_BUSY` | 200, run as the next native turn | 409 `SESSION_BUSY` |
| Refused send after idle | never in history | — | never in history |
| Laptop context recalled by the API turn, and vice versa | pass | pass | pass |
| Daemon restart: prior request IDs replay, nothing resent; fresh send accepted | pass | pass (after the restored TUI's own update prompt was dismissed) | pass |

Summary: [evidence/synchronous-delivery-2026-10-28.json](evidence/synchronous-delivery-2026-10-28.json).
Ambiguous hand-offs, lost responses, duplicate/conflicting request IDs,
competing notifications, teardown and restart are covered deterministically
(Go tests and `scripts/conversation-smoke.py`); no real provider was forced
into an ambiguous hand-off.

## Not covered

Physical-phone networking (localhost only), remote SSH sessions, streaming,
authentication, native permission prompts, every provider profile. A hands-on
demo needs a visible isolated desktop; the desktop's isolated verification
entry is hidden by design.
