package requestdriver

import (
	"path/filepath"
	"testing"
	"time"

	"buildgate/internal/request"
	"buildgate/internal/unresolved"
)

// A ticket run whose review gave no verdict (unresolved.go) enters
// resume_review through the same locked reminder: an operator's resume
// between the saves survives.
func TestAResumeDecidedAfterAnUnjudgedTicketRunEntersResumeReviewIsKept(t *testing.T) {
	dataDir, r := buildingRequest(t)
	after, decided := OperatorResumesBetweenTheSaves(t, r.ID)
	runRecord := unansweredReviewRun("run-1", reviewLedgers[unresolved.ModelUnavailable])
	runRecord.WorkspacePath = filepath.Join(t.TempDir(), "worktree")
	if err := runRecord.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	if handled, err := afterUnjudgedTicketRun(ReviewSaveDeps{AfterSave: after}, dataDir, r, runRecord, time.Now()); err != nil || !handled {
		t.Fatalf("handled %v, err %v", handled, err)
	}
	RequireOperatorResumeKept(t, dataDir, r.ID, decided, request.StateBuilding)
}

// A caller that holds the request's lock enters resume_review without
// waiting on it, and an operator's resume, which waits on that lock, lands
// after the reminder and is kept.
func TestEnteringResumeReviewHoldingTheLockKeepsAResumeThatWaitedOnIt(t *testing.T) {
	dataDir, r := buildingRequest(t)
	unlock, err := request.Lock(dataDir, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	decision := make(chan error, 1)
	go func() {
		_, err := request.ResumeRequest(dataDir, r.ID, request.ResumeScratch, "alice", time.Now(), nil)
		decision <- err
	}()
	entered := make(chan error, 1)
	go func() {
		entered <- EnterResumeReviewHoldingLock(dataDir, r, request.LostStep{Cause: unresolved.WorkerLost}, time.Now())
	}()
	select {
	case err := <-entered:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("EnterResumeReviewHoldingLock waited on the lock its caller holds")
	}
	if r.NotifyCount != 1 {
		t.Errorf("NotifyCount = %d, want the first reminder", r.NotifyCount)
	}
	unlock()
	if err := <-decision; err != nil {
		t.Fatalf("the operator's resume: %v", err)
	}
	got, err := request.Load(dataDir, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != request.StateBuilding || got.PendingResumeVerb() != request.ResumeScratch {
		t.Fatalf("state %s, pending verb %q: want the operator's decision kept", got.State, got.PendingResumeVerb())
	}
}
