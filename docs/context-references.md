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
as a warning for done tickets, which are runtime-owned and never edited directly.
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

## Migration

All registered architects were migrated once to this grammar on 27 October
2026 (ticket and document references); the one-off migration tool was then
removed. A fresh workspace needs nothing: it has no legacy references.
