// Package policy provides pure, deterministic decisions over evidence
// collected by factoryd. Keeping these decisions separate from subprocess,
// git, and persistence I/O follows the plan's requirement that "every former
// approval" becomes a policy.Check with deterministic input and output.
package policy

import (
	"fmt"
	"path"
	"regexp"
	"strings"

	"buildgate/internal/run"
)

var (
	frozenStatusRE    = regexp.MustCompile(`^STATUS:\s*FROZEN\b`)
	draftStatusRE     = regexp.MustCompile(`^STATUS:\s*DRAFT\b`)
	markdownHeadingRE = regexp.MustCompile(`^#{2,6}\s+(.+?)\s*#*$`)
	numberedSectionRE = regexp.MustCompile(`^\d+(?:\.\d+)*[.)]?\s+`)
)

// AllGateChecks names every gate EvaluateRun can run, in the order it runs
// them: the four built-in checks below, followed by CommandGateIDs()'s own
// registry order (policy.CommandGates) -- lint, security_audit,
// unit_tests, integration_tests, reference_oracle today. A caller that
// wants to require "the strong gates actually ran" rather than merely "no
// recorded gate failed" (internal/release.MergePolicy.RequiredGates
// exists for exactly that -- see its doc comment) uses this as its
// default rather than hand-maintaining a second copy of these strings
// that could silently drift from EvaluateRun's own.
//
// "code_review" is deliberately NOT listed here: a run whose
// -code-review-policy is "off" never declares a code-review command. Listing it would make it
// internal/release.MergePolicy's default RequiredGates member, and every run
// with the review off would then deny release outright for a gate it was
// never meant to satisfy. Add it here only alongside a merge_policy.go
// exemption for "declared but not run", as full_suite_verify has.
var AllGateChecks = append([]string{
	"canonical_verify",
	"diff_scope",
	TestsRedOnBaseCheck,
	"full_suite_verify",
}, CommandGateIDs()...)

// CanonicalVerify evaluates the canonical-verification evidence.
func CanonicalVerify(cmd []string, buildExitCode, verifyExitCode int, durationMs int64, logSHA256 string) run.GateResult {
	return run.GateResult{
		Check:      "canonical_verify",
		Command:    cmd,
		Passed:     buildExitCode == 0 && verifyExitCode == 0,
		ExitCode:   verifyExitCode,
		DurationMs: durationMs,
		LogSHA256:  logSHA256,
	}
}

// DiffScope evaluates whether every changed file is in the allowed list and
// returns the out-of-scope files for reporting.
func DiffScope(changed, allowed []string) (run.GateResult, []string) {
	violations := diffScopeViolations(changed, allowed)
	return run.GateResult{
		Check:  "diff_scope",
		Passed: len(violations) == 0,
	}, violations
}

// Scope is a run's changed-file boundary, the one input of the diff_scope
// check. AllowedFiles is the ticket's Allowed-Files (nil: the ticket declared
// none). Unapproved says that list is the model's guess, not a person's: no
// person approved the plan it came from (request.ScopeApproved), so a changed
// file outside it is a note on the pull request and refuses nothing.
// AllowedPaths is the repository's .factory.yml allowed_paths, which a person
// wrote and which therefore always refuses: a changed file that matches none
// of its patterns (PathAllowed).
type Scope struct {
	AllowedFiles []string
	Unapproved   bool
	AllowedPaths []string
}

// ScopeNotePrefix begins the note of a changed file outside an unapproved
// Allowed-Files list; the file follows.
const ScopeNotePrefix = "changed outside the plan's Allowed-Files: "

// Declared reports whether s bounds the run at all: the diff_scope check runs
// only then.
func (s Scope) Declared() bool {
	return s.AllowedFiles != nil || len(s.AllowedPaths) > 0
}

// ScopeVerdict is Scope.Judge's answer: the files outside an approved
// Allowed-Files list, the files outside the repository's allowed_paths (each
// refuses), and the files outside an unapproved list (each a note).
type ScopeVerdict struct {
	OutsideTicket []string
	OutsideRepo   []string
	Noted         []string
}

// Refused is every file the verdict refuses the run for, without repeats.
func (v ScopeVerdict) Refused() []string {
	out := append([]string(nil), v.OutsideTicket...)
	for _, f := range v.OutsideRepo {
		if !containsPath(out, f) {
			out = append(out, f)
		}
	}
	return out
}

// Judge sorts changed (already stripped of harness byproducts and factory
// oracles by the caller) against s.
func (s Scope) Judge(changed []string) ScopeVerdict {
	var v ScopeVerdict
	if s.AllowedFiles != nil {
		outside := diffScopeViolations(changed, s.AllowedFiles)
		if s.Unapproved {
			v.Noted = outside
		} else {
			v.OutsideTicket = outside
		}
	}
	if len(s.AllowedPaths) > 0 {
		for _, f := range changed {
			if !PathAllowed(f, s.AllowedPaths) {
				v.OutsideRepo = append(v.OutsideRepo, f)
			}
		}
		// A file the repository refuses needs no note beside it.
		var noted []string
		for _, f := range v.Noted {
			if !containsPath(v.OutsideRepo, f) {
				noted = append(noted, f)
			}
		}
		v.Noted = noted
	}
	return v
}

// PathAllowed reports whether the repository-relative file matches one of
// patterns, anchored at the repository root: a pattern ending in "/" allows
// every path under that directory, and any other pattern is a path.Match glob
// against the whole path ("*.md" allows root-level markdown only, "docs/*.md"
// that directory's). "**" is not supported (projectconfig refuses it).
func PathAllowed(file string, patterns []string) bool {
	for _, p := range patterns {
		if strings.HasSuffix(p, "/") {
			if strings.HasPrefix(file, p) {
				return true
			}
			continue
		}
		if ok, err := path.Match(p, file); err == nil && ok {
			return true
		}
	}
	return false
}

func containsPath(list []string, f string) bool {
	for _, x := range list {
		if x == f {
			return true
		}
	}
	return false
}

// UnchangedRequiredFiles returns the files the ticket declared as required
// that do not appear in the changed-file inventory. A passing canonical
// verification only proves the ticket's Verify-Command exited 0, not that the
// required implementation change was made; the missing files are listed as a
// note on the pull request. They refuse nothing: a check that refuses must
// execute something, and this one reads only file lists.
func UnchangedRequiredFiles(changed, required []string) []string {
	changedSet := stringSet(changed)
	var missing []string
	for _, f := range required {
		if !changedSet[f] {
			missing = append(missing, f)
		}
	}
	return missing
}

// stringSet returns the members of list as a set.
func stringSet(list []string) map[string]bool {
	set := make(map[string]bool, len(list))
	for _, s := range list {
		set[s] = true
	}
	return set
}

