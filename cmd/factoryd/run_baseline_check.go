package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"buildgate/internal/forge"
	"buildgate/internal/run"
	"buildgate/internal/workflow"
)

// baselineCheckSpec is the ticket a -baseline-check run carries: the real
// run path needs one, and this one names nothing to build. The run stops
// after its baseline (workflow.RunWorkflowInput.BaselineCheck).
const baselineCheckSpec = `# Ticket: baseline check

Checks that the repository's verify and full-suite commands pass in the
sandbox before any model call. Nothing is built.

## Goal

None: the run stops after the baseline.

## Required changes

None.

## Out of scope

Everything.
`

// baselineChecksDir is the directory, inside a data dir, that holds each
// baseline check's own directory and data dir.
const baselineChecksDir = "baseline-checks"

// prepareBaselineCheck turns a -baseline-check run into a run of the real
// path with nothing to build: a fixed ticket id, baselineCheckSpec, and a
// data directory of its own, <data dir>/baseline-checks/<id>/data (inside the
// data dir, so the sandbox sees it wherever the data dir is shared; status and
// the console read only <data dir>/runs), removed when the command returns; a
// failed check keeps its logs (finishBaselineCheck).
func (tr *ticketRun) prepareBaselineCheck() error {
	if *tr.ticket != "" || *tr.spec != "" {
		return fmt.Errorf("-baseline-check takes no -ticket or -spec: it builds nothing")
	}
	if *tr.workspace == "" {
		tr.flags.Usage()
		return fmt.Errorf("-baseline-check needs -workspace")
	}
	for name, set := range map[string]bool{
		"-repository": *tr.repository != "", "-prior-run": *tr.priorRun != "", "-on-branch": *tr.onBranch != "",
		"-request-ticket": *tr.rf.requestTicket, "-open-pull-request": *tr.rf.openPullRequest, "-ticket-file": *tr.rf.ticketFile != "",
		"-reference-oracle-dir": *tr.rf.referenceOracleDir != "", "-resume-worktree-of": *tr.rf.resumeWorktreeOf != "", "-run-id": *tr.rf.runID != "",
	} {
		if set {
			return fmt.Errorf("-baseline-check checks the repository's HEAD on its own and builds nothing: it takes no %s", name)
		}
	}
	dataDir, err := filepath.Abs(*tr.dataDir)
	if err != nil {
		return fmt.Errorf("-baseline-check: resolve -data-dir: %w", err)
	}
	tr.approvalDataDir = dataDir
	checkDir := filepath.Join(dataDir, baselineChecksDir, "check-"+time.Now().UTC().Format("20060102-150405.000"))
	if err := os.MkdirAll(checkDir, 0o750); err != nil {
		return fmt.Errorf("-baseline-check: create %s: %w", checkDir, err)
	}
	specPath := filepath.Join(checkDir, "baseline-check.spec.md")
	if err := os.WriteFile(specPath, []byte(baselineCheckSpec), 0o600); err != nil {
		return fmt.Errorf("-baseline-check: write its ticket: %w", err)
	}
	*tr.ticket, *tr.spec, *tr.dataDir = "baseline-check", specPath, filepath.Join(checkDir, "data")
	// The check is about the commands, not the repository's documents:
	// doctor and submit report the project preflight on their own.
	*tr.skipProjectCheck = true
	// A check always runs the full-suite command: no cadence skips it.
	*tr.fullSuiteCadence = 1
	tr.atExit(func() { finishBaselineCheck(checkDir) })
	return nil
}

// finishBaselineCheck removes the check's run records first, so nothing that
// counts runs (make baseline reads every runs/*/run.json under ~/buildgate)
// ever counts a check, then the rest of its directory, unless the run halted:
// a halt keeps the logs and names them. Only a halt on the baseline itself
// is reported as a failed check; any other halt (a timeout, an interrupt, an
// unreachable server) is reported as a check that did not finish.
func finishBaselineCheck(checkDir string) {
	dataDir := filepath.Join(checkDir, "data")
	records, _ := filepath.Glob(filepath.Join(dataDir, "runs", "*", "run.json"))
	halted, reason := false, ""
	var haltedRun *run.Run
	for _, path := range records {
		if r, err := run.Load(dataDir, filepath.Base(filepath.Dir(path))); err == nil && r.State == run.StateHalted {
			halted, reason = true, r.HaltReasonCode
			haltedRun = r
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "baseline check: could not remove its run record %s: %v\n", path, err)
		}
	}
	if !halted {
		if err := os.RemoveAll(checkDir); err != nil {
			fmt.Fprintf(os.Stderr, "baseline check: could not remove %s: %v\n", checkDir, err)
		}
		return
	}
	if reason == run.HaltReasonBaselineVerifyFailed {
		printBaselineCheckFailure(haltedRun)
		fmt.Fprintf(os.Stderr, "baseline check failed; its logs are in %s\n", checkDir)
		return
	}
	fmt.Fprintf(os.Stderr, "baseline check did not finish (%s); its logs are in %s\n", reason, checkDir)
}

