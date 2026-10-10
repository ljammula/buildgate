package triage

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"buildgate/internal/conformity"
	"buildgate/internal/evidence"
	"buildgate/internal/policy"
	"buildgate/internal/progress"
	"buildgate/internal/run"
	"buildgate/internal/sanitize"
	"buildgate/internal/ticketspec"
)

// maxTriageLogTailBytes bounds how much of a gate's log Run reads
// when hunting for a failure marker: only the log's own TAIL, since a
// runner's failure summary is conventionally printed last, bounded so a
// pathologically large log can never make triage itself slow or
// memory-heavy.
const maxTriageLogTailBytes = 256 * 1024

// maxTriageSentenceLen caps Run's return value to one short
// sentence -- long enough to be useful, short enough to fit the same
// recap surfaces (`factoryd status`, the halt notification reason) that
// already show today's shorter "gate failed: <check>" text.
const maxTriageSentenceLen = 200

// Run derives one factory-authored sentence explaining why r quarantined
// or halted, from evidence the factory already holds -- GateResults,
// Attempts and their logs, ChangedFiles, and halt codes -- never from the
// agent's own self-reported prose (AgentEvidence; see its doc comment for
// why that distinction is load-bearing everywhere else in this codebase).
// Returns "" when nothing can be said with confidence: this field is
// purely informational, so it must never guess.
func Run(r *run.Run, dataDir string) string {
	if s := triageFailedGate(r, dataDir); s != "" {
		return truncateTriage(s)
	}
	if s := triageHalt(r); s != "" {
		return truncateTriage(s)
	}
	return ""
}

// triageFailedGate returns a sentence for the FIRST failing gate in
// r.GateResults, matching the order EvaluateRun appends them in (the same
// order a human reading the recap would expect to be told about first).
func triageFailedGate(r *run.Run, dataDir string) string {
	for _, g := range r.GateResults {
		if !g.Passed {
			return gateSentence(r, dataDir, g, true)
		}
	}
	return ""
}

// GateFinding is what the factory can say about one failed gate.
type GateFinding struct {
	Check    string
	ExitCode int
	// Sentence is the same factory-authored sentence Run gives for the
	// gate, "" when nothing can be said with confidence.
	Sentence string
	// LogTail is the end of the output of the command the gate ran, for a
	// gate that is a command run on the result (canonical verification,
	// the full suite, a named or repository gate). It is what that command
	// printed, so it is data from the repository under build. Empty for
	// every other gate, and always for the reference oracle, whose output
	// is the oracle's own assertions.
	LogTail string
	// BuildStoppedBySetup is set on canonical_verify when the factory's own
	// records show the build ended at a failing setup command before its
	// first agent turn (BuildStoppedBySetup).
	BuildStoppedBySetup bool
}

// FailedGates returns one finding per failed gate of r, each check once, in
// the order the gates were recorded: Run's sentence for every failing gate
// rather than the first, for a reader that is told about all of them (the
// handoff to a later build attempt). The sentences never name or hint at a
// reference oracle, and the reference_oracle gate itself gets none: its
// log's failing line is the oracle's own assertion.
func FailedGates(r *run.Run, dataDir string) []GateFinding {
	var out []GateFinding
	seen := map[string]bool{}
	for _, g := range r.GateResults {
		if g.Passed || seen[g.Check] {
			continue
		}
		seen[g.Check] = true
		finding := GateFinding{Check: g.Check, ExitCode: g.ExitCode, BuildStoppedBySetup: g.Check == "canonical_verify" && BuildStoppedBySetup(r)}
		if g.Check != policy.ReferenceOracleGateID {
			finding.Sentence = truncateTriage(gateSentence(r, dataDir, g, false))
			finding.LogTail = commandGateLogTail(r, g.Check)
		}
		out = append(out, finding)
	}
	return out
}

// commandGateLogTail returns the end of the log of the command a failed
// gate ran, "" for a gate that ran none. Canonical verification's is the
// verify attempt's log only: when the build itself exited non-zero the log
// that explains the gate is the build's, which is the agent's own
// transcript, not a command's output.
func commandGateLogTail(r *run.Run, check string) string {
	kind := check
	switch {
	case check == "canonical_verify":
		kind = "verify"
	case check == "full_suite_verify" || policy.IsRepoGate(check):
	default:
		named := false
		for _, id := range policy.CommandGateIDs() {
			named = named || id == check
		}
		if !named {
			return ""
		}
	}
	attempt, found := lastAttempt(r, kind)
	if !found {
		return ""
	}
	return readLogTail(attempt.LogPath)
}

