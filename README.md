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
hiverynd mcp --daemon-url http://127.0.0.1:4200 --architect-key hiveryn
```

`hiverynd mcp` is intended to be spawned by `agentruntime`; session scoping comes from `HIVERYN_SESSION_TYPE` (`architect` or `work`).

## Configuration

The daemon reads `~/.hiveryn/config.yaml` on startup. If the file doesn't exist, it creates a default file.

```yaml
port: 4200
bind_address: 127.0.0.1
log_level: info

agent_profiles: {}
architects: {}
```

| Field | Default |
|---|---|
| `port` | `4200` |
| `bind_address` | `127.0.0.1` (localhost only) |
| `log_level` | `info` |

## Data

Local runtime state is stored at `~/.hiveryn/daemon.db`. This file is safe to delete — it will be recreated on next start. Profiles, architects, and repo mappings live in `~/.hiveryn/config.yaml`. Your architect workspace (tickets, conclusions) is stored separately as markdown files and is never affected.

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
| `GET` | `/api/architects/{key}/tickets` | List filesystem-backed ticket board columns |
| `POST` | `/api/architects/{key}/tickets` | Create a backlog ticket in the architect folder |
| `GET` | `/api/architects/{key}/tickets/{id}` | Get one filesystem-backed ticket by ID |
| `PATCH` | `/api/architects/{key}/tickets/{id}` | Apply a targeted body edit to a ticket |
| `PATCH` | `/api/architects/{key}/tickets/{id}/metadata` | Update ticket frontmatter (`title`, `repo`, `references`) |
| `DELETE` | `/api/architects/{key}/tickets/{id}` | Delete a ticket folder and its contents |
| `POST` | `/api/architects/{key}/tickets/{id}/move?to=...` | Move a ticket between backlog, progress, and done |
| `POST` | `/api/architects/{key}/tickets/{id}/spawn` | Spawn a worker session for a ticket (backlog → progress) |
| `GET` | `/api/architects/{key}/events` | Stream architect-scoped workspace_changed SSE hints |
| `GET` | `/api/architects/{key}/repos` | List repos for an architect |
| `GET` | `/api/architects/{key}/repos/{repoKey}` | Get one architect repo by key |
| `GET` | `/api/sessions` | List sessions; supports `?status=running` |
| `GET` | `/api/sessions/{id}` | Get one session |
| `DELETE` | `/api/sessions/{id}` | Kill and delete a session |
| `GET` | `/api/sessions/{id}/events` | Stream structured session events over SSE |
| `WS` | `/ws/session/{id}` | Stream PTY output and send terminal input |

Profile, architect, and repo configuration endpoints are read-only. Edit `~/.hiveryn/config.yaml` directly to change profiles, architects, or repos.

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
