# Ticket workers over SSH

Hiveryn runs ticket workers in existing remote repositories. Tickets, approvals,
questions, Actions and conclusions stay on the laptop. No remote Hiveryn binary,
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
connection state from worker activity. Lost connections clear stale activity.
A missing or exited managed worker is reported as unavailable without starting
a replacement; inspect its retained tmux pane, then discard/recreate as needed.
Stop, discard and conclusion first confirm owned remote resources are stopped.
If a host is unreachable, the local session remains and the error explains that
termination was not confirmed. Retry after restoring connectivity.

A reverse SSH tunnel maps a session's stable remote loopback port to a dedicated
local worker gateway. Only authenticated worker MCP tools and session-scoped
hooks are exposed, never the general daemon API or architect tools. The random
port and token survive daemon restart. A port conflict or refused forwarding is
reported; the worker remains behind its launch gate until attachment succeeds.
Private runtime files live under `~/.local/state/hiveryn-workers/<session UUID>`
and are removed during confirmed cleanup. SQLite holds private credentials and
ownership records; the database is restricted to the local account.

If Hiveryn MCP is unreachable, workers are instructed to stop work and wait.
This is agent guidance, not process suspension. An interrupted mutation may have
succeeded; do not blindly replay it. Existing approvals, tool outcomes and
one-hour questions retain their usual semantics. Remote Git working-tree,
commit-diff and conclusion-commit checks run in the actual remote checkout.

Project documents and selected workflows arrive as embedded text. Laptop path
references and local Action output folders are not automatically accessible to
remote workers. Remote architects, remote Actions, cross-machine writable
scopes, file browsing and offline MCP delivery are not supported.

## Verification

`python3 scripts/remote-smoke.py` runs an isolated Docker SSH server with fresh
keys and known-hosts, a disposable daemon/database and fixture provider CLIs.
It requires Docker, Go, Python 3, `websocket-client` and OpenSSH. It does not read
live Hiveryn/SSH configuration or model credentials. The daemon is race-enabled.

The fixture uses Alpine Linux 3.22, OpenSSH 10 and tmux 3.5a, with a macOS client.
Unit tests cover provider target filesystems, scope validation, shell quoting
and gateway authorization. Fixture evidence does not establish real provider
CLI behavior, interactive Electron acceptance, other remote operating systems,
or a minimum supported version for those tools. Run the selected authenticated
CLI on the intended host before using it for work.