// gateSentence is the sentence for one failing gate. forOperator is Run's
// form: a canonical_verify sentence also says how many other gates failed
// (it stands alone), and a diff_scope or spec_conformity sentence may add
// the hint that an approved oracle could be the thing that is wrong, naming
// it. FailedGates lists every gate for a later build attempt, which is not
// told about the oracle, so it gets neither.
func gateSentence(r *run.Run, dataDir string, g run.GateResult, forOperator bool) string {
	switch g.Check {
	case "diff_scope":
		return triageDiffScope(r, dataDir, forOperator)
	case "required_files_changed":
		return triageRequiredFilesChanged(r, dataDir)
	case "required_content_present":
		return triageRequiredContentPresent(r, dataDir)
	case "tests_added":
		return triageTestsAdded(r)
	case "spec_conformity":
		return triageSpecConformity(r, dataDir, forOperator)
	case "canonical_verify":
		if g.SetupNotRun() {
			return "canonical_verify: " + run.SetupNotRunMessage
		}
		if g.ReclaimNotChecked() {
			// The factory's own words; the reason in it is a host-side
			// read's error, made one line where it was recorded.
			return "canonical_verify: " + g.Command[0]
		}
		// Ahead of "the agent made no changes": a build stopped by its
		// setup made none because it never had a turn.
		if s := buildStoppedBySetupSentence(r, forOperator); s != "" {
			return s
		}
		if s := triageNoChanges(r); s != "" {
			return s
		}
		// A live run once quarantined with "canonical_verify failed:
		// exit 1" while diff_scope and required_files_changed also
		// failed, and the actual cause (a Go compile error) was only
		// in the verify log -- triageLogGate/extractFailureMarker
		// above already surface that line when recognized; naming
		// how many other gates also failed keeps this sentence from
		// implying canonical_verify was the only problem. Computed
		// BEFORE triageLogGate, not after, so its length can be
		// reserved up front: sizing the marker's own %q-quoting
		// budget from an already-known suffix length is what
		// guarantees the closing quote and the suffix both survive,
		// rather than truncating the finished sentence afterward
		// and risking cutting the quote off entirely.
		suffix := ""
		if forOperator {
			suffix = passedInsideTheBuildSuffix(r) + otherFailingGatesSuffix(r, g.Check)
		}
		s := triageLogGate(r, g.Check, len(suffix))
		if s == "" {
			return ""
		}
		// fitTriageSuffix stays as a defensive outer bound (a no-op
		// once triageLogGate has already reserved enough room, which
		// it now does for the quoted-marker branch) for the
		// non-quoted branches -- a bounded test-name marker, or the
		// bare "exit N" fallback -- where triageLogGate's
		// suffixReserve isn't itself consulted.
		return fitTriageSuffix(s, suffix)
	default:
		if g.FailsOnBase() {
			return failsOnBaseSentence(g)
		}
		return triageLogGate(r, g.Check, 0)
	}
}

// failsOnBaseSentence words a command gate that failed on the build's result
// and, rerun, on the commit the ticket's work started from
// (run.GateBaseCheck): no build can make it pass, so the sentence tells the
// operator what to change and that nothing was spent on trying. Every word is
// the factory's; the check's name and the commit come from the run record.
func failsOnBaseSentence(g run.GateResult) string {
	base := g.BaseCheck.BaseSHA
	if len(base) > 12 {
		base = base[:12]
	}
	return fmt.Sprintf("%s fails on the base commit %s too, so no build can fix it: fix the gate command or the repository. No corrective build is started.", g.Check, base)
}

// specSnapshotPathFor is the durable, factory-written copy of the
// ticket's spec (see run_ticket.go's own specSnapshotPath), the same file
// every ticketspec.Parse* call in this package already reads at run time
// -- retained in the run's own directory, so it's still readable here
// long after the workspace it was drawn from may be gone.
func specSnapshotPathFor(dataDir, id string) string {
	return filepath.Join(run.Dir(dataDir, id), "spec.snapshot.md")
}

// triageDiffScope recomputes DiffScope's own verdict from the same two
// inputs the gate itself used -- r.ChangedFiles (already durable on the
// run) and the ticket's Allowed-Files (re-read from the durable spec
// snapshot, since GateResult itself records no detail) -- rather than
// guessing which changed file was out of scope.
func triageDiffScope(r *run.Run, dataDir string, oracleHint bool) string {
	allowed, err := ticketspec.ParseAllowedFiles(specSnapshotPathFor(dataDir, r.ID))
	if err != nil || len(allowed) == 0 {
		return ""
	}
	_, violations := policy.DiffScope(policy.ExcludeFactoryOracles(policy.ExcludeHarnessByproducts(r.ChangedFiles), r.Oracles), allowed)
	if len(violations) == 0 {
		return ""
	}
	base := fmt.Sprintf("diff_scope: changed %s outside Allowed-Files", strings.Join(violations, ", "))
	if !oracleHint {
		return base
	}
	if hint := oracleScopeHint(r); hint != "" {
		return base + " -- " + hint
	}
	return base
}

// maxTriageOracleManifestBytes bounds the MANIFEST.json read below --
// operator-populated (or drafter-populated) content, same untrusted-input
// caution evidence.ReadHostileFile's own doc comment describes.
const maxTriageOracleManifestBytes = 64 * 1024

// oracleScopeHint returns a hedged, one-clause addition to a diff_scope
// triage sentence when this run's OWN gate evidence is consistent with the
// out-of-scope edit having been made to satisfy a reference oracle, not a
// defect in the build. Found in a 2026-09-22 live run (a calculator app repo run
// 2): an already-accepted oracle's own errors.Is assertion (comparing two
// independently-constructed wrapped errors, which
// is never == without a custom Is method) forced an otherwise-correct,
// in-scope ticket to add an Is(error) bool method to a file outside its own
// declared Allowed-Files just to keep the oracle's own re-run passing --
// the build agent diagnosed this correctly in its own report, but that
// self-report is exactly the kind of agent-authored prose this package's
// own doc comment forbids trusting (see Run's own doc comment), so this
// derives the same signal from durable gate evidence instead: a reference
// oracle was active for this run (r.ReferenceOracleDir != "") AND its own
// gate PASSED this run (conformity.OracleOutcome), i.e. the build
// succeeded specifically at satisfying the oracle -- exactly the situation
// under which an over-reaching oracle assertion can force a scope-widening
// edit. This never claims the oracle caused THIS SPECIFIC violation (that
// would be the guess Run's own doc comment forbids); it only surfaces the
// possibility, hedged, so a human reviewer checks the oracle before
// blaming the build.
func oracleScopeHint(r *run.Run) string {
	if r.ReferenceOracleDir == "" {
		return ""
	}
	passed := conformity.OracleOutcome(r.GateResults)
	if passed == nil || !*passed {
		return ""
	}
	name := "(the reference oracle)"
	if data, err := evidence.ReadHostileFile(filepath.Join(r.ReferenceOracleDir, "MANIFEST.json"), maxTriageOracleManifestBytes); err == nil {
		if covered, parseErr := conformity.ParseOracleManifest(data); parseErr == nil && len(covered) > 0 {
			name = strings.TrimSpace(covered[0])
			if len(covered) > 1 {
				name += fmt.Sprintf(" (+%d more criteria)", len(covered)-1)
			}
		}
	}
	return fmt.Sprintf(
		"the file was touched to satisfy oracle %s: the oracle may be wrong, not the build (check its errors.Is/sentinel assertions and whether it reaches beyond its own target file)",
		name,
	)
}

