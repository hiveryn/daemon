# Context references and worker context reads

A ticket's `references` list, and the `reference` argument of the worker
context tools, use one strict grammar (`domain.ParseReference` in shared):

| Form | Addresses | Lives on |
| --- | --- | --- |
| `<ticket-id>` | a ticket on the same board | — (read with `readTicket`) |
| `architect:<path>` | a file or directory in the owning architect workspace | the Hiveryn host (the laptop) |
| `<repoKey>:<path>` | a file or directory in that repo of the architect's `hiveryn.yaml` | the repo's configured machine |

`<path>` is relative to the root, `/`-separated and canonical: no leading `/`
or `~`, no `.`/`..`/empty segment, no trailing `/`, no backslash or control
character. `.` alone names the root (`daemon:.`). Directories are named like
files; the resolved `path_kind` says which it is.

Everything else is rejected with a diagnostic naming the fix: absolute and
home-relative paths, unqualified paths (`notes.md`), URLs, unknown prefixes or
repo keys (the message lists the configured keys), traversal and
non-canonical forms, duplicates, and a target that resolves outside its root
through a symlink. There is no legacy reader: an absolute path is never
interpreted.

`architect` is a reserved prefix. Configuration refuses a repo key spelled
`architect` (any case) and any key that is not `[A-Za-z0-9][A-Za-z0-9._-]*`,
so every repo key can prefix a reference.

## Resolution and diagnostics

Each reference resolves where its own target lives, independently of the
ticket's primary repo or the worker's machine: `architect:` targets are
checked on this machine, `<repoKey>:` targets on that repo's machine (one
batched SSH inspection per machine, the same cache, 2 s wait budget and
background refresh as before). `resolved_references` entries carry `kind`
(`ticket`/`architect`/`repo`), `status` (`present`, `missing`, `unverified`
with `error`, `invalid` with `error`), `owner`, `target`, `location`
(`local`, `remote` + `machine`, `unknown`), `path_kind` and `access` — the
worker tool that reads it (`readTicket`, `readContext`, `listContext`). An
unreachable machine is `unverified`, never `missing`, and is never answered
from another machine.

Ticket warnings: `INVALID_REFERENCE`, `BROKEN_REFERENCE` (unknown ticket id),
`MISSING_REFERENCE`, `UNVERIFIED_REFERENCE`; messages start with
`ticket.md line N:`. A malformed entry is reported on its own and never makes
the ticket `INVALID_TICKET` or hides its other references. The workspace check
reports `INVALID_REFERENCE` at its ticket.md line as an **error** for backlog
and progress tickets — so it reaches the architect through the existing
`workspace_validation` agent message after the two-second quiet period — and
as a warning for done tickets, which only the migration below may rewrite.
Ticket writes through the API (`POST`/`PATCH .../metadata`) reject invalid
references with every problem listed; existence is not required.

## Worker context tools

Ticket workers (local stdio MCP and the remote worker gateway alike) have
`readContext` and `listContext`. The daemon serves them at
`GET /api/sessions/{id}/context/file` and `/context/directory`; the scope is the
calling session's architect — its workspace and configured repos — taken from
the session record, never from the request. A remote worker's gateway has one
fixed session identity, so it can read exactly its own project.

- `readContext(reference, offset?, limit?, expected_sha256?)`: one page of a
  UTF-8 text file, 1-based lines (default 400, max 2000, ≤256 KiB per page;
  files ≤4 MiB). Every page carries `sha256`, `size` and `modified_at` of the
  whole file and `next_offset` while more lines follow.
- `listContext(reference, offset?, limit?, expected_sha256?)`: one page
  (default 200, max 1000) of a directory's immediate entries in name order,
  each with its own `reference`, `type` (`file`, `directory`, `symlink` — listed,
  not followed — `other`), `size` and `modified_at`. Never recursive.

