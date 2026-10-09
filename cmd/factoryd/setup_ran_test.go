package main

import (
	"strings"
	"testing"

	"buildgate/internal/run"
	"buildgate/internal/workflow"
)

func setupFormCommand(command string, setup ...string) []string {
	return append([]string{"sh", "-c", "script", "buildgate-setup", command}, setup...)
}

// An accepted result whose canonical verify did not carry the run's setup
// commands was judged by a Worker that predates them: it is quarantined.
func TestAcceptedResultWithoutSetupIsNotAccepted(t *testing.T) {
	setup := []string{"make generate"}
	ran := workflow.RunWorkflowResult{State: run.StateAccepted, Attempts: []run.Attempt{
		{Kind: "build"}, {Kind: "verify", Command: setupFormCommand("make verify", "make generate")},
	}}
	if got := requireSetupRan(setup, ran); got.State != run.StateAccepted || len(got.GateResults) != 0 {
		t.Errorf("a result whose verify ran setup changed: state %s, gates %+v", got.State, got.GateResults)
	}
	stale := workflow.RunWorkflowResult{State: run.StateAccepted, Attempts: []run.Attempt{
		{Kind: "verify", Command: []string{"sh", "-c", "make verify"}},
	}}
	got := requireSetupRan(setup, stale)
	if got.State != run.StateQuarantined || len(got.GateResults) != 1 || got.GateResults[0].Check != "canonical_verify" || got.GateResults[0].Passed {
		t.Fatalf("stale result = state %s, gates %+v, want quarantined with a failed canonical_verify", got.State, got.GateResults)
	}
	if want := "stale worker"; !strings.Contains(got.GateResults[0].Command[0], want) {
		t.Errorf("gate command %q does not name a %s", got.GateResults[0].Command, want)
	}
	if got := requireSetupRan(nil, stale); got.State != run.StateAccepted {
		t.Errorf("a run with no setup commands was quarantined")
	}
	other := workflow.RunWorkflowResult{State: run.StateAccepted, Attempts: []run.Attempt{
		{Kind: "verify", Command: setupFormCommand("make verify", "make other")},
	}}
	if got := requireSetupRan(setup, other); got.State != run.StateQuarantined {
		t.Errorf("a verify that ran different setup commands was accepted")
	}
	halted := workflow.RunWorkflowResult{State: run.StateHalted}
	if got := requireSetupRan(setup, halted); got.State != run.StateHalted || len(got.GateResults) != 0 {
		t.Errorf("a result that is not accepted changed: %+v", got)
	}
}
