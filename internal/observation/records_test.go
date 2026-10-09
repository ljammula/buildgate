package observation

import (
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"buildgate/internal/request"
	"buildgate/internal/run"
)

func quarantinedOn(id, ticket string, checks ...string) *run.Run {
	r := finished(id, run.StateQuarantined)
	r.Ticket, r.UpdatedAt = ticket, "2026-10-08T09:00:00Z"
	for _, c := range checks {
		r.GateResults = append(r.GateResults, run.GateResult{Check: c, ExitCode: 1})
	}
	return r
}

func acceptedAfter(id, ticket, earlier string, files ...string) *run.Run {
	r := finished(id, run.StateAccepted, passed(1, files...))
	r.Ticket, r.EarlierAttemptOf, r.UpdatedAt = ticket, earlier, "2026-10-08T10:00:00Z"
	return r
}

func ofKind(list []Observation, kind string) []Observation {
	var out []Observation
	for _, o := range list {
		if o.Kind == kind {
			out = append(out, o)
		}
	}
	return out
}

var sentences = func(r *run.Run) map[string]string {
	return map[string]string{"canonical_verify": "The verify command failed on TestKey.\x1b[31m"}
}

func TestCheckFixedThroughARetryGivenTheRecordIsFromTheRun(t *testing.T) {
	q := quarantinedOn("q1", "t-1", "canonical_verify", "tests_added")
	a := acceptedAfter("a1", "t-1", "q1", "key.go", "key_test.go")
	report := FromRuns("app", []*run.Run{q, a}, nil, Sources{Sentences: sentences})
	got := ofKind(report.Observations, KindCheckFixed)
	if len(got) != 1 {
		t.Fatalf("check_fixed = %+v, want one", got)
	}
	o := got[0]
	if o.Source != SourceRun || o.RunID != "q1" || o.AcceptedRunID != "a1" || o.At != a.UpdatedAt {
		t.Errorf("observation = %+v", o)
	}
	want := []CheckNote{{Check: "canonical_verify", Sentence: "The verify command failed on TestKey."}, {Check: "tests_added"}}
	if !reflect.DeepEqual(o.Checks, want) {
		t.Errorf("Checks = %+v, want %+v", o.Checks, want)
	}
	if !reflect.DeepEqual(o.ChangedFiles, []string{"key.go", "key_test.go"}) {
		t.Errorf("ChangedFiles = %v", o.ChangedFiles)
	}
	if want := handID(KindCheckFixed, "q1,a1", "", "canonical_verify,tests_added"); o.ID != want {
		t.Errorf("ID = %s, want %s", o.ID, want)
	}
}

func TestCheckFixedThroughACorrectiveRoundIsFromTheRequest(t *testing.T) {
	q := quarantinedOn("q1", "t-1", "canonical_verify")
	a := acceptedAfter("a1", "t-1", "q1")
	req := &request.Request{ID: "req-1", Project: "app", Tickets: []request.Ticket{{Index: 1, Rounds: []request.Round{
		{Index: 1, Kind: request.CorrectiveRoundKind, RunID: "a1", Outcome: request.RoundAccepted},
	}}}}
	got := ofKind(FromRuns("app", []*run.Run{q, a}, []*request.Request{req}, Sources{}).Observations, KindCheckFixed)
	if len(got) != 1 || got[0].Source != SourceRequest {
		t.Fatalf("check_fixed = %+v, want one from the request", got)
	}
}

