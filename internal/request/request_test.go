package request

import (
	"reflect"
	"testing"
	"time"
)

var fixedNow = time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)

func TestNewCreatesRequestInSubmittedState(t *testing.T) {
	r := New("req-1", "/repos/app", "app", Source{Kind: SourceText}, fixedNow)
	if r.State != StateSubmitted {
		t.Errorf("State = %q, want %q", r.State, StateSubmitted)
	}
	wantTS := "2026-09-11T12:00:00Z"
	if r.SubmittedAt != wantTS {
		t.Errorf("SubmittedAt = %q, want %q", r.SubmittedAt, wantTS)
	}
	if r.UpdatedAt != wantTS || r.EnteredAt != wantTS {
		t.Errorf("UpdatedAt/EnteredAt = %q/%q, want both %q", r.UpdatedAt, r.EnteredAt, wantTS)
	}
}

// TestLegalTransitionsSucceed covers every legal edge in the plan's state
// machine: submitted -> spec_drafting -> spec_review -> planning ->
// plan_review -> building -> pr_review -> done, plus Quarantine/Halt/
// Cancel from every non-terminal state.
func TestLegalTransitionsSucceed(t *testing.T) {
	cases := []struct {
		name string
		fn   func(r *Request) error
		want State
	}{
		{"StartSpecDrafting", func(r *Request) error { return r.StartSpecDrafting(fixedNow) }, StateSpecDrafting},
		{"CompleteSpecDrafting", func(r *Request) error { r.State = StateSpecDrafting; return r.CompleteSpecDrafting(fixedNow) }, StateSpecReview},
		{"ApproveSpec", func(r *Request) error { r.State = StateSpecReview; return r.ApproveSpec("op", fixedNow) }, StatePlanning},
		{"RejectSpec", func(r *Request) error { r.State = StateSpecReview; return r.RejectSpec("op", "not good", fixedNow) }, StateSpecDrafting},
		{"CompletePlanning", func(r *Request) error { r.State = StatePlanning; return r.CompletePlanning(fixedNow) }, StatePlanReview},
		{"ApprovePlan", func(r *Request) error { r.State = StatePlanReview; return r.ApprovePlan("op", fixedNow) }, StateBuilding},
		{"RejectPlan", func(r *Request) error { r.State = StatePlanReview; return r.RejectPlan("op", "not good", fixedNow) }, StatePlanning},
		{"StartPRReview", func(r *Request) error { r.State = StateBuilding; return r.StartPRReview(fixedNow) }, StatePRReview},
		{"Complete", func(r *Request) error { r.State = StatePRReview; return r.Complete(fixedNow) }, StateDone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := New("req-1", "/repos/app", "app", Source{Kind: SourceText}, fixedNow)
			if err := tc.fn(r); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if r.State != tc.want {
				t.Errorf("State = %q, want %q", r.State, tc.want)
			}
		})
	}

	for _, from := range nonTerminalStates {
		t.Run("Quarantine from "+string(from), func(t *testing.T) {
			r := New("req-1", "/repos/app", "app", Source{Kind: SourceText}, fixedNow)
			r.State = from
			if err := r.Quarantine("gate failed", fixedNow); err != nil {
				t.Fatalf("Quarantine: %v", err)
			}
			if r.State != StateQuarantined {
				t.Errorf("State = %q, want %q", r.State, StateQuarantined)
			}
			if r.Error != "gate failed" {
				t.Errorf("Error = %q, want %q", r.Error, "gate failed")
			}
		})
		t.Run("Halt from "+string(from), func(t *testing.T) {
			r := New("req-1", "/repos/app", "app", Source{Kind: SourceText}, fixedNow)
			r.State = from
			if err := r.Halt("infra failure", fixedNow); err != nil {
				t.Fatalf("Halt: %v", err)
			}
			if r.State != StateHalted {
				t.Errorf("State = %q, want %q", r.State, StateHalted)
			}
			if r.Error != "infra failure" {
				t.Errorf("Error = %q, want %q", r.Error, "infra failure")
			}
		})
		t.Run("Cancel from "+string(from), func(t *testing.T) {
			r := New("req-1", "/repos/app", "app", Source{Kind: SourceText}, fixedNow)
			r.State = from
			if err := r.Cancel("op", "", fixedNow); err != nil {
				t.Fatalf("Cancel: %v", err)
			}
			if r.State != StateCancelled {
				t.Errorf("State = %q, want %q", r.State, StateCancelled)
			}
		})
	}
}

