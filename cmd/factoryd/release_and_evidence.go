package main

import (
	"buildgate/internal/requestdriver"
	"buildgate/internal/requestsubmit"
	"context"
	"errors"
	"fmt"
	"log"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"buildgate/internal/conformity"
	"buildgate/internal/forge"
	"buildgate/internal/notify"
	gatepolicy "buildgate/internal/policy"
	"buildgate/internal/release"
	"buildgate/internal/request"
	"buildgate/internal/run"
	"buildgate/internal/sandbox"
	"buildgate/internal/sanitize"
	"buildgate/internal/ticketspec"
)

// relaySpendSummary sums the actual relay spend recorded across every
// attempt (Attempt.RelayConsumedCostMicroUSD/RelayConsumedInputTokens/
// RelayConsumedOutputTokens) and reports whether any attempt's total is a
// partial recovery, not a confirmed final spend -- Attempt.
// RelaySpendPartial's own doc comment explains why that can happen (the
// relay exited abnormally and its usage ledger, not its container logs,
// is what got read at cleanup). Callers rendering totalCostMicroUSD/
// totalTokens as evidence must say so whenever partial is true, per
// CLAIMS.md's "crash-orphaned relay" residual: an invented-looking
// complete number is worse than an honest partial one when it can feed a
// budget-ceiling decision.
func relaySpendSummary(attempts []run.Attempt) (totalCostMicroUSD, totalTokens int64, partial bool) {
	for _, a := range attempts {
		totalCostMicroUSD += a.RelayConsumedCostMicroUSD
		totalTokens += a.RelayConsumedInputTokens + a.RelayConsumedOutputTokens
		if a.RelaySpendPartial {
			partial = true
		}
	}
	return totalCostMicroUSD, totalTokens, partial
}

// relaySpendLine renders a relaySpendSummary result as the text every
// cost/evidence line below shares: "not recorded" when nothing was ever
// spent, the partial marking (never a bare number presented as final) when
// any attempt's spend is an incomplete recovery, or the plain total
// otherwise.
func relaySpendLine(totalCostMicroUSD, totalTokens int64, partial, subscriptionBilled bool) string {
	if partial {
		s := fmt.Sprintf("partial (relay exited abnormally after $%.4f recorded, %d tokens)", float64(totalCostMicroUSD)/1_000_000, totalTokens)
		if subscriptionBilled {
			s += subscriptionCostSuffix
		}
		return s
	}
	if totalCostMicroUSD == 0 && totalTokens == 0 {
		return "not recorded"
	}
	s := fmt.Sprintf("%d tokens, $%.4f", totalTokens, float64(totalCostMicroUSD)/1_000_000)
	if subscriptionBilled {
		s += subscriptionCostSuffix
	}
	return s
}

// subscriptionCostSuffix is appended, for a SINGLE run's own cost figure
// (this PR body, `factoryd status`/`watch`, and the progress-feed round
// summary all render one run's cost, never a rollup across runs), when
// run.SubscriptionBilled says that run's spend was billed to a ChatGPT/
// Copilot subscription rather than a metered API key. A live walk found a
// chatgpt-codex-routed run's PR body reading "Cost: 84086 tokens,
// $0.3045" -- a real number, computed from the relay's own configured
// InputMicroUSDPerMTok/CachedInputMicroUSDPerMTok/CacheWriteMicroUSDPerMTok/
// OutputMicroUSDPerMTok (relay.go's cost ledger prices every route
// identically regardless of credential mode,
// deliberately unchanged by this fix -- see run.SubscriptionBilled's own
// doc comment), but nothing was actually charged to the operator in
// dollars: their ChatGPT/Copilot subscription, not the relay's own price
// list, is what they're billed by. One consistent treatment (labeling the
// estimate, not hiding it) is applied at every single-run cost call site
// rather than switching to tokens-only, since the estimate is still the
// best signal an operator has for "was this an expensive run."
//
// For a figure that can mix runs -- internal/api's costSummary
// (GET /requests, across every ticket's build plus any corrective
// review rounds) and cmd/factoryd's apiProjectStatsProvider
// (GET /projects/{project}/stats' MedianAcceptedCostMicroUSD, across a
// whole project's accepted runs) -- this wording is wrong: "billed to
// your subscription" is a positive claim about the WHOLE figure that can
// be false whenever only some of the contributing spend was
// subscription-billed (or, for costSummary's Spec/Plan components, has
// no credential-mode evidence to check at all). Those two render
// subscriptionCostAggregateSuffix instead (an adversarial review of
// PR #18's fix) -- see that constant's own doc comment.
const subscriptionCostSuffix = " (API-price est.; billed to your subscription)"

// subscriptionCostAggregateSuffix is subscriptionCostSuffix's own
// softer-worded counterpart for a figure that mixes runs -- see that
// constant's own doc comment for which figures those are and why the
// stronger wording is a claim that can be false there. Not rendered
// anywhere in this Go binary today, and the console shows tokens rather
// than a dollar figure (console/src/domain/cost.ts) -- defined here anyway
// so the aggregate wording sits beside the single-run one it must not be
// confused with.
const subscriptionCostAggregateSuffix = " (includes subscription-billed runs; API-price est.)"

// releasePolicyFromFlags builds an internal/release.MergePolicy from the
// -release-* flags. Factored out of runMain's own accepted branch so
// runViaTemporal/runViaRepositoryOwner (called from runMain, where these
// flag pointers are in scope) can build the identical policy to pass down
// to applyRunWorkflowResult, rather than each re-deriving it.
func releasePolicyFromFlags(protectedPathsCSV string, maxFilesChanged, maxInsertions int, rollbackPlan string, allowOverrides, allowDependencyLockfileChanges, allowUnsandboxed, allowSkippedProjectCheck bool) release.MergePolicy {
	var protectedPaths []string
	if protectedPathsCSV != "" {
		protectedPaths = strings.Split(protectedPathsCSV, ",")
	}
	return release.MergePolicy{
		ProtectedPaths:                 protectedPaths,
		MaxFilesChanged:                maxFilesChanged,
		MaxInsertions:                  maxInsertions,
		RollbackPlan:                   rollbackPlan,
		AllowOverrides:                 allowOverrides,
		AllowDependencyLockfileChanges: allowDependencyLockfileChanges,
		AllowUnsandboxed:               allowUnsandboxed,
		AllowSkippedProjectCheck:       allowSkippedProjectCheck,
		// Hardcoded rather than another -release-* flag: unlike the
		// fields above, there is no legitimate operator reason to want
		// fewer gates required, only a stale caller that hasn't been
		// updated -- so this defaults with no opt-out, the same way
		// RollbackPlan's "" default always denies rather than passing an
		// unconfigured policy by omission (found via the 2026-09-05 Opus
		// review, S1). Deliberately NOT policy.AllGateChecks (every gate
		// EvaluateRun can produce): diff_scope/required_files_changed/
		// required_content_present run only if the ticket declared the
		// corresponding key, which is not recorded anywhere on run.Run,
		// so requiring them unconditionally would deny release for every
		// legitimately undeclared-scope ticket (found via a real local
		// `codex review` pass on this same PR -- the same pass also
		// caught full_suite_verify being required even on a
		// -full-suite-cadence slice that was never scheduled to run it;
		// MergePolicyCheck's own RequiredGates handling now exempts that
		// case via r.FullSuiteScheduled, so canonical_verify and
		// full_suite_verify are the two names actually safe to require
		// unconditionally here).
		RequiredGates: []string{"canonical_verify", "full_suite_verify"},
	}
}

// fullSuiteCommandNone, fullSuiteSourceVerifyCommand/fullSuiteSourceNone
// and resolveFullSuiteCommand live in internal/requestsubmit (its
// ResolveFullSuiteCommand doc comment has the operator decision and the
// gate reasoning) so `factoryd submit`, the console's POST /requests and
// this package's own run paths share one resolution.
const (
	fullSuiteCommandNone         = requestsubmit.FullSuiteCommandNone
	fullSuiteSourceVerifyCommand = requestsubmit.FullSuiteSourceVerifyCommand
	fullSuiteSourceNone          = requestsubmit.FullSuiteSourceNone
)

