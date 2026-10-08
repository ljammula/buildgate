package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"go.temporal.io/sdk/client"

	"buildgate/internal/release"
	"buildgate/internal/run"
	"buildgate/internal/workflow"
)

// resetStopLineMain clears a RepositoryOwnerWorkflow's tripped stop line —
// the one and only way to resume a repository the owner has halted after
// recurring infrastructure failures, a quarantined run, or a child that
// committed to the shared workspace before failing (see
// RepositoryOwnerWorkflow's own stop-line doc comments): none of those
// trip conditions have any other durable recovery path once set. Requires
// -by/-reason, the same attribution convention `override` already
// enforces for a per-run decision — the owner itself cannot verify the
// shared workspace was actually restored or otherwise confirmed safe
// before resuming dispatch; that judgment is entirely this operator's own.
// newResetStopLineFlags builds `factoryd reset-stop-line`'s FlagSet in
// isolation from parsing, so USAGE.md's doc-vs-flag drift test
// (TestUSAGEDocFlagsExistOnSubcommand) can enumerate its real flags without
// executing the command.
func newResetStopLineFlags() (flags *flag.FlagSet, temporalAddress, repository, by, reason *string) {
	flags = flag.NewFlagSet("reset-stop-line", flag.ContinueOnError)
	temporalAddress = flags.String("temporal-address", "", "Temporal server address (required)")
	repository = flags.String("repository", "", "repository identity whose stop line to reset (required)")
	by = flags.String("by", "", "name of the human resetting the stop line (required)")
	reason = flags.String("reason", "", "justification for resetting the stop line (required, e.g. confirmation the shared workspace was restored)")
	plainFlagUsage(flags)
	return
}

func resetStopLineMain(args []string) error {
	flags, temporalAddress, repository, by, reason := newResetStopLineFlags()
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *temporalAddress == "" || *repository == "" || *by == "" || *reason == "" {
		flags.Usage()
		return fmt.Errorf("-temporal-address, -repository, -by, and -reason are required")
	}

	temporalClient, err := client.Dial(client.Options{HostPort: *temporalAddress})
	if err != nil {
		return fmt.Errorf("dial Temporal at %s: %w", *temporalAddress, err)
	}
	defer temporalClient.Close()

	ownerID := workflow.RepositoryOwnerWorkflowID(*repository)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// The owner rejects a reset whose Generation doesn't match its own
	// current RepositoryOwnerResult.StopLineGeneration (see that field's
	// own doc comment for why: an authorization issued before a trip it
	// never observed must not resume work past it). Querying immediately
	// before signaling keeps that window as narrow as this CLI invocation
	// can make it — it cannot be fully closed from outside the workflow,
	// since a new trip could still land between this query and the signal
	// actually being drained, which is exactly the case the generation
	// check itself exists to reject rather than requiring this caller to
	// somehow prevent.
	queryResp, err := temporalClient.QueryWorkflow(ctx, ownerID, "", workflow.RepositoryOwnerQueryName)
	if err != nil {
		return fmt.Errorf("query repository owner %s for its current stop-line state: %w", ownerID, err)
	}
	var ownerResult workflow.RepositoryOwnerResult
	if err := queryResp.Get(&ownerResult); err != nil {
		return fmt.Errorf("decode repository owner %s query result: %w", ownerID, err)
	}
	if !ownerResult.StopLineTripped {
		return fmt.Errorf("repository %q: stop line is not currently tripped; nothing to reset", *repository)
	}

	if err := temporalClient.SignalWorkflow(ctx, ownerID, "", workflow.ResetStopLineSignalName,
		workflow.ResetStopLineSignal{By: *by, Reason: *reason, Generation: ownerResult.StopLineGeneration},
	); err != nil {
		return fmt.Errorf("signal repository owner %s to reset its stop line: %w", ownerID, err)
	}
	if _, err := fmt.Fprintf(os.Stdout, "repository %q: stop-line reset signaled (by=%s, generation=%d)\n", *repository, *by, ownerResult.StopLineGeneration); err != nil {
		return fmt.Errorf("print reset result: %w", err)
	}
	return nil
}