// maxTriageConformityEvidenceBytes bounds the CONFORMITY_EVIDENCE.json read
// below -- the reviewer's own untrusted output (produced by an LLM reading
// the agent's diff), same caution as maxTriageOracleManifestBytes above.
const maxTriageConformityEvidenceBytes = 256 * 1024

// maxTriageConformityDetailLen bounds how much of a flagged verdict's own
// "detail" field triageSpecConformity quotes -- long enough to be useful,
// short enough that one criterion's detail can't crowd out the rest of the
// sentence (Run's own 200-byte overall cap truncates further if needed).
const maxTriageConformityDetailLen = 80

// leadingCriterionNumber extracts a criterion's own leading "N." / "N)"
// ordinal, the same convention conformity.NormalizeCriterion strips.
var leadingCriterionNumber = regexp.MustCompile(`^\s*(\d+)[.)]`)

// criterionNumber returns criterion's leading ordinal ("2" from
// "2. Handles empty input."), or the trimmed criterion text itself when it
// carries no leading number -- never empty for a non-empty criterion, so
// callers always have something to name.
func criterionNumber(criterion string) string {
	if m := leadingCriterionNumber.FindStringSubmatch(criterion); m != nil {
		return m[1]
	}
	return strings.TrimSpace(criterion)
}

// readConformityVerdicts reads and parses this run's own retained
// CONFORMITY_EVIDENCE.json (run.Dir/CONFORMITY_EVIDENCE.json, the same
// host-retained copy loadSpecConformityVerdicts reads -- see its own doc
// comment for why the retained copy, not the workspace original). Treated
// as untrusted like every other agent-influenced evidence file this
// package reads (evidence.ReadHostileFile): a missing or malformed file
// returns ok=false so the caller falls back to the plain log-based
// sentence, never a guess.
func readConformityVerdicts(r *run.Run, dataDir string) (verdicts []run.ReviewVerdict, ok bool) {
	path := filepath.Join(run.Dir(dataDir, r.ID), "CONFORMITY_EVIDENCE.json")
	data, err := evidence.ReadHostileFile(path, maxTriageConformityEvidenceBytes)
	if err != nil {
		return nil, false
	}
	verdicts, err = conformity.ParseVerdicts(data)
	if err != nil {
		return nil, false
	}
	return verdicts, true
}

// triageSpecConformity reports the criteria the independent conformity
// reviewer flagged, and -- when the run's OWN gate evidence is consistent
// with an approved oracle having encoded the wrong behaviour for one of
// those criteria -- names the oracle so a human checks it before retrying
// the build (live incident 2026-09-24: an operator-approved oracle
// wrongly expected non-RFC-7396 merge-patch semantics, the build agent
// invented a rule to satisfy it, reference_oracle passed, and
// spec_conformity correctly flagged the criteria the wrong oracle covered
// -- but the triage sentence at the time just said "spec_conformity
// failed: exit 1", giving no hint the approved oracle was the likely
// culprit). Falls back to the plain log-based sentence (triageLogGate)
// whenever CONFORMITY_EVIDENCE.json is missing/malformed or nothing was
// flagged, so this never invents criteria the evidence doesn't support.
func triageSpecConformity(r *run.Run, dataDir string, oracleHint bool) string {
	verdicts, ok := readConformityVerdicts(r, dataDir)
	if !ok {
		return triageLogGate(r, "spec_conformity", 0)
	}
	var flagged []run.ReviewVerdict
	for _, v := range verdicts {
		if v.Verdict == "flagged" {
			flagged = append(flagged, v)
		}
	}
	if len(flagged) == 0 {
		return triageLogGate(r, "spec_conformity", 0)
	}
	nums := make([]string, 0, len(flagged))
	for _, v := range flagged {
		nums = append(nums, criterionNumber(v.Criterion))
	}
	base := fmt.Sprintf("spec_conformity failed: criteria %s flagged", strings.Join(nums, ", "))
	if oracleHint {
		if hint := conformityOracleHint(r, flagged); hint != "" {
			return base + " -- " + hint
		}
	}
	for _, v := range flagged {
		detail := strings.TrimSpace(v.Detail)
		if detail == "" {
			continue
		}
		if len(detail) > maxTriageConformityDetailLen {
			detail = detail[:maxTriageConformityDetailLen] + "..."
		}
		return base + ": " + detail
	}
	return base
}

