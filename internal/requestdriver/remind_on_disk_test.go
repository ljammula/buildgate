package requestdriver

import (
	"testing"
	"time"

	"buildgate/internal/request"
)

// An operator's write that lands after the driver saved the review state and
// before it records the first reminder (an answer to a read-back question)
// survives: the reminder is recorded on the request as it is on disk, not on
// the driver's copy.
func TestTheFirstReminderKeepsAnAnswerGivenSinceTheReviewWasEntered(t *testing.T) {
	dataDir := t.TempDir()
	r := notifiedRequest(t, dataDir, request.StateSpecReview)
	r.ReadBack = &request.ReadBack{Summary: "s", Questions: []request.ReadBackQuestion{{ID: "Q1", Text: "How long?"}}}
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	// The operator answers from the console; r is now the driver's stale copy.
	if _, err := request.AnswerReadBack(dataDir, r.ID, "Q1", request.QuestionSHA256("How long?"), "op", "24 hours.", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := remindOnDisk(dataDir, r, time.Now()); err != nil {
		t.Fatal(err)
	}
	loaded, err := request.Load(dataDir, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded.ReadBack.Questions[0].Answer; got != "24 hours." {
		t.Fatalf("answer = %q after the reminder, want the operator's kept", got)
	}
	if loaded.NotifyCount != 1 || loaded.LastNotifiedAt == "" {
		t.Fatalf("NotifyCount = %d, LastNotifiedAt = %q; want the reminder recorded", loaded.NotifyCount, loaded.LastNotifiedAt)
	}
	if r.ReadBack.Questions[0].Answer != "24 hours." || r.NotifyCount != 1 {
		t.Fatalf("the driver's copy was not refreshed: %+v", r)
	}
}

// A request the operator already moved out of the review gets no reminder.
func TestTheFirstReminderSkipsARequestThatLeftTheReview(t *testing.T) {
	dataDir := t.TempDir()
	r := notifiedRequest(t, dataDir, request.StateSpecReview)
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	moved := *r
	moved.State = request.StateCancelled
	if err := moved.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	if err := remindOnDisk(dataDir, r, time.Now()); err != nil {
		t.Fatal(err)
	}
	loaded, err := request.Load(dataDir, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.State != request.StateCancelled || loaded.NotifyCount != 0 {
		t.Fatalf("State = %q, NotifyCount = %d; want cancelled and no reminder", loaded.State, loaded.NotifyCount)
	}
}
