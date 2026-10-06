package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"

	"buildgate/internal/daemonheartbeat"
	"buildgate/internal/sandbox"
)

// ActivitySupersededType marks an Activity attempt that found a later
// Temporal attempt of the same execution already holding the activity lease
// (see takeActivityLease). Non-retryable: the later attempt owns the
// execution's worktree and completed checkpoint, so this one must stop
// without touching either. Temporal discards a superseded attempt's result,
// so this type exists for auditability in error strings and logs.
const ActivitySupersededType = "ActivitySuperseded"

// errActivitySuperseded is returned when an attempt is older than the
// attempt named by the execution's lease.
var errActivitySuperseded = errors.New("activity attempt superseded by a later attempt")

// activityLease records the latest Temporal attempt that took ownership of
// an Activity execution. It lives beside the completed checkpoint, per
// execution (not per attempt), because the thing it fences is per execution.
type activityLease struct {
	WorkflowID      string `json:"workflow_id"`
	RunID           string `json:"run_id"`
	ActivityID      string `json:"activity_id"`
	ActivityAttempt int32  `json:"activity_attempt"`
}

// Why the completed checkpoint needs fencing: it is deliberately keyed per
// execution, not per attempt (any attempt's completion is the execution's
// answer, so a redispatch finds it and returns it). That makes it the one
// write a stale attempt can clobber: an attempt that resumed after a laptop
// sleep, while Temporal had already redispatched the Activity, would finish
// late and overwrite the live attempt's checkpoint with a result computed
// against a worktree the live attempt has since changed. Every attempt >1
// therefore takes the lease first, and saveActivityCheckpoint refuses to
// write once a later attempt holds it.
//
// Why attempt 1 takes no lease: there is nothing earlier to fence, and
// attempt 1's writes are made refusable by the lease a later attempt takes,
// not by one of its own. Attempt 1 therefore creates no lease file; it does
// still take the lock file (<execKey>.lease.lock) on every checkpoint save,
// because that flock is what stops a stale save landing after a later
// attempt's lease: the save's read-check-write and the lease take are
// mutually exclusive. flock must work on the checkpoint dir's filesystem
// (local disk, the data dir); a network filesystem without flock support
// would silently lose the fence.
//
// Each Activity fences BEFORE its completed-checkpoint lookup: a stale
// attempt's save either lands before the lease (the retry then loads and
// returns that completed checkpoint) or is refused after it. Looking up
// first would leave a window where the retry sees nothing, the stale save
// lands, and the retry then redoes work against a completed answer.
//
// Fencing is also re-checked at every launch (leaseChecked, and before the
// relay launch): a stale attempt woken from sleep sees its container exit
// as an infrastructure failure and would otherwise relaunch a fresh relay
// and worker on the worktree.

func activityLeasePath(checkpointDir, workflowID, runID, activityID string) string {
	return filepath.Join(checkpointDir, "activity-checkpoints", activityExecutionKey(workflowID, runID, activityID)+".lease.json")
}

// withActivityLeaseLock runs fn holding an exclusive flock on the lease's
// lock file, serialising every lease read-check-write for one execution
// across goroutines and processes (an old worker process may still be alive).
func withActivityLeaseLock(leasePath string, fn func() error) error {
	if err := os.MkdirAll(filepath.Dir(leasePath), 0o750); err != nil {
		return fmt.Errorf("create lease dir: %w", err)
	}
	lock, err := os.OpenFile(leasePath[:len(leasePath)-len(".json")]+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open lease lock: %w", err)
	}
	defer lock.Close()
	for {
		err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX)
		if err != syscall.EINTR {
			break
		}
	}
	if err != nil {
		return fmt.Errorf("lock lease: %w", err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) //nolint:errcheck // closing the descriptor releases the lock anyway
	return fn()
}

// readActivityLease returns the lease, or ok=false when none exists. An
// unreadable or malformed lease is an error: failing closed beats letting a
// stale attempt through on a file it cannot parse.
func readActivityLease(leasePath string) (lease activityLease, ok bool, err error) {
	b, err := os.ReadFile(leasePath)
	if os.IsNotExist(err) {
		return lease, false, nil
	}
	if err != nil {
		return lease, false, fmt.Errorf("read activity lease: %w", err)
	}
	if err := json.Unmarshal(b, &lease); err != nil {
		return lease, false, fmt.Errorf("unmarshal activity lease: %w", err)
	}
	return lease, true, nil
}

// leaseAllows reports whether attempt may proceed under lease: no lease, or
// a lease naming this attempt or an earlier one. Caller holds the lock.
func leaseAllows(leasePath string, attempt int32) (bool, error) {
	lease, ok, err := readActivityLease(leasePath)
	if err != nil {
		return false, err
	}
	return !ok || lease.ActivityAttempt <= attempt, nil
}

// takeActivityLease makes attempt the execution's lease holder, unless a
// later attempt already holds it (errActivitySuperseded). Re-taking at the
// same attempt is a no-op rewrite.
func takeActivityLease(checkpointDir, workflowID, runID, activityID string, attempt int32) error {
	path := activityLeasePath(checkpointDir, workflowID, runID, activityID)
	return withActivityLeaseLock(path, func() error {
		allowed, err := leaseAllows(path, attempt)
		if err != nil {
			return err
		}
		if !allowed {
			return errActivitySuperseded
		}
		return writeFileAtomic(path, activityLease{WorkflowID: workflowID, RunID: runID, ActivityID: activityID, ActivityAttempt: attempt})
	})
}

func writeFileAtomic(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal %s: %w", filepath.Base(path), err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", filepath.Base(path), err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename %s: %w", filepath.Base(path), err)
	}
	return nil
}