func TestCheckFixedConformityRoundFindsTheQuarantinedRunOnItsBranch(t *testing.T) {
	q := quarantinedOn("q1", "t-1", "spec_conformity")
	q.RequestID, q.Branch = "req-1", "factoryd/q1"
	a := acceptedAfter("a1", "t-1", "")
	a.RequestID, a.Branch = "req-1", "factoryd/q1"
	req := &request.Request{ID: "req-1", Project: "app", Tickets: []request.Ticket{{Index: 1, Rounds: []request.Round{
		{Index: 1, Kind: request.ConformityRoundKind, RunID: "a1", Outcome: request.RoundAccepted},
	}}}}
	got := ofKind(FromRuns("app", []*run.Run{q, a}, []*request.Request{req}, Sources{}).Observations, KindCheckFixed)
	if len(got) != 1 || got[0].RunID != "q1" || got[0].Source != SourceRequest {
		t.Fatalf("check_fixed = %+v", got)
	}
}

func TestCheckFixedNegatives(t *testing.T) {
	// No later accepted run.
	q := quarantinedOn("q1", "t-1", "canonical_verify")
	if got := ofKind(FromRuns("app", []*run.Run{q}, nil, Sources{}).Observations, KindCheckFixed); len(got) != 0 {
		t.Errorf("a quarantined run alone: %+v", got)
	}
	// The corrective round was quarantined too.
	q2 := quarantinedOn("q2", "t-1", "canonical_verify")
	q2.EarlierAttemptOf = "q1"
	if got := ofKind(FromRuns("app", []*run.Run{q, q2}, nil, Sources{}).Observations, KindCheckFixed); len(got) != 0 {
		t.Errorf("a quarantined corrective round: %+v", got)
	}
	// An accepted run of another ticket is not a fix.
	other := acceptedAfter("a1", "t-2", "q1")
	if got := ofKind(FromRuns("app", []*run.Run{q, other}, nil, Sources{}).Observations, KindCheckFixed); len(got) != 0 {
		t.Errorf("another ticket: %+v", got)
	}
	// An earlier run that is not quarantined, or failed no recorded check.
	halted := finished("h1", run.StateHalted)
	halted.Ticket = "t-1"
	noGate := quarantinedOn("q3", "t-1")
	for _, earlier := range []*run.Run{halted, noGate} {
		a := acceptedAfter("a9", "t-1", earlier.ID)
		if got := ofKind(FromRuns("app", []*run.Run{earlier, a}, nil, Sources{}).Observations, KindCheckFixed); len(got) != 0 {
			t.Errorf("earlier %s: %+v", earlier.ID, got)
		}
	}
}

const (
	commentMarker   = "COMMENT-BODY-MARKER-4471"
	editMarker      = "EDIT-TEXT-MARKER-4472"
	rejectionMarker = "REJECTION-REASON-MARKER-4473"
)

func requestWithRecords() *request.Request {
	return &request.Request{
		ID: "req-1", Project: "app",
		Tickets: []request.Ticket{{Index: 2, Rounds: []request.Round{
			{Index: 1, ThreadIDs: []string{"PRRT_a", "PRRT_b"}, RunID: "pr-run-1", Outcome: request.RoundAccepted, Pushed: true, At: "2026-10-08T11:00:00Z", Error: commentMarker},
			{Index: 2, ThreadIDs: []string{"PRRT_c"}, RunID: "pr-run-2", Outcome: request.RoundAccepted, At: "2026-10-08T11:30:00Z"},
			{Index: 3, ThreadIDs: []string{"PRRT_d"}, RunID: "pr-run-3", Outcome: request.RoundQuarantined, At: "2026-10-08T11:40:00Z"},
			{Index: 1, Kind: request.CorrectiveRoundKind, RunID: "c-run", Outcome: request.RoundAccepted, Pushed: true},
		}}},
		Edits: []request.Edit{
			{By: "operator", At: "2026-10-08T08:00:00Z", Path: "spec.md", FromState: request.StateSpecReview, Diff: "+ " + editMarker},
			{By: "operator", At: "2026-10-08T08:10:00Z", Path: "tickets/001.spec.md", FromState: request.StatePlanReview, Diff: "- " + editMarker},
			{By: "operator", At: "2026-10-08T08:20:00Z", Path: "oracle/RUN_COMMAND.txt", FromState: request.StateOracleReview, Diff: editMarker},
		},
		Rejections: []request.Rejection{
			{By: "operator", At: "2026-10-08T08:30:00Z", FromState: request.StateSpecReview, Reason: rejectionMarker,
				Anchors: []request.RejectionAnchor{{Path: "spec.md", Section: "## Acceptance criteria", Item: 2, Note: rejectionMarker}}, Note: rejectionMarker},
			{By: "operator", At: "2026-10-08T08:40:00Z", FromState: request.StateOracleReview, Reason: rejectionMarker},
			{By: "operator", At: "2026-10-08T08:50:00Z", FromState: request.StateQuarantined, ForStage: request.StatePlanReview, Reason: rejectionMarker},
		},
	}
}

