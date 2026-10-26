package requestdriver

import (
	"testing"
	"time"

	"buildgate/internal/request"
)

// OperatorResumesBetweenTheSaves makes the operator decide request id
// (`factoryd resume -from scratch`) at the moment the driver has saved it in
// resume_review and has not yet recorded the first reminder. The returned
// flag reports whether that moment came.
func OperatorResumesBetweenTheSaves(t *testing.T, id string) *bool {
	t.Helper()
	decided := false
	betweenReviewSaves = func(dataDir, savedID string) {
		if savedID != id || decided {
			return
		}
		r, err := request.Load(dataDir, id)
		if err != nil || r.State != request.StateResumeReview {
			return
		}
		if _, err := request.ResumeRequest(dataDir, id, request.ResumeScratch, "alice", time.Now(), nil); err != nil {
			t.Errorf("the operator's resume between the saves: %v", err)
			return
		}
		decided = true
	}
	t.Cleanup(func() { betweenReviewSaves = nil })
	return &decided
}

// RequireOperatorResumeKept fails t unless request id, on disk, carries the
// operator's decision OperatorResumesBetweenTheSaves wrote: back in the lost
// step's state with the decision pending, and no reminder recorded over it.
func RequireOperatorResumeKept(t *testing.T, dataDir, id string, decided *bool, from request.State) {
	t.Helper()
	if !*decided {
		t.Fatal("the driver never saved the request in resume_review before its reminder")
	}
	got, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != from || got.PendingResumeVerb() != request.ResumeScratch {
		t.Fatalf("state %s, pending verb %q: the operator's decision was overwritten (want %s with %q)", got.State, got.PendingResumeVerb(), from, request.ResumeScratch)
	}
	if got.NotifyCount != 0 {
		t.Errorf("NotifyCount = %d: a reminder was recorded for a review the operator had already decided", got.NotifyCount)
	}
}

// SetBetweenReviewSaves runs f at the moment OperatorResumesBetweenTheSaves
// decides in.
func SetBetweenReviewSaves(t *testing.T, f func(dataDir, id string)) {
	betweenReviewSaves = f
	t.Cleanup(func() { betweenReviewSaves = nil })
}
