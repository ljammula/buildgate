package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"buildgate/internal/evidence"
	"buildgate/internal/policy"
	"buildgate/internal/request"
)

// ProjectCheckResult is one project-level structural check's outcome,
// distinct from run.GateResult (which is shaped around a subprocess-based
// per-ticket check: a command, an exit code, a duration, a verify-surface
// hash). None of that applies here: policy.TicketStructure and
// policy.ArchitectureStructure are pure content checks against a file
// already read into memory, not something that ever runs a command.
//
// Path and SHA256 bind the result to the exact artifact version that was
// actually checked, the same way run.Run.SpecSHA256 binds a run's own
// gate results to the frozen spec content they verified — without them a
// PASS record can't be distinguished from a PASS against a since-edited
// file, and can't be traced back to which artifact produced it. SHA256 is empty when the artifact could not be
// read (see the Reasons entry in that case).
type ProjectCheckResult struct {
	Check   string   `json:"check"`
	Path    string   `json:"path"`
	SHA256  string   `json:"sha256,omitempty"`
	Passed  bool     `json:"passed"`
	Reasons []string `json:"reasons,omitempty"`
	// Advisory is true for architecture_structure, and for an
	// auto-discovered ticket path that does not exist: the check still runs
	// and its result still appears here, but a false Passed never fails the
	// overall preflight (see evaluateProjectBootstrapChecks).
	Advisory bool `json:"advisory,omitempty"`
}

// ProjectCheckRecord is the project-bootstrap preflight's durable evidence
// record (architecture and ticket structure), the same way run.Run is the
// durable record for a single ticket's own build/verify gates.
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
// direct-root convention (root =
// -workspace itself, when -workspace already IS the repo root with no
// separate placeholder).
//
// Prefers the historical convention whenever its own spec/spec.md already
// exists there, so every existing placeholder-subdirectory setup is
// completely unaffected; prefers workspaceAbs itself whenever that
// location has its own spec/spec.md instead (found via Codex review of PR
// #142, rounds 1 and 2: the artifact paths and
// resolvePiTicketPath each independently hand-coded the same
// filepath.Dir(workspaceAbs) assumption, so scaffolding at the repo root and
// then running with -workspace <repo> found neither the bootstrap artifacts
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
// historical placeholder-subdirectory convention ever produces). Found live (2026-09-16): a
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

