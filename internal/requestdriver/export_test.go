package requestdriver

import (
	"context"
	"testing"
	"time"

	"buildgate/internal/forge"
	"buildgate/internal/request"
)

// ReviewSaveDeps is the Deps of a test in this package that enters a review
// state with no other dependency: its review save runs AfterSave, when set,
// right after the record is written.
type ReviewSaveDeps struct {
	Deps
	AfterSave func(dataDir, id string)
}

func (d ReviewSaveDeps) SaveReviewEntry(dataDir string, r *request.Request) error {
	if err := r.Save(dataDir); err != nil {
		return err
	}
	if d.AfterSave != nil {
		d.AfterSave(dataDir, r.ID)
	}
	return nil
}

// OperatorResumesBetweenTheSaves returns the function that makes the operator
// decide request id (`factoryd resume -from scratch`) at the moment the
// driver has saved it in resume_review and has not yet recorded the first
// reminder; the test sets it as its Deps' review-entry save callback. The
// returned flag reports whether that moment came.
func OperatorResumesBetweenTheSaves(t *testing.T, id string) (func(dataDir, savedID string), *bool) {
	t.Helper()
	decided := false
	return func(dataDir, savedID string) {
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
	}, &decided
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

// WriteRoundAddendum writes <request>/rounds/<ticket>-<round>/addendum.md
// (<ticket>-<round>-fix<n> for the round's n-th fix attempt) for the external
// tests of this package.
func WriteRoundAddendum(dataDir, requestID string, ticket *request.Ticket, threads []forge.Thread, roundIndex, fix int) (string, error) {
	path, _, err := writeRoundAddendum(dataDir, requestID, ticket, threads, roundIndex, fix)
	return path, err
}

// RestackCarvedOntoPredecessor is restackCarvedOntoPredecessor for a test.
func RestackCarvedOntoPredecessor(dp Deps, ctx context.Context, dataDir string, r *request.Request, ticket *request.Ticket, now time.Time) (bool, error) {
	return restackCarvedOntoPredecessor(dp, ctx, dataDir, r, ticket, now)
}

// ParseNumstatZ is parseNumstatZ for the external test package.
func ParseNumstatZ(out string) []FileStat { return parseNumstatZ(out) }

// WriteTicketBuildInput is writeTicketBuildInput for the external test package.
func WriteTicketBuildInput(dataDir string, r *request.Request, ticket *request.Ticket) (string, error) {
	return writeTicketBuildInput(dataDir, r, *ticket)
}
