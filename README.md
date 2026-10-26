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

An unknown `hiverynd` command is a usage error; only no arguments, bare flags or `serve` start the daemon.

## Configuration

The daemon reads five YAML files from `HIVERYN_HOME` (default `~/.hiveryn`). Only `config.yaml` is required; the others default to empty when missing. `variants.yaml`, `architects.yaml`, `tabs.yaml`, and `shortcuts.yaml` are reloaded on demand, so changes to profiles, the architect registry, tab layouts, and keybindings do not require a daemon restart. The per-architect `hiveryn.yaml` files those entries point at are reloaded the same way. Passing `--config` points `config.yaml` elsewhere and, because config loading is directory-scoped, also changes where `variants.yaml`, `architects.yaml`, `tabs.yaml`, and `shortcuts.yaml` are read from.

### `config.yaml` — daemon core

```yaml
shell: /bin/zsh
port: 4201
bind_address: 127.0.0.1
log_level: info
intent_wait_timeout: 20
question_escalation_delay: 120
archive_agent_events: false
```

| Field | Default |
|---|---|
| `shell` | `$SHELL`, then `bash` |
| `port` | `4201` |
| `bind_address` | `127.0.0.1` (localhost only) |
| `log_level` | `info` |
| `intent_wait_timeout` | `20` (seconds) — how long a pending intent waits for the user before its tool's policy fires. Values `<= 0` are coerced back to the default. Must stay safely under the smallest agent-runtime tool-call ceiling (~60s). |
| `question_escalation_delay` | `120` (seconds) — how long a ticket worker's `askQuestion` stays unanswered by the user before its project architect is told about it (below). Omit it for the default; `0` disables escalation; a negative value, or one at or above the one-hour question expiry (3600), is a configuration error that stops the daemon from starting. Read at startup like the rest of `config.yaml`. |
| `archive_agent_events` | `false` — set `true` to archive every normalized agentruntime event to per-day JSONL files under `HIVERYN_HOME/archive/agent_events/`. Full `Raw` payloads are preserved unredacted; archival failures never block ingestion. |
| `notifications.ntfy` | absent — phone notifications for agents' `notify` and `askQuestion` (below). |

#### Phone notifications (`notifications.ntfy`)

Agents (architects and ticket workers) reach the user through two MCP tools: `notify(shortMessage)` publishes an alert and returns `Notification sent to user`; `askQuestion(question, answers, recommendedIndex)` publishes an alert, shows the question in the agent's desktop session (suggested answers, the recommended one marked, or free text) and waits up to one hour for the answer. The daemon publishes to [ntfy](https://ntfy.sh):

```yaml
notifications:
  ntfy:
    server: https://ntfy.sh      # or your own ntfy server
    topic: kareem-hiveryn-7f3k   # subscribe to it in the ntfy phone app
    # optional authentication — either an access token…
    token: tk_xxxxxxxxxxxxxxxx
    # …or a username and password
    # username: kareem
    # password: secret
```

1. Install the ntfy app on the phone and subscribe to the topic (on the public `ntfy.sh`, anyone who knows the topic can read it: pick a hard-to-guess name, or use a reserved/protected topic with a token).
2. Add the section to `config.yaml` and restart the daemon; like the rest of `config.yaml` it is read at startup, and an invalid section (missing server or topic, credentials embedded in the URL, both a token and a password) stops the daemon from starting with the field named.
3. Without the section, `notify` and `askQuestion` fail with an error telling the agent the user was not notified; nothing is ever reported as sent unless the ntfy server accepted it (which is publication, not proof of phone delivery). A refused or failed publication returns the server's reason, never the credentials.

Alerts are titled with the project and session (`<project> · architect`, `<project> · <ticket title>`). Pending questions belong to the daemon: they expire after one hour regardless of the agent's client, end when the session ends or the agent stops waiting, and are marked interrupted if the daemon stops — an answer is accepted only while the agent's call is still waiting for it.

#### Escalating unanswered worker questions to the architect (`question_escalation_delay`)

A ticket worker's question the user has not answered after `question_escalation_delay` seconds (default 120) is announced once to the project architect as an agent message (kind `question_escalated`, the same durable outbox as ticket-conclusion notices: delivered into the running architect session, or its next one, never by launching one). The notice names the ticket, the question id, the question, its choices and recommendation, how long it has waited and when it expires, and tells the architect it may answer with `answerQuestion(questionId, answer)` only if it is confident — otherwise nothing is expected, since the user was already alerted by phone. Architect questions never escalate; escalation changes nothing about the question itself (phone-first alert, one-hour expiry, cancellation and restart interruption stay as they are), and a notice that cannot be recorded leaves the user's pending question untouched.

