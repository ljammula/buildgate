package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"go.temporal.io/sdk/client"

	"buildgate/internal/sandbox"
	wsisolation "buildgate/internal/workspace"
)

// reconcileMain runs the same stranded-worktree/branch reconciliation a
// subsequent run against this repository would run automatically at its
// own startup (reconcileIsolationMarkers), but on demand and standalone --
// without needing to submit a new run first. Found live: an interrupted
// `factoryd` invocation (e.g. a killed process, a lost SSH session) leaves
// its isolation marker and, when not resumable (no completed prepare
// checkpoint, and its Temporal workflow — if any — no longer running), its
// git worktree/branch sitting in the repository indefinitely; nothing
// reaps them until the operator happens to launch another run against the
// same -workspace, which is often not soon. This command lets an operator
// clean up right away, or just see what reconcileIsolationMarkers would
// report — it applies the exact same safety logic (a run that's still
// StateHalted-but-not-confirmed, still resumable via a completed Temporal
// prepare checkpoint, or whose workflow the given -temporal-address still
// reports running, is preserved, never reaped).
//
// Takes the same non-blocking repository lock a bare run does (not the
// blocking, polling AcquireDirectLockContext a run uses to wait its turn):
// reconciliation is opportunistic maintenance, not a queued operation, so
// if a real run currently holds the lock, this simply reports that and
// exits rather than waiting to reap markers stranded moments ago that a
// live run may itself be about to touch.
// newReconcileFlags builds `factoryd reconcile`'s FlagSet in isolation from
// parsing, so USAGE.md's doc-vs-flag drift test
// (TestUSAGEDocFlagsExistOnSubcommand) can enumerate its real flags without
// executing the command.
func newReconcileFlags() (flags *flag.FlagSet, workspace, dataDir, temporalAddress, sandboxDocker *string) {
	flags = flag.NewFlagSet("reconcile", flag.ContinueOnError)
	workspace = flags.String("workspace", "", "path to the repository to reconcile stranded isolation markers/worktrees for (required) -- same meaning as factoryd <run>'s -workspace")
	dataDir = flags.String("data-dir", "data", "directory holding durable run records and isolation markers (must match the value the original runs used)")
	temporalAddress = flags.String("temporal-address", "", "Temporal server address; when set, a Temporal-mode marker whose workflow this server still reports running is preserved rather than reaped. Omitted: Temporal-mode markers are preserved unconditionally as inconclusive, the same fail-safe default reconcileIsolationMarkers itself uses for a nil client")
	sandboxDocker = flags.String("sandbox-docker", "docker", "Docker executable used to reconcile orphaned sandbox containers and check whether a sandboxed run's container is still present before reaping its worktree; a check that fails for any reason (including this executable not being resolvable) is treated as inconclusive and preserves the marker rather than assuming no container exists. Pass -sandbox-docker=\"\" explicitly to skip Docker entirely if this fleet never uses sandboxing and reconciliation should never wait on it")
	plainFlagUsage(flags)
	return
}

