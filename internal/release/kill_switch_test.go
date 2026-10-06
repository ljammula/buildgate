package release

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
)

func TestKillSwitchEngageAndDisengage(t *testing.T) {
	dir := t.TempDir()
	now := func() string { return "2026-08-26T12:00:00Z" }

	if err := Engage(dir, "widget", "alice", "pause releases", now); err != nil {
		t.Fatalf("Engage: %v", err)
	}
	engaged, err := IsEngaged(dir, "widget")
	if err != nil {
		t.Fatalf("IsEngaged after Engage: %v", err)
	}
	if !engaged {
		t.Error("IsEngaged after Engage = false, want true")
	}
	if err := Disengage(dir, "widget", "bob", "release issue resolved", now); err != nil {
		t.Fatalf("Disengage: %v", err)
	}
	engaged, err = IsEngaged(dir, "widget")
	if err != nil {
		t.Fatalf("IsEngaged after Disengage: %v", err)
	}
	if engaged {
		t.Error("IsEngaged after Disengage = true, want false")
	}
}

func TestKillSwitchTransitionsAreIdempotent(t *testing.T) {
	dir := t.TempDir()
	now := func() string { return "2026-08-26T12:00:00Z" }

	// Redundant requests succeed but do not append history because no state
	// transition occurred; this keeps the timeline limited to actual changes.
	if err := Disengage(dir, "widget", "alice", "already safe", now); err != nil {
		t.Fatalf("initial Disengage: %v", err)
	}
	if err := Engage(dir, "widget", "alice", "pause releases", now); err != nil {
		t.Fatalf("Engage: %v", err)
	}
	if err := Engage(dir, "widget", "bob", "still investigating", now); err != nil {
		t.Fatalf("second Engage: %v", err)
	}
	record, err := LoadKillSwitch(dir, "widget")
	if err != nil {
		t.Fatalf("LoadKillSwitch: %v", err)
	}
	if len(record.History) != 1 {
		t.Errorf("len(History) = %d, want 1 actual transition", len(record.History))
	}
}

func TestIsEngagedMissingRecord(t *testing.T) {
	engaged, err := IsEngaged(t.TempDir(), "untouched")
	if err != nil {
		t.Fatalf("IsEngaged: %v", err)
	}
	if engaged {
		t.Error("IsEngaged for missing record = true, want false")
	}
}

func TestKillSwitchHistoryRoundTrips(t *testing.T) {
	dir := t.TempDir()
	times := []string{"2026-08-26T12:00:00Z", "2026-08-26T12:05:00Z"}
	nextTime := func() string {
		at := times[0]
		times = times[1:]
		return at
	}

	if err := Engage(dir, "widget", "alice", "verification incident", nextTime); err != nil {
		t.Fatalf("Engage: %v", err)
	}
	if err := Disengage(dir, "widget", "bob", "incident resolved", nextTime); err != nil {
		t.Fatalf("Disengage: %v", err)
	}

	record, err := LoadKillSwitch(dir, "widget")
	if err != nil {
		t.Fatalf("LoadKillSwitch: %v", err)
	}
	if record.Project != "widget" || record.Engaged {
		t.Errorf("record = %+v, want project widget and disengaged", record)
	}
	want := []KillSwitchTransition{
		{Engaged: true, By: "alice", Reason: "verification incident", At: "2026-08-26T12:00:00Z"},
		{Engaged: false, By: "bob", Reason: "incident resolved", At: "2026-08-26T12:05:00Z"},
	}
	if len(record.History) != len(want) {
		t.Fatalf("len(History) = %d, want %d", len(record.History), len(want))
	}
	for i := range want {
		if record.History[i] != want[i] {
			t.Errorf("History[%d] = %+v, want %+v", i, record.History[i], want[i])
		}
	}
	wantPath := filepath.Join(dir, "projects", "widget", "kill_switch.json")
	if gotPath, err := killSwitchPath(dir, "widget"); err != nil || gotPath != wantPath {
		t.Errorf("killSwitchPath() = %q, %v, want %q", gotPath, err, wantPath)
	}
}

func TestKillSwitchConcurrentTransitionsRemainConsistent(t *testing.T) {
	dir := t.TempDir()
	const rounds = 50
	const callersPerRound = 16

	for round := 0; round < rounds; round++ {
		engaged := round%2 == 0
		var wg sync.WaitGroup
		errs := make(chan error, callersPerRound)
		for caller := 0; caller < callersPerRound; caller++ {
			wg.Add(1)
			go func(caller int) {
				defer wg.Done()
				now := func() string { return fmt.Sprintf("round-%d-caller-%d", round, caller) }
				if engaged {
					errs <- Engage(dir, "widget", "operator", "concurrent test", now)
					return
				}
				errs <- Disengage(dir, "widget", "operator", "concurrent test", now)
			}(caller)
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatalf("concurrent transition in round %d: %v", round, err)
			}
		}
	}

	record, err := LoadKillSwitch(dir, "widget")
	if err != nil {
		t.Fatalf("LoadKillSwitch: %v", err)
	}
	if record.Engaged {
		t.Error("final Engaged = true, want false")
	}
	if len(record.History) != rounds {
		t.Fatalf("len(History) = %d, want %d actual transitions", len(record.History), rounds)
	}
	for i, transition := range record.History {
		wantEngaged := i%2 == 0
		if transition.Engaged != wantEngaged {
			t.Errorf("History[%d].Engaged = %v, want %v", i, transition.Engaged, wantEngaged)
		}
	}
}