// baselineCheckAttempt returns the last attempt of kind. A retry is the
// command's result for output purposes, just as it is in the run record.
func baselineCheckAttempt(attempts []run.Attempt, kind string) (run.Attempt, bool) {
	for i := len(attempts) - 1; i >= 0; i-- {
		if attempts[i].Kind == kind {
			return attempts[i], true
		}
	}
	return run.Attempt{}, false
}

// baselineCheckDuration formats the recorded wall time. The baseline record
// has the exact duration for verify; attempt timestamps cover full suite and
// older records that predate the duration field.
func baselineCheckDuration(attempts []run.Attempt, kind string, durationMs int64) string {
	if durationMs > 0 {
		return (time.Duration(durationMs) * time.Millisecond).String()
	}
	attempt, ok := baselineCheckAttempt(attempts, kind)
	if !ok {
		return "unknown"
	}
	started, startErr := time.Parse(time.RFC3339, attempt.StartedAt)
	finished, finishErr := time.Parse(time.RFC3339, attempt.FinishedAt)
	if startErr != nil || finishErr != nil || finished.Before(started) {
		return "unknown"
	}
	return finished.Sub(started).String()
}

func baselineCheckAttemptSummary(attempt run.Attempt) string {
	if attempt.ExitCode == 0 {
		return "passed"
	}
	return fmt.Sprintf("failed: exit %d", attempt.ExitCode)
}

// baselineCheckFailureDetail keeps the one-line output useful for a full
// suite halt, whose baseline record only describes verify. The run's triage
// sentence is factory-authored and is the best extra detail when present.
func baselineCheckFailureDetail(r *run.Run, fallback string) string {
	if r != nil && strings.TrimSpace(r.Triage) != "" {
		return fallback + "; " + strings.TrimSpace(r.Triage)
	}
	return fallback
}

func printBaselineCheckFailure(r *run.Run) {
	if r == nil {
		return
	}
	if b := r.BaselineVerify; b != nil {
		fmt.Fprintf(os.Stderr, "baseline check: verify %s: %s\n", baselineCheckDuration(r.Attempts, run.BaselineVerifyAttemptKind, b.DurationMs), b.Summary())
	} else if attempt, ok := baselineCheckAttempt(r.Attempts, run.BaselineVerifyAttemptKind); ok {
		fmt.Fprintf(os.Stderr, "baseline check: verify %s: %s\n", baselineCheckDuration(r.Attempts, run.BaselineVerifyAttemptKind, 0), baselineCheckAttemptSummary(attempt))
	}
	if attempt, ok := baselineCheckAttempt(r.Attempts, run.FullSuiteVerifyAttemptKind); ok {
		fmt.Fprintf(os.Stderr, "baseline check: full suite %s: %s\n", baselineCheckDuration(r.Attempts, run.FullSuiteVerifyAttemptKind, 0), baselineCheckFailureDetail(r, baselineCheckAttemptSummary(attempt)))
	}
}

// finishRunWorkflow is awaitRunWorkflow's end once the workflow returned: a
// passed -baseline-check built nothing, so it records no verdict and reaches
// no release; every other result is applied to the run record.
func finishRunWorkflow(dp *deps, r *run.Run, opts runOptions, taskQueue string, result workflow.RunWorkflowResult) error {
	if result.BaselineChecked {
		baseline, _ := run.LoadBaselineVerify(run.Dir(opts.DataDir, opts.ID))
		verifySummary, verifyDuration := "passed", int64(0)
		if baseline != nil {
			verifySummary, verifyDuration = baseline.Summary(), baseline.DurationMs
		}
		fmt.Printf("baseline check: verify %s: %s\n", baselineCheckDuration(result.Attempts, run.BaselineVerifyAttemptKind, verifyDuration), verifySummary)
		if attempt, ok := baselineCheckAttempt(result.Attempts, run.FullSuiteVerifyAttemptKind); ok {
			fmt.Printf("baseline check: full suite %s: %s\n", baselineCheckDuration(result.Attempts, run.FullSuiteVerifyAttemptKind, 0), baselineCheckAttemptSummary(attempt))
		}
		return nil
	}
	return applyRunWorkflowResult(dp, r, opts.DataDir, opts.ID, opts.Ticket, opts.WorkspacePath, opts.BaseSHA, taskQueue, result, true, &opts.ReleasePolicy, forge.GHPullRequestOpener{}, false)
}