// conformityOracleHint returns a hedged clause naming any approved
// oracle(s) that cover a criterion the conformity reviewer just flagged --
// only when this run's OWN gate evidence shows the reference_oracle gate
// PASSED (i.e. the build succeeded specifically at satisfying that
// oracle), the same "passed, not merely configured" precondition
// oracleScopeHint applies for the same reason (see its own doc comment).
// Returns "" whenever the oracle didn't pass, wasn't configured, its
// manifest can't be read, or none of the flagged criteria are
// oracle-covered -- never a guess.
func conformityOracleHint(r *run.Run, flagged []run.ReviewVerdict) string {
	if r.ReferenceOracleDir == "" {
		return ""
	}
	passed := conformity.OracleOutcome(r.GateResults)
	if passed == nil || !*passed {
		return ""
	}
	data, err := evidence.ReadHostileFile(filepath.Join(r.ReferenceOracleDir, "MANIFEST.json"), maxTriageOracleManifestBytes)
	if err != nil {
		return ""
	}
	coverage, err := conformity.ParseOracleCoverage(data)
	if err != nil || len(coverage) == 0 {
		return ""
	}
	oracleByCriterion := make(map[string]string, len(coverage))
	for _, c := range coverage {
		oracleByCriterion[conformity.NormalizeCriterion(c.Criterion)] = c.OracleFile
	}
	var nums, names []string
	seen := make(map[string]bool)
	for _, v := range flagged {
		oracle, covered := oracleByCriterion[conformity.NormalizeCriterion(v.Criterion)]
		if !covered {
			continue
		}
		nums = append(nums, criterionNumber(v.Criterion))
		if !seen[oracle] {
			seen[oracle] = true
			names = append(names, oracle)
		}
	}
	if len(nums) == 0 {
		return ""
	}
	// Kept short and name-first so Run's maxTriageSentenceLen cap can't cut
	// off the actionable part: at most two oracle names, then a count.
	shown := names
	more := ""
	if len(names) > 2 {
		shown = names[:2]
		more = fmt.Sprintf(" +%d more", len(names)-2)
	}
	return fmt.Sprintf("approved oracle(s) %s%s may encode the wrong behaviour; re-check before retrying", strings.Join(shown, ", "), more)
}

// triageRequiredFilesChanged mirrors triageDiffScope for the
// required_files_changed gate.
func triageRequiredFilesChanged(r *run.Run, dataDir string) string {
	required, err := ticketspec.ParseRequiredChangedFiles(specSnapshotPathFor(dataDir, r.ID))
	if err != nil || len(required) == 0 {
		return ""
	}
	_, missing := policy.RequiredFilesChanged(r.ChangedFiles, required)
	if len(missing) == 0 {
		return ""
	}
	return fmt.Sprintf("required_files_changed: %s untouched", strings.Join(missing, ", "))
}

// triageRequiredContentPresent looks for the ticket's declared
// Required-Content markers among the added lines of the run's own
// snapshotted diff (run.DiffPath) -- durable factory evidence of exactly
// what this run added, the same evidence a human clicking "view diff"
// would read. The first declared marker that never appears on an added
// line is reported; if the diff was never captured (DiffAvailable false)
// this returns "" rather than guessing which marker was missing.
func triageRequiredContentPresent(r *run.Run, dataDir string) string {
	required, err := ticketspec.ParseRequiredContent(specSnapshotPathFor(dataDir, r.ID))
	if err != nil || len(required) == 0 || !r.DiffAvailable {
		return ""
	}
	diff, err := os.ReadFile(run.DiffPath(dataDir, r.ID))
	if err != nil {
		return ""
	}
	added := addedDiffLines(string(diff))
	for _, marker := range required {
		found := false
		for _, line := range added {
			if strings.Contains(line, marker) {
				found = true
				break
			}
		}
		if !found {
			return fmt.Sprintf("required_content_present: missing %q", marker)
		}
	}
	return ""
}

// addedDiffLines returns the content of every unified-diff added line
// (a "+" line, excluding the "+++ b/path" file header), stripped of its
// leading "+".
func addedDiffLines(diff string) []string {
	var out []string
	for _, line := range strings.Split(diff, "\n") {
		if strings.HasPrefix(line, "+++") {
			continue
		}
		if strings.HasPrefix(line, "+") {
			out = append(out, line[1:])
		}
	}
	return out
}

// triageTestsAdded reports either the ticket's own declared opt-out
// reason (Tests-Required: no -- <reason>, already recorded on the run) or
// the plain fact that no changed file matched a test pattern -- both are
// durable, factory-recorded facts, never a guess at which file "should
// have" been a test.
func triageTestsAdded(r *run.Run) string {
	if r.TestsRequiredOptOut != "" {
		return fmt.Sprintf("tests_added: opted out (%s)", r.TestsRequiredOptOut)
	}
	return "tests_added: no test file changed"
}

// triageNoChanges special-cases a canonical_verify failure that comes
// with an empty changed-file inventory: the agent's build attempts ran
// but never touched the workspace, evidence already durable on the run
// (r.ChangedFiles, r.Attempts) rather than build_app.py's own
// self-reported stopped_reason, which this codebase's ground rules
// forbid using to explain a factory decision (see run.AgentEvidence's own
// doc comment). Returns "" for every other case, including "evidence
// collection was never attempted" (r.ChangedFiles == nil, distinct from
// a collected empty slice -- see that field's own doc comment).
func triageNoChanges(r *run.Run) string {
	if r.ChangedFiles == nil || len(r.ChangedFiles) > 0 {
		return ""
	}
	rounds := 0
	for _, a := range r.Attempts {
		if a.Kind == "build" {
			rounds++
		}
	}
	if rounds == 0 {
		return ""
	}
	return fmt.Sprintf("build: agent made no changes in %d round%s", rounds, progress.Plural(rounds))
}

