package request

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A bug request's ticket 1 must add a test that fails on the base commit, so
// a plan whose ticket 1 declares Tests-Required: no is refused at approval,
// before any state change; the same plan on a request that is not a bug
// request is approved.
func TestApprovePlanRefusesABugTicketOneThatOptsOutOfTests(t *testing.T) {
	optOut := strings.Replace(wellFormedApprovableTicket, "Verify-Command: true\n", "Verify-Command: true\nTests-Required: no -- the change is a rename\n", 1)
	for _, tc := range []struct {
		name    string
		bug     bool
		ticket  string
		refused bool
	}{
		{"bug request, opt-out", true, optOut, true},
		{"bug request, tests required", true, wellFormedApprovableTicket, false},
		{"not a bug request, opt-out", false, optOut, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dataDir := t.TempDir()
			r := newApprovableRequest(t, dataDir, "req-1", StatePlanReview, true)
			r.Bug = tc.bug
			if err := r.Save(dataDir); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(Dir(dataDir, "req-1"), "tickets", "001.spec.md"), []byte(tc.ticket), 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := Approve(dataDir, "req-1", "alice", fixedNow, nil)
			if !tc.refused {
				if err != nil || got.State != StateBuilding {
					t.Fatalf("Approve = %v, %v; want building", got, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "ticket 1") || !strings.Contains(err.Error(), "Tests-Required") {
				t.Fatalf("Approve err = %v, want a refusal naming ticket 1 and Tests-Required", err)
			}
			after, lerr := Load(dataDir, "req-1")
			if lerr != nil || after.State != StatePlanReview || len(after.ApprovedSHA256) != 0 {
				t.Fatalf("a refused approval changed the request: %+v, %v", after, lerr)
			}
		})
	}
}

// A request record in a state this release does not define is refused when
// read, with an error naming the state and telling the operator to finish or
// cancel it on the earlier release.
func TestLoadRefusesARequestInAStateThisReleaseDoesNotDefine(t *testing.T) {
	for _, state := range []string{"oracle_drafting", "oracle_review", "no_such_state"} {
		dataDir := t.TempDir()
		r := New("req-old", "/repos/app", "app", Source{Kind: SourceText}, fixedNow)
		r.State = State(state)
		if err := os.MkdirAll(Dir(dataDir, "req-old"), 0o750); err != nil {
			t.Fatal(err)
		}
		b, err := json.MarshalIndent(r, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(Path(dataDir, "req-old"), b, 0o600); err != nil {
			t.Fatal(err)
		}
		_, err = Load(dataDir, "req-old")
		if !errors.Is(err, ErrUnknownState) || !strings.Contains(err.Error(), `"`+state+`"`) || !strings.Contains(err.Error(), "earlier release") {
			t.Errorf("Load of state %q: err = %v, want ErrUnknownState naming the state and the earlier release", state, err)
		}
		if _, err := List(dataDir); !errors.Is(err, ErrUnknownState) {
			t.Errorf("List with a request in state %q: err = %v, want ErrUnknownState", state, err)
		}
	}
}
