// Package requestsubmit holds the workspace-validation and
// request-creation core that used to live only inside `factoryd submit`
// (cmd/factoryd/submit.go). It was factored out here so the
// new POST /requests console route (internal/api) and the `factoryd
// submit` CLI call the exact same validation and
// internal/request.Request construction -- neither one is allowed to
// grow a second, hand-synced copy of "what makes a workspace/verify-
// command combination legal to submit," the same anti-duplication
// reasoning cmd/factoryd/validate.go's own dataDirInsideWorkspace doc
// comment already gives for run_ticket.go and doctor.go sharing one
// guard instead of two.
package requestsubmit

import (
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"buildgate/internal/policy"
	"buildgate/internal/projectconfig"
	"buildgate/internal/release"
	"buildgate/internal/request"
	"buildgate/internal/run"
	"buildgate/internal/sessionconfig"
	"buildgate/internal/workspace"
)

// GitToplevel resolves the git repository containing dir via `git -C
// <dir> rev-parse --show-toplevel`, erroring when dir is not inside a
// git working tree. Exported so POST /requests (internal/api) can apply
// the same "is this actually a git root" test `factoryd quickstart`
// already uses (cmd/factoryd/quickstart.go's own quickstartGitToplevel,
// which this mirrors) before ever accepting an HTTP caller's workspace
// path -- release.RepositoryRoot deliberately can't serve this purpose
// on its own, since it falls back to returning a non-repo path
// unchanged rather than erroring (it exists to derive a project id for
// an already-validated workspace, not to validate one).
func GitToplevel(dir string) (string, error) {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", fmt.Errorf("not a git repository: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// AgentsFilePrompt is the prompt every AGENTS.md refusal hands the operator
// for their own coding agent (Claude Code, Copilot, Codex): buildgate does
// not write the file, since nothing in it would have been checked by a
// person.
const AgentsFilePrompt = "Write " + run.RootInstructionFile + " at the root of this repository for coding agents: its setup, test, build and lint commands (run each one and keep only what works), its layout, and the conventions a change must follow. Keep it short and exact."

// RequireAgentsFile refuses a repository whose root AGENTS.md is missing or
// holds only whitespace at HEAD: that file is how every harness learns the
// repository's ways of working, so no request starts without it. It reads git
// objects, never the working tree, so an uncommitted file does not pass, and
// only the exact root name counts (not another case, a symlink or a copy in a
// subdirectory). submit, quickstart, doctor -target-repo and the single-ticket
// run share it; no flag or config key turns it off.
func RequireAgentsFile(workspaceAbs string) error {
	const fix = "have your coding agent write one at the repository root, review it and commit it. Prompt: \"" + AgentsFilePrompt + "\""
	git := func(args ...string) ([]byte, error) {
		return exec.Command("git", append([]string{"-C", workspaceAbs}, args...)...).Output()
	}
	if _, err := git("rev-parse", "--verify", "--quiet", "HEAD^{commit}"); err != nil {
		// git's own first line when it has one (a repository it refuses to
		// read says why there); a checkout with no commit prints nothing.
		reason := ""
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			reason, _, _ = strings.Cut(strings.TrimSpace(string(exit.Stderr)), "\n")
		}
		if reason != "" {
			reason = " (git: " + reason + ")"
		}
		return fmt.Errorf("%s is not a git checkout with a commit at HEAD%s, so it has no committed %s: %s", workspaceAbs, reason, run.RootInstructionFile, fix)
	}
	out, err := git("ls-tree", "--full-tree", "-z", "HEAD", "--", run.RootInstructionFile)
	if err != nil {
		return fmt.Errorf("read %s at HEAD of %s: %w", run.RootInstructionFile, workspaceAbs, err)
	}
	meta, name, _ := strings.Cut(strings.TrimSuffix(string(out), "\x00"), "\t")
	fields := strings.Fields(meta) // mode type object
	if name != run.RootInstructionFile || len(fields) != 3 {
		return fmt.Errorf("%s has no %s committed at its root (read from git at HEAD, not the working tree): %s", workspaceAbs, run.RootInstructionFile, fix)
	}
	if fields[0] != "100644" && fields[0] != "100755" {
		return fmt.Errorf("%s at HEAD of %s is not a regular file: %s", run.RootInstructionFile, workspaceAbs, fix)
	}
	blob, err := git("cat-file", "blob", fields[2])
	if err != nil {
		return fmt.Errorf("read %s at HEAD of %s: %w", run.RootInstructionFile, workspaceAbs, err)
	}
	// A byte-order mark is all some editors save for an empty file.
	if strings.TrimSpace(strings.TrimPrefix(string(blob), "\ufeff")) == "" {
		return fmt.Errorf("%s at HEAD of %s is empty: %s", run.RootInstructionFile, workspaceAbs, fix)
	}
	return nil
}