// triageLogGate reports the first failure marker found in the failing
// check's own attempt log, or a generic "exit N" when no runner-specific
// marker is recognized. suffixReserve is how many bytes the caller will
// append after this sentence (e.g. otherFailingGatesSuffix's result) --
// consulted only by the wholeLine-marker branch, which is the one that
// closes a %q quote and so is the one where truncating the FINISHED
// sentence afterward (fitTriageSuffix) risks cutting the closing quote
// off entirely. Every other caller (spec_conformity's own fallback, the
// default case) passes 0.
func triageLogGate(r *run.Run, check string, suffixReserve int) string {
	attempt, ok := attemptForCheck(r, check)
	if !ok {
		return ""
	}
	logPath, exitCode := attempt.LogPath, attempt.ExitCode
	if sentence := setupFailureSentence(check, attempt, suffixReserve); sentence != "" {
		return sentence
	}
	if marker, wholeLine := extractFailureMarker(logPath); marker != "" {
		if wholeLine {
			// An adversarial review of #17's fix found: marker is a
			// line copied straight out of the build/verify log, which
			// agent-written code produced -- untrusted the same way any
			// other agent output is (sanitize.Line, in extractFailureMarker,
			// already stripped ANSI/control/format characters and folded
			// any line-breaking whitespace, but the TEXT itself is still
			// the agent's own words). Presented as an explicit quote FROM
			// THE LOG, never phrased as a factory verdict, so a planted
			// line can at most misdirect an operator to look at the log
			// themselves, not impersonate this package's own authorship.
			return quotedLogMarkerSentence(check, marker, suffixReserve)
		}
		return fmt.Sprintf("%s failed: %s", check, marker)
	}
	if exitCode != 0 {
		return fmt.Sprintf("%s failed: exit %d", check, exitCode)
	}
	return ""
}

// setupFailureSentence words a step that exited run.SetupFailedExitCode after
// running the repository's setup commands (its attempt carries their digest):
// one failed, and the step's log names it. The name is the log's text, so it
// is quoted as every log-derived sentence quotes. "" for any other step: a
// step that ran no setup exits 95 for its own reasons, and a line it prints
// is not the factory's to word.
func setupFailureSentence(check string, a run.Attempt, suffixReserve int) string {
	if a.ExitCode != run.SetupFailedExitCode || a.SetupSHA256 == "" {
		return ""
	}
	cmd := run.SetupFailedCommand(readLogTail(a.LogPath))
	if cmd == "" {
		return ""
	}
	prefix := fmt.Sprintf("%s failed; setup command failed: ", check)
	return prefix + safeQuote(sanitize.Line(cmd), maxTriageSentenceLen-len(prefix)-suffixReserve)
}

// BuildStoppedBySetup reports whether r's build ended at a failing repository
// setup command (`.factory.yml` setup:) before its first agent turn, as far
// as the factory's own records can show it. All of these must hold:
//
//   - the run has setup commands and its last build attempt exited
//     run.SetupFailedExitCode, as the build script does in that case. The
//     exit status and the line in the log are the sandbox's, so they are
//     necessary and decide nothing alone;
//   - the meter counted nothing for any build attempt of the run: each ran
//     on a metered route, with no input or output token, no cost, no
//     unsettled request and no ceiling hit (the ledger is kept outside the
//     sandbox, and a worker reaches a model only through it);
//   - the run's result is the commit it started from: nothing was committed;
//   - the run did not adopt a lost run's worktree, whose uncommitted work
//     an earlier session made.
//
// Every build of the ticket from that commit runs the same commands first
// and ends the same way, so no corrective build answers it
// (internal/handoff). A setup failure in any other step (the verify, a gate,
// a rerun after the oracle commit) runs on the build's result and is that
// step's own failed check.
func BuildStoppedBySetup(r *run.Run) bool {
	if r.ResumeSpendCarried != nil || r.BaseSHA == "" || r.ResultSHA != r.BaseSHA {
		return false
	}
	last, found := lastAttempt(r, "build")
	if !found || last.ExitCode != run.SetupFailedExitCode || last.SetupSHA256 == "" {
		return false
	}
	for _, a := range r.Attempts {
		if a.Kind == "build" && !meteredAndUnused(a) {
			return false
		}
	}
	return true
}

// meteredAndUnused reports whether a ran on a metered model route and the
// meter counted no request for it. An attempt that resumed an interrupted
// one holds that one's work and is never unused.
func meteredAndUnused(a run.Attempt) bool {
	metered := a.RelayRoute != "" || a.RelayCredentialMode != ""
	spent := a.RelayConsumedInputTokens != 0 || a.RelayConsumedOutputTokens != 0 || a.RelayConsumedCostMicroUSD != 0
	return metered && !spent && !a.RelaySpendPartial && !a.RelayCeilingExceeded && a.ResumedFromCheckpoint == ""
}

// buildStoppedBySetupSentence is canonical_verify's sentence for a run whose
// build setup stopped (BuildStoppedBySetup), "" for any other run. It holds
// no text of the build's log, which is where the build script names the
// command.
func buildStoppedBySetupSentence(r *run.Run, forOperator bool) string {
	if !BuildStoppedBySetup(r) {
		return ""
	}
	s := "the build stopped before its first agent turn: a .factory.yml setup: command failed (no model call, no commit); the build log names it"
	if !forOperator {
		return s
	}
	return fitTriageSuffix(s, otherFailingGatesSuffix(r, "canonical_verify"))
}

// passedInsideTheBuildSuffix says, for a failed canonical_verify, that the
// build's own last run of the same command passed. It states only that
// fact, in few bytes (the marker ahead of it shares the sentence's limit);
// USAGE.md's troubleshooting table says what it usually means: the build's
// container has whatever its rounds left outside the repository (an
// installed package, a cache, a file under $HOME) and the verify's is
// fresh, or the test is flaky. Found live 2026-10-08: a verify command missing a test
// plugin failed round 1, the agent installed the plugin in its container,
// rounds 2 and 3 passed, and the run quarantined as "canonical_verify
// failed: exit 1" with nothing saying why a passing build did not verify.
// "" when the build did not fail that way.
func passedInsideTheBuildSuffix(r *run.Run) string {
	if r.AgentEvidence == nil || len(r.AgentEvidence.Rounds) == 0 {
		return ""
	}
	if a, found := lastAttempt(r, "build"); found && a.ExitCode != 0 {
		return ""
	}
	last := r.AgentEvidence.Rounds[len(r.AgentEvidence.Rounds)-1]
	if last.VerifyPassed == nil || !*last.VerifyPassed {
		return ""
	}
	return fmt.Sprintf("; it passed inside the build (round %d)", last.Index)
}