`answerQuestion` is architect-only and authorized by the daemon from the calling session: a running architect session of the question's project, for an escalated ticket-worker question of that project. User and architect may race; exactly one answer wins, and a late, duplicate, expired or unescalated submission gets a `not_answerable` outcome naming what happened rather than overwriting anything. The answer's source travels with the resolution (`answered_by: user|architect`, shown in the desktop) and reaches the worker behind the prefix `Answered by the project architect, not the user:` — an architect answer is never presented as the user's answer or approval. A notice still undelivered when the question resolves is withdrawn.

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

Claude Code's automatic memory is **disabled by default** on every Claude launch and resume — architect and ticket worker sessions alike. A `claude` variant opts in with `claude_auto_memory: true`. The daemon passes the choice to agentruntime, which always sets `CLAUDE_CODE_DISABLE_AUTO_MEMORY` (`1`, or `0` when opted in) in the session's launch env. That value overrides one inherited from the daemon's own environment and the user's `autoMemoryEnabled` setting. Setting that variable under a variant's `env`, or `claude_auto_memory` on a non-claude variant, is a config validation error. The setting is frozen with the run, so a resume keeps the setting it was launched with; a run snapshotted before this setting existed resumes with auto-memory disabled unless its frozen `env` already set the variable to `0`. The session's extra terminal tabs share the launch env, so a `claude` started by hand in one gets the same setting. It does not touch global Claude settings, existing memory files, `CLAUDE.md`/project instructions or transcript persistence. Memory content already in a resumed transcript stays in it.

```yaml
claude-opus-memory:
  agent: claude
  model: claude-opus-5-5
  claude_auto_memory: true   # omit (or false) to keep auto-memory off
```

A variant belongs to one execution location, because its executable, config directories, environment and MCP servers describe that machine. `machine` is optional: omitted means **local only**; `machine: <key>` (a `machines.yaml` key, else a config load error) means only on that machine. An unassigned variant is never reused remotely and nothing is copied between machines — define a separate variant per machine. Architects run locally and take only local variants; a ticket worker runs on its repositories' machine and takes only that machine's variants. The daemon enforces this for desktop launches, and `spawnTicketWorker` (before the approval and again when the approved request launches); a refused variant's error names the target location and its eligible variants. `GET /api/agent-profiles` reports each variant's `machine`. The run's frozen snapshot records the machine; resume and reattachment use the snapshot, so editing `variants.yaml` never switches or migrates a running worker.

```yaml
codex-bk:
  agent: codex
  machine: bk          # runs only on machine bk; its CODEX_HOME is a path on bk
  env:
    CODEX_HOME: /Users/me/.codex
```

