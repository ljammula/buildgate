
For each qualifying criterion, write a Python test file (by default ONE file
per criterion), directly inside the directory {draft_dir} (relative to
your current working directory; create it if it does not already exist) --
NOT in a subdirectory of it. ONLY Python oracles are accepted in this
invocation: this repository was classified as Python, so mark every
criterion null if you cannot actually see Python source to test against.
Use a plain file name with no path separators of the shape
test_oracle_NNN.py (any other file in the directory -- a helper, fixture,
testdata file -- makes the whole draft unusable). PLAIN, TOP-LEVEL
functions only: NO pytest, NO unittest, NO test classes, NO decorators, NO
fixtures. The sandbox that runs the oracle has no pytest or other test
framework installed -- only python3 itself -- and it collects the oracle by
importing your file and calling every top-level name that starts with
"test" and is callable, so a test framework's own collection rules (which
skip a bare function, or require a unittest.TestCase subclass) would make
your test silently not run. Name every test function test_<something>, and
signal failure the same way this exact style always has: a bare `assert`
statement (never a framework's own assertion helper). Only stdlib imports
and the target repository's own modules may be imported (see the
"Repository file inventory" above for what exists) -- never subprocess, os,
sys, socket, urllib, http, shutil, or a third-party package (requests,
pytest, ...); never call eval, exec, compile, __import__, open, or input.
A drafted oracle only needs to assert a value already knowable from the
spec, so none of those are ever legitimately needed, and the host runs a
lint that catches a file importing or calling one of them and rejects the
draft (the file is never executed on the host, only parsed and inspected) --
this lint only catches obvious mistakes, not every way to reach the network
or filesystem, so the real containment is that your file only ever runs
inside the Docker sandbox the oracle gate runs in, never on the host.
Group several criteria into one file ONLY when they will CERTAINLY be
implemented by the same change (a later step splits the work into
tickets, and a file is run in full against whichever ticket implements the
last criterion it covers, so a file mixing criteria from different tickets
fails deterministically). When unsure, keep them separate. Write at most
{max_files} oracle files in total for the request; a later step enforces at
most {max_files} files and 64 KiB per ticket, so do not merge criteria just
to stay small. Inside one shared file, one test function per criterion is
fine; give every criterion it covers its own manifest entry naming that
same oracle_file (and the same target_path).
The test must be fully self-contained (no helper or fixture files: inline
any data) and runnable on its own against this repository's code -- it
will later be mounted read-only next to the workspace, never edited again
after you write it.

Two defect classes were seen in earlier drafts; a human reviewer will read
your file against them and the host statically checks it before review:
- The file must actually import and call the real function, method, or
  class the acceptance criteria themselves name (a criterion stating a
  name, or calling it by name), directly -- e.g. `from add import
  multiply_numbers`, matching how this repository's own modules are laid
  out (see the file inventory above). Never call a helper the criteria do
  not name -- do not guess one from the behaviour described. If no
  criterion names what to call, mark the criterion null rather than
  guessing.
- Never contradict the spec's own examples. When a criterion quotes a
  literal input (for example `ALL` or `done `) as accepted, valid, or
  producing some result, assert exactly that outcome for it, including any
  normalisation the criterion states (case-insensitive, whitespace-trimmed).
  Do not put an input the criterion lists as accepted into an "invalid",
  "rejected" or "error" case, and do not invent extra invalid cases at the
  boundary of a stated normalisation rule (a differently-cased or padded
  spelling of an accepted input): the spec is the only authority.

Write {manifest_name} FIRST, before any oracle file, to that same
directory, then write the oracle files it names one at a time. A run can be
cut off by a time limit, and only files already on disk beside a manifest
are kept, so an early manifest is what saves finished work; if you change
your plan, update the manifest to match. The manifest is a JSON array with EXACTLY ONE ENTRY PER CRITERION UNDER
"## Acceptance criteria" ABOVE, IN THE SAME ORDER, each of the form:

{{"criterion": "<criterion text, verbatim, including its leading number>",
  "oracle_file": "<filename you wrote, relative to {draft_dir}>" or null,
  "target_path": "<repo-relative FILE path (directory and file name) where this test file belongs in the repository, following the inventory above> or null",
  "rationale": "<one sentence: which of the three sources above, or why it doesn't qualify>"}}

Use null for oracle_file on every criterion that doesn't qualify --
never invent a test for one just to fill the field. target_path must be
the FULL repo-relative path of the FILE, including its file name (for
example tests/test_oracle_001.py -- never just the directory), a clean
relative path (no leading "/", no ".." components) never under .git,
.buildgate or .oracle; use null when unsure. Each oracle file must
stay under 16 KiB. Write only
{manifest_name} and the oracle files it references to {draft_dir}. Do
not print anything as your response text; write every file using your
own file-editing tool.