// otherFailingGatesSuffix counts r.GateResults entries other than check
// that also failed, and returns a "(+N more failing gate(s))" suffix, or
// "" when check was the only failing gate -- a canonical_verify sentence
// must not read as though it were the run's only problem when
// diff_scope/required_files_changed/etc. failed too.
func otherFailingGatesSuffix(r *run.Run, check string) string {
	count := 0
	for _, g := range r.GateResults {
		if !g.Passed && g.Check != check {
			count++
		}
	}
	if count == 0 {
		return ""
	}
	return fmt.Sprintf(" (+%d more failing gate%s)", count, progress.Plural(count))
}

// attemptForCheck finds the log evidence for a failing named/canonical
// gate. canonical_verify is special: it fails when either the build or
// the verify exit code is nonzero (policy.CanonicalVerify), but the
// gate's own recorded ExitCode is always the verify command's -- so a
// build that itself failed is checked first, falling back to the verify
// attempt's own log otherwise. Every other check's Attempt.Kind is
// recorded identically to its GateResult.Check (see run_ticket.go's
// runGate/full-suite/spec-conformity attempt construction).
func attemptForCheck(r *run.Run, check string) (run.Attempt, bool) {
	if check == "canonical_verify" {
		if a, found := lastAttempt(r, "build"); found && a.ExitCode != 0 {
			return a, true
		}
		return lastAttempt(r, "verify")
	}
	return lastAttempt(r, check)
}

// lastAttempt returns the LAST attempt of the given kind, matching
// Run.Sandboxed's own precedent: a retried check's final attempt is the
// one whose outcome this run's own state was actually decided on.
func lastAttempt(r *run.Run, kind string) (run.Attempt, bool) {
	for i := len(r.Attempts) - 1; i >= 0; i-- {
		if r.Attempts[i].Kind == kind {
			return r.Attempts[i], true
		}
	}
	return run.Attempt{}, false
}

// failureMarkerPattern is one runner's own first-failure convention.
// wholeLine marks the two entries below (go build, exception) whose
// capture is the failure line itself, not a bounded test name -- see
// extractFailureMarker's own doc comment for why that distinction
// matters for both length and how the caller must present it.
type failureMarkerPattern struct {
	re        *regexp.Regexp
	runner    string
	wholeLine bool
}

// maxTriageMarkerLen bounds a wholeLine marker's captured text BEFORE it
// is wrapped into a sentence: the regex's own `.+?` is otherwise bounded
// only by the line length, so a single very long log line could by
// itself consume triageLogGate's whole maxTriageSentenceLen budget,
// leaving fitTriageSuffix nothing to work with.
const maxTriageMarkerLen = 160

// failureMarkerPatterns covers the runners this repository's own gates
// and the projects factoryd builds for commonly use. Each regexp's first
// capture group is the failing test's own name, except the two wholeLine
// entries below (go build, exception), where a test name doesn't apply
// and the captured text is the failure line itself.
//
// The go build entry was added after a live run quarantined as
// "canonical_verify failed: exit 1" while the verify log's own first
// line -- `internal/habit/habit_current_streak_test.go:148:2: expected
// '}', found 'EOF'` -- a plain `go build`/`go vet` compile error, went
// unquoted because no pattern here recognized it. The exception entry
// (originally "python traceback") is renamed and no longer matches
// `Warning:`: a warning is never itself a failure cause, and the
// runner-specific name was already wrong for e.g. Node's `TypeError:`,
// which this same pattern also matches.
var failureMarkerPatterns = []failureMarkerPattern{
	{re: regexp.MustCompile(`(?m)^--- FAIL: (\S+)`), runner: "go test"},
	{re: regexp.MustCompile(`(?m)^FAILED (\S+)`), runner: "pytest"},
	{re: regexp.MustCompile(`(?m)^_{3,} (.+?) _{3,}\s*$`), runner: "pytest"},
	{re: regexp.MustCompile(`(?m)^ERROR (\S+\.py\S*)`), runner: "pytest"},
	{re: regexp.MustCompile(`(?m)^\s*(?:\x{25cf}|\x{2715})\s+(.+?)\s*$`), runner: "jest"},
	{re: regexp.MustCompile(`(?m)^\d\d:\d\d\s+\+\d+\s+-\d+:\s+(.+?)\s*\[E\]\s*$`), runner: "flutter test"},
	{re: regexp.MustCompile(`(?m)^test (\S+) \.\.\. FAILED`), runner: "cargo test"},
	{re: regexp.MustCompile(`(?m)^(\S+\.go:\d+:\d+:\s.+?)\s*$`), runner: "go build", wholeLine: true},
	{re: regexp.MustCompile(`(?m)^(\w+(?:\.\w+)*(?:Error|Exception): .+?)\s*$`), runner: "exception", wholeLine: true},
}

