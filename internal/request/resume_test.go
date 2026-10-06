package request

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func requestIn(state State) *Request {
	r := New("req-1", "/repos/app", "app", Source{Kind: SourceText}, fixedNow)
	r.State = state
	r.prevState = state
	return r
}

func TestEnterResumeReviewFromEveryLostStepState(t *testing.T) {
	for _, from := range []State{StateSpecDrafting, StateOracleDrafting, StatePlanning, StateBuilding, StatePRReview} {
		t.Run(string(from), func(t *testing.T) {
			r := requestIn(from)
			r.WaitingSince, r.NotifyCount = "earlier", 3
			if err := r.EnterResumeReview(from, "run-lost", fixedNow); err != nil {
				t.Fatalf("EnterResumeReview: %v", err)
			}
			if r.State != StateResumeReview {
				t.Fatalf("State = %q, want resume_review", r.State)
			}
			want := wantResume(from)
			if r.Resume == nil || r.Resume.FromState != from || r.Resume.LostRunID != "run-lost" || r.Resume.Generation != 1 {
				t.Errorf("Resume = %+v, want %+v", r.Resume, want)
			}
			if r.Error != ResumePrompt("req-1", from) {
				t.Errorf("Error = %q, want the resume prompt", r.Error)
			}
			last := r.History[len(r.History)-1]
			if last.From != from || last.To != StateResumeReview || last.By != factoryActor || last.Reason != r.Error {
				t.Errorf("history entry = %+v", last)
			}
			if r.WaitingSince != "" || r.NotifyCount != 0 {
				t.Errorf("reminder state not reset: %q %d", r.WaitingSince, r.NotifyCount)
			}
			for _, word := range []string{string(from), "factoryd resume req-1", "-from scratch", "factoryd cancel req-1"} {
				if !strings.Contains(r.Error, word) {
					t.Errorf("prompt %q lacks %q", r.Error, word)
				}
			}
		})
	}
}

// wantResume is the ResumeInfo expected for a first entry from from.
func wantResume(from State) *ResumeInfo {
	return &ResumeInfo{FromState: from, LostRunID: "run-lost", Generation: 1}
}

func TestEnterResumeReviewRefusesEveryOtherState(t *testing.T) {
	for _, from := range []State{StateSubmitted, StateSpecReview, StateOracleReview, StatePlanReview, StateDone, StateQuarantined, StateHalted, StateCancelled, StateResumeReview} {
		r := requestIn(from)
		err := r.EnterResumeReview(from, "", fixedNow)
		if !errors.Is(err, ErrIllegalTransition) {
			t.Errorf("from %s: err = %v, want ErrIllegalTransition", from, err)
		}
		if r.State != from || r.Resume != nil {
			t.Errorf("from %s: request changed on a refused entry: %q %+v", from, r.State, r.Resume)
		}
	}
	// The named state must be the request's own.
	r := requestIn(StateBuilding)
	if err := r.EnterResumeReview(StatePlanning, "", fixedNow); !errors.Is(err, ErrIllegalTransition) {
		t.Errorf("mismatched from: err = %v, want ErrIllegalTransition", err)
	}
}

func TestResumeDecideReturnsToTheLostState(t *testing.T) {
	for _, from := range []State{StateSpecDrafting, StateOracleDrafting, StatePlanning, StateBuilding, StatePRReview} {
		for _, verb := range []string{ResumeRound, ResumeScratch} {
			t.Run(string(from)+"/"+verb, func(t *testing.T) {
				r := requestIn(from)
				if err := r.EnterResumeReview(from, "run-lost", fixedNow); err != nil {
					t.Fatal(err)
				}
				to, err := r.ResumeDecide(verb, "alice", fixedNow.Add(time.Minute))
				if err != nil {
					t.Fatalf("ResumeDecide: %v", err)
				}
				if to != from || r.State != from {
					t.Fatalf("returned to %q (state %q), want %q", to, r.State, from)
				}
				d := r.ResumeDecision
				if d == nil || d.Verb != verb || d.Generation != 1 || d.By != "alice" || d.At == "" {
					t.Errorf("ResumeDecision = %+v", d)
				}
				if r.PendingResumeVerb() != verb {
					t.Errorf("PendingResumeVerb = %q, want %q", r.PendingResumeVerb(), verb)
				}
				if r.Error != "" {
					t.Errorf("Error = %q, want cleared", r.Error)
				}
				if r.Resume == nil || r.Resume.LostRunID != "run-lost" {
					t.Errorf("Resume = %+v, want the lost run kept for the step that applies the decision", r.Resume)
				}
				last := r.History[len(r.History)-1]
				if last.From != StateResumeReview || last.To != from || last.By != "alice" {
					t.Errorf("history entry = %+v", last)
				}
			})
		}
	}
}