Results are a closed status catalog, never an empty success: `ok`, `missing`,
`permission_denied`, `not_a_file`, `not_a_directory`, `binary`, `too_large`,
`changed` (the hash differs from `expected_sha256`; restart at the first
page), `unreachable` (the owning machine could not be reached; existence
unknown) and `outside_root`. Invalid references and unknown owners are tool
validation errors. Reads return current contents, not a launch snapshot.
Content is never logged; the request log records the endpoint path only, not the query (reference) or result.

Project documents (Overview, knowledge, investigations, specs, workflows)
point to context in the same form — `architect:<path>` or `<repoKey>:<path>` —
never with absolute paths or relative Markdown links, so a worker can pass any
pointer it finds straight to `readContext`. Reading context grants no write access; `repo` and
`additional_repos` remain the only writable scope. Bulk or binary
materialization and dependency crawling are not provided.

## Migration of existing workspaces

`hiverynd references-migrate` talks to the running daemon
(`HIVERYN_DAEMON_URL`, default `http://127.0.0.1:4201`):

```sh
hiverynd references-migrate plan > plan.json            # every registered architect; changes nothing
hiverynd references-migrate plan -architect radius > radius.json
hiverynd references-migrate apply plan.json > result.json
```

The plan lists, per architect, every ticket (backlog, progress, done) with a
reference that is not already valid, each reference with a disposition, and
every absolute path and relative Markdown link found in architect-owned
Markdown outside `tickets/` and `architect-sessions/` (Overview, knowledge,
investigations, specs, workflows). Ticket reference dispositions:

- `convert` — `to` holds the inferred reference: the deepest configured root
  (this architect's workspace or repos) containing the path. A repo is
  inferred only on the machine the removed interpreter used (the ticket's
  primary repo machine); a path in the architect workspace maps to
  `architect:` even when it was formerly looked up remotely (the defect being
  corrected), and the reason says so.
- `ambiguous` — several candidates (listed), or a repo on another machine, or
  an unconfigured primary repo; decide.
- `unmapped` — no configured root contains it; decide.
- `cross_architect` — another architect's workspace; references cannot address
  it, so copy what is needed or drop it. Nothing is granted across architects.
- `invalid` — not a path and not valid (non-canonical ids, unknown keys,
  duplicates); decide.

Review the file: set `to` to a valid reference or `drop: true`; undecided
entries stay as they are and keep their diagnostic. Apply rewrites only the
reference scalars of each ticket.md in place — other frontmatter (comments
included), the body, `updated_at` and every conclusion are untouched — and
re-parses the result to prove it. Each ticket is applied through the
daemon's workspace protection (`Protection.Mutate`), so progress/done records
stay accepted rather than becoming conflicts, and only when the file still has
the plan's `sha256`. Results per ticket: `applied`, `already_applied`
(idempotent rerun), `no_change`, `conflict` (changed or moved since the plan;
plan again), `blocked` (the workspace has an unresolved protected-record
conflict; nothing there is written), `rejected` (invalid decision or a layout
it will not rewrite — comments on items, multi-line forms) and `failed`.
Document findings are never changed by apply; each carries the suggested
reference (a relative link resolves against its file to `architect:<path>`;
one leaving the workspace is `unmapped`). Edit the author-owned Markdown to use
it.

`plan -rules rules.json` takes reviewed prefix rules — explicit mappings such
as a repo that moved, `[{"architect": "radius", "from_prefix":
"/Users/me/one/repos/radius-engine", "to_prefix": "radius-engine:"}]` — applied
before inference (longest prefix wins; a global rule only where its owner is
configured), recorded in the plan and validated against the architect's keys.

### Rollout

1. Deploy the daemon with this resolver (rebuild/restart). There is no
   compatibility reader: from then on every legacy absolute reference is
   `INVALID_REFERENCE` (an error notice for backlog/progress tickets, a warning
   for done ones) until migrated, and the old daemon is never asked to accept
   new forms.
2. Run `plan`, review and decide, run `apply`; repeat `plan` until only
   deliberate leftovers remain. Fix document findings by editing the
   author-owned Markdown.
3. Restarted architects and workers receive the new instructions; running
   sessions keep their prompts and tool lists until restarted.

A fresh workspace needs nothing: it has no legacy references.