// TestTransitionFunctionsAppendOneHistoryEntry covers every named
// transition function in this file: a successful call must append
// exactly one Transition to History, with From/To/By set correctly (see
// each function's own doc comment for what By should be).
func TestTransitionFunctionsAppendOneHistoryEntry(t *testing.T) {
	cases := []struct {
		name     string
		fn       func(r *Request) error
		wantFrom State
		wantTo   State
		wantBy   string
	}{
		{"StartSpecDrafting", func(r *Request) error { return r.StartSpecDrafting(fixedNow) }, StateSubmitted, StateSpecDrafting, factoryActor},
		{"CompleteSpecDrafting", func(r *Request) error { r.State = StateSpecDrafting; return r.CompleteSpecDrafting(fixedNow) }, StateSpecDrafting, StateSpecReview, factoryActor},
		{"ApproveSpec", func(r *Request) error { r.State = StateSpecReview; return r.ApproveSpec("alice", fixedNow) }, StateSpecReview, StatePlanning, "alice"},
		{"RejectSpec", func(r *Request) error {
			r.State = StateSpecReview
			return r.RejectSpec("alice", "needs work", fixedNow)
		}, StateSpecReview, StateSpecDrafting, "alice"},
		{"CompletePlanning", func(r *Request) error {
			r.State = StatePlanning
			r.TicketCount = 3
			return r.CompletePlanning(fixedNow)
		}, StatePlanning, StatePlanReview, factoryActor},
		{"ApprovePlan", func(r *Request) error { r.State = StatePlanReview; return r.ApprovePlan("alice", fixedNow) }, StatePlanReview, StateBuilding, "alice"},
		{"RejectPlan", func(r *Request) error {
			r.State = StatePlanReview
			return r.RejectPlan("alice", "needs work", fixedNow)
		}, StatePlanReview, StatePlanning, "alice"},
		{"StartPRReview", func(r *Request) error { r.State = StateBuilding; return r.StartPRReview(fixedNow) }, StateBuilding, StatePRReview, factoryActor},
		{"ResumeBuilding", func(r *Request) error { r.State = StatePRReview; return r.ResumeBuilding(fixedNow) }, StatePRReview, StateBuilding, factoryActor},
		{"Complete", func(r *Request) error { r.State = StatePRReview; return r.Complete(fixedNow) }, StatePRReview, StateDone, factoryActor},
		{"Quarantine", func(r *Request) error { return r.Quarantine("gate failed", fixedNow) }, StateSubmitted, StateQuarantined, factoryActor},
		{"Halt", func(r *Request) error { return r.Halt("infra failure", fixedNow) }, StateSubmitted, StateHalted, factoryActor},
		{"Cancel", func(r *Request) error { return r.Cancel("op", "", fixedNow) }, StateSubmitted, StateCancelled, "op"},
		{"ResumeDrafting", func(r *Request) error {
			r.State = StateHalted
			return r.ResumeDrafting(StateSpecDrafting, "op", "", fixedNow)
		}, StateHalted, StateSpecDrafting, "op"},
		{"ResumeReview", func(r *Request) error { r.State = StateQuarantined; return r.ResumeReview("op", "", fixedNow) }, StateQuarantined, StatePRReview, "op"},
		{"Retry", func(r *Request) error { r.State = StateHalted; return r.Retry("op", "", fixedNow) }, StateHalted, StateBuilding, "op"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := New("req-1", "/repos/app", "app", Source{Kind: SourceText}, fixedNow)
			if err := tc.fn(r); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if len(r.History) != 1 {
				t.Fatalf("History = %+v, want exactly one entry", r.History)
			}
			got := r.History[0]
			if got.From != tc.wantFrom || got.To != tc.wantTo {
				t.Errorf("History[0] From/To = %q/%q, want %q/%q", got.From, got.To, tc.wantFrom, tc.wantTo)
			}
			if got.By != tc.wantBy {
				t.Errorf("History[0].By = %q, want %q", got.By, tc.wantBy)
			}
			if got.At == "" {
				t.Errorf("History[0].At is empty")
			}
		})
	}
}

