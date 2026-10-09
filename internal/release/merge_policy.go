package release

import (
	"fmt"

	"buildgate/internal/run"
)

// MergePolicy configures the evidence limits checked before a future
// autonomous merge may proceed.
type MergePolicy struct {
	ProtectedPaths  []string
	MaxFilesChanged int
	MaxInsertions   int
	// RollbackPlan identifies the rollback plan included in the evidence
	// package. Checking that reference is present does not implement rollback.
	RollbackPlan string
	// AllowOverrides is deliberately false by default: an override records a
	// human judgment, so silently treating an override-touched run as eligible
	// for autonomous merge would discard relevant review context.
	AllowOverrides bool
	// AllowDependencyLockfileChanges is deliberately false by default, same
	// reasoning as AllowOverrides: a dependency-lockfile touch is a real
	// supply-chain-risk signal (internal/evidence.DependencyLockfilesTouched
	// is filename-based only — it doesn't know what changed inside the
	// lockfile), so silently letting it through by default would understate
	// the risk. This checks presence, not content: it cannot yet evaluate
	// which dependency or version changed, only that a known lockfile was
	// touched at all.
	AllowDependencyLockfileChanges bool
	// AllowUnsandboxed is deliberately false by default, same reasoning as
	// AllowOverrides/AllowDependencyLockfileChanges: before this field
	// existed, an accepted run's own state and gate results looked
	// identical whether the code that produced them ran inside the Docker
	// sandbox or bare on the host -- containment status was real evidence
	// (run.Attempt.ImageDigest) but invisible to this exact fail-closed
	// path (found via a real Opus review pass, 2026-09-04). This does
	// not require every run in the repo to be sandboxed --
	// Phase 5 sandboxing itself stays opt-in -- it only means an
	// unsandboxed run is denied at *this* gate unless an operator
	// explicitly opts back in, the same restrictive-by-default shape
	// every other flag on this struct already uses.
	AllowUnsandboxed bool
	// RequiredGates lists the policy.GateResult.Check names that must both
	// appear in r.GateResults and have passed for this run to be
	// release-eligible. Every other check in this function inspects
	// gate results that already ran; none of them can tell "the strong
	// gates passed" apart from "the strong gates never ran" -- a run that
	// never received -full-suite-command runs only canonical_verify and
	// is accepted with a GateResults slice that looks, in shape, identical
	// to a fully-gated one. RequiredGates closes that for full_suite_verify:
	// a name listed here but absent from r.GateResults denies with a
	// distinct reason from "a recorded gate failed" (found via the
	// 2026-09-05 Opus review, S1) -- UNLESS the run's own record says that
	// gate was never actually supposed to run this slice, currently only
	// meaningful for "full_suite_verify" via r.FullSuiteScheduled (found
	// via a real local `codex review` pass on this same PR: an earlier
	// version required it unconditionally, denying every valid
	// non-cadence-boundary run under -full-suite-cadence). diff_scope/
	// required_files_changed/required_content_present are deliberately
	// NOT included in cmd/factoryd's own default list below: unlike
	// full_suite_verify, whether a ticket declared the corresponding key
	// is not currently recorded anywhere on run.Run, so this function has
	// no way to distinguish "the ticket legitimately didn't declare one"
	// from "a ticket-generation regression dropped it" for those three --
	// requiring them unconditionally would deny release for every
	// legitimately undeclared-scope ticket, the same class of bug the
	// full_suite_verify fix above closes. Nil means no additional gate is
	// required beyond whatever already ran -- the caller (cmd/factoryd) is
	// expected to default this to at least canonical_verify and
	// full_suite_verify for any policy actually used to gate a real
	// release decision, the same way it defaults RollbackPlan to "" so an
	// unconfigured policy denies honestly rather than passing by omission.
	RequiredGates []string
	// AllowSkippedProjectCheck is deliberately false by default, same
	// restrictive-by-default reasoning as AllowOverrides/AllowUnsandboxed:
	// a run recorded with SkipProjectCheck bypassed the mandatory
	// project-bootstrap preflight (product_spec_frozen/
	// program_design_structure/architecture_structure), so its acceptance
	// evidence was never checked against spec/spec.md, spec/contract.md,
	// or ARCHITECTURE.md. Silently treating that run as merge-eligible
	// would discard exactly the signal SkipProjectCheck exists to record.
	AllowSkippedProjectCheck bool
}