func reconcileMain(args []string) error {
	flags, workspace, dataDir, temporalAddress, sandboxDocker := newReconcileFlags()
	if err := flags.Parse(args); err != nil {
		return err
	}
	if err := resolveDataDirFromSessionConfig(flags, dataDir, ""); err != nil {
		return err
	}
	if *workspace == "" {
		flags.Usage()
		return fmt.Errorf("-workspace is required")
	}
	if _, err := os.Stat(*workspace); err != nil {
		return fmt.Errorf("workspace: %w", err)
	}
	// Canonicalized the same way the regular run path resolves the
	// repository containing -workspace (filepath.Abs then EvalSymlinks) --
	// found via review: a relative -workspace (e.g. "-workspace .") left
	// unresolved here would still let AcquireDirectLock succeed, but
	// ValidateIsolationMarker's own filepath.EvalSymlinks(repoDir) does NOT
	// make a relative path absolute (per its documented behavior), while
	// every marker's own RepoDir was written from the fully-resolved
	// absolute path a real run computes. The two would then never match,
	// so every marker looks like it belongs to some other repository —
	// reconcile would exit 0 having silently reaped nothing.
	absWorkspace, err := filepath.Abs(*workspace)
	if err != nil {
		return fmt.Errorf("resolve workspace path: %w", err)
	}
	resolvedWorkspace, err := filepath.EvalSymlinks(absWorkspace)
	if err != nil {
		return fmt.Errorf("resolve workspace path: %w", err)
	}
	*workspace = resolvedWorkspace
	absDataDir, err := canonicalPath(*dataDir)
	if err != nil {
		return fmt.Errorf("resolve data dir: %w", err)
	}
	lock, err := wsisolation.AcquireDirectLock(*workspace)
	if err != nil {
		if errors.Is(err, wsisolation.ErrBusy) {
			return fmt.Errorf("repository is busy: another factoryd process holds -workspace %q; reconcile again once it finishes", *workspace)
		}
		return fmt.Errorf("acquire repository ownership: %w", err)
	}
	defer func() {
		if closeErr := lock.Close(); closeErr != nil {
			log.Printf("release repository ownership: %v", closeErr)
		}
	}()

	var temporalClient client.Client
	if *temporalAddress != "" {
		dialCtx, cancelDial := context.WithTimeout(context.Background(), 3*time.Second)
		var dialErr error
		temporalClient, dialErr = client.DialContext(dialCtx, client.Options{HostPort: *temporalAddress})
		cancelDial()
		if dialErr != nil {
			return fmt.Errorf("dial Temporal at %s: %w", *temporalAddress, dialErr)
		}
		defer temporalClient.Close()
	}
	// Best-effort, same convention as the direct-run startup path's own
	// call to these (see its own doc comment): a project that has never
	// used sandboxing at all shouldn't have plain worktree reconciliation
	// start failing because Docker itself is unreachable for unrelated
	// reasons. Run before the worktree/marker reconciliation below so a
	// container-caused liveSandboxContainerName check inside it sees
	// up-to-date container state, not one about to change underneath it.
	if removed, reconcileErr := sandbox.ReconcileOrphans(context.Background(), *sandboxDocker, absDataDir); reconcileErr != nil {
		log.Printf("sandbox: orphaned container reconciliation for %s: %v", absDataDir, reconcileErr)
	} else if len(removed) > 0 {
		log.Printf("sandbox: removed %d orphaned container(s) from a prior run: %v", len(removed), removed)
	}
	if removed, reconcileErr := sandbox.ReconcileRelayOrphans(context.Background(), *sandboxDocker, absDataDir); reconcileErr != nil {
		log.Printf("sandbox: orphaned relay reconciliation for %s: %v", absDataDir, reconcileErr)
	} else if len(removed) > 0 {
		log.Printf("sandbox: removed %d orphaned sidecar resource(s) from a prior run: %v", len(removed), removed)
	}
	// scratchRunInFlight, same predicate the direct-run startup path's own
	// removeScratchDirs call already shares: a compose-services project/
	// network is not live under exactly the same condition a run's scratch
	// cache isn't -- its owning run's record is missing or terminal.
	if removed, reconcileErr := sandbox.ReconcileComposeServicesOrphans(context.Background(), *sandboxDocker, absDataDir, scratchRunInFlight(absDataDir), sandbox.ComposeServicesOrphanHooks{}); reconcileErr != nil {
		log.Printf("sandbox: orphaned compose services reconciliation for %s: %v", absDataDir, reconcileErr)
	} else if len(removed) > 0 {
		log.Printf("sandbox: removed %d orphaned compose services resource(s) from a prior run: %v", len(removed), removed)
	}
	// Propagated, unlike the direct-run startup path's own best-effort
	// call to this same function (found via review): this command's only
	// job is reconciliation, so a caller or script needs its exit status
	// to actually distinguish "cleaned up" from "tried and failed" --
	// silently returning nil either way made every failure invisible.
	return reconcileIsolationMarkers(context.Background(), absDataDir, *workspace, *sandboxDocker, lock, temporalClient)
}