// resolveEffectiveFullSuiteCommand is run_ticket.go's own final
// resolution step -- applied after the
// .factory.yml fallback already folded into command (that flag's own
// resolution block, unchanged by this function). Unlike
// resolveFullSuiteCommand (used fresh by submit.go/request_driver.go,
// deciding from a configured value with no upstream context), this must
// also respect a "verify_command"/"none" source ALREADY forwarded from
// upstream via the -full-suite-source flag (worker_config.go's own args
// builder, set from QueueEntry.FullSuiteSource): by the time a
// substituted command reaches this run as an ordinary -full-suite-command
// flag value, it is indistinguishable from an operator's own direct
// configuration -- the source hint is what tells this run's own
// evidence/release-decision text it was a substitution, not a fresh local
// decision, and what stops a forwarded "none" opt-out (command == "",
// nothing left to forward as a flag at all) from being silently
// re-substituted here.
//
// command is *fullSuiteCommand as resolved by flag/.factory.yml (BEFORE
// this function's own substitution); source is *fullSuiteSource as
// forwarded (possibly "", meaning no upstream hint -- the bare-CLI/
// API-started/-repository case, where this run makes the decision fresh,
// same as resolveFullSuiteCommand). verifyCommand is this run's own
// resolved canonical verify command.
func resolveEffectiveFullSuiteCommand(command, source, verifyCommand string) (effectiveCommand, effectiveSource string) {
	switch {
	case command == fullSuiteCommandNone:
		// The literal opt-out sentinel arrived as the command itself
		// (an operator's own direct -full-suite-command none, on this
		// invocation).
		return "", fullSuiteSourceNone
	case command == "" && source == fullSuiteSourceNone:
		// Already decided upstream: the request explicitly opted out,
		// and worker's own args builder correctly forwarded no
		// -full-suite-command flag at all (there is no command) alongside
		// -full-suite-source none -- do not re-substitute.
		return "", fullSuiteSourceNone
	case command == "":
		// Nothing configured anywhere and no upstream opt-out: the
		// 2026-09-24 substitution.
		return requestdriver.ResolveFullSuiteCommand("", verifyCommand)
	case source != "":
		// A non-empty command with an upstream-forwarded source: trust it
		// as-is -- already decided, possibly a substitution, by
		// submit.go/request_driver.go.
		return command, source
	default:
		// A non-empty command with no upstream hint: configured directly
		// (a CLI flag, or .factory.yml) -- not a substitution.
		return command, ""
	}
}

// runReleasePolicy converts an internal/release.MergePolicy into the
// run.ReleasePolicy shape persisted on a run record (internal/run cannot
// import internal/release -- see run.Run.ReleasePolicy's own doc
// comment). Call at run start so a later reconciliation
// (reconcileReclaimedRun, apply_run_result.go's nil-releasePolicy
// fallback) can recover the policy this run actually started with
// instead of always falling back to release.MergePolicy{}.
func runReleasePolicy(p release.MergePolicy) *run.ReleasePolicy {
	return &run.ReleasePolicy{
		ProtectedPaths:                 p.ProtectedPaths,
		MaxFilesChanged:                p.MaxFilesChanged,
		MaxInsertions:                  p.MaxInsertions,
		RollbackPlan:                   p.RollbackPlan,
		AllowOverrides:                 p.AllowOverrides,
		AllowDependencyLockfileChanges: p.AllowDependencyLockfileChanges,
		AllowUnsandboxed:               p.AllowUnsandboxed,
		RequiredGates:                  p.RequiredGates,
		AllowSkippedProjectCheck:       p.AllowSkippedProjectCheck,
	}
}

// mergePolicyFromRun is runReleasePolicy's inverse: recovers an
// internal/release.MergePolicy from a run record's persisted
// ReleasePolicy. Returns release.MergePolicy{} (the fail-closed,
// always-deny zero value) and ok=false when the run predates this field
// (a legacy record) -- callers must keep today's behavior for that case
// and log why, per run.Run.ReleasePolicy's own doc comment, rather than
// guessing a permissive policy for a run that never recorded one.
func mergePolicyFromRun(r *run.Run) (release.MergePolicy, bool) {
	if r == nil || r.ReleasePolicy == nil {
		return release.MergePolicy{}, false
	}
	p := r.ReleasePolicy
	return release.MergePolicy{
		ProtectedPaths:                 p.ProtectedPaths,
		MaxFilesChanged:                p.MaxFilesChanged,
		MaxInsertions:                  p.MaxInsertions,
		RollbackPlan:                   p.RollbackPlan,
		AllowOverrides:                 p.AllowOverrides,
		AllowDependencyLockfileChanges: p.AllowDependencyLockfileChanges,
		AllowUnsandboxed:               p.AllowUnsandboxed,
		RequiredGates:                  p.RequiredGates,
		AllowSkippedProjectCheck:       p.AllowSkippedProjectCheck,
	}, true
}

// recordReleaseDecision evaluates and durably records a release decision
// for one accepted run -- groundwork only, per
// internal/release/decision.go's own doc comment: "It performs no merge,
// push, or deploy side effect." Before this existed,
// MergePolicyCheck/EvaluateDecision/SaveDecision were correctly tested but
// had zero call sites anywhere in this repo (found via the 2026-09-03
// Opus factory-pipeline review) -- Phase 7's evidence window
// never started accumulating against real runs. Called from both the
// direct-execution path and applyRunWorkflowResult (the Temporal-result
// counterpart), so every accepted run gets one regardless of which engine
// executed it.
//
// Returns the recorded decision, or nil on any RecordDecision error -- the
// caller must treat nil the same as a denied decision (fail closed: no
// decision on file means no pull request opens) rather than falling back
// to the old "log and otherwise ignore" behavior, which used to let a
// recording failure silently leave -open-pull-request's own gate
// unenforced.
func recordReleaseDecision(dataDir, id string, r *run.Run, policy release.MergePolicy) *release.Decision {
	decision, err := release.RecordDecision(dataDir, release.ProjectOf(r), *r, policy)
	if err != nil {
		log.Printf("run %s: warning: could not record release decision: %v", id, err)
		return nil
	}
	return decision
}

// releasePullRequestWithheldReason describes, for notifyAcceptedRun's
// Reason text, why -open-pull-request did not result in a pull request
// even though the run itself was accepted: the release decision denied
// it (decision non-nil, not Allowed) or the decision could not be
// recorded at all (decision nil -- recordReleaseDecision's own fail-closed
// case). Returns "" when decision.Allowed is true, i.e. when this
// shouldn't have been called in the first place.
func releasePullRequestWithheldReason(decision *release.Decision) string {
	if decision == nil {
		return "release policy denied (decision could not be recorded)"
	}
	if decision.Allowed {
		return ""
	}
	return fmt.Sprintf("release policy denied (%s)", strings.Join(decision.Reasons, "; "))
}

// releasePolicyCanNeverAllow reports whether policy can never allow a
// release decision regardless of any run's own evidence: MergePolicyCheck
// denies unconditionally whenever RollbackPlan == "", MaxFilesChanged ==
// 0, or MaxInsertions == 0 -- and those are this repo's own documented
// -release-* flag defaults (see releasePolicyFromFlags above), so an
// operator who turns on -open-pull-request without also setting real
// values for these three would otherwise get no pull requests, ever, with
// no indication why. A pure, static check against the parsed flags -- no
// run or decision needed, which is what makes it usable at startup rather
// than only after a run has already been silently denied.
func releasePolicyCanNeverAllow(policy release.MergePolicy) bool {
	return policy.CanNeverAllow()
}

// releasePolicyNeverAllowsWarning is the loud, startup-time line
// warnIfReleasePolicyCanNeverAllow logs -- named as a const so a test can
// assert on its exact text without duplicating it.
const releasePolicyNeverAllowsWarning = "warning: -open-pull-request is set but the release policy denies by default (no -release-rollback-plan and/or -release-max-files-changed=0/-release-max-insertions=0 means every run will be denied and no pull request will ever open); set real values or PRs will never appear"

