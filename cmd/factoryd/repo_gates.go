package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"

	"buildgate/internal/policy"
	"buildgate/internal/projectconfig"
	"buildgate/internal/run"
	"buildgate/internal/sanitize"
	"buildgate/internal/workflow"
)

// requireRepoGateResults quarantines an accepted result that has no gate
// result for a gate the target repository defines for itself. The workflow
// runs every one of them, so a missing result means the workflow code that
// ran predates repo-defined gates (a long-lived Worker started before an
// upgrade): the run was judged without a check its repository requires, and
// the evidence, which lists only gates that ran, would not show it.
func requireRepoGateResults(gateCommands map[string]string, result workflow.RunWorkflowResult) workflow.RunWorkflowResult {
	if result.State != run.StateAccepted {
		return result
	}
	ran := make(map[string]bool, len(result.GateResults))
	for _, g := range result.GateResults {
		ran[g.Check] = true
	}
	for _, check := range policy.RepoGateChecks(gateCommands) {
		if ran[check] {
			continue
		}
		result.State = run.StateQuarantined
		result.GateResults = append(result.GateResults, run.GateResult{
			Check:    check,
			Command:  []string{"sh", "-c", gateCommands[check]},
			ExitCode: -1,
		})
	}
	return result
}

// requireCurrentWorker runs, on a result about to be applied to its run
// record, every guard against a worker older than what the run was
// dispatched with: requireRepoGateResults and requireSetupRan. Every path
// that applies a workflow result calls it first: the submitter with its own
// options, a reclaim with the lists it reads again from the commit the run
// was dispatched from (requireCurrentWorkerAtReclaim).
func requireCurrentWorker(gateCommands map[string]string, setup []string, result workflow.RunWorkflowResult) workflow.RunWorkflowResult {
	result = requireRepoGateResults(gateCommands, result)
	return requireSetupRan(setup, result)
}

// requireCurrentWorkerAtReclaim is requireCurrentWorker for a result applied
// by a process other than the run's submitter, which holds none of the
// submitter's options. The repository's gates and setup commands are read
// again from .factory.yml as the commit on the run record holds it
// (dispatchedRepoChecks); the record itself carries no copy, so nothing an
// older worker's rewrite of the record can drop decides this. When they
// cannot be read as the record names them, an accepted result is refused:
// quarantined, with a canonical_verify result (exit -1) whose command is
// run.ReclaimNotCheckedMessage (a short reason, what the checkout must hold
// and what applies the result then), which is the operator's. A result that
// is not accepted is returned as it is, without a read.
func requireCurrentWorkerAtReclaim(ctx context.Context, dp *deps, r *run.Run, result workflow.RunWorkflowResult) workflow.RunWorkflowResult {
	if result.State != run.StateAccepted {
		return result
	}
	repoDir := firstNonEmptyString(r.RepositoryRoot, r.ProjectPath)
	gateCommands, setup, reason, err := dispatchedRepoChecks(ctx, dp, repoDir, r)
	if reason != "" {
		// The error's own text (git's, the parser's) stays in the log.
		log.Printf("run %s: reclaimed accepted result not applied: %s (repository %s, commit %s): %v", r.ID, reason, repoDir, r.ProjectConfigCommitSHA, err)
		return refuseAccepted(result, run.GateResult{
			Check:    "canonical_verify",
			Command:  []string{run.ReclaimNotCheckedMessage(reason, sanitize.Line(repoDir), r.ProjectConfigCommitSHA)},
			ExitCode: -1,
		})
	}
	return requireCurrentWorker(gateCommands, setup, result)
}

// The reasons a reclaimed result cannot be checked, as the operator reads
// them.
const (
	reclaimNoCommit         = "no commit on the run record"
	reclaimNoRepository     = "repository not found"
	reclaimNoSuchCommit     = "commit not found"
	reclaimFileDiffers      = "file differs"
	reclaimFileTooLarge     = "file too large"
	reclaimFileNotRegular   = "file is not a regular file"
	reclaimFileDoesNotParse = "file does not parse"
)

// dispatchedRepoChecks returns the repository's own gates (by check name) and
// setup commands r was dispatched with: those of .factory.yml as
// r.ProjectConfigCommitSHA holds it, read from git objects in repoDir (the
// operator's checkout), never from a worktree. The bytes must hash to
// r.ProjectConfigSHA256; a commit without the file must go with a record
// without a hash, which is a repository with no project config and so with
// neither gates nor setup. Anything else returns one of the reclaim reasons
// above, with the error behind it when there is one.
func dispatchedRepoChecks(ctx context.Context, dp *deps, repoDir string, r *run.Run) (gateCommands map[string]string, setup []string, reason string, err error) {
	if r.ProjectConfigCommitSHA == "" {
		return nil, nil, reclaimNoCommit, nil
	}
	if info, statErr := os.Stat(repoDir); statErr != nil || !info.IsDir() {
		return nil, nil, reclaimNoRepository, statErr
	}
	data, found, err := dp.host.blobAtCommit(ctx, repoDir, r.ProjectConfigCommitSHA, projectconfig.FileName)
	switch {
	case errors.Is(err, errBlobTooLarge):
		return nil, nil, reclaimFileTooLarge, err
	case errors.Is(err, errNotRegularBlob):
		return nil, nil, reclaimFileNotRegular, err
	case err != nil:
		return nil, nil, reclaimNoSuchCommit, err
	case !found && r.ProjectConfigSHA256 != "":
		return nil, nil, reclaimFileDiffers, errors.New("the commit has no such file, but the run read one")
	case !found:
		return nil, nil, "", nil
	}
	cfg, err := projectconfig.Parse(data)
	if err != nil {
		return nil, nil, reclaimFileDoesNotParse, err
	}
	if cfg.SHA256 != r.ProjectConfigSHA256 {
		return nil, nil, reclaimFileDiffers, fmt.Errorf("it hashes to %s, the run read %s", cfg.SHA256, r.ProjectConfigSHA256)
	}
	return cfg.RepoGateCommands(), cfg.Setup, "", nil
}
