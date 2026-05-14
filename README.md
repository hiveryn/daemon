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

The daemon reads four YAML files from `~/.hiveryn/` on startup. Only `config.yaml` is required; the others default to empty when missing.

### `config.yaml` — daemon core

```yaml
port: 4201
bind_address: 127.0.0.1
log_level: info
```

| Field | Default |
|---|---|
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
```

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
    name: lazygit
    command: lazygit
  - type: terminal
    name: shell

work:
  - type: event-log
  - type: terminal
    name: shell
```

Terminal entries require a unique `name` within the session type. Entries without `command` default to the user's shell. When a session spawns, the daemon auto-creates PTY terminals for every `type: terminal` entry in the matching session type section.

## Data

Local runtime state is stored at `~/.hiveryn/daemon.db`. This file is safe to delete — it will be recreated on next start. Variants, architects, repo mappings, and tab layouts live in `~/.hiveryn/*.yaml`. Your architect workspace (tickets, conclusions) is stored separately as markdown files and is never affected.

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
| `POST` | `/api/architects/{key}/spawn` | Spawn an architect session using an agent profile |
| `GET` | `/api/architects/{key}/tickets` | List ticket board columns; supports `?status=backlog\|progress\|done` and `?limit=N` |
| `POST` | `/api/architects/{key}/tickets` | Create a backlog ticket in the architect folder |
| `GET` | `/api/architects/{key}/tickets/{id}` | Get one filesystem-backed ticket by ID |
| `PATCH` | `/api/architects/{key}/tickets/{id}` | Apply a targeted body edit to a ticket |
| `PATCH` | `/api/architects/{key}/tickets/{id}/metadata` | Update ticket frontmatter (`title`, `repo`, `references`) |
| `DELETE` | `/api/architects/{key}/tickets/{id}` | Delete a ticket folder and its contents |
| `POST` | `/api/architects/{key}/tickets/{id}/move?to=...` | Move a ticket between backlog, progress, and done |
| `POST` | `/api/architects/{key}/tickets/{id}/spawn` | Spawn a worker session for a ticket (backlog → progress) |
| `GET` | `/api/architects/{key}/events` | Stream architect-scoped workspace_changed SSE hints |
| `GET` | `/api/architects/{key}/conclusions` | List recent conclusions (IDs + timestamps); supports `?limit=N` |
| `GET` | `/api/architects/{key}/conclusions/recent` | Read the most recent architect session conclusion |
| `GET` | `/api/architects/{key}/conclusions/{id}` | Read a conclusion by ID |
| `GET` | `/api/architects/{key}/repos` | List repos for an architect |
| `GET` | `/api/architects/{key}/repos/{repoKey}` | Get one architect repo by key |
| `GET` | `/api/sessions` | List sessions; supports `?status=running` |
| `GET` | `/api/sessions/{id}` | Get one session |
| `DELETE` | `/api/sessions/{id}` | Kill and delete a session |
| `POST` | `/api/sessions/{id}/conclude` | Conclude a running session (architect or work) |
| `POST` | `/api/sessions/{id}/terminals` | Create a new user terminal in a session |
| `GET` | `/api/sessions/{id}/terminals` | List all terminals for a session |
| `DELETE` | `/api/sessions/{id}/terminals/{name}` | Kill a specific user terminal |
| `GET` | `/api/sessions/{id}/events` | Stream structured session events over SSE |
| `WS` | `/ws/session/{id}/terminal/{name}` | Stream PTY output and send terminal input for a named terminal |

Profile, architect, repo, and tab configuration endpoints are read-only. Edit `~/.hiveryn/*.yaml` directly to change variants, architects, repos, or tabs.

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
