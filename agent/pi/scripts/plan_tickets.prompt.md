Decompose the approved spec above into one or more TICKETS, each a
separate Markdown file, written to the directory {draft_dir} (relative to
your current working directory; create it if it does not already exist).
Name the files 001.spec.md, 002.spec.md, ... in DEPENDENCY ORDER -- a
ticket must never depend on a later-numbered ticket's own changes.

Decomposition rule: {decomposition_rule}

In particular, never split
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

Never list `.factory.yml`, or anything under `.factory/` or `.buildgate/`, in a ticket's
Allowed-Files or Required-Changed-Files: they are the operator's factory
configuration, the factory never merges a build that changes them, and it
rejects a plan that names them. If the request needs a change there, say
so under the ticket's "Out of scope" for the operator to make.

Multi-file criteria rule: when one criterion names files owned by
several different tickets (e.g. "tests cover A, B and C" where A, B and C
belong to three different tickets' own Allowed-Files), list that
criterion under EVERY ticket that owns one of those files, not just one
of them -- and each such ticket's own part of the criterion (the file(s)
it owns) must actually be satisfied by that ticket's own plan. For
example, a criterion that names a handler file, a service file and an
MCP-tool file owned by three tickets is listed under all three; listed
under the MCP-tool ticket alone, no ticket can satisfy it, and the
factory rejects the plan.

Write EVERY criterion from the `## Acceptance criteria, as the factory
numbers them` list above into at least one ticket's own "### Acceptance
criteria covered" list (see the exact shape below) -- every number in that
factory-numbered list must appear in at least one ticket, and no criterion may
be claimed by zero tickets. Cover every criterion by the number the factory
gave it, even when the approved spec itself uses a different list marker or
does not have an "## Acceptance criteria" section.

Each ticket file must contain, in this exact order:

Verify-Command: {verify_command}
Allowed-Files: <comma-separated list of every file this ticket touches>
Required-Changed-Files: <comma-separated list of files this ticket must actually change>
Tests-Required: no -- <reason> (omit this line entirely unless this ticket adds no test file; see the tests rule above)
Estimated-Changed-Lines: <N>

The Estimated-Changed-Lines line is your estimate of this ticket's changed
lines, insertions plus deletions, tests included. Write digits only, e.g.
"Estimated-Changed-Lines: 240". The factory shows it at plan review; a value
that is not a whole number above zero refuses the plan.

## Goal

(One paragraph: what this ticket accomplishes.)

## Plan

### Files to touch

(A list of the files this ticket creates or modifies.)

### Steps

(An ordered list of implementation steps. When the spec's criteria carry
IDs ("N. AC-N: ..."), end each step that serves a criterion with
"(AC-N)", or "(AC-N, AC-M)" for several.)

### Tests to add

(A list of the tests this ticket adds. When the spec's criteria carry IDs,
end each test with the IDs of the criteria it checks, as "(AC-N)" or
"(AC-N, AC-M)": every criterion this ticket covers is cited by at least one
test unless the approved oracle already checks it.)

### Acceptance criteria covered

(A list of the spec's own acceptance-criteria numbers this ticket
satisfies, e.g. "- 1" / "- 3", one number per list item; when the spec's
criteria carry IDs, write the ID instead: "- AC-1" / "- AC-3". Do not
restate the criteria's text here -- the number or ID only.)

## Out of scope

(What this ticket deliberately does not do.)

Write the finished ticket files, and nothing else, to {draft_dir}. Do not
print any ticket as your response text; write each one to its own file
using your own file-editing tool.
