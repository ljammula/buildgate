package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"buildgate/internal/release"
	"buildgate/internal/run"
)

// newOverrideFlags builds `factoryd override`'s FlagSet in isolation from
// parsing, so USAGE.md's doc-vs-flag drift test
// (TestUSAGEDocFlagsExistOnSubcommand) can enumerate its real flags without
// executing the command.
func newOverrideFlags() (flags *flag.FlagSet, runID, dataDir, by, reason, state, releaseProtectedPaths *string, releaseMaxFilesChanged, releaseMaxInsertions *int, releaseRollbackPlan *string, releaseAllowOverrides, releaseAllowDependencyLockfileChanges, releaseAllowUnsandboxed, releaseAllowSkippedProjectCheck *bool) {
	flags = flag.NewFlagSet("override", flag.ContinueOnError)
	runID = flags.String("run", "", "run identifier (required)")
	dataDir = flags.String("data-dir", "data", "directory containing durable run records")
	by = flags.String("by", "", "name of the human applying the override (required)")
	reason = flags.String("reason", "", "justification for the override (required)")
	state = flags.String("state", "", "new terminal state: accepted or halted (required)")
	releaseProtectedPaths = flags.String("release-protected-paths", "", "internal/release.MergePolicy.ProtectedPaths, comma-separated -- see runMain's own flag of the same name")
	releaseMaxFilesChanged = flags.Int("release-max-files-changed", 0, "internal/release.MergePolicy.MaxFilesChanged -- see runMain's own flag of the same name")
	releaseMaxInsertions = flags.Int("release-max-insertions", 0, "internal/release.MergePolicy.MaxInsertions -- see runMain's own flag of the same name")
	releaseRollbackPlan = flags.String("release-rollback-plan", "", "internal/release.MergePolicy.RollbackPlan -- see runMain's own flag of the same name")
	releaseAllowOverrides = flags.Bool("release-allow-overrides", false, "internal/release.MergePolicy.AllowOverrides -- see runMain's own flag of the same name")
	releaseAllowDependencyLockfileChanges = flags.Bool("release-allow-dependency-lockfile-changes", false, "internal/release.MergePolicy.AllowDependencyLockfileChanges -- see runMain's own flag of the same name")
	releaseAllowUnsandboxed = flags.Bool("release-allow-unsandboxed", false, "internal/release.MergePolicy.AllowUnsandboxed -- see runMain's own flag of the same name")
	releaseAllowSkippedProjectCheck = flags.Bool("release-allow-skipped-project-check", false, "internal/release.MergePolicy.AllowSkippedProjectCheck -- see runMain's own flag of the same name")
	plainFlagUsage(flags)
	return
}

func overrideMain(dp *deps, args []string) error {
	flags, runID, dataDir, by, reason, state, releaseProtectedPaths, releaseMaxFilesChanged, releaseMaxInsertions, releaseRollbackPlan, releaseAllowOverrides, releaseAllowDependencyLockfileChanges, releaseAllowUnsandboxed, releaseAllowSkippedProjectCheck := newOverrideFlags()
	if err := flags.Parse(args); err != nil {
		return err
	}
	if err := resolveDataDirFromSessionConfig(flags, dataDir, ""); err != nil {
		return err
	}
	if *runID == "" || *by == "" || *reason == "" || *state == "" {
		flags.Usage()
		return fmt.Errorf("-run, -by, -reason, and -state are required")
	}

	var newState run.State
	switch *state {
	case string(run.StateAccepted):
		newState = run.StateAccepted
	case string(run.StateHalted):
		newState = run.StateHalted
	default:
		return fmt.Errorf("-state must be %q or %q", run.StateAccepted, run.StateHalted)
	}

	// Shared with internal/api's own POST /runs/{id}/override endpoint —
	// see run.WithLock's own doc comment for why: this CLI subcommand and
	// that HTTP endpoint can both mutate the same run.json from entirely
	// separate processes, and only a real OS-level lock (not an
	// in-process mutex, which either caller alone could not see the
	// other's) can exclude them from each other.
	return run.WithLock(*dataDir, *runID, func() error {
		r, err := run.Load(*dataDir, *runID)
		if err != nil {
			return fmt.Errorf("load run %q: %w", *runID, err)
		}
		if err := r.ApplyOverride(*by, *reason, newState, func() string {
			return time.Now().Format(time.RFC3339)
		}); err != nil {
			return fmt.Errorf("override run %q: %w", *runID, err)
		}
		// The run was not accepted when its build ended, so nothing was read
		// about its root AGENTS.md then: read it now, before the one save
		// that persists it and before the release decision below.
		if newState == run.StateAccepted {
			recordMemoryEdit(dp, r, *dataDir, overrideObjectsDir(r))
		}
		if err := save(r, *dataDir); err != nil {
			return fmt.Errorf("save overridden run %q: %w", *runID, err)
		}
		// Found via Codex review of PR #45: an override to accepted is
		// exactly the AllowOverrides=true case internal/release.MergePolicy
		// exists to evaluate, but neither this path nor the API's own
		// override endpoint ever called recordReleaseDecision -- an
		// overridden run had no release-decision file at all, so
		// AllowOverrides could never be evaluated for the runs it's
		// actually meant to cover. r.ProjectPath, not a -workspace flag:
		// this subcommand only ever operates on an already-durable run
		// record, which already carries it.
		if newState == run.StateAccepted {
			recordReleaseDecision(*dataDir, *runID, r, releasePolicyFromFlags(*releaseProtectedPaths, *releaseMaxFilesChanged, *releaseMaxInsertions, *releaseRollbackPlan, *releaseAllowOverrides, *releaseAllowDependencyLockfileChanges, *releaseAllowUnsandboxed, *releaseAllowSkippedProjectCheck))
		}
		// Found via codex review (round 2, 2026-08-28): the run-time
		// rollback defer in runMainWithReady deliberately skips
		// StateQuarantined because an operator override might still
		// promote it to accepted — but an override *to* StateHalted is
		// exactly that decision resolved the other way, and nothing else
		// ever revisits a quarantined isolated run's worktree afterward.
		// Without this, overriding to halted left the isolated worktree
		// and branch behind indefinitely. r.Branch is only ever set for
		// an isolated run (see wsisolation.Prepare), so this is a no-op
		// for every non-isolated run's override, exactly as it should be.
		if newState == run.StateHalted && r.Branch != "" {
			// The worktree may be the only place the build's report and
			// round logs exist (a run kept for resume never had its
			// evidence collected): copy them out first.
			if err := run.RetainBuildArtifacts(r.WorkspacePath, run.Dir(*dataDir, r.ID)); err != nil {
				log.Printf("run %s: before override rollback: %v", r.ID, err)
			}
			if err := release.Rollback(r.ProjectPath, r.WorkspacePath, r.Branch, r.OnBranch != ""); err != nil {
				log.Printf("run %s: rollback of isolated workspace after override failed: %v", r.ID, err)
			} else {
				if r.KeptForResume {
					r.KeptForResume = false
					if err := r.Persist(*dataDir); err != nil {
						log.Printf("run %s: clear kept-for-resume after override rollback: %v", r.ID, err)
					}
				}
			}
		}
		if _, err := fmt.Fprintf(os.Stdout, "run %s: state=%s\n", r.ID, r.State); err != nil {
			return fmt.Errorf("print override result: %w", err)
		}
		return nil
	})
}

