package request

import (
	"strings"
	"testing"
)

// TestNextActionForPRReviewSaysWhatEachPullRequestWaitsOn: a request in
// pr_review used to have no next step at all, so the console and `status`
// showed a state and nothing to do about it.
func TestNextActionForPRReviewSaysWhatEachPullRequestWaitsOn(t *testing.T) {
	pr := func(n int, state string) Ticket {
		return Ticket{Index: n, PRURL: "https://example.test/pull/" + string(rune('0'+n)), PRState: state}
	}
	for _, c := range []struct {
		name    string
		tickets []Ticket
		want    []string
		wantNot []string
	}{
		{"one ready pull request", []Ticket{pr(1, "ready")},
			[]string{"review https://example.test/pull/1", "pr_trusted_authors", "corrective round", "done when every pull request is merged"},
			[]string{"is approved", "stacked", "draft"}},
		{"approved", []Ticket{pr(1, "approved")},
			[]string{"merge https://example.test/pull/1", "the factory never merges"},
			[]string{"review https://"}},
		{"a draft the factory is still working on", []Ticket{pr(1, "draft")},
			[]string{"https://example.test/pull/1 is still a draft", "checks pass"}, nil},
		{"merged, ready and stacked together", []Ticket{pr(1, "merged"), pr(2, "ready"), pr(3, "stacked")},
			[]string{"review https://example.test/pull/2", "https://example.test/pull/3 is stacked", "merge that one first"},
			[]string{"pull/1"}},
		{"two ready pull requests are named together", []Ticket{pr(1, "ready"), pr(2, "ready")},
			[]string{"review https://example.test/pull/1, https://example.test/pull/2:"}, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := New("req-1", "/w", "w", Source{Kind: SourceText}, fixedNow)
			r.State, r.Tickets = StatePRReview, c.tickets
			got := r.NextAction()
			for _, want := range c.want {
				if !strings.Contains(got, want) {
					t.Errorf("NextAction() = %q, want it to contain %q", got, want)
				}
			}
			for _, not := range c.wantNot {
				if strings.Contains(got, not) {
					t.Errorf("NextAction() = %q, want it not to contain %q", got, not)
				}
			}
		})
	}
}

// TestNextActionForPRReviewIsEmptyWithoutAnOpenPullRequest: nothing to act
// on before a pull request exists or once every one is merged.
func TestNextActionForPRReviewIsEmptyWithoutAnOpenPullRequest(t *testing.T) {
	r := New("req-1", "/w", "w", Source{Kind: SourceText}, fixedNow)
	r.State = StatePRReview
	if got := r.NextAction(); got != "" {
		t.Errorf("no tickets: NextAction() = %q, want empty", got)
	}
	r.Tickets = []Ticket{{Index: 1, RunID: "run-1"}, {Index: 2, PRURL: "https://example.test/pull/2", PRState: "merged"}}
	if got := r.NextAction(); got != "" {
		t.Errorf("no open pull request: NextAction() = %q, want empty", got)
	}
}

// TestNextActionForPRReviewNamesAFailedCorrectiveRound: found on a real
// corrective round. The round was quarantined, nothing was pushed, and the
// request still said "review ... or leave review comments".
func TestNextActionForPRReviewNamesAFailedCorrectiveRound(t *testing.T) {
	r := New("req-1", "/w", "w", Source{Kind: SourceText}, fixedNow)
	r.State = StatePRReview
	r.Tickets = []Ticket{{
		Index: 1, PRURL: "https://example.test/pull/1", PRState: "ready",
		Rounds: []Round{
			{Index: 1, RunID: "run-r1", Outcome: RoundAccepted},
			{Index: 2, RunID: "run-r2", Outcome: RoundQuarantined, Error: "policy gate did not pass: code_review"},
		},
	}}
	got := r.NextAction()
	for _, want := range []string{
		"corrective round 2 on https://example.test/pull/1 was quarantined and pushed nothing",
		"(policy gate did not pass: code_review)",
		"max_review_rounds",
		"resolve the thread",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("NextAction() = %q, want it to contain %q", got, want)
		}
	}
	if strings.Contains(got, "approve and merge it") {
		t.Errorf("NextAction() = %q, still reads as a plain review", got)
	}

	// A later accepted round answers the failure, and a conformity round is
	// not a PR-review round.
	r.Tickets[0].Rounds = append(r.Tickets[0].Rounds, Round{Index: 3, Outcome: RoundAccepted})
	if got := r.NextAction(); !strings.Contains(got, "approve and merge it") {
		t.Errorf("after an accepted round: NextAction() = %q", got)
	}
	r.Tickets[0].Rounds = []Round{{Index: 1, Kind: ConformityRoundKind, Outcome: RoundQuarantined}}
	if got := r.NextAction(); !strings.Contains(got, "approve and merge it") {
		t.Errorf("a conformity round only: NextAction() = %q", got)
	}
}

// TestNextActionForPRReviewNamesAReadyToMergePullRequest: once a poll has
// checked a pull request against the ready-to-merge bar, the next step says
// to merge it, or names what the bar still lacks.
func TestNextActionForPRReviewNamesAReadyToMergePullRequest(t *testing.T) {
	pr := func(state string, readiness *MergeReadiness) Ticket {
		return Ticket{Index: 1, PRURL: "https://example.test/pull/1", PRState: state, MergeReadiness: readiness}
	}
	notReady := &MergeReadiness{Blockers: []string{"its checks are pending or failing", "1 review thread(s) are open"}}
	for _, c := range []struct {
		name    string
		ticket  Ticket
		want    []string
		wantNot []string
	}{
		{"ready and passing the bar", pr("ready", &MergeReadiness{Ready: true}),
			[]string{"merge https://example.test/pull/1: ready to merge", "checks pass", "no review thread is open", "code review of the whole diff is clean", "the factory never merges"},
			[]string{"review https://"}},
		{"approved and passing the bar", pr("approved", &MergeReadiness{Ready: true}),
			[]string{"merge https://example.test/pull/1: ready to merge"}, []string{"it is approved"}},
		{"approved on GitHub but below the bar", pr("approved", notReady),
			[]string{"https://example.test/pull/1 is not ready to merge: its checks are pending or failing; 1 review thread(s) are open"},
			[]string{"merge https://"}},
		{"a draft is described as a draft", pr("draft", &MergeReadiness{Blockers: []string{"it is still a draft"}}),
			[]string{"is still a draft: the factory marks it ready"}, []string{"not ready to merge"}},
		{"not yet checked", pr("ready", nil),
			[]string{"review https://example.test/pull/1"}, []string{"ready to merge"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := New("req-1", "/w", "w", Source{Kind: SourceText}, fixedNow)
			r.State, r.Tickets = StatePRReview, []Ticket{c.ticket}
			got := r.NextAction()
			for _, want := range c.want {
				if !strings.Contains(got, want) {
					t.Errorf("next action lacks %q:\n%s", want, got)
				}
			}
			for _, wantNot := range c.wantNot {
				if strings.Contains(got, wantNot) {
					t.Errorf("next action has %q:\n%s", wantNot, got)
				}
			}
		})
	}
}
