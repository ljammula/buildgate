package main

import (
	"buildgate/internal/run"
	"buildgate/internal/workflow"
)

// requireSetupRan quarantines an accepted result whose canonical verify did
// not run the repository's setup commands first. The workflow runs them in
// every verify sandbox and each verify attempt records the digest of the list
// it ran (run.Attempt.SetupSHA256), so an attempt without exactly the run's
// digest means the workflow code that ran predates `setup:` (a long-lived
// Worker started before an upgrade): the run was judged in an environment its
// repository does not verify in. A run with no setup commands is returned as
// it is.
//
// The run's canonical_verify result is replaced by one with exit -1 and
// run.SetupNotRunMessage, which the handoff sorts into the operator's bin, as
// a repository gate that never ran: no build can fix a stale worker.
func requireSetupRan(setup []string, result workflow.RunWorkflowResult) workflow.RunWorkflowResult {
	if len(setup) == 0 || result.State != run.StateAccepted {
		return result
	}
	want := run.SetupDigest(setup)
	seen := false
	for _, a := range result.Attempts {
		if a.Kind != "verify" {
			continue
		}
		if a.SetupSHA256 != want {
			seen = false
			break
		}
		seen = true
	}
	if seen {
		return result
	}
	result.State = run.StateQuarantined
	notRun := run.GateResult{Check: "canonical_verify", Command: []string{run.SetupNotRunMessage}, ExitCode: -1}
	gates := append([]run.GateResult(nil), result.GateResults...)
	for i, g := range gates {
		if g.Check == "canonical_verify" {
			gates[i] = notRun
			result.GateResults = gates
			return result
		}
	}
	result.GateResults = append(gates, notRun)
	return result
}