// overrideObjectsDir is the checkout whose git objects hold r's commits: its
// own worktree while that exists, else the repository it was cut from.
func overrideObjectsDir(r *run.Run) string {
	if r.WorkspacePath != "" {
		if info, err := os.Stat(r.WorkspacePath); err == nil && info.IsDir() {
			return r.WorkspacePath
		}
	}
	return r.ProjectPath
}

// overrideRateMain scans every durable run record under -data-dir and
// reports how many required a human override to reach run.StateAccepted.
//
// Per the plan's 2026-08-28 Opus review, this is the single most
// decision-relevant number the factory has produced and the one least
// visible: it lives only in scattered run.json Overrides fields, nothing
// surfaces it, and no test asserts on it. It is also the direct measure
// of Phase 7's precondition ("N accepted slices with zero required human
// interventions") — under an unattended multi-slice policy, every
// override is a line stop, so this command exists to make that count
// impossible to lose track of as real runs accumulate.
// newOverrideRateFlags builds `factoryd override-rate`'s FlagSet in
// isolation from parsing, so USAGE.md's doc-vs-flag drift test
// (TestUSAGEDocFlagsExistOnSubcommand) can enumerate its real flags without
// executing the command.
func newOverrideRateFlags() (flags *flag.FlagSet, dataDir *string) {
	flags = flag.NewFlagSet("override-rate", flag.ContinueOnError)
	dataDir = flags.String("data-dir", "data", "directory containing durable run records")
	plainFlagUsage(flags)
	return
}

func overrideRateMain(args []string) error {
	flags, dataDir := newOverrideRateFlags()
	if err := flags.Parse(args); err != nil {
		return err
	}
	if err := resolveDataDirFromSessionConfig(flags, dataDir, ""); err != nil {
		return err
	}

	entries, err := os.ReadDir(filepath.Join(*dataDir, "runs"))
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Println("no runs recorded yet")
			return nil
		}
		return fmt.Errorf("read runs dir: %w", err)
	}

	var total, accepted, acceptedViaOverride, anyOverride int
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		r, err := run.Load(*dataDir, entry.Name())
		if err != nil {
			if os.IsNotExist(err) {
				continue // a run directory that never got a durable run.json
			}
			return fmt.Errorf("load run %q: %w", entry.Name(), err)
		}
		total++
		hasOverride := len(r.Overrides) > 0
		if hasOverride {
			anyOverride++
		}
		if r.State == run.StateAccepted {
			accepted++
			if hasOverride {
				acceptedViaOverride++
			}
		}
	}

	fmt.Printf("runs recorded: %d\n", total)
	fmt.Printf("reached accepted: %d\n", accepted)
	fmt.Printf("accepted via human override: %d\n", acceptedViaOverride)
	fmt.Printf("any override recorded (accepted or not): %d\n", anyOverride)
	if accepted > 0 {
		fmt.Printf("override rate among accepted runs: %d%%\n", (100*acceptedViaOverride)/accepted)
	}
	return nil
}