// Params holds every already-resolved input Submit needs. Unlike
// cmd/factoryd's old submitParams, there is no request-file/-issue/
// trailing-argument resolution here: that is a CLI-only concern
// (cmd/factoryd's resolveSubmitRequestText, which shells out to `gh
// issue view`) that has no equivalent for an HTTP caller, who always
// posts the request text directly. A caller resolves RequestText/Source
// itself and passes the result in.
type Params struct {
	// WorkspaceArg is kept separate from the resolved absolute workspace
	// path purely so error messages can keep quoting exactly what the
	// caller passed, matching `factoryd submit`'s historical wording.
	WorkspaceArg string
	DataDir      string
	RequestText  string
	// IDText, when non-empty, is slugged into the request id instead of
	// RequestText (see request.GenerateID) -- cmd/factoryd's -issue path
	// sets this to the issue's own title so an issue body's words (which
	// can be long, unrelated, or code-shaped) never leak into the id.
	// Empty means "slug RequestText itself", the prior behavior for
	// every other source.
	IDText string
	// ID, when non-empty, is a request id the caller already claimed with
	// request.ClaimID (its directory exists and is empty), used as is: a
	// caller that must write a file keyed by the id before the request
	// exists claims it first.
	ID string
	// Source describes where RequestText came from (SourceText/
	// SourceIssue/SourceFile) and, for SourceIssue, the fully-qualified
	// issue reference the eventual PR closes. The caller builds this
	// (cmd/factoryd's resolveSubmitRequestText already does; the API
	// route always uses SourceText) rather than Submit re-deriving it,
	// since only the caller knows which of its own inputs produced the
	// text.
	Source request.Source

	VerifyCommand         string
	VerifyCommandExplicit bool
	FullSuiteCommand      string
	NoCommitOracles       bool
	DraftOracles          bool
	// ImportedSpec is the content of `factoryd submit -spec-file`: a
	// finished spec the operator hands over. It must pass
	// request.ValidateSpecSkeleton, the check a drafted spec passes, or
	// the submission is refused. Empty: the spec is drafted.
	ImportedSpec string
	// ImportedTickets is `factoryd submit -plan-dir`: the tickets of a
	// finished plan for ImportedSpec, in order. Each must pass
	// request.ValidateTicketPlan and together they must cover every
	// acceptance criterion of the spec; the planning stage applies the
	// rest of a drafted plan's checks. Empty: the plan is drafted.
	ImportedTickets []ImportedTicket

	PreflightProfile         string
	PreflightProfileExplicit bool

	// Harnesses is `factoryd submit -harness role=name` / POST /requests' own
	// "harnesses" body field: a per-request coding-agent CLI pick for "planning"
	// and/or "execution", validated below
	// (sessionconfig.ValidateRequestHarnesses) against Settings' own
	// roles.<role>.allowed_harnesses, exactly like Models.
	Harnesses map[string]string

	// SessionTokenCeiling/SessionCostCeilingMicroUSD are the effective
	// relay ceilings (sessionconfig.Settings.EffectiveRelayCeilings) the
	// caller's own session config resolved -- the same ceilings the
	// eventual run would actually launch under, needed so
	// ApplyProjectConfigDefaults' tighten-only check compares a repo's
	// .factory.yml against the session's REAL ceiling rather than
	// submitDefaultMeterTokenBudget/submitDefaultMeterCostBudget's
	// hardcoded legacy defaults. Zero (the default for a caller that
	// hasn't resolved session config, e.g. a test) means "compare
	// against the legacy defaults," preserving prior behavior.
	SessionTokenCeiling        int64
	SessionCostCeilingMicroUSD int64

	// Models is `factoryd submit -model role=model` / POST /requests' own
	// "models" body field: a per-request model pick for "planning" and/or
	// "execution", validated below (sessionconfig.ValidateRequestModels)
	// against Settings' own roles.<role>.allowed before the request is
	// ever created -- review is never requester-selectable, and any role
	// not in this map keeps the factory's own default entirely. Empty
	// (the common case) records nothing on the request. Settings is the
	// caller's own already-resolved session config (`factoryd submit`'s
	// -config; POST /requests' api.WithSessionRoles) -- Submit itself
	// never loads session config on its own.
	Models   map[string]string
	Settings sessionconfig.Settings
}

// Result is what a caller needs out of a successful Submit.
type Result struct {
	ID string
	// Project/RepositoryRoot are the same values the entry itself is
	// recorded under (release.RepositoryRoot/SinglePathComponent below).
	Project        string
	RepositoryRoot string
}

