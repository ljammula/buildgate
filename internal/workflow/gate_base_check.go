package workflow

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.temporal.io/sdk/activity"

	"buildgate/internal/evidence"
	"buildgate/internal/policy"
	"buildgate/internal/run"
	"buildgate/internal/runner"
	"buildgate/internal/sandbox"
	"buildgate/internal/sanitize"
	wsisolation "buildgate/internal/workspace"
)

// A named or repository gate that fails on the build's result is rerun once on
// the commit the ticket's work started from, inside the gate's own Activity
// (RunNamedGateActivity): a gate that fails there the same way (sameGateFailure)
// cannot be fixed by any build, and the handoff sorts it as the operator's
// instead of spending a corrective build on it (handoff.binFor). A gate that
// was already failing there in another way stays a failure a build is given:
// turning that gate green may be the ticket's job.
//
// The rerun is evidence about the gate, never part of its verdict:
//
//   - it runs only after the gate failed, so a passing gate costs nothing;
//   - it runs in a scratch worktree of the base commit under the run's own
//     directory, never in the run's worktree, and through the same launch
//     path as the gate (runSandboxWithRetries): the same image, setup
//     commands, limits, registry proxy, compose services and read-only
//     `.factory/` from the trusted commit, and no model route;
//   - the gate's own result is checkpointed first, with the rerun recorded as
//     interrupted, so a worker that dies mid-rerun leaves a completed gate and
//     a retry of the Activity returns that checkpoint without launching
//     anything again;
//   - whatever stops the rerun from reaching an exit code is recorded as "not
//     checked" with the reason, and the gate stays the failure it was.
//
// The reference oracle is not rerun: its gate has its own canary and bin.

// gateBaseDirName is the directory, in the run's own directory, that holds
// the scratch worktrees of base reruns, one per gate, named by its check.
const gateBaseDirName = "gate-base"

// gateBaseCheckReserve is kept back from what is left of the gate Activity's
// deadline when the rerun starts: the scratch worktree is removed and the
// checkpoint saved again inside it. The Activity must never time out because
// of the rerun, since its retry would then be asked to trust the first run.
const gateBaseCheckReserve = time.Minute

// The waits of the rerun's host steps. gateBaseRemoveLockWait is how long
// the removal after a rerun waits for the git metadata lock, inside
// gateBaseCheckReserve, before it deletes the directory without it.
// gateBaseSweepLockWait is the same for a retried Activity's sweep, which
// the Activity itself waits for no longer than gateBaseSweepWait.
const (
	gateBaseRemoveLockWait = 30 * time.Second
	gateBaseSweepLockWait  = 2 * time.Second
	gateBaseSweepWait      = 5 * time.Second
)

// gateBaseHeartbeatInterval is how often the rerun heartbeats.
const gateBaseHeartbeatInterval = activityHeartbeatInterval

// gateBaseInterrupted is the reason checkpointed before the rerun starts; it
// is what the run records when the rerun's worker did not live to finish it.
const gateBaseInterrupted = "the rerun on the base commit did not finish: its worker stopped or its result could not be saved"

// maxGateBaseReasonBytes caps a recorded "not checked" reason.
const maxGateBaseReasonBytes = 300

// gateBaseWorktreePath is the scratch worktree of check's base rerun.
func gateBaseWorktreePath(logDir, check string) (string, error) {
	return filepath.Abs(filepath.Join(logDir, gateBaseDirName, check))
}

// gateBaseNotChecked is the record of a rerun that reached no exit code.
func gateBaseNotChecked(baseSHA, format string, args ...any) *run.GateBaseCheck {
	reason := sanitize.Line(fmt.Sprintf(format, args...))
	if len(reason) > maxGateBaseReasonBytes {
		reason = strings.ToValidUTF8(reason[:maxGateBaseReasonBytes], "") + "…"
	}
	return &run.GateBaseCheck{Outcome: run.GateBaseNotChecked, BaseSHA: baseSHA, ExitCode: -1, Reason: reason}
}

// pendingGateBaseCheck is the base check a gate's first checkpoint carries:
// nil when the gate is not rerun (it passed, it did not reach an exit code,
// or it is the reference oracle), else the "interrupted" record the finished
// rerun replaces.
func pendingGateBaseCheck(input NamedGateActivityInput, result runner.Result, runErr, evidenceErr error) *run.GateBaseCheck {
	if runErr != nil || evidenceErr != nil || result.ExitCode == 0 || input.Check == policy.ReferenceOracleGateID {
		return nil
	}
	base, _ := gateBaseCommit(input.RunWorkflowInput)
	return gateBaseNotChecked(base, "%s", gateBaseInterrupted)
}

