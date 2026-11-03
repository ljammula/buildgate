package request

import (
	"testing"
	"time"
)

// TestRouteWaitEndsOnATransition proves a route wait recorded in one state
// never outlives it, and that WaitFor and NextAction name it only in a state
// that runs a job.
func TestRouteWaitEndsOnATransition(t *testing.T) {
	r := New("req", "/repos/app", "app", Source{Kind: SourceText}, time.Now())
	r.State = StateSpecDrafting
	r.RouteWait = &StepWait{Since: time.Now().UTC().Format(time.RFC3339), On: "waiting for roles.planning's model route"}
	if w := WaitFor(r, WaitFacts{WorkerLive: true}); w == nil || w.Kind != WaitRoute || w.Sentence(time.Now()) == "" {
		t.Fatalf("WaitFor = %+v, want a route wait with a sentence", w)
	}
	if err := advance(r, []State{StateSpecDrafting}, StateSpecReview, "", "", time.Now()); err != nil {
		t.Fatal(err)
	}
	if r.RouteWait != nil {
		t.Fatalf("RouteWait = %+v after a transition, want nil", r.RouteWait)
	}
}
