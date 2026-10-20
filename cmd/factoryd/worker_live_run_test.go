package main

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

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
// built, is not moved to resume_review: the step moves the request itself
// when the run ends. "Being built" is a fact of this process (a step of it in
// flight for the request) or a fresh owner heartbeat of another process; the
// owner pid of a worker's own run is its own and says nothing, so a run with
// no step in flight in this process is parked, never declined for good.
func TestAHeartbeatTimeoutDoesNotParkARunThatIsStillBeingBuilt(t *testing.T) {
	self := func(*testing.T) int { return os.Getpid() }
	parent := func(*testing.T) int { return os.Getppid() }
	none := func(*testing.T) int { return 0 }
	for name, tc := range map[string]struct {
		state    run.State
		pid      func(*testing.T) int
		inFlight bool
		stale    bool
		moved    bool
	}{
		"a step of this process is in flight":         {run.StateSliceRunning, self, true, false, false},
		"no step in flight, own pid":                  {run.StateSliceRunning, self, false, false, true},
		"another live process with a fresh heartbeat": {run.StateSliceRunning, parent, false, false, false},
		"another live process with a stale heartbeat": {run.StateSliceRunning, parent, false, true, true},
		"dead owner":      {run.StateSliceRunning, deadPID, false, false, true},
		"no owner marker": {run.StateSliceRunning, none, false, false, true},
		"run finished, a step still counted in flight": {run.StateHalted, self, true, false, true},
	} {
		dp := newTestDeps(t)
		acts, id := liveRunFixture(t, dp, tc.state, tc.pid(t))
		if tc.stale {
			old := time.Now().Add(-time.Hour)
			if err := os.Chtimes(filepath.Join(run.Dir(acts.dataDir, "building-run"), "sandbox-owner.pid"), old, old); err != nil {
				t.Fatal(err)
			}
		}
		if tc.inFlight {
			addActiveRequest(id)
		}
		before, err := request.Load(acts.dataDir, id)
		if err != nil {
			t.Fatal(err)
		}
		live, why := acts.ticketRunStillLive(before, 0)
		if live == tc.moved || (live && why == "") {
			t.Errorf("%s: ticketRunStillLive = %v (%q), want %v with a reason", name, live, why, !tc.moved)
		}
		if tc.inFlight {
			if own, _ := acts.ticketRunStillLive(before, 1); own {
				t.Errorf("%s: the step's own entry counted as another build", name)
			}
		}
		err = acts.HaltLostRequestStep(context.Background(), workflow.HaltLostRequestStepInput{RequestID: id, State: string(request.StateBuilding)})
		if tc.inFlight {
			removeActiveRequest(id)
		}
		if err != nil {
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
