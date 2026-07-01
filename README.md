# Hiveryn Daemon

Local daemon for [Hiveryn](https://github.com/hiveryn) — an AI-native software engineering environment.

The daemon runs in the background as a macOS Launch Agent, independent of the desktop app. The desktop app registers it on first launch; the daemon stays alive across app restarts.

## Running (development)

```bash
go install github.com/hiveryn/daemon/cmd/hiverynd@latest
hiverynd
```

Or from source:

```bash
git clone https://github.com/hiveryn/daemon.git
cd daemon
go run ./cmd/hiverynd
```

Pass `--port` to override the listen port at launch (takes precedence over `config.yaml`):

```bash
hiverynd serve --port 4202
```

Set `HIVERYN_HOME` to move daemon-owned local runtime state under a different root, and `HIVERYN_ENV` to label the runtime mode exposed by `/api/system/runtime`:

```bash
HIVERYN_HOME=~/.hiveryn-dev HIVERYN_ENV=development hiverynd serve --port 4202
```

The daemon binary also exposes an MCP stdio subcommand for agent-launched tool access:

```bash
hiverynd mcp --daemon-url http://127.0.0.1:4201 --architect-key hiveryn
```

`hiverynd mcp` is intended to be spawned by `agentruntime`; session scoping comes from `HIVERYN_SESSION_TYPE` (`architect`, `ticket`, or `freeform`).

## Configuration

The daemon reads five YAML files from `HIVERYN_HOME` (default `~/.hiveryn`). Only `config.yaml` is required; the others default to empty when missing. `variants.yaml`, `architects.yaml`, `tabs.yaml`, and `shortcuts.yaml` are reloaded on demand, so changes to profiles, the architect registry, tab layouts, and keybindings do not require a daemon restart. The per-architect `hiveryn.yaml` files those entries point at are reloaded the same way. Passing `--config` points `config.yaml` elsewhere and, because config loading is directory-scoped, also changes where `variants.yaml`, `architects.yaml`, `tabs.yaml`, and `shortcuts.yaml` are read from.

### `config.yaml` — daemon core

```yaml
shell: /bin/zsh
port: 4201
bind_address: 127.0.0.1
log_level: info
conclusion_approval_timeout: 20
```

| Field | Default |
|---|---|
| `shell` | `$SHELL`, then `bash` |
| `port` | `4201` |
| `bind_address` | `127.0.0.1` (localhost only) |
| `log_level` | `info` |
| `conclusion_approval_timeout` | `20` (seconds; 0 = infinite, auto-approves on timeout) |

### `variants.yaml` — agent variants

```yaml
claude-sonnet:
  agent: claude
  args: [--model, claude-sonnet-4-6]
codex-personal:
  agent: codex
  args: [--dangerously-bypass-approvals-and-sandbox]
  env:
    CODEX_HOME: /Users/kareem/.codex-personal
deep-personal:
  agent: opencode
  args: [--model, gpt-5]
```

A variant may declare its own MCP servers under `mcp_servers` (keyed by server name). Those servers are attached only to sessions launched for that variant, merged after the base `hiveryn-daemon` server that every session always receives. Each entry mirrors the agent MCP server fields — set exactly one of `command` (stdio) or `url` (HTTP); `args`, `env`, `cwd`, and `bearer_token_env_var` are optional. The name `hiveryn-daemon` is reserved. The frozen set is restored when a session is resumed after a daemon restart.

```yaml
claude-sonnet-plan:
  agent: claude
  args: [--model, claude-sonnet-4-6]
  mcp_servers:
    sentrux:
      command: sentrux
      args: [--mcp]
```

For architect sessions using `agent: opencode`, the daemon defines a named OpenCode agent automatically from `prompts/architect/SYSTEM.md`, using the architect key as the agent name and passing `--agent <architect_key>` at launch. Do not put `--agent` in OpenCode architect variant args; the daemon treats that as a launch error. Ticket and freeform OpenCode sessions do not define a named agent.

### `architects.yaml` — architect registry

A bare `key: path` map. Each value points at an architect workspace directory containing a `hiveryn.yaml`; the architect's name, repos, and prompts are read from there.

```yaml
hiveryn: /Users/kareem/architects/hiveryn
litho: /Users/kareem/architects/litho
```

### `hiveryn.yaml` — per-architect configuration

Lives at the root of each architect workspace. The `key` from `architects.yaml` remains the identifier used in URLs and session records; `name` is a display label.

```yaml
name: Hiveryn
repos:
  daemon:  /Users/kareem/hiveryn/daemon
  desktop: /Users/kareem/hiveryn/desktop
prompts:                                       # optional
  architect:                                   # optional
    system: prompts/architect/SYSTEM.md        # optional path
    kickoff: prompts/architect/KICKOFF.md      # optional path
  ticket:                                      # optional
    kickoffs:                                  # optional; default is the built-in kickoff
      - { path: prompts/work/KICKOFF.md }              # default entry (no repos)
      - { path: prompts/work/DAEMON.md, repos: [daemon] }  # repo-scoped
```

Repo paths may start with `~` or `~/` to reference the user's home directory; they are expanded to absolute paths at config load. Absolute and relative paths keep working as before.

Prompt paths resolve relative to the workspace directory (absolute paths are used as-is) and override the daemon's embedded defaults; omit a field to keep the built-in prompt.

Ticket-kickoff selection: the entry whose `repos` contains the ticket's repo wins over the default (no-`repos`) entry — most specific wins, regardless of list order. When `kickoffs` is absent, the embedded default is used. Configuration fails to load if any kickoff references a repo key not declared under `repos`, if two entries are both default, or if a repo key appears in more than one entry.

The architect can manage this file through MCP tools (`listRepos`/`addRepo`/`removeRepo`, `listKickoffs`/`addKickoff`/`updateKickoff`/`removeKickoff`, `getArchitectPrompts`/`setArchitectSystem`/`setArchitectKickoff`, `describePromptSchema`) instead of editing it by hand. Those tools enforce the rules above and scaffold the embedded default template when wiring a prompt path whose file does not exist yet.

### `tabs.yaml` — tab layout per session type

```yaml
architect:
  - type: kanban
  - type: event-log
  - type: terminal
    command: lazygit
  - type: terminal

ticket:
  - type: event-log
  - type: terminal

freeform:
  - type: event-log
  - type: terminal
```

Terminal entries only support `type` and optional `command`. Entries without `command` default to the user's shell. When a session run starts, the daemon auto-creates PTY terminals for every `type: terminal` entry in the matching session type section and assigns each terminal a UUID.

`tabs.yaml` also accepts pluggable tab types (e.g. `type: git-diff`). These are declarative (no `command`); the daemon calls `Init` on spawn and `Close` on session end for registered plugins, and exposes `POST /api/sessions/{id}/plugins/call` for RPC. Unknown types at call time return a daemon 404 envelope; plugin errors are returned inside the strict plugin envelope at 200. See `internal/plugin` and the `tabplugin` contract repo.

### `shortcuts.yaml` — keybindings

```yaml
global:
  focus-left:    "Cmd+Shift+h"
  focus-right:   "Cmd+Shift+l"
  focus-down:    "Cmd+Shift+j"
  focus-up:      "Cmd+Shift+k"
  focus-main:    "Cmd+1"
  first-session: "Cmd+Shift+0"
  prev-session:  "Cmd+Shift+["
  next-session:  "Cmd+Shift+]"
  close-tab:     "Cmd+w"
  new-terminal:  "Cmd+t"
  quit:          "q"

kanban:
  left:    "h"
  right:   "l"
  down:    "j"
  up:      "k"
  open:    "o"
  spawn:   "s"
  refresh: "r"

event-log:
  down: "j"
  up:   "k"
  open: "o"
  copy: "c"
```

Maps are two-level: top-level keys are sections (`global`, `kanban`, `event-log`, etc.), each containing `action: keybinding` pairs. Missing sections or actions fall back to hardcoded daemon defaults. Keybinding string format is the desktop's concern.

## Data

Local runtime state is stored at `HIVERYN_HOME/daemon.db` by default and managed by the daemon through migrations. Schema changes should be written as new migration files in `internal/store/migrations/`. Variants, architects, repo mappings, and tab layouts live in `HIVERYN_HOME/*.yaml` by default. Your architect workspace (tickets, conclusions) is stored separately as markdown files and is never affected. `--db` overrides the SQLite path explicitly.

The daemon also writes append-only structured JSONL logs to `HIVERYN_HOME/logs/daemon.jsonl` and `HIVERYN_HOME/logs/requests.jsonl` by default. `daemon.jsonl` contains app/runtime logs with source location metadata; `requests.jsonl` contains one JSON object per HTTP request/response, including the response envelope for JSON API calls.

## API

| Method | Path | Description |
|---|---|---|
| `GET` | `/api/system/runtime` | Get resolved daemon runtime identity and paths |
| `GET` | `/api/health` | Health check |
| `GET` | `/api/agent-profiles` | List all agent profiles |
| `GET` | `/api/agent-profiles/{name}` | Get one agent profile by name |
| `GET` | `/api/architects` | List configured architects |
| `GET` | `/api/architects/status` | List all configured architects plus their running architect status and nested running ticket/freeform worker sessions for desktop command-palette/session pickers |
| `GET` | `/api/architects/{key}` | Get one configured architect by key |
| `GET` | `/api/architects/{key}/tickets` | List ticket board columns; supports `?status=backlog\|progress\|done` and `?limit=N` |
| `POST` | `/api/architects/{key}/tickets` | Create a backlog ticket in the architect folder |
| `GET` | `/api/architects/{key}/tickets/{id}` | Get one filesystem-backed ticket by ID |
| `PATCH` | `/api/architects/{key}/tickets/{id}` | Apply a targeted body edit to a backlog ticket |
| `PATCH` | `/api/architects/{key}/tickets/{id}/metadata` | Update frontmatter (`title`, `repo`, `references`) for a backlog ticket |
| `DELETE` | `/api/architects/{key}/tickets/{id}` | Delete a backlog ticket folder and its contents |
| `POST` | `/api/architects/{key}/tickets/{id}/move?to=...` | Move a ticket between backlog, progress, and done |
| `POST` | `/api/architects/{key}/tickets/{id}/move-to-done` | Architect-driven ticket completion without a worker session: backlog → done (architect resolved it directly) or progress → done (manually closing a dead/stuck worker session — fails with `CONFLICT` if a worker session is currently running). Writes a `conclusion.md`; requires `commits` unless `rejected=true` with a `rejection_reason`. Called by the MCP `moveTicketToDone` tool. |
| `GET` | `/api/architects/{key}/events` | Stream architect-scoped workspace_changed SSE hints |
| `GET` | `/api/architects/{key}/conclusions` | List recent conclusions (IDs + timestamps); supports `?limit=N` |
| `GET` | `/api/architects/{key}/conclusions/recent` | Read the most recent architect session conclusion |
| `GET` | `/api/architects/{key}/conclusions/{id}` | Read a conclusion by ID |
| `GET` | `/api/architects/{key}/repos` | List repos for an architect |
| `GET` | `/api/architects/{key}/repos/{repoKey}` | Get one architect repo by key |
| `GET` | `/api/architects/{key}/config/repos` | List the architect's `hiveryn.yaml` repos (key + absolute path) |
| `POST` | `/api/architects/{key}/config/repos` | Add a repo to `hiveryn.yaml`; `CONFLICT` if the key already exists |
| `DELETE` | `/api/architects/{key}/config/repos/{repoKey}` | Remove a repo; `CONFLICT` if still referenced by a kickoff entry |
| `GET` | `/api/architects/{key}/config/kickoffs` | List ticket-kickoff entries (path, repo scope, which is default) |
| `POST` | `/api/architects/{key}/config/kickoffs` | Add a ticket-kickoff entry (empty `repos` = default); scaffolds the embedded default when the file is missing |
| `PUT` | `/api/architects/{key}/config/kickoffs` | Re-scope an existing ticket-kickoff entry (by `path`) |
| `DELETE` | `/api/architects/{key}/config/kickoffs` | Remove a ticket-kickoff entry (by `path`); returns repos that fall back to the default |
| `GET` | `/api/architects/{key}/config/architect-prompts` | Get the architect system/kickoff prompt paths (`null` when unset) |
| `PUT` | `/api/architects/{key}/config/architect-prompts/system` | Set the architect system prompt path; scaffolds the embedded default when missing |
| `PUT` | `/api/architects/{key}/config/architect-prompts/kickoff` | Set the architect kickoff prompt path; scaffolds the embedded default when missing |
| `GET` | `/api/architects/{key}/config/prompt-schema?kind=architect\|ticket` | Describe the Go template variables available to a prompt kind |
| `GET` | `/api/config/shortcuts` | Get resolved shortcuts config (global + per-pane keybindings) |
| `POST` | `/api/sessions` | Create a durable session intent for architect planning, ticket work, or freeform exploration |
| `GET` | `/api/sessions` | List session intents with their current run, if any |
| `GET` | `/api/sessions/{id}` | Get one session intent |
| `POST` | `/api/sessions/{id}/runs` | Start a run for a session intent using an agent profile; ticket runs move the ticket backlog → progress on successful launch |
| `POST` | `/api/sessions/{id}/conclude` | Conclude an intent's running run, publish `ended`, kill its PTYs, and delete the intent row; architect conclude returns `CONFLICT` if same-architect ticket sessions are still running |
| `POST` | `/api/sessions/{id}/discard` | Discard a ticket session without writing a conclusion: move the ticket progress → backlog, publish `ended` with `raw.lifecycle=discarded`, kill PTYs, delete the run, and delete the intent row |
| `POST` | `/api/sessions/{id}/request-conclusion` | Request conclusion approval (blocking). Stores pending approval, publishes `approval_required` SSE event, blocks until approved, rejected, or timeout (auto-approves). Called by the MCP `concludeSession` tool. |
| `POST` | `/api/sessions/{id}/approve-conclusion` | Approve a pending conclusion request and run the conclusion. Called by the desktop app. |
| `POST` | `/api/sessions/{id}/reject-conclusion` | Reject a pending conclusion request with a reason. Returns the reason as a validation error to the blocked `request-conclusion` caller so the agent can retry. |
| `GET` | `/api/sessions/{id}/tabs` | Get the resolved right-pane tab layout for a session intent's current run |
| `POST` | `/api/sessions/{id}/plugins/call` | Call a pluggable tab function: body `{"type":"...","fn":"...","args":{...}}`; returns the strict plugin envelope (200 even if plugin sets inner error); 4xx/5xx only on dispatch failure |
| `GET` | `/api/sessions/{id}/ticket` | Get the associated ticket for a ticket session (returns NOT_FOUND for non-ticket sessions) |
| `POST` | `/api/sessions/{id}/terminals` | Create a new user terminal in a session intent's current run using the resolved default shell |
| `GET` | `/api/sessions/{id}/terminals` | List all terminals for a session intent's current run |
| `DELETE` | `/api/sessions/{id}/terminals/{uuid}` | Kill a specific user terminal by UUID |
| `GET` | `/api/sessions/{id}/events` | Stream structured session intent events over SSE |
| `WS` | `/ws/session/{id}/terminal/{uuid}` | Stream PTY output and send terminal input for a terminal UUID |

The `HIVERYN_HOME`-level config endpoints (profiles, architect registry, tabs, shortcuts) are read-only — edit `HIVERYN_HOME/*.yaml` directly unless you launched with `--config`. The per-architect `hiveryn.yaml`, however, is editable through the `/api/architects/{key}/config/...` endpoints above (surfaced to the architect as MCP tools), which own the yaml wiring, path resolution, and validation: every write is validated against the same rules the loader enforces and refused if it would fail to load. Changes in `variants.yaml`, `architects.yaml`, `tabs.yaml`, `shortcuts.yaml`, and each `hiveryn.yaml` apply without restarting the daemon.

Ticket mutations are status-gated: backlog tickets can be edited, metadata-updated, moved, or deleted; backlog or progress tickets can be concluded (worker conclusion still requires progress; architect-driven `move-to-done` accepts either, and rejects `progress → done` if a worker session is currently running); done tickets are read-only.

Session responses expose a durable intent plus its current run, if any. Each intent stores the create-time session contract: `id`, `architect_key`, `session_type`, `context_id`, `prompt`, `workdir`, optional `instructions`, and lifecycle metadata. `POST /api/sessions/{id}/runs` returns the created `run`, its `main_terminal_id`, and a `ws_url` so the desktop can attach immediately. Launch uses the stored `workdir` directly; ticket and architect workdirs are resolved during intent creation, not recalculated later. If the main agent PTY exits unexpectedly, the daemon automatically resumes the running run from its stored `native_id` (or id-less resume — a session picker — when `native_id` is empty), emits a `main_terminal_resumed` session event with the new `main_terminal_id`, and leaves the run status as `running`. Whenever a run's mapped agent status transitions (`active`/`idle`/`waiting`/`stopped`), the daemon emits a `{ type: "agent_status", status }` session event (emit-on-change only, persisted so it replays in the connect backlog) — distinct from the overloaded `type=status` events — letting the desktop drive a per-session tab icon without polling. A failed resume — on startup restore or after a PTY exit — marks the run failed (`restore_failed` on restore, `launch_failed` on PTY-exit resume), logs the error at ERROR level, and for ticket sessions moves the ticket back to `backlog`; it never crashes the daemon, and startup continues regardless of individual failures. `GET /api/sessions/{id}/tabs` returns the canonical right-pane layout using `type`, `id`, `command`, and `status` for terminal tabs. `POST /api/sessions/{id}/terminals` accepts an empty JSON object and always launches the session's default shell in the current run workdir.

The legitimate explicit session ends are `POST /api/sessions/{id}/conclude` and, for ticket sessions only, `POST /api/sessions/{id}/discard`. Concluding an architect intent fails fast with `CONFLICT` while ticket intents for the same `architect_key` still have a `running` run; the error message lists the blocking intent IDs.

### Discard ticket session

`POST /api/sessions/{id}/discard` is for the desktop "discard worker session" action. It accepts no request body. The target session must be a ticket session with a current run; architect and freeform sessions return a `VALIDATION` envelope. The daemon moves the ticket back to `backlog`, emits a live session SSE event with `type=status`, `status=ended`, `message=session discarded`, and `raw.lifecycle=discarded`, kills all PTYs/plugins for the session, deletes the `session_runs` row, and deletes the `session_intents` row. No `conclusion.md` is written and no commit/rejection invariant is checked. Git changes made by the agent are not reverted.

Successful response:

```json
{
  "data": {
    "success": true,
    "session_id": "intent-uuid",
    "ticket_id": "ticket-id"
  },
  "error": null,
  "logs": [],
  "commands": [],
  "meta": {
    "request_id": "..."
  }
}
```

Desktop consumers should remove the session tab either when the POST succeeds or when they receive the `ended` SSE event with `raw.lifecycle=discarded`. The architect event stream also receives a `workspace_changed` hint with `reason=ticket_moved` and the ticket ID so ticket boards can refresh.

### Conclusion approval flow

When an agent calls `concludeSession` via MCP, the daemon routes through an approval flow so the desktop user can review before the session ends:

1. MCP `concludeSession` calls `POST /api/sessions/{id}/request-conclusion` (blocks)
2. For ticket sessions, the daemon validates the commit/rejection invariant up front (commits required unless `rejected=true` with a reason) — before storing the approval or publishing the event — so an invalid conclusion returns a `VALIDATION` error to the agent immediately and no dialog is ever shown
3. The daemon stores a pending approval in memory, publishes an `approval_required` SSE event on the session event stream (with `raw.timeout_seconds` so the desktop can show a countdown), and blocks on a channel with the configured `conclusion_approval_timeout` (default 20s)
4. The desktop receives the SSE event and presents an approval dialog with a countdown timer
5. The desktop calls `POST /api/sessions/{id}/approve-conclusion` or `POST /api/sessions/{id}/reject-conclusion`
6. On approve: the daemon runs the conclusion and returns the result. On reject: the rejection reason propagates back as a `VALIDATION` error to the blocked MCP call so the agent can retry.
7. On timeout: the daemon auto-approves and runs the conclusion as if the user clicked approve.
8. Whenever a pending approval is resolved without a session-ending conclusion — reject, agent disconnect (request context cancelled), or a failed approve — the daemon publishes a durable `approval_resolved` SSE event (`raw.outcome` is `rejected`/`cancelled`/`error`). `approval_required` is persisted and replayed on every desktop (re)connect, but the pending approval lives only in memory; the resolution event is its durable counterpart, so replaying the event log always converges to "no dialog". A successful conclusion needs no resolution event — its `ended`/`concluded` event already dismisses the dialog. On startup the in-memory approval store is empty, so `ReconcilePendingApprovals` scans for any session whose latest approval event is still an unresolved `approval_required` (orphaned by a daemon restart) and appends `approval_resolved` (`outcome: daemon_restart`).

The original `POST /api/sessions/{id}/conclude` endpoint remains available for direct calls without approval.

`POST /api/sessions/{id}/conclude` now requires structured commit refs in requests for ticket sessions. `repo` is the configured repo key from `architects.yaml`, not a filesystem path. Freeform sessions may omit `commits`; when they do provide commits, the daemon validates the same `{sha, repo}` contract.

Request:

```json
{
  "body": "Implemented multi-repo conclusion support.",
  "commits": [
    {"sha": "abc123", "repo": "daemon"},
    {"sha": "def456", "repo": "desktop"}
  ],
  "rejected": false,
  "rejection_reason": ""
}
```

Ticket/conclusion output always returns the structured shape, including when reading older `conclusion.md` files that stored commits as flat SHA arrays:

```json
{
  "started_at": "2026-05-18T14:00:00Z",
  "concluded_at": "2026-05-18T14:30:00Z",
  "agent": "codex",
  "profile": "codex",
  "rejected": false,
  "rejection_reason": "",
  "commits": [
    {"sha": "abc123", "repo": "daemon"},
    {"sha": "def456", "repo": "desktop"}
  ],
  "body": "Implemented multi-repo conclusion support."
}
```

All responses use a standard envelope:

```json
{
  "data": { ... },
  "error": { "code": "VALIDATION", "message": "...", "details": { ... }, "stacktrace": "..." },
  "logs": [],
  "commands": [],
  "meta": { "request_id": "..." }
}
```

`data` and `error` are mutually exclusive. Error codes use uppercase snake_case: `VALIDATION`, `CONFLICT`, `NOT_FOUND`, `INTERNAL`.
