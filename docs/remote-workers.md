# Ticket workers over SSH

Hiveryn runs ticket workers in existing remote repositories. Tickets, approvals,
questions and conclusions stay on the laptop. No remote Hiveryn binary,
daemon, architect workspace copy or checkout synchronization is needed.

Configure an SSH alias first. Verify its host key and noninteractive login;
Hiveryn uses `BatchMode=yes` and the account's SSH configuration for user, port,
identity, proxy/jump hosts and authentication. The server must allow remote TCP
forwarding with loopback listeners (`AllowTcpForwarding yes`, `GatewayPorts no`).
Do not configure the server to force forwarded listeners onto public interfaces.

```yaml
# ~/.hiveryn/machines.yaml (or $HIVERYN_HOME/machines.yaml)
buildbox:
  ssh: buildbox
```

```yaml
# Project hiveryn.yaml
repos:
  desktop: /local/path/desktop
  backend:
    path: /home/kareem/backend
    machine: buildbox
```

Machine keys and aliases use letters, digits, dots, underscores and hyphens,
starting with a letter or digit. A missing machine means local. Local strings
and local `{path: ...}` objects support `~`; remote paths require full absolute
POSIX paths. They are never expanded or inspected on the laptop. All primary
and additional writable repositories in a ticket must use the same machine;
local is a separate location. Creation, scope edits and launch enforce this.
Repair invalid configuration before creating new scopes or workers.

Prepare the remote account with:

- A POSIX shell, OpenSSH server, tmux and Git, available to SSH commands.
- The selected `claude`, `codex` or `opencode` CLI, installed and authenticated
  there. Only the selected CLI is required. Current Claude/Codex hooks also
  require Node.js. Verify the CLI and Node PATH in a noninteractive SSH command.
- Existing writable checkouts, Git identity/authentication, build/test tools,
  and network access to the provider. Install/authenticate `gh` when a workflow
  uses GitHub; install `rtk` or other tools only when repo guidance requires them.

Variants are machine-scoped. Give each remote variant the machine key; a
variant without `machine` is local only and is never offered or launched
remotely. Settings are not copied between machines, so define a separate
variant per machine:

```yaml
# ~/.hiveryn/variants.yaml
codex-buildbox:
  agent: codex
  model: gpt-5
  machine: buildbox
  env:
    CODEX_HOME: /home/kareem/.codex   # a path on buildbox
```

A worker's variant must match its repositories' machine: the desktop spawn
picker lists only those, and the daemon refuses others for desktop launches and
`spawnTicketWorker` requests (again when an approved request launches), naming
the target machine and eligible variants. Architects are local and use local
variants. Existing
workers keep the variant frozen at launch; editing `variants.yaml` never
switches or migrates a running worker or its reattachment.

Variant environment values, config directories, executable arguments and MCP
server configuration are interpreted on the remote account. Laptop credentials
are not copied and inherited SSH `SendEnv` variables are suppressed. Provider
hook setup preserves unrelated remote configuration. OpenCode bearer MCP uses
its documented `{env:NAME}` syntax with OAuth disabled for explicit tokens:
[OpenCode MCP configuration](https://opencode.ai/v2/docs/mcp-servers).

## Lifecycle and tools

Each worker owns a separate tmux server socket named `hiveryn-<session UUID>` and
one `worker` session. Repository terminals use `hiveryn-aux-<terminal UUID>`.
These servers ignore the account's tmux configuration and hide the status bar,
so desktop terminal dimensions pass through unchanged. Hiveryn never kills an
unrelated tmux server. Extra terminals connect to the selected repository's
machine, including when opened from a local architect session.

SSH disconnect, laptop sleep and daemon restart leave remote processes alive.
Hiveryn reattaches existing workers and auxiliary shells; reconnect never
launches another provider. Recovered auxiliary terminals return as ordinary
tabs. The session banner separates
connection state from worker activity. Lost connections clear stale activity,
including the Claude input gate: hooks sent while the tunnel was down are lost,
so input waits for a new hook event or the remote transcript's recorded turn end
(a re-arming waiter is not idle evidence).
A missing or exited managed worker is reported as unavailable without starting
a replacement; inspect its retained tmux pane, then discard/recreate as needed.
Stop, discard and conclusion first confirm owned remote resources are stopped.
If a host is unreachable, the local session remains and the error explains that
termination was not confirmed. Retry after restoring connectivity.

Launching a worker validates the repositories and prepares the worker over SSH.
A launch is bounded by the daemon (two minutes) rather than by the requester: a
client that stops waiting does not interrupt it, and the session appears once it
starts. A second launch of a session already launching is rejected as a
conflict. If preparation fails, the provider has not started (it waits behind
its launch gate), so the owned tmux server and runtime directory are removed and
the same session can be launched again; if that cleanup cannot be confirmed, or
an owned server from an earlier attempt already exists, the error says so and
the session must be discarded before spawning again.

### Launch cost

A fresh SSH connection's handshake usually dominates a remote command (on the
host that motivated this, ~7.5 s for `sh -c true`, ~6.2 s to establish a
connection and ~1.3 s per command over it). Preparation therefore pays the
handshake once and keeps its command count fixed:

