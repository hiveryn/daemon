# Remote ticket workers

Outcome: run ticket workers and repository terminals on configured SSH machines, retaining workers in owned tmux sessions across transport loss; keep local orchestration, scoped authenticated MCP, provider hooks, and remote Git review coherent.

Constraints: no remote Hiveryn binary/service, credentials copied from the laptop, remote architects/Actions, cross-machine writable tickets, or live-session test fixtures. Existing local behavior and unrelated work must remain intact. Remote paths are target-machine paths. Approval and mutation outcomes remain authoritative locally.

Approach:
1. Trace existing configuration, scope validation, provider preparation, session persistence, terminal lifecycle, MCP and Git boundaries.
2. Add machine/repository location contracts and enforce one-machine scope at creation, edits and launch; persist execution location for restore.
3. Give provider preparation a target-filesystem boundary, owned by agentruntime, without moving product lifecycle there.
4. Implement daemon-owned SSH/tmux lifecycle, reconnectable reverse forwarding and authenticated worker-only MCP/hook access. Keep transport state separate from run state and require confirmed remote cleanup for explicit termination.
5. Route repository terminals and Git operations by location; expose location/connection state in desktop. Document setup and disconnection behavior.
6. Verify meaningful unit/integration fixtures across changed repositories, review the complete change, commit/push coherent main commits and inspect exact-commit CI.

Verification: configuration/scope rejection, provider artifacts and remote paths, worker tool authorization, SSH disconnect survival and duplicate-free reattachment, termination failures, remote terminal/Git routing, and local regressions. Seek an isolated SSH fixture; real-provider and interactive acceptance must be reported separately, never inferred from fixtures.

Progress: ticket and project documents read; all four repositories initially clean. Existing daemon resumes an exited local PTY, so remote transport loss must bypass that path. Provider setup and temporary launch files currently use local filesystem APIs; a plain SSH command wrapper is insufficient.

Verification environment: isolated Docker SSH/tmux host with disposable keys, configuration and fixture CLIs; no live provider credentials used.

Implementation progress:
- Configuration accepts SSH machines and local/remote repo forms, enforces same-machine ticket scope, and skips local stat/home expansion for remote paths. Session snapshots retain machine/alias plus private tunnel credentials; remote resource ownership survives daemon restart.
- Provider setup and temporary artifacts use an optional target filesystem. Local adapter tests pass. Review found OpenCode's pre-existing HTTP bearer placeholder used Claude syntax; corrected it to documented `{env:NAME}` and disabled OAuth fallback for explicit bearer credentials (https://opencode.ai/v2/docs/mcp-servers).
- Dedicated loopback worker gateways expose only the fixed worker MCP registry and scoped native hooks. SSH forwards bind remote loopback. Workers launch once behind a tmux launch gate, then attachment/restart paths only reattach.
- Real SSH/tmux Docker fixture passed all three adapter paths for tool discovery/readTicket, remote Git diff, disconnect reattachment, daemon restart, one launch, and discard. These are fixture CLIs, not real model-provider acceptance.
- Final review corrected discard/stop ordering, auxiliary resource recovery and private runtime cleanup. Reconnect clears stale activity and authenticates native hook ownership. An active forwarded MCP request could keep SSH alive after tmux detachment; an attachment-end marker now closes only the local transport and retries attachment. Missing/dead managed workers are reported without restarting a provider.
- Expanded race-enabled fixture passed Claude, Codex and OpenCode launch paths: authenticated reads and mutations, native hooks/IDs, remote working-tree and commit diffs, terminal input/resize/recovery, unreachable-host termination refusal, reconnect during periodic MCP calls, daemon restart with exactly one provider launch, missing-worker reporting, discard and worker conclusion, owned-directory cleanup and credential redaction.
- Local verification passed: daemon vet/tests/build/lint plus focused race tests; agentruntime tidy/vet/tests/build/lint; shared Go tests/vet; desktop typecheck/lint, 106 tests and production build. Dependency delivery pins the published agentruntime commit. Exact-commit GitHub CI results are recorded in the session conclusion; shared and desktop have no configured workflow.

Final assessment: the change meets the remote execution boundary without introducing a second daemon or copying laptop credentials. Provider preparation remains in agentruntime, transport/lifecycle/authorization remain in daemon, and shared/desktop changes expose location and connection state. No unrelated refactoring is needed. Real authenticated provider CLI runs and interactive Electron acceptance were not performed; the fixture evidence does not imply them. Recovered auxiliary terminals preserve shell processes but not split placement. No further implementation questions remain.