// AbsentRequiredContent returns the strings the ticket declared as required
// that are not newly present in at least one required file's final content:
// present now, and not already present in that same file at the run's base
// commit. A file touched only cosmetically still has a diff, so a concrete
// marker of the real change (a new widget's Key literal, a new test's function
// name) is what shows the change was made. baseContent and finalContent are
// keyed by the same required file paths. The result is a note on the pull
// request and refuses nothing.
//
// Checked per file, not by concatenating every file's content into one blob:
// concatenation discards the base/final pairing (a marker newly added to file
// B would be masked by the same marker already in file A at base) and can
// fabricate a match from fragments split across the seam of two files.
func AbsentRequiredContent(baseContent, finalContent map[string]string, required []string) []string {
	var missing []string
	for _, s := range required {
		found := false
		for path, final := range finalContent {
			if strings.Contains(final, s) && !strings.Contains(baseContent[path], s) {
				found = true
				break
			}
		}
		if !found {
			missing = append(missing, s)
		}
	}
	return missing
}

// DefaultTestPatterns is used when the project's .factory.yml doesn't
// declare test_patterns. Multi-language on purpose: buildgate
// itself only ever produces Go changes, but a ticket's own workspace
// (hermes-config, in particular) can be any language its own tooling
// targets -- a Python repo's tests/test_foo.py change failed this gate
// outright when the default covered only *_test.go (found live). Covers
// the common per-ecosystem test-file naming conventions: Go, Python
// (both prefix and suffix forms), JavaScript/TypeScript (plain and React
// .test./.spec. forms), Ruby, Java, C#, and Rust.
var DefaultTestPatterns = []string{
	"*_test.go",
	"test_*.py", "*_test.py",
	"*.test.js", "*.test.ts", "*.test.tsx",
	"*.spec.js", "*.spec.ts",
	"*_spec.rb", "*_test.rb",
	"*Test.java",
	"*Tests.cs",
	"*_test.rs",
	"__tests__/",
}

// IsTestFile reports whether the repo-relative file counts as a test file
// under patterns: the single matcher behind the tests_red_on_base gate and the
// plan-time feasibility check. A pattern ending in "/" names a test
// directory and matches any file with that directory as a path segment
// sequence anywhere in its path ("__tests__/" matches "__tests__/a.js" and
// "src/x/__tests__/a.js", not "my__tests__/a.js"). Any other pattern is a
// path.Match glob matched against the full path and the basename; a
// malformed pattern never matches.
func IsTestFile(file string, patterns []string) bool {
	for _, p := range patterns {
		if strings.HasSuffix(p, "/") {
			if strings.HasPrefix(p, "/") {
				continue
			}
			if strings.Contains("/"+file, "/"+p) {
				return true
			}
			continue
		}
		if matched, err := path.Match(p, file); err == nil && matched {
			return true
		}
		if matched, err := path.Match(p, path.Base(file)); err == nil && matched {
			return true
		}
	}
	return false
}

// TestFiles returns the files of changed that count as tests under patterns
// (DefaultTestPatterns when empty), after harness byproducts and the
// factory-committed oracles are set aside: an oracle's own test file must not
// satisfy the agent's obligation to add a test. A malformed pattern never
// matches (patterns come from a committed .factory.yml, so a typo shows up as
// "no test file changed", fixable by the repository owner, rather than
// halting an unrelated run). These are the files the tests_red_on_base proof
// runs.
func TestFiles(changed, patterns []string, oracles *run.OracleEvidence) []string {
	if len(patterns) == 0 {
		patterns = DefaultTestPatterns
	}
	var tests []string
	for _, f := range ExcludeFactoryOracles(ExcludeHarnessByproducts(changed), oracles) {
		if IsTestFile(f, patterns) {
			tests = append(tests, f)
		}
	}
	return tests
}

// TestsRedOnBaseCheck is the check through which a change must prove it
// changed behaviour: a test it adds or changes fails on the base commit and
// passes on the result commit, shown by running it.
const TestsRedOnBaseCheck = "tests_red_on_base"

// TestsRedOnBaseInput is the factory's runs of the changed test files (the
// proof): one of run.TestsRedProved, TestsRedNotExercised or
// TestsRedNoVerdict, and a one-line reason.
type TestsRedOnBaseInput struct {
	Verdict string
	Reason  string
}

// TestsRedOnBase evaluates the tests_red_on_base gate over tests (TestFiles),
// the ticket's opt-out reason (ticketspec.ParseTestsRequiredOptOut's return
// value; "" means none) and the proof, nil when the run took none. It
// refuses, in order:
//
//   - with no opt-out and no test file changed;
//   - a proof that every test that ran also passes on the base commit.
//
// An opt-out passes with no proof (the pull request names the reason). A
// proof with no verdict, or none at all, passes and returns a note: a check
// that executes something refuses only on what it saw run.
func TestsRedOnBase(tests []string, optOutReason string, proof *TestsRedOnBaseInput) (run.GateResult, string) {
	gate := run.GateResult{Check: TestsRedOnBaseCheck, Passed: true}
	switch {
	case optOutReason != "":
		return gate, ""
	case len(tests) == 0:
		gate.Passed = false
		return gate, ""
	case proof == nil:
		return gate, "tests_red_on_base: the changed tests were not run"
	}
	switch proof.Verdict {
	case run.TestsRedProved:
		return gate, ""
	case run.TestsRedNotExercised:
		gate.Passed = false
		return gate, ""
	}
	return gate, "tests_red_on_base: no verdict (" + proof.Reason + ")"
}

// TicketTestsFeasible reports whether a ticket declaring allowed as
// its Allowed-Files could ever pass the tests_red_on_base gate (it must be
// able to change a test file), given patterns (or DefaultTestPatterns when
// empty, as TestFiles does) and the ticket's own declared Tests-Required
// opt-out reason (ticketspec.ParseTestsRequiredOptOut's return value; ""
// means no opt-out).
//
// Found live (a Flutter + Go app repo run 3, 2026-09-28): a drafted ticket's
// Allowed-Files/Required-Changed-Files named only
// backend/internal/handler/dispatch_mux.go, it declared no Tests-Required
// opt-out, and its own "Tests to add" prose said the route would be
// exercised by a *different* ticket's test -- a ticket that, by its own
// declared scope, could never touch a test file. A
// human approved the plan anyway, the build ran (~$1+), every other gate passed
// but the plan's own Allowed-Files could never include a test file, and the request quarantined for a defect planning
// itself could have caught for free. This function is that catch, run at
// plan-drafting time (internal/requestdriver/request_driver.go's
// writeAndValidateDraftedTickets) before the plan ever reaches
// plan_review.
//
// An empty allowed is treated as feasible (true): writeAndValidateDraftedTickets
// already rejects a ticket with no Allowed-Files at all as a separate,
// missing-Allowed-Files error, so an empty list reaching this function
// would be redundant to flag here too.
//
// Deliberately conservative in allowed's favor, per this bug's own fix
// plan: only the clear case -- every Allowed-Files entry a concrete,
// non-test file -- is flagged. A directory entry (trailing "/") or a
// glob (contains "*", "?", or "[") could still expand to include a test
// file the model hasn't named yet, so either makes a ticket feasible
// without further checking; see matchesAllowed's own doc comment for why
// those are the only two non-exact Allowed-Files forms.
func TicketTestsFeasible(allowed []string, patterns []string, optOutReason string) bool {
	if optOutReason != "" {
		return true
	}
	if len(allowed) == 0 {
		return true
	}
	if len(patterns) == 0 {
		patterns = DefaultTestPatterns
	}
	for _, entry := range allowed {
		if strings.HasSuffix(entry, "/") {
			return true
		}
		if strings.ContainsAny(entry, "*?[") {
			return true
		}
		if IsTestFile(entry, patterns) {
			return true
		}
	}
	return false
}