// canonicalPath is a one-line delegation to workspace.CanonicalPath, the
// same one cmd/factoryd/validate.go's canonicalPath makes (that package is
// main, so it cannot be imported). pathWithin is pure filepath.Rel logic
// with no project-specific policy in it; DataDirInsideWorkspace below is
// its one caller, and cmd/factoryd reaches it through that function.
func canonicalPath(path string) (string, error) {
	return workspace.CanonicalPath(path)
}

func pathWithin(parent, candidate string) bool {
	rel, err := filepath.Rel(filepath.Clean(parent), filepath.Clean(candidate))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// DataDirInsideWorkspace resolves workspace and dataDir with
// canonicalPath and reports whether dataDir's resolved path falls inside
// workspace's -- the one fail-closed check that must keep durable run
// records outside whatever the worker mounts, since Docker containment
// is unconditional. Exported copy of cmd/factoryd's own
// dataDirInsideWorkspace (validate.go), which now delegates to this
// function instead of keeping its own body, so run_ticket.go/doctor.go's
// real-run guard, `factoryd submit`, and POST /requests all evaluate the
// identical condition.
func DataDirInsideWorkspace(workspace, dataDir string) (inside bool, workspaceAbs, dataAbs string, err error) {
	workspaceAbs, err = canonicalPath(workspace)
	if err != nil {
		return false, "", "", fmt.Errorf("resolve -workspace: %w", err)
	}
	dataAbs, err = canonicalPath(dataDir)
	if err != nil {
		return false, "", "", fmt.Errorf("resolve -data-dir: %w", err)
	}
	return pathWithin(workspaceAbs, dataAbs), workspaceAbs, dataAbs, nil
}

// effectivePreflightProfile is a standalone copy of cmd/factoryd's own
// project_check.go function of the same name -- see that copy's doc
// comment for the shared reasoning (a preview must never validate
// against a looser profile than the run it previews). Small and pure
// enough that, like pathWithin/canonicalPath above, a second copy is an
// acceptable, explicitly-flagged tradeoff against pulling in the rest of
// cmd/factoryd's unimportable main package.
const preflightProfileBrownfield = "brownfield"

// repoRootForWorkspace and ProjectBootstrapArtifactPaths are standalone
// copies of cmd/factoryd/project_check.go's own functions of the same
// name (the latter exported here, unlike its cmd/factoryd twin, so
// cmd/factoryd's own parity test --
// TestProjectBootstrapArtifactPathsParityWithRequestSubmit,
// project_check_and_doctor_test.go -- can call this copy directly and
// assert both resolve identical paths, rather than the two silently
// drifting apart) -- see effectivePreflightProfile's doc comment just
// above for why a second copy of a small, pure resolution helper is an
// acceptable, explicitly-flagged tradeoff here rather than importing
// cmd/factoryd's unimportable main package. Needed so
// submitProjectBootstrapPreflight below can resolve the exact same
// spec.md/contract.md/ARCHITECTURE.md paths a real run's own
// project-bootstrap preflight would check, so a request this function
// accepts never fails those same three checks later, at run time, for a
// reason its own inputs (workspace + resolved preflight profile) already
// fully determined at submission time.
func repoRootForWorkspace(workspaceAbs string) string {
	historicalRoot := filepath.Dir(workspaceAbs)
	if _, err := os.Stat(filepath.Join(historicalRoot, "spec", "spec.md")); err == nil {
		return historicalRoot
	}
	if _, err := os.Stat(filepath.Join(workspaceAbs, "spec", "spec.md")); err == nil {
		return workspaceAbs
	}
	if entries, err := os.ReadDir(workspaceAbs); err == nil && len(entries) > 0 {
		return workspaceAbs
	}
	return historicalRoot
}

func ProjectBootstrapArtifactPaths(workspaceAbs string) (specPath, contractPath, architecturePath string) {
	root := repoRootForWorkspace(workspaceAbs)
	return filepath.Join(root, "spec", "spec.md"), filepath.Join(root, "spec", "contract.md"), filepath.Join(root, "ARCHITECTURE.md")
}

// submitProjectBootstrapPreflight runs the same product_spec_frozen/
// program_design_structure/architecture_structure checks a real run's own
// project-bootstrap preflight (runProjectBootstrapCheck,
// evaluateProjectBootstrapChecks) would run for the resolved profile --
// EXCEPT ticket_structure, which has no ticket to check yet at submission
// time (the request pipeline cuts one per ticket only once spec/plan
// review approves, long after Submit returns). Under
// preflightProfileBrownfield this is a no-op: that profile skips
// product_spec_frozen/program_design_structure entirely and only ever
// marks architecture_structure advisory (never blocking), so there is
// nothing new for a brownfield submission to fail here that a real
// brownfield run wouldn't already tolerate.
//
// Found live (2026-09-25): a strict-profile request accepted by Submit
// with an unfrozen spec.md, a malformed contract.md, or no ARCHITECTURE.md
// at all sat through spec_review and plan_review -- both purely textual
// approvals with no artifact check of their own -- before halting at
// ticket 1's own run-time preflight, discarding two rounds of human review
// on a request that could never have passed. Running these three checks
// up front, before Submit ever records the request, refuses it
// immediately with the same diagnostic runProjectBootstrapCheck would give
// at run time instead.
func submitProjectBootstrapPreflight(workspaceAbs, preflightProfile string) error {
	failures := ProjectBootstrapFailures(workspaceAbs, preflightProfile)
	if len(failures) == 0 {
		return nil
	}
	return fmt.Errorf(
		"project-bootstrap preflight failed, request not submitted (this would halt every ticket's own run-time preflight identically, after spec/plan review already spent human attention on it): %s -- fix these before submitting, or set preflight_profile: brownfield in the workspace's %s (or pass -preflight-profile brownfield / preflight_profile \"brownfield\" on this request) if this repo hasn't adopted the spec/contract/ARCHITECTURE.md convention (see `factoryd onboard`)",
		strings.Join(failures, " | "), projectconfig.FileName,
	)
}

// ProjectBootstrapFailures returns one line per project-bootstrap check
// a submission against workspaceAbs under preflightProfile fails (none
// under brownfield). submit refuses on any; doctor -target-repo reports
// them before the operator submits.
func ProjectBootstrapFailures(workspaceAbs, preflightProfile string) []string {
	if preflightProfile == preflightProfileBrownfield {
		return nil
	}
	specPath, contractPath, architecturePath := ProjectBootstrapArtifactPaths(workspaceAbs)
	var failures []string
	checkFile := func(name, path string, check func(string) (bool, []string)) {
		b, err := os.ReadFile(path)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s (%s): could not read artifact: %s", name, path, err))
			return
		}
		if passed, reasons := check(string(b)); !passed {
			failures = append(failures, fmt.Sprintf("%s (%s): %s", name, path, strings.Join(reasons, "; ")))
		}
	}
	checkFile("product_spec_frozen", specPath, func(content string) (bool, []string) {
		passed, reason := policy.ProductSpecFrozen(content)
		if reason == "" {
			return passed, nil
		}
		return passed, []string{reason}
	})
	checkFile("program_design_structure", contractPath, policy.ProgramDesignStructure)
	checkFile("architecture_structure", architecturePath, func(content string) (bool, []string) {
		return policy.ArchitectureStructure(content, nil)
	})
	return failures
}

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

