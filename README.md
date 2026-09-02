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
intent_wait_timeout: 20
archive_agent_events: false
```

| Field | Default |
|---|---|
| `shell` | `$SHELL`, then `bash` |
| `port` | `4201` |
| `bind_address` | `127.0.0.1` (localhost only) |
| `log_level` | `info` |
| `intent_wait_timeout` | `20` (seconds) — how long a pending intent waits for the user before its tool's policy fires. Values `<= 0` are coerced back to the default. Must stay safely under the smallest agent-runtime tool-call ceiling (~60s). |
| `archive_agent_events` | `false` — set `true` to archive every normalized agentruntime event to per-day JSONL files under `HIVERYN_HOME/archive/agent_events/`. Full `Raw` payloads are preserved unredacted; archival failures never block ingestion.

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

The architect manages this file through MCP tools instead of editing it by hand: `readArchitectConfig` returns the whole config plus an opaque version token, `updateArchitectConfig` does a version-guarded whole-document replace, and `readDefaultPrompt` returns an embedded template + its variables. Writes enforce the rules above (a write that would fail to load is refused) and auto-scaffold the embedded default template when wiring a prompt path whose file does not exist yet.

### `roadmap/` — the architect's structured roadmap

Each architect workspace can hold a durable roadmap above the ticket level in `roadmap/current.yaml` (live goals/initiatives/milestones as a flat item graph with `parent_id`/`order`) and `roadmap/archive.yaml` (archived subtrees, restorable). The pair is one logical document: one opaque content-hash version token covers both files, every update is an atomic version-guarded batch (`create`, `update`, `move`, `link_ticket`, `unlink_ticket`, `archive`, `restore` — no delete), and both files are written with deterministic ordering/formatting so they stay readable and git-shareable. Missing files read as a valid empty roadmap; the first successful update creates them. The architect reads with `readRoadmap` and mutates through flat single-op MCP tools (`createRoadmapItem`, `updateRoadmapItem`, `moveRoadmapItem`, `linkRoadmapTicket`, `unlinkRoadmapTicket`, `archiveRoadmapItem`, `restoreRoadmapItem`, `setRoadmapTitle`), each wrapping a one-op batch against the HTTP endpoint — the HTTP contract itself stays an atomic ordered batch for future desktop use. Linked ticket IDs resolve against the same board (missing tickets warn, never block). Validation is strict and errors say how to recover: unknown YAML keys are rejected on load; item IDs are kebab-case ≤64 chars and globally unique (archived IDs stay reserved); ticket links must match the board ID shape `^\d{4}-\d{2}-\d{2}-\d{4}-[a-z0-9-]+$`; kind nesting follows goal > initiative > milestone (same kind may nest); `depends_on` may not target ancestors/descendants and must be acyclic; titles cap at 200 chars, outcomes at 2000, archive summaries at 500; success criteria are non-empty and deduplicated.

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

`tabs.yaml` also accepts arbitrary non-`terminal` tab types (e.g. `type: git-diff`, `type: kanban`). These are declarative (no `command`) and are emitted as plain layout entries with no daemon-side lookup — git diffs, for example, are served natively via `GET /api/architects/{key}/repos/{repoKey}/diff` and `GET /api/architects/{key}/repos/{repoKey}/commits/{sha}/diff`.

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

> Pre-release note: the base schema (`0001_sessions.sql`) is rewritten in place rather than migrated — the daemon carries no migration debt while it has no users. A `daemon.db` created before such a rewrite will fail loudly on the first query (`no such table: sessions`). Delete `HIVERYN_HOME/daemon.db*` and restart to recreate it.

The daemon also writes append-only structured JSONL logs to `HIVERYN_HOME/logs/daemon.jsonl` and `HIVERYN_HOME/logs/requests.jsonl` by default. `daemon.jsonl` contains app/runtime logs with source location metadata; `requests.jsonl` contains one JSON object per HTTP request/response, including the response envelope for JSON API calls. When `archive_agent_events` is enabled, the daemon also writes per-day JSONL event archives to `HIVERYN_HOME/archive/agent_events/agent_events_YYYY-MM-DD.jsonl`.

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
| `POST` | `/api/architects/{key}/tickets` | Create a backlog ticket with required primary `repo` and optional `additional_repos` keys |
| `GET` | `/api/architects/{key}/tickets/{id}` | Get one filesystem-backed ticket by ID |
| `PATCH` | `/api/architects/{key}/tickets/{id}` | Apply a targeted body edit to a backlog ticket |
| `PATCH` | `/api/architects/{key}/tickets/{id}/metadata` | Update frontmatter (`title`, `repo`, `additional_repos`, `references`) for a backlog ticket |
| `DELETE` | `/api/architects/{key}/tickets/{id}` | Delete a backlog ticket folder and its contents |
| `POST` | `/api/architects/{key}/tickets/{id}/move?to=...` | Move a ticket between backlog, progress, and done |
| `POST` | `/api/architects/{key}/tickets/{id}/move-to-done` | Architect-driven ticket completion without a worker session: backlog → done (architect resolved it directly) or progress → done (manually closing a dead/stuck worker session — fails with `CONFLICT` if a worker session is currently running). Writes a `conclusion.md`; requires `outcome` (`completed`/`exploratory`/`rejected`) — `completed` requires `commits`, `rejected` requires `rejection_reason`. Called by the MCP `moveTicketToDone` tool. |
| `GET` | `/api/architects/{key}/events` | Stream architect-scoped `workspace_changed` SSE hints. `reason` is a ticket reason (`ticket_created`/`ticket_updated`/`ticket_moved`/`ticket_deleted`/`ticket_concluded`) or a session reason (`session_started`/`session_ended`). Session reasons carry `session_id` — the only place in any stream where a session is named before a client knows it exists, so it is how a client discovers sessions it did not create (architect MCP spawns included). No backlog: reconcile on every (re)connect. |
| `GET` | `/api/architects/{key}/conclusions` | List recent conclusions (IDs + timestamps); supports `?limit=N` |
| `GET` | `/api/architects/{key}/conclusions/recent` | Read the most recent architect session conclusion |
| `GET` | `/api/architects/{key}/conclusions/{id}` | Read a conclusion by ID |
| `GET` | `/api/architects/{key}/repos` | List repos for an architect |
| `GET` | `/api/architects/{key}/repos/{repoKey}` | Get one architect repo by key |
| `GET` | `/api/architects/{key}/repos/{repoKey}/diff` | Current working-tree diff (staged + unstaged + untracked) for a repo, read-only |
| `GET` | `/api/architects/{key}/repos/{repoKey}/status` | Changed paths only (`git status --porcelain`, untracked expanded to files), each with its raw `index`/`worktree` columns and `orig_path` for renames — the lightweight sibling of `/diff` for decorating file trees |
| `GET` | `/api/architects/{key}/repos/{repoKey}/commits/{sha}/diff` | Diff of a single commit (root commits diff against the empty tree; merge commits diff against their first parent) |
| `GET` | `/api/architects/{key}/config` | Read the whole `hiveryn.yaml` config (repos, prompts, kickoffs — verbatim paths), a resolved view (absolute paths + per-prompt `exists`), warnings for missing wired prompt files, and an opaque `version` token |
| `PUT` | `/api/architects/{key}/config` | Replace the whole config (declarative), guarded by `version` (`VALIDATION` if missing/invalid, `CONFLICT` if stale); auto-scaffolds missing wired prompt files and returns them in `created` |
| `GET` | `/api/architects/{key}/config/default-prompt?kind=architect-system\|architect-kickoff\|ticket-kickoff` | Return the embedded default template for a prompt kind plus its valid Go template variables |
| `GET` | `/api/architects/{key}/roadmap` | Read the roadmap: `?view=current` (default) or `?view=archive` (compact entry summaries; `?id=root_id` for one full stored subtree), `?id=` + optional `?depth=` for a focused current subtree. Returns items, resolved linked-ticket metadata, warnings, and an opaque `version` token |
| `PUT` | `/api/architects/{key}/roadmap` | Apply an ordered op batch atomically (`create`/`update`/`move`/`link_ticket`/`unlink_ticket`/`archive`/`restore`, plus optional top-level `title`), guarded by `version` (`CONFLICT` if stale); a failing op writes nothing, and one batch may not mix `archive` with `restore` (opposite crash-safety rename orders). Surfaced to the architect as flat single-op MCP tools (`createRoadmapItem` … `setRoadmapTitle`), each sending a one-op batch |
| `GET` | `/api/config/shortcuts` | Get resolved shortcuts config (global + per-pane keybindings) |
| `GET` | `/api/fs/tree?path=<absolute path>` | List one directory level (name, kind, size, mtime, best-effort gitignore `ignored` flag); not architect-scoped — takes any absolute path. Capped at 2000 entries with a `truncated` flag; symlink entries are reported, not followed |
| `GET` | `/api/fs/file?path=<absolute path>` | Read a file's raw bytes with a sniffed `Content-Type`, `X-File-Size`, and `X-File-Truncated` headers (2 MiB read cap); not architect-scoped |
| `PUT` | `/api/fs/file?path=<absolute path>[&create=true]` | Write a file from a JSON `{ "content": "<string>" }` body (atomic temp-file+rename); returns `{ path, size, mtime }`. Default mode is overwrite-only and preserves the file's mode (`NOT_FOUND` if missing); `create=true` inverts it — the target must NOT exist (`CONFLICT` if it does), parent dirs are created, mode is `0644`. Either way `VALIDATION` for a directory or over the 2 MiB cap; not architect-scoped |
| `GET` | `/api/fs/search?path=<absolute path>&q=<query>&limit=<1..100>` | Ranked filename search under a root (case-insensitive substring/subsequence, basename matches first). Git-aware: roots inside a work tree list via `git ls-files` (tracked + untracked-unignored); plain roots are walked, with nested repos listed the same way, so gitignored files never appear. 100-result cap, 200k-candidate walk budget with a `truncated` flag |
| `GET` | `/api/fs/search-content?path=<absolute path>&q=<query>&limit=<1..200>` | Case-insensitive fixed-string **content** search under a root, returning `{ path, line, text }` per matched line (lines capped at 500 bytes). Git-aware: a root inside a work tree greps via one `git grep -I --untracked` subprocess, streamed and killed at the cap; other roots scan the same git-aware candidate set in-process, skipping binaries and files over 1 MiB. 200-match cap with a `truncated` flag |
| `POST` | `/api/sessions` | Create a durable session for architect planning, ticket work, or freeform exploration |
| `GET` | `/api/sessions` | List sessions with their current run, if any |
| `GET` | `/api/sessions/{id}` | Get one session |
| `POST` | `/api/sessions/{id}/runs` | Start a run for a session using an agent profile; ticket runs move the ticket backlog → progress on successful launch |
| `POST` | `/api/sessions/{id}/conclude` | Conclude a session's running run, publish `ended`, kill its PTYs, and delete the session row; architect conclude returns `CONFLICT` if same-architect ticket sessions are still running |
| `POST` | `/api/sessions/{id}/discard` | Discard a ticket session without writing a conclusion: move the ticket progress → backlog, publish `ended` with `raw.lifecycle=discarded`, kill PTYs, delete the run, and delete the session row |
| `POST` | `/api/sessions/{id}/intents/conclude-session` | **Blocking.** Raise a conclude intent: render the structured input into the canonical `conclusion.md` body, publish `intent`/`required`, and block until the user answers or the policy fires. Returns an intent resolution. Called by the MCP conclude tools (`concludeArchitectSession`, `concludeTicketSession`, `concludeFreeformSession`). |
| `POST` | `/api/sessions/{id}/intents/create-work-ticket` | **Blocking.** Raise a createWorkTicket intent; the ticket is written only on approval. Session-scoped so the architect key comes from the stored session, never the request. Called by the MCP `createWorkTicket` tool. |
| `POST` | `/api/sessions/{id}/intents/{intentID}/approve` | Approve a pending intent and run its side effect. Called by the desktop app. |
| `POST` | `/api/sessions/{id}/intents/{intentID}/deny` | Deny a pending intent with a reason. The side effect never runs; the blocked agent call returns `outcome: denied_by_user`. Called by the desktop app. |
| `GET` | `/api/sessions/{id}/tabs` | Get the resolved right-pane tab layout for a session's current run |
| `GET` | `/api/sessions/{id}/ticket` | Get the associated ticket for a ticket session (returns NOT_FOUND for non-ticket sessions) |
| `POST` | `/api/sessions/{id}/terminals` | Create a new user terminal in a session's current run using the resolved default shell |
| `GET` | `/api/sessions/{id}/terminals` | List all terminals for a session's current run |
| `DELETE` | `/api/sessions/{id}/terminals/{uuid}` | Kill a specific user terminal by UUID |
| `POST` | `/api/sessions/{id}/browser-tabs` | Open a new browser tab (omit `tab_id`) or navigate an existing one (`tab_id` + `target`); called by the MCP `previewInBrowserTab` tool |
| `DELETE` | `/api/sessions/{id}/browser-tabs/{tabID}` | Close a browser tab by ID |
| `GET` | `/api/sessions/{id}/events` | Stream structured session events over SSE |
| `WS` | `/ws/session/{id}/terminal/{uuid}` | Stream PTY output and send terminal input for a terminal UUID |

The `HIVERYN_HOME`-level config endpoints (profiles, architect registry, tabs, shortcuts) are read-only — edit `HIVERYN_HOME/*.yaml` directly unless you launched with `--config`. The per-architect `hiveryn.yaml`, however, is editable through the `/api/architects/{key}/config/...` endpoints above (surfaced to the architect as MCP tools), which own the yaml wiring, path resolution, and validation: every write is validated against the same rules the loader enforces and refused if it would fail to load. Changes in `variants.yaml`, `architects.yaml`, `tabs.yaml`, `shortcuts.yaml`, and each `hiveryn.yaml` apply without restarting the daemon.

Ticket mutations are status-gated: backlog tickets can be edited, metadata-updated, moved, or deleted; backlog or progress tickets can be concluded (worker conclusion still requires progress; architect-driven `move-to-done` accepts either, and rejects `progress → done` if a worker session is currently running); done tickets are read-only.

Session responses expose a durable session plus its current run, if any. Each session stores the create-time contract: `id`, `architect_key`, `session_type`, `context_id`, `prompt`, primary `workdir`, normalized `additional_repos`, resolved `additional_workdirs`, optional `instructions`, and lifecycle metadata. Ticket scope is resolved once from `hiveryn.yaml`; run creation copies that snapshot, and initial launch, daemon restore, and unexpected-terminal resume never re-resolve mutable ticket or workspace configuration. `POST /api/sessions/{id}/runs` returns the created `run`, its `main_terminal_id`, and a `ws_url` so the desktop can attach immediately. If the main agent PTY exits unexpectedly, the daemon automatically resumes the running run from its stored `native_id` (or id-less resume — a session picker — when `native_id` is empty), emits a `main_terminal_resumed` session event with the new `main_terminal_id`, and leaves the run status as `running`. Whenever a run's mapped agent status transitions (`active`/`idle`/`waiting`/`stopped`), the daemon emits a `{ type: "agent_status", status }` session event (emit-on-change only, persisted so it replays in the connect backlog) — distinct from the overloaded `type=status` events — letting the desktop drive a per-session tab icon without polling. A failed resume — on startup restore or after a PTY exit — marks the run failed (`restore_failed` on restore, `launch_failed` on PTY-exit resume), logs the error at ERROR level, and for ticket sessions moves the ticket back to `backlog`; it never crashes the daemon, and startup continues regardless of individual failures. `GET /api/sessions/{id}/tabs` returns the canonical right-pane layout using `type`, `id`, `command`, and `status` for terminal tabs, and `type`, `id`, and `target` for browser tabs. `POST /api/sessions/{id}/terminals` accepts an empty JSON object and always launches the session's default shell in the current run workdir. `POST /api/sessions/{id}/browser-tabs` takes `{ "target": "...", "tab_id": "..." }` — `target` must be a `file://`, absolute path, `http://localhost:*`, or `https://` URL; omitting `tab_id` opens a new tab (assigned a UUID), passing an existing `tab_id` navigates that tab in place. Both create/navigate and `DELETE /api/sessions/{id}/browser-tabs/{tabID}` publish a `type=status`, `status=tab_changed` session event so the desktop knows to refetch the tab list; browser tab state is in-memory only, same as ad-hoc terminals, and does not survive a daemon restart.

The legitimate explicit session ends are `POST /api/sessions/{id}/conclude` and, for ticket sessions only, `POST /api/sessions/{id}/discard`. Concluding an architect intent fails fast with `CONFLICT` while ticket intents for the same `architect_key` still have a `running` run; the error message lists the blocking intent IDs.

### Discard ticket session

`POST /api/sessions/{id}/discard` is for the desktop "discard worker session" action. It accepts no request body. The target session must be a ticket session with a current run; architect and freeform sessions return a `VALIDATION` envelope. The daemon moves the ticket back to `backlog`, emits a live session SSE event with `type=status`, `status=ended`, `message=session discarded`, and `raw.lifecycle=discarded`, kills all PTYs for the session, deletes the `session_runs` row, and deletes the `sessions` row. No `conclusion.md` is written and no commit/rejection invariant is checked. Git changes made by the agent are not reverted.

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

### Intent approval flow

Agent tool calls that mutate user-visible state route through the **intent system** so the desktop user can approve them before they take effect. Two tools are routed today: the conclude tools (`concludeArchitectSession`, `concludeTicketSession`, `concludeFreeformSession`, role-scoped so a session only sees its own) and `createWorkTicket` (registered for all three session types). Each takes **discrete structured fields** rather than a freeform body; the daemon renders/validates them and, for conclusions, produces the canonical `conclusion.md`. See "Structured conclusions" below.

An intent is a pending tool call awaiting the user's answer. The write lives daemon-side and runs only on approval, so the agent cannot bypass it.

1. The MCP tool calls its blocking endpoint — `POST /api/sessions/{id}/intents/conclude-session` or `.../intents/create-work-ticket` — held open by an untimed HTTP client. No polling.
2. The daemon validates the input up front (required conclusion fields; ticket outcome invariants; a non-blank title and a configured repo for createWorkTicket). A failure returns a `VALIDATION` error to the agent immediately, before any intent is raised — an invalid call never shows a dialog.
3. The daemon raises a pending intent, publishes a durable `type: "intent"` / `status: "required"` SSE event carrying `{intent_id, intent_type, summary, payload, origin, wait_seconds, policy}` (origin = architect key / session / ticket, so the desktop can render one popup across all sessions), and blocks on a channel.
4. The desktop presents a popup with a countdown seeded from `wait_seconds`.
5. The desktop calls `POST /api/sessions/{id}/intents/{intentID}/approve` or `.../deny`.
6. On approve: the daemon runs the tool's side effect and returns the result. On deny: the blocked call returns `outcome: denied_by_user` with the reason — **not** a tool error, so the agent knows not to retry.
7. On timeout: the tool's hardcoded policy fires. All current tools are `wait-then-allow`, so the action runs and the agent is told `auto_approved`.
8. Every resolution publishes a durable `type: "intent"` / `status: "resolved"` event (`raw.outcome` + optional `reason`/`result`), paired to the `required` event by `intent_id`. The `required` event replays on every desktop (re)connect but the pending intent lives only in memory, so the resolution event is its durable counterpart and the log always converges to "no popup". On startup the intent store is empty, so `ReconcileIntents` resolves any still-unpaired `required` (orphaned by a restart) with `outcome: error`.

**Idempotency.** Intents dedup within a 1h window on `hash(session, tool, normalized-args)`. A retry (e.g. after an agent-runtime tool-call timeout) attaches to the live intent, or replays the resolved outcome if it already finished — so a duplicated `createWorkTicket` yields one ticket, not two. Because a tool-call timeout is itself a context cancellation, the intent does not die with the caller's context; a detached owner goroutine holds the wait window.

**Outcome catalog.** Every intent resolves to exactly one of `approved`, `auto_approved`, `denied_by_user`, `auto_denied`, `error`. The two `denied_*` outcomes read as "do not retry"; `error` reads as "maybe retry". Denials come back as successful tool results carrying the outcome, never as tool errors.

The direct `POST /api/sessions/{id}/conclude` and `POST /api/architects/{key}/tickets` endpoints remain available without approval — they are the desktop's own paths, since approval gates the agent, not the user. `repo` is the required primary configured repo key from the architect's `hiveryn.yaml`; `additional_repos` is an optional unique, non-overlapping key array. Filesystem paths are never accepted in ticket scope.

Direct `/conclude` request:

```json
{
  "body": "Implemented multi-repo conclusion support.",
  "commits": [
    {"sha": "abc123", "repo": "daemon"},
    {"sha": "def456", "repo": "desktop"}
  ],
  "outcome": "completed",
  "rejection_reason": ""
}
```

#### Structured conclusions

The MCP conclude tools (and therefore `intents/conclude-session`) take **discrete structured fields** instead of a freeform `body`; the daemon renders them into the canonical `conclusion.md`. The frontmatter metadata and the read-path shape (frontmatter + rendered `body`) are unchanged — this is an input contract, not a persisted structured copy. Every presentational section is a **Markdown string** the agent authors itself (bullets/prose as text) — no conclude section is an array, so none can be dropped by the MCP client's required-array serialization bug. Only `commits` stays a structured array, because it is persisted and read back as data. Required fields are rejected if blank; a required section with nothing to report takes the literal Markdown `"None"`. Each type has its own canonical section order:

- **`concludeArchitectSession`** — `summary`*, `narrative`*, `tickets_touched`, `decisions`, `config_changes`, `user_priorities`, `open_questions`, `next_steps`†
- **`concludeTicketSession`** — `summary`*, `outcome`* (`completed`/`exploratory`/`rejected`), `implementation`* (the writeup — required for `completed`/`exploratory`, renders as "Implementation" or "Findings" respectively; omitted for `rejected`), `deviations`, `verification`, `follow_ups` (Markdown referencing candidate follow-up ticket IDs), `open_questions` — plus the `commits`/`rejection_reason` frontmatter metadata (`commits` required for `completed`, `rejection_reason` required for `rejected`)
- **`concludeFreeformSession`** — `summary`*, `findings`*, `recommendations`†, `open_questions`† — plus optional `commits`; `outcome` is disallowed (ticket-only concept)

(`*` = required; `†` = required, `"None"` accepted. All section fields are Markdown strings; `commits` is the only array.) Example `intents/conclude-session` request for a ticket session:

```json
{
  "summary": "Implemented multi-repo conclusion support.",
  "implementation": "Added the render layer and split the conclude tools.",
  "verification": "go test ./... passed",
  "follow_ups": "- ticket-52: wire the desktop approval surface",
  "commits": [
    {"sha": "abc123", "repo": "daemon"},
    {"sha": "def456", "repo": "desktop"}
  ]
}
```

Ticket/conclusion output always returns the structured shape, including when reading older `conclusion.md` files that stored commits as flat SHA arrays:

```json
{
  "started_at": "2026-05-18T14:00:00Z",
  "concluded_at": "2026-05-18T14:30:00Z",
  "agent": "codex",
  "profile": "codex",
  "outcome": "completed",
  "rejection_reason": "",
  "commits": [
    {"sha": "abc123", "repo": "daemon"},
    {"sha": "def456", "repo": "desktop"}
  ],
  "body": "Implemented multi-repo conclusion support."
}
```

`conclusion.md` files written before the `outcome` field existed have no `outcome` key; the read path infers it once from the legacy `rejected` boolean (`true` → `rejected`, absent/`false` → `completed`, since `exploratory` did not exist as a concept yet) without rewriting the file.

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
