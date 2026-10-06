Decompose the approved spec above into one or more TICKETS, each a
separate Markdown file, written to the directory {draft_dir} (relative to
your current working directory; create it if it does not already exist).
Name the files 001.spec.md, 002.spec.md, ... in DEPENDENCY ORDER -- a
ticket must never depend on a later-numbered ticket's own changes.

Decomposition rule: write exactly one ticket when the change fits one
package and a reviewer could read the resulting PR in under ~400 changed
lines. Otherwise split by service or package boundary -- never split a
single service/package's own change across multiple tickets by layer
(e.g. never "one ticket for the backend half, one for the frontend half"
of what is really one change to one package). In particular, never split
wiring -- route registration, a router/mux entry, a dispatch-table line --
into its own ticket separate from the code and tests that exercise it:
a wiring-only ticket has no test of its own to add and can never pass the
tests rule below no matter what a later, differently-scoped ticket does.

Tests rule: every ticket's own Allowed-Files and Required-Changed-Files
must include at least one test file it will change. A ticket whose
Allowed-Files names only non-test files can never pass the factory's
tests_added gate, and the factory will reject the whole plan and ask you
to redraft it before a human ever sees it. The one exception is a ticket
that genuinely cannot be tested (e.g. a docs-only change): declare
"Tests-Required: no -- <reason>" as an extra header line instead of
adding a test file -- see the ticket format below.

Multi-file criteria rule: when one criterion names files owned by
several different tickets (e.g. "tests cover A, B and C" where A, B and C
belong to three different tickets' own Allowed-Files), list that
criterion under EVERY ticket that owns one of those files, not just one
of them -- and each such ticket's own part of the criterion (the file(s)
it owns) must actually be satisfied by that ticket's own plan. Found
live (example-app habit-insights request, 2026-09-28): a criterion named a handler
file, a service file, and an MCP-tool file across three tickets, but was
only listed as covered by the MCP-tool ticket -- that ticket could never
satisfy the part of the criterion that named the other two tickets' own
files, and the factory now rejects a plan shaped like this before a
human ever sees it.

Write EVERY spec acceptance criterion into at least one ticket's own
"### Acceptance criteria covered" list (see the exact shape below) --
every criterion number from the spec's own numbered "## Acceptance
criteria" list must appear in at least one ticket, and no criterion may
be claimed by zero tickets.

Each ticket file must contain, in this exact order:

Verify-Command: {verify_command}
Allowed-Files: <comma-separated list of every file this ticket touches>
Required-Changed-Files: <comma-separated list of files this ticket must actually change>
Tests-Required: no -- <reason> (omit this line entirely unless this ticket adds no test file; see the tests rule above)

## Goal

(One paragraph: what this ticket accomplishes.)

## Plan

### Files to touch

(A list of the files this ticket creates or modifies.)

### Steps

(An ordered list of implementation steps.)

### Tests to add

(A list of the tests this ticket adds.)

### Acceptance criteria covered

(A list of the spec's own acceptance-criteria numbers this ticket
satisfies, e.g. "- 1" / "- 3", one number per list item. Do not restate
the criteria's text here -- numbers only.)

## Out of scope

(What this ticket deliberately does not do.)

Write the finished ticket files, and nothing else, to {draft_dir}. Do not
print any ticket as your response text; write each one to its own file
using your own file-editing tool.