// addRepoGates puts the gates cfg defines for its own repository (`gates:`)
// into gateCommands under their "repo-<id>" names, and returns those names
// for the "applied" line. They have no flag, so nothing can be explicit
// over them: the committed .factory.yml is their only source.
func addRepoGates(gateCommands map[string]*string, cfg *projectconfig.Config) []string {
	if gateCommands == nil {
		return nil
	}
	var names []string
	for _, g := range cfg.Gates {
		command := g.Command
		check := policy.RepoGateCheck(g.ID)
		gateCommands[check] = &command
		names = append(names, "gate "+check)
	}
	return names
}

// ApplyProjectConfigDefaults is the exported form of cmd/factoryd's own
// applyProjectConfigDefaults (validate.go), which now delegates to this
// function -- see that copy's doc comment for the full "explicit flag
// wins, otherwise .factory.yml, relay ceilings are tighten-only"
// contract this preserves verbatim. Only the two fields `submit`/POST
// /requests actually surface (verifyCommand/preflightProfile) are ever
// read back by this package's own Submit; the rest exist purely so the
// same .factory.yml read and the same precedence rules apply here as in
// every other caller of this logic.
// gateCommands is keyed by policy.CommandGate.ID (see policy.CommandGates),
// one *string per command gate a caller wants defaulted from .factory.yml;
// a caller uninterested in a given gate's resolved value (requestsubmit's
// own Submit, which never uses the named-gate commands) still passes a
// non-nil placeholder for every registry entry, matching the "always
// receives one flag per registry entry" contract the CLI edge (cmd/
// factoryd's newRunFlags) already has -- see this function's own tests.
func ApplyProjectConfigDefaults(explicit map[string]bool, workspaceDir string, verifyCommand, fastCheckCommand *string, gateCommands map[string]*string, preflightProfile, releaseProtectedPaths *string, testPatterns *[]string, meterTokenCeiling *int, meterCostCeilingMicroUSD *int64, meterTokenBudget int, meterCostBudget int64) error {
	cfg, found, err := projectconfig.Load(workspaceDir)
	if err != nil {
		return fmt.Errorf("load %s: %w", projectconfig.FileName, err)
	}
	if !found {
		return nil
	}

	var applied []string
	if !explicit["verify-command"] && cfg.VerifyCommand != "" {
		*verifyCommand = cfg.VerifyCommand
		applied = append(applied, "verify-command")
	}
	if !explicit["fast-check-command"] && cfg.FastCheckCommand != "" {
		*fastCheckCommand = cfg.FastCheckCommand
		applied = append(applied, "fast-check-command")
	}
	projectGateCommands := cfg.GateCommands()
	for _, g := range policy.CommandGates {
		dest, ok := gateCommands[g.ID]
		if !ok || dest == nil {
			continue
		}
		value := projectGateCommands[g.ID]
		if !explicit[g.Flag] && value != "" {
			*dest = value
			applied = append(applied, g.Flag)
		}
	}
	applied = append(applied, addRepoGates(gateCommands, cfg)...)
	if len(cfg.TestPatterns) > 0 {
		*testPatterns = cfg.TestPatterns
		applied = append(applied, "test-patterns")
	}
	if !explicit["preflight-profile"] {
		resolved, err := effectivePreflightProfile(*preflightProfile, cfg)
		if err != nil {
			return err
		}
		if resolved != *preflightProfile {
			*preflightProfile = resolved
			applied = append(applied, "preflight-profile")
		}
	}
	if !explicit["release-protected-paths"] && len(cfg.ProtectedPaths) > 0 {
		*releaseProtectedPaths = strings.Join(cfg.ProtectedPaths, ",")
		applied = append(applied, "release-protected-paths")
	}
	defaultTokenCeiling := 5 * int64(meterTokenBudget)
	effectiveTokenCeiling := int64(*meterTokenCeiling)
	if effectiveTokenCeiling == 0 {
		effectiveTokenCeiling = defaultTokenCeiling
	}
	// A repo's .factory.yml may only TIGHTEN the session's relay
	// ceilings, never loosen them: a higher committed value would let a
	// target repo raise its own spend/token limit past what the
	// operator running the session actually approved. Refused rather
	// than ignored: an ignored higher value leaves an operator reading
	// the .factory.yml believing the repo's number is in effect.
	// Equal is a no-op
	// (nothing to apply); 0/absent means the repo declared no ceiling at
	// all and this comparison never runs.
	if cfg.TokenCeiling > effectiveTokenCeiling {
		return fmt.Errorf("%s token_ceiling (%d) exceeds the session ceiling (%d); a repo may only lower it", projectconfig.FileName, cfg.TokenCeiling, effectiveTokenCeiling)
	}
	if cfg.TokenCeiling != 0 && cfg.TokenCeiling < effectiveTokenCeiling {
		if cfg.TokenCeiling == defaultTokenCeiling {
			*meterTokenCeiling = 0
		} else {
			*meterTokenCeiling = int(cfg.TokenCeiling)
		}
		applied = append(applied, "meter-token-ceiling")
	}
	defaultCostCeiling := 5 * meterCostBudget
	effectiveCostCeiling := *meterCostCeilingMicroUSD
	if effectiveCostCeiling == 0 {
		effectiveCostCeiling = defaultCostCeiling
	}
	if cfg.CostCeilingMicroUSD > effectiveCostCeiling {
		return fmt.Errorf("%s cost_ceiling_micro_usd (%d) exceeds the session ceiling (%d); a repo may only lower it", projectconfig.FileName, cfg.CostCeilingMicroUSD, effectiveCostCeiling)
	}
	if cfg.CostCeilingMicroUSD != 0 && cfg.CostCeilingMicroUSD < effectiveCostCeiling {
		if cfg.CostCeilingMicroUSD == defaultCostCeiling {
			*meterCostCeilingMicroUSD = 0
		} else {
			*meterCostCeilingMicroUSD = cfg.CostCeilingMicroUSD
		}
		applied = append(applied, "meter-cost-ceiling-micro-usd")
	}
	if len(applied) > 0 {
		log.Printf("%s: applied its %s (no flag given)", projectconfig.FileName, strings.Join(applied, ", "))
	}
	return nil
}