// FullSuiteVerify evaluates the project's complete test suite, run against
// the isolated post-build worktree in addition to the ticket's own
// (targeted) canonical_verify. canonical_verify only proves the ticket's
// own Verify-Command still passes; it says nothing about whether this
// slice broke a *different*, already-accepted slice's test — the
// regression-oracle gap the plan's 2026-08-28 readiness review named as
// gap 3. cmd is nil exactly when the project didn't declare a full-suite
// command, and EvaluateRun skips this gate entirely in that case, the
// same convention CanonicalVerify's sibling gates already follow.
func FullSuiteVerify(cmd []string, exitCode int, durationMs int64, logSHA256 string) run.GateResult {
	return run.GateResult{
		Check:      "full_suite_verify",
		Command:    cmd,
		Passed:     exitCode == 0,
		ExitCode:   exitCode,
		DurationMs: durationMs,
		LogSHA256:  logSHA256,
	}
}

// CodeReview evaluates the review step's own pass/fail
// (agent/pi/scripts/combined_review.py, internal/codereview). cmd is nil
// exactly when -code-review-policy was "off" or the run never reached this
// phase (an earlier gate already failed), and EvaluateRun skips this gate
// entirely in that case, the same convention FullSuiteVerify already
// follows. exitCode is the gate's exit code decoded from the launch
// (reviewstep.ReviewExitCode): 0 when the reviewer gave a verdict, or under
// "advisory" regardless; findings never fail it (SC-022: a finding refuses a
// run only through its reproduction).
func CodeReview(cmd []string, exitCode int, durationMs int64, logSHA256 string) run.GateResult {
	return run.GateResult{
		Check:      "code_review",
		Command:    cmd,
		Passed:     exitCode == 0,
		ExitCode:   exitCode,
		DurationMs: durationMs,
		LogSHA256:  logSHA256,
	}
}

// ReviewReproductionCheck is the check through which a reviewer's finding
// refuses a run (SC-022).
const ReviewReproductionCheck = "review_reproduction"

// ReviewReproductionInput is the factory's runs of the reproductions a
// review held (SC-022): how many it decided, and how many of those refuse
// the run (a finding whose reproduction blocks, or, after a corrective build,
// a held reproduction still failing on its result).
type ReviewReproductionInput struct {
	Decided  int
	Refusing int
}

// ReviewReproduction evaluates the review_reproduction check: it fails iff
// any reproduction refuses the run. A finding that is advice never fails it.
func ReviewReproduction(in ReviewReproductionInput) run.GateResult {
	exitCode := 0
	if in.Refusing > 0 {
		exitCode = 1
	}
	return run.GateResult{
		Check:    "review_reproduction",
		Passed:   in.Refusing == 0,
		ExitCode: exitCode,
	}
}

// NamedGateInput is one operator-configured named gate beyond the
// built-in checks above -- lint/security_audit/unit_tests/
// integration_tests, declared via .factory.yml's lint_command/
// security_command/unit_test_command/integration_test_command. Command is
// nil exactly when the project didn't configure this particular gate;
// EvaluateRun skips it entirely in that case (never "passed"), the same
// convention FullSuiteCommand already uses for full_suite_verify.
type NamedGateInput struct {
	Check      string
	Command    []string
	ExitCode   int
	DurationMs int64
	LogSHA256  string
	// ReferenceOracleSHA256 is meaningful only for the "reference_oracle"
	// check -- see run.GateResult.ReferenceOracleSHA256's own doc
	// comment. Left empty for every other named gate, and copied through
	// verbatim (never computed here; policy stays pure/deterministic
	// over already-collected evidence, the same reason LogSHA256 is
	// passed in rather than hashed by this function).
	ReferenceOracleSHA256 string
	// BaseCheck is the gate's rerun on the base commit, copied through to
	// run.GateResult.BaseCheck. It never decides Passed.
	BaseCheck *run.GateBaseCheck
}

// NamedGate evaluates one NamedGateInput the same way FullSuiteVerify
// evaluates the full-suite gate: pass iff the command exited zero.
func NamedGate(check string, cmd []string, exitCode int, durationMs int64, logSHA256, referenceOracleSHA256 string) run.GateResult {
	return run.GateResult{
		Check:                 check,
		Command:               cmd,
		Passed:                exitCode == 0,
		ExitCode:              exitCode,
		DurationMs:            durationMs,
		LogSHA256:             logSHA256,
		ReferenceOracleSHA256: referenceOracleSHA256,
	}
}

