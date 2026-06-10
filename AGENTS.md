# daemon Architecture

`daemon` is the local HTTP/WebSocket server that owns Hiveryn's local state, agent spawn lifecycle, filesystem mutations, and MCP tool surface.

## Purpose

The daemon is the **single mutation and event hub** for Hiveryn. Every state change — whether initiated by the desktop app, an MCP tool call from a running agent, or a lifecycle event from `agentruntime` — flows through the daemon. It owns:

- **Local state**: bootstrap config under `HIVERYN_HOME` (default `~/.hiveryn`) with `config.yaml` plus `variants.yaml`, `architects.yaml`, `tabs.yaml`, and `shortcuts.yaml`; SQLite for sessions, terminal buffers, and runtime events.
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
                                   ├─ Pty (daemon-owned process I/O)
                                   └─ Approval store (in-memory sessionID → pending conclusion)
```

The daemon is the integration point. The desktop app, MCP tools, and agent processes all talk to the daemon. The daemon calls `agentruntime` during spawn and receives hook events back through `agentruntime`'s ingest pipeline.

## Phased scope (from `08-phasing.md`)

| Phase | What the daemon owns | Status |
|---|---|---|
| Phase 2 — daemon core | Architect folder FS ops, ticket CRUD, conclusions, registered folders, repo mappings, agent profiles, HTTP API | **in progress** |
| Phase 3 — MCP + first session | MCP tools, first architect spawn through daemon pty, `concludeSession` | **in progress** |
| Phase 4 — desktop shell | Pty WebSocket, app event stream (SSE/WS), session surface integration | planned |
| Phase 5 — worker loop | Worker spawn from ticket repo key, repo mapping resolution, worker MCP tools, cancel/reject | **in progress** |
| Phase 6 — collab loop | Collab sessions, prompt/conclusion files, collab MCP tools, recent session history | planned |

## Package boundaries

```
cmd/hiverynd          entrypoint: `serve` daemon mode and `mcp` stdio subcommand
internal/
  app/                dependency wiring, startup/shutdown orchestration
  archevents/         in-memory publish/subscribe hub for architect-scoped SSE events
  architectfs/        architect folder filesystem operations (ticket CRUD, frontmatter, body edits)
  config/             bootstrap config (~/.hiveryn/{config,variants,architects,tabs,shortcuts}.yaml) — port, bind_address, log_level, shell, conclusion_approval_timeout, variants, architects, tabs, shortcuts
  domain/             re-exports shared data types (github.com/hiveryn/shared/domain) + local interfaces, Envelope, ArchitectEvent, AgentStatus consts — zero imports of store/api
  logging/            structured JSONL app/request logging to ~/.hiveryn/logs/*.jsonl
  mcp/                stdio MCP server; registers role-scoped tools and translates tool calls into daemon HTTP API requests
  server/             HTTP server lifecycle (Listen, Shutdown) — thin wrapper around net/http
  api/                HTTP handlers, routing, middleware (request ID, recovery, access logging), JSON helpers
  sessionruntime/     session orchestration (architect + ticket + freeform), agentruntime ingest bridge, daemon-owned PTY manager
  store/              SQLite persistence: DB open, migration runner, session/event repository implementations
```

**Key rules:**

- `domain/` must not import `store/`, `api/`, or `server/`. It defines the contract everything else depends on.
- `store/` implements repository interfaces from `domain/` where runtime state is persisted in SQLite.
- Config-backed read APIs read from `config/`, not SQLite. Architect/repo lookups must come from the current `architects.yaml` view so new architect and repo mappings apply without a daemon restart.
- `app/` wires everything together — it's the only package that imports both `store/` and `api/`.
- `sessionruntime/` owns live process/PTY state and bridges `agentruntime` events into persisted session events.
- `sessionruntime/` also owns the in-memory conclusion approval store (one pending approval per session), resolved per-intent terminal UUIDs, and right-pane tab layout state for the current run; SQLite stores durable `session_intents`, `session_runs`, and structured events, not terminal identity/layout snapshots or approval state.
- `logging/` owns append-only JSONL sinks and schema shaping for app logs and request logs. Middleware and services should emit structured fields, not hand-built JSON strings.
- `mcp/` stays transport-focused: role-specific tool registration plus HTTP client shims back into daemon APIs. Keep tool handlers out of `cmd/` and avoid filesystem mutations here.
- `config/` is self-contained. Bootstrap config lives outside SQLite because the server needs it before the DB opens.

## Adding a new resource

Example: adding a SQLite-backed session resource.

1. **Domain** (`internal/domain/session.go`): re-export pure data types from `github.com/hiveryn/shared/domain` (SessionIntent/SessionRun/SessionEvent/SessionTab/Ticket*/CommitRef/error types/enums/etc.) via type aliases + const re-exports; define `SessionRepository`/`SessionService`/`TicketService` interfaces and any daemon-local contracts (Envelope, ArchitectEvent, AgentStatus, TerminalAttachment, etc.) here.
2. **Migration** (`internal/store/migrations/0002_<resource>.sql`): CREATE TABLE. Add to `migrationFiles` slice in `internal/store/migrate.go`.
3. **Repository** (`internal/store/intents.go`, `internal/store/runs.go`): `SessionStore` methods implementing `domain.SessionRepository` with SQLite queries.
4. **API** (`internal/api/sessions.go`): handlers using `domain.SessionRepository` interface. Use Go 1.24 method-pattern routing (`"POST /api/sessions"`).
5. **Routes** (`internal/api/router.go`): register handler methods in `NewHandler`.
6. **Wiring** (`internal/app/app.go`): instantiate the new store, pass the repo to the handler.