- **Launch-scoped shared connection** (`remoteexec.Connection`). The launch
  starts one OpenSSH master for the session's alias (`ControlMaster=yes`,
  `ControlPersist=no`) with its control socket in a fresh 0700 directory of the
  daemon's account, and its preparation commands (repository check, probe,
  provider setup, launch files, tmux creation, the attach pre-check, repository
  terminals and failed-launch cleanup) run over it. It is closed (master killed,
  directory removed) when the launch returns, success or failure. User, port,
  identity, proxy/jump and authentication still come from the alias's SSH
  configuration; only the master/control options of these commands are fixed,
  overriding any `ControlMaster`/`ControlPath` of the alias for them alone. The
  attached worker PTY and every reconnect open their own connections, so nothing
  that outlives the launch depends on it. If the master cannot be established
  (or exits) the launch continues over direct connections and logs why; the
  first failing command still reports its original cause.
- **Batched probes.** All repository directories are checked in one command
  that names every path that is missing or not a directory; a failed SSH command
  is reported as such, never as a missing repository. Prerequisites (`tmux`, the
  provider CLI, Node for Claude/Codex) and the target environment are one
  command that names every missing prerequisite. The private runtime directory
  is created by the first file written into it, after its ownership record. An
  unchanged provider setup is one read.

| 11-repo Claude worker | Handshakes | Commands | Estimate at the measured cost |
| --- | --- | --- | --- |
| Before | 24 (23 commands + attach) | 23 | ~180 s — exceeded the 2 min bound |
| After, changed setup | 2 (master + attach) | 9 (+1 Claude waiter settings since 28 Oct) | ~6 + 9 × 1.3 + 7.5 ≈ 25 s |
| After, unchanged setup | 2 | 7 | ≈ 23 s |
| After, master refused | 11 (failed master + 9 + attach) | 9 | ≈ 6 + 9 × 7.5 + 7.5 ≈ 81 s |

Counts are the same for one repository; before the change one repository took
13 commands and each further repository one more. Estimates are arithmetic from
the measured per-connection costs, not a timed launch on that host.

Each remote launch logs `[spawn] remote launch prepared` (or `failed`) with its
duration, whether the shared connection was used, how long it took to become
ready, and per-stage elapsed time and command counts (`validate repositories`,
`prerequisites`, `<agent> setup`, `prepare launch`, `create tmux`, `attach`,
`repository terminals`, `cleanup`). Logs never include scripts, settings,
environment values, tokens or file contents. When the launch's own bound runs
out the error says `launch did not complete within …` and names the stage the
budget ran out in with the same timing; a remote command that genuinely failed
keeps its own exit status and stderr instead.

Opening a remote repository terminal creates its tmux server over SSH, likewise
detached from the requester and bounded by the daemon (45 seconds). A completed
terminal appears as a tab even if the requester stopped waiting; a second
creation in the same session while one is in flight is a conflict. A failed or
timed-out creation removes the possibly created server before reporting; if that
removal cannot be confirmed, its ownership record is kept and the server is
removed when the session ends. Closing a terminal drops its record once removal
is confirmed. Remote working-tree and commit diffs are bounded at 60 seconds and
each run as one SSH invocation (`internal/gitdiff/batch.go`): the operations a
local diff runs separately — directory check, staged/unstaged/untracked diffs,
or commit resolution and its diff — run in one remote script that reports
each one as a length-prefixed record and stops at the first failure, which is
reported by operation with its stderr, never as an empty diff. A missing
remote repository directory is a not-found, as it is locally.

Approvals and session ends follow the same rule: once accepted, the operation
belongs to the daemon. An approved intent (a remote conclusion, a worker launch)
runs detached from both the approving request and the requesting agent, bounded
at three minutes, and its outcome is recorded on a context of its own, so a
client that stops waiting never strands it or suppresses its resolved event.
While it runs the intent is published as `resolving`; answering it again is a
conflict. Conclusion, discard and stop confirm remote termination within a
two-minute daemon bound, one at a time per session (a retry meanwhile is a
conflict). A failed approval resolves as `error` with its original cause and is
not replayed: the worker's retry is a fresh request the user approves again,
while an approved outcome is replayed so a retry never repeats it. A worker
killed by its own approved conclusion loses only the reply, never the outcome.
Agent-facing tool calls keep the 65-minute provider deadline; the agent's own
waits (approval window, one-hour questions) are
unchanged.

