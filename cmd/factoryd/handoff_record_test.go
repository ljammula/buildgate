package main

import (
	"os"
	"path/filepath"
	"testing"

	"buildgate/internal/handoff"
	"buildgate/internal/run"
)

// TestSaveKeepsTheHandoffInStepWithTheRun: the record of what an attempt
// left is written with the run's stopped state and its hash is on the run;
// a later save in another stopped state rewrites it, and a save as accepted
// removes it.
func TestSaveKeepsTheHandoffInStepWithTheRun(t *testing.T) {
	dataDir := t.TempDir()
	failed := false
	r := &run.Run{
		ID: "run-1", Ticket: "fixture-ticket", State: run.StateHalted, HaltError: "context canceled",
		AgentEvidence: &run.AgentEvidence{Rounds: []run.AgentEvidenceRound{
			{Index: 1, VerifyPassed: &failed, Blockers: []string{"canonical verification failed"}, FailureSignature: "aaaa"},
		}},
	}
	if err := save(r, dataDir); err != nil {
		t.Fatalf("save: %v", err)
	}
	halted := r.HandoffSHA256
	if len(halted) != 64 {
		t.Fatalf("HandoffSHA256 = %q, want a SHA-256", halted)
	}
	doc, err := handoff.Load(run.Dir(dataDir, r.ID), halted, run.StateHalted)
	if err != nil || doc.Next != handoff.BinOperator {
		t.Fatalf("halted handoff = %+v, %v, want the operator bin", doc, err)
	}
	reloaded, err := run.Load(dataDir, r.ID)
	if err != nil || reloaded.HandoffSHA256 != halted {
		t.Fatalf("run.json HandoffSHA256 = %q, %v, want %q", reloaded.HandoffSHA256, err, halted)
	}

	// The delayed result arrives: the run was in fact quarantined by a check.
	r.State, r.Triage = run.StateQuarantined, ""
	r.GateResults = []run.GateResult{{Check: "tests_added", Passed: false}, {Check: "lint", Passed: false, ExitCode: 2}}
	if err := save(r, dataDir); err != nil {
		t.Fatalf("second save: %v", err)
	}
	if r.HandoffSHA256 == halted {
		t.Error("the handoff was not rewritten when the run stopped in another way")
	}
	if _, err := handoff.Load(run.Dir(dataDir, r.ID), halted, run.StateQuarantined); err == nil {
		t.Error("the halted handoff still loads for a quarantined run")
	}
	doc, err = handoff.Load(run.Dir(dataDir, r.ID), r.HandoffSHA256, run.StateQuarantined)
	if err != nil || doc.Next != handoff.BinNever || len(doc.Checks) != 2 || len(doc.Rounds) != 1 {
		t.Errorf("quarantined handoff = %+v, %v, want the two failed checks and the round", doc, err)
	}

	// An operator accepts it: nothing is left to hand on.
	r.State = run.StateAccepted
	if err := save(r, dataDir); err != nil {
		t.Fatalf("third save: %v", err)
	}
	if r.HandoffSHA256 != "" {
		t.Errorf("HandoffSHA256 = %q after the run was accepted, want none", r.HandoffSHA256)
	}
	if _, err := os.Stat(filepath.Join(run.Dir(dataDir, r.ID), handoff.FileName)); !os.IsNotExist(err) {
		t.Errorf("the handoff file outlived the run's acceptance (%v)", err)
	}
}

func TestSaveWritesNoHandoffForARunThatIsNotStopped(t *testing.T) {
	for _, state := range []run.State{run.StateAccepted, run.StateSliceRunning, run.StateVerifying} {
		dataDir := t.TempDir()
		r := &run.Run{ID: "run-1", Ticket: "fixture-ticket", State: state}
		if err := save(r, dataDir); err != nil {
			t.Fatalf("%s: save: %v", state, err)
		}
		if r.HandoffSHA256 != "" {
			t.Errorf("%s: HandoffSHA256 = %q, want none", state, r.HandoffSHA256)
		}
		if _, err := os.Stat(filepath.Join(run.Dir(dataDir, r.ID), handoff.FileName)); !os.IsNotExist(err) {
			t.Errorf("%s: a handoff file was written (%v)", state, err)
		}
	}
}

func TestSaveWritesTheHandoffForAHaltWithTheOperatorBin(t *testing.T) {
	dataDir := t.TempDir()
	r := &run.Run{ID: "run-h", Ticket: "fixture-ticket", State: run.StateHalted, HaltReasonCode: run.HaltReasonRelayCeilingExceeded}
	if err := save(r, dataDir); err != nil {
		t.Fatalf("save: %v", err)
	}
	doc, err := handoff.Load(run.Dir(dataDir, r.ID), r.HandoffSHA256, run.StateHalted)
	if err != nil || doc.Next != handoff.BinOperator || doc.Stopped == "" {
		t.Errorf("handoff = %+v, %v, want the operator bin and the halt's sentence", doc, err)
	}
}