// EvaluateRunInput bundles every already-collected piece of evidence
// EvaluateRun needs to decide a run's accept/quarantine outcome. It exists
// so that decision — which gates run, and how their results combine into
// one accept/quarantine verdict — is written exactly once and shared
// identically between cmd/factoryd's direct-supervisor path and a future
// Temporal Activity, instead of each independently reimplementing it and
// risking the two silently drifting apart (the same class of bug this
// plan's --review-base-sha incident and CLAIMS.md's false-accept gaps
// were both about: two things that were supposed to agree, didn't).
// AllowedFiles, RequiredChangedFiles, and RequiredContent are nil exactly
// when the ticket didn't declare the corresponding key — each check is
// skipped entirely in that case, not evaluated against an empty list.
type EvaluateRunInput struct {
	VerifyCommand    []string
	BuildExitCode    int
	VerifyExitCode   int
	VerifyDurationMs int64
	VerifyLogSHA256  string
	// VerifyRerun is canonical verification's rerun, copied through to its
	// GateResult.Rerun. The exit code and hash above are already the
	// rerun's when it ran; it never decides Passed itself.
	VerifyRerun *run.VerifyRerun

	ChangedFiles         []string
	AllowedFiles         []string
	RequiredChangedFiles []string

	// ScopeUnapproved and AllowedPaths complete AllowedFiles into the run's
	// Scope: the list is the model's guess (notes only), and the
	// repository's .factory.yml allowed_paths (always refuses).
	ScopeUnapproved bool
	AllowedPaths    []string

	RequiredContent           []string
	RequiredContentBaseFiles  map[string]string
	RequiredContentFinalFiles map[string]string

	// TestPatterns/TestsRequiredOptOut/TestsRedOnBase feed the
	// tests_red_on_base gate (TestsRedOnBase). Unlike AllowedFiles et al.,
	// this gate always runs (TestPatterns empty just means "use
	// DefaultTestPatterns"), so there is no nil-skips-the-gate case here;
	// TestsRedOnBase is nil when the run took no proof.
	TestPatterns        []string
	TestsRequiredOptOut string
	TestsRedOnBase      *TestsRedOnBaseInput

	FullSuiteCommand    []string
	FullSuiteExitCode   int
	FullSuiteDurationMs int64
	FullSuiteLogSHA256  string
	// FullSuiteRerun is the full suite's rerun, as VerifyRerun is
	// canonical verification's.
	FullSuiteRerun *run.VerifyRerun

	// NamedGates is the lint/security_audit/unit_tests/
	// integration_tests named gates, one entry per gate the project
	// actually configured (an unconfigured gate is simply absent from this slice,
	// not present with a nil Command) -- see NamedGateInput's own doc
	// comment.
	NamedGates []NamedGateInput

	// CodeReviewCommand/CodeReviewExitCode/... mirror
	// FullSuiteCommand's own nil-skips-the-gate convention -- nil
	// exactly when -code-review-policy was "off", or the run never
	// reached the code-review phase at all (an earlier gate already
	// failed). See policy.CodeReview's own doc comment.
	CodeReviewCommand    []string
	CodeReviewExitCode   int
	CodeReviewDurationMs int64
	CodeReviewLogSHA256  string

	// ReviewReproduction is nil exactly when the run took no reproduction
	// phase (a run without -review-reproduction, or one that never reached
	// the review), and EvaluateRun then skips the review_reproduction check.
	ReviewReproduction *ReviewReproductionInput

	// Oracles is the host-collected committed-oracle evidence (nil for
	// every run without one). It only ever REMOVES factory-authored,
	// hash-verified paths from the diff_scope and test-file inventories
	// (ExcludeFactoryOracles).
	Oracles *run.OracleEvidence `json:"oracles,omitempty"`
}

// EvaluateRunResult is EvaluateRun's outcome: every gate that actually ran
// (in evaluation order: canonical_verify, then diff_scope, tests_red_on_base,
// full_suite_verify, each only if its input was declared), whether the run is
// accepted overall, the detail each non-passing check reported, and the
// notes (one line each) the pull request lists: a required file or required
// content the ticket declared and the run did not produce, and a
// tests_red_on_base check that reached no verdict. Notes refuse nothing.
type EvaluateRunResult struct {
	GateResults         []run.GateResult
	Accepted            bool
	FailedChecks        []string
	DiffScopeViolations []string
	Notes               []string
}

// add records one gate's result, and its check as failed when it did not
// pass, and reports whether it passed.
func (r *EvaluateRunResult) add(gate run.GateResult) bool {
	r.GateResults = append(r.GateResults, gate)
	if !gate.Passed {
		r.FailedChecks = append(r.FailedChecks, gate.Check)
	}
	return gate.Passed
}

// EvaluateRun runs every declared gate against input and combines their
// results into one accept/quarantine verdict.
func EvaluateRun(input EvaluateRunInput) EvaluateRunResult {
	var result EvaluateRunResult

	gate := CanonicalVerify(input.VerifyCommand, input.BuildExitCode, input.VerifyExitCode, input.VerifyDurationMs, input.VerifyLogSHA256)
	gate.Rerun = input.VerifyRerun
	result.GateResults = append(result.GateResults, gate)
	accepted := gate.Passed
	if !gate.Passed {
		result.FailedChecks = append(result.FailedChecks, gate.Check)
	}

	scope := Scope{AllowedFiles: input.AllowedFiles, Unapproved: input.ScopeUnapproved, AllowedPaths: input.AllowedPaths}
	if scope.Declared() {
		// ExcludeHarnessByproducts is applied here, at the ticket-scope
		// call site, and deliberately not inside DiffScope itself — see
		// ExcludeHarnessByproducts' own doc comment for why DiffScope must
		// stay a neutral matcher for internal/release's differently-scoped
		// reuse of it.
		verdict := scope.Judge(ExcludeFactoryOracles(ExcludeHarnessByproducts(input.ChangedFiles), input.Oracles))
		refused := verdict.Refused()
		scopeGate := run.GateResult{Check: "diff_scope", Passed: len(refused) == 0}
		result.GateResults = append(result.GateResults, scopeGate)
		result.DiffScopeViolations = refused
		for _, f := range verdict.Noted {
			result.Notes = append(result.Notes, ScopeNotePrefix+f)
		}
		if !scopeGate.Passed {
			accepted = false
			result.FailedChecks = append(result.FailedChecks, "diff_scope")
		}
	}

	if input.RequiredChangedFiles != nil {
		for _, f := range UnchangedRequiredFiles(input.ChangedFiles, input.RequiredChangedFiles) {
			result.Notes = append(result.Notes, "required file not changed: "+f)
		}
	}
	if input.RequiredContent != nil {
		for _, c := range AbsentRequiredContent(input.RequiredContentBaseFiles, input.RequiredContentFinalFiles, input.RequiredContent) {
			result.Notes = append(result.Notes, fmt.Sprintf("required content not newly present: %q", c))
		}
	}

	testsGate, note := TestsRedOnBase(TestFiles(input.ChangedFiles, input.TestPatterns, input.Oracles), input.TestsRequiredOptOut, input.TestsRedOnBase)
	if note != "" {
		result.Notes = append(result.Notes, note)
	}
	accepted = result.add(testsGate) && accepted

	if input.FullSuiteCommand != nil {
		suiteGate := FullSuiteVerify(input.FullSuiteCommand, input.FullSuiteExitCode, input.FullSuiteDurationMs, input.FullSuiteLogSHA256)
		suiteGate.Rerun = input.FullSuiteRerun
		result.GateResults = append(result.GateResults, suiteGate)
		if !suiteGate.Passed {
			accepted = false
			result.FailedChecks = append(result.FailedChecks, suiteGate.Check)
		}
	}

	for _, g := range input.NamedGates {
		gate := NamedGate(g.Check, g.Command, g.ExitCode, g.DurationMs, g.LogSHA256, g.ReferenceOracleSHA256)
		gate.BaseCheck = g.BaseCheck
		result.GateResults = append(result.GateResults, gate)
		if !gate.Passed {
			accepted = false
			result.FailedChecks = append(result.FailedChecks, gate.Check)
		}
	}

	if input.CodeReviewCommand != nil {
		accepted = result.add(CodeReview(input.CodeReviewCommand, input.CodeReviewExitCode, input.CodeReviewDurationMs, input.CodeReviewLogSHA256)) && accepted
	}
	if input.ReviewReproduction != nil {
		accepted = result.add(ReviewReproduction(*input.ReviewReproduction)) && accepted
	}

	result.Accepted = accepted
	return result
}

