package main

import (
	"testing"

	"buildgate/internal/request"
	"buildgate/internal/requestdriver/requestdrivertest"
)

// TestCancelMainParsesRealFlagSet proves -reason and -data-dir parse
// through cancelMain's own real flag.FlagSet, and that a real cancel
// succeeds end to end, mirroring TestApproveMainParsesRealFlagSet/
// TestRejectMainParsesRealFlagSet.
func TestCancelMainParsesRealFlagSet(t *testing.T) {
	dp := newTestDeps(t)
	t.Parallel()
	dataDir := t.TempDir()
	requestdrivertest.NewApprovableDriverRequest(t, dataDir, "req-1", request.StateSpecReview)

	if err := cancelMain(dp, []string{"-reason", "no longer needed", "-data-dir", dataDir, "req-1"}); err != nil {
		t.Fatalf("cancelMain: %v", err)
	}
	loaded, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.State != request.StateCancelled {
		t.Errorf("State = %q, want %q", loaded.State, request.StateCancelled)
	}
	last := loaded.History[len(loaded.History)-1]
	if last.Reason != "no longer needed" {
		t.Errorf("last History entry Reason = %q, want %q", last.Reason, "no longer needed")
	}
	if last.By == "" {
		t.Error("last History entry By left empty")
	}
}

// TestCancelMainRefusesTerminalState proves cancel refuses a request
// already in a terminal state, naming it, and leaves the request
// unchanged.
func TestCancelMainRefusesTerminalState(t *testing.T) {
	dp := newTestDeps(t)
	t.Parallel()
	dataDir := t.TempDir()
	requestdrivertest.NewApprovableDriverRequest(t, dataDir, "req-1", request.StateDone)

	err := cancelMain(dp, []string{"-data-dir", dataDir, "req-1"})
	if err == nil {
		t.Fatal("cancelMain from a terminal state: want an error, got nil")
	}
	loaded, loadErr := request.Load(dataDir, "req-1")
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if loaded.State != request.StateDone {
		t.Errorf("State = %q, want unchanged %q", loaded.State, request.StateDone)
	}
}

// TestCancelMainFromQuarantinedOrHaltedSucceeds proves an operator can
// dismiss a dead quarantined or halted request -- before this, `factoryd
// cancel` on one failed outright ("illegal transition from \"quarantined\"
// to \"cancelled\""), leaving no way to retire it.
func TestCancelMainFromQuarantinedOrHaltedSucceeds(t *testing.T) {
	dp := newTestDeps(t)
	t.Parallel()
	for _, state := range []request.State{request.StateQuarantined, request.StateHalted} {
		state := state
		t.Run(string(state), func(t *testing.T) {
			dataDir := t.TempDir()
			requestdrivertest.NewApprovableDriverRequest(t, dataDir, "req-1", state)

			if err := cancelMain(dp, []string{"-data-dir", dataDir, "req-1"}); err != nil {
				t.Fatalf("cancelMain from %s: %v", state, err)
			}
			loaded, err := request.Load(dataDir, "req-1")
			if err != nil {
				t.Fatal(err)
			}
			if loaded.State != request.StateCancelled {
				t.Errorf("State = %q, want %q", loaded.State, request.StateCancelled)
			}
		})
	}
}

func TestCancelMainRequiresExactlyOnePositionalArgument(t *testing.T) {
	dp := newTestDeps(t)
	t.Parallel()
	if err := cancelMain(dp, []string{"-data-dir", t.TempDir()}); err == nil {
		t.Fatal("cancelMain with no request id: want an error, got nil")
	}
	if err := cancelMain(dp, []string{"-data-dir", t.TempDir(), "req-1", "req-2"}); err == nil {
		t.Fatal("cancelMain with two request ids: want an error, got nil")
	}
}
