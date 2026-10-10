package main

import (
	"context"
	"errors"
	"fmt"

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
// quarantined, with a canonical_verify result (exit -1) whose command says
// why (run.ReclaimNotCheckedPrefix), which is the operator's. A result that
// is not accepted is returned as it is, without a read.
func requireCurrentWorkerAtReclaim(ctx context.Context, dp *deps, r *run.Run, result workflow.RunWorkflowResult) workflow.RunWorkflowResult {
	if result.State != run.StateAccepted {
		return result
	}
	gateCommands, setup, err := dispatchedRepoChecks(ctx, dp, r)
	if err != nil {
		return refuseAccepted(result, run.GateResult{
			Check:    "canonical_verify",
			Command:  []string{run.ReclaimNotCheckedPrefix + sanitize.Line(err.Error())},
			ExitCode: -1,
		})
	}
	return requireCurrentWorker(gateCommands, setup, result)
}

// dispatchedRepoChecks returns the repository's own gates (by check name) and
// setup commands r was dispatched with: those of .factory.yml as
// r.ProjectConfigCommitSHA holds it, read from git objects in the operator's
// checkout, never from a worktree. The bytes must hash to
// r.ProjectConfigSHA256; a commit without the file must go with a record
// without a hash, which is a repository with no project config and so with
// neither gates nor setup. Anything else is an error: a record that names no
// commit, a commit or repository that cannot be read, a file that is not the
// one the run read or does not parse.
func dispatchedRepoChecks(ctx context.Context, dp *deps, r *run.Run) (map[string]string, []string, error) {
	if r.ProjectConfigCommitSHA == "" {
		return nil, nil, errors.New("the run record does not name the commit it was read from")
	}
	repoDir := firstNonEmptyString(r.RepositoryRoot, r.ProjectPath)
	short := r.ProjectConfigCommitSHA
	if len(short) > 12 {
		short = short[:12]
	}
	data, found, err := dp.host.blobAtCommit(ctx, repoDir, r.ProjectConfigCommitSHA, projectconfig.FileName)
	if err != nil {
		return nil, nil, fmt.Errorf("commit %s: %w", short, err)
	}
	if !found {
		if r.ProjectConfigSHA256 != "" {
			return nil, nil, fmt.Errorf("commit %s has no such file, but the run read one", short)
		}
		return nil, nil, nil
	}
	cfg, err := projectconfig.Parse(data)
	if err != nil {
		return nil, nil, err
	}
	if cfg.SHA256 != r.ProjectConfigSHA256 {
		return nil, nil, fmt.Errorf("the file at commit %s is not the one the run read", short)
	}
	return cfg.RepoGateCommands(), cfg.Setup, nil
}
