package request

import "testing"

func historyWith(entries ...Transition) *Request {
	r := New("req-1", "/repos/app", "app", Source{Kind: SourceText}, fixedNow)
	r.History = append(r.History, entries...)
	r.Tickets = []Ticket{{Index: 1}, {Index: 2}}
	return r
}

func TestScopeApproved(t *testing.T) {
	personApproved := Transition{From: StatePlanReview, To: StateBuilding, By: "kanna"}
	factoryApproved := Transition{From: StatePlanning, To: StateBuilding, By: factoryActor}
	replanned := Transition{From: StatePlanReview, To: StatePlanning, By: "kanna", Reason: "split it"}
	for _, tc := range []struct {
		name   string
		mutate func(r *Request)
		want   bool
	}{
		{"a person approved the plan at plan_review", func(r *Request) { r.History = append(r.History, personApproved) }, true},
		{"the factory approved a one-ticket plan itself", func(r *Request) { r.History = append(r.History, factoryApproved) }, false},
		{"a handed-over plan the factory approved itself", func(r *Request) {
			r.History = append(r.History, factoryApproved)
			r.PlanImported = true
		}, true},
		{"a hand-over that plan-stage feedback ended", func(r *Request) {
			r.History = append(r.History, factoryApproved)
			r.PlanImported, r.PlanImportEnded = true, true
		}, false},
		{"a person's reject and re-plan, then their approval", func(r *Request) {
			r.History = append(r.History, replanned, personApproved)
		}, true},
		{"the factory's approval after an earlier person's approval of an older plan", func(r *Request) {
			r.History = append(r.History, personApproved, replanned, factoryApproved)
		}, false},
		{"amend-scope on the ticket after the factory approved", func(r *Request) {
			r.History = append(r.History, factoryApproved)
			r.Tickets[0].ScopeAmendedBy = "kanna"
		}, true},
		{"a record with no plan decision at all", func(r *Request) {}, true},
		{"later transitions are not plan decisions", func(r *Request) {
			r.History = append(r.History, factoryApproved,
				Transition{From: StateBuilding, To: StatePRReview, By: factoryActor},
				Transition{From: StatePRReview, To: StateBuilding, By: factoryActor})
		}, false},
	} {
		r := historyWith()
		tc.mutate(r)
		got, why := ScopeApproved(r, 1)
		if got != tc.want || why == "" {
			t.Errorf("%s: ScopeApproved = %v (%q), want %v with a reason", tc.name, got, why, tc.want)
		}
	}
}

func TestScopeApprovedAmendIsPerTicket(t *testing.T) {
	r := historyWith(Transition{From: StatePlanning, To: StateBuilding, By: factoryActor})
	r.Tickets[1].ScopeAmendedBy = "kanna"
	if got, _ := ScopeApproved(r, 1); got {
		t.Error("ticket 1 was widened by nobody, yet its list counts as approved")
	}
	if got, _ := ScopeApproved(r, 2); !got {
		t.Error("ticket 2 was widened by a person, yet its list counts as unapproved")
	}
}

func TestAmendScopeRecordsWhoWidenedTheTicket(t *testing.T) {
	dataDir := t.TempDir()
	newAmendScopeFixture(t, dataDir, "req-1", "Verify-Command: go test ./...\nAllowed-Files: a.go\n\n## Goal\n\nx\n")
	r, err := AmendScope(dataDir, "req-1", "kanna", "also b.go", []string{"b.go"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if by := r.Tickets[0].ScopeAmendedBy; by != "kanna" {
		t.Errorf("ScopeAmendedBy = %q, want kanna", by)
	}
}
