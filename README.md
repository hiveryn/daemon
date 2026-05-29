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

The daemon reads five YAML files from `HIVERYN_HOME` (default `~/.hiveryn`). Only `config.yaml` is required; the others default to empty when missing. `architects.yaml` is reloaded on demand for architect/repo lookups, so new architect and repo mappings do not require a daemon restart. Passing `--config` points `config.yaml` elsewhere and, because config loading is directory-scoped, also changes where `variants.yaml`, `architects.yaml`, `tabs.yaml`, and `shortcuts.yaml` are read from.

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

For architect sessions using `agent: opencode`, the daemon defines a named OpenCode agent automatically from `prompts/architect/SYSTEM.md`, using the architect key as the agent name and passing `--agent <architect_key>` at launch. Do not put `--agent` in OpenCode architect variant args; the daemon treats that as a launch error. Ticket and freeform OpenCode sessions do not define a named agent.

### `architects.yaml` — architect definitions

```yaml
hiveryn:
  path: /Users/kareem/architects/hiveryn
  group: personal
  repos:
    daemon: /Users/kareem/hiveryn/daemon
    desktop: /Users/kareem/hiveryn/desktop
```

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

Local runtime state is stored at `HIVERYN_HOME/daemon.db` by default. This file is safe to delete — it will be recreated on next start. Variants, architects, repo mappings, and tab layouts live in `HIVERYN_HOME/*.yaml` by default. Your architect workspace (tickets, conclusions) is stored separately as markdown files and is never affected. `--db` overrides the SQLite path explicitly.

The daemon also writes append-only structured JSONL logs to `HIVERYN_HOME/logs/daemon.jsonl` and `HIVERYN_HOME/logs/requests.jsonl` by default. `daemon.jsonl` contains app/runtime logs with source location metadata; `requests.jsonl` contains one JSON object per HTTP request/response, including the response envelope for JSON API calls.

## API

| Method | Path | Description |
|---|---|---|
| `GET` | `/api/system/runtime` | Get resolved daemon runtime identity and paths |
| `GET` | `/api/health` | Health check |
| `GET` | `/api/agent-profiles` | List all agent profiles |
| `GET` | `/api/agent-profiles/{name}` | Get one agent profile by name |
| `GET` | `/api/architect-groups` | List architect groups |
| `GET` | `/api/architect-groups/{name}` | Get one architect group by name |
| `GET` | `/api/architects` | List configured architects |
| `GET` | `/api/architects/{key}` | Get one configured architect by key |
| `GET` | `/api/architects/{key}/tickets` | List ticket board columns; supports `?status=backlog\|progress\|done` and `?limit=N` |
| `POST` | `/api/architects/{key}/tickets` | Create a backlog ticket in the architect folder |
| `GET` | `/api/architects/{key}/tickets/{id}` | Get one filesystem-backed ticket by ID |
| `PATCH` | `/api/architects/{key}/tickets/{id}` | Apply a targeted body edit to a backlog ticket |
| `PATCH` | `/api/architects/{key}/tickets/{id}/metadata` | Update frontmatter (`title`, `repo`, `references`) for a backlog ticket |
| `DELETE` | `/api/architects/{key}/tickets/{id}` | Delete a backlog ticket folder and its contents |
| `POST` | `/api/architects/{key}/tickets/{id}/move?to=...` | Move a ticket between backlog, progress, and done |
| `GET` | `/api/architects/{key}/events` | Stream architect-scoped workspace_changed SSE hints |
| `GET` | `/api/architects/{key}/conclusions` | List recent conclusions (IDs + timestamps); supports `?limit=N` |
| `GET` | `/api/architects/{key}/conclusions/recent` | Read the most recent architect session conclusion |
| `GET` | `/api/architects/{key}/conclusions/{id}` | Read a conclusion by ID |
| `GET` | `/api/architects/{key}/repos` | List repos for an architect |
| `GET` | `/api/architects/{key}/repos/{repoKey}` | Get one architect repo by key |
| `GET` | `/api/config/shortcuts` | Get resolved shortcuts config (global + per-pane keybindings) |
| `POST` | `/api/sessions` | Create a durable session intent for architect planning, ticket work, or freeform exploration |
| `GET` | `/api/sessions` | List session intents with their current run, if any |
| `GET` | `/api/sessions/{id}` | Get one session intent |
| `POST` | `/api/sessions/{id}/runs` | Start a run for a session intent using an agent profile; ticket runs move the ticket backlog → progress on successful launch |
| `POST` | `/api/sessions/{id}/conclude` | Conclude an intent's running run, publish `ended`, kill its PTYs, and delete the intent row; architect conclude returns `CONFLICT` if same-architect ticket sessions are still running |
| `POST` | `/api/sessions/{id}/request-conclusion` | Request conclusion approval (blocking). Stores pending approval, publishes `approval_required` SSE event, blocks until approved, rejected, or timeout (auto-approves). Called by the MCP `concludeSession` tool. |
| `POST` | `/api/sessions/{id}/approve-conclusion` | Approve a pending conclusion request and run the conclusion. Called by the desktop app. |
| `POST` | `/api/sessions/{id}/reject-conclusion` | Reject a pending conclusion request with a reason. Returns the reason as a validation error to the blocked `request-conclusion` caller so the agent can retry. |
| `GET` | `/api/sessions/{id}/tabs` | Get the resolved right-pane tab layout for a session intent's current run |
| `GET` | `/api/sessions/{id}/ticket` | Get the associated ticket for a ticket session (returns NOT_FOUND for non-ticket sessions) |
| `POST` | `/api/sessions/{id}/terminals` | Create a new user terminal in a session intent's current run using the resolved default shell |
| `GET` | `/api/sessions/{id}/terminals` | List all terminals for a session intent's current run |
| `DELETE` | `/api/sessions/{id}/terminals/{uuid}` | Kill a specific user terminal by UUID |
| `GET` | `/api/sessions/{id}/events` | Stream structured session intent events over SSE |
| `WS` | `/ws/session/{id}/terminal/{uuid}` | Stream PTY output and send terminal input for a terminal UUID |

