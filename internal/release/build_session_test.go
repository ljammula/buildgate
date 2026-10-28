package release

import (
	"os"
	"path/filepath"
	"testing"

	"buildgate/internal/request"
	"buildgate/internal/run"
)

// sessionRun saves a run of requestID's ticket with a kept build session
// holding one file, plus one other file in the run's directory, and returns
// the session's path.
func sessionRun(t *testing.T, dataDir, id, requestID string, ticket int) string {
	t.Helper()
	if err := (&run.Run{ID: id, State: run.StateAccepted, RequestID: requestID, TicketIndex: ticket}).Save(dataDir); err != nil {
		t.Fatal(err)
	}
	session := filepath.Join(run.Dir(dataDir, id), run.BuildSessionDirName)
	if err := os.MkdirAll(filepath.Join(session, "sessions"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(session, "sessions", "own.jsonl"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(run.Dir(dataDir, id), "agent-notes.md"), []byte("notes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return session
}

func exists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Lstat(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return err == nil
}

// TestPruneBuildSessionsKeepsOnlyWhatARoundCanStillFollow: a request building
// or waiting at resume_review on ticket 2 keeps ticket 2's sessions and loses
// ticket 1's; in pr_review or a terminal state it loses all of them. Another
// request's sessions and every other file of a run directory stay.
func TestPruneBuildSessionsKeepsOnlyWhatARoundCanStillFollow(t *testing.T) {
	for _, tc := range []struct {
		state       request.State
		keepTicket2 bool
	}{
		{request.StateBuilding, true},
		{request.StateResumeReview, true},
		{request.StatePRReview, false},
		{request.StateDone, false},
		{request.StateQuarantined, false},
		{request.StateHalted, false},
		{request.StateCancelled, false},
	} {
		t.Run(string(tc.state), func(t *testing.T) {
			dataDir := t.TempDir()
			t1 := sessionRun(t, dataDir, "req-1-001", "req-1", 1)
			t1round := sessionRun(t, dataDir, "req-1-001-advice1", "req-1", 1)
			t2 := sessionRun(t, dataDir, "req-1-002", "req-1", 2)
			t2round := sessionRun(t, dataDir, "req-1-002-check1", "req-1", 2)
			other := sessionRun(t, dataDir, "req-2-001", "req-2", 1)
			r := &request.Request{ID: "req-1", State: tc.state, TicketIndex: 2, TicketCount: 2}

			if err := PruneBuildSessions(dataDir, r); err != nil {
				t.Fatal(err)
			}
			for _, gone := range []string{t1, t1round} {
				if exists(t, gone) {
					t.Errorf("%s: ticket 1's session %s survived", tc.state, gone)
				}
			}
			for _, s := range []string{t2, t2round} {
				if got := exists(t, s); got != tc.keepTicket2 {
					t.Errorf("%s: ticket 2's session %s exists = %v, want %v", tc.state, s, got, tc.keepTicket2)
				}
			}
			if !exists(t, filepath.Join(other, "sessions", "own.jsonl")) {
				t.Errorf("%s: another request's session was removed", tc.state)
			}
			for _, id := range []string{"req-1-001", "req-1-001-advice1", "req-1-002", "req-1-002-check1"} {
				for _, name := range []string{"run.json", "agent-notes.md"} {
					if !exists(t, filepath.Join(run.Dir(dataDir, id), name)) {
						t.Errorf("%s: %s of run %s was removed", tc.state, name, id)
					}
				}
			}
		})
	}
}

// TestPruneBuildSessionsDoesNotFollowALink: a build-session that is a link is
// left alone, and what it points at survives.
func TestPruneBuildSessionsDoesNotFollowALink(t *testing.T) {
	dataDir := t.TempDir()
	if err := (&run.Run{ID: "req-1-001", State: run.StateAccepted, RequestID: "req-1", TicketIndex: 1}).Save(dataDir); err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	kept := filepath.Join(target, "own.jsonl")
	if err := os.WriteFile(kept, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(run.Dir(dataDir, "req-1-001"), run.BuildSessionDirName)
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	if err := PruneBuildSessions(dataDir, &request.Request{ID: "req-1", State: request.StateDone}); err != nil {
		t.Fatal(err)
	}
	if !exists(t, kept) {
		t.Error("the link's target lost its file")
	}
	if !exists(t, link) {
		t.Error("the link itself was removed")
	}
}

// TestPruneBuildSessionsWithNoRunsIsANoOp: a data dir with no runs directory
// is not an error.
func TestPruneBuildSessionsWithNoRunsIsANoOp(t *testing.T) {
	if err := PruneBuildSessions(t.TempDir(), &request.Request{ID: "req-1", State: request.StateDone}); err != nil {
		t.Fatal(err)
	}
}

// TestPruneBuildSessionsKeepsWhatARoundInFlightFollows: in pr_review, where
// every session would otherwise go, the run a round still in flight follows
// keeps its session: a round run not yet terminal naming it as
// EarlierAttemptOf, or a ticket's advice round not yet started naming it as
// Advice.Of. Once the round's run is terminal, the session goes.
func TestPruneBuildSessionsKeepsWhatARoundInFlightFollows(t *testing.T) {
	for _, tc := range []struct {
		name       string
		roundState run.State
		advice     bool
		wantKept   bool
	}{
		{"round run still verifying", run.StateVerifying, false, true},
		{"round run halted, not confirmed", run.StateHalted, false, true},
		{"round run accepted", run.StateAccepted, false, false},
		{"advice round not yet started", "", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dataDir := t.TempDir()
			followed := sessionRun(t, dataDir, "req-1-001", "req-1", 1)
			if tc.roundState != "" {
				if err := (&run.Run{ID: "req-1-001-advice1", State: tc.roundState, RequestID: "req-1", TicketIndex: 1, EarlierAttemptOf: "req-1-001"}).Save(dataDir); err != nil {
					t.Fatal(err)
				}
			}
			r := &request.Request{ID: "req-1", State: request.StatePRReview, TicketIndex: 1, TicketCount: 1, Tickets: []request.Ticket{{Index: 1}}}
			if tc.advice {
				r.Tickets[0].Advice = &request.TicketAdvice{Of: "req-1-001"}
			}

			if err := PruneBuildSessions(dataDir, r); err != nil {
				t.Fatal(err)
			}
			if got := exists(t, followed); got != tc.wantKept {
				t.Errorf("session of the followed run exists = %v, want %v", got, tc.wantKept)
			}
		})
	}
}

// TestPruneBuildSessionsKeepsARunWithNoTicketIndexWhileBuilding: a run
// recorded with no ticket index keeps its session while the request builds.
func TestPruneBuildSessionsKeepsARunWithNoTicketIndexWhileBuilding(t *testing.T) {
	dataDir := t.TempDir()
	s := sessionRun(t, dataDir, "req-1-001", "req-1", 0)
	if err := PruneBuildSessions(dataDir, &request.Request{ID: "req-1", State: request.StateBuilding, TicketIndex: 2, TicketCount: 2}); err != nil {
		t.Fatal(err)
	}
	if !exists(t, s) {
		t.Error("a session of a run with no ticket index was removed while the request builds")
	}
}

// TestPruneBuildSessionsDoesNotFollowALinkedRunDirectory: a run directory that
// is a link out of the runs directory is never pruned through.
func TestPruneBuildSessionsDoesNotFollowALinkedRunDirectory(t *testing.T) {
	dataDir := t.TempDir()
	outside := t.TempDir()
	sessionRun(t, outside, "req-1-001", "req-1", 1)
	outsideSession := filepath.Join(run.Dir(outside, "req-1-001"), run.BuildSessionDirName, "sessions", "own.jsonl")
	if err := os.MkdirAll(run.Dir(dataDir, ""), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(run.Dir(outside, "req-1-001"), run.Dir(dataDir, "req-1-001")); err != nil {
		t.Fatal(err)
	}

	if err := PruneBuildSessions(dataDir, &request.Request{ID: "req-1", State: request.StateDone}); err != nil {
		t.Fatal(err)
	}
	if !exists(t, outsideSession) {
		t.Error("a session outside the runs directory was removed through a linked run directory")
	}
}
