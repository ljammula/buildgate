package main

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"buildgate/internal/request"
	"buildgate/internal/requestdriver/requestdrivertest"
	"buildgate/internal/run"
	"buildgate/internal/workflow"
)

// liveRunFixture is a building request whose ticket's run record is in state,
// with an owner marker naming pid (none when pid is 0).
func liveRunFixture(t *testing.T, dp *deps, state run.State, pid int) (*requestActivities, string) {
	t.Helper()
	dataDir, id := requestdrivertest.BuildingFixture(dp, t, 1)
	r, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	r.TicketIndex = 1
	r.Tickets[0].RunID = "building-run"
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	if err := (&run.Run{ID: "building-run", State: state, RequestID: id}).Save(dataDir); err != nil {
		t.Fatal(err)
	}
	if pid > 0 {
		if err := os.WriteFile(filepath.Join(run.Dir(dataDir, "building-run"), "sandbox-owner.pid"), []byte(strconv.Itoa(pid)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return &requestActivities{dp: dp, dataDir: dataDir}, id
}

// A step whose heartbeat the server timed out, but whose run is still being
// built by a live process, is not moved to resume_review: the step moves the
// request itself when the run ends. Any other lost run is.
func TestAHeartbeatTimeoutDoesNotParkARunItsLiveOwnerStillBuilds(t *testing.T) {
	for name, tc := range map[string]struct {
		state run.State
		pid   func(*testing.T) int
		moved bool
	}{
		"live owner, run in flight": {run.StateSliceRunning, func(*testing.T) int { return os.Getpid() }, false},
		"dead owner":                {run.StateSliceRunning, deadPID, true},
		"no owner marker":           {run.StateSliceRunning, func(*testing.T) int { return 0 }, true},
		"run finished":              {run.StateHalted, func(*testing.T) int { return os.Getpid() }, true},
	} {
		dp := newTestDeps(t)
		acts, id := liveRunFixture(t, dp, tc.state, tc.pid(t))
		before, err := request.Load(acts.dataDir, id)
		if err != nil {
			t.Fatal(err)
		}
		if live := acts.ticketRunStillLive(context.Background(), before); live == tc.moved {
			t.Errorf("%s: ticketRunStillLive = %v, want %v", name, live, !tc.moved)
		}
		if err := acts.HaltLostRequestStep(context.Background(), workflow.HaltLostRequestStepInput{RequestID: id, State: string(request.StateBuilding)}); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got, err := request.Load(acts.dataDir, id)
		if err != nil {
			t.Fatal(err)
		}
		if moved := got.State == request.StateResumeReview; moved != tc.moved {
			t.Errorf("%s: state %s, moved to resume_review = %v, want %v", name, got.State, moved, tc.moved)
		}
	}
}