// gateBaseCommit is the commit the ticket's work started from, when the run's
// own input proves which it is; otherwise "" and why it is not known.
//
//   - A run given a diff base (a corrective build, a PR-review round, a retry
//     that continues on the failed attempt's commit) names it: the ticket's
//     base, whatever commit the run itself started from.
//   - A run that adopts a halted run's worktree (ResumeFrom) or checks out an
//     existing branch (OnBranch), with no diff base, starts from a commit that
//     may already hold the ticket's work: a resumed corrective round's base is
//     the failed attempt's own commit. A gate red there says nothing about
//     the ticket's base, so the rerun is not made.
//   - Any other run made its own branch at its base commit.
func gateBaseCommit(input RunWorkflowInput) (sha, unknown string) {
	switch {
	case input.DiffBaseSHA != "":
		return input.DiffBaseSHA, ""
	case input.ResumeFrom != nil:
		return "", "the run resumed an earlier run's worktree and names no diff base, so its base may already hold the ticket's work"
	case input.OnBranch != "":
		return "", "the run continues an existing branch and names no diff base, so its base may already hold the ticket's work"
	case input.BaseSHA == "":
		return "", "the run recorded no base commit"
	}
	return input.BaseSHA, ""
}

// sweepGateBaseWorktree removes the scratch worktree an earlier attempt of
// this gate's Activity left when its worker died mid-rerun. It is called from
// the Activity's completed-checkpoint return and must never hold that return
// up: the removal runs on its own, takes the git metadata lock only if it is
// free within gateBaseSweepLockWait (else it deletes the directory and leaves
// the registration for the next prune), and the Activity waits for it no
// longer than gateBaseSweepWait. Best effort: a leftover costs disk only.
func (a *Activities) sweepGateBaseWorktree(input NamedGateActivityInput) {
	scratch, err := gateBaseWorktreePath(a.logDirFor(input.RunWorkflowInput), input.Check)
	if err != nil {
		return
	}
	if _, err := os.Lstat(scratch); err != nil {
		return
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		ctx, cancel := context.WithTimeout(context.Background(), gateBaseSweepLockWait)
		defer cancel()
		_ = removeGateBaseWorktree(ctx, input.WorkspacePath, scratch)
	}()
	select {
	case <-done:
	case <-time.After(gateBaseSweepWait):
	}
}

// removeGateBaseWorktree removes the scratch worktree and, when the git
// metadata lock is free before ctx ends, its registration in the repository.
// A tree git did not remove (the lock was busy, or the gate's command left
// files git cannot delete) is deleted directly; a registration left behind
// names a path that is gone and holds no branch, and the next prune drops it.
func removeGateBaseWorktree(ctx context.Context, workspace, scratch string) error {
	if _, err := os.Lstat(scratch); err != nil {
		// Nothing was checked out: no lock is taken for nothing.
		_ = os.Remove(filepath.Dir(scratch))
		return nil
	}
	err := wsisolation.RemoveScratchWorktree(ctx, workspace, scratch)
	if _, statErr := os.Lstat(scratch); statErr == nil {
		if rmErr := os.RemoveAll(scratch); rmErr != nil {
			return errors.Join(err, rmErr)
		}
		err = nil
	}
	// Removing the last worktree leaves the empty parent; a later run of
	// the gate makes it again.
	_ = os.Remove(filepath.Dir(scratch))
	return err
}

// finishGateBaseCheck is the tail of RunNamedGateActivity for a gate whose
// result is already checkpointed: when that result carries a pending base
// check it reruns the gate on the base commit, saves the checkpoint again
// with what the rerun showed and returns that result. A second save that
// fails returns the result the checkpoint still holds, so the Activity's
// return value and its checkpoint never disagree.
func (a *Activities) finishGateBaseCheck(ctx context.Context, input NamedGateActivityInput, command []string, registrySpec *sandbox.RegistryProxySpec, path string, checkpoint activityCheckpoint[VerifyActivityResult], gateResult VerifyActivityResult) VerifyActivityResult {
	if gateResult.BaseCheck == nil {
		return gateResult
	}
	checked := gateResult
	checked.BaseCheck = a.checkGateOnBase(ctx, input, gateResult.Result, command, registrySpec)
	checkpoint.Result = checked
	if err := saveActivityCheckpoint(path, checkpoint, activity.GetInfo(ctx).Attempt); err != nil {
		activity.GetLogger(ctx).Warn("the base rerun's result could not be saved; the gate keeps its recorded result", "check", input.Check, "error", err.Error())
		return gateResult
	}
	return checked
}

// checkGateOnBase reruns a failed gate's command on the base commit and
// returns what it showed. It returns no error: every failure of its own is a
// "not checked" record. The Activity heartbeats for as long as it runs, its
// host steps (the git metadata lock, the checkout, the removal) included:
// another run of the repository may hold that lock for longer than the
// Activity's heartbeat timeout.
func (a *Activities) checkGateOnBase(ctx context.Context, input NamedGateActivityInput, gate runner.Result, command []string, registrySpec *sandbox.RegistryProxySpec) *run.GateBaseCheck {
	started := time.Now()
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		ticker := time.NewTicker(gateBaseHeartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				activity.RecordHeartbeat(ctx, HeartbeatDetails{Stage: input.Check + "_base", Elapsed: time.Since(started)})
			}
		}
	}()
	return a.rerunGateOnBase(ctx, input, gate, command, registrySpec)
}