// ProductSpecFrozen checks the first-line specification status. Invoked by
// `factoryd check-project -spec` (cmd/factoryd/main.go).
func ProductSpecFrozen(specContent string) (bool, string) {
	firstLine, _, _ := strings.Cut(specContent, "\n")
	firstLine = strings.TrimSuffix(firstLine, "\r")
	if frozenStatusRE.MatchString(firstLine) {
		return true, ""
	}
	if strings.HasPrefix(firstLine, "STATUS:") {
		if draftStatusRE.MatchString(firstLine) {
			return false, "spec is still DRAFT, not frozen"
		}
		return false, "spec STATUS line is not FROZEN"
	}
	return false, "spec has no STATUS line"
}

// TicketStructure checks the ticket headings and required text. Invoked by
// `factoryd check-project -ticket -ticket-number` (cmd/factoryd/main.go).
//
// Headings and required text are located in a fenced-code-stripped view of
// ticketContent (same length and offsets, fenced lines blanked out) so a
// well-formed example ticket embedded in a fenced block cannot itself
// satisfy the checks — mirroring how markdownSections already excludes
// fenced content for ArchitectureStructure/ProgramDesignStructure. Found
// via review: the prior version scanned ticketContent directly, so a ticket
// consisting only of a fenced template passed every check.
func TicketStructure(ticketContent string, ticketNumber int) (bool, []string) {
	scan := stripFencedMarkdown(ticketContent)
	goal := headingOffset(scan, "## Goal")
	required := headingOffset(scan, "## Required changes")
	verification := headingOffset(scan, "## Verification")
	commit := headingOffset(scan, "## Commit")

	var reasons []string
	for _, heading := range []struct {
		name   string
		offset int
	}{
		{"## Goal", goal},
		{"## Required changes", required},
		{"## Verification", verification},
		{"## Commit", commit},
	} {
		if heading.offset < 0 {
			reasons = append(reasons, "ticket is missing "+heading.name)
		}
	}
	if goal >= 0 && required >= 0 && verification >= 0 && commit >= 0 &&
		(goal >= required || required >= verification || verification >= commit) {
		reasons = append(reasons, "ticket headings are not in the required order")
	}

	// Body-text checks below scan ticketContent (the original, un-stripped
	// text), not scan — only heading *location* needs the fenced-blind
	// view above. Found via review: a well-formed ticket that fences its
	// actual verification command (a normal, common ticket-writing style —
	// e.g. "```sh\nmake verify\n```" under a real, non-fenced ## Verification
	// heading) was being rejected because the fix for the fenced-template
	// finding blanked that real fenced content along with the fake
	// templates it was meant to exclude. This stays safe against the
	// original finding because a fully-fenced fake ticket never gets a
	// valid required/verification offset in the first place: those offsets
	// only ever come from scan, so an entirely-fenced document still fails
	// on "missing" headings before either of these Contains checks matter.
	const stateFilesInstruction = "Update `ARCHITECTURE.md` and append a `PROGRESS.md` entry before finishing."
	requiredEnd := len(ticketContent)
	if verification > required {
		requiredEnd = verification
	}
	if required < 0 || !strings.Contains(ticketContent[required:requiredEnd], stateFilesInstruction) {
		reasons = append(reasons, "## Required changes is missing the ARCHITECTURE.md/PROGRESS.md update instruction")
	}

	if verification < 0 {
		reasons = append(reasons, "## Verification does not mention make verify")
	} else {
		end := len(ticketContent)
		if commit > verification {
			end = commit
		}
		if !mentionsMakeVerify(ticketContent[verification:end]) {
			reasons = append(reasons, "## Verification does not mention make verify")
		}
	}

	// The source template supplies a longer context paragraph, but this check is
	// intentionally limited to the sentence named by the policy contract.
	if ticketNumber != 1 {
		context := strings.Index(scan, "This is an existing repo.")
		if context < 0 || (goal >= 0 && context > goal) {
			reasons = append(reasons, "ticket is missing the pre-Goal context line: This is an existing repo.")
		}
	}

	return len(reasons) == 0, reasons
}

// requiredBrownfieldTicketSections is the section shape
// agent/pi/scripts/plan_tickets.py instructs the model to produce for
// each ticket (mirrored, name-for-name, by
// internal/request.ValidateTicketPlan's own requiredTicketHeadings) --
// sectionOffset matches by name regardless of heading level, so the ###
// subsections under "Plan" are named here exactly like the ## sections
// around them.
var requiredBrownfieldTicketSections = []string{
	"Goal",
	"Plan",
	"Files to touch",
	"Steps",
	"Tests to add",
	"Acceptance criteria covered",
	"Out of scope",
}

// requiredBrownfieldTicketHeaderKeys are the internal/ticketspec header
// lines every brownfield ticket must declare -- Required-Content: is
// deliberately excluded, since the plan-drafting pass marks it
// optional, matching ticketspec's own optional-key convention for it.
var requiredBrownfieldTicketHeaderKeys = []string{
	"Verify-Command:",
	"Allowed-Files:",
	"Required-Changed-Files:",
}

// TicketStructureBrownfield checks the ticket shape the plan-drafting
// pass produces: the header key lines internal/ticketspec parses, plus the
// Goal/Plan/Out-of-scope section shape internal/request.ValidateTicketPlan
// also checks (in more depth -- non-empty sections, parseable acceptance
// criteria). This is TicketStructure's brownfield sibling, not a
// modification of it: greenfield tickets (## Required changes/##
// Verification/## Commit, ticketNumber-based context-line rule) are
// unrelated to this shape and keep their own, unchanged check.
//
// Header keys are located in a fenced-code-stripped view of
// ticketContent, the same reasoning TicketStructure's own doc comment
// gives: a ticket that merely quotes or illustrates a key inside prose or
// a fenced example must not satisfy this check.
func TicketStructureBrownfield(ticketContent string) (bool, []string) {
	scan := stripFencedMarkdown(ticketContent)
	var reasons []string
	for _, key := range requiredBrownfieldTicketHeaderKeys {
		if !hasTopLevelLineWithPrefix(scan, key) {
			reasons = append(reasons, "ticket is missing required header line "+key)
		}
	}

	_, sectionReasons := requiredMarkdownSections(ticketContent, "ticket", requiredBrownfieldTicketSections)
	reasons = append(reasons, sectionReasons...)
	return len(reasons) == 0, reasons
}