// releasePolicyWarnedOnce guards releasePolicyNeverAllowsWarning's own
// log line -- deliberately NOT a sync.Once wrapping the whole call (an
// earlier version of this fix did that at each call site): a long-lived
// `factoryd serve` process's first API-started run can easily have
// -open-pull-request false (a per-request field, not a static flag) or
// -release-* values that don't yet trigger the warning, and a bare
// sync.Once around the call would permanently consume the ONE chance to
// warn on that first, silent call -- a LATER request that does enable PR
// opening against an unchanged, still-denying daemon policy would then
// never see the warning at all (found via review, GitHub Codex App, PR
// #154). Gating the one-time state on the actual warning CONDITION,
// inside this function, instead of on merely being called, means every
// caller (worker's own startup check, and runMainWithReady's, reached
// once per drained/API-started run) can call this unconditionally and
// the warning still fires exactly once, the first time it's actually
// warranted -- never zero times because an earlier no-op call consumed
// the guard, and never more than once because of the two call sites this
// process's worker/API paths both reach.
var (
	releasePolicyWarnedMu sync.Mutex
	releasePolicyWarned   bool
)

// warnIfReleasePolicyCanNeverAllow logs releasePolicyNeverAllowsWarning,
// at most once per process, the first time openPullRequest is set and
// policy can never allow a release (see releasePolicyCanNeverAllow) --
// so turning the gate added by this same change on does not turn into a
// silent PR blackout: this repo's actual current -release-* defaults
// deny every run today, so shipping the "check the decision before
// opening a PR" fix with no accompanying warning would make every
// -open-pull-request operator's PRs stop appearing with no clue why.
func warnIfReleasePolicyCanNeverAllow(openPullRequest bool, policy release.MergePolicy) {
	if !openPullRequest || !releasePolicyCanNeverAllow(policy) {
		return
	}
	releasePolicyWarnedMu.Lock()
	alreadyWarned := releasePolicyWarned
	releasePolicyWarned = true
	releasePolicyWarnedMu.Unlock()
	if alreadyWarned {
		return
	}
	log.Println(releasePolicyNeverAllowsWarning)
}

// openEvidencePullRequest is -open-pull-request's own best-effort call
// site: push r.Branch and open a draft PR whose body is r's own evidence
// package. A push/PR-open failure here is logged and never changes this
// run's own already-recorded acceptance, matching the same best-effort
// contract internal/notify's own Discord dispatch already follows for a
// quarantine notification.
//
// Returns the opened PR's URL, or "" when no PR was opened (no branch, or
// an opener failure) -- it does not persist anything onto the durable run
// record itself; recordAcceptedRunSideEffects is the one place that
// happens, for both this and notifyAcceptedRun's result together (see its
// own doc comment for why: this function used to also re-save the run
// record under a fresh run.WithLock, which deadlocks when called from
// applyRunWorkflowResult's own already-locked callers -- found via Codex
// review of PR #86).
//
// opener is forge.GHPullRequestOpener{} at this function's one real call
// site; injectable so a test can point it at fake git/gh executables
// instead of needing a real GitHub remote (this codebase's own
// established shape for every other real external dependency --
// RunStarter, ProjectChecker, DaemonController -- see internal/api's own
// doc comments for that same reasoning).
//
// policy is the same release.MergePolicy the caller already built for
// recordReleaseDecision -- passed through so renderEvidenceMarkdown's risk
// header can check protected-path membership via
// policy.ProtectedFilesTouched without re-deriving or re-loading anything.
// nil exactly when the caller has no such policy to give (see
// renderRiskHeader's own doc comment on reconcileReclaimedRun).
//
// dataDir is where a memory request's proposal is read from: such a run's
// body ends with the lines the change adds and removes
// (memoryChangesMarkdown).
func openEvidencePullRequest(dataDir, execDir, id string, r *run.Run, policy *release.MergePolicy, opener forge.PullRequestOpener) string {
	if r.Branch == "" {
		log.Printf("run %s: -open-pull-request set but no isolated branch exists, skipping", id)
		return ""
	}
	body := renderEvidenceMarkdown(r, policy) + memoryChangesMarkdown(dataDir, r)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	// r.ResultSHA pinned into an explicit push refspec, not a bare branch
	// push -- applied here too, not just retryPullRequestOpener's own
	// call, since it's
	// strictly safer for this path as well: pushing exactly the commit
	// the evidence body above was just rendered from, not whatever the
	// branch ref happens to point at the instant this push actually runs.
	url, err := opener.OpenDraftPullRequest(ctx, execDir, r.Branch, r.PRBase, r.ResultSHA, pullRequestTitle(r, id), body)
	if err != nil {
		log.Printf("run %s: -open-pull-request: could not push/open a pull request (run remains accepted): %v", id, err)
		msg, _, _ := strings.Cut(err.Error(), "\n")
		r.PROpenError = msg
		return ""
	}
	fmt.Printf("run %s: opened draft pull request %s\n", id, url)
	return url
}

// retryPullRequestOpener is request.PROpener's one real implementation.
// request.Retry calls it for a ticket halted with request.HaltAcceptedNoPR -- its run
// was accepted (a real, paid build already succeeded) but no PR exists
// for it -- to re-attempt ONLY the push/PR-open, against that run's own
// already-pushed branch and workspace; the build runner is never invoked.
//
// Re-runs the release decision exactly the way the original accept-time
// open did (release.RecordDecision, then only open when Allowed): against
// mergePolicyFromRun(loaded), the policy this run was actually accepted
// under (runReleasePolicy pins it onto the run record at start), not
// whatever -release-* flags this process happens to be running with now
// -- so a run that would be denied under its own recorded policy stays
// denied here, and CLAIMS.md's "re-checked immediately before each side
// effect" rule for the forge/PR-review surface holds for this path too,
// not just the original one. A genuine denial reports WithheldReason,
// never Err -- request.Retry falls back to rebuilding the ticket under
// today's policy for that case (see its own doc comment); RecordDecision's own
// error (a transient/infra failure: lock, load, evaluate) is NOT the
// same thing and must never be reported as a denial, since retrying the
// exact same way again might simply succeed.
//
// Idempotent (the same review): a PR already recorded on the run
// (loaded.PullRequestURL, from an earlier successful attempt whose own
// request-side save then failed or crashed) is linked with no gh call
// and no second "accepted" notification; and when gh itself reports the
// PR already exists (the same crash window, but for THIS attempt's own
// push/create instead of an earlier one), the URL embedded in gh's own
// error text is verified (round 2 of the same review -- open,
// same-repository, pointing at ResultSHA -- not just trusted outright)
// before being linked the same way, rather than refusing forever.
//
// Refuses to push a branch whose real ref no longer matches the run's
// own ResultSHA (sharpened by a round-2 review: checks
// refs/heads/<branch> directly, not just the worktree's HEAD, and an
// empty ResultSHA now refuses rather than silently skipping the check --
// see forge.branchTip's own doc comment) -- the release
// decision and evidence above were computed against ResultSHA
// specifically, and a branch can drift after acceptance (a corrective
// round, a maintainer's own push, a reused/reclaimed worktree) --
// pushing whatever it points at NOW would land unevaluated code on the
// PR. openEvidencePullRequest itself additionally pins ResultSHA into an
// explicit push refspec (forge.PullRequestOpener.OpenDraftPullRequest),
// closing the remaining check-then-push race this alone can only narrow.
//
// forge.pullRequestOpener is the forge.PullRequestOpener this
// pushes/opens/verifies through, and forge.branchTip
// reads the branch's own real ref -- both boundary methods, not hardcoded
// literals, so a test can substitute fakes without a real git/gh binary
// (mirrors this package's other externally-shelling-out package vars,
// e.g. forge.replyToReviewComment, forge.gitToplevel).
func (impl realForge) pullRequestOpener() forge.PullRequestOpener { return forge.GHPullRequestOpener{} }

