package request

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// savedLapsed saves a request in state that has waited three hours.
func savedLapsed(t *testing.T, state State) (dataDir string) {
	t.Helper()
	dataDir = t.TempDir()
	if err := SaveText(dataDir, "req-5", "text"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(Dir(dataDir, "req-5"), "spec.md"), []byte("# Spec\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := requestAt(state, time.Now().Add(-3*time.Hour))
	if state == StateResumeReview {
		r.Resume = &ResumeInfo{FromState: StateBuilding, LostRunID: "run-1", Generation: 1}
	}
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	return dataDir
}

func halted(t *testing.T, dataDir string) *Request {
	t.Helper()
	got, err := Load(dataDir, "req-5")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StateHalted || got.HaltKind != HaltGateExpired {
		t.Fatalf("request = state %q kind %q, want halted gate_expired", got.State, got.HaltKind)
	}
	return got
}

// Every decision on a gate refuses it under its own lock, halting the request
// in the same step.
func TestDecisionsRefuseALapsedGateAtomically(t *testing.T) {
	expiry := WithGateExpiry(2 * time.Hour)
	now := time.Now()
	decisions := map[string]struct {
		state State
		do    func(dataDir string) error
	}{
		"approve": {StateSpecReview, func(d string) error { _, err := Approve(d, "req-5", "alice", now, nil, expiry); return err }},
		"approve shown": {StateSpecReview, func(d string) error {
			_, err := ApproveShown(d, "req-5", "alice", now, nil, expiry)
			return err
		}},
		"reject": {StatePlanReview, func(d string) error { _, err := Reject(d, "req-5", "alice", "no", now, expiry); return err }},
		"resume": {StateResumeReview, func(d string) error {
			_, err := ResumeRequest(d, "req-5", ResumeScratch, "alice", now, nil, expiry)
			return err
		}},
		"oracle run command": {StateOracleReview, func(d string) error {
			_, err := SetOracleRunCommand(d, "req-5", TicketOracleRunCommandFilename, "go test ./...\n", "", expiry)
			return err
		}},
	}
	for name, c := range decisions {
		t.Run(name, func(t *testing.T) {
			dataDir := savedLapsed(t, c.state)
			err := c.do(dataDir)
			if !errors.Is(err, ErrGateExpired) || !errors.Is(err, ErrIllegalTransition) {
				t.Fatalf("decision on a lapsed gate = %v, want ErrGateExpired", err)
			}
			halted(t, dataDir)
		})
	}

	t.Run("an open gate, or no expiry, is decided", func(t *testing.T) {
		dataDir := savedLapsed(t, StateSpecReview)
		if _, err := Approve(dataDir, "req-5", "alice", now, nil, WithGateExpiry(5*time.Hour)); errors.Is(err, ErrGateExpired) {
			t.Errorf("approve inside the expiry = %v", err)
		}
		dataDir = savedLapsed(t, StateSpecReview)
		if _, err := Approve(dataDir, "req-5", "alice", now, nil, WithGateExpiry(0)); errors.Is(err, ErrGateExpired) {
			t.Errorf("approve with expiry off = %v", err)
		}
		dataDir = savedLapsed(t, StateSpecReview)
		if _, err := Approve(dataDir, "req-5", "alice", now, nil); errors.Is(err, ErrGateExpired) {
			t.Errorf("approve without the option = %v", err)
		}
	})
}

func TestSaveNeverBringsACancelledRequestBack(t *testing.T) {
	dataDir := savedLapsed(t, StateBuilding)
	stale, err := Load(dataDir, "req-5") // a step's in-memory copy
	if err != nil {
		t.Fatal(err)
	}
	cancelled, err := Load(dataDir, "req-5")
	if err != nil {
		t.Fatal(err)
	}
	if err := cancelled.Cancel("alice", "", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := cancelled.Save(dataDir); err != nil {
		t.Fatalf("saving the cancel: %v", err)
	}
	if err := stale.Halt("late result", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := stale.Save(dataDir); !errors.Is(err, ErrCancelledOnDisk) {
		t.Fatalf("Save of a stale copy over a cancel = %v, want ErrCancelledOnDisk", err)
	}
	got, err := Load(dataDir, "req-5")
	if err != nil || got.State != StateCancelled {
		t.Errorf("state = %v (%v), want cancelled", got.State, err)
	}
	// Under the request's lock too (the caller holds it): the guard still holds.
	unlock, err := Lock(dataDir, "req-5")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if err := stale.Save(dataDir); !errors.Is(err, ErrCancelledOnDisk) {
		t.Errorf("Save under the lock = %v, want ErrCancelledOnDisk", err)
	}
}
