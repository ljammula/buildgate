Write ONLY a spec for the request above, as a single Markdown document,
using exactly this skeleton (every heading present, in this order, each
with real content underneath -- do not omit or rename any heading, and do
not add others):

# Spec

## Problem

## Scope

(If the request itself names a specific file, module, class, or function --
e.g. "`sub.py` providing `subtract_numbers(a, b)`" -- carry that name over
verbatim here rather than abstracting it into a description of what it does
(e.g. do not write "a subtraction operation" in place of "sub.py's
subtract_numbers"). The approved spec is the contract every later stage,
including oracle drafting, builds from; a later stage cannot recover a name
this document drops.)

## Non-goals

## Affected services and packages

## Acceptance criteria

(A numbered list. Each criterion must be concrete and independently
checkable by a reviewer reading the eventual diff -- not a restatement of
the request. Do NOT include a criterion about the commit message/subject
line itself -- the build system, not this ticket, owns commit authorship,
and a merge squashes/rewrites the subject anyway, so a criterion checking
it tests something that gets discarded before anyone reads it.
Likewise, when the deliverable is product behaviour, do NOT include a
criterion that is merely a proxy about the tests -- one ABOUT test files,
suites or coverage (e.g. "the tests pass", "a test exists for X", "the
unit test exercises Y"), or that a build, lint or formatter passes. Behavioural
criteria that name cases are fine and wanted (e.g. "X handles case Y by
returning Z"). The pipeline checks tests and the verify command as separate
gates; a criterion about them is judged by a reviewer reading the diff, who
then quarantines the whole run over an incidental style defect in a test
file. When the request itself asks for tests or tooling (e.g. "add
regression coverage for auth edge cases", "make the formatter config
enforce a rule"), the criteria must describe that observable outcome (e.g.
"running X reports Y for input Z"), never "a test exists". Every criterion states an
observable outcome of the deliverable: behaviour, output, or -- for a
documentation deliverable -- the content the document must contain (e.g.
"the README documents the recovery procedure"). As in Scope above, carry
over verbatim any file, module, or function/API name the request itself
states, rather than abstracting it away -- and put it IN each criterion
that exercises it, not only in Scope: when the request says where a
function lives, the criterion names that file, e.g. "`subtract_numbers(a,
b)` in `sub.py` returns `a - b` for numeric operands". Acceptance tests are
drafted from the criteria alone, one criterion at a time; a criterion that
omits the file leaves each test to guess its import, and guesses disagree.
Naming the file a function is exposed from is part of the contract, not an
implementation plan. Together the criteria must cover the primary success
flow and relevant negative, error, and boundary behaviour. Do not use vague
qualities such as "fast", "robust", or "user-friendly" unless the criterion
states what observable result makes that quality true.)

## Risks

(Record consequential assumptions and dependencies under Risks, including
the failure consequence when one proves false. Do not add an Assumptions
heading.)

## Open questions

Before writing, do a private ambiguity and quality pass. Consider only areas
relevant to this request: scope and actors; data identity and lifecycle;
success, error, empty, and edge behaviour; external dependencies and failure
modes; security and privacy; performance and reliability; terminology; and
whether every outcome is testable. Do not print this analysis.

Use a reasonable, repository-supported default for minor gaps. A detail that
later planning can decide is not an open question. For a high-impact choice
with multiple reasonable answers and no safe default -- one that could
materially change scope, behaviour, security, data, tests, or operations --
write a `[NEEDS DECISION]` item under Open questions. Include the recommended
option, two to five concrete alternatives, and one sentence explaining why
the decision matters. Write at most three such items, ordered by impact and
uncertainty. Never create a blocking decision for style or a minor detail. If
no material decision remains, write "None."

When operator feedback above answers a previous open question, integrate the
answer into every affected section. Do not emit a `[NEEDS DECISION]` marker
for that resolved issue or text that contradicts the answer; otherwise keep
the draft focused on the original request and feedback.

This is a SPEC, not a plan: do not list files to change, do not list
implementation steps, and do not propose an ordered sequence of work.
Decomposing the spec into tickets with a plan is a separate, later pass
-- this document only defines the problem, its boundaries, and how to
tell the eventual work is done.

Before saving, privately check the finished draft: Scope and Non-goals are
bounded; every acceptance criterion is unambiguous and independently
checkable; primary and relevant failure paths are covered; consequential
assumptions and dependencies are visible; terminology is consistent; and no
implementation sequence has leaked in. Revise the draft silently until it
passes. Do not write that review or a checklist into the spec.

Write the finished document, and nothing else, to the file at exactly
this path (relative to your current working directory): {draft_path}
Do not print the spec as your response text; write it to that file using
your own file-editing tool. Create its parent directory if it does not
already exist.