func TestReviewCommentAcceptedIsAPushedPullRequestRoundWithThreadIDsOnly(t *testing.T) {
	got := ofKind(FromRuns("app", nil, []*request.Request{requestWithRecords()}, Sources{}).Observations, KindReviewCommentAccepted)
	if len(got) != 1 {
		t.Fatalf("review_comment_accepted = %+v, want only the pushed accepted PR round", got)
	}
	o := got[0]
	if o.Source != SourceRequest || o.RequestID != "req-1" || o.TicketIndex != 2 || o.RunID != "pr-run-1" ||
		!reflect.DeepEqual(o.Rounds, []int{1}) || !reflect.DeepEqual(o.ThreadIDs, []string{"PRRT_a", "PRRT_b"}) || o.At != "2026-10-08T11:00:00Z" {
		t.Errorf("observation = %+v", o)
	}
	if want := handID(KindReviewCommentAccepted, "pr-run-1", "1", ""); o.ID != want {
		t.Errorf("ID = %s, want %s", o.ID, want)
	}
}

func TestOperatorEditCoversEditsAndRejectionsAtSpecAndPlanOnly(t *testing.T) {
	got := ofKind(FromRuns("app", nil, []*request.Request{requestWithRecords()}, Sources{}).Observations, KindOperatorEdit)
	type row struct{ stage, at string }
	var rows []row
	for _, o := range got {
		rows = append(rows, row{o.Stage, o.At})
		if o.Source != SourceRequest || o.RequestID != "req-1" || o.RunID != "" {
			t.Errorf("observation = %+v", o)
		}
	}
	// Newest first; the oracle-stage edit and rejection are left out; a
	// send-back counts at the stage it feeds.
	want := []row{
		{"plan_review", "2026-10-08T08:50:00Z"}, {"spec_review", "2026-10-08T08:30:00Z"},
		{"plan_review", "2026-10-08T08:10:00Z"}, {"spec_review", "2026-10-08T08:00:00Z"},
	}
	if !reflect.DeepEqual(rows, want) {
		t.Errorf("rows = %v, want %v", rows, want)
	}
	for _, o := range got {
		if o.At == "2026-10-08T08:30:00Z" && !reflect.DeepEqual(o.Anchors, []string{"spec.md ## Acceptance criteria" + fmt.Sprintf(" item %d", 2)}) {
			t.Errorf("rejection anchors = %v", o.Anchors)
		}
		if o.At == "2026-10-08T08:10:00Z" && !reflect.DeepEqual(o.Anchors, []string{"tickets/001.spec.md"}) {
			t.Errorf("edit anchors = %v", o.Anchors)
		}
	}
	if want := handID(KindOperatorEdit, "req-1", "1", "rejection"); !hasID(got, want) {
		t.Errorf("no operator_edit has the hand-computed id %s", want)
	}
}

func hasID(list []Observation, id string) bool {
	for _, o := range list {
		if o.ID == id {
			return true
		}
	}
	return false
}

