package request

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func requestAt(state State, entered time.Time) *Request {
	r := New("req-5", "/repos/app", "app", Source{Kind: SourceText}, entered)
	r.State = state
	r.EnteredAt = entered.UTC().Format(time.RFC3339Nano)
	r.History = []Transition{{From: StateSubmitted, To: StateSpecDrafting}, {From: StateSpecDrafting, To: state}}
	return r
}

func TestExpiringGatesAreTheFourDecisionStatesAndNotPRReview(t *testing.T) {
	for _, s := range AllStates() {
		want := s == StateSpecReview || s == StatePlanReview || s == StateResumeReview
		if got := ExpiringGate(s); got != want {
			t.Errorf("ExpiringGate(%s) = %v, want %v", s, got, want)
		}
	}
	entered := time.Now().Add(-48 * time.Hour)
	if _, ok := requestAt(StatePRReview, entered).GateDeadline(time.Hour); ok {
		t.Error("pr_review has a gate deadline")
	}
	if _, ok := requestAt(StateSpecReview, entered).GateDeadline(0); ok {
		t.Error("an expiry of 0 gives a deadline")
	}
}

func TestExpireIfDueHaltsOnlyAGatePastItsExpiry(t *testing.T) {
	dataDir := t.TempDir()
	now := time.Now()
	r := requestAt(StatePlanReview, now.Add(-3*time.Hour))
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	if _, expired, err := ExpireIfDue(dataDir, r.ID, 4*time.Hour, now); err != nil || expired {
		t.Fatalf("within the expiry: expired %v err %v, want neither", expired, err)
	}
	if _, expired, err := ExpireIfDue(dataDir, r.ID, 0, now); err != nil || expired {
		t.Fatalf("expiry off: expired %v err %v, want neither", expired, err)
	}
	got, expired, err := ExpireIfDue(dataDir, r.ID, 2*time.Hour, now)
	if err != nil || !expired {
		t.Fatalf("past the expiry: expired %v err %v", expired, err)
	}
	if got.State != StateHalted || got.HaltKind != HaltGateExpired || !strings.Contains(got.Error, "plan_review expired: no decision in 2h0m0s (review_gate_expiry)") {
		t.Errorf("after = state %q kind %q error %q", got.State, got.HaltKind, got.Error)
	}
	saved, err := Load(dataDir, r.ID)
	if err != nil || saved.State != StateHalted || saved.HaltKind != HaltGateExpired {
		t.Errorf("saved = %+v (%v), want the halt on disk", saved, err)
	}
	if _, expired, _ := ExpireIfDue(dataDir, r.ID, 2*time.Hour, now); expired {
		t.Error("a halted request expired again")
	}
}

func TestReopenReviewFromGateExpiredHaltOnly(t *testing.T) {
	now := time.Now()
	for _, gate := range []State{StateSpecReview, StatePlanReview} {
		r := requestAt(gate, now.Add(-5*time.Hour))
		r.NotifyCount, r.LastAsk = 3, "review"
		if err := r.HaltGateExpired(time.Hour, now); err != nil {
			t.Fatalf("%s: %v", gate, err)
		}
		if err := r.ReopenReview("alice", "", now); err != nil {
			t.Fatalf("%s: ReopenReview: %v", gate, err)
		}
		if r.State != gate || r.Error != "" || r.HaltKind != "" || r.NotifyCount != 0 || r.LastAsk != "" {
			t.Errorf("%s: after = state %q error %q kind %q notify %d ask %q, want the same review with fresh bookkeeping", gate, r.State, r.Error, r.HaltKind, r.NotifyCount, r.LastAsk)
		}
		if since, ok := r.InStateSince(); !ok || since.Before(now.Add(-time.Second)) {
			t.Errorf("%s: EnteredAt = %v, want it restarted", gate, since)
		}
	}

	for name, r := range map[string]*Request{
		"an ordinary halt": func() *Request {
			r := requestAt(StatePlanReview, now)
			_ = r.Halt("boom", now)
			return r
		}(),
		"a review": requestAt(StateSpecReview, now),
	} {
		if err := r.ReopenReview("alice", "", now); !errors.Is(err, ErrIllegalTransition) {
			t.Errorf("%s: ReopenReview = %v, want ErrIllegalTransition", name, err)
		}
	}

	lost := requestAt(StateResumeReview, now.Add(-5*time.Hour))
	if err := lost.HaltGateExpired(time.Hour, now); err != nil {
		t.Fatal(err)
	}
	if err := lost.ReopenReview("alice", "", now); !errors.Is(err, ErrIllegalTransition) {
		t.Errorf("a resume_review with no lost step recorded: ReopenReview = %v, want a refusal", err)
	}
	lost.Resume = &ResumeInfo{FromState: StateBuilding, LostRunID: "run-1", Generation: 1}
	if err := lost.ReopenReview("alice", "", now); err != nil || lost.State != StateResumeReview {
		t.Errorf("a resume_review with its lost step: state %q err %v, want resume_review", lost.State, err)
	}
}

func TestRetryOnAnExpiredGateReopensTheReview(t *testing.T) {
	dataDir := t.TempDir()
	now := time.Now()
	r := requestAt(StateSpecReview, now.Add(-5*time.Hour))
	if err := r.HaltGateExpired(time.Hour, now); err != nil {
		t.Fatal(err)
	}
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	got, err := Retry(dataDir, r.ID, "alice", "back from leave", now, nil)
	if err != nil {
		t.Fatalf("Retry: %v", err)
	}
	if got.State != StateSpecReview || got.HaltKind != "" {
		t.Errorf("after retry = state %q kind %q, want spec_review: no redraft", got.State, got.HaltKind)
	}
	if last := got.History[len(got.History)-1]; !strings.Contains(last.Reason, "back from leave") || last.By != "alice" {
		t.Errorf("last history = %+v, want the operator and reason", last)
	}
	if _, err := RetryFromRound(dataDir, r.ID, "alice", "", 1, now, nil); !errors.Is(err, ErrIllegalTransition) {
		t.Errorf("RetryFromRound on a reopened review = %v, want a refusal", err)
	}
}

func TestExpiredRefusalNamesTheRetry(t *testing.T) {
	r := requestAt(StateSpecReview, time.Now().Add(-5*time.Hour))
	if err := r.HaltGateExpired(time.Hour, time.Now()); err != nil {
		t.Fatal(err)
	}
	err := r.ExpiredRefusal()
	if !errors.Is(err, ErrIllegalTransition) || !strings.Contains(err.Error(), "spec_review expired") || !strings.Contains(err.Error(), "factoryd retry req-5") {
		t.Errorf("ExpiredRefusal = %v, want the expiry and the retry that reopens it", err)
	}
}