For architect sessions using `agent: opencode`, the daemon defines a named OpenCode agent automatically from the built-in architect instructions, using the architect key as the agent name and passing `--agent <architect_key>` at launch. Do not put `--agent` in OpenCode architect variant args; the daemon treats that as a launch error. Ticket OpenCode sessions do not define a named agent.

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
```

The file holds only `name` and `repos`, decoded strictly; any other key (such as a leftover `availableActions` from the removed Actions feature) is a load error. Architect and worker instructions are built into the daemon; project-specific instructions belong in `AGENTS.md`. Invalid configuration edits leave the last valid runtime configuration in service and appear in automatic validation. Worker launch checks the current files and refuses invalid context, while architect startup remains available for repairs.

### Filesystem-authored architect workspaces

```text
hiveryn.yaml                 # name, repos
OVERVIEW.md                  # required durable brief; updated_at UTC frontmatter
AGENTS.md                    # optional project operational instructions
workflows/*.md               # plain Markdown; explicit selection at spawn
tickets/backlog/<id>/ticket.md  # architect-authored
 tickets/progress/           # runtime-owned
 tickets/done/               # runtime-owned, including conclusions
architect-sessions/          # runtime-owned conclusions
```

Ticket identity is its stable `YYYY-MM-DD-HHMM-readable-slug` folder name; status is its directory. Ticket frontmatter requires `title`, `repo`, `created_at` and `updated_at`, with optional `additional_repos` and `references`. Writable repositories must be configured and share a machine. Imported completed records may retain unrecorded or retired repository scope; missing historical scope is reported as a warning, never inferred or made launchable. New and active tickets remain strict. Architects create, edit and delete backlog files directly. Neither architects nor workers have a ticket-creation MCP tool. Workers retain `readTicket` for remote access and tool-managed conclusion.

Directory watches cover configuration, Overview, workflows, tickets and architect conclusions, including atomic saves and newly created directories. After two seconds of quiet, the daemon validates the workspace, refreshes the board through architect events, and sends all current errors through durable architect messaging. Startup and architect-session creation reconcile current files too. Messages live in SQLite and cannot trigger a filesystem feedback loop. Invalid tickets remain visible with diagnostics.

Accepted fingerprints of active/completed records and conclusions live in SQLite (`workspace_records`). External edits, moves, additions and deletions produce explicit conflicts without reverting files or changing runtime session ownership. The first reconciliation establishes the existing history baseline. Daemon lifecycle operations serialize with reconciliation and update only the affected records; unresolved protected-record conflicts refuse mutations. Restore the accepted content from reviewed history to resolve a conflict. A write interrupted before its fingerprint is persisted may require that same explicit reconciliation.

Architect startup uses the approved built-in SYSTEM and the project/session-time kickoff. It does not load custom workspace SYSTEM files. Architects read configuration, Overview, tickets and conclusions with their filesystem tools.

Workers receive the full `OVERVIEW.md` and the selected plain Markdown workflows in their first message, labelled by name, without a workspace write grant. Launch and resume validate current context; ordinary resume does not reinject it. `GET /api/architects/{key}/workflows` lists canonical paths, validity and diagnostics. The desktop starts with an empty selection, and `POST /api/sessions` stores the explicit `workflows` list (empty is valid). Invalid, missing, duplicate, noncanonical or escaping paths fail launch rather than being omitted. `workspace/worker-preflight` reports Overview/configuration blockers separately from unselected workflows.

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

`tabs.yaml` also accepts arbitrary non-`terminal` tab types (e.g. `type: git-diff`, `type: kanban`). These are declarative (no `command`) and are emitted as plain layout entries with no daemon-side lookup; a type the desktop does not render (such as the removed `files` tab) is simply not shown — git diffs, for example, are served natively via `GET /api/architects/{key}/repos/{repoKey}/diff` and `GET /api/architects/{key}/repos/{repoKey}/commits/{sha}/diff`.

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
| `GET` | `/api/architects/{key}/tickets` | List ticket board columns; supports `?q=keyword` (keep tickets whose title, body or conclusion contains it, case-insensitively, in list order — the desktop's kanban search), `?status=backlog\|progress\|done` and `?limit=N` |
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
| `GET` | `/api/architects/{key}/repos/{repoKey}/commits/{sha}/diff` | Diff of a single commit (root commits diff against the empty tree; merge commits diff against their first parent) |
| `GET` | `/api/architects/{key}/workspace/check` | Internal read-only structural report for configuration, Overview, workflows and tickets. Invalid artifacts are diagnostics, not omitted rows. |
| `GET` | `/api/architects/{key}/workspace/worker-preflight` | Launch readiness of current configuration and Overview; selected workflow validity is checked at launch. |
| `GET` | `/api/architects/{key}/workspace/artifacts/{kind}` | Internal schema definitions for HIVERYN_YAML, OVERVIEW and WORKFLOW. No MCP schema tool is exposed. |
| `GET` | `/api/architects/{key}/workflows` | Plain Markdown workflows with canonical paths and diagnostics; explicit selection only. |
| `GET` | `/api/config/shortcuts` | Get resolved shortcuts config (global + per-pane keybindings) |
| `POST` | `/api/sessions` | Create a durable session for architect planning or ticket work (`session_type` is `architect` or `ticket`). Ticket sessions accept `workflows` — the explicit canonical workflow selection, validated against the workspace (see "Workflow selection") |
| `GET` | `/api/sessions` | List sessions with their current run, if any |
| `GET` | `/api/sessions/{id}` | Get one session |
| `POST` | `/api/sessions/{id}/runs` | Start a run for a session using an agent profile; ticket runs revalidate the worker context (project documents + selected workflows) and move the ticket backlog → progress on successful launch |
| `POST` | `/api/sessions/{id}/conclude` | Conclude a session's running run, publish `ended`, kill its PTYs, and delete the session row; architect conclude returns `CONFLICT` if same-architect ticket sessions are still running |
| `POST` | `/api/sessions/{id}/discard` | Discard a ticket session without writing a conclusion: move the ticket progress → backlog, publish `ended` with `raw.lifecycle=discarded`, kill PTYs, delete the run, and delete the session row |
| `POST` | `/api/sessions/{id}/intents/conclude-session` | **Blocking.** Raise a conclude intent: render the structured input into the canonical `conclusion.md` body, publish `intent`/`required`, and block until the user answers or the policy fires. Returns an intent resolution. Called by the MCP conclude tools (`concludeArchitectSession`, `concludeTicketSession`). |
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

Session responses expose a durable session plus its current run, if any. Each session stores the create-time contract: `id`, `architect_key`, `session_type`, `context_id`, `prompt` (the fixed kickoff), primary `workdir`, normalized `additional_repos`, resolved `additional_workdirs`, `workflows` (the canonical workflow selection; empty for non-ticket sessions), optional `instructions` (the built-in role instructions), and lifecycle metadata. Ticket scope is resolved once from `hiveryn.yaml`; run creation copies that snapshot, and initial launch, daemon restore, and unexpected-terminal resume never re-resolve mutable ticket or workspace configuration — they do, however, revalidate the worker context (project documents and the stored selection) against the live workspace before launching. `POST /api/sessions/{id}/runs` returns the created `run`, its `main_terminal_id`, and a `ws_url` so the desktop can attach immediately. If the main agent PTY exits unexpectedly, the daemon automatically resumes the running run from its stored `native_id` (or id-less resume — a session picker — when `native_id` is empty), emits a `main_terminal_resumed` session event with the new `main_terminal_id`, and leaves the run status as `running`. Whenever a run's mapped agent status transitions (`active`/`idle`/`waiting`/`stopped`), the daemon emits a `{ type: "agent_status", status }` session event (emit-on-change only, persisted so it replays in the connect backlog) — distinct from the overloaded `type=status` events — letting the desktop drive a per-session tab icon without polling. A failed resume — on startup restore or after a PTY exit — marks the run failed (`restore_failed` on restore, `launch_failed` on PTY-exit resume), logs the error at ERROR level, and for ticket sessions moves the ticket back to `backlog`; it never crashes the daemon, and startup continues regardless of individual failures. `GET /api/sessions/{id}/tabs` returns the canonical right-pane layout using `type`, `id`, `command`, and `status` for terminal tabs. `POST /api/sessions/{id}/terminals` launches the default shell in a chosen workdir (see below); every auxiliary terminal is an ordinary right-pane tab.

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

Agent tool calls that mutate user-visible state route through the **intent system** so the desktop user can approve them before they take effect. Blocking tools are routed today: the conclude tools (`concludeArchitectSession`, `concludeTicketSession`, role-scoped so a session only sees its own), and `spawnTicketWorker` (architect only). Each takes **discrete structured fields** rather than a free-text body; the daemon renders/validates them and, for conclusions, produces the canonical `conclusion.md`. See "Structured conclusions" below.

An intent is a pending tool call awaiting the user's answer. The write lives daemon-side and runs only on approval, so the agent cannot bypass it.

2. The daemon validates the input up front (required conclusion fields; ticket outcome invariants; valid ticket/variant/workflow selection for spawnTicketWorker). A failure returns a `VALIDATION` error to the agent immediately, before any intent is raised — an invalid call never shows a dialog.
3. The daemon raises a pending intent, publishes a durable `type: "intent"` / `status: "required"` SSE event carrying `{intent_id, intent_type, summary, payload, origin, wait_seconds, policy}` (origin = architect key / session / ticket, so the desktop can render one popup across all sessions), and blocks on a channel.
4. The desktop presents a popup with a countdown seeded from `wait_seconds`.
5. The desktop calls `POST /api/sessions/{id}/intents/{intentID}/approve` or `.../deny`.
6. On approve: the daemon runs the tool's side effect and returns the result. On deny: the blocked call returns `outcome: denied_by_user` with the reason — **not** a tool error, so the agent knows not to retry.
7. On timeout: the tool's hardcoded policy fires. All current tools are `wait-then-allow`, so the action runs and the agent is told `auto_approved`.
8. Every resolution publishes a durable `type: "intent"` / `status: "resolved"` event (`raw.outcome` + optional `reason`/`result`), paired to the `required` event by `intent_id`. The `required` event replays on every desktop (re)connect but the pending intent lives only in memory, so the resolution event is its durable counterpart and the log always converges to "no popup". On startup the intent store is empty, so `ReconcileIntents` resolves any still-unpaired `required` (orphaned by a restart) with `outcome: error`.

**Idempotency.** Intents dedup within a 1h window (counted from resolution) on `hash(session, tool, normalized-args)`. A retry (e.g. after an agent-runtime tool-call timeout) attaches to the live intent, or replays the resolved outcome if it already finished — so a duplicated worker launch yields one session, not two. Because a tool-call timeout is itself a context cancellation, the intent does not die with the caller's context; a detached owner goroutine holds the wait window.

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

## Remote ticket workers

See [SSH machine setup and lifecycle](docs/remote-workers.md) for remote repositories, tmux persistence, worker MCP, verification and limitations.

### Notification recovery

The outbox records the recipient, provider identity and attempt before any
hand-off. Transport failures retain an uncertain `handed` attempt for receipt
inspection, including after restart. Three reserved attempts without a receipt
end in visible failure; a crash before sending can consume an attempt. Pending
preparation/storage failures back off without sending. Failed post-send writes
are retried before further dispatch. Confirmed receipts are retained in memory
until persisted; if both persistence and the process fail, recovery depends on
the provider record still being available. This is bounded recovery, not an
exactly-once guarantee. Provider crash windows remain documented in agentruntime.