// hasTopLevelLineWithPrefix reports whether scan (already
// fenced-code-stripped) contains a line, with no leading whitespace,
// starting with prefix -- the same "top-level, not an indented example"
// convention internal/ticketspec's own forEachTopLevelLine uses for the
// identical class of header key line.
func hasTopLevelLineWithPrefix(scan, prefix string) bool {
	for _, line := range strings.Split(scan, "\n") {
		if line != strings.TrimLeft(line, " \t") {
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(line), prefix) {
			return true
		}
	}
	return false
}

// DefaultArchitectureRequiredSections returns a fresh copy of the
// section-heading convention goal_pilot.py's own drafted ARCHITECTURE.md
// uses for a factory-scaffolded project. An onboarded EXISTING repo's own
// architecture doc, written organically over real development, essentially
// never already uses these exact three headings in this exact order --
// found via a real brownfield validation attempt against a real existing
// Go application (2026-09-08): forcing every onboarded team to rename
// real, substantive sections of their own doc just to satisfy this check
// does not scale once multiple teams' repos, each with their own
// established documentation convention, are in scope.
// ArchitectureStructure's own requiredSections parameter (see its doc
// comment) exists to let an operator declare their repo's actual heading
// names instead of demanding the repo conform to the factory's.
//
// A function, not an exported []string var: a caller building a custom
// list by copying this one (e.g. `s := DefaultArchitectureRequiredSections;
// s[1] = "My verification"`) would otherwise mutate the same backing array
// every other caller in the process reads as "the default" -- including a
// concurrent preflight check on a different run -- silently corrupting it
// for everyone, found via an adversarial pass on this exact function
// (2026-09-08) rather than by a failing test. Returning a fresh slice each
// call makes that class of bug impossible regardless of what a caller does
// with the result.
func DefaultArchitectureRequiredSections() []string {
	return []string{"Repo layout", "Verification", "Known deviations"}
}

// ArchitectureStructure checks the stable shape of the architecture
// artifact against requiredSections (nil or empty uses
// DefaultArchitectureRequiredSections(), the convention goal_pilot.py's own
// drafted ARCHITECTURE.md follows). The artifact convention is
// deliberately small: it must describe the repository layout, how the
// repository is verified, and any known deviations, each under its own
// heading, in that order -- but an onboarded existing repo may already
// document exactly that under headings of its own choosing (e.g. "System
// overview" instead of "Repo layout"), and requiredSections lets an
// operator name those instead of requiring the repo's own doc to change.
// These are structural checks only; neither prose nor a reviewer-agent
// verdict is used as evidence of architectural quality.
func ArchitectureStructure(architectureContent string, requiredSections []string) (bool, []string) {
	if len(requiredSections) == 0 {
		requiredSections = DefaultArchitectureRequiredSections()
	}
	return requiredMarkdownSections(architectureContent, "architecture", requiredSections)
}

// AcceptanceSuiteWired checks that every drafted acceptance slice
// (spec/acceptance/<NNN>/) is actually referenced somewhere in the
// project's canonical verify command -- not merely present on disk.
// `goal_pilot.py`'s own /contract-plan step drafts these slices, but
// staging one into the verify command (a Makefile target, typically) is
// left as a separate step a ticket's own build must do, and nothing
// previously checked that it had been.
//
// Found via real live builds, twice independently: a calculator app repo (the
// pre-existing history behind commit 00e27bb's neighbors) and a notes app repo
// ticket 001 (2026-09-06) both shipped real contract deviations (wrong
// error envelope, wrong timestamp precision, ignored server
// configuration) that slice 001's own acceptance tests -- already
// drafted, sitting on disk the whole time -- would have caught
// immediately, because nothing ever ran them: they were never staged
// into `make verify`/`make verify-full`.
//
// sliceDirs is the caller's own directory listing of spec/acceptance/*
// (this function does no filesystem I/O, matching every other check in
// this package); an empty list means no acceptance suite has been
// drafted yet and always passes -- there is nothing to wire in.
// verifyCommandContent is the content of whatever file declares the
// project's canonical verify command (its Makefile, typically); a slice
// passes once its own directory path appears anywhere in that content,
// on the theory that a real reference to `spec/acceptance/001` (a
// `python3 -m unittest discover -s spec/acceptance/001` line, a
// `cd 'spec/acceptance/001' && go test ./...` line, or an equivalent) is
// what "staged into verify" concretely means -- this does not attempt to
// parse or execute the command, only to catch the case where a slice's
// path is mentioned nowhere at all.
func AcceptanceSuiteWired(verifyCommandContent string, sliceDirs []string) (bool, []string) {
	var reasons []string
	for _, dir := range sliceDirs {
		if !strings.Contains(verifyCommandContent, dir) {
			reasons = append(reasons, fmt.Sprintf("acceptance slice %q is drafted but not referenced anywhere in the verify command -- stage it in before this ticket is considered done", dir))
		}
	}
	return len(reasons) == 0, reasons
}

// ProgramDesignStructure checks the stable shape of the API/program-design
// artifact produced by the Pi contract step. Numbered Markdown headings are
// accepted because the local contract artifacts use both "## Conventions"
// and "## 1. Conventions" forms. At least one endpoint section is required,
// and the conventions section must precede it. This check does not inspect
// endpoint prose or accept a model's review as a pass signal.
func ProgramDesignStructure(programDesignContent string) (bool, []string) {
	sections := markdownSections(programDesignContent)
	// The two local contract artifacts use these equivalent names: the
	// calculator contract calls the section "Conventions", while the budget
	// contract starts with an "Error envelope" section after its preamble.
	conventions := sectionOffsetAny(sections, "conventions", "error envelope")
	endpoint := firstEndpointSection(sections)

	var reasons []string
	if conventions < 0 {
		reasons = append(reasons, "program design is missing the Conventions or Error envelope section")
	}
	if endpoint < 0 {
		reasons = append(reasons, "program design is missing an Endpoint section")
	}
	if conventions >= 0 && endpoint >= 0 && conventions >= endpoint {
		reasons = append(reasons, "program design sections are not in the required order: Conventions before Endpoint")
	}
	return len(reasons) == 0, reasons
}

type markdownSection struct {
	name   string
	offset int
}

