# daemon Architecture

`daemon` is the local HTTP/WebSocket server that owns Hiveryn's local state, agent spawn lifecycle, filesystem mutations, and MCP tool surface.

## Purpose

The daemon is the **single mutation and event hub** for Hiveryn. Every state change — whether initiated by the desktop app, an MCP tool call from a running agent, or a lifecycle event from `agentruntime` — flows through the daemon. It owns:

- **Local state** (SQLite): agent profiles, architect folder registrations, repo mappings, active/recent sessions, terminal buffers, runtime events.
- **Agent lifecycle**: spawn, kill, and track agent processes through daemon-owned ptys; delegate launch/config synthesis to `agentruntime`.
- **Filesystem mutations**: read/write architect folder markdown (tickets, conclusions, collabs). The architect folder is the shared source of truth; the daemon's SQLite is local-only.
- **MCP tools**: exposed by the daemon so running agents can mutate project state (create tickets, conclude sessions) without direct filesystem access.
- **Event stream**: SSE or WebSocket hints so the desktop app updates views without polling.

## Consumers

| Consumer | How it uses the daemon | Load profile |
|---|---|---|
| Desktop app | HTTP API for workspace views, session surfaces, and settings; WebSocket for pty I/O and app event stream | primary — active when Hiveryn is open |
| MCP tools (in-agent) | HTTP API for ticket/collab/conclusion mutations scoped to the active session | occasional — bursts of mutations during agent runs |
| `agentruntime` ingest | HTTP hook endpoint for normalized agent status/tool events | frequent — events stream from running sessions |

v0.1 expects **1 desktop app + 0–2 MCP sessions at a time**. No multi-user concurrency, no connection pooling beyond what stdlib provides. Scale the server design for hundreds of APIs and dozens of tables, but the load profile remains a handful of local consumers per machine.

## Architecture shape (v0.1 target)

```
Desktop app        ←HTTP/WS→  Daemon  ←MCP stdio/HTTP→  Agent process
                                   │
                                   ├─ SQLite (local state)
                                   ├─ Filesystem (architect folder = markdown)
                                   ├─ agentruntime (launch/config/status primitives)
                                   └─ Pty (daemon-owned process I/O)
```

The daemon is the integration point. The desktop app, MCP tools, and agent processes all talk to the daemon. The daemon calls `agentruntime` during spawn and receives hook events back through `agentruntime`'s ingest pipeline.

## Phased scope (from `08-phasing.md`)

| Phase | What the daemon owns | Status |
|---|---|---|
| Phase 2 — daemon core | Architect folder FS ops, ticket CRUD, conclusions, registered folders, repo mappings, agent profiles, HTTP API | **in progress** |
| Phase 3 — MCP + first session | MCP tools, first architect spawn through daemon pty, `concludeSession` | planned |
| Phase 4 — desktop shell | Pty WebSocket, app event stream (SSE/WS), session surface integration | planned |
| Phase 5 — worker loop | Worker spawn from ticket repo key, repo mapping resolution, worker MCP tools, cancel/reject | planned |
| Phase 6 — collab loop | Collab sessions, prompt/conclusion files, collab MCP tools, recent session history | planned |

## Package boundaries

```
cmd/hiverynd          entrypoint: flags → app.Run()
internal/
  app/                dependency wiring, startup/shutdown orchestration
  config/             bootstrap config (~/.hiveryn/daemon.yaml) — port, bind_address, log_level
  domain/             pure types, repository interfaces, shared errors — zero imports of store/api
  server/             HTTP server lifecycle (Listen, Shutdown) — thin wrapper around net/http
  api/                HTTP handlers, routing, middleware (request ID, recovery, access logging), JSON helpers
  store/              SQLite persistence: DB open, migration runner, per-resource repository implementations
```

**Key rules:**

- `domain/` must not import `store/`, `api/`, or `server/`. It defines the contract everything else depends on.
- `store/` implements repository interfaces from `domain/`.
- `api/` handlers depend on `domain/` interfaces (e.g. `domain.ProfileRepository`), never on `store/` structs directly.
- `app/` wires everything together — it's the only package that imports both `store/` and `api/`.
- `config/` is self-contained. Bootstrap config lives outside SQLite because the server needs it before the DB opens.

## Adding a new resource

Example: adding a `sessions` table and API.

1. **Domain** (`internal/domain/session.go`): `Session` struct, `SessionRepository` interface, any enums or validation errors.
2. **Migration** (`internal/store/migrations/0002_sessions.sql`): CREATE TABLE. Add to `migrationFiles` slice in `internal/store/migrate.go`.
3. **Repository** (`internal/store/sessions.go`): `SessionStore` struct implementing `domain.SessionRepository` with SQLite queries.
4. **API** (`internal/api/sessions.go`): handlers using `domain.SessionRepository` interface. Use Go 1.24 method-pattern routing (`"POST /api/sessions"`).
5. **Routes** (`internal/api/router.go`): register handler methods in `NewHandler`.
6. **Wiring** (`internal/app/app.go`): instantiate the new store, pass the repo to the handler.

Each resource is self-contained across four packages — no cross-contamination. Handlers don't know SQL. Store doesn't know HTTP.

## Design rules

- Bind to localhost by default. Allow loopback-only addresses in config validation.
- Use Go 1.24 stdlib `http.ServeMux` method-pattern routing (`"GET /api/agent-profiles/{id}"`). No third-party routers.
- Prefer interfaces over concrete dependencies at handler boundaries.
- One repository file per table/aggregate in `store/`. One handler file per resource in `api/`.
- Migrations are idempotent, versioned, and run inside a transaction per file.
- Access logging, panic recovery, and request IDs are enforced by middleware — not per-handler.
- SQLite uses `SetMaxOpenConns(1)` (single-writer). Busy timeout is 5 seconds.
- Never log secrets from profiles, env configs, or MCP configurations.
- The architect folder's markdown is the source of truth for tickets and conclusions. SQLite stores local preferences and runtime state only. Deleting `state.db` must be recoverable by re-registering architect folders.

## Development

- `make vet` — static analysis
- `make test` — run all tests
- `make build` — compile check all packages
- `make lint` — golangci-lint (v2 config, same linter set as `agentruntime`)
- Run `make tidy && git diff --exit-code -- go.mod go.sum` before merging.
