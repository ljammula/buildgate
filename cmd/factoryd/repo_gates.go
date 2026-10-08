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
