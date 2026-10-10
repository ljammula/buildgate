package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"syscall"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"

	"buildgate/internal/requestdriver"
	"buildgate/internal/run"
	"buildgate/internal/sandbox"
	wsisolation "buildgate/internal/workspace"
)

// deadOwnerTriage is the Run.Triage of a run reclaimDeadOwnerRuns halts.
const deadOwnerTriage = "halted: the factoryd process running this build stopped mid-run (killed, crashed or lost); the next worker start halted it -- `factoryd retry` runs it again"

// deadOwnerHaltError is the matching Run.HaltError.
const deadOwnerHaltError = "owning factoryd process is gone; run reclaimed at worker start"

// workflowTerminator is the slice of client.Client reclaimDeadOwnerRuns
// needs, so tests can stub it.
type workflowTerminator interface {
	DescribeWorkflowExecution(ctx context.Context, workflowID, runID string) (*workflowservice.DescribeWorkflowExecutionResponse, error)
	TerminateWorkflow(ctx context.Context, workflowID, runID, reason string, details ...interface{}) error
}

// dialTerminator opens a Temporal client for address. A boundary method so tests
// inject a stub.
func (impl realTemporal) dialTerminator(ctx context.Context, address string) (workflowTerminator, func(), error) {
	dialCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	c, err := client.DialContext(dialCtx, client.Options{HostPort: address})
	if err != nil {
		return nil, nil, err
	}
	return c, c.Close, nil
}

// runOwnerDead reports whether the process that owned runID's sandbox is
// provably gone. The owner is the PID in the run's sandbox-owner.pid marker,
// the same marker the sandbox reconcilers use (sandbox.OwnerPID). Dead means
// kill(pid, 0) says ESRCH. No marker, an unreadable one, EPERM, or any other
// answer counts as alive: the run is left alone. A reused PID also reads as
// alive, which only delays reclaim, never touches a live run. The worker's
// own PID is alive by construction, so a run it owns is never reclaimed.
func runOwnerDead(dataDir, runID string) bool {
	pid, ok := sandbox.OwnerPID(dataDir, runID)
	if !ok {
		return false
	}
	return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
}

// reclaimDeadOwnerRuns runs at worker start, once the caller holds the
// queue drain lock. `factoryd daemon` reclaims abandoned runs continuously;
// without this, a worker killed with SIGKILL left its run record
// slice_running, its relay container Up and its Temporal workflow Running. For every nonterminal run whose owner is dead (runOwnerDead) it:
//
//  1. terminates the run's Temporal workflow if that is still Running (a
//     workflow that cannot be confirmed closed leaves the run untouched, to
//     retry on the next start);
//  2. records the run halted and confirmed, with deadOwnerTriage;
//  3. removes its worker, relay and compose-sidecar containers by running the
//     sandbox reconcilers, which treat a confirmed-halted run as terminal and
//     skip every run that is still nonterminal.
//
// Best-effort: failures are logged, never returned, so a broken Docker or
// Temporal cannot stop the worker from starting. temporalAddress is the
// fallback for a run record that names no address of its own; dockerBinary ""
// skips step 3. It returns the runs it halted; `factoryd worker` (which runs it at
// start) halts each one's request.