// extractFailureMarker reads logPath's own tail and returns the
// earliest-occurring runner failure marker found there, plus whether it
// is a wholeLine marker (a raw log line -- see triageLogGate for how
// that changes the sentence built around it) as opposed to a bounded
// test/case name formatted "<name> (<runner>)". "" if none of the known
// conventions matched.
//
// Every captured marker -- wholeLine or not -- is run through
// sanitize.Line, not sanitize.Text, before use: the matched text comes
// from a build/verify log an agent-controlled process wrote, so it is
// exactly as untrusted as any other agent output this codebase already
// refuses to trust verbatim
// (see run.AgentEvidence's own doc comment on that ground rule), and
// this sentence reaches factoryd status/notifications/the console as a
// SINGLE line with no filtering of its own downstream -- sanitize.Text
// alone leaves a `\r`/U+2028 embedded in the matched text free to
// rewrite or split that line once printed to a real terminal (a `(.+?)`
// capture from ANY of these patterns, not just the wholeLine ones,
// could carry one).
func extractFailureMarker(logPath string) (marker string, wholeLine bool) {
	// Colour codes are stripped before matching, not only from the captured
	// name: a runner that colours its output (pytest under `--color=yes`)
	// starts a failure line with one, and every pattern is anchored at the
	// line start.
	content := sanitize.Text(readLogTail(logPath))
	if content == "" {
		return "", false
	}
	bestPos := -1
	for _, p := range failureMarkerPatterns {
		loc := p.re.FindStringSubmatchIndex(content)
		if loc == nil {
			continue
		}
		name := sanitize.Line(content[loc[2]:loc[3]])
		if name == "" {
			continue
		}
		if bestPos != -1 && loc[0] >= bestPos {
			continue
		}
		bestPos = loc[0]
		if p.wholeLine {
			marker = truncateMarker(name)
			wholeLine = true
		} else {
			marker = fmt.Sprintf("%s (%s)", name, p.runner)
			wholeLine = false
		}
	}
	return marker, wholeLine
}

// FirstFailureLine is the earliest failure marker in the log at logPath,
// as extractFailureMarker finds it, for a caller that reports a failed
// command outside a gate (the baseline verify); "" when none is recognised.
func FirstFailureLine(logPath string) string {
	marker, _ := extractFailureMarker(logPath)
	return marker
}

// truncateMarker caps s at maxTriageMarkerLen bytes, at a UTF-8-safe
// boundary -- see maxTriageMarkerLen's own doc comment. A coarse,
// early pre-bound only: quotedLogMarkerSentence does its own precise,
// budget-aware truncation later, sized from the actual sentence prefix
// and suffix reserve -- this just keeps an absurdly long single log line
// from being carried around at all before that happens.
func truncateMarker(s string) string {
	if len(s) <= maxTriageMarkerLen {
		return s
	}
	cut := maxTriageMarkerLen
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// quotedLogMarkerSentence builds triageLogGate's own wholeLine-marker
// sentence -- "<check> failed; first error in log: \"<marker>\"" --
// sizing marker's %q-quoted budget from the ACTUAL prefix text (which
// varies with check's own length) and suffixReserve, so the closing
// quote and the suffix a caller appends afterward both always survive:
// the earlier version truncated the FINISHED sentence (fitTriageSuffix)
// to make room for the suffix, which could cut the closing quote off
// entirely once a maxTriageMarkerLen-sized marker plus its %q escaping
// had already used up the whole maxTriageSentenceLen budget on its own.
func quotedLogMarkerSentence(check, marker string, suffixReserve int) string {
	prefix := fmt.Sprintf("%s failed; first error in log: ", check)
	budget := maxTriageSentenceLen - len(prefix) - suffixReserve
	return prefix + safeQuote(marker, budget)
}

// safeQuote returns strconv.Quote(s), truncating s first (at a UTF-8-safe
// boundary) as many times as it takes for the QUOTED result to fit
// within budget bytes -- unlike a plain byte-slice of an already-quoted
// string, this can never leave a dangling, unterminated quote, because
// it only ever truncates the RAW text before quoting it, never the
// quoted output itself. budget < 2 (not enough room for even `""`) is
// treated as 2, since an empty quoted string is the smallest sentinel
// this can produce.
func safeQuote(s string, budget int) string {
	if budget < 2 {
		budget = 2
	}
	if len(s) > budget-2 {
		cut := budget - 2
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = s[:cut]
	}
	for {
		quoted := strconv.Quote(s)
		if len(quoted) <= budget || s == "" {
			return quoted
		}
		_, size := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-size]
	}
}

// readLogTail returns up to the last maxTriageLogTailBytes of logPath, or
// "" if it can't be opened/read -- a missing or unreadable log is not a
// reason for Run to fail, only to say less.
func readLogTail(logPath string) string {
	if logPath == "" {
		return ""
	}
	f, err := os.Open(logPath)
	if err != nil {
		return ""
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return ""
	}
	start := int64(0)
	if info.Size() > maxTriageLogTailBytes {
		start = info.Size() - maxTriageLogTailBytes
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return ""
	}
	buf, err := io.ReadAll(f)
	if err != nil {
		return ""
	}
	return string(buf)
}

// triageHalt classifies a halted run's own recorded evidence, in the
// priority order HaltReasonCode's own doc comment implies: a fixed
// machine-readable code first (the one case this codebase already
// distinguishes deliberately), then a classified HaltError, then
// HaltError's own first line verbatim as a last resort.
func triageHalt(r *run.Run) string {
	if r.State != run.StateHalted {
		return ""
	}
	switch r.HaltReasonCode {
	case run.HaltReasonRelayCeilingExceeded:
		return "halted: relay budget ceiling exceeded"
	case run.HaltReasonComposeServicesRejected:
		return "halted: the target repo's compose file was rejected before the build; fix the named services or compose_services_* config and retry"
	case run.HaltReasonReviewInstructionsFailed:
		return reviewInstructionsSentence(r)
	case run.HaltReasonFactoryDirFailed:
		return factoryDirSentence(r)
	case run.HaltReasonBaselineVerifyFailed:
		// No model call was made: the verify command failed on the base
		// commit with a failure the ticket does not name.
		if r.BaselineVerify != nil {
			return "halted before the build: baseline verify " + r.BaselineVerify.Summary()
		}
		return "halted before the build: the verify command fails on the untouched repository"
	}
	if r.HaltError == "" {
		return ""
	}
	if s := classifyHaltError(r.HaltError); s != "" {
		return s
	}
	firstLine, _, _ := strings.Cut(r.HaltError, "\n")
	firstLine = strings.TrimSpace(firstLine)
	if firstLine == "" {
		return ""
	}
	return "halted: " + firstLine
}