func TestNewKindsCarryNoCommentEditOrRejectionText(t *testing.T) {
	q := quarantinedOn("q1", "t-1", "canonical_verify")
	a := acceptedAfter("a1", "t-1", "q1")
	report := FromRuns("app", []*run.Run{q, a}, []*request.Request{requestWithRecords()}, Sources{Sentences: sentences})
	b, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{commentMarker, editMarker, rejectionMarker} {
		if strings.Contains(string(b), marker) {
			t.Errorf("report carries %s: %s", marker, b)
		}
	}
}

func TestNamesTakenFromRecordsAreCleanedAndCapped(t *testing.T) {
	req := &request.Request{ID: "req-1", Project: "app", Tickets: []request.Ticket{{Index: 1, Rounds: []request.Round{
		{Index: 1, ThreadIDs: []string{"PRRT_a\n\x1b[31mb", strings.Repeat("x", 500)}, RunID: "r", Outcome: request.RoundAccepted, Pushed: true},
	}}}}
	o := ofKind(FromRuns("app", nil, []*request.Request{req}, Sources{}).Observations, KindReviewCommentAccepted)[0]
	if o.ThreadIDs[0] != "PRRT_a b" || len(o.ThreadIDs[1]) != maxNameBytes {
		t.Errorf("ThreadIDs = %q", o.ThreadIDs)
	}
}

var idPattern = regexp.MustCompile(`^[0-9a-f]{16}$`)

// Every observation of a rich fixture has a well-formed id, no two share
// one, and a second derivation gives the same ids.
func TestIDsAreWellFormedUniqueAndStable(t *testing.T) {
	runs := func() []*run.Run {
		multi := finished("m1", run.StateQuarantined, failed(1, "a"), failed(2, "a", "no changes made to the workspace"), failed(3, "b"))
		multi.GateResults = []run.GateResult{{Check: "canonical_verify", ExitCode: 1}, {Check: "tests_added"}, {Check: "diff_scope", ExitCode: 2}}
		fixed := finished("f1", run.StateAccepted, failed(1, "a"), passed(2, "a.go"), failed(3, "c"), passed(4, "b.go"))
		q := quarantinedOn("q1", "t-1", "canonical_verify")
		q2 := quarantinedOn("q2", "t-1", "tests_added")
		return []*run.Run{multi, fixed, q, q2, acceptedAfter("a1", "t-1", "q1"), acceptedAfter("a2", "t-1", "q2"), finished("h1", run.StateHalted)}
	}
	derive := func() []Observation {
		return FromRuns("app", runs(), []*request.Request{requestWithRecords()}, Sources{}).Observations
	}
	first, second := derive(), derive()
	seen := map[string]Observation{}
	for _, o := range first {
		if !idPattern.MatchString(o.ID) {
			t.Errorf("id %q of %s is not 16 hex characters", o.ID, o.Kind)
		}
		if other, dup := seen[o.ID]; dup {
			t.Errorf("%s and %s share id %s", o.What, other.What, o.ID)
		}
		seen[o.ID] = o
	}
	if len(first) < 15 {
		t.Errorf("only %d observations: the fixture lost some", len(first))
	}
	if len(first) != len(second) {
		t.Fatalf("%d observations then %d", len(first), len(second))
	}
	for i := range first {
		if first[i].ID != second[i].ID {
			t.Errorf("observation %d: id %s then %s", i, first[i].ID, second[i].ID)
		}
	}
}

func TestSignatureIsTheFailedRoundsAndAbsentOtherwise(t *testing.T) {
	r := finished("r1", run.StateQuarantined, failed(1, "sig-1"), failed(2, "sig-1"))
	r.GateResults = []run.GateResult{{Check: "canonical_verify", ExitCode: 1}}
	for _, o := range FromRun(r) {
		if o.Signature != "sig-1" {
			t.Errorf("%s signature = %q", o.Kind, o.Signature)
		}
	}
	h := finished("h1", run.StateHalted)
	if got := FromRun(h); got[0].Signature != "" {
		t.Errorf("halt signature = %q", got[0].Signature)
	}
}
