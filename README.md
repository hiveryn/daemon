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

`hiverynd mcp` is intended to be spawned by `agentruntime`; session scoping comes from `HIVERYN_SESSION_TYPE` (`architect` or `ticket`; any other value, including the removed `freeform`, is a startup error).

### Validating an Action

```bash
hiverynd action validate [--json] [--strict] [path]
```

Checks an Action repository (default: the current directory) with the daemon's own definition rules — `.git`, strict `action.yaml` keys and required fields, `suggestions` bounds, and the `KICKOFF.md` placeholders. It runs fully offline: no running daemon, config, database or provider setup, and the path need not be inside `HIVERYN_HOME/actions`. The action name is the directory's base name, as it will be once installed.

Every finding has a stable `code`, a `severity` and the absolute `path` of the file (or action directory) it concerns; `--json` prints them as a report (`path`, `name`, `valid`, `strict`, `passed`, `errors`, `warnings`, `diagnostics`). Errors make the definition invalid, exactly as the runtime refuses to launch it. Warnings are advice that never blocks a launch — currently `SUGGESTIONS_MISSING`, when `suggestions` is absent or empty, recommending 2–3 useful manual launch prompts.

| Exit | Meaning |
|---|---|
| `0` | No errors (warnings allowed unless `--strict`) |
| `1` | Validation failed: errors, or any warning under `--strict` |
| `2` | Usage error, or the path is missing / not a directory (the filesystem error is printed) |

An unknown `hiverynd` command or `action` subcommand is a usage error; only no arguments, bare flags or `serve` start the daemon.

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