// TestIllegalTransitionsReturnErrorAndDoNotMutate covers every named
// transition function called from a state it does not allow: it must
// return an error and leave the request completely unchanged (state,
// timestamps, everything).
func TestIllegalTransitionsReturnErrorAndDoNotMutate(t *testing.T) {
	allStates := []State{
		StateSubmitted, StateSpecDrafting, StateSpecReview,
		StatePlanning, StatePlanReview, StateBuilding, StatePRReview,
		StateDone, StateQuarantined, StateHalted, StateCancelled,
	}
	transitions := []struct {
		name    string
		allowed State
		fn      func(r *Request) error
	}{
		{"StartSpecDrafting", StateSubmitted, func(r *Request) error { return r.StartSpecDrafting(fixedNow) }},
		{"CompleteSpecDrafting", StateSpecDrafting, func(r *Request) error { return r.CompleteSpecDrafting(fixedNow) }},
		{"ApproveSpec", StateSpecReview, func(r *Request) error { return r.ApproveSpec("op", fixedNow) }},
		{"RejectSpec", StateSpecReview, func(r *Request) error { return r.RejectSpec("op", "not good", fixedNow) }},
		{"CompletePlanning", StatePlanning, func(r *Request) error { return r.CompletePlanning(fixedNow) }},
		{"ApprovePlan", StatePlanReview, func(r *Request) error { return r.ApprovePlan("op", fixedNow) }},
		{"RejectPlan", StatePlanReview, func(r *Request) error { return r.RejectPlan("op", "not good", fixedNow) }},
		{"StartPRReview", StateBuilding, func(r *Request) error { return r.StartPRReview(fixedNow) }},
		{"Complete", StatePRReview, func(r *Request) error { return r.Complete(fixedNow) }},
	}

	for _, tr := range transitions {
		for _, from := range allStates {
			if from == tr.allowed {
				continue
			}
			t.Run(tr.name+" from "+string(from), func(t *testing.T) {
				r := New("req-1", "/repos/app", "app", Source{Kind: SourceText}, fixedNow)
				r.State = from
				before := *r
				err := tr.fn(r)
				if err == nil {
					t.Fatalf("%s from %q: want an error, got nil", tr.name, from)
				}
				if !reflect.DeepEqual(*r, before) {
					t.Errorf("%s from %q mutated the request on failure: before=%+v after=%+v", tr.name, from, before, *r)
				}
			})
		}
	}

	// Quarantine/Halt from every terminal state must also fail, including
	// a same-state self-transition (e.g. quarantined -> quarantined).
	// Cancel is exercised separately below: unlike Quarantine/Halt, it is
	// legal from quarantined/halted (an operator dismissing a dead
	// request), so it only fails from done and from cancelled itself.
	for _, from := range []State{StateDone, StateQuarantined, StateHalted, StateCancelled} {
		for _, fn := range []struct {
			name string
			call func(r *Request) error
		}{
			{"Quarantine", func(r *Request) error { return r.Quarantine("x", fixedNow) }},
			{"Halt", func(r *Request) error { return r.Halt("x", fixedNow) }},
		} {
			t.Run(fn.name+" from terminal "+string(from), func(t *testing.T) {
				r := New("req-1", "/repos/app", "app", Source{Kind: SourceText}, fixedNow)
				r.State = from
				before := *r
				if err := fn.call(r); err == nil {
					t.Fatalf("%s from terminal %q: want an error, got nil", fn.name, from)
				}
				if !reflect.DeepEqual(*r, before) {
					t.Errorf("%s from terminal %q mutated the request on failure", fn.name, from)
				}
			})
		}
	}

	for _, from := range []State{StateDone, StateCancelled} {
		t.Run("Cancel from terminal "+string(from), func(t *testing.T) {
			r := New("req-1", "/repos/app", "app", Source{Kind: SourceText}, fixedNow)
			r.State = from
			before := *r
			if err := r.Cancel("op", "", fixedNow); err == nil {
				t.Fatalf("Cancel from terminal %q: want an error, got nil", from)
			}
			if !reflect.DeepEqual(*r, before) {
				t.Errorf("Cancel from terminal %q mutated the request on failure", from)
			}
		})
	}

	// Cancel from quarantined/halted is now legal (an operator dismissing
	// a dead request) -- see cancellableStates' own doc comment.
	for _, from := range []State{StateQuarantined, StateHalted} {
		t.Run("Cancel from "+string(from)+" succeeds", func(t *testing.T) {
			r := New("req-1", "/repos/app", "app", Source{Kind: SourceText}, fixedNow)
			r.State = from
			if err := r.Cancel("op", "", fixedNow); err != nil {
				t.Fatalf("Cancel from %q: %v", from, err)
			}
			if r.State != StateCancelled {
				t.Errorf("State = %q, want %q", r.State, StateCancelled)
			}
		})
	}
}

