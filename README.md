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

The daemon binary also exposes an MCP stdio subcommand for agent-launched tool access:

```bash
hiverynd mcp --daemon-url http://127.0.0.1:4201 --architect-key hiveryn
```

`hiverynd mcp` is intended to be spawned by `agentruntime`; session scoping comes from `HIVERYN_SESSION_TYPE` (`architect` or `work`).

## Configuration

The daemon reads five YAML files from `~/.hiveryn/`. Only `config.yaml` is required; the others default to empty when missing. `architects.yaml` is reloaded on demand for architect/repo lookups, so new architect and repo mappings do not require a daemon restart.

### `config.yaml` — daemon core

```yaml
shell: /bin/zsh
port: 4201
bind_address: 127.0.0.1
log_level: info
```

| Field | Default |
|---|---|
| `shell` | `$SHELL`, then `bash` |
| `port` | `4201` |
| `bind_address` | `127.0.0.1` (localhost only) |
| `log_level` | `info` |

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

For architect sessions using `agent: opencode`, the daemon defines a named OpenCode agent automatically from `prompts/architect/SYSTEM.md`, using the architect key as the agent name and passing `--agent <architect_key>` at launch. Do not put `--agent` in OpenCode architect variant args; the daemon treats that as a launch error. Worker OpenCode sessions do not define a named agent.

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

work:
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

Local runtime state is stored at `~/.hiveryn/daemon.db`. This file is safe to delete — it will be recreated on next start. Variants, architects, repo mappings, and tab layouts live in `~/.hiveryn/*.yaml`. Your architect workspace (tickets, conclusions) is stored separately as markdown files and is never affected.

The daemon also writes append-only structured JSONL logs to `~/.hiveryn/logs/daemon.jsonl` and `~/.hiveryn/logs/requests.jsonl`. `daemon.jsonl` contains app/runtime logs with source location metadata; `requests.jsonl` contains one JSON object per HTTP request/response, including the response envelope for JSON API calls.

## API

| Method | Path | Description |
|---|---|---|
| `GET` | `/api/system/home` | Get daemon host home directory |
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
| `PATCH` | `/api/architects/{key}/tickets/{id}` | Apply a targeted body edit to a ticket |
| `PATCH` | `/api/architects/{key}/tickets/{id}/metadata` | Update ticket frontmatter (`title`, `repo`, `references`) |
| `DELETE` | `/api/architects/{key}/tickets/{id}` | Delete a ticket folder and its contents |
| `POST` | `/api/architects/{key}/tickets/{id}/move?to=...` | Move a ticket between backlog, progress, and done |
| `GET` | `/api/architects/{key}/events` | Stream architect-scoped workspace_changed SSE hints |
| `GET` | `/api/architects/{key}/conclusions` | List recent conclusions (IDs + timestamps); supports `?limit=N` |
| `GET` | `/api/architects/{key}/conclusions/recent` | Read the most recent architect session conclusion |
| `GET` | `/api/architects/{key}/conclusions/{id}` | Read a conclusion by ID |
| `GET` | `/api/architects/{key}/repos` | List repos for an architect |
| `GET` | `/api/architects/{key}/repos/{repoKey}` | Get one architect repo by key |
| `GET` | `/api/config/shortcuts` | Get resolved shortcuts config (global + per-pane keybindings) |
| `POST` | `/api/sessions` | Create a durable session intent for architect planning or ticket work |
| `GET` | `/api/sessions` | List session intents with their current run, if any |
| `GET` | `/api/sessions/{id}` | Get one session intent |
| `POST` | `/api/sessions/{id}/runs` | Start a run for a session intent using an agent profile; work runs move the ticket backlog → progress on successful launch |
| `POST` | `/api/sessions/{id}/conclude` | Conclude an intent's running run, publish `ended`, kill its PTYs, and delete the intent row; architect conclude returns `CONFLICT` if same-architect work sessions are still running |
| `GET` | `/api/sessions/{id}/tabs` | Get the resolved right-pane tab layout for a session intent's current run |
| `POST` | `/api/sessions/{id}/terminals` | Create a new user terminal in a session intent's current run using the resolved default shell |
| `GET` | `/api/sessions/{id}/terminals` | List all terminals for a session intent's current run |
| `DELETE` | `/api/sessions/{id}/terminals/{uuid}` | Kill a specific user terminal by UUID |
| `GET` | `/api/sessions/{id}/events` | Stream structured session intent events over SSE |
| `WS` | `/ws/session/{id}/terminal/{uuid}` | Stream PTY output and send terminal input for a terminal UUID |

Profile, architect, repo, and tab configuration endpoints are read-only. Edit `~/.hiveryn/*.yaml` directly to change variants, architects, repos, or tabs. `architects.yaml` changes apply to architect/repo reads plus new ticket/session operations without restarting the daemon.

Session responses expose a durable intent plus its current run, if any. `POST /api/sessions/{id}/runs` returns the created `run`, its `main_terminal_id`, and a `ws_url` so the desktop can attach immediately. If the main agent PTY exits unexpectedly, the daemon automatically resumes the running run from its stored `native_id`, emits a `main_terminal_resumed` session event with the new `main_terminal_id`, and leaves the run status as `running`; restore failures mark the run `restore_failed` and abort daemon startup. `GET /api/sessions/{id}/tabs` returns the canonical right-pane layout using `type`, `id`, `command`, and `status` for terminal tabs. `POST /api/sessions/{id}/terminals` accepts an empty JSON object and always launches the session's default shell in the current run workdir.

Only `POST /api/sessions/{id}/conclude` legitimately ends a session. Concluding an architect intent fails fast with `CONFLICT` while worker intents for the same `architect_key` still have a `running` run; the error message lists the blocking intent IDs.

`POST /api/sessions/{id}/conclude` now requires structured commit refs in requests for work sessions. `repo` is the configured repo key from `architects.yaml`, not a filesystem path.

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
