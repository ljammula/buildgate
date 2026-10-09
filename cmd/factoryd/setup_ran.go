package main

import (
	"slices"

	"buildgate/internal/run"
	"buildgate/internal/workflow"
)

// staleWorkerSetupMessage is the Command of the failed canonical_verify
// result requireSetupRan adds: the evidence shows why the run was refused.
const staleWorkerSetupMessage = "buildgate: the repository's setup commands did not run before verify (stale worker)"

// requireSetupRan quarantines an accepted result whose canonical verify did
// not run the repository's setup commands first. The workflow runs them in
// every verify sandbox, so a verify attempt without them means the workflow
// code that ran predates `setup:` (a long-lived Worker started before an
// upgrade): the run was judged in an environment its repository does not
// verify in. A run with no setup commands is returned as it is.
func requireSetupRan(setup []string, result workflow.RunWorkflowResult) workflow.RunWorkflowResult {
	if len(setup) == 0 || result.State != run.StateAccepted {
		return result
	}
	seen := false
	for _, a := range result.Attempts {
		if a.Kind != "verify" {
			continue
		}
		seen = true
		if !carriesSetup(a.Command, setup) {
			seen = false
			break
		}
	}
	if seen {
		return result
	}
	result.State = run.StateQuarantined
	result.GateResults = append(result.GateResults, run.GateResult{
		Check:    "canonical_verify",
		Command:  []string{staleWorkerSetupMessage},
		ExitCode: -1,
	})
	return result
}

// carriesSetup reports whether command is the setup form of a step
// (workflow stepCommand) with exactly the setup commands: the fixed script's
// arguments are the step's command, then each setup command.
func carriesSetup(command, setup []string) bool {
	const setupArgsFrom = 5
	return len(command) == setupArgsFrom+len(setup) && command[3] == "buildgate-setup" && slices.Equal(command[setupArgsFrom:], setup)
}