func requiredMarkdownSections(content, artifact string, required []string) (bool, []string) {
	sections := markdownSections(content)
	var reasons []string
	for _, requiredName := range required {
		if sectionOffset(sections, requiredName) < 0 {
			reasons = append(reasons, artifact+" is missing the "+requiredName+" section")
		}
	}
	for i := 1; i < len(required); i++ {
		previous := sectionOffset(sections, required[i-1])
		current := sectionOffset(sections, required[i])
		if previous >= 0 && current >= 0 && previous >= current {
			reasons = append(reasons, artifact+" sections are not in the required order")
			break
		}
	}
	return len(reasons) == 0, reasons
}

func markdownSections(content string) []markdownSection {
	var sections []markdownSection
	offset := 0
	inFence := false
	var fenceChar byte
	var fenceLen int
	for _, line := range strings.SplitAfter(content, "\n") {
		raw := strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		trimmed := strings.TrimSpace(raw)
		if inFence {
			if closesMarkdownFence(trimmed, fenceChar, fenceLen) {
				inFence = false
			}
			offset += len(line)
			continue
		}
		if ch, length := markdownFenceMarker(trimmed); ch != 0 {
			inFence, fenceChar, fenceLen = true, ch, length
			offset += len(line)
			continue
		}
		// Require real artifact headings, not indented code examples.
		if raw != strings.TrimLeft(raw, " \t") {
			offset += len(line)
			continue
		}
		if match := markdownHeadingRE.FindStringSubmatch(raw); match != nil {
			name := strings.TrimSpace(match[1])
			name = strings.TrimSpace(numberedSectionRE.ReplaceAllString(name, ""))
			if name != "" {
				sections = append(sections, markdownSection{name: name, offset: offset})
			}
		}
		offset += len(line)
	}
	return sections
}

// stripFencedMarkdown returns content with every fenced-code-block line
// (fence markers included) blanked to spaces, preserving length and line
// offsets so callers can keep using byte offsets into the original content.
// Shares markdownSections' fence-tracking rules so both stay in sync.
func stripFencedMarkdown(content string) string {
	var out strings.Builder
	out.Grow(len(content))
	inFence := false
	var fenceChar byte
	var fenceLen int
	for _, line := range strings.SplitAfter(content, "\n") {
		trimmed := strings.TrimSpace(strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r"))
		if inFence {
			if closesMarkdownFence(trimmed, fenceChar, fenceLen) {
				inFence = false
			}
			out.WriteString(blankExceptNewline(line))
			continue
		}
		if ch, length := markdownFenceMarker(trimmed); ch != 0 {
			inFence, fenceChar, fenceLen = true, ch, length
			out.WriteString(blankExceptNewline(line))
			continue
		}
		out.WriteString(line)
	}
	return out.String()
}

// blankExceptNewline replaces every byte of line with a space except a
// trailing \r\n or \n, so byte offsets computed against the blanked result
// still line up with the original content.
func blankExceptNewline(line string) string {
	body := line
	suffix := ""
	if strings.HasSuffix(body, "\n") {
		body, suffix = body[:len(body)-1], "\n"+suffix
	}
	if strings.HasSuffix(body, "\r") {
		body, suffix = body[:len(body)-1], "\r"+suffix
	}
	return strings.Repeat(" ", len(body)) + suffix
}

func markdownFenceMarker(trimmed string) (byte, int) {
	if len(trimmed) < 3 || (trimmed[0] != '`' && trimmed[0] != '~') {
		return 0, 0
	}
	ch := trimmed[0]
	length := 0
	for length < len(trimmed) && trimmed[length] == ch {
		length++
	}
	if length < 3 {
		return 0, 0
	}
	return ch, length
}

func closesMarkdownFence(trimmed string, ch byte, minLen int) bool {
	length := 0
	for length < len(trimmed) && trimmed[length] == ch {
		length++
	}
	return length >= minLen && strings.TrimSpace(trimmed[length:]) == ""
}

func sectionOffset(sections []markdownSection, want string) int {
	for _, section := range sections {
		if strings.EqualFold(section.name, want) {
			return section.offset
		}
	}
	return -1
}

func sectionOffsetAny(sections []markdownSection, wants ...string) int {
	for _, want := range wants {
		if offset := sectionOffset(sections, want); offset >= 0 {
			return offset
		}
	}
	return -1
}

func firstEndpointSection(sections []markdownSection) int {
	for _, section := range sections {
		name := strings.ToLower(section.name)
		if name == "endpoint" || name == "endpoints" || strings.HasPrefix(name, "endpoint:") || strings.HasPrefix(name, "endpoint ") {
			return section.offset
		}
		if httpEndpointHeadingRE.MatchString(section.name) {
			return section.offset
		}
	}
	return -1
}

// httpEndpointHeadingRE recognizes a real, live-validated alternate
// convention for naming an endpoint section: a heading naming the HTTP
// method and path directly (GET /, POST /calculate, PATCH
// /api/transactions/{id}/category, optionally backtick-wrapped) instead
// of wrapping it under a literal "Endpoint"/"Endpoints" heading. Found
// via a real, non-fixture
// `/contract-plan` run (2026-08-30, calculator-pilot-v2): the generated
// contract used this exact shape under a `## Web server` parent instead
// of `## Endpoints`, and the mandatory project-bootstrap preflight
// rejected a real, fully-built, fully-tested app over a heading-vocabulary
// mismatch alone -- the same two-conventions-for-one-concept shape gap 1's
// `internal/ticketspec` vs. pi-harness-native ticket formats already
// named, just on the contract side this time. Backticks are optional
// (this repo's own budget-pilot contract wraps its endpoint headings in
// backticks; the real run above did not). Anchored only at the start, not
// the end: the real run's own headings carry trailing prose after the
// method+path ("`GET /` -- HTML form page"), which budget-pilot's
// backtick-wrapped-with-nothing-after style never needed to.
var httpEndpointHeadingRE = regexp.MustCompile("^`?(?:GET|POST|PUT|PATCH|DELETE|HEAD|OPTIONS|TRACE|CONNECT)\\s+/\\S*`?(?:\\s|$)")

func headingOffset(content, heading string) int {
	offset := 0
	for _, line := range strings.SplitAfter(content, "\n") {
		if strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r") == heading {
			return offset
		}
		offset += len(line)
	}
	return -1
}

func mentionsMakeVerify(section string) bool {
	for start := 0; ; {
		i := strings.Index(section[start:], "make verify")
		if i < 0 {
			return false
		}
		match := start + i
		after := match + len("make verify")
		if (match == 0 || !commandWordByte(section[match-1])) &&
			(after == len(section) || !commandWordByte(section[after])) {
			return true
		}
		start = after
	}
}

func commandWordByte(b byte) bool {
	return b == '-' || b == '_' || b >= '0' && b <= '9' || b >= 'A' && b <= 'Z' || b >= 'a' && b <= 'z'
}