// submitDefaultMeterTokenBudget/submitDefaultMeterCostBudget mirror
// cmd/factoryd's own submitDefaultMeterTokenBudget/
// submitDefaultMeterCostBudget -- the same run_ticket.go
// -meter-token-budget/-meter-cost-budget-micro-usd literal defaults,
// needed only to compute ApplyProjectConfigDefaults' tighten-only
// relay-ceiling comparison (Submit itself never runs a relay or writes a
// ceiling flag).
const (
	submitDefaultMeterTokenBudget = 1_000_000
	submitDefaultMeterCostBudget  = 5_000_000
)

// validateDesignGuide refuses a submission whose repository names a design
// guide (.factory.yml design_guide) the session config cannot resolve, so
// the operator hears it at submit and not when spec drafting fails.
func validateDesignGuide(workspace string, settings sessionconfig.Settings) error {
	cfg, found, err := projectconfig.Load(workspace)
	if err != nil {
		return fmt.Errorf("load %s: %w", projectconfig.FileName, err)
	}
	if !found || cfg.DesignGuide == "" {
		return nil
	}
	if _, err := sessionconfig.LoadDesignGuide(settings, cfg.DesignGuide); err != nil {
		return fmt.Errorf("%s names design_guide %q: %w", projectconfig.FileName, cfg.DesignGuide, err)
	}
	return nil
}