func TestWaitingOnQueuesEveryJobStateBehindTheOldest(t *testing.T) {
	requests := []*Request{
		{ID: "a-review", State: StateSpecReview},
		{ID: "b-build", State: StateBuilding},
		{ID: "c-pr", State: StatePRReview},
		{ID: "d-plan", State: StatePlanning},
		{ID: "e-submitted", State: StateSubmitted},
		{ID: "f-quarantined", State: StateQuarantined},
	}
	got := WaitingOn(requests, nil, 1)
	want := map[string]string{"d-plan": "b-build", "e-submitted": "b-build"}
	if len(got) != len(want) {
		t.Fatalf("WaitingOn = %v, want %v", got, want)
	}
	for id, head := range want {
		if got[id] != head {
			t.Errorf("WaitingOn[%q] = %q, want %q", id, got[id], head)
		}
	}
	if len(WaitingOn(requests[:2], nil, 1)) != 0 {
		t.Error("a single job-state request must not wait on anything")
	}
}

// TestWaitingOnPrefersTheActiveRequest: found live 2026-09-26. An older
// request re-entered planning (its plan was rejected) while a newer one's
// build was running; the build is what everyone waits on.
func TestWaitingOnPrefersTheActiveRequest(t *testing.T) {
	requests := []*Request{
		{ID: "old-planning", State: StatePlanning},
		{ID: "new-building", State: StateBuilding},
		{ID: "newer-building", State: StateBuilding},
	}
	got := WaitingOn(requests, []string{"new-building"}, 1)
	if _, ok := got["new-building"]; ok {
		t.Errorf("the active request must not wait: %v", got)
	}
	if got["old-planning"] != "new-building" || got["newer-building"] != "new-building" {
		t.Errorf("WaitingOn = %v, want both others behind new-building", got)
	}
	// An active id no longer in the list (cancelled and pruned) falls back
	// to the oldest job-state request.
	if got := WaitingOn(requests, []string{"gone"}, 1); got["new-building"] != "old-planning" {
		t.Errorf("fallback WaitingOn = %v, want new-building behind old-planning", got)
	}
}

// TestWaitingOnWithSeveralSlots: a worker running 3 jobs at once leaves
// nobody waiting until every slot is taken; then each job-state request that
// is not running waits on the first active one, and review states never wait.
func TestWaitingOnWithSeveralSlots(t *testing.T) {
	requests := []*Request{
		{ID: "a-build", State: StateBuilding},
		{ID: "b-plan", State: StatePlanning},
		{ID: "c-review", State: StateSpecReview},
		{ID: "d-build", State: StateBuilding},
		{ID: "e-submitted", State: StateSubmitted},
		{ID: "f-plan", State: StatePlanning},
	}
	if got := WaitingOn(requests, []string{"a-build", "b-plan"}, 3); len(got) != 0 {
		t.Errorf("not full: WaitingOn = %v, want nobody waiting", got)
	}
	got := WaitingOn(requests, []string{"b-plan", "a-build", "d-build"}, 3)
	want := map[string]string{"f-plan": "b-plan"}
	if len(got) != len(want) || got["f-plan"] != "b-plan" {
		t.Errorf("full: WaitingOn = %v, want %v", got, want)
	}
}

