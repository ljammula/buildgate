package main

import (
	"strings"
	"testing"

	"buildgate/internal/handoff"
	"buildgate/internal/run"
	"buildgate/internal/workflow"
)

// gatewayShape is a step's recorded command as the sandbox runtime reports
// it: the worker wrapper's argv around the step's own, whatever that is. The
// guard must never read it.
func gatewayShape(stepArgv ...string) []string {
	return append([]string{"/bin/sh", "-c", "guard script", "--"}, stepArgv...)
}

func acceptedWith(attempts ...run.Attempt) workflow.RunWorkflowResult {
	return workflow.RunWorkflowResult{
		State:       run.StateAccepted,
		Attempts:    attempts,
		GateResults: []run.GateResult{{Check: "canonical_verify", Passed: true}, {Check: "lint", Passed: true}},
	}
}

func TestSetupRanIsDecidedByTheDigestNotTheRecordedCommand(t *testing.T) {
	setup := []string{"make generate", "npm ci"}
	for _, cmd := range [][]string{
		gatewayShape("sh", "-c", "script", "buildgate-setup", "make verify", "make generate", "npm ci"),
		{"sh", "-c", "make verify"},
		nil,
	} {
		ran := acceptedWith(run.Attempt{Kind: "build"}, run.Attempt{Kind: "verify", Command: cmd, SetupSHA256: run.SetupDigest(setup)})
		if got := requireSetupRan(setup, ran); got.State != run.StateAccepted || len(got.GateResults) != 2 {
			t.Errorf("command %q: a verify that recorded the setup digest changed the result: %s, %+v", cmd, got.State, got.GateResults)
		}
	}
}

// A worker that predates setup: its verify attempt carries no digest (or a
// different list's). The run ends quarantined with its one canonical_verify
// result replaced by a never-ran result, which the handoff sorts into the
// operator's bin and no corrective build is started for.
func TestAStaleWorkersAcceptedResultIsTheOperators(t *testing.T) {
	setup := []string{"make generate"}
	for name, digest := range map[string]string{"no digest": "", "other list": run.SetupDigest([]string{"make other"})} {
		stale := acceptedWith(run.Attempt{Kind: "verify", Command: gatewayShape("sh", "-c", "make verify"), SetupSHA256: digest})
		got := requireSetupRan(setup, stale)
		if got.State != run.StateQuarantined {
			t.Fatalf("%s: state = %s, want quarantined", name, got.State)
		}
		canonical := 0
		for _, g := range got.GateResults {
			if g.Check == "canonical_verify" {
				canonical++
				if g.Passed || g.ExitCode != -1 || !g.SetupNotRun() || !strings.Contains(g.Command[0], "factoryd restart") {
					t.Errorf("%s: canonical_verify = %+v, want the never-ran result", name, g)
				}
			}
		}
		if canonical != 1 {
			t.Errorf("%s: %d canonical_verify results, want the existing one replaced", name, canonical)
		}
		r := &run.Run{ID: "stale", Ticket: "t", State: got.State, GateResults: got.GateResults}
		doc := handoff.Build(r, t.TempDir())
		if doc.Next != handoff.BinOperator {
			t.Errorf("%s: handoff Next = %q, want operator", name, doc.Next)
		}
	}
	if got := requireSetupRan(nil, acceptedWith(run.Attempt{Kind: "verify"})); got.State != run.StateAccepted {
		t.Errorf("a run with no setup commands was quarantined")
	}
	if got := requireSetupRan(setup, acceptedWith()); got.State != run.StateQuarantined {
		t.Errorf("an accepted result with no verify attempt was accepted")
	}
	halted := workflow.RunWorkflowResult{State: run.StateHalted}
	if got := requireSetupRan(setup, halted); got.State != run.StateHalted || len(got.GateResults) != 0 {
		t.Errorf("a result that is not accepted changed: %+v", got)
	}
}
