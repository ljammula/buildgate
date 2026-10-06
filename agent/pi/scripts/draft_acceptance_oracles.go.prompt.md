
For each qualifying criterion, write a Go test file (by default ONE file
per criterion), directly inside the directory {draft_dir} (relative to
your current working directory; create it if it does not already exist) --
NOT in a subdirectory of it. ONLY Go oracles are accepted: the sandbox
that runs them has no other language's test runner, so if this repository
is not Go, mark every criterion null.
Use a plain file name with no path separators of the shape
oracle_NNN_test.go (any other file in the directory -- a helper, fixture,
testdata file -- makes the whole draft unusable), in the SAME package
clause as the code it tests (put it in that package's directory via
target_path); every test function must be named TestOracle<Something>
(the runner selects tests with -run TestOracle, so any other name
silently runs nothing).
Group several criteria into one file ONLY when they will CERTAINLY be
implemented by the same change (a later step splits the work into
tickets, and a file is run in full against whichever ticket implements the
last criterion it covers, so a file mixing criteria from different tickets
fails deterministically) AND they live in the same Go package (one
package and one target_path per file: never group across packages). When
unsure, keep them separate. Write at most {max_files} oracle files in
total for the request; a later step enforces at most {max_files} files and
64 KiB per ticket, so do not merge criteria just to stay small. Inside one shared file, one Go test
function per criterion, each named TestOracle<Something>, is fine; give
every criterion it covers its own manifest entry naming that same
oracle_file (and the same target_path).
The test must be fully self-contained (no helper or fixture files: inline
any data) and runnable on its own against this repository's code -- it
will later be mounted read-only next to the workspace, never edited again
after you write it.

Three defect classes were seen in earlier drafts; a human reviewer will read
your file against them and the host type-checks it before review:
- The file must COMPILE. Every call must match the callee's real signature
  (arity and result count: a function with no result cannot be used as a
  value or argument), every imported package must be used, and every helper
  and test name must be unique across all the oracle files you write for one
  package. Symbols that the spec says the change will add may be undefined
  today; that is expected, but nothing else may be wrong.
- Never contradict the spec's own examples. When a criterion quotes a
  literal input (for example `ALL` or `done `) as accepted, valid, or
  producing some result, assert exactly that outcome for it, including any
  normalisation the criterion states (case-insensitive, whitespace-trimmed).
  Do not put an input the criterion lists as accepted into an "invalid",
  "rejected" or "error" case, and do not invent extra invalid cases at the
  boundary of a stated normalisation rule (a differently-cased or padded
  spelling of an accepted input): the spec is the only authority.
- Test the function, method or type the acceptance criteria themselves
  name (a criterion stating a signature, or calling it by name), directly.
  Never call a helper the criteria do not name -- do not guess one from the
  behaviour described -- and never reach it through another layer (an HTTP
  handler, a store, a CLI) unless the criterion is itself about that
  layer. If no criterion names what to call, mark the criterion null
  rather than guessing (found live 2026-09-24: 4 of 30 drafted tests called
  an invented helper, an unrelated existing function, or the HTTP handler,
  each with otherwise correct expected values).
- When asserting on an error value, use errors.Is/errors.As against an
  exported sentinel or type the package already declares, or compare the
  error's message text -- never errors.Is(err, someConstructor(x)) against
  a second, freshly-constructed error value. Two separately-constructed
  wrapped errors (e.g. two calls to the same fmt.Errorf-based helper) are
  not == to each other and errors.Is does not recursively compare their
  wrapped values unless the error type defines its own Is(error) bool
  method -- most hand-written error types do not. A test built this way
  only passes if the implementation is changed to add that method, which
  can force an otherwise-unrelated ticket outside its own declared file
  scope just to satisfy your test (found live 2026-09-22: exactly this
  pattern quarantined a correct, in-scope ticket).

Write {manifest_name} FIRST, before any oracle file, to that same
directory, then write the oracle files it names one at a time. A run can be
cut off by a time limit, and only files already on disk beside a manifest
are kept, so an early manifest is what saves finished work; if you change
your plan, update the manifest to match. The manifest is a JSON array with EXACTLY ONE ENTRY PER CRITERION UNDER
"## Acceptance criteria" ABOVE, IN THE SAME ORDER, each of the form:

{{"criterion": "<criterion text, verbatim, including its leading number>",
  "oracle_file": "<filename you wrote, relative to {draft_dir}>" or null,
  "target_path": "<repo-relative FILE path (directory and file name) where this test file belongs in the repository, following its package/test layout and the inventory above> or null",
  "rationale": "<one sentence: which of the three sources above, or why it doesn't qualify>"}}

Use null for oracle_file on every criterion that doesn't qualify --
never invent a test for one just to fill the field. target_path must be
the FULL repo-relative path of the FILE, including its file name (for
example backend/internal/categorize/oracle_001_test.go -- never just the
directory), a clean relative path (no leading "/", no ".." components) never under
.git, .buildgate or .oracle; use null when unsure. Each oracle file must
stay under 16 KiB. Write only
{manifest_name} and the oracle files it references to {draft_dir}. Do
not print anything as your response text; write every file using your
own file-editing tool.