// TestJobSpendAddAccumulatesAcrossRedraft covers JobSpend.Add's own
// contract (see its doc comment): a nil receiver returns other unchanged
// (the first attempt), a nil argument returns the receiver unchanged, and
// two real spends sum their token/cost fields, OR their SpendPartial, and
// keep the later At -- the accumulation cmd/factoryd's drafting jobs rely
// on so a re-draft never silently drops an earlier attempt's real spend.
func TestJobSpendAddAccumulatesAcrossRedraft(t *testing.T) {
	var nilSpend *JobSpend
	first := &JobSpend{Role: "planning", Model: "gpt-5.6-luna", InputTokens: 10, OutputTokens: 5, CostMicroUSD: 100, At: time.Unix(1000, 0)}
	if got := nilSpend.Add(first); got != first {
		t.Errorf("nil.Add(first) = %+v, want the exact first pointer back", got)
	}
	if got := first.Add(nil); got != first {
		t.Errorf("first.Add(nil) = %+v, want the exact first pointer back", got)
	}

	second := &JobSpend{Role: "planning", Model: "gpt-5.6-luna", InputTokens: 4, OutputTokens: 2, CostMicroUSD: 40, SpendPartial: true, At: time.Unix(2000, 0)}
	got := first.Add(second)
	if got.InputTokens != 14 || got.OutputTokens != 7 || got.CostMicroUSD != 140 {
		t.Errorf("first.Add(second) tokens/cost = %+v, want input=14 output=7 cost=140", got)
	}
	if !got.SpendPartial {
		t.Error("first.Add(second).SpendPartial = false, want true (second was partial)")
	}
	if !got.At.Equal(time.Unix(2000, 0)) {
		t.Errorf("first.Add(second).At = %v, want the later timestamp", got.At)
	}
	if got.Role != "planning" || got.Model != "gpt-5.6-luna" {
		t.Errorf("first.Add(second) Role/Model = %q/%q, want carried through", got.Role, got.Model)
	}

	// A second spend with an empty Role/Model (should never happen in
	// practice, but Add must not silently blank out a real value) falls
	// back to the receiver's own.
	blank := &JobSpend{InputTokens: 1}
	if got := first.Add(blank); got.Role != "planning" || got.Model != "gpt-5.6-luna" {
		t.Errorf("first.Add(blank) Role/Model = %q/%q, want first's own carried through", got.Role, got.Model)
	}
}

// TestJobSpendAddKeepsEachModelsShare covers a role whose model changes
// between the summed attempts: the totals still sum, and Shares returns what
// each role/model pair spent. Attempts on one model record no split.
func TestJobSpendAddKeepsEachModelsShare(t *testing.T) {
	local := &JobSpend{Role: "planning", Model: "model-local", InputTokens: 10, OutputTokens: 5}
	sameModel := local.Add(&JobSpend{Role: "planning", Model: "model-local", InputTokens: 1, OutputTokens: 1})
	if len(sameModel.ByModel) != 0 {
		t.Errorf("ByModel = %+v after two attempts on one model, want none", sameModel.ByModel)
	}
	if got, want := sameModel.Shares(), []ModelSpend{{Role: "planning", Model: "model-local", InputTokens: 11, OutputTokens: 6}}; !reflect.DeepEqual(got, want) {
		t.Errorf("Shares() = %+v, want %+v", got, want)
	}

	hosted := &JobSpend{Role: "planning", Model: "model-hosted", InputTokens: 100, OutputTokens: 50, CostMicroUSD: 7}
	// An attempt that recorded no model is its own share, under no model.
	unnamed := &JobSpend{Role: "planning", InputTokens: 3}
	// The right-hand side carries a split of its own, as one planning pass
	// that re-planned on another model does.
	got := sameModel.Add(hosted.Add(unnamed).Add(local))
	if got.InputTokens != 124 || got.OutputTokens != 61 || got.CostMicroUSD != 7 {
		t.Errorf("totals = %+v, want input=124 output=61 cost=7", got)
	}
	if got.Model != "model-local" {
		t.Errorf("Model = %q, want the latest attempt's", got.Model)
	}
	want := []ModelSpend{
		{Role: "planning", Model: "model-local", InputTokens: 21, OutputTokens: 11},
		{Role: "planning", Model: "model-hosted", InputTokens: 100, OutputTokens: 50, CostMicroUSD: 7},
		{Role: "planning", InputTokens: 3},
	}
	if !reflect.DeepEqual(got.Shares(), want) {
		t.Errorf("Shares() = %+v, want %+v", got.Shares(), want)
	}
	var sum ModelSpend
	for _, share := range got.Shares() {
		sum.InputTokens += share.InputTokens
		sum.OutputTokens += share.OutputTokens
		sum.CostMicroUSD += share.CostMicroUSD
	}
	if sum.InputTokens != got.InputTokens || sum.OutputTokens != got.OutputTokens || sum.CostMicroUSD != got.CostMicroUSD {
		t.Errorf("shares sum to %+v, want the totals of %+v", sum, got)
	}
	if local.ByModel != nil || hosted.ByModel != nil || len(sameModel.ByModel) != 0 {
		t.Error("Add changed one of its operands")
	}
	if (*JobSpend)(nil).Shares() != nil {
		t.Error("nil.Shares() != nil")
	}
}