// CanNeverAllow reports whether p denies every release decision
// unconditionally, regardless of any run's own evidence: MergePolicyCheck
// denies whenever RollbackPlan == "", MaxFilesChanged == 0, or
// MaxInsertions == 0 -- and those are this repo's own documented
// zero-value/-release-* flag defaults, so an unconfigured policy silently
// denies every PR forever with no indication why. A pure, static check
// against the policy alone -- no run or decision needed -- which is what
// makes it usable both at CLI startup (cmd/factoryd's own
// releasePolicyCanNeverAllow wraps this) and from a read-only API/console
// surface (internal/api's consoleConfig) that has no run to evaluate
// against.
func (p MergePolicy) CanNeverAllow() bool {
	return p.RollbackPlan == "" || p.MaxFilesChanged == 0 || p.MaxInsertions == 0
}

// MergePolicyCheck evaluates the release evidence currently recorded on a run.
// It is a pure decision and returns every reason the run must not be merged.
//
// Full semantic dependency-change limits (which dependency, which version)
// are intentionally not checked: Run has no collected evidence of that
// yet — internal/evidence.DependencyLockfilesTouched only says a known
// lockfile's basename appeared in the changed-file inventory, not what
// changed inside it — so pretending to enforce a version-aware limit would
// create a false safety claim. What that filename-based evidence DOES
// support — blocking on the mere presence of a lockfile touch unless
// explicitly allowed — is checked below. The caller supplies the
// rollback-plan reference; linked product/technical checks and other wider
// Phase 7 evidence still need durable inputs before those gaps can be
// evaluated here.
func MergePolicyCheck(r run.Run, cfg MergePolicy) (bool, []string) {
	var reasons []string

	if r.State != run.StateAccepted {
		reasons = append(reasons, fmt.Sprintf("run state is %q, not %q", r.State, run.StateAccepted))
	}
	if len(r.GateResults) == 0 {
		reasons = append(reasons, "run has no gate results")
	}
	for i, gate := range r.GateResults {
		if !gate.Passed {
			reasons = append(reasons, fmt.Sprintf("gate result %d (%q) did not pass", i, gate.Check))
		}
	}
	if r.BaseSHA == "" {
		reasons = append(reasons, "run has no base SHA")
	}
	if r.ResultSHA == "" {
		reasons = append(reasons, "run has no result SHA")
	}
	if r.ChangedFiles == nil {
		reasons = append(reasons, "run has no changed-file inventory")
	} else {
		for _, changed := range cfg.ProtectedFilesTouchedByRun(r) {
			reasons = append(reasons, fmt.Sprintf("changed file %q is protected", changed))
		}
		if r.DependencyLockfilesTouched == nil {
			reasons = append(reasons, "run has no dependency-lockfile evidence")
		} else if len(r.DependencyLockfilesTouched) > 0 && !cfg.AllowDependencyLockfileChanges {
			reasons = append(reasons, fmt.Sprintf("run touched dependency lockfile(s) %v and policy does not allow dependency-lockfile changes", r.DependencyLockfilesTouched))
		}
	}

	if r.DiffStat == nil {
		reasons = append(reasons, "run has no diff stat")
	} else {
		if r.DiffStat.FilesChanged < 0 {
			reasons = append(reasons, "diff files changed must not be negative")
		} else if cfg.MaxFilesChanged < 0 {
			reasons = append(reasons, "max files changed must not be negative")
		} else if r.DiffStat.FilesChanged > cfg.MaxFilesChanged {
			reasons = append(reasons, fmt.Sprintf("diff changes %d files, limit is %d", r.DiffStat.FilesChanged, cfg.MaxFilesChanged))
		}
		if r.DiffStat.Insertions < 0 {
			reasons = append(reasons, "diff insertions must not be negative")
		} else if cfg.MaxInsertions < 0 {
			reasons = append(reasons, "max insertions must not be negative")
		} else if r.DiffStat.Insertions > cfg.MaxInsertions {
			reasons = append(reasons, fmt.Sprintf("diff has %d insertions, limit is %d", r.DiffStat.Insertions, cfg.MaxInsertions))
		}
	}
	if cfg.RollbackPlan == "" {
		reasons = append(reasons, "evidence package has no rollback plan")
	}

	if len(r.Overrides) != 0 && !cfg.AllowOverrides {
		reasons = append(reasons, "run has override history and policy does not allow overridden runs")
	}

	if !r.Sandboxed() && !cfg.AllowUnsandboxed {
		reasons = append(reasons, "run's build attempt did not execute inside the Docker sandbox (no image digest recorded) and policy does not allow unsandboxed runs")
	}

	if len(cfg.RequiredGates) > 0 {
		passed := make(map[string]bool, len(r.GateResults))
		for _, gate := range r.GateResults {
			if gate.Passed {
				passed[gate.Check] = true
			}
		}
		for _, name := range cfg.RequiredGates {
			// full_suite_verify is legitimately absent from GateResults
			// on a run this policy still fully expects to be release-
			// eligible: -full-suite-cadence deliberately clears the
			// effective command (leaving r.FullSuiteScheduled false) on
			// every slice that isn't due, by design, not by omission —
			// found via a real local `codex review` pass on this PR: an
			// earlier version of this check required it unconditionally,
			// which denied the release decision for every valid
			// non-cadence-boundary run. r.FullSuiteScheduled (set
			// wherever the effective command is resolved) is the
			// authoritative "was this run actually supposed to run it"
			// signal EvaluateRun's own input construction already
			// records, so this only holds a run to the requirement when
			// that's true.
			//
			// r.FullSuiteConfigured, not r.FullSuiteScheduled alone
			// (found via a real GitHub Codex App review of this same
			// PR): FullSuiteScheduled is also false whenever
			// -full-suite-command was never configured at all, not
			// just when cadence skipped a configured one -- exempting
			// on that alone let a run that never enabled the oracle in
			// the first place satisfy this requirement with no
			// result, silently reopening the exact canonical-only
			// release path this fix exists to close. Only a
			// cadence-skipped slice of a genuinely configured suite
			// (Configured true, Scheduled false) is exempt; an
			// entirely unconfigured suite (Configured false) still
			// denies below, same as any other missing required gate.
			if name == "full_suite_verify" && r.FullSuiteConfigured && !r.FullSuiteScheduled {
				continue
			}
			if !passed[name] {
				reasons = append(reasons, fmt.Sprintf("policy requires gate %q but it has no passing result on this run -- either it never ran or it failed", name))
			}
		}
	}

	// InvalidatedByRunID/SpecDriftDetectedByRunID are attribution the
	// factory only ever adds after this run was already accepted -- a
	// later chained run observed a full-suite regression or a project
	// spec/contract change it attributes back to this one. Neither
	// changes r.State (see run.Run's own doc comments on both fields for
	// why), so without reading them here a run durably flagged this way
	// still evaluates identically to one that was never flagged at all.
	// There is no AllowInvalidated/AllowSpecDrift escape hatch: unlike
	// AllowOverrides or AllowUnsandboxed, these do not represent a
	// legitimate operator choice to accept known risk -- they are the
	// factory's own record that this run's evidence may no longer be
	// trustworthy, so a human must resolve the underlying flag (not this
	// policy) before the run can be merge-eligible again.
	if r.InvalidatedByRunID != "" {
		reasons = append(reasons, fmt.Sprintf("run was invalidated by run %q: %s", r.InvalidatedByRunID, r.InvalidatedReason))
	}
	if r.SpecDriftDetectedByRunID != "" {
		reasons = append(reasons, fmt.Sprintf("spec/contract drift was detected by run %q: %s", r.SpecDriftDetectedByRunID, r.SpecDriftReason))
	}
	if r.SkipProjectCheck && !cfg.AllowSkippedProjectCheck {
		reasons = append(reasons, "run bypassed the mandatory project-bootstrap preflight (-skip-project-check) and policy does not allow skipped-preflight runs")
	}

	reasons = append(reasons, memoryEditReasons(r.MemoryEdit)...)

	return len(reasons) == 0, reasons
}