For architect sessions using `agent: opencode`, the daemon defines a named OpenCode agent automatically from the built-in architect instructions (plus the workspace's optional `ARCHITECT_SYSTEM.md`), using the architect key as the agent name and passing `--agent <architect_key>` at launch. Do not put `--agent` in OpenCode architect variant args; the daemon treats that as a launch error. Ticket OpenCode sessions do not define a named agent.

Instructions are always additive to the agent's own system prompt: Claude receives them through `--append-system-prompt`, Codex through `developer_instructions`, OpenCode through an instruction file. The kickoff (the first user message) is separate from the instructions on every provider. Repository `AGENTS.md`/`CLAUDE.md` files are picked up natively by each agent and are not touched by the daemon.

### `architects.yaml` — architect registry

A bare `key: path` map. Each value points at an architect workspace directory containing a `hiveryn.yaml`; the architect's name and repos are read from there.

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
availableActions:   # optional
  - demo-evidence
```

The file holds `name`, `repos` and the optional `availableActions`; it is decoded strictly, so any other key — in particular a leftover `prompts:` block — fails to load with an error naming the key. There are no prompt overrides: the architect and worker instructions are built into the daemon (`internal/sessionruntime/prompts/`), the kickoffs are fixed markdown templates in the same directory whose placeholders are daemon-supplied values, and the one per-project customization is the optional `ARCHITECT_SYSTEM.md` at the workspace root, appended to the architect instructions at session start.

`availableActions` names the global Actions (`HIVERYN_HOME/actions/<name>`) this architect may discover and request; omitted means none. Entries must be unique, well-formed action names. A listed name that has no valid definition is not a load error — `getAvailableActions` reports it invalid with its problems, and it cannot be requested until repaired. The list never restricts the user's own manual launches, and there is no default variant: the user picks one when approving each request.

Repo paths may start with `~` or `~/` to reference the user's home directory; they are expanded to absolute paths at config load. A blank repo path is rejected.

The architect edits this file — like the project documents and workflows — with its own file tools, guided by `describeArtifact(HIVERYN_YAML)` and checked by `checkWorkspace`. An invalid edit never replaces the running config: the daemon keeps serving the last config that validated, logs the reload failure at ERROR once per distinct error, reports it under `config` in `GET /api/system/runtime`, and `checkWorkspace` shows the broken file. Worker launch validates `hiveryn.yaml` on disk directly, so a broken edit blocks new ticket sessions (with the error) until it is repaired; architect sessions keep starting so the repair can be made.

### Architect workspace context

Architect and worker sessions read their project context from files in the architect workspace rather than from prompt templates:

```text
hiveryn.yaml                 # name + repo map (above)
PROJECT_OVERVIEW.md          # required; lastUpdatedAt frontmatter
PROJECT_STATE.md             # required; lastUpdatedAt frontmatter
ROADMAP_CURRENT.md           # optional; lastUpdatedAt frontmatter when present
ARCHITECT_SYSTEM.md          # optional collaboration preferences, architect-only
workflows/*.md               # flat; attach: manual | attach: suggested + repos
```

**Architect sessions** start with the fixed kickoff — project name, session start time, configured repos, and `Start by running checkWorkspace.` followed by the board and latest conclusion — plus the built-in architect instructions. When `ARCHITECT_SYSTEM.md` exists and is valid it is appended under a "Project collaboration preferences" heading; when it exists but cannot be loaded (empty, a directory, not UTF-8, unreadable) the instructions say so explicitly with the diagnostics, the daemon logs it at ERROR, and the session still starts so the file can be repaired. An incomplete workspace never blocks architect startup — `checkWorkspace` reports what to repair.

**Ticket (worker) sessions** receive the built-in `WORKER_SYSTEM` instructions (additive; they describe only `createWorkTicket` and `concludeTicketSession`) and a fixed kickoff naming the ticket id, the writable repositories (`key: path`), the canonical paths of the project documents (`PROJECT_OVERVIEW.md`, `PROJECT_STATE.md`, and `ROADMAP_CURRENT.md` only when it exists). The worker reads those documents live in the architect workspace; the workspace is never added to its writable scope. The stored kickoff does not name workflows: at each run launch the daemon appends the full Markdown body (frontmatter stripped, otherwise verbatim) of every workflow selected for the session, each in a `<workflow path="<canonical path>">` block, in selection order — or `Workflows selected for this session: none`. The bodies come from the same read the launch validation just performed and are never stored.

### Workflow selection (launch contract)

The desktop is the only launch authority. Selection is supplied at session creation, persisted with the session, and revalidated at every launch:

Before offering a launch at all, a client can ask `GET /api/architects/{key}/workspace/worker-preflight` whether the workspace's required project context is ready: it runs the launch's own project-context validation without a selection and answers `launchable` plus one actionable `problems` line per finding. It is deliberately not `checkWorkspace.valid` — architect-only artifacts and unselected invalid workflows make that verdict false without blocking a worker.

1. Discover with `GET /api/architects/{key}/workflows?repos=<primary,additional...>`. Each workflow carries its canonical `path`, `attach`, `repos`, `valid`, `diagnostics`, and `suggested` (a valid `attach: suggested` workflow overlapping the scope on any repo). Suggestions are preselected defaults for the user to keep or remove; manual workflows can be added. The daemon never attaches anything on its own and never infers dependencies.
2. Create the session with `POST /api/sessions` `{ "session_type": "ticket", "architect_key", "ticket_id", "workflows": ["<canonical path>", ...] }`. `workflows` may be omitted or empty; it is rejected on non-ticket sessions. Every entry must be the canonical absolute path reported by the listing, directly inside this workspace's `workflows/`, unique, existing, and valid. An invalid, missing, renamed, non-canonical, symlinked-out or duplicated entry fails the request with `VALIDATION` (`field: workflows`) listing every finding; nothing is dropped or substituted. Missing or invalid `hiveryn.yaml`, `PROJECT_OVERVIEW.md` or `PROJECT_STATE.md` fails with `VALIDATION` (`field: workspace`); `ROADMAP_CURRENT.md` is optional and only checked when present. Unselected invalid workflows and `ARCHITECT_SYSTEM.md` never block a worker.
3. The selection is stored on the session as `workflows` (canonical paths, in the order given) and returned on every session read. Runs do not copy it: it belongs to the session like the prompt does.
4. `POST /api/sessions/{id}/runs`, daemon-restart restore and main-terminal resume rerun the same validation against the live workspace before launching. A failure is the same actionable error (a failed restore/resume marks the run failed and moves the ticket back to backlog, as for any resume failure). To repair, fix the files or discard the session and create a new one with an adjusted selection — the stored selection is never edited in place.
5. Hiveryn does not track edits to selected workflows (no content digests). An active agent is not updated by a file edit; tell it about the change. Workflow bodies travel only in a run's first message, never in the instructions (system prompt). Restore and resume continue the agent's conversation as it is, with the stored instructions unchanged: neither the kickoff nor the workflow bodies are re-sent. The selection is still revalidated, so a broken selected file blocks the resume.

The architect also has two read-only workspace tools: parameterless `checkWorkspace` (the workspace is resolved from the session, so an agent cannot address another one) and `describeArtifact(kind)`. Both are architect-only — a worker maintains no workspace.

### Action definitions — `HIVERYN_HOME/actions/<name>`

Each Action is its own Git repository directly under `HIVERYN_HOME/actions`; the directory name is the action name (lowercase letters, digits, `.`, `_`, `-`, at most 64 characters). It is read live and needs two files:

```yaml
# action.yaml — decoded strictly; no other key is allowed
name: demo-evidence            # required, equal to the directory name
description: |                 # required: what the Action does and what the prompt must contain
  Compare synthetic AMS and LDN evidence. Say how many collections to run.
artifacts: |                   # required: the delivered artifact package contract
  summary.md and results.json in the output directory.
suggestions:                   # optional: ready-made prompts for the manual launch form
  - Compare AMS and LDN with three collections.
  - Compare AMS and LDN with six collections and a first-attempt failure.
```

`KICKOFF.md` holds the launch instructions and must contain `{{prompt}}` (the caller's prompt) and `{{output_dir}}` (the fresh output directory); no other `{{…}}` placeholder is supplied.

`suggestions` is a list of plain prompt strings, in the order the launch form shows them — at most 10, each nonblank after trimming, unique and at most 1000 characters (multi-line block scalars are fine). In the Actions window each appears as a chip under the Launch prompt; clicking one replaces the prompt text, which stays editable, and never launches anything. Suggestions are manual-entry conveniences only: they are not part of `getAvailableActions`, architect requests or their approval. Omit the key when there is nothing to suggest. A definition that breaks any rule is listed with its problems (the Actions window shows them) and cannot launch until repaired.

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
| `GET` | `/api/system/runtime` | Get resolved daemon runtime identity and paths, plus `config` — the most recent YAML reload status (`error`/`failed_at` when an on-disk edit is broken and the last valid config is being served) |
| `GET` | `/api/health` | Health check |
| `GET` | `/api/agent-profiles` | List all agent profiles |
| `GET` | `/api/agent-profiles/{name}` | Get one agent profile by name |
| `GET` | `/api/architects` | List configured architects |
| `GET` | `/api/architects/status` | List all configured architects plus their running architect status and nested running ticket worker sessions for desktop command-palette/session pickers |
| `GET` | `/api/architects/{key}` | Get one configured architect by key |
| `GET` | `/api/architects/{key}/tickets` | List ticket board columns; supports `?status=backlog\|progress\|done` and `?limit=N` |
| `POST` | `/api/architects/{key}/tickets` | Create a backlog ticket with required primary `repo` and optional `additional_repos` keys |
| `GET` | `/api/architects/{key}/tickets/{id}` | Get one filesystem-backed ticket by ID |
| `PATCH` | `/api/architects/{key}/tickets/{id}` | Apply a targeted body edit to a backlog ticket |
| `PATCH` | `/api/architects/{key}/tickets/{id}/metadata` | Update frontmatter (`title`, `repo`, `additional_repos`, `references`) for a backlog ticket |
| `DELETE` | `/api/architects/{key}/tickets/{id}` | Delete a backlog ticket folder and its contents |
| `POST` | `/api/architects/{key}/tickets/{id}/move?to=...` | Move a ticket between backlog, progress, and done |
| `POST` | `/api/architects/{key}/tickets/{id}/move-to-done` | Architect-driven ticket completion without a worker session: backlog → done (architect resolved it directly) or progress → done (manually closing a dead/stuck worker session — fails with `CONFLICT` if a worker session is currently running). Writes a `conclusion.md`; requires `outcome` (`completed`/`exploratory`/`rejected`) — `completed` requires `commits`, `rejected` requires `rejection_reason`. Called by the MCP `moveTicketToDone` tool. |
| `GET` | `/api/architects/{key}/events` | Stream architect-scoped `workspace_changed` SSE hints. `reason` is a ticket reason (`ticket_created`/`ticket_updated`/`ticket_moved`/`ticket_deleted`/`ticket_concluded`), or a session reason (`session_started`/`session_ended`). Session reasons carry `session_id` — the only place in any stream where a session is named before a client knows it exists, so it is how a client discovers sessions it did not create (a spawn from another window). No backlog: reconcile on every (re)connect. |
| `GET` | `/api/architects/{key}/conclusions` | List recent conclusions (IDs + timestamps); supports `?limit=N` |
| `GET` | `/api/architects/{key}/conclusions/recent` | Read the most recent architect session conclusion |
| `GET` | `/api/architects/{key}/conclusions/{id}` | Read a conclusion by ID |
| `GET` | `/api/architects/{key}/repos` | List repos for an architect |
| `GET` | `/api/architects/{key}/repos/{repoKey}` | Get one architect repo by key |
| `GET` | `/api/architects/{key}/repos/{repoKey}/diff` | Current working-tree diff (staged + unstaged + untracked) for a repo, read-only |
| `GET` | `/api/architects/{key}/repos/{repoKey}/status` | Changed paths only (`git status --porcelain`, untracked expanded to files), each with its raw `index`/`worktree` columns and `orig_path` for renames — the lightweight sibling of `/diff` for decorating file trees |
| `GET` | `/api/architects/{key}/repos/{repoKey}/commits/{sha}/diff` | Diff of a single commit (root commits diff against the empty tree; merge commits diff against their first parent) |
| `GET` | `/api/architects/{key}/workspace/check` | Structural, read-only check of the file-based architect workspace. Returns the expected/discovered tree (`hiveryn.yaml`, `PROJECT_OVERVIEW.md`, `PROJECT_STATE.md`, optional `ROADMAP_CURRENT.md` and `ARCHITECT_SYSTEM.md`, `workflows/`) with per-entry `exists`, `valid`, the document's own `document_updated_at`, the filesystem `modified_at`, and `code`/`severity`/`path`/`line`/`message` diagnostics — plus ticket totals by status and per-backlog-ticket warnings. Always `200`: a broken workspace is the payload, not an error. Called by the MCP `checkWorkspace` tool. |
| `GET` | `/api/architects/{key}/workspace/worker-preflight` | Read-only answer to "could a ticket session launch into this workspace right now?": `launchable` plus one actionable `problems` line per project-context finding. Runs the launch's own validation (`hiveryn.yaml`, `PROJECT_OVERVIEW.md`, `PROJECT_STATE.md`, and `ROADMAP_CURRENT.md` only when present), so it is narrower than `workspace/check` and never blocks on architect-only artifacts or unselected workflows. Always `200`. Selection validity comes from the workflow listing and is revalidated at launch |
| `GET` | `/api/architects/{key}/workspace/artifacts/{kind}` | Describe one artifact kind (`HIVERYN_YAML`, `PROJECT_OVERVIEW`, `PROJECT_STATE`, `ROADMAP_CURRENT`, `ARCHITECT_SYSTEM`, `WORKFLOW`): schema version, location/naming, fields, rules, and a minimal example, rendered from the same definitions the check executes. `VALIDATION` for anything else — tickets and conclusions keep their own tools and schemas. Called by the MCP `describeArtifact` tool. |
| `GET` | `/api/architects/{key}/workflows?repos=a,b` | Discover flat `workflows/*.md`: canonical path, `attach` (`manual`/`suggested`), `repos`, validity and diagnostics. `repos` is the session's writable repo scope; a valid `attach: suggested` workflow overlapping it on **any** repo comes back `suggested: true`. An empty scope suggests nothing, and an invalid workflow is listed with its diagnostics but never suggested. |
| `GET` | `/api/config/shortcuts` | Get resolved shortcuts config (global + per-pane keybindings) |
| `GET` | `/api/fs/tree?path=<absolute path>` | List one directory level (name, kind, size, mtime, best-effort gitignore `ignored` flag); not architect-scoped — takes any absolute path. Capped at 2000 entries with a `truncated` flag; symlink entries are reported, not followed |
| `GET` | `/api/fs/file?path=<absolute path>` | Read a file's raw bytes with a sniffed `Content-Type`, `X-File-Size`, and `X-File-Truncated` headers (2 MiB read cap); not architect-scoped |
| `PUT` | `/api/fs/file?path=<absolute path>[&create=true]` | Write a file from a JSON `{ "content": "<string>" }` body (atomic temp-file+rename); returns `{ path, size, mtime }`. Default mode is overwrite-only and preserves the file's mode (`NOT_FOUND` if missing); `create=true` inverts it — the target must NOT exist (`CONFLICT` if it does), parent dirs are created, mode is `0644`. Either way `VALIDATION` for a directory or over the 2 MiB cap; not architect-scoped |
| `GET` | `/api/fs/search?path=<absolute path>&q=<query>&limit=<1..100>` | Ranked filename search under a root (case-insensitive substring/subsequence, basename matches first). Git-aware: roots inside a work tree list via `git ls-files` (tracked + untracked-unignored); plain roots are walked, with nested repos listed the same way, so gitignored files never appear. 100-result cap, 200k-candidate walk budget with a `truncated` flag |
| `GET` | `/api/fs/search-content?path=<absolute path>&q=<query>&limit=<1..200>` | Case-insensitive fixed-string **content** search under a root, returning `{ path, line, text }` per matched line (lines capped at 500 bytes). Git-aware: a root inside a work tree greps via one `git grep -I --untracked` subprocess, streamed and killed at the cap; other roots scan the same git-aware candidate set in-process, skipping binaries and files over 1 MiB. 200-match cap with a `truncated` flag |
| `POST` | `/api/sessions` | Create a durable session for architect planning or ticket work (`session_type` is `architect` or `ticket`). Ticket sessions accept `workflows` — the explicit canonical workflow selection, validated against the workspace (see "Workflow selection") |
| `GET` | `/api/sessions` | List sessions with their current run, if any |
| `GET` | `/api/sessions/{id}` | Get one session |
| `POST` | `/api/sessions/{id}/runs` | Start a run for a session using an agent profile; ticket runs revalidate the worker context (project documents + selected workflows) and move the ticket backlog → progress on successful launch |
| `POST` | `/api/sessions/{id}/conclude` | Conclude a session's running run, publish `ended`, kill its PTYs, and delete the session row; architect conclude returns `CONFLICT` if same-architect ticket sessions are still running |
| `POST` | `/api/sessions/{id}/discard` | Discard a ticket session without writing a conclusion: move the ticket progress → backlog, publish `ended` with `raw.lifecycle=discarded`, kill PTYs, delete the run, and delete the session row |
| `POST` | `/api/sessions/{id}/intents/conclude-session` | **Blocking.** Raise a conclude intent: render the structured input into the canonical `conclusion.md` body, publish `intent`/`required`, and block until the user answers or the policy fires. Returns an intent resolution. Called by the MCP conclude tools (`concludeArchitectSession`, `concludeTicketSession`). |
| `POST` | `/api/sessions/{id}/intents/create-work-ticket` | **Blocking.** Raise a createWorkTicket intent; the ticket is written only on approval. Session-scoped so the architect key comes from the stored session, never the request. Called by the MCP `createWorkTicket` tool. |
| `GET` | `/api/sessions/{id}/available-actions` | The calling architect session's `availableActions`, in config order, each enriched with description, artifact contract, validity/problems and any running execution id. A configured name missing from the library is listed invalid, not dropped. Called by the MCP `getAvailableActions` tool. |
| `POST` | `/api/sessions/{id}/intents/execute-action` | **Deferred, returns at once (`202`).** Request one execution of an available Action (`{name, prompt}`): records it `pending_approval` and raises a manual approval with a required variant choice. Returns the `ActionResult` under the execution id, which is also the intent id. `VALIDATION` if the Action is not available to this architect or invalid; `CONFLICT` if it is running. Called by the MCP `executeAction` tool. |
| `GET` | `/api/sessions/{id}/action-results/{executionID}` | The architect-scoped result of a requested execution (status, requested/started/ended times, elapsed seconds once started, denial reason, error, agent summary, output directory once started, agent activity when reported). Another architect's or a manual execution is `NOT_FOUND`. Called by the MCP `getActionResult` tool. |
| `GET` | `/api/sessions/{id}/action-results/{executionID}/wait?timeout_seconds=N` | Bounded long poll (1–30 s, default 30): returns on a status change or at once when final, otherwise the unchanged result with `timed_out: true`. The caller going away never affects the execution. Called by the MCP `waitForActionResult` tool. |
| `POST` | `/api/sessions/{id}/intents/{intentID}/approve` | Approve a pending intent and run its side effect. Called by the desktop app. |
| `POST` | `/api/sessions/{id}/intents/{intentID}/deny` | Deny a pending intent with a reason. The side effect never runs; the blocked agent call returns `outcome: denied_by_user`. Called by the desktop app. |
| `GET` | `/api/sessions/{id}/tabs` | Get the resolved right-pane tab layout for a session's current run |
| `GET` | `/api/sessions/{id}/ticket` | Get the associated ticket for a ticket session (returns NOT_FOUND for non-ticket sessions) |
| `POST` | `/api/sessions/{id}/terminals` | Create a new user terminal in a session's current run using the resolved default shell |
| `GET` | `/api/sessions/{id}/terminals` | List all terminals for a session's current run |
| `DELETE` | `/api/sessions/{id}/terminals/{uuid}` | Kill a specific user terminal by UUID |
| `GET` | `/api/sessions/{id}/events` | Stream structured session events over SSE |
| `WS` | `/ws/session/{id}/terminal/{uuid}` | Stream PTY output and send terminal input for a terminal UUID |

The config endpoints (profiles, architect registry, tabs, shortcuts) are read-only — edit `HIVERYN_HOME/*.yaml` directly unless you launched with `--config`, and edit each architect's `hiveryn.yaml` with file tools (the architect does so guided by `describeArtifact`). Changes in `variants.yaml`, `architects.yaml`, `tabs.yaml`, `shortcuts.yaml`, and each `hiveryn.yaml` apply without restarting the daemon; a change that fails to load is reported, not applied.

The workspace endpoints above are read-only and deterministic, and they mutate nothing. Missing or invalid files stay inspectable rather than blocking architect startup: the check reports what to repair instead of failing. Its verdict is structural only — `valid` says a document has the required shape, never that its contents are true or current, and `lastUpdatedAt` records edit time, not verification. A file caught mid-write is reported as `INCOMPLETE_READ` rather than validated from a torn read, and ticket warnings are diagnostic only and never make a workspace invalid. `describeArtifact` and the check share one definition per artifact kind, so a rule the architect is told about is a rule that is actually enforced.

Ticket mutations are status-gated: backlog tickets can be edited, metadata-updated, moved, or deleted; backlog or progress tickets can be concluded (worker conclusion still requires progress; architect-driven `move-to-done` accepts either, and rejects `progress → done` if a worker session is currently running); done tickets are read-only.

Session responses expose a durable session plus its current run, if any. Each session stores the create-time contract: `id`, `architect_key`, `session_type`, `context_id`, `prompt` (the fixed kickoff), primary `workdir`, normalized `additional_repos`, resolved `additional_workdirs`, `workflows` (the canonical workflow selection; empty for non-ticket sessions), optional `instructions` (the built-in role instructions), and lifecycle metadata. Ticket scope is resolved once from `hiveryn.yaml`; run creation copies that snapshot, and initial launch, daemon restore, and unexpected-terminal resume never re-resolve mutable ticket or workspace configuration — they do, however, revalidate the worker context (project documents and the stored selection) against the live workspace before launching. `POST /api/sessions/{id}/runs` returns the created `run`, its `main_terminal_id`, and a `ws_url` so the desktop can attach immediately. If the main agent PTY exits unexpectedly, the daemon automatically resumes the running run from its stored `native_id` (or id-less resume — a session picker — when `native_id` is empty), emits a `main_terminal_resumed` session event with the new `main_terminal_id`, and leaves the run status as `running`. Whenever a run's mapped agent status transitions (`active`/`idle`/`waiting`/`stopped`), the daemon emits a `{ type: "agent_status", status }` session event (emit-on-change only, persisted so it replays in the connect backlog) — distinct from the overloaded `type=status` events — letting the desktop drive a per-session tab icon without polling. A failed resume — on startup restore or after a PTY exit — marks the run failed (`restore_failed` on restore, `launch_failed` on PTY-exit resume), logs the error at ERROR level, and for ticket sessions moves the ticket back to `backlog`; it never crashes the daemon, and startup continues regardless of individual failures. `GET /api/sessions/{id}/tabs` returns the canonical right-pane layout using `type`, `id`, `command`, and `status` for terminal tabs. `POST /api/sessions/{id}/terminals` accepts an empty JSON object and always launches the session's default shell in the current run workdir.

The legitimate explicit session ends are `POST /api/sessions/{id}/conclude` and, for ticket sessions only, `POST /api/sessions/{id}/discard`. Concluding an architect intent fails fast with `CONFLICT` while ticket intents for the same `architect_key` still have a `running` run; the error message lists the blocking intent IDs.

Auxiliary terminal creation is workdir-explicit: `GET /api/sessions/{id}/terminal-workdirs` returns ordered daemon-authoritative choices with stable identities and daemon-host-relative display paths. `POST /api/sessions/{id}/terminals` requires one returned `workdir_id`; the daemon resolves and revalidates it immediately before launching the PTY and records its identity and display metadata on the terminal tab.

### Discard ticket session

`POST /api/sessions/{id}/discard` is for the desktop "discard worker session" action. It accepts no request body. The target session must be a ticket session with a current run; architect sessions return a `VALIDATION` envelope. The daemon moves the ticket back to `backlog`, emits a live session SSE event with `type=status`, `status=ended`, `message=session discarded`, and `raw.lifecycle=discarded`, kills all PTYs for the session, deletes the `session_runs` row, and deletes the `sessions` row. No `conclusion.md` is written and no commit/rejection invariant is checked. Git changes made by the agent are not reverted.

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

Agent tool calls that mutate user-visible state route through the **intent system** so the desktop user can approve them before they take effect. Two blocking tools are routed today: the conclude tools (`concludeArchitectSession`, `concludeTicketSession`, role-scoped so a session only sees its own) and `createWorkTicket` (registered for both session types). The architect's `executeAction` is a deferred intent instead — see "Architect-requested Actions". Each takes **discrete structured fields** rather than a free-text body; the daemon renders/validates them and, for conclusions, produces the canonical `conclusion.md`. See "Structured conclusions" below.

An intent is a pending tool call awaiting the user's answer. The write lives daemon-side and runs only on approval, so the agent cannot bypass it.

1. The MCP tool calls its blocking endpoint — `POST /api/sessions/{id}/intents/conclude-session` or `.../intents/create-work-ticket` — held open by an untimed HTTP client. No polling.
2. The daemon validates the input up front (required conclusion fields; ticket outcome invariants; a non-blank title and a configured repo for createWorkTicket). A failure returns a `VALIDATION` error to the agent immediately, before any intent is raised — an invalid call never shows a dialog.
3. The daemon raises a pending intent, publishes a durable `type: "intent"` / `status: "required"` SSE event carrying `{intent_id, intent_type, summary, payload, origin, wait_seconds, policy}` (origin = architect key / session / ticket, so the desktop can render one popup across all sessions), and blocks on a channel.
4. The desktop presents a popup with a countdown seeded from `wait_seconds`.
5. The desktop calls `POST /api/sessions/{id}/intents/{intentID}/approve` or `.../deny`.
6. On approve: the daemon runs the tool's side effect and returns the result. On deny: the blocked call returns `outcome: denied_by_user` with the reason — **not** a tool error, so the agent knows not to retry.
7. On timeout: the tool's hardcoded policy fires. All current tools are `wait-then-allow`, so the action runs and the agent is told `auto_approved`.
8. Every resolution publishes a durable `type: "intent"` / `status: "resolved"` event (`raw.outcome` + optional `reason`/`result`), paired to the `required` event by `intent_id`. The `required` event replays on every desktop (re)connect but the pending intent lives only in memory, so the resolution event is its durable counterpart and the log always converges to "no popup". On startup the intent store is empty, so `ReconcileIntents` resolves any still-unpaired `required` (orphaned by a restart) with `outcome: error`.

**Idempotency.** Intents dedup within a 1h window (10 min for `executeAction`, counted from resolution) on `hash(session, tool, normalized-args)`. A retry (e.g. after an agent-runtime tool-call timeout) attaches to the live intent, or replays the resolved outcome if it already finished — so a duplicated `createWorkTicket` yields one ticket, not two. Because a tool-call timeout is itself a context cancellation, the intent does not die with the caller's context; a detached owner goroutine holds the wait window.

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

### Architect-requested Actions

An architect's `executeAction(name, prompt)` is a **deferred** intent (`intent_type: executeAction`, policy `manual`): the call returns immediately with the execution id in `pending_approval`, the desktop shows the request with a required "Agent variant" choice (no default, no timer), and only the user resolves it. The intent id *is* the execution id, so the architect uses one id from request to result.

- **Deny** — the execution becomes `denied` with the user's reason; it never ran.
- **Approve** — the variant is validated before the claim (an invalid choice is a correctable `400` and the request stays pending); then the architect's `availableActions`, the variant, the definition and the single-run rule are rechecked. If another execution won meanwhile, or anything else prevents the start, the execution is `failed` ("could not start: …") without having started. Otherwise it moves `pending_approval → running` and launches like a manual run; from then on the Action lifecycle owns the record, independent of the requesting architect session, and it stays `running` until the agent concludes, the user cancels or it fails. The generic deferred record completing when the launch returns says nothing about the execution.
- **Requesting session ends first** — the pending request is `failed` as never run. **Daemon restart** — pending requests are `failed` as never run; a running execution follows the Action restart rules (restored and kept running, or failed as interrupted).
- Results are read from the durable execution record (never pruned), scoped to the architect: a later session of the same architect can read them, another architect cannot. An identical request from the same session returns the original execution while it is pending and for 10 minutes after it is approved or denied; after that a new request is created (the single-run rule still applies).

Action agents get no Actions tools; they cannot request Actions.

#### Structured conclusions

The MCP conclude tools (and therefore `intents/conclude-session`) take **discrete structured fields** instead of a free-text `body`; the daemon renders them into the canonical `conclusion.md`. The frontmatter metadata and the read-path shape (frontmatter + rendered `body`) are unchanged — this is an input contract, not a persisted structured copy. Every presentational section is a **Markdown string** the agent authors itself (bullets/prose as text) — no conclude section is an array, so none can be dropped by the MCP client's required-array serialization bug. Only `commits` stays a structured array, because it is persisted and read back as data. Required fields are rejected if blank; a required section with nothing to report takes the literal Markdown `"None"`. Each type has its own canonical section order:

- **`concludeArchitectSession`** — `summary`*, `narrative`*, `tickets_touched`, `decisions`, `config_changes`, `user_priorities`, `open_questions`, `next_steps`†
- **`concludeTicketSession`** — `summary`*, `outcome`* (`completed`/`exploratory`/`rejected`), `implementation`* (the writeup — required for `completed`/`exploratory`, renders as "Implementation" or "Findings" respectively; omitted for `rejected`), `deviations`, `verification`, `follow_ups` (Markdown referencing candidate follow-up ticket IDs), `open_questions` — plus the `commits`/`rejection_reason` frontmatter metadata (`commits` required for `completed`, `rejection_reason` required for `rejected`)

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
