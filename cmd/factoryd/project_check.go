package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"buildgate/internal/evidence"
	"buildgate/internal/policy"
	"buildgate/internal/projectconfig"
)

// ProjectCheckResult is one project-level structural check's outcome —
// checkProjectMain's own evidence unit, distinct from run.GateResult
// (which is shaped around a subprocess-based per-ticket check: a command,
// an exit code, a duration, a verify-surface hash). None of that applies
// here: policy.ProductSpecFrozen/TicketStructure/ArchitectureStructure/
// ProgramDesignStructure are pure content checks against a file already
// read into memory, not something that ever runs a command.
//
// Path and SHA256 bind the result to the exact artifact version that was
// actually checked, the same way run.Run.SpecSHA256 binds a run's own
// gate results to the frozen spec content they verified — without them a
// PASS record can't be distinguished from a PASS against a since-edited
// file, and can't be traced back to which of -spec/-contract/-architecture
// /-ticket produced it. SHA256 is empty when the artifact could not be
// read (see the Reasons entry in that case).
type ProjectCheckResult struct {
	Check   string   `json:"check"`
	Path    string   `json:"path"`
	SHA256  string   `json:"sha256,omitempty"`
	Passed  bool     `json:"passed"`
	Reasons []string `json:"reasons,omitempty"`
	// Advisory is true only under -preflight-profile=brownfield's own
	// architecture_structure check: it still runs and its result still
	// appears here, but a false Passed never fails the overall preflight
	// (see evaluateProjectBootstrapChecks). Always false for every other
	// check, and for architecture_structure itself under the default
	// (strict) profile.
	Advisory bool `json:"advisory,omitempty"`
}

// preflightProfileBrownfield is -preflight-profile's one accepted non-empty
// value -- see that flag's own doc comment.
const preflightProfileBrownfield = "brownfield"

// effectivePreflightProfile resolves the preflight profile a real run
// against workspace would actually use: explicit wins when non-empty
// (still validated here, so a caller gets the same rejection a real run
// would), otherwise cfg's own preflight_profile supplies the default
// (nil cfg, or an unset field, means no default -- strict). Shared by
// applyProjectConfigDefaults (the real run path) and apiProjectChecker
// (the POST /projects/check preview) so the preview never reports a
// verdict against a stricter profile than the run it's previewing would
// actually use.
func effectivePreflightProfile(explicit string, cfg *projectconfig.Config) (string, error) {
	if explicit != "" {
		if explicit != preflightProfileBrownfield {
			return "", fmt.Errorf("preflight_profile must be \"\" or %q, got %q", preflightProfileBrownfield, explicit)
		}
		return explicit, nil
	}
	if cfg != nil {
		return cfg.PreflightProfile, nil
	}
	return "", nil
}

// ProjectCheckRecord is checkProjectMain's own durable evidence record —
// the plan's Phase 3 "every prior mandatory-approval point now has a
// deterministic automated check with logged pass/fail evidence" applied
// to the project-bootstrap artifacts (spec/contract/architecture/ticket
// structure) themselves, the same way run.Run is the durable record for
// a single ticket's own build/verify gates.
type ProjectCheckRecord struct {
	Project   string               `json:"project"`
	CheckedAt string               `json:"checked_at"`
	Results   []ProjectCheckResult `json:"results"`
	Passed    bool                 `json:"passed"`
}