// Reasons memoryEditReasons returns. Callers and tests match on them.
const (
	// ReasonMemorySectionNotMemoryChange: the base has a memory section and
	// a run with no proposal changed a root instruction name.
	ReasonMemorySectionNotMemoryChange = "AGENTS.md of a repository with a memory section changed by a run that is not a memory change"
	// ReasonMemoryMarkersAdded: the base has no section and a run with no
	// proposal left "buildgate:memory" in a root instruction file.
	ReasonMemoryMarkersAdded = "a run that is not a memory change added memory markers to AGENTS.md"
	// ReasonSeveralRootInstructionNames: the result tree spells the root
	// instruction file more than one way.
	ReasonSeveralRootInstructionNames = "the result holds more than one root file named AGENTS.md in some letter case"
	// ReasonRootInstructionNotRegular: a run with no proposal changed a root
	// instruction name into a symlink, a submodule or a directory.
	ReasonRootInstructionNotRegular = "a run that is not a memory change made AGENTS.md something other than a regular file"
	ReasonMemoryChangeNotApproved   = "memory change does not match the approved text"
	reasonMemoryCheckIncomplete     = "memory section check could not be completed"
)

// memoryEditReasons turns the host's evidence about the root instruction
// names into denials. A memory request's run (one with an approved proposal)
// is released only when AGENTS.md is exactly the approved file and no other
// file changed. Any other run may not change a root instruction name of a
// repository whose base has a memory section, and elsewhere may not leave a
// memory marker, a second spelling or a non-file there. It reads the evidence
// only: the host did the I/O.
func memoryEditReasons(m *run.MemoryEdit) []string {
	switch {
	case m == nil:
		return nil
	case m.Error != "":
		return []string{fmt.Sprintf("%s: %s", reasonMemoryCheckIncomplete, m.Error)}
	case m.Proposal:
		if m.Matches && len(m.OtherFilesChanged) == 0 {
			return nil
		}
		if len(m.OtherFilesChanged) == 0 {
			return []string{ReasonMemoryChangeNotApproved}
		}
		return []string{namingPaths(ReasonMemoryChangeNotApproved+": other changed files", m.OtherFilesChanged)}
	case m.BaseHasSection:
		if len(m.ChangedRootNames) == 0 {
			return nil
		}
		return []string{namingPaths(ReasonMemorySectionNotMemoryChange, m.ChangedRootNames)}
	}
	var reasons []string
	if len(m.MarkerIn) > 0 {
		reasons = append(reasons, namingPaths(ReasonMemoryMarkersAdded, m.MarkerIn))
	}
	if len(m.ResultRootNames) > 1 {
		reasons = append(reasons, namingPaths(ReasonSeveralRootInstructionNames, m.ResultRootNames))
	}
	if len(m.NotRegularFile) > 0 {
		reasons = append(reasons, namingPaths(ReasonRootInstructionNotRegular, m.NotRegularFile))
	}
	return reasons
}

// namingPaths is reason followed by at most three of paths, quoted.
func namingPaths(reason string, paths []string) string {
	shown := paths[:min(len(paths), 3)]
	reason = fmt.Sprintf("%s: %q", reason, shown)
	if more := len(paths) - len(shown); more > 0 {
		reason += fmt.Sprintf(" and %d more", more)
	}
	return reason
}
