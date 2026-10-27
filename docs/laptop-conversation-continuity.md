# Native laptop conversation continuity: bounded investigation

27 October 2026, ticket `2026-10-27-0125-continue-laptop-agent-sessions-from-connect-only-chat-ui`.

**The requested three-provider laptop → browser → laptop workflow is not delivered.**
The browser is now connect-only, but existing terminal sessions still have no
conversation read/send API. No desktop, normal launch, provider ownership or live
session was changed. This is partial delivery with a concrete architectural blocker,
not a readiness declaration for a replacement demo.

## Scope decision

Desktop's launcher and TicketWorkflow currently create terminal sessions;
MainTerminalStack renders structured chat sessions as a placeholder. Making the
existing single-owner structured runtime the normal laptop experience would require
replacing the native TUI with a desktop conversation pane.

The user declined that boundary: “We just need to make changes to API and
chat-ui-poc, dont change Hiveryn”. The subsequent architect answer retained all
three native TUIs as the target, permitted bounded investigation and partial
connect-only delivery with explicit blockers, and did not approve a Codex-only
reduction. No conversion, second owner, separate phone chat, desktop replacement,
network exposure or persistent review demo is authorized.

## Mechanisms assessed

The [previous native investigation](conversation-runtime-investigation.md) remains
the evidence for Claude notification provenance and OpenCode busy-turn displacement.
Those probes were not repeated.

| Provider | Existing-owner path | Result under the retained contract |
| --- | --- | --- |
| Claude | asyncRewake notification | A reply is possible, but input is `task_notification`, not genuine user input. Cannot relabel this as human conversation continuation. |
| Claude | Remote Control | Officially supports terminal/browser continuity, but through Anthropic's service and Claude clients. No supported local HTTP attachment contract for this custom client was identified. Enabling it would change the agreed local-only integration boundary. |
| Claude | MCP Channels | Requires session opt-in and delivers channel events; it is not a transparent user-input attachment to existing sessions. Prior configured-login evidence reports organization policy blocking channels. No policy or launch configuration was changed. |
| Codex | Native durable thread queue | New bounded native probe passed TUI → queue → TUI context continuity under one conversation owner. A feasible provider-specific candidate, not an implemented Hiveryn terminal conversation API. |
| OpenCode | Existing TUI server message/prompt_async | Genuine native messages, but the earlier busy probe displaced the first requested final reply. A daemon mutex cannot serialize laptop input that bypasses it; an idle check has a race. |
| OpenCode | TUI append/submit controls | These act on the current editable prompt. The documented surface offers no atomic submit-with-request-ID-and-draft-preservation operation. Combining calls would race laptop editing and does not establish durable admission/receipt semantics. |

Primary references checked on 27 October:
[Claude Remote Control](https://code.claude.com/docs/en/remote-control),
[Claude Channels](https://code.claude.com/docs/en/channels),
[OpenCode server](https://opencode.ai/docs/server/).
Remote Control registers with Anthropic and routes traffic through its API;
Channels require an explicit per-session flag and represent messages as channel
events. OpenCode documents separate append-prompt and submit-prompt operations.
The integration judgments above are inferences from those contracts and retained
native evidence, not claims that no future provider mechanism can work.

Current installed CLI versions: Claude 2.1.311, Codex 0.164.0, OpenCode 1.17.18.
No new Claude or OpenCode model call, external relay setup or policy change was
needed to assess those alternatives.

## New Codex evidence

A disposable native TUI with read-only sandbox, approval policy `never`, model
`gpt-5.6-sol`, isolated CODEX_HOME and a read-only authentication copy received
three synthetic inputs. The external helper called only initialize and native
thread-queue methods; it never started/resumed a second conversation owner.

1. TUI input supplied `MAPLE-742`; native completion: `LAPTOP-READY`.
2. External queued user input asked for the prior token and added `cobalt`;
   native completion: `PHONE-MAPLE-742`.
3. TUI input asked for both; native completion:
   `LAPTOP-RETURN MAPLE-742 cobalt`.

All used native session `01a17afe-d2b6-7720-acfa-5541e79cdb39`; explicit native
turn triggers were `user`, `queue`, `user`. The sanitized
[evidence excerpt](evidence/laptop-continuity-2026-10-27.json) retains native turn
IDs, completion text and source-record hashes. The initial extraction assertion
expected older `agent_message` events; it was corrected to validate the recorded
`task_complete.last_agent_message` fields without repeating model calls.

This did **not** exercise desktop launch, the Hiveryn conversation API, browser
submission, roles, questions, approval, worker conclusion or restart. The prior
Codex busy-queue evidence remains relevant; its Esc stall and native
crash-between-dispatch-and-delete duplicate window remain unresolved. Native
queue admission alone is not completion. A product implementation would need
terminal-history normalization (excluding instructions/reasoning), durable
request correlation/reconciliation and honest blocked/uncertain states before
advertising send capability. No such capability is advertised by this delivery.

Scratch diagnostics are under ignored `daemon/runs/laptop-continuity-probe/`.
The owned TUI and helper stopped, copied auth was removed, and no live session
was accessed. Raw diagnostics are not committed.

## Delivered browser restriction and verification

`chat-ui-poc` removes New chat and Launch controls and their client mutation
methods. The proxy refuses session creation and run/resume mutations while
retaining message, question and intent operations. Non-running sessions display
a read-only notice; terminal limitations and connectivity errors remain explicit.
The daemon's lifecycle APIs are unchanged.

Typecheck, lint, all 15 Vitest tests and production build passed. An isolated
Chromium 390×844 check selected existing fixtures, verified the terminal
unsupported notice, disabled non-running input and absence of launch/create
buttons, and sent actual proxy POST requests to sessions/runs/resume: all
returned 404. No fixture provider was launched. API-created fixtures were used
only for browser presentation checks, never counted as laptop acceptance.
Screenshots were inspected; evidence is retained locally in
`chat-ui-poc/runs/2026-10-27-0125-connect-only/`. Owned daemon/Next/browser processes
were stopped. The client repository has no remote or applicable GitHub CI.

## Remaining decision and handoff

All-provider acceptance remains blocked on an existing-owner mechanism that
preserves genuine user input, laptop drafts, turn completion and retry identity.
The user would need to authorize a changed boundary (such as a shared structured
laptop client or provider-specific relay/launch integration), approve reduced
provider support, or wait for suitable provider capabilities. Neither a notification
bridge nor hiding creation controls satisfies the original outcome.

Do not stand up the corrected demo as if continuity works. The client README
still provides isolated startup for reviewing the connect-only restriction;
starting that fixture daemon does not create usable conversations. Full isolated
Electron → browser → Electron acceptance remains unperformed and must accompany
any later implementation. Physical-phone networking remains separate: local
phone-sized browser verification does not make localhost phone-reachable.