func reclaimDeadOwnerRuns(dp *deps, ctx context.Context, dataDir, dockerBinary, temporalAddress string) []*run.Run {
	entries, err := os.ReadDir(filepath.Join(dataDir, "runs"))
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("worker: could not scan runs for dead owners: %v", err)
		}
		return nil
	}
	terminators := map[string]workflowTerminator{}
	var closers []func()
	defer func() {
		for _, c := range closers {
			c()
		}
	}()
	var reclaimed []*run.Run
	for _, entry := range entries {
		if !entry.IsDir() || ctx.Err() != nil {
			continue
		}
		id := entry.Name()
		r, loadErr := run.Load(dataDir, id)
		if loadErr != nil || runTerminalConfirmed(r) || !runOwnerDead(dataDir, id) {
			continue
		}
		terminatedWorkflow := ""
		if r.TemporalWorkflowID != "" {
			addr := r.TemporalAddress
			if addr == "" {
				addr = temporalAddress
			}
			if addr == "" {
				log.Printf("worker: run %s left as is: no Temporal address to terminate workflow %s", id, r.TemporalWorkflowID)
				continue
			}
			t, ok := terminators[addr]
			if !ok {
				var closeFn func()
				var dialErr error
				t, closeFn, dialErr = dp.temporal.dialTerminator(ctx, addr)
				if dialErr != nil {
					log.Printf("worker: run %s left as is: could not reach Temporal at %s to terminate workflow %s: %v", id, addr, r.TemporalWorkflowID, dialErr)
					continue
				}
				terminators[addr] = t
				closers = append(closers, closeFn)
			}
			did, termErr := terminateRunningWorkflow(ctx, t, r.TemporalWorkflowID, r.TemporalRunID)
			if termErr != nil {
				log.Printf("worker: run %s left as is: could not terminate workflow %s: %v", id, r.TemporalWorkflowID, termErr)
				continue
			}
			if did {
				terminatedWorkflow = r.TemporalWorkflowID
			}
		}
		halted, haltErr := haltDeadOwnerRun(dataDir, id)
		if haltErr != nil {
			log.Printf("worker: could not halt run %s whose owner is gone: %v", id, haltErr)
			continue
		}
		if !halted {
			continue
		}
		reclaimed = append(reclaimed, r)
		line := fmt.Sprintf("worker: reclaimed run %s (was %s): its factoryd process is gone; halted it", id, r.State)
		if terminatedWorkflow != "" {
			line += ", terminated workflow " + terminatedWorkflow
		}
		log.Print(line + ", removing its containers")
	}
	if len(reclaimed) == 0 || dockerBinary == "" {
		return reclaimed
	}
	if removed, reconcileErr := sandbox.ReconcileOrphans(ctx, dockerBinary, dataDir); reconcileErr != nil {
		log.Printf("sandbox: orphaned container reconciliation for %s: %v", dataDir, reconcileErr)
	} else if len(removed) > 0 {
		log.Printf("sandbox: removed %d orphaned container(s) from a prior run: %v", len(removed), removed)
	}
	if removed, reconcileErr := sandbox.ReconcileRelayOrphans(ctx, dockerBinary, dataDir); reconcileErr != nil {
		log.Printf("sandbox: orphaned relay reconciliation for %s: %v", dataDir, reconcileErr)
	} else if len(removed) > 0 {
		log.Printf("sandbox: removed %d orphaned sidecar resource(s) from a prior run: %v", len(removed), removed)
	}
	if removed, reconcileErr := sandbox.ReconcileComposeServicesOrphans(ctx, dockerBinary, dataDir, scratchRunInFlight(dataDir), sandbox.ComposeServicesOrphanHooks{}); reconcileErr != nil {
		log.Printf("sandbox: orphaned compose services reconciliation for %s: %v", dataDir, reconcileErr)
	} else if len(removed) > 0 {
		log.Printf("sandbox: removed %d orphaned compose services resource(s) from a prior run: %v", len(removed), removed)
	}
	removeScratchDirs(dataDir)
	return reclaimed
}

// reclaimDeadRequestJobs removes the worker and relay containers that a
// request-level job (spec, oracle or plan drafting) left behind when its
// process died: reclaimDeadOwnerRuns only scans run records, and these jobs
// have none. The owner is the PID in the request's sandbox-owner.pid marker,
// read like a run's (runOwnerDead). Best-effort: failures are logged.
func reclaimDeadRequestJobs(ctx context.Context, dataDir, dockerBinary string) {
	if dockerBinary == "" {
		return
	}
	sandboxDataDir, err := requestdriver.SandboxDataDirFor(dataDir)
	if err != nil {
		log.Printf("sandbox: orphaned request-job reconciliation for %s: %v", dataDir, err)
		return
	}
	removed, err := sandbox.ReconcileRequestJobOrphans(ctx, dockerBinary, sandboxDataDir, func(id string) bool {
		return runOwnerDead(sandboxDataDir, id)
	})
	if err != nil {
		log.Printf("sandbox: orphaned request-job reconciliation for %s: %v", dataDir, err)
	}
	if len(removed) > 0 {
		log.Printf("sandbox: removed %d orphaned request-job resource(s) from a stopped process: %v", len(removed), removed)
	}
}