// rerunGateOnBase is checkGateOnBase's work.
func (a *Activities) rerunGateOnBase(ctx context.Context, input NamedGateActivityInput, gate runner.Result, command []string, registrySpec *sandbox.RegistryProxySpec) *run.GateBaseCheck {
	base, unknown := gateBaseCommit(input.RunWorkflowInput)
	if base == "" {
		return gateBaseNotChecked("", "%s", unknown)
	}
	// The rerun gets what is left of the gate's own time limit, less the
	// reserve, as a deadline of its own: when it runs out a wait for the git
	// metadata lock ends, or the launch is stopped and torn down, while the
	// Activity still has time to finish.
	rerunCtx := ctx
	if deadline, ok := ctx.Deadline(); ok {
		left := time.Until(deadline) - gateBaseCheckReserve
		if left <= sandboxAttemptTeardownMargin {
			return gateBaseNotChecked(base, "too little of the gate's time limit was left to run it again on the base commit")
		}
		var cancel context.CancelFunc
		rerunCtx, cancel = context.WithTimeout(ctx, left)
		defer cancel()
	}
	logDir := a.logDirFor(input.RunWorkflowInput)
	scratch, err := gateBaseWorktreePath(logDir, input.Check)
	if err != nil {
		return gateBaseNotChecked(base, "resolve the scratch worktree's path: %v", err)
	}
	// A leftover of an earlier attempt would make the checkout fail. Its
	// registration, if any, is pruned by the checkout under the lock.
	if err := os.RemoveAll(scratch); err != nil {
		return gateBaseNotChecked(base, "remove an earlier scratch worktree: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(scratch), 0o750); err != nil {
		return gateBaseNotChecked(base, "create the scratch worktree's directory: %v", err)
	}
	// Registered before the checkout: one that fails half-way is removed
	// too. The removal has its own short wait for the lock, inside the
	// reserve, and is not cut short by the rerun's deadline.
	defer func() {
		removeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), gateBaseRemoveLockWait)
		defer cancel()
		if err := removeGateBaseWorktree(removeCtx, input.WorkspacePath, scratch); err != nil {
			activity.GetLogger(ctx).Warn("the base rerun's scratch worktree could not be removed", "path", scratch, "error", err.Error())
		}
	}()
	if err := wsisolation.AddScratchWorktree(rerunCtx, input.WorkspacePath, scratch, base); err != nil {
		return gateBaseNotChecked(base, "check out the base commit in a scratch worktree: %v", err)
	}
	// As for the run's own worktree: the worker's identity writes through
	// the group.
	if err := wsisolation.EnableWorkerGroupWrite(scratch, os.Getgid()); err != nil {
		return gateBaseNotChecked(base, "make the scratch worktree writable for the worker: %v", err)
	}
	composeSpec, err := a.composeServicesSpecFor(input.RunWorkflowInput, input.Check+"_base")
	if err != nil {
		return gateBaseNotChecked(base, "%v", err)
	}
	onBase := input.RunWorkflowInput
	onBase.WorkspacePath = scratch
	logPath := activityLogPath(activityExecutionLogPath(ctx, logDir, input.Check+".base.log"))
	// No intent and no journal: the rerun is not an attempt of the gate, and
	// the gate's checkpoint already exists. The lease is still checked, so a
	// superseded attempt launches nothing.
	beforeAttempt := a.leaseChecked(ctx, a.checkpointDirFor(input.RunWorkflowInput), nil)
	var result runner.Result
	var runErr error
	if a.hasFakeRunner() {
		result, runErr = a.runWithRetriesFn()(rerunCtx, scratch, logPath, 1, beforeAttempt, nil, command[0], command[1:]...)
	} else {
		// nil relay, as for the gate itself: the rerun calls no model.
		result, runErr = a.runSandboxWithRetries(rerunCtx, onBase, logPath, 1, beforeAttempt, nil, nil, registrySpec, composeSpec, "", "", nil, nil, command[0], command[1:]...)
	}
	if runErr != nil {
		return gateBaseNotChecked(base, "the gate could not be run on the base commit: %v", runErr)
	}
	check := &run.GateBaseCheck{Outcome: run.GateBasePasses, BaseSHA: base, ExitCode: result.ExitCode, LogPath: result.LogPath}
	if result.ExitCode != 0 {
		check.Outcome = run.GateBaseFailsDifferently
		if sameGateFailure(gate, result) {
			check.Outcome = run.GateBaseFailsSame
		}
	}
	// A log that cannot be hashed leaves the outcome standing without one.
	check.LogSHA256, _ = evidence.SHA256File(result.LogPath)
	return check
}