func TestResumeDecideRefusals(t *testing.T) {
	r := requestIn(StateBuilding)
	if _, err := r.ResumeDecide(ResumeRound, "alice", fixedNow); !errors.Is(err, ErrIllegalTransition) {
		t.Errorf("from building: err = %v, want ErrIllegalTransition", err)
	}
	for _, from := range []State{StateHalted, StateQuarantined, StateCancelled, StateDone, StatePlanReview} {
		r := requestIn(from)
		if _, err := r.ResumeDecide(ResumeRound, "alice", fixedNow); !errors.Is(err, ErrIllegalTransition) {
			t.Errorf("from %s: err = %v, want ErrIllegalTransition", from, err)
		}
	}
	r = requestIn(StateBuilding)
	if err := r.EnterResumeReview(StateBuilding, "run-lost", fixedNow); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ResumeDecide("cancel", "alice", fixedNow); err == nil || errors.Is(err, ErrIllegalTransition) {
		t.Errorf("unknown verb: err = %v, want a plain input error", err)
	}
	if r.State != StateResumeReview || r.ResumeDecision != nil {
		t.Errorf("a refused decision changed the request: %q %+v", r.State, r.ResumeDecision)
	}
}

func TestResumeReviewLeavesToCancelledHaltedAndQuarantined(t *testing.T) {
	enter := func() *Request {
		r := requestIn(StateBuilding)
		if err := r.EnterResumeReview(StateBuilding, "run-lost", fixedNow); err != nil {
			t.Fatal(err)
		}
		return r
	}
	if r := enter(); r.Cancel("alice", "", fixedNow) != nil || r.State != StateCancelled {
		t.Errorf("cancel from resume_review: state %q", r.State)
	}
	if r := enter(); r.Halt("boom", fixedNow) != nil || r.State != StateHalted {
		t.Errorf("halt from resume_review: state %q", r.State)
	}
	if r := enter(); r.Quarantine("gate", fixedNow) != nil || r.State != StateQuarantined {
		t.Errorf("quarantine from resume_review: state %q", r.State)
	}
}

func TestResumeGenerationGrowsAndStaleDecisionIsIgnored(t *testing.T) {
	r := requestIn(StateBuilding)
	if err := r.EnterResumeReview(StateBuilding, "run-1", fixedNow); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ResumeDecide(ResumeRound, "alice", fixedNow); err != nil {
		t.Fatal(err)
	}
	if r.PendingResumeVerb() != ResumeRound {
		t.Fatalf("a decision for the current generation must apply, got %q", r.PendingResumeVerb())
	}
	// The resumed build is lost in turn: a second entry is a new Generation.
	if err := r.EnterResumeReview(StateBuilding, "run-2", fixedNow.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if r.Resume.Generation != 2 || r.Resume.LostRunID != "run-2" {
		t.Fatalf("Resume = %+v, want generation 2 for run-2", r.Resume)
	}
	if r.ResumeDecision != nil {
		t.Errorf("entering resume_review must drop the old decision, got %+v", r.ResumeDecision)
	}
	// Even a decision record that survived (a hand-edited or racing write)
	// answers generation 1 and so does not apply to generation 2.
	r.ResumeDecision = &ResumeDecision{Verb: ResumeRound, Generation: 1, By: "alice"}
	if got := r.PendingResumeVerb(); got != "" {
		t.Errorf("PendingResumeVerb = %q for a stale generation, want none", got)
	}
	r.ResumeDecision.Generation = 2
	if got := r.PendingResumeVerb(); got != ResumeRound {
		t.Errorf("PendingResumeVerb = %q for the current generation, want round", got)
	}
}

func TestEveryOtherTransitionDropsTheResumeDecision(t *testing.T) {
	r := requestIn(StateBuilding)
	if err := r.EnterResumeReview(StateBuilding, "run-1", fixedNow); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ResumeDecide(ResumeRound, "alice", fixedNow); err != nil {
		t.Fatal(err)
	}
	if err := r.Halt("start failed", fixedNow); err != nil {
		t.Fatal(err)
	}
	if r.ResumeDecision != nil {
		t.Errorf("ResumeDecision = %+v after a halt, want none: a later retry rebuilds", r.ResumeDecision)
	}
}

func TestRefuseResumeReturnsToResumeReviewWithReasons(t *testing.T) {
	r := requestIn(StateBuilding)
	if err := r.EnterResumeReview(StateBuilding, "run-1", fixedNow); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ResumeDecide(ResumeRound, "alice", fixedNow); err != nil {
		t.Fatal(err)
	}
	if err := r.RefuseResume([]string{"a container is alive", "history rewritten"}, fixedNow.Add(time.Minute)); err != nil {
		t.Fatalf("RefuseResume: %v", err)
	}
	if r.State != StateResumeReview || r.Resume.Generation != 2 || len(r.Resume.Refused) != 2 {
		t.Fatalf("state %q resume %+v", r.State, r.Resume)
	}
	if r.PendingResumeVerb() != "" {
		t.Error("the refused decision must not apply again")
	}
	for _, want := range []string{"a container is alive", "history rewritten", "-from scratch", "factoryd cancel req-1"} {
		if !strings.Contains(r.Error, want) || !strings.Contains(r.NextAction(), want) {
			t.Errorf("Error %q / NextAction %q lack %q", r.Error, r.NextAction(), want)
		}
	}
	if strings.Contains(r.NextAction(), "factoryd resume req-1`") {
		t.Errorf("NextAction offers a plain resume after a refusal: %q", r.NextAction())
	}
	// A scratch decision after a refusal clears the reasons.
	if _, err := r.ResumeDecide(ResumeScratch, "alice", fixedNow); err != nil {
		t.Fatal(err)
	}
	if len(r.Resume.Refused) != 0 {
		t.Errorf("Refused = %v after a decision", r.Resume.Refused)
	}
	if err := requestIn(StatePlanning).RefuseResume([]string{"x"}, fixedNow); !errors.Is(err, ErrIllegalTransition) {
		t.Errorf("RefuseResume outside building: err = %v", err)
	}
}

func TestRetryRefusesInResumeReviewWithAHint(t *testing.T) {
	dataDir := t.TempDir()
	r := requestIn(StatePlanning)
	if err := r.EnterResumeReview(StatePlanning, "", fixedNow); err != nil {
		t.Fatal(err)
	}
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	_, err := Retry(dataDir, "req-1", "alice", "", fixedNow, nil)
	if !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("Retry err = %v, want ErrIllegalTransition", err)
	}
	if !strings.Contains(err.Error(), "factoryd resume req-1") {
		t.Errorf("Retry err = %q, want the `factoryd resume` hint", err)
	}
	loaded, err := Load(dataDir, "req-1")
	if err != nil || loaded.State != StateResumeReview {
		t.Errorf("request after refused retry: %v %v", loaded, err)
	}
}