A reverse SSH tunnel maps a session's stable remote loopback port to a dedicated
local worker gateway. Only authenticated worker MCP tools and session-scoped
hooks are exposed, never the general daemon API or architect tools. The random
port and token survive daemon restart. A port conflict or refused forwarding is
reported; the worker remains behind its launch gate until attachment succeeds.
Private runtime files live under `~/.local/state/hiveryn-workers/<session UUID>`
and are removed during confirmed cleanup. SQLite holds private credentials and
ownership records; the database is restricted to the local account.

The conversation API reads and sends to remote Claude and Codex workers over
SSH (no remote install; remote OpenCode is explicitly unsupported there): see
[remote workers in the conversation API](conversation-api.md#remote-workers-over-ssh).
Its commands run over a session-scoped OpenSSH master separate from the
launch's, and Claude's input waiter uses the gateway's token-scoped
`/hooks/<token>/claude/wait` route, bound to the gateway's own session. Claude
workers therefore carry one more launch file (the per-launch waiter settings):
one more remote write than the table above.

If Hiveryn MCP is unreachable, workers are instructed to stop work and wait.
This is agent guidance, not process suspension. An interrupted mutation may have
succeeded; do not blindly replay it. Existing approvals, tool outcomes and
one-hour questions retain their usual semantics. Remote Git working-tree,
commit-diff and conclusion-commit checks run in the actual remote checkout.

Project documents and selected workflows arrive as embedded text. Anything
else a remote worker needs from the laptop — documents the Overview links to,
investigations, specs, ticket conclusions — or from a repo on another machine,
it reads on demand with `readContext`/`listContext` through its authenticated
gateway: the daemon reads the architect workspace locally and each repo on its
own configured machine, scoped to the gateway's fixed session
(`docs/context-references.md`). Ticket references (`architect:<path>`,
`<repoKey>:<path>`, ticket ids) are likewise inspected where each target lives,
not on the ticket's primary machine: remote targets over one detached SSH call
per machine (a read waits at most 2 s, then reports `unverified` until the
answer arrives and the board is told to refresh; answers are reused for 15 s,
then served while refreshed for up to 10 min). A target absent there is
`missing` (`MISSING_REFERENCE`); one that cannot be inspected (unreachable
host, permission) is `unverified` with its `error` (`UNVERIFIED_REFERENCE`) and
is never checked on another machine instead. Laptop files are not copied to remote workers; reads are live and bounded (no
bulk or binary materialization). Remote architects, cross-machine writable
scopes, file browsing and offline MCP delivery are not supported.

## Verification

`python3 scripts/remote-smoke.py` runs an isolated Docker SSH server with fresh
keys and known-hosts, a disposable daemon/database and fixture provider CLIs.
It requires Docker, Go, Python 3, `websocket-client` and OpenSSH. It does not read
live Hiveryn/SSH configuration or model credentials. The daemon is race-enabled.
Final phases inject SSH latency to check launches, diffs and repository
terminals longer than five seconds, requesters that stop waiting, concurrent
launches/terminals, failed-preparation cleanup and unconfirmed terminal cleanup.

The fixture uses Alpine Linux 3.22, OpenSSH 10 and tmux 3.5a, with a macOS client.
Unit tests cover provider target filesystems, scope validation, shell quoting
and gateway authorization. `internal/sessionruntime/remote_launch_test.go`
launches an 11-repository Claude worker through `CreateRun` against the fake
`ssh` with per-connection latency scaled from the measured cost (fresh 187 ms,
shared 33 ms, bound 3 s): it asserts one master and a fixed command count for
one and eleven repositories, an unchanged second setup without a write, the
fallback without multiplexing, missing repositories and prerequisites, a
changed machine, a failed setup write, budget exhaustion, cleanup and retry
with exactly one tmux creation. The same test against the previous code fails
its bound during the prerequisite checks after 23 fresh connections. Remote reference inspection and Git diffs are
covered by unit tests against `internal/remoteexec/sshtest`, a fake `ssh` that
runs each command locally in a per-machine home directory. Fixture evidence does not establish real provider
CLI behavior, interactive Electron acceptance, other remote operating systems,
or a minimum supported version for those tools. Run the selected authenticated
CLI on the intended host before using it for work.