Each resource is self-contained across four packages — no cross-contamination. Handlers don't know SQL. Store doesn't know HTTP.

## Design rules

- Bind to localhost by default. Allow loopback-only addresses in config validation.
- Use Go 1.24 stdlib `http.ServeMux` method-pattern routing (`"GET /api/agent-profiles/{name}"`). No third-party routers.
- Prefer interfaces over concrete dependencies at handler boundaries.
- One repository file per table/aggregate in `store/`. One handler file per resource in `api/`.
- Migrations are idempotent, versioned, and run inside a transaction per file.
- Access logging, panic recovery, and request IDs are enforced by middleware — not per-handler.
- Structured daemon logs live under `HIVERYN_HOME/logs/daemon.jsonl`; request logs live under `HIVERYN_HOME/logs/requests.jsonl`. Keep every record as single-line valid JSON.
- SQLite uses `SetMaxOpenConns(1)` (single-writer). Busy timeout is 5 seconds.
- Never log secrets from profiles, env configs, or MCP configurations.
- PTY/process handles stay in memory under `sessionruntime`; SQLite stores session metadata and structured events only.
- Architect sessions must not conclude while same-architect ticket sessions are still running; return a `Conflict` with the active ticket session IDs instead of orphaning PTYs.
- Ticket filesystem mutations are status-gated: only backlog tickets can be edited, metadata-updated, or deleted; backlog or progress tickets can be concluded (worker conclusion still requires progress; architect-driven `move-to-done` accepts either); done tickets are read-only.
- The only legitimate session end is `concludeSession`. Any unexpected main agent PTY exit must auto-resume the current run from the stored `native_id`, publish the new `main_terminal_id`, and leave run status as `running`; restore failures mark the run `restore_failed`, log the error at ERROR level, and for ticket sessions move the ticket back to `backlog`; the daemon continues startup regardless of individual restore failures.
- Architect sessions launched with `AgentOpenCode` must define a named `StartRequest.OpenCodeAgentConfig` entry keyed by `architect_key`, use the architect system prompt as that agent's `Prompt`, and prepend `--agent <architect_key>` to launch args. Ticket and freeform OpenCode sessions must not define a named agent, and architect OpenCode profile args must not include `--agent` because the daemon owns that flag.
- `session_intents` store the fully resolved create-time contract: `id` (runtime identity), `architect_key`, `session_type`, `context_id` (artifact/context identity), `prompt`, `workdir`, optional `instructions`, and lifecycle metadata. Launch must use the stored `workdir` directly instead of re-resolving repo mappings or architect paths.
- Ticket-session conclusion commit metadata is stored and returned as structured `{sha, repo}` entries, where `repo` is the architect repo key from config. Legacy conclusion markdown that stored flat SHA arrays must remain readable and resolve those SHAs against the ticket's repo key.
- The architect folder's markdown is the source of truth for tickets and conclusions. `HIVERYN_HOME/config.yaml` is the source of truth for daemon core settings, including the default terminal shell, unless launch flags override it. `HIVERYN_HOME/variants.yaml`, `HIVERYN_HOME/architects.yaml`, `HIVERYN_HOME/tabs.yaml`, and `HIVERYN_HOME/shortcuts.yaml` are the source of truth for variants, architects, repo mappings, tab layouts, and shortcuts when using the default runtime layout. `architects.yaml` changes must be picked up without a daemon restart; SQLite stores runtime state only. `HIVERYN_ENV` identifies the daemon runtime mode but does not change paths by itself.
- All API responses use a standard envelope (`domain.Envelope`) with `data`/`error` (mutually exclusive), `logs`, `commands`, and `meta.request_id`. Handlers write via `writeJSON(w, r, ...)` and `writeError(w, r, ...)` — envelope wrapping is automatic.

## Error handling

- Never swallow error details. Every error path must either return the error verbatim or wrap it with `fmt.Errorf("context: %w", err)`.
- `writeDomainError` must log unexpected errors before converting them to 500 responses.
- All new handlers and service methods must surface internal errors with enough detail to diagnose failures from logs alone.
- Domain errors (Validation, Conflict, NotFound) are the only errors that should be converted to user-facing messages. Everything else is an internal error and must be logged with full context.

## Development

- `make vet` — static analysis
- `make test` — run all tests
- `make build` — compile check all packages
- `make lint` — golangci-lint (v2 config, same linter set as `agentruntime`)
- Run `make tidy && git diff --exit-code -- go.mod go.sum` before merging.