// projectArchitecturePath returns the ARCHITECTURE.md the project-bootstrap
// preflight (runMainWithReady, unless -skip-project-check) checks:
// ARCHITECTURE.md at repoRootForWorkspace(workspaceAbs).
func projectArchitecturePath(workspaceAbs string) string {
	return filepath.Join(repoRootForWorkspace(workspaceAbs), "ARCHITECTURE.md")
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
// to forward to the Temporal path's own PreflightActivity, which hard-fails
// whenever it is handed a non-empty TicketPath it cannot read, with no
// advisory case. resolvePiTicketPath always returns a deterministic
// candidate path when -ticket-file is unset, whether or not that file
// exists; evaluateProjectBootstrapChecks treats a missing candidate as
// advisory, so it is not forwarded either. An explicit -ticket-file that
// does not exist is rejected earlier, by resolvePiTicketPath.
func temporalPreflightTicketPath(piTicketPath string) string {
	if piTicketPath == "" {
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

// evaluateProjectBootstrapChecks is the pure, no-persistence, no-error-on-
// failure core of the project-bootstrap preflight: read each artifact, run
// its policy check against the content, and report every result regardless
// of pass/fail. runProjectBootstrapCheck adds the durable record and the
// run-aborting error; apiProjectChecker (the console's "check project setup"
// preview) calls this directly, since a preview must never write a record
// or fail closed.
//
// architecture_structure runs but is advisory: its result is reported so an
// operator can see what is missing, but a false Passed never flips
// allPassed. ticket_structure is checked whenever the ticket file exists;
// an auto-discovered ticket path that does not exist is advisory too, since
// most repositories have no spec/tickets/ directory and a request's own
// tickets never live there. An explicitly passed -ticket-file that is
// missing never reaches here -- resolvePiTicketPath rejects it first.
//
// requestTicketCheck, set for runMainWithReady's own -request-ticket
// (requestTicketCheck on ticketRun picks it), means ticketPath is a
// request-pipeline ticketspec-format ticket (-spec itself), not a
// repo-native pi-harness one: ticket_structure is then checked with it
// instead of policy.TicketStructure, and is never advisory -- ticketPath is
// the run's own -spec snapshot, which always exists by this point.
func evaluateProjectBootstrapChecks(architecturePath, ticketPath string, ticketNumber int, architectureRequiredSections []string, requestTicketCheck func(string) (bool, []string)) ([]ProjectCheckResult, bool) {
	type namedCheck struct {
		name     string
		path     string
		checker  func(string) (bool, []string)
		advisory bool
	}
	var checks []namedCheck
	checks = append(checks, namedCheck{"architecture_structure", architecturePath, func(content string) (bool, []string) {
		return policy.ArchitectureStructure(content, architectureRequiredSections)
	}, true})
	if ticketPath != "" {
		if requestTicketCheck != nil {
			// See this function's own doc comment: ticketPath is -spec
			// itself here, in the request pipeline's ticketspec format,
			// never advisory.
			checks = append(checks, namedCheck{"ticket_structure", ticketPath, requestTicketCheck, false})
		} else {
			ticketAdvisory := false
			if _, err := os.Stat(ticketPath); os.IsNotExist(err) {
				ticketAdvisory = true
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

// runProjectBootstrapCheck runs evaluateProjectBootstrapChecks against the
// architecture path projectArchitecturePath derives from a run's
// -workspace, plus the optional ticket check, and persists a durable
// ProjectCheckRecord (saveProjectCheckRecord, normally under
// <dataDir>/project-checks/). Returns a non-nil error naming every failing
// non-advisory artifact if any check fails -- runMainWithReady treats that
// as fail-closed, exactly like a declared policy.Check gate failing, and
// before this run has touched anything else.
//
// dataDirInsideWorkspace is true only when -data-dir itself resolves inside
// the shared checkout. In that case a record for a failing check is
// redirected to the OS temp directory instead of dataDir, so a rejected run
// leaves the checkout untouched; a record for a passing check still goes to
// dataDir. resolvedWorkspace (only meaningful when dataDirInsideWorkspace)
// is the canonicalized -workspace, reused here to confirm the redirect
// actually lands outside it -- os.MkdirTemp honors $TMPDIR, which a
// misconfigured environment could itself point inside -workspace.
func runProjectBootstrapCheck(dataDir string, dataDirInsideWorkspace bool, resolvedWorkspace string, project, architecturePath, ticketPath string, ticketNumber int, architectureRequiredSections []string, requestTicketCheck func(string) (bool, []string)) error {
	results, allPassed := evaluateProjectBootstrapChecks(architecturePath, ticketPath, ticketNumber, architectureRequiredSections, requestTicketCheck)

	record := ProjectCheckRecord{Project: project, CheckedAt: time.Now().Format(time.RFC3339Nano), Results: results, Passed: allPassed}
	recordDataDir := dataDir
	if !allPassed && dataDirInsideWorkspace {
		tmpDir, err := os.MkdirTemp("", "factoryd-project-check-")
		if err != nil {
			return fmt.Errorf("create out-of-checkout dir for rejected project-bootstrap check record: %w", err)
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
			return fmt.Errorf("resolve out-of-checkout dir for rejected project-bootstrap check record: %w", err)
		}
		if resolvedTmpDir == resolvedWorkspace || strings.HasPrefix(resolvedTmpDir, resolvedWorkspace+string(filepath.Separator)) {
			os.RemoveAll(tmpDir)
			return fmt.Errorf("cannot persist rejected project-bootstrap check record outside -workspace %q: the temp directory %q (from $TMPDIR) itself resolves inside it -- set $TMPDIR to a directory outside the checkout", resolvedWorkspace, resolvedTmpDir)
		}
		recordDataDir = tmpDir
	}
	recordPath, err := saveProjectCheckRecord(recordDataDir, record)
	if err != nil {
		return fmt.Errorf("persist project-bootstrap check record: %w", err)
	}
	if !allPassed {
		var failures []string
		for _, r := range results {
			// Advisory failures never make allPassed false on their own,
			// so listing one here would misname it as a reason this run
			// was rejected when a different, non-advisory check is the
			// actual cause.
			if !r.Passed && !r.Advisory {
				failures = append(failures, fmt.Sprintf("%s (%s): %s", r.Check, r.Path, strings.Join(r.Reasons, "; ")))
			}
		}
		return fmt.Errorf("%w, run not started (record: %s; -skip-project-check bypasses this): %s", errProjectBootstrapCheckFailed, recordPath, strings.Join(failures, " | "))
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

// requestTicketCheck is the start-of-build structure check of a request's
// ticket: a ticket with no ticket scope (the verbatim spec ticket, or a
// carved replay's input) in any shape a spec is approved in, any other
// ticket in the ticket shape; nil for a run that is not a request's.
func (tr *ticketRun) requestTicketCheck() func(string) (bool, []string) {
	switch {
	case !*tr.requestTicket:
		return nil
	case *tr.rf.scopeApproval == "none":
		return request.SpecTicketStructure
	default:
		return request.RequestTicketStructure
	}
}