func TestResumeRequestPreflightAndSave(t *testing.T) {
	dataDir := t.TempDir()
	r := requestIn(StateBuilding)
	if err := r.EnterResumeReview(StateBuilding, "run-lost", fixedNow); err != nil {
		t.Fatal(err)
	}
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	refuse := func(*Request) ([]string, error) { return []string{"container alive"}, nil }
	_, err := ResumeRequest(dataDir, "req-1", ResumeRound, "alice", fixedNow, refuse)
	var refused *ResumeRefusedError
	if !errors.As(err, &refused) || !errors.Is(err, ErrIllegalTransition) || refused.Reasons[0] != "container alive" {
		t.Fatalf("err = %v, want a ResumeRefusedError", err)
	}
	if loaded, _ := Load(dataDir, "req-1"); loaded.State != StateResumeReview {
		t.Fatalf("a refused resume changed the request to %q", loaded.State)
	}
	// scratch does not consult the preflight: it keeps nothing.
	called := false
	probe := func(*Request) ([]string, error) { called = true; return nil, nil }
	got, err := ResumeRequest(dataDir, "req-1", ResumeScratch, "alice", fixedNow, probe)
	if err != nil || called || got.State != StateBuilding || got.PendingResumeVerb() != ResumeScratch {
		t.Fatalf("scratch: %v called=%v state=%v", err, called, got)
	}
	if _, err := ResumeRequest(dataDir, "req-1", ResumeRound, "alice", fixedNow, nil); !errors.Is(err, ErrIllegalTransition) {
		t.Errorf("second resume of a building request: err = %v, want ErrIllegalTransition", err)
	}
}

func TestResumeRequestChecksDraftingStepsForEveryVerb(t *testing.T) {
	for _, verb := range []string{ResumeRound, ResumeScratch} {
		dataDir := t.TempDir()
		r := requestIn(StateSpecDrafting)
		if err := r.EnterResumeReview(StateSpecDrafting, "", fixedNow); err != nil {
			t.Fatal(err)
		}
		if err := r.Save(dataDir); err != nil {
			t.Fatal(err)
		}
		refuse := func(*Request) ([]string, error) { return []string{"a container is alive"}, nil }
		_, err := ResumeRequest(dataDir, "req-1", verb, "alice", fixedNow, refuse)
		var refused *ResumeRefusedError
		if !errors.As(err, &refused) || !errors.Is(err, ErrIllegalTransition) {
			t.Fatalf("%s: err = %v, want a ResumeRefusedError", verb, err)
		}
		msg := err.Error()
		if !strings.Contains(msg, "lost spec_drafting step: a container is alive") || strings.Contains(msg, "-from scratch") {
			t.Errorf("%s: refusal = %q, want the drafting wording without a rebuild hint", verb, msg)
		}
		if loaded, _ := Load(dataDir, "req-1"); loaded.State != StateResumeReview {
			t.Fatalf("%s: a refused resume changed the request to %q", verb, loaded.State)
		}
		allow := func(*Request) ([]string, error) { return nil, nil }
		got, err := ResumeRequest(dataDir, "req-1", verb, "alice", fixedNow, allow)
		if err != nil || got.State != StateSpecDrafting {
			t.Fatalf("%s: resume once the check passes: %v %v", verb, err, got)
		}
	}
}

func TestResumeReviewIsNotAJobState(t *testing.T) {
	if StateResumeReview.RunsJob() {
		t.Error("resume_review must not run a job: it waits on a human")
	}
	r := requestIn(StateResumeReview)
	if got := WaitingOn([]*Request{r}, nil, 1); len(got) != 0 {
		t.Errorf("WaitingOn = %v, want none", got)
	}
}
