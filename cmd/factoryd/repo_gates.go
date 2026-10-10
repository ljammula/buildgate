package main

import (
	"buildgate/internal/policy"
	"buildgate/internal/run"
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

// repoGateCommands is the repo-defined gates among a run's gate commands,
// by check name: what the run record keeps (run.Run.RepoGateCommands). nil
// when the repository defines none.
func repoGateCommands(gateCommands map[string]string) map[string]string {
	var out map[string]string
	for _, check := range policy.RepoGateChecks(gateCommands) {
		if out == nil {
			out = map[string]string{}
		}
		out[check] = gateCommands[check]
	}
	return out
}

// requireCurrentWorker runs, on a result about to be applied to its run
// record, every guard against a worker older than what the run was
// dispatched with: requireRepoGateResults and requireSetupRan. Every path
// that applies a workflow result calls it first: the submitter with its own
// options, a reclaim with the lists on the run record.
func requireCurrentWorker(gateCommands map[string]string, setup []string, result workflow.RunWorkflowResult) workflow.RunWorkflowResult {
	result = requireRepoGateResults(gateCommands, result)
	return requireSetupRan(setup, result)
}