// branchTip reads refs/heads/<branch>'s own real
// commit -- not HEAD: checking the worktree's
// symbolic HEAD instead of the branch ref it resolves through leaves a
// narrower but real gap open (HEAD could in principle be detached, or
// something could repoint it, without moving the branch ref itself, or
// vice versa) -- reading the ref this push will actually update is the
// one value that has to match ResultSHA for the push below to be safe.
func (impl realForge) branchTip(ctx context.Context, workspaceDir, branch string) (string, error) {
	out, err := exec.CommandContext(ctx, "git", "-C", workspaceDir, "rev-parse", "refs/heads/"+branch).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func retryPullRequestOpener(dp *deps, dataDir, runID string) request.PROpenOutcome {
	loaded, err := run.Load(dataDir, runID)
	if err != nil {
		return request.PROpenOutcome{Err: fmt.Errorf("load run %s: %w", runID, err)}
	}
	// Idempotent, as above: already-recorded, no gh call, no duplicate
	// notification.
	if loaded.PullRequestURL != "" {
		return request.PROpenOutcome{PRURL: loaded.PullRequestURL}
	}
	if loaded.State != run.StateAccepted {
		return request.PROpenOutcome{Err: fmt.Errorf("run %s is not accepted (state %q); nothing to open a pull request for", runID, loaded.State)}
	}
	if loaded.Branch == "" {
		return request.PROpenOutcome{Err: fmt.Errorf("run %s has no isolated branch to push (was it run on a worktree branch?)", runID)}
	}
	// A round-2 review found an empty ResultSHA used to skip this check
	// entirely -- refuse instead, the same "rebuild-recoverable" shape as
	// a real policy denial (request.Retry's own same-ticket rule for a
	// release-DENIED run applies here too).
	if loaded.ResultSHA == "" {
		return request.PROpenOutcome{WithheldReason: fmt.Sprintf("run %s has no recorded ResultSHA, so its branch tip can't be verified safe to push", runID)}
	}
	tipCtx, tipCancel := context.WithTimeout(context.Background(), 30*time.Second)
	tip, tipErr := dp.forge.branchTip(tipCtx, loaded.WorkspacePath, loaded.Branch)
	tipCancel()
	if tipErr != nil {
		return request.PROpenOutcome{Err: fmt.Errorf("read refs/heads/%s of run %s's own worktree %s: %w", loaded.Branch, runID, loaded.WorkspacePath, tipErr)}
	}
	if tip != loaded.ResultSHA {
		return request.PROpenOutcome{Err: fmt.Errorf("run %s's branch %s is now at %s, but its recorded ResultSHA -- what the release decision and evidence were computed from -- is %s; refusing to push a branch that moved since acceptance. Start a fresh build (`factoryd retry` on a ticket with no accepted run yet) rather than retrying this run", runID, loaded.Branch, tip, loaded.ResultSHA)}
	}
	policy, ok := mergePolicyFromRun(loaded)
	if !ok {
		return request.PROpenOutcome{Err: fmt.Errorf("run %s predates recorded release policy; retry cannot re-evaluate a release decision for it", runID)}
	}
	ensureMemoryEdit(dp, dataDir, runID, loaded)
	decision, recErr := release.RecordDecision(dataDir, release.ProjectOf(loaded), *loaded, policy)
	if recErr != nil {
		return request.PROpenOutcome{Err: fmt.Errorf("record release decision for run %s: %w", runID, recErr)}
	}
	if !decision.Allowed {
		return request.PROpenOutcome{WithheldReason: releasePullRequestWithheldReason(decision)}
	}
	prURL := openEvidencePullRequest(dataDir, loaded.WorkspacePath, runID, loaded, &policy, dp.forge.pullRequestOpener())
	if prURL == "" {
		// The idempotency case's other half, sharpened by a round-2
		// review: gh reporting the PR already exists is not a real
		// failure -- but the URL it names must be VERIFIED (open, same-repository,
		// pointing at exactly ResultSHA) before ever being linked, not
		// trusted outright.
		recovered, recoverErr := recoverExistingPullRequest(dp, dataDir, runID, loaded)
		switch {
		case recoverErr == nil && recovered != "":
			return request.PROpenOutcome{PRURL: recovered}
		case recoverErr != nil:
			// recoverExistingPullRequest found a URL in gh's own error
			// text but rejected linking it (closed, cross-repository, or
			// the wrong commit) -- this reason is more specific than the
			// raw gh error below, so it replaces loaded.PROpenError on
			// disk rather than being silently discarded.
			if lockErr := run.WithLock(dataDir, runID, func() error {
				fresh, loadErr := run.Load(dataDir, runID)
				if loadErr != nil {
					return loadErr
				}
				fresh.PROpenError = recoverErr.Error()
				return fresh.Persist(dataDir)
			}); lockErr != nil {
				log.Printf("run %s: record retry PR-open error: %v", runID, lockErr)
			}
			return request.PROpenOutcome{Err: recoverErr}
		}
		// recoverErr == nil && recovered == "": gh's error named no URL
		// to recover at all (not an "already exists" shape) -- fall
		// through to the generic handling below.
		//
		// openEvidencePullRequest already set loaded.PROpenError in memory;
		// persist it the same way a failed accept-time open does, against a
		// freshly loaded copy (recordAcceptedRunSideEffects' own
		// underLock=false shape -- this function holds no lock on runID).
		if lockErr := run.WithLock(dataDir, runID, func() error {
			fresh, loadErr := run.Load(dataDir, runID)
			if loadErr != nil {
				return loadErr
			}
			fresh.PROpenError = loaded.PROpenError
			return fresh.Persist(dataDir)
		}); lockErr != nil {
			log.Printf("run %s: record retry PR-open error: %v", runID, lockErr)
		}
		return request.PROpenOutcome{Err: errors.New(loaded.PROpenError)}
	}
	n := notifyAcceptedRun(dataDir, runID, loaded.Ticket, loaded.RequestID, prURL, "")
	recordAcceptedRunSideEffects(dataDir, loaded, prURL, n, false)
	return request.PROpenOutcome{PRURL: prURL}
}

// recoverExistingPullRequest is retryPullRequestOpener's own "gh says
// this PR already exists" recovery path: extracts the URL gh's own error
// text names (forge.
// OpenDraftPullRequestErrorURL), then verifies it via
// forge.PullRequestOpener.VerifyExistingPullRequest before linking --
// gh's own `pr view <branch>` (this function's own pre-round-2 shape)
// could match a PR that's since been closed, merged, or belongs to a
// same-named branch on a fork, none of which are safe to silently treat
// as "the retry succeeded".
//
// Three distinct outcomes, not two: ("", nil) means there was no URL to
// recover at all (gh's error wasn't an "already exists" shape, or a
// future gh version's wording changed) -- the caller's own generic
// PROpenError handling applies, unchanged. (url, nil) means a URL was
// found AND verified safe to link: OPEN, not cross-repository, and its
// own headRefOid matches loaded.ResultSHA exactly. ("", a non-nil error)
// means a URL was found but rejected -- the caller persists THIS
// specific reason in place of the raw gh error, rather than the generic
// path silently discarding it.
func recoverExistingPullRequest(dp *deps, dataDir, runID string, loaded *run.Run) (string, error) {
	url := forge.OpenDraftPullRequestErrorURL(loaded.PROpenError)
	if url == "" {
		return "", nil
	}
	verifyCtx, verifyCancel := context.WithTimeout(context.Background(), 30*time.Second)
	state, headRefOid, isCrossRepository, verifyErr := dp.forge.pullRequestOpener().VerifyExistingPullRequest(verifyCtx, loaded.WorkspacePath, url)
	verifyCancel()
	if verifyErr != nil {
		return "", fmt.Errorf("verify recovered pull request %s: %w", url, verifyErr)
	}
	switch {
	case isCrossRepository:
		return "", fmt.Errorf("recovered pull request %s is cross-repository (a fork's own branch of the same name), refusing to link it", url)
	case !strings.EqualFold(state, "OPEN"):
		return "", fmt.Errorf("recovered pull request %s is %s, not OPEN, refusing to link it", url, state)
	case headRefOid != loaded.ResultSHA:
		return "", fmt.Errorf("recovered pull request %s's own head %s does not match run %s's recorded ResultSHA %s, refusing to link it", url, headRefOid, runID, loaded.ResultSHA)
	}
	if lockErr := run.WithLock(dataDir, runID, func() error {
		fresh, loadErr := run.Load(dataDir, runID)
		if loadErr != nil {
			return loadErr
		}
		fresh.PullRequestURL = url
		fresh.PROpenError = ""
		return fresh.Persist(dataDir)
	}); lockErr != nil {
		log.Printf("run %s: record retry's recovered pull request URL: %v", runID, lockErr)
	}
	// No second "accepted" notification: this PR already existed, it
	// just wasn't linked on this run record yet.
	return url, nil
}

// pullRequestTitle is openEvidencePullRequest's own title: a review found
// the old, unconditional
// `ticket(<ticket>): evidence-backed run <id>` told an operator scanning
// their PR list nothing about what any given PR actually does. Prefers
// ticketspec.GoalTitle(r.SpecPath) -- the ticket's own "## Goal" paragraph,
// sanitized and capped -- falling back to the old generic form whenever
// that's empty (unreadable spec, no Goal section, or a run that predates
// SpecPath). The run id still appears, in the body (renderEvidenceMarkdown's
// own "for run `%s`" line), never dropped -- just no longer squeezed into
// the one line a PR list actually shows.
func pullRequestTitle(r *run.Run, id string) string {
	if title := ticketspec.GoalTitle(r.SpecPath); title != "" {
		return title
	}
	return fmt.Sprintf("ticket(%s): evidence-backed run %s", r.Ticket, id)
}

// notifyAcceptedRun best-effort builds and dispatches an accepted run's
// completion notification -- the durable LogNotifier append, then a
// best-effort Discord page, mirroring the existing quarantine notification
// shape (see notify.PrepareHalt's own doc comment for why the durable
// local record and the network paging channel are kept separate). prURL is
// "" when -open-pull-request wasn't requested or didn't succeed.
//
// Returns the built Notification for the caller to append onto the
// durable run record (see recordAcceptedRunSideEffects) rather than doing
// that here itself: two of this function's three effective call sites
// (applyRunWorkflowResult, invoked from runViaRepositoryOwner and
// reconcileReclaimedRun) already run inside a caller-held run.WithLock on
// this same run id, and run.WithLock is not reentrant within one process
// -- flock blocks a second acquisition on a new file descriptor even for
// the same id, from the very same process, so taking the lock again in
// here would deadlock (found via Codex review of PR #86, on an earlier
// version of this function that did exactly that).
//
// prWithheldReason is "" unless -open-pull-request was requested but no
// PR was even attempted because the release decision denied it (see
// releasePullRequestWithheldReason) -- distinct from an opener that was
// attempted and failed (also prURL == "", but prWithheldReason == "" in
// that case), so a human reading this Reason later can tell "fix the
// policy flags" apart from "check gh/git creds, maybe retry".
func notifyAcceptedRun(dataDir, id, ticket, requestID, prURL, prWithheldReason string) notify.Notification {
	reason := "run accepted"
	switch {
	case prURL != "":
		reason = fmt.Sprintf("run accepted; pull request: %s", prURL)
	case prWithheldReason != "":
		reason = fmt.Sprintf("run accepted; pull request withheld: %s", prWithheldReason)
	}
	// Next is the pull request when one was opened -- the operator's
	// actual next action -- else a hint naming the run, since there is
	// nothing else actionable to point at.
	next := prURL
	if next == "" {
		next = fmt.Sprintf("factoryd status (run %s)", id)
	}
	delivered := true
	n := notify.Notification{
		RunID:     id,
		Ticket:    ticket,
		Reason:    reason,
		State:     run.StateAccepted,
		SentAt:    time.Now().Format(time.RFC3339),
		Delivered: &delivered,
		RunDir:    run.AbsDir(dataDir, id),
		Next:      next,
		Link:      consoleRunURL(resolveConsoleBaseURL("", dataDir), id),
	}
	notifyCtx, cancelNotify := context.WithTimeout(context.Background(), 2*time.Second)
	notifyErr := (notify.LogNotifier{Path: filepath.Join(run.Dir(dataDir, id), "notifications.log")}).Notify(notifyCtx, n)
	cancelNotify()
	if notifyErr != nil {
		log.Printf("run %s: append accepted notification: %v", id, notifyErr)
		delivered = false
		n.DeliveryError = notifyErr.Error()
	}
	// DispatchExternal itself runs its fan-out on a background goroutine
	// and returns immediately -- notify.Notifier's own contract requires
	// network-I/O dispatch to happen asynchronously from run completion,
	// and its three channels each carry their own 5s timeout, so even run
	// concurrently the fan-out is up to 5s were this call inline. main() calls
	// notify.WaitForPendingDispatches before this process exits, so an
	// exit shortly after this call still gives the goroutine a bounded
	// chance to actually reach the network.
	dispatchRunNotification(dataDir, requestID, n)
	return n
}

// dispatchRunNotification sends a run's own notification (halted,
// quarantined, accepted) to the desktop, Slack and Discord, unless the run
// is a ticket of request requestID. That run's record and notifications.log
// keep the notification either way; what reaches the operator is the
// request's own, sent when the request starts waiting on them, with the
// request's page as its link: one notification for one event, and none for
// an outcome the factory goes on from by itself (a corrective round).
func dispatchRunNotification(dataDir, requestID string, n notify.Notification) {
	if requestID != "" {
		return
	}
	notify.DispatchExternal(dataDir, n)
}

// recordAcceptedRunSideEffects persists prURL (when non-empty) and n --
// openEvidencePullRequest's and notifyAcceptedRun's own results -- onto
// id's durable run record. Neither of those two functions may take
// run.WithLock itself (see notifyAcceptedRun's own doc comment on why),
// so this is the one place both land on disk, and underLock says which of
// two mutually exclusive ways to do that is safe at this call site:
//
// underLock true means the caller already holds id's run.WithLock for
// this call's entire duration (applyRunWorkflowResult's own
// runViaRepositoryOwner/reconcileReclaimedRun callers, both of which load
// r fresh and pass it in before ever releasing that lock) -- no other
// writer can touch run.json meanwhile, so this mutates r in place and
// saves it directly; acquiring the lock again here would deadlock.
//
// underLock false means no such lock is held (run_ticket.go's own call,
// and applyRunWorkflowResult's runViaTemporal-routed call) -- r may
// already be stale relative to a concurrent operator override that landed
// after this run's own terminal save, so this acquires its own lock
// against a freshly loaded copy rather than r itself, the same protective
// shape openEvidencePullRequest used to follow on its own before this
// split.
func recordAcceptedRunSideEffects(dataDir string, r *run.Run, prURL string, n notify.Notification, underLock bool) {
	apply := func(target *run.Run) {
		if prURL != "" {
			target.PullRequestURL = prURL
		} else if r.PROpenError != "" {
			target.PROpenError = r.PROpenError
		}
		target.Notifications = append(target.Notifications, n)
	}
	if underLock {
		apply(r)
		if err := save(r, dataDir); err != nil {
			log.Printf("run %s: record accepted run's pull request/notification: %v", r.ID, err)
		}
		return
	}
	if lockErr := run.WithLock(dataDir, r.ID, func() error {
		loaded, loadErr := run.Load(dataDir, r.ID)
		if loadErr != nil {
			return loadErr
		}
		apply(loaded)
		return loaded.Persist(dataDir)
	}); lockErr != nil {
		log.Printf("run %s: record accepted run's pull request/notification: %v", r.ID, lockErr)
	}
}

// writeRepoGateLines lists the repository's own gates (.factory.yml
// `gates:`) among results, in the order they ran. One that is not listed did
// not run: there is no fixed set to report "not configured" against.
func writeRepoGateLines(b *strings.Builder, results []run.GateResult) {
	for _, g := range results {
		if !gatepolicy.IsRepoGate(g.Check) {
			continue
		}
		status := "pass"
		if !g.Passed {
			status = "FAIL"
		}
		fmt.Fprintf(b, "- `%s`: %s\n", sanitizeMarkdownField(g.Check), status)
	}
}

// renderEvidenceMarkdown renders r's own already-durable evidence as the
// body of the draft PR -open-pull-request opens -- the exact
// differentiator that sets this project apart from its peers: every peer
// this project was compared against lands a PR, none attach independently
// computed evidence.
//
// Every field interpolated below except r.ID/BaseSHA/ResultSHA (factory- or
// git-SHA-shaped, never free text) is passed through sanitizeMarkdownField
// first. ChangedFiles and DependencyChanges in particular are not safe to
// embed raw: GitDiffNameOnly and GitStatusPaths (internal/runner/runner.go)
// deliberately return `-z` (unquoted, unescaped) paths so a filename
// containing a backtick or newline still matches correctly for scope
// checks -- but that same raw byte content, written by an agent this
// factory treats as untrusted, would otherwise land verbatim in a public
// GitHub PR body: a backtick breaks out of the code span, a newline
// injects an arbitrary extra markdown line. Found in adversarial review,
// 2026-09-08.
//
// policy is needed only for the risk header below (see renderRiskHeader),
// to check r.ChangedFiles against policy.ProtectedFilesTouched; nothing
// else here reads it.
func renderEvidenceMarkdown(r *run.Run, policy *release.MergePolicy) string {
	var b strings.Builder
	b.WriteString(renderRiskHeader(r, policy))
	b.WriteString("\n")
	fmt.Fprintf(&b, "Opened automatically by `factoryd -open-pull-request` for run `%s`.\n\n", r.ID)
	fmt.Fprintf(&b, "**Base:** `%s`  **Result:** `%s`\n\n", r.BaseSHA, r.ResultSHA)
	if r.DiffStat != nil {
		fmt.Fprintf(&b, "**Diff:** %d file(s), +%d/-%d\n\n", r.DiffStat.FilesChanged, r.DiffStat.Insertions, r.DiffStat.Deletions)
	}
	if len(r.GateResults) > 0 {
		b.WriteString("## Gates\n\n")
		for _, g := range r.GateResults {
			status := "pass"
			if !g.Passed {
				status = "FAIL"
			}
			fmt.Fprintf(&b, "- `%s`: %s (exit %d, %dms)\n", sanitizeMarkdownField(g.Check), status, g.ExitCode, g.DurationMs)
		}
		b.WriteString("\n")
	}
	// FullSuiteSource makes the operator-approved verify-command
	// substitution visible on the PR itself, next to the
	// full_suite_verify gate result just
	// rendered above -- the gate ran and is reported like any other; this
	// line only explains what command it actually ran, for a reviewer who
	// would otherwise assume a separately-configured full-suite command
	// exists.
	if r.FullSuiteSource == fullSuiteSourceVerifyCommand {
		b.WriteString("full suite = verify command (no separate full_suite_command configured)\n\n")
	}
	// tests_added's opt-out reason gets its own line rather than
	// living only in the gate's pass/fail bit above -- a reviewer seeing
	// "tests_added: pass" with no changed test file otherwise has no way
	// to tell "a real test file satisfied this" apart from "the ticket
	// opted out and why".
	writeOracleOptOutLine(&b, r)
	if r.TestsRequiredOptOut != "" {
		fmt.Fprintf(&b, "`tests_added` opted out: %s\n\n", sanitizeMarkdownField(r.TestsRequiredOptOut))
	}
	// Per-criterion spec-conformity verdicts, when
	// -spec-acceptance-criteria was given. Purely informational -- see
	// run.AgentEvidence.ReviewVerdicts' own doc comment for why this is
	// never itself a gate: the actual required/advisory/degraded
	// enforcement already happened inside conformity_review.py, reflected
	// here only through the run's own accept/quarantine outcome.
	//
	// Reads r.SpecConformityVerdicts, the independent phase-2 reviewer's
	// output. AgentEvidence.ReviewVerdicts is only the fallback for a run
	// recorded before that field existed: since the phase-2 split
	// build_app.py no longer receives the criteria, so that legacy field is
	// empty for every current run and this section never rendered at all.
	writeSpecConformitySection(&b, r)
	b.WriteString(renderCodeReviewMarkdown(r.CodeReview))
	// Every policy.CommandGate always gets its own line, even when the
	// project never configured one -- "not configured" is a real,
	// distinct state a reviewer needs to see, not silence the way an
	// undeclared ticket-scoped gate (diff_scope, required_files_changed,
	// required_content_present) is: those have no "the operator should
	// have configured this" implication, these project-scoped ones do.
	writeNamedGatesSection(&b, r)
	writeChangedFilesAndDependencies(&b, r)
	if totalCostMicroUSD, totalTokens, partial := relaySpendSummary(r.Attempts); totalCostMicroUSD > 0 || totalTokens > 0 || partial {
		fmt.Fprintf(&b, "**Relay spend:** %s\n\n", relaySpendLine(totalCostMicroUSD, totalTokens, partial, run.SubscriptionBilled(r.Attempts)))
	}
	fmt.Fprintf(&b, "Full evidence: `GET /runs/%s/release` (release decision) and `GET /runs/%s/diff` (unified diff), or the durable run record on disk.\n", r.ID, r.ID)
	// r.PRCloses is a fully-qualified "<owner>/<repo>#<N>" GitHub issue
	// reference built by resolveSubmitRequestText from -issue's own URL
	// (factoryd submit -issue), never free text an agent or ticket author
	// controls -- unlike ChangedFiles/DependencyChanges above, it needs no
	// sanitizeMarkdownField pass before interpolation. "Closes
	// <owner>/<repo>#<N>" is GitHub's own auto-close convention: merging
	// this PR closes that issue. Always qualified, never a bare "#<N>",
	// since the issue and this run's own repository are independent
	// inputs that may differ (see run.Run.PRCloses's own doc comment).
	if r.PRCloses != "" {
		fmt.Fprintf(&b, "\nCloses %s\n", r.PRCloses)
	}
	return b.String()
}

// writeOracleOptOutLine says what became of a `-no-commit-oracles` request.
func writeOracleOptOutLine(b *strings.Builder, r *run.Run) {
	if r.OraclesNotCommittedByRequest {
		switch {
		case r.Oracles != nil && len(r.Oracles.Authored) > 0:
			b.WriteString("Oracle commit opt-out requested (`-no-commit-oracles`) but oracles WERE committed by the factory: the worker that ran this did not honour it (mixed-version deployment).\n\n")
		default:
			oracleGated := false
			for _, g := range r.GateResults {
				if g.Check == "reference_oracle" && g.Passed {
					oracleGated = true
				}
			}
			if oracleGated {
				b.WriteString("Oracle commit opt-out requested (`-no-commit-oracles`; honoured on single-binary deploys): the oracle gated this run and no factory oracle commit is recorded.\n\n")
			} else {
				b.WriteString("Oracle commit opt-out requested (`-no-commit-oracles`; honoured on single-binary deploys); no reference oracle ran.\n\n")
			}
		}
	}
}

// writeSpecConformitySection lists each criterion's verdict and whether the
// reference oracle agrees.
func writeSpecConformitySection(b *strings.Builder, r *run.Run) {
	verdicts := r.SpecConformityVerdicts
	if len(verdicts) == 0 && r.AgentEvidence != nil {
		verdicts = r.AgentEvidence.ReviewVerdicts
	}
	if len(verdicts) > 0 {
		b.WriteString("## Spec conformity\n\n")
		// Cross-check against the reference oracle: for criteria a
		// deterministic oracle also covers, say whether the LLM reviewer and the oracle
		// agree -- a disagreement is the drift signal a human should read.
		// Still informational only; neither input is a gate here.
		oraclePassed := conformity.OracleOutcome(r.GateResults)
		disagreements := 0
		for _, c := range conformity.CrossCheck(verdicts, r.OracleCoveredCriteria, oraclePassed) {
			fmt.Fprintf(b, "- %s: **%s**", sanitizeMarkdownField(c.Criterion), sanitizeMarkdownField(c.Verdict))
			switch c.Agreement {
			case conformity.AgreementAgree:
				b.WriteString(" (oracle-checked, agrees)")
			case conformity.AgreementDisagree:
				disagreements++
				b.WriteString(" (oracle-checked, **DISAGREES with the reference oracle**)")
			case conformity.AgreementOracleNotRun:
				b.WriteString(" (oracle-covered, oracle did not run)")
			case conformity.AgreementOracleFailed:
				b.WriteString(" (oracle-covered, the reference oracle failed)")
			case conformity.AgreementReviewerUnavailable:
				b.WriteString(" (oracle-covered, reviewer produced no verdict)")
			}
			b.WriteString("\n")
		}
		if disagreements > 0 {
			fmt.Fprintf(b, "\n%d criterion verdict(s) disagree with the reference oracle -- the reviewer and the deterministic check reached opposite conclusions, so the prose criterion and the oracle may have drifted apart. Read both before merging.\n", disagreements)
		}
		b.WriteString("\n")
	}
}

// writeNamedGatesSection lists every command gate and the repository's own
// gates.
func writeNamedGatesSection(b *strings.Builder, r *run.Run) {
	b.WriteString("## Named gates\n\n")
	namedGateResults := make(map[string]run.GateResult, len(r.GateResults))
	for _, g := range r.GateResults {
		namedGateResults[g.Check] = g
	}
	for _, g := range gatepolicy.CommandGates {
		check := g.ID
		gateResult, ran := namedGateResults[check]
		status := "not configured"
		if ran {
			status = "pass"
			if !gateResult.Passed {
				status = "FAIL"
			}
		}
		fmt.Fprintf(b, "- `%s`: %s\n", check, status)
		// The reference-oracle content hash (SC-012, PR #151 review
		// round-2 follow-up) only exists when -reference-oracle-dir was
		// configured -- empty otherwise, including for every other named
		// gate, so this line is added only when there's something real
		// to show.
		if check == gatepolicy.ReferenceOracleGateID && gateResult.ReferenceOracleSHA256 != "" {
			fmt.Fprintf(b, "  - reference-oracle content: `sha256:%s`\n", gateResult.ReferenceOracleSHA256)
		}
	}
	writeRepoGateLines(b, r.GateResults)
	b.WriteString("\n")
}

// writeChangedFilesAndDependencies lists the changed files and the dependency
// changes.
func writeChangedFilesAndDependencies(b *strings.Builder, r *run.Run) {
	if len(r.ChangedFiles) > 0 {
		fmt.Fprintf(b, "## Changed files (%d)\n\n", len(r.ChangedFiles))
		for _, f := range r.ChangedFiles {
			fmt.Fprintf(b, "- `%s`\n", sanitizeMarkdownField(f))
		}
		b.WriteString("\n")
	}
	if len(r.DependencyChanges) > 0 {
		b.WriteString("## Dependency changes\n\n")
		for _, d := range r.DependencyChanges {
			fmt.Fprintf(b, "- `%s`: %s -> %s\n", sanitizeMarkdownField(d.Name), sanitizeMarkdownField(d.Base), sanitizeMarkdownField(d.Result))
		}
		b.WriteString("\n")
	}
}

// insertionsLowThreshold is the fixed cutoff riskLabel uses to call a diff
// "small". Chosen as a starting guess, not a measurement; revisit once
// real PR sizes are known.
const insertionsLowThreshold = 200

// riskLabel produces a one-word triage label from
// simple, fixed thresholds, never a model's judgment. HIGH whenever either
// signal a human reviewer specifically wants flagged is present (a
// protected path touched, a dependency lockfile changed, or the compose
// file changed -- a new sidecar this run never launched) -- each
// overrides diff size and an unknown protected/dependency status.
// LOW requires every input to actually be known, and favorable: a recorded
// diff stat under insertionsLowThreshold, protected-path status evaluated
// (not touched), and dependency evidence collected (none touched). Any one
// of those being unrecorded/unevaluated can't be shown to be favorable, so
// that falls to MEDIUM rather than a false LOW. Everything else is MEDIUM.
func riskLabel(haveDiffStat bool, insertions int, protectedTouched, protectedKnown bool, dependencyLockfilesChanged int, dependencyKnown, composeFileChanged bool) string {
	if protectedTouched || dependencyLockfilesChanged > 0 || composeFileChanged {
		return "HIGH"
	}
	if haveDiffStat && insertions < insertionsLowThreshold && protectedKnown && dependencyKnown {
		return "LOW"
	}
	return "MEDIUM"
}

// renderRiskHeader implements Vercel Foreman's risk assessment before
// human merge: a fixed-format triage summary at the very top of the PR
// body, computed only from r's own durable evidence and the
// same release.MergePolicy the run was evaluated against -- never from
// agent-authored prose -- so a reviewer can triage in seconds without
// reading the full evidence package below it. Every value here is a count
// or a fixed enum word, never a raw string sourced from the diff or the
// agent, so none of it needs sanitizeMarkdownField the way
// ChangedFiles/DependencyChanges below do.
//
// policy is nil only for a reclaimed legacy run whose record predates
// run.Run.ReleasePolicy (applyRunWorkflowResult otherwise passes the policy
// it recovered from the run record). Such a run is
// evaluated against the always-deny zero policy, so in practice this never
// reaches a PR; a nil policy is still rendered as its own distinct "not
// evaluated" line, never as a false "not touched".
func renderRiskHeader(r *run.Run, policy *release.MergePolicy) string {
	haveDiffStat := r.DiffStat != nil
	diffLine := "not recorded"
	insertions := 0
	if haveDiffStat {
		insertions = r.DiffStat.Insertions
		diffLine = fmt.Sprintf("%d file(s), +%d/-%d", r.DiffStat.FilesChanged, r.DiffStat.Insertions, r.DiffStat.Deletions)
	}

	var protectedLine string
	protectedKnown := policy != nil && r.ChangedFiles != nil
	protectedCount := 0
	switch {
	case policy == nil:
		protectedLine = "not evaluated (policy not retained for a reclaimed run)"
	case r.ChangedFiles == nil:
		protectedLine = "not recorded"
	default:
		protectedCount = len(policy.ProtectedFilesTouchedByRun(*r))
		protectedLine = "not touched"
		if protectedCount > 0 {
			protectedLine = fmt.Sprintf("touched (%d)", protectedCount)
		}
	}

	dependencyKnown := r.DependencyLockfilesTouched != nil
	dependencyCount := len(r.DependencyLockfilesTouched)
	dependencyLine := "not recorded"
	if dependencyKnown {
		dependencyLine = "none"
		if dependencyCount > 0 {
			dependencyLine = fmt.Sprintf("%d lockfile(s) changed", dependencyCount)
		}
	}

	totalCostMicroUSD, totalTokens, partial := relaySpendSummary(r.Attempts)
	costLine := "not recorded"
	if totalCostMicroUSD > 0 || totalTokens > 0 || partial {
		costLine = relaySpendLine(totalCostMicroUSD, totalTokens, partial, run.SubscriptionBilled(r.Attempts))
	}

	var b strings.Builder
	fmt.Fprintf(&b, "**Risk: %s**\n", riskLabel(haveDiffStat, insertions, protectedCount > 0, protectedKnown, dependencyCount, dependencyKnown, len(r.ComposeFilesChanged) > 0))
	fmt.Fprintf(&b, "- Diff: %s\n", diffLine)
	fmt.Fprintf(&b, "- Protected paths: %s\n", protectedLine)
	fmt.Fprintf(&b, "- Dependency changes: %s\n", dependencyLine)
	// Compose lines are omitted for a run with neither, like most runs.
	// Service names come from the target repo, so each is a sanitized code
	// span, unlike the counts and fixed words above.
	baseServices := "none"
	if len(r.ComposeServices) > 0 {
		names := make([]string, len(r.ComposeServices))
		for i, name := range r.ComposeServices {
			names[i] = "`" + sanitizeMarkdownField(name) + "`"
		}
		baseServices = strings.Join(names, ", ")
		fmt.Fprintf(&b, "- Compose services: %s (launched from the base commit)\n", baseServices)
	}
	if len(r.ComposeFilesChanged) > 0 {
		// A worker's compose edit is never launched in the run that made
		// it (it must not shape its own containment), so verify ran
		// against the base commit's services only.
		fmt.Fprintf(&b, "- Compose file: changed (%s), not launched this run (base services: %s)\n", strings.Join(r.ComposeFilesChanged, ", "), baseServices)
	}
	fmt.Fprintf(&b, "- Cost: %s\n", costLine)
	return b.String()
}

// recordComposeEvidence sets r.ComposeServices from the build phase's
// compose services report and r.ComposeFilesChanged from r.ChangedFiles;
// both feed renderRiskHeader. Shared by the direct and Temporal paths. The
// root .env counts once services were launched: the loader interpolates the
// compose file with it (sandbox.LoadComposeServicesSpecFromGit), so an edit
// there reshapes the next run's sidecars just as a compose edit would.
func recordComposeEvidence(r *run.Run, dataDir string) {
	r.ComposeServices = sandbox.ComposeServicesLaunched(dataDir, r.ID, "build")
	r.ComposeFilesChanged = nil
	for _, path := range r.ChangedFiles {
		if slices.Contains(sandbox.ComposeServicesFileNames, path) || (path == ".env" && len(r.ComposeServices) > 0) {
			r.ComposeFilesChanged = append(r.ComposeFilesChanged, path)
		}
	}
}

// sanitizeMarkdownField makes s safe to embed inside a single backtick-
// fenced markdown code span. It replaces backticks (which would otherwise
// close the span early and let anything after them render as live
// markdown) and newlines/carriage returns (which would otherwise inject an
// extra line into the PR body) with a single quote and a space
// respectively -- keeping every field on its own line and the byte count
// unchanged, so this stays a display-only substitution, not a truncation.
func sanitizeMarkdownField(s string) string {
	replacer := strings.NewReplacer("`", "'", "\n", " ", "\r", " ")
	return replacer.Replace(s)
}

// codeReviewFieldMaxRunes bounds every model-written CodeReviewFinding
// field (summary, failure scenario, file path) rendered into the PR
// body: a reviewer turn is free-form prose, unlike the fixed-shape
// evidence every other section here renders, so nothing else bounds its
// length before it reaches sanitizePRField below.
const codeReviewFieldMaxRunes = 400

// sanitizePRField makes one model-written code-review finding field safe
// to embed in the PR body: sanitize.Line first (strips ANSI/control
// characters, redacts an obvious credential, and folds any embedded
// newline/line-breaking rune to a single space -- this field is
// reviewer-authored prose, not the display-only single-line values
// sanitizeMarkdownField's own callers already control the shape of), then
// sanitizeMarkdownField (backtick/newline neutralization for the
// surrounding markdown), then truncated to codeReviewFieldMaxRunes runes
// (rune-safe: sliced on the decoded rune sequence, not raw bytes, so a
// multi-byte rune straddling the cutoff is never split into invalid
// UTF-8) with a trailing "…" when truncation actually happened.
func sanitizePRField(s string) string {
	s = sanitizeMarkdownField(sanitize.Line(s))
	runes := []rune(s)
	if len(runes) <= codeReviewFieldMaxRunes {
		return s
	}
	return string(runes[:codeReviewFieldMaxRunes]) + "…"
}

// codeReviewFindingsMaxRendered caps how many CodeReviewFinding entries
// renderCodeReviewMarkdown ever writes into the PR body -- a runaway or
// adversarial reviewer response ballooning a public PR body without
// bound is the same class of risk internal/codereview's own maxFindings
// already defends against for the run record; this is the second,
// independent cap on what actually reaches GitHub.
const codeReviewFindingsMaxRendered = 20

// renderCodeReviewMarkdown renders the "## Code review" PR-body section
// for the standalone AI code-review pass (agent/pi/scripts/code_review.py,
// internal/codereview) -- nil (the ticket never ran this phase, i.e.
// -code-review-policy was "off" or the run never reached it) renders
// nothing at all, matching the "## Spec conformity" section's own
// convention of only appearing when that phase actually ran.
//
// Findings are rendered high severity first, then medium, then low,
// stable within a severity (Go's sort.SliceStable equivalent -- see
// sort.Stable below) so two findings of the same severity keep
// code_review.py's own reported order. Capped at
// codeReviewFindingsMaxRendered, with a final "… N more" line naming
// exactly how many were left out, pointing at the full evidence file for
// a reviewer who wants the rest.
func renderCodeReviewMarkdown(cr *run.CodeReviewResult) string {
	if cr == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("## Code review\n\n")
	fmt.Fprintf(&b, "Policy: %s.", sanitizeMarkdownField(cr.Policy))
	if !cr.Available {
		b.WriteString(" Reviewer unavailable.")
	}
	b.WriteString("\n\n")
	if cr.Available && len(cr.Findings) == 0 {
		b.WriteString("No findings.\n\n")
		return b.String()
	}
	findings := make([]run.CodeReviewFinding, len(cr.Findings))
	copy(findings, cr.Findings)
	severityRank := map[string]int{"high": 0, "medium": 1, "low": 2}
	rank := func(sev string) int {
		if r, ok := severityRank[sev]; ok {
			return r
		}
		return len(severityRank)
	}
	sort.SliceStable(findings, func(i, j int) bool {
		return rank(findings[i].Severity) < rank(findings[j].Severity)
	})
	shown := findings
	remaining := 0
	if len(shown) > codeReviewFindingsMaxRendered {
		remaining = len(shown) - codeReviewFindingsMaxRendered
		shown = shown[:codeReviewFindingsMaxRendered]
	}
	for _, f := range shown {
		b.WriteString("- **")
		b.WriteString(sanitizeMarkdownField(f.Severity))
		b.WriteString("**")
		if f.File != "" {
			b.WriteString(" `")
			b.WriteString(sanitizePRField(f.File))
			if f.Line != 0 {
				fmt.Fprintf(&b, ":%d", f.Line)
			}
			b.WriteString("`")
		}
		b.WriteString(" ")
		b.WriteString(sanitizePRField(f.Summary))
		if f.FailureScenario != "" {
			b.WriteString(" — ")
			b.WriteString(sanitizePRField(f.FailureScenario))
		}
		b.WriteString("\n")
	}
	if remaining > 0 {
		fmt.Fprintf(&b, "- … %d more in `CODE_REVIEW_EVIDENCE.json`\n", remaining)
	}
	b.WriteString("\n")
	return b.String()
}

// invalidateStoredReleaseDecision is the fix for a real GitHub Codex App
// review finding on this PR: InvalidatedByRunID is attribution the
// factory only ever adds to a run's own record *after* that run was
// already accepted -- by which point recordReleaseDecision has typically
// already evaluated and durably saved its release.Decision.
// release.MergePolicyCheck denying on InvalidatedByRunID (added alongside
// this fix) closes the gap for any *future* re-evaluation, but does
// nothing to a decision file already on disk: GET /runs/{id}/release
// (and factoryd's own kill-switch tooling) load that file directly, never
// recomputing it, so a run flagged by a later regression kept reporting
// allowed: true regardless.
//
// Delegates to release.InvalidateDecision, not a local load-modify-save
// (an earlier version of this fix did that directly, and raced
// recordReleaseDecision: see InvalidateDecision's own doc comment for the
// lock the two now share, and for why an invalidation reaching an
// as-yet-unrecorded decision is written as a tombstone rather than
// silently dropped). Best-effort and non-fatal, like every other
// cross-run attribution write in this file: the run record itself (not
// this stored decision) is the attribution's real source of truth.
//
// Deliberately NOT called from recordSpecDriftIfDetected (found via the
// same review round): unlike a full-suite regression, which is only ever
// attributed after the successor run has itself fully completed (already
// past any chain validation that could reject it), spec-drift detection
// runs *before* the -repository path's own chain validation
// (ValidateSliceChainActivity) confirms the declared successor is even a
// legitimate one -- recordSpecDriftIfDetected's own doc comment already
// documented this as an accepted approximation ("a chain that later
// turns out stale... may still have been marked drifted here") back when
// spec drift was purely advisory and never gated anything. Immediately
// flipping an already-published Allowed: true decision to denied on that
// same speculative signal would make a previously-harmless approximation
// actively wrong. release.MergePolicyCheck still denies on
// SpecDriftDetectedByRunID for any future fresh evaluation, per the
// original review's own request; only the eager retroactive flip is
// withheld here.
func invalidateStoredReleaseDecision(dataDir, project, runID, reason string) {
	if err := release.InvalidateDecision(dataDir, project, runID, reason, time.Now().Format(time.RFC3339)); err != nil {
		log.Printf("run %s: warning: could not invalidate release decision: %v", runID, err)
	}
}
