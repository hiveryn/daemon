# daemon Architecture

`daemon` is the local HTTP/WebSocket server that owns Hiveryn's local state, agent spawn lifecycle, filesystem mutations, and MCP tool surface.

## Purpose

The daemon is the **single mutation and event hub** for Hiveryn. Every state change — whether initiated by the desktop app, an MCP tool call from a running agent, or a lifecycle event from `agentruntime` — flows through the daemon. It owns:

- **Local state**: bootstrap config under `HIVERYN_HOME` (default `~/.hiveryn`) with `config.yaml` plus `variants.yaml`, `architects.yaml`, `tabs.yaml`, and `shortcuts.yaml`; SQLite for sessions, terminal buffers, and runtime events.
- **Agent lifecycle**: spawn, kill, and track agent processes through daemon-owned ptys; delegate launch/config synthesis to `agentruntime`.
- **Filesystem mutations**: read/write architect folder markdown (tickets, conclusions, collabs). The architect folder is the shared source of truth; the daemon's SQLite is local-only.
- **MCP tools**: exposed by the daemon so running agents can mutate project state (create tickets, conclude sessions, and manage the architect's own `hiveryn.yaml` — repos, ticket kickoffs, architect prompts) without direct filesystem access. Architect sessions can also list a redacted, decision-only view of configured agent profiles and request a backlog ticket spawn with an explicit profile through approval; profile discovery never exposes env, credentials, MCP config, args, or other launch internals. The architect edits its config through a declarative whole-document surface (`readArchitectConfig` → edit → `updateArchitectConfig`) guarded by a stateless content-hash version token, plus `readDefaultPrompt` for embedded templates. Every write is validated against the loader's ruleset so an invalid edit can never brick session spawning; repo paths are additionally checked for an on-disk `.git` directory at write time only (kept out of the loader's ruleset, since a repo dir moved/deleted after being configured must not fail the whole config load), giving the architect an actionable error immediately instead of a lazy failure at ticket-spawn time.
- **Event stream**: SSE or WebSocket hints so the desktop app updates views without polling.

## Package boundaries

```
cmd/hiverynd          entrypoint: `serve` daemon mode and `mcp` stdio subcommand
internal/
  app/                dependency wiring, startup/shutdown orchestration
  archevents/         in-memory publish/subscribe hub for architect-scoped SSE events
  archive/            optional append-only JSONL archive of normalized agentruntime events (daily rotation, fire-and-forget — failures never block ingestion)
  architectfs/        architect folder filesystem operations (ticket CRUD, frontmatter, body edits)
  config/             bootstrap config (~/.hiveryn/{config,variants,architects,tabs,shortcuts}.yaml) — port, bind_address, log_level, shell, intent_wait_timeout, archive_agent_events, variants, architects, tabs, shortcuts
  domain/             re-exports shared data types (github.com/hiveryn/shared/domain) + local interfaces, Envelope, ArchitectEvent, AgentStatus consts — zero imports of store/api
  gitdiff/            git working-tree + single-commit diff computation (git shell-out, file-level diff parsing) plus batch gitignore checks (`CheckIgnore`) reused by the fs browse API — no session dependency
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
- Config-backed read APIs read from `config/`, not SQLite. Architect/repo lookups plus variant/tab/shortcut reads must come from the current YAML view so edits to `architects.yaml` (and the per-architect `hiveryn.yaml` files it points at), `variants.yaml`, `tabs.yaml`, and `shortcuts.yaml` apply without a daemon restart. `architects.yaml` is a bare `key: path` registry; each architect's name, repos, and prompts live in its workspace `hiveryn.yaml`.
- `app/` wires everything together — it's the only package that imports both `store/` and `api/`.
- `sessionruntime/` owns live process/PTY state and bridges `agentruntime` events into persisted session events.
- `sessionruntime/` also owns the in-memory **intent store** (N pending intents per session, keyed by intent id and indexed by session), resolved per-session terminal UUIDs, and right-pane tab layout state for the current run — a single tab list shared by terminal tabs (PTY-backed) and browser tabs (`type: "browser"`, no PTY, mutated via `PreviewBrowserTab`/`CloseBrowserTab`), each keyed by its own UUID in `SessionTab.ID`; SQLite stores durable `sessions`, `session_runs`, and structured events, not terminal identity/layout snapshots or intent state.
- **`Intent` vs `Session`.** A `Session` (table `sessions`) is the durable spawn record — "run an agent for ticket X in workdir Y". An `Intent` is a pending agent tool call awaiting the user's approval. They are unrelated; do not reintroduce `SessionIntent` for either.
- `logging/` owns append-only JSONL sinks and schema shaping for app logs and request logs. Middleware and services should emit structured fields, not hand-built JSON strings.
- `mcp/` stays transport-focused: role-specific tool registration plus HTTP client shims back into daemon APIs. Keep tool handlers out of `cmd/` and avoid filesystem mutations here.
- `config/` is self-contained. Bootstrap config lives outside SQLite because the server needs it before the DB opens.

## Adding a new resource

Example: adding a SQLite-backed session resource.

1. **Domain** (`internal/domain/session.go`): re-export pure data types from `github.com/hiveryn/shared/domain` (Session/SessionRun/SessionEvent/SessionTab/Intent*/Ticket*/CommitRef/error types/enums/etc.) via type aliases + const re-exports; define `SessionRepository`/`SessionService`/`TicketService` interfaces and any daemon-local contracts (Envelope, ArchitectEvent, AgentStatus, TerminalAttachment, etc.) here.
2. **Migration** (`internal/store/migrations/0002_<resource>.sql`): CREATE TABLE. Add to `migrationFiles` slice in `internal/store/migrate.go`.
3. **Repository** (`internal/store/sessions.go`, `internal/store/runs.go`): `SessionStore` methods implementing `domain.SessionRepository` with SQLite queries.
4. **API** (`internal/api/sessions.go`): handlers using `domain.SessionRepository` interface. Use Go 1.24 method-pattern routing (`"POST /api/sessions"`).
5. **Routes** (`internal/api/router.go`): register handler methods in `NewHandler`.
6. **Wiring** (`internal/app/app.go`): instantiate the new store, pass the repo to the handler.

Each resource is self-contained across four packages — no cross-contamination. Handlers don't know SQL. Store doesn't know HTTP.

## Design rules

- Bind to localhost by default. Allow loopback-only addresses in config validation.
- Design for a single local machine: **1 desktop app + 0–2 MCP sessions at a time**. No multi-user concurrency, no connection pooling beyond what stdlib provides. Scale the code for hundreds of APIs and dozens of tables, but the runtime load profile stays a handful of local consumers per machine.
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
- Agent tool calls that mutate user-visible state route through the **intent system** (`sessionruntime/intents*.go`): the daemon raises a pending intent, publishes a durable `type: "intent"` / `status: "required"` session event carrying `{intent_id, intent_type, summary, payload, origin, wait_seconds, policy}`, and BLOCKS the agent's MCP call on a channel until the user answers or the tool's policy fires. No polling. The side effect lives in the intent's `Exec` and runs only after the intent resolves approved, so enforcement is daemon-side and an agent cannot bypass it. `concludeSession`, `createWorkTicket`, and architect-only `spawnTicketSession` are routed today; spawn requires an explicit configured profile, revalidates profile and backlog ticket state inside `Exec`, and uses `wait-then-deny` so timeout never launches an agent. The desktop's own direct routes (`POST /api/sessions/{id}/conclude`, `POST /api/architects/{key}/tickets`) stay unapproved because approval gates the agent, not the user.
- Per-tool expiry policy is a hardcoded table (`intentPolicies` in `sessionruntime/intents_policy.go`) with `auto-allow | wait-then-allow | wait-then-deny`; adding a tool or flipping behavior is a one-line change, and an unregistered tool fails fast rather than defaulting to allow. The wait *window* is shared and comes from config (`intent_wait_timeout`, default 20s); it must stay safely under the smallest agent-runtime tool-call ceiling (~60s) — shrink the window, never raise the ceiling.
- Intents are idempotent within a 1h window, keyed by `hash(session, tool, normalized-args)`. A retry attaches to a live intent or replays the resolved outcome; it never mints a duplicate. Consequently an intent does NOT die with the requesting agent's ctx (a tool-call timeout *is* ctx death, and the retry must be able to replay) — a detached owner goroutine holds the timer, and a caller whose ctx dies merely detaches. Anything varying per call (e.g. `CreateTicketParams.Now`) must be stamped inside `Exec`, never in the hashed payload, or dedup silently stops working.
- Intent outcomes are a closed catalog (`approved`, `auto_approved`, `denied_by_user`, `auto_denied`, `error`). A denial is returned to the agent as a SUCCESSFUL tool result carrying the outcome, never as a tool error — a tool error reads to an agent as "bad input, retry", which is the retry loop the catalog exists to prevent. Only `error` is retryable.
- Known gap: `session_events` is a 100-event ring buffer per session (`store/runs.go`), so a session that emits ≥100 events during the wait window can trim its own `intent/required` row out of the log, and a desktop reconnecting mid-window will render no popup. Pairing stays correct (eviction is oldest-first, so `resolved` can never outlive its `required`) and the policy still resolves the intent, so no agent hangs and no write is lost. Fixing it means exempting `type='intent'` from both the count and the delete.
- The only legitimate session ends are the role-scoped conclude tools (`concludeArchitectSession`, `concludeTicketSession`, `concludeFreeformSession`) and explicit ticket-session discard. Discard moves the ticket back to `backlog`, emits an ended session event with `raw.lifecycle=discarded`, kills PTYs, deletes the run, deletes the intent, and writes no conclusion. Any unexpected main agent PTY exit must auto-resume the current run from the stored `native_id` (or id-less resume — a session picker — when `native_id` is empty), publish the new `main_terminal_id`, and leave run status as `running`. A failed resume — on startup restore or after a PTY exit — marks the run failed (`restore_failed` on restore, `launch_failed` on PTY-exit resume), logs at ERROR level, and for ticket sessions moves the ticket back to `backlog`; it never crashes the daemon, and startup continues regardless of individual failures.
- Architect sessions launched with `AgentOpenCode` must define a named `StartRequest.OpenCodeAgentConfig` entry keyed by `architect_key`, use the architect system prompt as that agent's `Prompt`, and prepend `--agent <architect_key>` to launch args. Ticket and freeform OpenCode sessions must not define a named agent, and architect OpenCode profile args must not include `--agent` because the daemon owns that flag.
- `sessions` store the fully resolved create-time contract: `id` (runtime identity), `architect_key`, `session_type`, `context_id` (artifact/context identity), `prompt`, primary `workdir`, normalized `additional_repos`, resolved `additional_workdirs`, optional `instructions`, and lifecycle metadata. Runs copy the repository-scope snapshot. Initial launch, restore, and terminal-exit resume must use stored paths instead of re-resolving mutable ticket or architect repo configuration.
- Tickets require one primary `repo` key and may declare unique, non-overlapping `additional_repos`; every key must exist in the architect's `hiveryn.yaml`. The primary repo selects the kickoff and working directory. Additional repo paths must remain distinct after `filepath.Clean`, and ticket conclusions may reference commits only from the complete ticket scope.
- Conclude tools take discrete structured fields per session type; the daemon renders them into `conclusion.md` (unchanged frontmatter metadata + a canonical markdown body with a fixed per-type section order). This is an input contract rendered in `RequestConclusion` before the approval surface — do not persist a parallel structured copy and do not change the conclusion read structs or frontmatter fields. Every presentational section is a Markdown string authored by the agent, not an array — this deliberately sidesteps the MCP client's required-array-drop bug (a required array field can be silently stripped before it reaches the daemon, yielding a spurious `-32602 missing properties`). `commits` is the sole exception: it is a structured `[]CommitRef` array because it is persisted to frontmatter and read back per-commit, not merely rendered. Required sections are rejected if blank; record "nothing to report" with the literal Markdown `"None"`. Old freeform-body `conclusion.md` files stay readable.
- Ticket-session conclusion commit metadata is stored and returned as structured `{sha, repo}` entries, where `repo` is the architect repo key from config. Legacy conclusion markdown that stored flat SHA arrays must remain readable and resolve those SHAs against the ticket's repo key.
- The architect folder's markdown is the source of truth for tickets and conclusions. `HIVERYN_HOME/config.yaml` is the source of truth for daemon core settings, including the default terminal shell, unless launch flags override it. `HIVERYN_HOME/variants.yaml`, `HIVERYN_HOME/architects.yaml`, `HIVERYN_HOME/tabs.yaml`, and `HIVERYN_HOME/shortcuts.yaml` are the source of truth for variants, the architect registry, tab layouts, and shortcuts when using the default runtime layout; each architect's repo mappings and prompts live in the `hiveryn.yaml` at its workspace root. Changes to those YAML files (including the per-architect `hiveryn.yaml`) must be picked up without a daemon restart; SQLite stores runtime state only. `HIVERYN_ENV` identifies the daemon runtime mode but does not change paths by itself.
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