// validateDraftingInputs checks, before anything is recorded, what the
// drafting stages will read: the repository's design guide resolves, and a
// handed-over spec passes the skeleton check a drafted one must pass.
func validateDraftingInputs(workspace string, p Params) error {
	if err := validateDesignGuide(workspace, p.Settings); err != nil {
		return err
	}
	if p.ImportedSpec == "" {
		if len(p.ImportedTickets) > 0 {
			return errors.New("-plan-dir needs -spec-file: tickets name the acceptance criteria of the spec they plan")
		}
		return nil
	}
	if err := request.ValidateSpecSkeleton(p.ImportedSpec); err != nil {
		return fmt.Errorf("-spec-file: %w", err)
	}
	return validateImportedTickets(p.ImportedSpec, p.ImportedTickets)
}

// ImportedTicket is one handed-over ticket: its file name (NNN.spec.md)
// and full text.
type ImportedTicket struct {
	Filename string
	Content  string
}

// validateImportedTickets checks a handed-over plan's shape at submit: the
// files are 001.spec.md, 002.spec.md, ... with no gap, each has the ticket
// skeleton, and together they cover every acceptance criterion of spec.
func validateImportedTickets(spec string, tickets []ImportedTicket) error {
	if len(tickets) == 0 {
		return nil
	}
	contents := make([]string, 0, len(tickets))
	for i, ticket := range tickets {
		if want := fmt.Sprintf("%03d.spec.md", i+1); ticket.Filename != want {
			return fmt.Errorf("-plan-dir: ticket %d is named %q, want %q (tickets are numbered from 001 with no gap)", i+1, ticket.Filename, want)
		}
		if err := request.ValidateTicketPlan(ticket.Content); err != nil {
			return fmt.Errorf("-plan-dir: ticket %s: %w", ticket.Filename, err)
		}
		contents = append(contents, ticket.Content)
	}
	criteria, err := request.SpecAcceptanceCriteriaCount(spec)
	if err != nil {
		return fmt.Errorf("-spec-file: %w", err)
	}
	if err := request.ValidatePlanCoverage(criteria, contents); err != nil {
		return fmt.Errorf("-plan-dir: %w", err)
	}
	return nil
}

// saveRequestDocuments writes the request's text and, when the operator
// handed one over, its spec, before the request record itself exists: a
// record marked SpecImported is never saved without its spec file.
func saveRequestDocuments(p Params, id, requestText string) error {
	if err := request.SaveText(p.DataDir, id, requestText); err != nil {
		return fmt.Errorf("write request text: %w", err)
	}
	if p.ImportedSpec == "" {
		return nil
	}
	if err := os.WriteFile(request.ImportedSpecPath(p.DataDir, id), []byte(p.ImportedSpec), 0o600); err != nil {
		return fmt.Errorf("write the handed-over spec: %w", err)
	}
	if len(p.ImportedTickets) == 0 {
		return nil
	}
	ticketsDir := request.ImportedTicketsDir(p.DataDir, id)
	if err := os.MkdirAll(ticketsDir, 0o750); err != nil {
		return fmt.Errorf("create the handed-over tickets directory: %w", err)
	}
	for _, ticket := range p.ImportedTickets {
		if err := os.WriteFile(filepath.Join(ticketsDir, ticket.Filename), []byte(ticket.Content), 0o600); err != nil {
			return fmt.Errorf("write the handed-over ticket %s: %w", ticket.Filename, err)
		}
	}
	return nil
}