// terminateRunningWorkflow terminates workflowID if Temporal reports it
// Running, and reports whether it did. A workflow Temporal does not know, or
// already closed, needs nothing; any other answer is an error so the caller
// leaves the run for a later start.
func terminateRunningWorkflow(ctx context.Context, t workflowTerminator, workflowID, runID string) (bool, error) {
	callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	desc, err := t.DescribeWorkflowExecution(callCtx, workflowID, runID)
	if err != nil {
		var notFound *serviceerror.NotFound
		if errors.As(err, &notFound) {
			return false, nil
		}
		return false, fmt.Errorf("describe: %w", err)
	}
	if desc.GetWorkflowExecutionInfo().GetStatus() != enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING {
		return false, nil
	}
	if err := t.TerminateWorkflow(callCtx, workflowID, runID, "owning factoryd process is gone; reclaimed by worker"); err != nil {
		var notFound *serviceerror.NotFound
		if errors.As(err, &notFound) {
			return false, nil
		}
		return false, fmt.Errorf("terminate: %w", err)
	}
	return true, nil
}

// haltDeadOwnerRun records runID halted-and-confirmed with deadOwnerTriage,
// under the run lock and re-checking terminal state and owner first. It
// reports false when the run turned terminal or its owner came back in the
// meantime.
func haltDeadOwnerRun(dataDir, runID string) (bool, error) {
	halted := false
	err := run.WithLock(dataDir, runID, func() error {
		fresh, loadErr := run.Load(dataDir, runID)
		if loadErr != nil {
			return loadErr
		}
		if runTerminalConfirmed(fresh) || !runOwnerDead(dataDir, runID) {
			return nil
		}
		fresh.State = run.StateHalted
		fresh.HaltConfirmed = true
		fresh.Triage = deadOwnerTriage
		markKeptForResume(dataDir, fresh)
		if saveErr := save(fresh, dataDir, errors.New(deadOwnerHaltError)); saveErr != nil {
			return saveErr
		}
		halted = true
		return nil
	})
	return halted, err
}

// markKeptForResume flags fresh KeptForResume when its lost build left a
// Temporal-mode isolated worktree behind: the prepared isolation marker names
// a worktree that still exists. The worktree holds the build's round state
// and files, so every reaper skips the run until a human decides (a resume
// adopts it; release.ClearKeptForResume reaps it). The worktree path and branch are
// copied onto the record when the run was lost before it recorded them.
// Direct-mode worktrees are never kept: only the Temporal path can resume
// one, so keeping it would leak it. Must run inside the run lock, before the
// save that records the halt.
func markKeptForResume(dataDir string, r *run.Run) {
	// Only a request's run: retry, cancel and rebuild are the human
	// decisions that clear the flag, and a single-ticket run has none.
	// A PR-review corrective round (OnBranch) is excluded too: it never
	// becomes a ticket's run, so nothing would ever reap it.
	if r.RequestID == "" || r.OnBranch != "" {
		return
	}
	marker, err := wsisolation.LoadIsolationMarker(wsisolation.IsolationMarkerPath(dataDir, r.ID))
	if err != nil || marker.Mode != "temporal" || !marker.Prepared || marker.WorktreePath == "" {
		return
	}
	if info, statErr := os.Stat(marker.WorktreePath); statErr != nil || !info.IsDir() {
		return
	}
	r.KeptForResume = true
	if r.WorkspacePath == "" || r.WorkspacePath == r.ProjectPath {
		r.WorkspacePath = marker.WorktreePath
		r.Branch = marker.Branch
	}
}