// fenceAttempt is the lease half of fenceEarlierAttempts, for the Activities
// that do host-side work only (no containers to reap). At attempt 1 it does
// nothing; at a later attempt it takes the lease, so an earlier attempt's
// checkpoint save is refused from then on.
func (a *Activities) fenceAttempt(ctx context.Context, checkpointDir string) error {
	info := activity.GetInfo(ctx)
	if info.Attempt <= 1 {
		return nil
	}
	if err := takeActivityLease(checkpointDir, info.WorkflowExecution.ID, info.WorkflowExecution.RunID, info.ActivityID, info.Attempt); err != nil {
		if errors.Is(err, errActivitySuperseded) {
			return temporal.NewNonRetryableApplicationError("a later attempt of this Activity holds its lease", ActivitySupersededType, err)
		}
		return temporal.NewApplicationErrorWithCause("take Activity lease", InfrastructureFailureType, err)
	}
	return nil
}

// fenceEarlierAttempts is called by each checkpointed Activity after its
// completed-checkpoint early return and before its prior-attempt records
// check. At attempt 1 it does nothing. At a later attempt it takes the lease
// (fenceAttempt) and, when reap is set, removes the run's containers so the
// earlier attempt cannot keep editing the worktree through them. The reap
// must be confirmed before this attempt touches the worktree: a surviving
// container from the earlier attempt would race it, so ErrCleanupUnconfirmed
// halts as an infrastructure failure. reap is set for sandbox Activities
// only; the rest do host-side work that a stale attempt can only affect via
// the checkpoint, which the lease fences. The reap is skipped when the
// Activity's runner is faked or the run id (runIDFor) is empty, as launches are in
// those cases.
func (a *Activities) fenceEarlierAttempts(ctx context.Context, input RunWorkflowInput, reap bool) error {
	if err := a.fenceAttempt(ctx, a.checkpointDirFor(input)); err != nil {
		return err
	}
	if !reap || activity.GetInfo(ctx).Attempt <= 1 || a.hasFakeRunner() || a.runIDFor(input) == "" {
		return nil
	}
	// The reap runs the Docker executable and labels the request names, so
	// those are validated first: an unvalidated request-supplied
	// -sandbox-docker must never be executed.
	if err := a.checkSandboxScope(input); err != nil {
		return err
	}
	if err := sandbox.RemoveRunContainers(ctx, a.sandboxDockerFor(input), a.dataDirFor(input), a.runIDFor(input)); err != nil {
		return temporal.NewApplicationErrorWithCause("remove earlier attempt's containers before retrying", InfrastructureFailureType, err)
	}
	return nil
}

// checkLease refuses with the non-retryable superseded error when a later
// attempt than this Activity's own holds the execution's lease. Outside an
// Activity context (direct callers) there is nothing to check.
func (a *Activities) checkLease(ctx context.Context, checkpointDir string) error {
	if !activity.IsActivity(ctx) {
		return nil
	}
	info := activity.GetInfo(ctx)
	return checkLeaseForExecution(checkpointDir, info.WorkflowExecution.ID, info.WorkflowExecution.RunID, info.ActivityID, info.Attempt)
}

func checkLeaseForExecution(checkpointDir, workflowID, runID, activityID string, attempt int32) error {
	path := activityLeasePath(checkpointDir, workflowID, runID, activityID)
	var allowed bool
	err := withActivityLeaseLock(path, func() (err error) {
		allowed, err = leaseAllows(path, attempt)
		return err
	})
	if err != nil {
		return temporal.NewApplicationErrorWithCause("read Activity lease", InfrastructureFailureType, err)
	}
	if !allowed {
		return temporal.NewNonRetryableApplicationError("a later attempt of this Activity holds its lease", ActivitySupersededType, errActivitySuperseded)
	}
	return nil
}

// leaseChecked wraps an Activity's beforeAttempt hook so every launch (each
// internal retry too) first confirms this attempt still holds the execution.
func (a *Activities) leaseChecked(ctx context.Context, checkpointDir string, before func(int) error) func(int) error {
	return func(attempt int) error {
		if err := a.checkLease(ctx, checkpointDir); err != nil {
			return err
		}
		if before == nil {
			return nil
		}
		return before(attempt)
	}
}

// checkSandboxScope refuses a request whose -data-dir or -sandbox-docker
// diverges from this Worker's own: its containers would be labeled for, or
// launched by, something this Worker's reconciliation never sees. It runs
// before anything executes the Docker executable or sweeps by data-dir
// label (a retried attempt's reap included), and its refusal is
// non-retryable: a diverging request never becomes valid by retrying.
func (a *Activities) checkSandboxScope(input RunWorkflowInput) error {
	if a.DataDir != "" && input.DataDir != "" && input.DataDir != a.DataDir {
		return temporal.NewNonRetryableApplicationError(fmt.Sprintf("sandboxed execution's data directory (%q) diverges from this Worker's own -data-dir (%q): its container would be labeled for a directory this Worker's own startup reconciliation never scans", input.DataDir, a.DataDir), InfrastructureFailureType, nil)
	}
	// Compared through daemonheartbeat.ResolveSandboxDocker: two values that
	// resolve to the same real executable ("docker" and its absolute path)
	// are not a divergence.
	if a.SandboxDocker != "" && input.SandboxDocker != "" && daemonheartbeat.ResolveSandboxDocker(input.SandboxDocker) != daemonheartbeat.ResolveSandboxDocker(a.SandboxDocker) {
		return temporal.NewNonRetryableApplicationError(fmt.Sprintf("sandboxed execution's Docker executable (%q) diverges from this Worker's own -sandbox-docker (%q): its container would be invisible to this Worker's own reconciliation", input.SandboxDocker, a.SandboxDocker), InfrastructureFailureType, nil)
	}
	return nil
}