// claimRequestID is the request's id: the one the caller claimed (p.ID), else
// one claimed here from p.IDText, or from the request text when that is empty.
func claimRequestID(p Params, requestText string) (string, error) {
	if p.ID != "" {
		return p.ID, nil
	}
	idText := strings.TrimSpace(p.IDText)
	if idText == "" {
		idText = requestText
	}
	id, err := request.ClaimID(p.DataDir, request.GenerateID(idText, time.Now()))
	if err != nil {
		return "", fmt.Errorf("generate request id: %w", err)
	}
	return id, nil
}

// Submit records a new internal/request.Request in StateSubmitted under
// dataDir/requests/<id>/, exactly as cmd/factoryd's old submitRequest
// did, and returns immediately without running anything itself.
func Submit(p Params) (Result, error) {
	if err := sessionconfig.ValidateRequestModels(p.Settings, p.Models); err != nil {
		return Result{}, err
	}
	if err := sessionconfig.ValidateRequestHarnesses(p.Settings, p.Harnesses); err != nil {
		return Result{}, err
	}
	workspaceAbs, err := filepath.Abs(p.WorkspaceArg)
	if err != nil {
		return Result{}, fmt.Errorf("resolve workspace: %w", err)
	}
	if info, err := os.Stat(workspaceAbs); err != nil {
		return Result{}, fmt.Errorf("workspace %q: %w", p.WorkspaceArg, err)
	} else if !info.IsDir() {
		return Result{}, fmt.Errorf("workspace %q is not a directory", p.WorkspaceArg)
	}
	if err := validateDraftingInputs(workspaceAbs, p); err != nil {
		return Result{}, err
	}
	if inside, _, dataAbs, err := DataDirInsideWorkspace(workspaceAbs, p.DataDir); err != nil {
		return Result{}, err
	} else if inside {
		return Result{}, fmt.Errorf("-data-dir (%q, resolving to %q) is inside -workspace %q; Docker containment is unconditional and a later worker will refuse to drain this entry once queued here, with no way to re-home it -- pass -data-dir <path outside -workspace>", p.DataDir, dataAbs, workspaceAbs)
	}
	repositoryRoot := release.RepositoryRoot(workspaceAbs)
	project := filepath.Base(repositoryRoot)
	if err := release.SinglePathComponent("project", project); err != nil {
		return Result{}, fmt.Errorf("workspace %q: %w", p.WorkspaceArg, err)
	}
	if err := release.RejectProjectCollision(p.DataDir, project, repositoryRoot); err != nil {
		return Result{}, err
	}

	requestText := strings.TrimSpace(p.RequestText)
	if requestText == "" {
		return Result{}, fmt.Errorf("request text must not be empty")
	}

	verifyCommand := p.VerifyCommand
	preflightProfile := p.PreflightProfile
	explicit := map[string]bool{}
	if p.VerifyCommandExplicit {
		explicit["verify-command"] = true
	}
	if p.PreflightProfileExplicit {
		explicit["preflight-profile"] = true
	}
	var discardedFastCheckCommand string
	discardedGateCommands := make(map[string]*string, len(policy.CommandGates))
	for _, g := range policy.CommandGates {
		var discarded string
		discardedGateCommands[g.ID] = &discarded
	}
	var discardedProtectedPaths string
	var discardedTestPatterns []string
	// discardedTokenCeiling/discardedCostCeiling seed as the caller's own
	// session ceilings when supplied (SessionTokenCeiling/
	// SessionCostCeilingMicroUSD), so ApplyProjectConfigDefaults' tighten-
	// only comparison below runs against the real ceiling the eventual
	// run would use, not submitDefaultMeterTokenBudget/
	// submitDefaultMeterCostBudget's hardcoded legacy defaults -- see
	// Params.SessionTokenCeiling's own doc comment.
	discardedTokenCeiling := int(p.SessionTokenCeiling)
	discardedCostCeiling := p.SessionCostCeilingMicroUSD
	if err := ApplyProjectConfigDefaults(explicit, workspaceAbs, &verifyCommand, &discardedFastCheckCommand, discardedGateCommands, &preflightProfile, &discardedProtectedPaths, &discardedTestPatterns, &discardedTokenCeiling, &discardedCostCeiling, submitDefaultMeterTokenBudget, submitDefaultMeterCostBudget); err != nil {
		return Result{}, err
	}
	if verifyCommand == "" {
		return Result{}, fmt.Errorf("no verify command resolvable: pass -verify-command, or commit a verify_command in the workspace's %s", projectconfig.FileName)
	}
	if err := submitProjectBootstrapPreflight(workspaceAbs, preflightProfile); err != nil {
		return Result{}, err
	}

	if err := RequireAgentsFile(workspaceAbs); err != nil {
		return Result{}, fmt.Errorf("request not submitted: %w", err)
	}

	id, err := claimRequestID(p, requestText)
	if err != nil {
		return Result{}, err
	}

	if err := saveRequestDocuments(p, id, requestText); err != nil {
		return Result{}, err
	}
	req := request.New(id, workspaceAbs, project, p.Source, time.Now())
	req.VerifyCommand = verifyCommand
	// The request's own full-suite command: the -full-suite-command /
	// body value when set, else the workspace's .factory.yml
	// full_suite_command, then ResolveFullSuiteCommand's operator-approved
	// verify-command substitution (an onboarding-plan decision). Shared by
	// `factoryd submit` and POST /requests, so both record the same
	// FullSuiteSource.
	fullSuiteConfigured := p.FullSuiteCommand
	if fullSuiteConfigured == "" {
		if cfg, found, err := projectconfig.Load(workspaceAbs); err != nil {
			return Result{}, fmt.Errorf("load %s: %w", projectconfig.FileName, err)
		} else if found {
			fullSuiteConfigured = cfg.FullSuiteCommand
		}
	}
	req.FullSuiteCommand, req.FullSuiteSource = ResolveFullSuiteCommand(fullSuiteConfigured, verifyCommand)
	req.NoCommitOracles = p.NoCommitOracles
	req.DraftOracles = p.DraftOracles
	req.SpecImported = p.ImportedSpec != ""
	req.PlanImported = len(p.ImportedTickets) > 0
	req.PreflightProfile = preflightProfile
	req.Harnesses = p.Harnesses
	req.Models = p.Models
	if err := req.Save(p.DataDir); err != nil {
		return Result{}, fmt.Errorf("write request: %w", err)
	}

	return Result{ID: id, Project: project, RepositoryRoot: repositoryRoot}, nil
}

