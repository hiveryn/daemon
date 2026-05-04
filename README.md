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

## Configuration

The daemon reads `~/.hiveryn/daemon.yaml` on startup. If the file doesn't exist, defaults are used.

```yaml
port: 4200
bind_address: 127.0.0.1
log_level: info
```

| Field | Default |
|---|---|
| `port` | `4200` |
| `bind_address` | `127.0.0.1` (localhost only) |
| `log_level` | `info` |

JSON format is also supported.

## Data

Local state is stored at `~/Library/Application Support/Hiveryn/state.db`. This file is safe to delete — it will be recreated on next start. Your architect workspace (tickets, conclusions) is stored separately as markdown files and is never affected.

## API

| Method | Path | Description |
|---|---|---|
| `GET` | `/api/health` | Health check |
| `GET` | `/api/agent-profiles` | List all agent profiles |
| `POST` | `/api/agent-profiles` | Create an agent profile |
| `GET` | `/api/agent-profiles/{id}` | Get one agent profile |
| `PUT` | `/api/agent-profiles/{id}` | Update an agent profile |
| `DELETE` | `/api/agent-profiles/{id}` | Delete an agent profile |

`agent_kind` must be `claude`, `codex`, or `opencode`.

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