// killSwitchMain is the operator surface for internal/release's per-project
// kill switch: the Phase 7 precondition the plan states as "a kill switch
// exists that a human can hit to halt all autonomous merges/deploys for a
// project instantly, independent of the rest of the system".
//
// release.Engage/Disengage/LoadKillSwitch were durable, attributable, and
// unit-tested, but had no caller anywhere outside their own tests -- so
// while every accepted run's release decision consults the switch (via
// release.EvaluateDecision, wired in PR #45), no operator could actually
// engage it. This closes that half, and nothing more: it reads or
// transitions the durable record and performs no merge, deploy, run-state,
// or workspace side effect. An engaged switch denies future release
// decisions exactly as EvaluateDecision already implements; it does not
// retroactively change decisions already recorded.
//
// -by/-reason are required for a transition for the same reason
// run.ApplyOverride requires them: an unattributable safety action is not
// auditable evidence. Reading the current state requires neither.
//
// Concurrent invocations of this command for one project serialize against
// each other through a real OS-level advisory lock, not merely an
// in-process mutex — see release.withKillSwitchLock, and internal/run's
// WithLock for the same precedent on run records.
// anyRunRecordedUnderProject reports whether any durable run record's
// project id (release.ProjectOf: r.Project, falling back to the legacy
// derivation from r.ProjectPath for a run recorded before that field
// existed) equals project. killSwitchMain uses
// this to warn an operator who typed a project id that no run has ever
// actually derived -- see its own doc comment (2026-09-05 Opus review,
// S4) for why a silent no-op there is the one place this factory can
// least afford it: the kill switch is the last-resort control, and
// before this check, engaging it under a typo'd or mismatched id
// protected nothing and reported no error on either side.
func anyRunRecordedUnderProject(dataDir, project string) (bool, error) {
	entries, err := os.ReadDir(filepath.Join(dataDir, "runs"))
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		r, loadErr := run.Load(dataDir, entry.Name())
		if loadErr != nil {
			continue
		}
		if release.ProjectOf(r) == project {
			return true, nil
		}
	}
	return false, nil
}

// newKillSwitchFlags builds `factoryd kill-switch`'s FlagSet in isolation
// from parsing, so USAGE.md's doc-vs-flag drift test
// (TestUSAGEDocFlagsExistOnSubcommand) can enumerate its real flags without
// executing the command.
func newKillSwitchFlags() (flags *flag.FlagSet, dataDir, project, state, by, reason *string) {
	flags = flag.NewFlagSet("kill-switch", flag.ContinueOnError)
	dataDir = flags.String("data-dir", "data", "directory containing durable project records")
	project = flags.String("project", "", "project whose kill switch to read or change (required) -- the same single path component release decisions are recorded under")
	state = flags.String("state", "", "new state: engaged or disengaged. Omit to report the current state and history without changing anything")
	by = flags.String("by", "", "name of the human changing the switch (required with -state)")
	reason = flags.String("reason", "", "justification for the change (required with -state)")
	plainFlagUsage(flags)
	return
}

func killSwitchMain(args []string) error {
	flags, dataDir, project, state, by, reason := newKillSwitchFlags()
	if err := flags.Parse(args); err != nil {
		return err
	}
	if err := resolveDataDirFromSessionConfig(flags, dataDir, ""); err != nil {
		return err
	}
	if *project == "" {
		flags.Usage()
		return fmt.Errorf("-project is required")
	}
	// Best-effort and non-fatal: an operator engaging the switch for a
	// project that has never yet run anything (e.g. ahead of that
	// project's very first run) is legitimate, so this warns rather than
	// refuses. What it closes is the silent-no-op case: -project typed
	// from memory that does not match what any run has actually derived
	// (e.g. -workspace pointed at a repository root rather than
	// <root>/workspace) previously engaged a switch that protected
	// nothing, with no signal on either side.
	if recorded, err := anyRunRecordedUnderProject(*dataDir, *project); err != nil {
		log.Printf("kill-switch: warning: could not check existing runs for project %q: %v", *project, err)
	} else if !recorded {
		fmt.Fprintf(os.Stderr, "warning: no durable run record has ever derived project id %q -- this may not be the id release decisions for your workspace actually land under (see GET /projects or internal/release.ProjectFromWorkspace); this kill switch will engage %q regardless\n", *project, *project)
	}
	var record *release.KillSwitchRecord
	if *state != "" {
		var engaged bool
		switch *state {
		case "engaged":
			engaged = true
		case "disengaged":
		default:
			return fmt.Errorf(`-state must be "engaged" or "disengaged"`)
		}
		if *by == "" || *reason == "" {
			flags.Usage()
			return fmt.Errorf("-by and -reason are required with -state")
		}
		// SetKillSwitch, not a transition followed by a separate
		// LoadKillSwitch: it reads the resulting record back under the same
		// cross-process lock that performed the transition, so what this
		// command prints is the outcome of the transition it just applied
		// rather than possibly a different one a concurrent invocation
		// wrote in between. It is not a claim that the printed state is
		// still current -- see SetKillSwitch's own doc comment.
		applied, err := release.SetKillSwitch(*dataDir, *project, *by, *reason, engaged, func() string {
			return time.Now().Format(time.RFC3339)
		})
		if err != nil {
			return fmt.Errorf("set kill switch for project %q: %w", *project, err)
		}
		record = applied
	} else {
		loaded, err := release.LoadKillSwitch(*dataDir, *project)
		if err != nil {
			return fmt.Errorf("load kill switch for project %q: %w", *project, err)
		}
		record = loaded
	}
	if _, err := fmt.Fprintf(os.Stdout, "project %q: kill switch engaged=%t (%d recorded transitions)\n", record.Project, record.Engaged, len(record.History)); err != nil {
		return fmt.Errorf("print kill switch state: %w", err)
	}
	for _, transition := range record.History {
		if _, err := fmt.Fprintf(os.Stdout, "  engaged=%t by=%s at=%s reason=%s\n", transition.Engaged, transition.By, transition.At, transition.Reason); err != nil {
			return fmt.Errorf("print kill switch history: %w", err)
		}
	}
	return nil
}
