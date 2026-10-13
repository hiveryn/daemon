# Actions

An Action is a reusable, agent-operated procedure that delivers an artifact
package. Each one is a Git checkout holding `action.yaml` and `KICKOFF.md`
(validate one with `hiverynd action validate <path>`), registered under a name
in the global registry.

## Registration

```yaml
# ~/.hiveryn/actions.yaml (or $HIVERYN_HOME/actions.yaml)
demo-evidence: ~/.hiveryn/actions/demo-evidence
nova-export:
  path: /home/kareem/actions/nova-export
  machine: buildbox            # a machines.yaml key; omitted means local
```

The registry is the only source of Actions: nothing is discovered from
directories. `~/.hiveryn/actions/<name>` is just the conventional local
checkout. On the first daemon start without an `actions.yaml`, Hiveryn writes
one registering every directory found there, so existing Actions stay
available; a directory whose name is not a valid action name is listed as a
comment instead. The registry is reread on every request and never rewritten
by Hiveryn afterwards.

Registration names an existing checkout; it never clones, pulls or copies
it. Local paths accept `~`. Remote paths are absolute paths on that machine and
are never inspected on the laptop. `action.yaml`'s `name` must equal the
registered name. A project's `hiveryn.yaml` `availableActions` lists the names
its architect and workers may request; that is permission, not installation.

## Where an Action runs

An Action always runs where it is registered, whoever requests it — a local
architect may run a remote Action and a remote worker a local one. Its variant
must be configured for that machine: discovery (`getAvailableActions`, the
Actions window) reports each Action's `machine` and eligible `variants`, and a
launch or request with another machine's variant is refused with the choices.

A remote Action runs like a remote worker (see
[remote-workers.md](remote-workers.md)): an owned tmux server, a tunnelled MCP
gateway that serves only the Action tools, reattachment after disconnects and
confirmed remote termination before the execution ends. Prepare the remote
account the same way (tmux, Git, the provider CLI, `tar`, `find`, `base64`).
Working output directories live in `~/.local/state/hiveryn-actions/runs` there,
and `HIVERYN_HOME/action-runs` locally. One execution of an Action runs at a
time, wherever it runs.

## Concluding: the frozen package

A completed conclusion names `delivery_dir`, the absolute path of the folder
holding the package on the execution machine — normally the output directory
the execution was given. Before the user sees the conclusion, the daemon checks
it there: an existing directory (not a symbolic link) containing at least one
file, only regular files and directories, within the limits. A folder that
fails is returned to the agent with the reason, so it can fix it and conclude
again. A failed conclusion takes no folder.

On approval the folder is copied into a Hiveryn-managed package on the
execution machine (`HIVERYN_HOME/action-artifacts/packages/<id>` locally,
`~/.local/state/hiveryn-actions/artifacts/packages/<id>` remotely), enforcing
the same rules on every entry, and the execution becomes `completed` only
together with that ready package. The original folder is left as it is, and
later changes to it do not reach the package.

## Delivery to the caller

`getActionResult` and `waitForActionResult` make a completed package available
on the caller's own machine, determined from the calling session:

- on the execution machine, the package itself;
- elsewhere, a verified copy in the caller's managed `deliveries/<id>`
  directory, copied over SSH through the laptop (two remote machines never need
  access to each other).

`artifacts.state` reports `ready` (the result's `output_dir` is the path on the
caller's machine, with `artifacts.expires_at`), `copying` (read again; copies
continue across calls, bounded at 10 minutes), `failed` (with the cause; the
next read retries) or `expired`. A partial copy is never at the destination
path. Delivery never changes the execution's status, and a ready copy is reused.

The Actions window shows each execution's machine, package and delivered copies
with their state and expiry; Reveal in Finder is offered only for ready
directories on this Mac.

## Limits and retention

```yaml
# ~/.hiveryn/config.yaml (read at startup)
action_packages:
  max_bytes: 104857600     # file content per package (default 100 MiB)
  max_files: 10000         # files per package; directories are bounded alike
  retention_days: 30       # packages and copies, from completion
```

Packages and copies expire `retention_days` after the execution completed;
reading them does not extend that. Expired artifacts are reported `expired`
while the execution, its summary and its records remain.

Removal happens once per daemon start, in the background: interrupted copies
and expired package/copy directories are removed with short bounded attempts,
machines in parallel; an unreachable machine fails fast and blocks nothing
else. A failed removal is logged and kept on its record (shown in the Actions
window) and retried at the next start. Only recorded Hiveryn-managed directories
are ever removed — never an agent's folder, and never a package while a copy of
it is in progress. Executions completed before frozen packages existed keep
their original output folder as a package that never expires. There is no
total-storage quota.