// reviewInstructionsPrefix opens the operator's sentence for a run halted
// because a review could not be given the base commit's instruction files
// (SC-019): an operator finding, since no ticket change fixes it.
const reviewInstructionsPrefix = "halted before the review (operator finding): the repository's instruction files could not be prepared: "

// reviewInstructionsSentence quotes the cleaned error the halted review
// attempt recorded. It is for the operator only: handoff.Build never uses it.
func reviewInstructionsSentence(r *run.Run) string {
	for i := len(r.Attempts) - 1; i >= 0; i-- {
		if cause := r.Attempts[i].ReviewInstructionsError; cause != "" {
			return reviewInstructionsPrefix + safeQuote(cause, maxTriageSentenceLen-len(reviewInstructionsPrefix))
		}
	}
	return "halted before the review (operator finding): the repository's instruction files could not be prepared; see the review attempt's record"
}

// factoryDirPrefix opens the operator's sentence for a run halted because a
// sandbox could not be given .factory/ as the commit .factory.yml was read
// from holds it: an operator finding, since no ticket change fixes it.
const factoryDirPrefix = "halted (operator finding): .factory/ cannot be mounted read-only: "

// factoryDirSentence quotes the cleaned reason the refused attempt recorded,
// which names the path and what is wrong with it.
func factoryDirSentence(r *run.Run) string {
	for i := len(r.Attempts) - 1; i >= 0; i-- {
		if cause := r.Attempts[i].FactoryDirError; cause != "" {
			return factoryDirPrefix + safeQuote(cause, maxTriageSentenceLen-len(factoryDirPrefix))
		}
	}
	return "halted (operator finding): .factory/ cannot be mounted read-only as the commit .factory.yml was read from holds it; see the refused attempt's record"
}

// classifyHaltError recognizes the handful of infrastructure-failure
// wordings this codebase's own sandbox package actually produces (see
// internal/sandbox's mount-visibility, image-pull, and readiness-timeout
// errors, compose services' start/pull failures, and sandbox_exec.go's
// own "sandbox attempts exhausted" wrap),
// returning "" for anything else so triageHalt falls back to HaltError's
// own first line rather than mis-tagging an error this list doesn't
// cover.
func classifyHaltError(haltErr string) string {
	lower := strings.ToLower(haltErr)
	switch {
	// Before the generic "timeout" case, which would blame the sandbox or
	// relay. RunBuildActivity/RunVerifyActivity heartbeat from their own
	// ticker regardless of build progress (workflow.heartbeatWhileRunning),
	// so a heartbeat timeout means the factoryd process hosting the
	// Activity died, froze, or lost Temporal -- not a slow build. The run
	// is not resumed (durable resume is deferred; see README.md), so
	// the operator's move is a retry -- see workerStoppedTriage for when.
	case strings.Contains(lower, "heartbeat timeout"):
		return workerStoppedTriage
	// Compose cases before "sandbox attempts exhausted" and "pull": a
	// sidecar that never went healthy wraps as attempts exhausted, and a
	// compose image pull contains "pull", but neither is the worker
	// sandbox's own failure.
	case strings.Contains(lower, "timed out waiting for the host-wide compose sidecar slot"):
		return "halted: queued behind another run's compose sidecars (compose_services_concurrency) for a whole run's timeout; retry once that run finishes"
	case strings.Contains(lower, "start compose services"):
		return "halted: compose services failed to start; service logs are under attempt-N/compose/"
	case strings.Contains(lower, "pull compose images"):
		return "halted: compose service image pull failure"
	case strings.Contains(lower, "sandbox attempts exhausted"):
		return "halted: sandbox attempts exhausted"
	case strings.Contains(lower, "mount"):
		return "halted: Docker mount/visibility failure"
	case strings.Contains(lower, "pull"):
		return "halted: sandbox image pull failure"
	case strings.Contains(lower, "timeout"), strings.Contains(lower, "did not report listening"):
		return "halted: sandbox/relay timeout"
	default:
		return ""
	}
}

// workerStoppedTriage is classifyHaltError's sentence for an Activity
// heartbeat timeout -- see that case's comment. A request waits in
// resume_review for `factoryd resume <id>`; a single-ticket run is started
// again. The old Activity may still be running if the process only froze or
// lost Temporal; its next heartbeat round-trip cancels it, and the run's
// kept worktree is what a resume continues from.
const workerStoppedTriage = "halted: the factoryd process running this build stopped mid-run (restarted, crashed, slept, or lost Temporal); resume the request (factoryd resume <id>) or start a single-ticket run again"

// truncateTriage caps s at maxTriageSentenceLen bytes -- Run.Triage's own
// doc comment promises callers a short sentence, not an unbounded one.
func truncateTriage(s string) string {
	if len(s) <= maxTriageSentenceLen {
		return s
	}
	cut := maxTriageSentenceLen
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// fitTriageSuffix truncates s (at a UTF-8-safe boundary), not suffix, so
// that s+suffix fits within maxTriageSentenceLen, then appends suffix.
// Appending a suffix and letting truncateTriage's later cap run on the
// combined string can silently drop the suffix entirely, or cut it
// mid-word, whenever the sentence ahead of it (e.g. a long Go
// compile-error line) already used up the budget. Truncating the
// sentence FIRST, reserving room for the whole suffix, keeps "(+N more
// failing gates)" always intact and always present when there's anything
// to say. suffix itself is never truncated -- callers only ever pass
// short, fixed-shape suffixes (otherFailingGatesSuffix), never untrusted
// text.
func fitTriageSuffix(s, suffix string) string {
	if suffix == "" {
		return s
	}
	budget := maxTriageSentenceLen - len(suffix)
	if budget < 0 {
		budget = 0
	}
	if len(s) > budget {
		cut := budget
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = s[:cut]
	}
	return s + suffix
}
