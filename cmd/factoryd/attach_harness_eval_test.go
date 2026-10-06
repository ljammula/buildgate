package main

import (
	"testing"
	"time"

	"buildgate/internal/request"
	"buildgate/internal/run"
)

// TestAttachHarnessEvalPopulatesFromOwningRequest is the regression test
// proving a run with an owning request gets a HarnessEval naming the harness
// its execution role resolved to (the last execution attempt's Harness).
func TestAttachHarnessEvalPopulatesFromOwningRequest(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()

	req := request.New("req-1", "/repo", "repo", request.Source{Kind: request.SourceText}, time.Now())
	if err := request.SaveText(dataDir, "req-1", "do the thing"); err != nil {
		t.Fatalf("SaveText: %v", err)
	}
	if err := req.Save(dataDir); err != nil {
		t.Fatalf("Save request: %v", err)
	}

	r := &run.Run{
		ID:        "run-1",
		RequestID: "req-1",
		State:     run.StateAccepted,
		Attempts: []run.Attempt{
			{Kind: "build", Role: run.AttemptRoleExecution, Harness: "pifork", RelayConsumedInputTokens: 12, RelayConsumedOutputTokens: 4},
			{Kind: "spec_conformity", Role: run.AttemptRoleReview, Harness: "pi"},
		},
	}

	attachHarnessEval(r, dataDir, "run-1")

	if r.HarnessEval == nil {
		t.Fatal("attachHarnessEval left HarnessEval nil, want it populated from the owning request")
	}
	if r.HarnessEval.Harness != "pifork" {
		t.Errorf("HarnessEval = %+v, want Harness=pifork (the execution role's, not the review role's)", r.HarnessEval)
	}
	if r.HarnessEval.Outcome != string(run.StateAccepted) {
		t.Errorf("HarnessEval.Outcome = %q, want %q", r.HarnessEval.Outcome, run.StateAccepted)
	}
}

// TestAttachHarnessEvalLeavesNilWithoutAnOwningRequest covers a plain
// `factoryd <run>` invocation (no RequestID) and a run whose RequestID
// points at nothing on disk — both must leave HarnessEval nil rather than
// error or fabricate one.
func TestAttachHarnessEvalLeavesNilWithoutAnOwningRequest(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()

	r := &run.Run{ID: "run-1", State: run.StateAccepted}
	attachHarnessEval(r, dataDir, "run-1")
	if r.HarnessEval != nil {
		t.Errorf("HarnessEval = %+v, want nil for a run with no RequestID and no owning request on disk", r.HarnessEval)
	}
}

// TestAttachHarnessEvalLeavesNilWithoutAnExecutionHarness covers a run
// recorded before Attempt.Harness existed: no execution attempt names a
// harness, so no all-empty HarnessEval is synthesized.
func TestAttachHarnessEvalLeavesNilWithoutAnExecutionHarness(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()

	req := request.New("req-2", "/repo", "repo", request.Source{Kind: request.SourceText}, time.Now())
	if err := request.SaveText(dataDir, "req-2", "do the thing"); err != nil {
		t.Fatalf("SaveText: %v", err)
	}
	if err := req.Save(dataDir); err != nil {
		t.Fatalf("Save request: %v", err)
	}

	r := &run.Run{ID: "run-2", RequestID: "req-2", State: run.StateAccepted, Attempts: []run.Attempt{{Kind: "build", Role: run.AttemptRoleExecution}}}
	attachHarnessEval(r, dataDir, "run-2")
	if r.HarnessEval != nil {
		t.Errorf("HarnessEval = %+v, want nil when no execution attempt recorded a harness", r.HarnessEval)
	}
}