Profile, architect, repo, and tab configuration endpoints are read-only. Edit `HIVERYN_HOME/*.yaml` directly to change variants, architects, repos, or tabs unless you launched with `--config`. `architects.yaml` changes apply to architect/repo reads plus new ticket/session operations without restarting the daemon.

Ticket mutations are status-gated: backlog tickets can be edited, metadata-updated, moved, or deleted; progress tickets can be concluded; done tickets are read-only.

Session responses expose a durable intent plus its current run, if any. Each intent stores the create-time session contract: `id`, `architect_key`, `session_type`, `context_id`, `prompt`, `workdir`, optional `instructions`, and lifecycle metadata. `POST /api/sessions/{id}/runs` returns the created `run`, its `main_terminal_id`, and a `ws_url` so the desktop can attach immediately. Launch uses the stored `workdir` directly; ticket and architect workdirs are resolved during intent creation, not recalculated later. If the main agent PTY exits unexpectedly, the daemon automatically resumes the running run from its stored `native_id`, emits a `main_terminal_resumed` session event with the new `main_terminal_id`, and leaves the run status as `running`; restore failures mark the run `restore_failed`, log the error at ERROR level, and for ticket sessions move the ticket back to `backlog`; the daemon continues startup regardless of individual restore failures. `GET /api/sessions/{id}/tabs` returns the canonical right-pane layout using `type`, `id`, `command`, and `status` for terminal tabs. `POST /api/sessions/{id}/terminals` accepts an empty JSON object and always launches the session's default shell in the current run workdir.

Only `POST /api/sessions/{id}/conclude` legitimately ends a session. Concluding an architect intent fails fast with `CONFLICT` while ticket intents for the same `architect_key` still have a `running` run; the error message lists the blocking intent IDs.

### Conclusion approval flow

When an agent calls `concludeSession` via MCP, the daemon routes through an approval flow so the desktop user can review before the session ends:

1. MCP `concludeSession` calls `POST /api/sessions/{id}/request-conclusion` (blocks)
2. The daemon stores a pending approval in memory, publishes an `approval_required` SSE event on the session event stream (with `raw.timeout_seconds` so the desktop can show a countdown), and blocks on a channel with the configured `conclusion_approval_timeout` (default 20s)
3. The desktop receives the SSE event and presents an approval dialog with a countdown timer
4. The desktop calls `POST /api/sessions/{id}/approve-conclusion` or `POST /api/sessions/{id}/reject-conclusion`
5. On approve: the daemon runs the conclusion and returns the result. On reject: the rejection reason propagates back as a `VALIDATION` error to the blocked MCP call so the agent can retry.
6. On timeout: the daemon auto-approves and runs the conclusion as if the user clicked approve.

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