// splitTrimmedCSV splits a comma-separated flag value into its trimmed,
// non-empty entries, returning nil for an empty/all-whitespace input --
// unlike a bare strings.Split(s, ","), which turns "" into []string{""} (one
// spurious empty entry) rather than an empty/nil slice a caller can treat
// as "unset, use the default" (see -architecture-required-sections and
// policy.ArchitectureStructure's own nil-means-default convention). Trims
// each entry, not just the whole string: a heading name is matched
// verbatim against a real Markdown heading, so a stray space after a comma
// in an operator-typed list ("Repo layout, Verification") would otherwise
// silently fail to match " Verification" against the real "Verification"
// heading.
func splitTrimmedCSV(csv string) []string {
	if strings.TrimSpace(csv) == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(csv, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// repoRootForWorkspace resolves the real project root for a given
// -workspace value, supporting both conventions this codebase has shipped
// for locating a project's root-anchored artifacts (the mandatory
// project-bootstrap spec/spec.md, spec/contract.md, ARCHITECTURE.md, and
// pi-harness-native tickets under spec/tickets/): the historical
// placeholder-subdirectory convention (root = -workspace's own parent,
// matching a -workspace value like <repo>/workspace) and the newer
// direct-root convention `factoryd onboard -root <repo>` produces (root =
// -workspace itself, when -workspace already IS the repo root with no
// separate placeholder).
//
// Prefers the historical convention whenever its own spec/spec.md already
// exists there, so every existing placeholder-subdirectory setup is
// completely unaffected; prefers workspaceAbs itself whenever that
// location has its own spec/spec.md instead (found via Codex review of PR
// #142, rounds 1 and 2: projectBootstrapArtifactPaths and
// resolvePiTicketPath each independently hand-coded the same
// filepath.Dir(workspaceAbs) assumption, so onboarding via -root and then
// running with -workspace <repo> found neither the bootstrap artifacts
// nor a native ticket under the new layout -- the exact silent-failure-mode
// this whole plan exists to close). Both callers share this one resolution
// instead of two/three hand-synced copies of the same os.Stat probe.
//
// When NEITHER location has spec.md yet -- a repo that has never been
// onboarded at all, under either convention -- falls back to
// workspaceContainsRealContent: workspaceAbs itself when it already holds
// real files (the direct -root convention pointed straight at an
// already-populated repo, just not onboarded yet), historicalRoot only
// when workspaceAbs is empty or doesn't exist yet (the one shape the
// historical placeholder-subdirectory convention ever produces --
// onboardMain's own doc comment: "-workspace is meant to stay an empty,
// ignored directory for its entire life"). Found live (2026-09-16): a
// bare `factoryd -workspace ~/code/todo-service ...` against a real,
// non-empty, never-onboarded repo used to resolve historicalRoot
// (~/code, workspaceAbs's parent) instead, so the project-bootstrap
// preflight silently checked spec.md/contract.md/ARCHITECTURE.md one
// directory up from the actual repo -- exactly the USAGE.md appendix's
// ("what quickstart does under the hood") "-workspace can point straight
// at the repo root itself" case this function's own doc comment already
// claimed to support, but didn't for a genuinely fresh repo.
func repoRootForWorkspace(workspaceAbs string) string {
	historicalRoot := filepath.Dir(workspaceAbs)
	if _, err := os.Stat(filepath.Join(historicalRoot, "spec", "spec.md")); err == nil {
		return historicalRoot
	}
	if _, err := os.Stat(filepath.Join(workspaceAbs, "spec", "spec.md")); err == nil {
		return workspaceAbs
	}
	if workspaceContainsRealContent(workspaceAbs) {
		return workspaceAbs
	}
	return historicalRoot
}

// workspaceContainsRealContent reports whether workspaceAbs already holds
// at least one entry -- see repoRootForWorkspace's own doc comment for why
// that's the one reliable signal distinguishing an already-populated repo
// checkout (the direct -root convention) from the historical placeholder
// convention's empty subdirectory, in the one case (neither convention's
// spec.md exists yet) where nothing else tells the two apart. A workspace
// that doesn't exist yet, or can't be read, is treated the same as empty
// -- both fall back to the historical convention, matching this
// function's own prior behavior before this check existed.
func workspaceContainsRealContent(workspaceAbs string) bool {
	entries, err := os.ReadDir(workspaceAbs)
	if err != nil {
		return false
	}
	return len(entries) > 0
}

// projectBootstrapArtifactPaths returns the fixed spec/contract/architecture
// paths the mandatory project-bootstrap preflight (runMainWithReady, unless
// -skip-project-check) and factoryd init's own scaffold agree on: spec/spec.md,
// spec/contract.md, and ARCHITECTURE.md at repoRootForWorkspace(workspaceAbs)
// -- matching the one real multi-file project on record
// (test-bed/budget-pilot's spec/ layout is a sibling of workspace/, not
// nested inside it) as well as a direct `onboard -root` layout.
func projectBootstrapArtifactPaths(workspaceAbs string) (specPath, contractPath, architecturePath string) {
	root := repoRootForWorkspace(workspaceAbs)
	return filepath.Join(root, "spec", "spec.md"), filepath.Join(root, "spec", "contract.md"), filepath.Join(root, "ARCHITECTURE.md")
}

// resolvePiTicketPath locates the pi-harness-native ticket corresponding to a
// factoryd run. The internal/ticketspec file passed by -spec is deliberately
// not used here: the two files have different formats and roles. A missing
// or ambiguous native ticket is a fail-closed authoring error; callers for a
// project that has not adopted the convention must use -skip-project-check.
// ticketFile is an explicit escape from discovery for callers whose ticket
// lives elsewhere.
func resolvePiTicketPath(workspace, ticket, ticketFile string) (string, int, error) {
	if ticketFile != "" {
		path, err := filepath.Abs(ticketFile)
		if err != nil {
			return "", 0, fmt.Errorf("resolve -ticket-file: %w", err)
		}
		number, err := piTicketNumber(path)
		if err != nil {
			return "", 0, err
		}
		if _, err := os.Stat(path); err != nil {
			return "", 0, fmt.Errorf("-ticket-file: %w", err)
		}
		return path, number, nil
	}

	// ticket (unlike ticketFile just above, an explicitly file-path
	// escape hatch for a fully trusted CLI caller) is meant to be a bare
	// identifier -- "012" or "012-word-count" -- never a path of its own.
	// Every branch below builds ticketsDir/<something derived from
	// ticket> via filepath.Join, which resolves ".." lexically rather
	// than refusing it: found via adversarial review of the new POST
	// /projects/check endpoint (2026-09-08), a ticket value like
	// "../../../../etc/passwd.md" resolves clean outside ticketsDir
	// entirely (confirmed live: filepath.Join("/a/b/spec/tickets",
	// "../../../../etc/passwd.md") produces "/etc/passwd.md"). The
	// resulting policy.TicketStructure check never echoes file content
	// back to a caller (only fixed, canned reason strings), so this was
	// always a narrow existence/readability oracle rather than a full
	// file-content-disclosure primitive -- but the same gap has been
	// reachable via a real run's own -ticket/api.StartRequest.Ticket all
	// along, just at the cost of actually starting (and quarantining) a
	// real run each probe. The new preview endpoint removes that cost
	// entirely (free, instant, no durable record), which is what made
	// this worth closing now rather than leaving as a pre-existing,
	// already-accepted risk. Rejected the same way internal/api's own
	// validRunID rejects a path-shaped id.
	if ticket == "." || ticket == ".." || strings.ContainsAny(ticket, `/\`) {
		return "", 0, fmt.Errorf("ticket %q must be a bare identifier, not a path", ticket)
	}

	workspaceAbs, err := filepath.Abs(workspace)
	if err != nil {
		return "", 0, fmt.Errorf("resolve workspace for pi-harness ticket discovery: %w", err)
	}
	ticketsDir := filepath.Join(repoRootForWorkspace(workspaceAbs), "spec", "tickets")
	info, err := os.Stat(ticketsDir)
	if os.IsNotExist(err) {
		return filepath.Join(ticketsDir, ticketName(ticket)), ticketNumberFromIdentifier(ticket), nil
	}
	if err != nil {
		return "", 0, fmt.Errorf("inspect pi-harness ticket directory: %w", err)
	}
	if !info.IsDir() {
		return "", 0, fmt.Errorf("pi-harness ticket path %q is not a directory", ticketsDir)
	}

	name := ticketName(ticket)
	candidate := filepath.Join(ticketsDir, name)
	if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
		number, numberErr := piTicketNumber(candidate)
		if numberErr != nil {
			return "", 0, numberErr
		}
		return candidate, number, nil
	} else if err != nil && !os.IsNotExist(err) {
		return "", 0, fmt.Errorf("inspect pi-harness ticket %q: %w", candidate, err)
	}
	// Operators commonly pass the slug without its numeric prefix. Resolve
	// that form only when it identifies one exact NNN-<slug>.md file.
	prefixedMatches, err := filepath.Glob(filepath.Join(ticketsDir, "???-"+ticket+".md"))
	if err != nil {
		return "", 0, fmt.Errorf("discover pi-harness ticket: %w", err)
	}
	if len(prefixedMatches) == 1 {
		number, numberErr := piTicketNumber(prefixedMatches[0])
		if numberErr != nil {
			return "", 0, numberErr
		}
		return prefixedMatches[0], number, nil
	}
	if len(prefixedMatches) > 1 {
		return "", 0, fmt.Errorf("pi-harness ticket %q is ambiguous: found %v", ticket, prefixedMatches)
	}

	// Accepting the bare sequence number is useful for operators and keeps
	// discovery independent of the ticket slug. It must resolve uniquely.
	if len(ticket) == 3 {
		matches, err := filepath.Glob(filepath.Join(ticketsDir, ticket+"-*.md"))
		if err != nil {
			return "", 0, fmt.Errorf("discover pi-harness ticket: %w", err)
		}
		if len(matches) == 1 {
			number, numberErr := piTicketNumber(matches[0])
			if numberErr != nil {
				return "", 0, numberErr
			}
			return matches[0], number, nil
		}
		if len(matches) > 1 {
			return "", 0, fmt.Errorf("pi-harness ticket %q is ambiguous: found %v", ticket, matches)
		}
	}
	return candidate, ticketNumberFromIdentifier(ticket), nil
}

// temporalPreflightTicketPath decides what pi-harness ticket path (if any)
// to forward to the Temporal path's own PreflightActivity, which -- unlike
// evaluateProjectBootstrapChecks/runProjectBootstrapCheck above -- has no
// concept of -preflight-profile at all: it hard-fails whenever it is handed
// a non-empty TicketPath it cannot read, with no advisory case. Found live
// (2026-09-13): resolvePiTicketPath always returns a deterministic
// candidate path when -ticket-file is unset, whether or not that file
// actually exists; under -preflight-profile=brownfield with no adopted
// pi-harness convention, that candidate never exists, and
// evaluateProjectBootstrapChecks' own doc comment already documents this as
// advisory-only for exactly that reason. But piTicketPath was forwarded to
// the Temporal workflow unconditionally, so the identical run -- having
// already passed this process's own local preflight -- unconditionally
// halted the moment it reached PreflightActivity on the Temporal path,
// something the direct-execution path never re-checks a second time at
// all. Passing an empty path here instead, in exactly the same
// brownfield-and-missing case runProjectBootstrapCheck already treats as
// advisory, makes the two paths agree again, while a genuine authoring
// error (an explicit -ticket-file that doesn't exist, or a missing ticket
// under the strict/default profile) still reaches PreflightActivity and
// still fails, matching runProjectBootstrapCheck's own behavior for that
// case.
func temporalPreflightTicketPath(piTicketPath, preflightProfile string) string {
	if piTicketPath == "" || preflightProfile != preflightProfileBrownfield {
		return piTicketPath
	}
	if _, err := os.Stat(piTicketPath); os.IsNotExist(err) {
		return ""
	}
	return piTicketPath
}

func ticketName(ticket string) string {
	if strings.HasSuffix(ticket, ".md") {
		return ticket
	}
	return ticket + ".md"
}

func ticketNumberFromIdentifier(ticket string) int {
	if len(ticket) >= 3 {
		if number, err := strconv.Atoi(ticket[:3]); err == nil && number > 0 {
			return number
		}
	}
	return 1
}

func piTicketNumber(ticketPath string) (int, error) {
	base := filepath.Base(ticketPath)
	if len(base) < 8 || base[3] != '-' || !strings.HasSuffix(base, ".md") {
		return 0, fmt.Errorf("pi-harness ticket %q must be named NNN-*.md", ticketPath)
	}
	number, err := strconv.Atoi(base[:3])
	if err != nil || number <= 0 {
		return 0, fmt.Errorf("pi-harness ticket %q must start with a positive three-digit ticket number", ticketPath)
	}
	return number, nil
}

// errProjectBootstrapCheckFailed distinguishes a project-bootstrap
// preflight failure -- a misconfigured -workspace/-spec pairing, a
// missing spec/contract/architecture artifact, an onboarding convention
// the project hasn't adopted yet -- from a genuine internal/infrastructure
// fault. It is exactly the caller-diagnosable, caller-fixable class
// api.ErrInvalidStartRequest already exists to carry back to an API
// caller with its real message intact (see apiStartStarter's own error
// handling): before this sentinel existed, apiStartStarter had no way to
// tell this failure apart from any other runMainWithReady error, so
// POST /runs collapsed it to a generic 500 "start run" with the actual,
// actionable diagnostic (which artifact, which path, why) discarded --
// found via a real console live-validation run, 2026-09-08, where the
// console's own error display had nothing more specific to show an
// operator than "Run API request failed (500)".
var errProjectBootstrapCheckFailed = errors.New("project-bootstrap preflight failed")

// runProjectBootstrapCheck runs the same project-bootstrap structural checks
// checkProjectMain runs for -spec/-contract/-architecture, against the
// fixed paths projectBootstrapArtifactPaths derives from a run's
// -workspace, plus the optional pi-harness-native ticket check, and persists
// a durable ProjectCheckRecord the same way (saveProjectCheckRecord, normally
// under <dataDir>/project-checks/).
// Returns a non-nil error naming every failing or unreadable artifact if
// any check fails — runMainWithReady treats that as fail-closed, exactly
// like a declared policy.Check gate failing, and before this run has
// touched anything else.
//
// dataDirInsideWorkspace is true only when -data-dir itself resolves
// inside the shared checkout — meaning dataDir names the shared checkout
// itself, not an independent records directory. In that case a record
// for a failing check is redirected to the OS temp directory instead of
// dataDir, so a rejected run leaves the checkout untouched exactly as
// promised; a record for a passing check still goes to dataDir as
// normal, since a run that's about to proceed against that same checkout
// has no mutation-before-rejection concern left to avoid.
// resolvedWorkspace (only meaningful when dataDirInsideWorkspace) is the
// same canonicalized -workspace the caller already computed for its own
// containment check, reused here to confirm the redirect actually lands
// outside it — os.MkdirTemp honors $TMPDIR, which a misconfigured
// environment could itself point inside -workspace, silently defeating
// the whole point of redirecting (found via a GitHub Codex App review
// round on an earlier version of this fix, 2026-08-30).
// specSHA256/contractSHA256 return the hashed content of specPath/
// contractPath (product_spec_frozen/program_design_structure's own
// artifacts, in that fixed checks-slice order below) on a successful,
// all-checks-passed return — never populated alongside a non-nil error,
// since every caller already treats any error as run-aborting and has no
// use for a partial result. Feeds run.Run.ProductSpecSHA256/ContractSHA256
// (see their own doc comment) so a later chained run can detect gap 5's
// "spec drift across a long build" against this run's recorded values.
//
// evaluateProjectBootstrapChecks is the pure, no-persistence, no-error-on-
// failure core: read each declared artifact, run its policy.* check
// against the content, and report every result regardless of pass/fail --
// extracted so a caller that wants the verdict without runProjectBootstrapCheck's
// own side effects (a durable ProjectCheckRecord, a run-aborting error on
// failure) has a real function to call instead of reimplementing this loop.
// projectCheckAPIDryRun (below) is exactly that caller: the console's
// "check project setup" preview must never write a record or fail closed
// the way a real run start does -- it is asking a question, not making a
// decision.
//
// profile is "" (strict, the default) or preflightProfileBrownfield: under
// the brownfield profile, product_spec_frozen and program_design_structure
// are omitted from checks entirely -- not run, not read, not reported --
// since a repo new to this convention has neither a goal_pilot.py-shaped
// spec.md nor contract.md to check in the first place, and
// architecture_structure runs but is marked ProjectCheckResult.Advisory:
// its own result is still reported so an operator can see what's missing,
// but a false Passed there never flips allPassed. ticket_structure is
// checked the same way under both profiles whenever the ticket file
// exists -- a ticket is still a ticket -- but under brownfield an
// auto-discovered ticket path that does not exist is advisory too: a repo
// new to this convention has no spec/tickets/ directory next to it any
// more than it has spec.md or contract.md, and `factoryd submit`'s own
// queue entries (whose request lives in the queue's spec.md, not in a
// pi-harness NNN-<slug>.md) never will. Found live 2026-09-10: the first
// submit-then-worker against a real repo under ~/code failed here
// looking for ~/code/spec/tickets/<id>.md, so no submit-queued run could
// ever pass the preflight it is documented to run. An explicitly passed
// -ticket-file that is missing never reaches here -- resolvePiTicketPath
// rejects it first -- so this advisory case is only ever the discovery
// convention's own hypothetical path.
//
// requestTicket (runMainWithReady's own -request-ticket, forwarded from
// QueueEntry.RequestTicket) means ticketPath is a request-pipeline
// ticketspec-format ticket (-spec itself), not a repo-native pi-harness
// one: when true, ticket_structure is checked with
// policy.TicketStructureBrownfield instead of policy.TicketStructure,
// under BOTH profiles, and is never advisory -- ticketPath is the run's
// own -spec snapshot, which always exists by this point, so there is no
// "convention not adopted yet" case to exempt the way the auto-discovered
// native-ticket path above has. Found live 2026-09-25: the 2026-09-10 fix
// above only covered a submit-then-worker's own -preflight-profile
// (brownfield) case -- a strict-profile repo (real spec/spec.md,
// spec/contract.md, ARCHITECTURE.md, no spec/tickets/) submitted through
// the request pipeline still resolved a nonexistent
// <repo>/spec/tickets/<request-id>-NNN.md and hard-failed ticket_structure
// non-advisory, since strict never marks the ticket check advisory at
// all -- strict and the request pipeline could never both pass.
func evaluateProjectBootstrapChecks(specPath, contractPath, architecturePath, ticketPath string, ticketNumber int, architectureRequiredSections []string, profile string, requestTicket bool) ([]ProjectCheckResult, bool) {
	type namedCheck struct {
		name     string
		path     string
		checker  func(string) (bool, []string)
		advisory bool
	}
	var checks []namedCheck
	if profile != preflightProfileBrownfield {
		checks = append(checks,
			namedCheck{"product_spec_frozen", specPath, func(content string) (bool, []string) {
				passed, reason := policy.ProductSpecFrozen(content)
				if reason == "" {
					return passed, nil
				}
				return passed, []string{reason}
			}, false},
			namedCheck{"program_design_structure", contractPath, policy.ProgramDesignStructure, false},
		)
	}
	checks = append(checks, namedCheck{"architecture_structure", architecturePath, func(content string) (bool, []string) {
		return policy.ArchitectureStructure(content, architectureRequiredSections)
	}, profile == preflightProfileBrownfield})
	if ticketPath != "" {
		if requestTicket {
			// See this function's own doc comment: ticketPath is -spec
			// itself here, in the request pipeline's ticketspec format, so
			// it is checked with the brownfield ticket checker under BOTH
			// profiles, never advisory -- unlike the native-ticket path
			// below, there is no "convention not adopted yet" case for a
			// request's own spec snapshot, which always exists.
			checks = append(checks, namedCheck{"ticket_structure", ticketPath, func(content string) (bool, []string) {
				return policy.TicketStructureBrownfield(content)
			}, false})
		} else {
			ticketAdvisory := false
			if profile == preflightProfileBrownfield {
				if _, err := os.Stat(ticketPath); os.IsNotExist(err) {
					ticketAdvisory = true
				}
			}
			checks = append(checks, namedCheck{"ticket_structure", ticketPath, func(content string) (bool, []string) {
				return policy.TicketStructure(content, ticketNumber)
			}, ticketAdvisory})
		}
	}

	var results []ProjectCheckResult
	allPassed := true
	for _, c := range checks {
		b, err := os.ReadFile(c.path)
		if err != nil {
			results = append(results, ProjectCheckResult{Check: c.name, Path: c.path, Passed: false, Reasons: []string{fmt.Sprintf("could not read artifact: %s", err)}, Advisory: c.advisory})
			if !c.advisory {
				allPassed = false
			}
			continue
		}
		passed, reasons := c.checker(string(b))
		results = append(results, ProjectCheckResult{Check: c.name, Path: c.path, SHA256: evidence.SHA256Bytes(b), Passed: passed, Reasons: reasons, Advisory: c.advisory})
		if !passed && !c.advisory {
			allPassed = false
		}
	}
	return results, allPassed
}

// onboardingNotDone reports whether none of the project-bootstrap artifacts
// exist on disk yet -- the "this repo has never been pointed at
// factoryd onboard/init at all" case, distinct from a repo that adopted the
// convention but has a genuinely invalid spec/contract/architecture (a typo
// in a required heading, an unfrozen spec, etc.). Only that first case gets
// runProjectBootstrapCheck's onboarding-command hint below: printing "run
// onboard" at someone who already has real spec/contract/architecture docs
// with one structural mistake would send them to the wrong fix.
func onboardingNotDone(specPath, contractPath, architecturePath string) bool {
	for _, path := range []string{specPath, contractPath, architecturePath} {
		if _, err := os.Stat(path); err == nil {
			return false
		}
	}
	return true
}

// repoRootHint is the real repository root to suggest as `factoryd onboard`'s
// own -root, used only for the onboardingNotDone message below -- callers
// should pass release.RepositoryRoot(-workspace) (the actual git toplevel),
// not something derived from projectBootstrapArtifactPaths' own
// repoRootForWorkspace heuristic: that heuristic falls back to
// filepath.Dir(workspace) whenever neither convention's spec.md exists yet
// (its documented default for a genuinely ambiguous, not-yet-onboarded
// workspace), which is exactly the one case this hint fires in -- deriving
// the suggested -root from architecturePath's own directory here would
// silently point the printed command at workspace's *parent* instead of
// workspace itself whenever -workspace already IS the repo root and just
// hasn't been onboarded yet.
func runProjectBootstrapCheck(dataDir string, dataDirInsideWorkspace bool, resolvedWorkspace string, repoRootHint string, project, specPath, contractPath, architecturePath, ticketPath string, ticketNumber int, architectureRequiredSections []string, preflightProfile string, requestTicket bool) (string, string, error) {
	results, allPassed := evaluateProjectBootstrapChecks(specPath, contractPath, architecturePath, ticketPath, ticketNumber, architectureRequiredSections, preflightProfile, requestTicket)

	record := ProjectCheckRecord{Project: project, CheckedAt: time.Now().Format(time.RFC3339Nano), Results: results, Passed: allPassed}
	recordDataDir := dataDir
	if !allPassed && dataDirInsideWorkspace {
		tmpDir, err := os.MkdirTemp("", "factoryd-project-check-")
		if err != nil {
			return "", "", fmt.Errorf("create out-of-checkout dir for rejected project-bootstrap check record: %w", err)
		}
		// Confirm the redirect actually escaped -workspace rather than
		// assuming it: $TMPDIR (os.MkdirTemp's base when given "") can
		// itself be set to somewhere inside -workspace, or symlink
		// there, which would silently defeat this whole redirect. A
		// violation here removes the just-created directory (it's
		// inside the checkout, so leaving it behind is exactly the
		// mutation being avoided) and fails closed with a diagnosis
		// instead of persisting anywhere.
		resolvedTmpDir, err := filepath.EvalSymlinks(tmpDir)
		if err != nil {
			return "", "", fmt.Errorf("resolve out-of-checkout dir for rejected project-bootstrap check record: %w", err)
		}
		if resolvedTmpDir == resolvedWorkspace || strings.HasPrefix(resolvedTmpDir, resolvedWorkspace+string(filepath.Separator)) {
			os.RemoveAll(tmpDir)
			return "", "", fmt.Errorf("cannot persist rejected project-bootstrap check record outside -workspace %q: the temp directory %q (from $TMPDIR) itself resolves inside it -- set $TMPDIR to a directory outside the checkout", resolvedWorkspace, resolvedTmpDir)
		}
		recordDataDir = tmpDir
	}
	recordPath, err := saveProjectCheckRecord(recordDataDir, record)
	if err != nil {
		return "", "", fmt.Errorf("persist project-bootstrap check record: %w", err)
	}
	if !allPassed {
		var failures []string
		for _, r := range results {
			// Advisory failures (architecture_structure under
			// -preflight-profile=brownfield) never make allPassed false on
			// their own, so listing one here would misname it as a reason
			// this run was rejected when a different, non-advisory check is
			// the actual cause.
			if !r.Passed && !r.Advisory {
				failures = append(failures, fmt.Sprintf("%s (%s): %s", r.Check, r.Path, strings.Join(r.Reasons, "; ")))
			}
		}
		// A repo that has never run onboard/init at all (none of the three
		// artifacts exist) gets the exact next command to run instead of the
		// generic "-skip-project-check bypasses this" pointer -- the USAGE.md
		// appendix's decision ("does your repo already have a real
		// ARCHITECTURE.md?") made concrete instead of left for the operator
		// to work out by hand.
		// -root uses repoRootHint (the caller's own real repo root), not
		// something derived from architecturePath -- see repoRootHint's own
		// doc comment on this function's signature for why.
		//
		// Strict profile only: under brownfield, "none of the three exist"
		// is the documented, legitimate layout (the profile never checks
		// spec/contract and treats architecture as advisory), so the only
		// non-advisory failure possible there is a malformed ticket -- and
		// this hint would both misdiagnose that as "not onboarded" and drop
		// the actual ticket_structure reason from the message.
		if preflightProfile != preflightProfileBrownfield && onboardingNotDone(specPath, contractPath, architecturePath) {
			return "", "", fmt.Errorf("%w, run not started (record: %s): this repo has not been onboarded yet -- none of %s, %s, %s exist. Run:\n  factoryd onboard -project %s -root %s -write-factory-yml\nwhich scaffolds exactly those three (spec/ created if missing) plus .factory.yml, pre-filling a real detected verify command; review the placeholder docs, freeze spec/spec.md's STATUS line, then re-run. (Already have your own ARCHITECTURE.md elsewhere, or don't want factoryd's tracked convention? Pass -skip-project-check instead -- see USAGE.md's onboarding appendix.)", errProjectBootstrapCheckFailed, recordPath, specPath, contractPath, architecturePath, project, repoRootHint)
		}
		return "", "", fmt.Errorf("%w, run not started (record: %s; -skip-project-check bypasses this for a project that hasn't adopted the spec/contract/architecture convention -- see `factoryd init`): %s", errProjectBootstrapCheckFailed, recordPath, strings.Join(failures, " | "))
	}
	// Looked up by check name, not positional index into checks/results
	// (they do happen to align 1:1 today) — resilient to that slice's own
	// order ever changing without this lookup silently reading the wrong
	// artifact's hash.
	var specHash, contractHash string
	for _, r := range results {
		switch r.Check {
		case "product_spec_frozen":
			specHash = r.SHA256
		case "program_design_structure":
			contractHash = r.SHA256
		}
	}
	return specHash, contractHash, nil
}

// checkProjectMain runs whichever of the four project-bootstrap structural
// checks the caller declares a file for (-spec/-contract/-architecture/
// -ticket — each optional, at least one required), prints a pass/fail
// summary with reasons, persists a durable ProjectCheckRecord, and returns
// a non-nil error (nonzero exit) if any declared check failed — fail
// closed, matching every other gate in this codebase.
//
// This is the invocation point policy.ProductSpecFrozen/TicketStructure/
// ArchitectureStructure/ProgramDesignStructure were written for but never
// had (see each function's own "factoryd does not invoke it yet" doc
// comment) — CLAIMS.md's own "a few approvals are not yet expressed that
// way" gap. Deliberately scoped to a synchronous CLI check, not a new
// Temporal/quarantine/override state machine: these are pure,
// fast, file-content checks with no subprocess or agent round-trip
// involved, unlike a ticket's own build/verify gate — there is nothing
// here that benefits from Activity retry/timeout handling, and inventing
// a parallel accept/quarantine/override lifecycle for four content checks
// would be a solution in search of a problem. An operator who disagrees
// with a failed check re-runs this command after fixing the artifact,
// the same way they would fix a failing `make verify` — there is no
// override to record because there is no build step this could ever
// force through unverified.
// newCheckProjectFlags builds `factoryd check-project`'s FlagSet in
// isolation from parsing, so USAGE.md's doc-vs-flag drift test
// (TestUSAGEDocFlagsExistOnSubcommand) can enumerate its real flags without
// executing the command.
func newCheckProjectFlags() (flags *flag.FlagSet, specPath, contractPath, architecturePath, architectureRequiredSections, ticketPath *string, ticketNumber *int, makefilePath, dataDir, project, preflightProfile *string) {
	flags = flag.NewFlagSet("check-project", flag.ContinueOnError)
	specPath = flags.String("spec", "", "path to the project's spec.md; checked for a frozen STATUS line if set")
	contractPath = flags.String("contract", "", "path to the contract/program-design artifact; checked for required sections (Conventions, at least one Endpoint) if set")
	architecturePath = flags.String("architecture", "", "path to the ARCHITECTURE.md-shaped artifact; checked for required sections (Repo layout, Verification, Known deviations, or -architecture-required-sections's own names) if set")
	architectureRequiredSections = flags.String("architecture-required-sections", "", "comma-separated section headings to require in -architecture, in order, instead of the default Repo layout/Verification/Known deviations -- see the same flag on factoryd's run command for why")
	ticketPath = flags.String("ticket", "", "path to a whole-app-build ticket file in the pi-harness-native format (## Goal/## Required changes/## Verification/## Commit headings, e.g. spec/tickets/NNN-*.md) -- NOT a factoryd -spec ticket in internal/ticketspec's own Verify-Command:/Allowed-Files: format (e.g. data/tickets/*.spec.md), which this check does not validate. Checked for required headings/content if set.")
	ticketNumber = flags.Int("ticket-number", 0, "the ticket's own sequence number, required whenever -ticket is set -- only ticket 1 is exempt from the 'This is an existing repo.' context-line requirement TicketStructure checks for every later ticket, so an omitted number must never silently default to 1")
	makefilePath = flags.String("makefile", "", "path to the project's Makefile (or other file declaring its canonical verify command); if set, every spec/acceptance/<NNN> slice found next to it (i.e. under filepath.Dir(-makefile)/spec/acceptance) must be referenced somewhere in its content -- a drafted acceptance slice that was never staged into verify catches nothing (see policy.AcceptanceSuiteWired)")
	dataDir = flags.String("data-dir", "data", "directory for durable project-check records")
	project = flags.String("project", "", "project identifier for the durable record filename (required)")
	preflightProfile = flags.String("preflight-profile", "", "\"\" (strict, the default) or \"brownfield\": mirrors factoryd <run>'s own -preflight-profile -- under brownfield, -spec/-contract are skipped entirely (not run, not read, not reported) and -architecture's own result is reported but marked advisory, never failing the overall check on its own, and so is a ticket_structure check whose auto-discovered spec/tickets/<ticket>.md does not exist (the shape of every `factoryd submit` entry)")
	plainFlagUsage(flags)
	return
}

func checkProjectMain(args []string) error {
	flags, specPath, contractPath, architecturePath, architectureRequiredSections, ticketPath, ticketNumber, makefilePath, dataDir, project, preflightProfile := newCheckProjectFlags()
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *specPath == "" && *contractPath == "" && *architecturePath == "" && *ticketPath == "" && *makefilePath == "" {
		flags.Usage()
		return fmt.Errorf("at least one of -spec, -contract, -architecture, -ticket, -makefile is required")
	}
	if *project == "" {
		flags.Usage()
		return fmt.Errorf("-project is required")
	}
	if *preflightProfile != "" && *preflightProfile != preflightProfileBrownfield {
		flags.Usage()
		return fmt.Errorf("-preflight-profile must be \"\" or %q, got %q", preflightProfileBrownfield, *preflightProfile)
	}
	if *ticketPath != "" && *ticketNumber <= 0 {
		flags.Usage()
		return fmt.Errorf("-ticket-number is required and must be positive when -ticket is set (a default of 1 would silently exempt later tickets from the 'existing repo' check)")
	}
	// Same traversal guard -ticket already gets in runMainWithReady:
	// *project becomes part of a path saveProjectCheckRecord builds below.
	if *project == "." || *project == ".." || strings.ContainsAny(*project, `/\`) {
		return fmt.Errorf("-project must be a single path component, not %q", *project)
	}

	var results []ProjectCheckResult
	addChecked := func(check, path, sha string, passed bool, reasons []string, advisory bool) {
		results = append(results, ProjectCheckResult{Check: check, Path: path, SHA256: sha, Passed: passed, Reasons: reasons, Advisory: advisory})
	}
	// readArtifact reads path and hashes it so the caller can bind the
	// check result to the exact content checked (Path+SHA256). On failure
	// it does NOT return early out of checkProjectMain the way earlier
	// versions of this command did -- it records the unreadable artifact
	// itself as a failed check (no SHA256) and reports ok=false, so the
	// durable evidence trail always has an entry for every artifact the
	// caller declared, even one that could not be opened. That keeps "this
	// check failed to read its artifact" distinguishable from "check-project
	// never ran at all", which a bare early-return error made impossible
	// to tell apart from the outside.
	readArtifact := func(check, path string, advisory bool) (content, sha string, ok bool) {
		b, err := os.ReadFile(path)
		if err != nil {
			addChecked(check, path, "", false, []string{fmt.Sprintf("could not read artifact: %s", err)}, advisory)
			return "", "", false
		}
		// Hash the bytes already in hand rather than re-opening path: a
		// second, independent read races a concurrent modification
		// between the two reads, which would silently persist a SHA256
		// for different content than what was actually policy-checked --
		// the exact thing this field exists to prevent.
		return string(b), evidence.SHA256Bytes(b), true
	}

	brownfield := *preflightProfile == preflightProfileBrownfield
	if *specPath != "" && !brownfield {
		if content, sha, ok := readArtifact("product_spec_frozen", *specPath, false); ok {
			passed, reason := policy.ProductSpecFrozen(content)
			var reasons []string
			if reason != "" {
				reasons = []string{reason}
			}
			addChecked("product_spec_frozen", *specPath, sha, passed, reasons, false)
		}
	}
	if *contractPath != "" && !brownfield {
		if content, sha, ok := readArtifact("program_design_structure", *contractPath, false); ok {
			passed, reasons := policy.ProgramDesignStructure(content)
			addChecked("program_design_structure", *contractPath, sha, passed, reasons, false)
		}
	}
	if *architecturePath != "" {
		if content, sha, ok := readArtifact("architecture_structure", *architecturePath, brownfield); ok {
			passed, reasons := policy.ArchitectureStructure(content, splitTrimmedCSV(*architectureRequiredSections))
			addChecked("architecture_structure", *architecturePath, sha, passed, reasons, brownfield)
		}
	}
	if *ticketPath != "" {
		if content, sha, ok := readArtifact("ticket_structure", *ticketPath, false); ok {
			passed, reasons := policy.TicketStructure(content, *ticketNumber)
			addChecked("ticket_structure", *ticketPath, sha, passed, reasons, false)
		}
	}
	if *makefilePath != "" {
		if content, sha, ok := readArtifact("acceptance_suite_wired", *makefilePath, false); ok {
			acceptanceRoot := filepath.Join(filepath.Dir(*makefilePath), "spec", "acceptance")
			var sliceDirs []string
			entries, err := os.ReadDir(acceptanceRoot)
			switch {
			case err == nil:
				for _, entry := range entries {
					if entry.IsDir() {
						sliceDirs = append(sliceDirs, filepath.Join("spec", "acceptance", entry.Name()))
					}
				}
				passed, reasons := policy.AcceptanceSuiteWired(content, sliceDirs)
				addChecked("acceptance_suite_wired", *makefilePath, sha, passed, reasons, false)
			case os.IsNotExist(err):
				// A missing spec/acceptance directory is not an error here
				// -- it means no acceptance suite has been drafted yet at
				// all (e.g. checked before /contract-plan ever ran), which
				// AcceptanceSuiteWired already treats as passing (nothing
				// to wire in), the same as an explicit empty sliceDirs.
				passed, reasons := policy.AcceptanceSuiteWired(content, sliceDirs)
				addChecked("acceptance_suite_wired", *makefilePath, sha, passed, reasons, false)
			default:
				// Any other failure (permissions, an I/O error) is not the
				// same claim as "no suite drafted yet" -- treating it that
				// way recorded a false acceptance_suite_wired: PASS with no
				// slices, never actually inspecting whatever suite really
				// exists there (found via review, Codex, PR #59).
				addChecked("acceptance_suite_wired", *makefilePath, sha, false, []string{fmt.Sprintf("could not read %s: %s", acceptanceRoot, err)}, false)
			}
		}
	}

	allPassed := true
	for _, r := range results {
		status := "PASS"
		if !r.Passed {
			status = "FAIL"
			// Advisory failures (architecture_structure under
			// -preflight-profile=brownfield) are still reported but never
			// fail the overall check on their own -- mirrors
			// evaluateProjectBootstrapChecks's identical rule for the
			// factoryd <run> and API preflight paths.
			if !r.Advisory {
				allPassed = false
			}
		}
		if r.Advisory {
			status += " (advisory)"
		}
		fmt.Printf("%s: %s\n", r.Check, status)
		for _, reason := range r.Reasons {
			fmt.Printf("  - %s\n", reason)
		}
	}

	record := ProjectCheckRecord{
		Project:   *project,
		CheckedAt: time.Now().Format(time.RFC3339Nano),
		Results:   results,
		Passed:    allPassed,
	}
	recordPath, err := saveProjectCheckRecord(*dataDir, record)
	if err != nil {
		return fmt.Errorf("persist project-check record: %w", err)
	}
	fmt.Printf("record: %s\n", recordPath)

	if !allPassed {
		return fmt.Errorf("project checks failed")
	}
	return nil
}

// saveProjectCheckRecord persists record under
// <dataDir>/project-checks/<project>-<timestamp>.json, following the same
// write-temp-then-rename discipline run.Run.Save uses for its own durable
// records, and returns the path written. RFC3339Nano (not RFC3339): two
// checks against the same project can otherwise collide on the same
// whole-second filename — see run.Run.CreatedAt's own doc comment for the
// identical reasoning. The temp file has its own unique name: two builds of
// one repository can start in the same clock tick, and a shared temp name
// made the second rename fail.
func saveProjectCheckRecord(dataDir string, record ProjectCheckRecord) (string, error) {
	dir := filepath.Join(dataDir, "project-checks")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", fmt.Errorf("create project-checks dir: %w", err)
	}
	filename := fmt.Sprintf("%s-%s.json", record.Project, strings.ReplaceAll(record.CheckedAt, ":", ""))
	path := filepath.Join(dir, filename)
	b, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal project-check record: %w", err)
	}
	tmpFile, err := os.CreateTemp(dir, filename+".*.tmp")
	if err != nil {
		return "", fmt.Errorf("write project-check record: %w", err)
	}
	tmp := tmpFile.Name()
	_, err = tmpFile.Write(b)
	if closeErr := tmpFile.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("write project-check record: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("rename project-check record: %w", err)
	}
	return path, nil
}