// harnessByproducts are files the Pi harness itself creates or edits as a
// side effect of running build_app.py -- never something a ticket is
// "about", and never something a ticket author should have to enumerate
// in Allowed-Files to avoid an unrelated diff_scope violation.
//
// Found live across every real run so far (see CLAIMS.md and the plan's
// 2026-08-28 Opus review): the harness's own housekeeping — a
// `.gitignore` line added for `.pi-build-session/`, `BUILD_REPORT.md`, or
// `BUILD_EVIDENCE.json` — rides along in the diff and factoryd's own
// safety-net commit sweeps it in, so a run can quarantine on scope purely
// because of the harness's own byproducts, not the agent's actual change.
//
// `.gitignore` is exempted wholesale here, not content-checked against
// this list — a real limitation, not an oversight: this check only has
// filenames, not `.gitignore`'s base/final content, so it cannot yet
// verify the added lines are *only* these entries. `.gitignore` is not a
// security-sensitive file for this project's threat model (see
// safety-contract.md), so the exemption is judged an acceptable trade
// against the real, repeated false-quarantine cost; a content-aware
// version of this check remains open work.
//
// Deliberately NOT applied inside diffScopeViolations/DiffScope
// themselves: internal/release.MergePolicyCheck reuses DiffScope as a
// *protected*-path membership test ("passes precisely when the changed
// file is protected"), the opposite semantics of a ticket's Allowed-Files
// scope check. Baking this exemption into DiffScope made every changed
// `.gitignore`/`BUILD_REPORT.md`/etc. silently "pass" that check too,
// which MergePolicyCheck reads as "this file is protected" regardless of
// cfg.ProtectedPaths — denying an otherwise-clean release purely because
// routine harness housekeeping was mistaken for a protected-path hit
// (found via codex review round 1, 2026-08-28). ExcludeHarnessByproducts
// is applied only at EvaluateRun's ticket-scope call site instead, so
// DiffScope itself stays a neutral, generic path-set matcher usable
// correctly by both callers.
var harnessByproducts = []string{
	".gitignore",
	"BUILD_REPORT.md",
	"BUILD_EVIDENCE.json",
	".pi-build-session/",
	// The round loop's resume point (build_app.py write_round_state).
	".pi-build-round-state.json",
	// .pi-conformity-session/ is the retired spec-conformity reviewer's
	// session directory, kept for worktrees an older build left it in: a
	// harness session's own .jsonl transcript must never ride along in the
	// diff and quarantine a run on diff_scope.
	".pi-conformity-session/",
	// .pi-code-review-session/ is the retired standalone code review's
	// session directory, kept for the same reason as the entry above.
	".pi-code-review-session/",
	// .pi-combined-review-session/: combined_review.py's session directory
	// (both reviews in one call) -- same reasoning as the two above.
	".pi-combined-review-session/",
}

func isHarnessByproduct(f string) bool {
	for _, b := range harnessByproducts {
		if strings.HasSuffix(b, "/") {
			if f == strings.TrimSuffix(b, "/") || strings.HasPrefix(f, b) {
				return true
			}
			continue
		}
		if f == b {
			return true
		}
	}
	return false
}

// ExcludeHarnessByproducts returns changed with every harness-byproduct
// path removed. Callers that check a ticket's declared Allowed-Files
// scope (EvaluateRun) should filter through this before calling
// DiffScope; callers that use DiffScope for a different membership check
// (e.g. internal/release's protected-path check) must not, since a
// harness byproduct is not exempt from being a protected path.
func ExcludeHarnessByproducts(changed []string) []string {
	filtered := make([]string, 0, len(changed))
	for _, f := range changed {
		if !isHarnessByproduct(f) {
			filtered = append(filtered, f)
		}
	}
	return filtered
}

// ExcludeFactoryOracles returns changed with every path the factory host
// itself committed as an accepted acceptance oracle (or its index, or a
// manifest-declared supersession deletion) removed -- but only where o
// proves the blob at ResultSHA still hashes to the pinned hash (or the
// deleted path is really absent). A path the agent also wrote, with
// different bytes, therefore stays in the inventory. Like
// ExcludeHarnessByproducts it is a sibling filter applied at EvaluateRun's
// ticket-scope call sites, never inside DiffScope. nil o returns changed
// untouched, which is every run that has no committed oracles.
func ExcludeFactoryOracles(changed []string, o *run.OracleEvidence) []string {
	if o == nil || (len(o.Authored) == 0 && len(o.Deleted) == 0) {
		return changed
	}
	filtered := make([]string, 0, len(changed))
	for _, f := range changed {
		if !o.FactoryAuthoredIntact(f) {
			filtered = append(filtered, f)
		}
	}
	return filtered
}

// matchesAllowed reports whether changed file f satisfies one of a
// ticket's declared Allowed-Files patterns. Three forms, checked in order:
//
//   - exact path match (the original, and still the common, form — every
//     already-declared ticket keeps working unchanged);
//   - a directory prefix ending in "/", matching that directory and
//     everything under it recursively — for a whole directory of
//     tool-generated files (e.g. a Flutter `gen-l10n` output directory)
//     whose exact filenames a ticket author can't enumerate up front;
//   - a shell glob (path.Match semantics: "*" matches within one path
//     segment, "?" matches one character, no "/" crossing) — for a known
//     filename pattern within a single directory, e.g.
//     "app/l10n/app_localizations*.dart".
//
// This is deliberately not extended to Required-Changed-Files: a required
// file names one specific file a human wants confirmed changed, and a
// glob there would weaken that to "something in this set changed,"
// defeating the check's own purpose.
func matchesAllowed(f string, patterns []string) bool {
	for _, pattern := range patterns {
		if f == pattern {
			return true
		}
		if strings.HasSuffix(pattern, "/") {
			if strings.HasPrefix(f, pattern) {
				return true
			}
			continue
		}
		if ok, err := path.Match(pattern, f); err == nil && ok {
			return true
		}
	}
	return false
}

// PathCoveredByAllowed reports whether path is covered by allowed, using
// the identical exact/directory-prefix/glob matching matchesAllowed
// applies to a single ticket's own declared Allowed-Files against its own
// diff. Exported for cmd/factoryd's plan-time criterion-paths feasibility
// check (writeAndValidateDraftedTickets), which matches a spec
// criterion's own named file against the UNION of Allowed-Files of every
// ticket that lists the criterion as covered, not one ticket's own diff.
func PathCoveredByAllowed(path string, allowed []string) bool {
	return matchesAllowed(path, allowed)
}

func diffScopeViolations(changed, allowed []string) []string {
	var violations []string
	for _, f := range changed {
		if !matchesAllowed(f, allowed) {
			violations = append(violations, f)
		}
	}
	return violations
}