// FullSuiteCommandNone is the explicit `-full-suite-command none` /
// `.factory.yml full_suite_command: none` opt-out value: it keeps
// ResolveFullSuiteCommand's pre-substitution behavior (no full suite
// command means full_suite_verify never runs, and RequiredGates denies
// release the same way it always has) for an operator who deliberately
// doesn't want a full suite run at all, rather than being defaulted into
// one they didn't ask for.
const FullSuiteCommandNone = "none"

// FullSuiteSourceVerifyCommand/FullSuiteSourceNone are the two non-empty
// request.Request.FullSuiteSource/run.Run.FullSuiteSource values
// ResolveFullSuiteCommand can produce -- see FullSuiteSource's own doc
// comment on each struct. "" (the zero value) means a real,
// operator/`.factory.yml`-configured command, no substitution.
const (
	FullSuiteSourceVerifyCommand = "verify_command"
	FullSuiteSourceNone          = "none"
)

// ResolveFullSuiteCommand implements the operator-approved 2026-09-24
// decision (the other half of fixing a happy path that ended in a
// policy denial): when no full_suite_command resolves from any source
// (a request's own -full-suite-command, or the workspace's
// .factory.yml), the resolved
// canonical verify command becomes the full-suite command instead of
// leaving full_suite_verify permanently unconfigured -- before this, a
// repository with no committed full_suite_command could never open a
// pull request, since MergePolicyCheck's RequiredGates only exempts
// full_suite_verify when it was configured but skipped by cadence
// (r.FullSuiteConfigured && !r.FullSuiteScheduled), never when it was
// never configured at all (see MergePolicyCheck's own comment on that
// exemption -- deliberately NOT relaxed by this change: this function
// supplies a command for that gate to actually run against, it does not
// touch the gate's own required-ness).
//
// configured is whatever full-suite command has already been resolved
// from flag/session-config/.factory.yml (BEFORE this substitution — not
// yet defaulted to the verify command); verifyCommand is this same
// request/run's own resolved canonical verify command. Returns the
// effective full-suite command to actually configure and run, and which
// of FullSuiteSourceVerifyCommand/FullSuiteSourceNone/"" it came from:
// FullSuiteCommandNone (the literal opt-out sentinel) returns ("",
// FullSuiteSourceNone) -- no substitution, matching pre-2026-09-24
// behavior; a real configured value is returned unchanged with source ""
// (nothing to record: it was not a substitution); an empty configured
// value substitutes verifyCommand (itself possibly still empty, which
// this function does not special-case -- an empty verifyCommand yields
// ("", "") the same as before, since there's nothing to substitute with).
func ResolveFullSuiteCommand(configured, verifyCommand string) (command, source string) {
	switch {
	case configured == FullSuiteCommandNone:
		return "", FullSuiteSourceNone
	case configured != "":
		return configured, ""
	case verifyCommand != "":
		return verifyCommand, FullSuiteSourceVerifyCommand
	default:
		return "", ""
	}
}
