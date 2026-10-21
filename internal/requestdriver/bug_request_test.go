package requestdriver_test

import (
	"context"
	"strings"
	"testing"

	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
	"buildgate/internal/requestdriver/requestdrivertest"
	"buildgate/internal/run"
)

func TestBuildTicketRunArgsForwardsBugOnlyWhenSet(t *testing.T) {
	with := &requestdriver.QueueEntry{ID: "t1", Workspace: "/repo", SpecPath: "/repo/spec.md", VerifyCommand: "make verify", Bug: true}
	if !containsFlag(requestdriver.BuildTicketRunArgs("data", with, requestdriver.WorkerConfig{}), "-bug") {
		t.Error("a bug entry must forward -bug")
	}
	without := &requestdriver.QueueEntry{ID: "t2", Workspace: "/repo", SpecPath: "/repo/spec.md", VerifyCommand: "make verify"}
	if containsFlag(requestdriver.BuildTicketRunArgs("data", without, requestdriver.WorkerConfig{}), "-bug") {
		t.Error("an entry that is not a bug must not forward the flag")
	}
}

// TestABugRunWhoseOraclePassesOnBaseQuarantinesTheRequest: a ticket run the
// workflow halted because the oracle passes on the base commit quarantines
// the request naming that check, instead of halting it for a retry.
func TestABugRunWhoseOraclePassesOnBaseQuarantinesTheRequest(t *testing.T) {
	dp := newFakeDeps(t)
	dataDir, id := requestdrivertest.BuildingFixture(dp, t, 1)
	r, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	r.Bug = true
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	var args []string
	runner := func(_ context.Context, a []string, onReady func(*run.Run)) error {
		args = a
		ticket := requestdrivertest.ArgValue(a, "-ticket")
		onReady(&run.Run{ID: ticket, State: run.StateReady})
		return (&run.Run{ID: ticket, State: run.StateHalted, HaltReasonCode: run.HaltReasonOraclePassesOnBase, HaltError: "the acceptance oracle passes on the base commit"}).Save(dataDir)
	}
	got := requestdrivertest.AdvanceBuildingOnce(dp, t, dataDir, id, requestdriver.ResumeGate{}, runner)
	if !containsFlag(args, "-bug") {
		t.Errorf("build args %v lack -bug for a bug request", args)
	}
	if got.State != request.StateQuarantined || got.QuarantineCheck != request.QuarantineCheckOracleGreenOnBase {
		t.Fatalf("state %s check %q, want quarantined by %s (error: %s)", got.State, got.QuarantineCheck, request.QuarantineCheckOracleGreenOnBase, got.Error)
	}
	if !strings.Contains(got.NextAction(), "factoryd reject -to spec") {
		t.Errorf("next action %q does not name the send-back", got.NextAction())
	}
}

// TestABugRunWhoseOracleRanNoTestOnBaseQuarantinesTheRequest: the same, with
// its own check, for an oracle that never ran.
func TestABugRunWhoseOracleRanNoTestOnBaseQuarantinesTheRequest(t *testing.T) {
	dp := newFakeDeps(t)
	dataDir, id := requestdrivertest.BuildingFixture(dp, t, 1)
	runner := func(_ context.Context, a []string, onReady func(*run.Run)) error {
		ticket := requestdrivertest.ArgValue(a, "-ticket")
		onReady(&run.Run{ID: ticket, State: run.StateReady})
		return (&run.Run{ID: ticket, State: run.StateHalted, HaltReasonCode: run.HaltReasonOracleNotRunOnBase, HaltError: "the acceptance oracle did not run"}).Save(dataDir)
	}
	got := requestdrivertest.AdvanceBuildingOnce(dp, t, dataDir, id, requestdriver.ResumeGate{}, runner)
	if got.State != request.StateQuarantined || got.QuarantineCheck != request.QuarantineCheckOracleNotRun {
		t.Fatalf("state %s check %q, want quarantined by %s", got.State, got.QuarantineCheck, request.QuarantineCheckOracleNotRun)
	}
	if !strings.Contains(got.NextAction(), "RUN_COMMAND.txt") {
		t.Errorf("next action %q does not say how to fix it", got.NextAction())
	}
}
