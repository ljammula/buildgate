package request

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newQuarantinedRequestWithPlan builds a quarantined/halted request that
// looks like #295's own case: an approved spec, an approved plan (one ticket
// pinned), a build that started on ticket 1 but never reached accepted --
// exactly the shape SendBack exists to unstick. approvedSpec/approvedPlan
// let a test omit either pin to exercise SendBackToPlan's own precondition.
func newQuarantinedRequestWithPlan(t *testing.T, dataDir, id string, state State, approvedSpec, approvedPlan bool) *Request {
	t.Helper()
	r := newApprovableRequest(t, dataDir, id, state, true)
	r.ApprovedSHA256 = map[string]string{}
	if approvedSpec {
		r.ApprovedSHA256[specFileName] = "deadbeef"
	}
	if approvedPlan {
		r.ApprovedSHA256["tickets/001.spec.md"] = "cafef00d"
		r.ApprovedSHA256["tickets/002.spec.md"] = "cafef00d"
		r.TicketCount = 1
		r.TicketIndex = 1
		r.Tickets = []Ticket{{Index: 1}}
	}
	if err := r.Save(dataDir); err != nil {
		t.Fatalf("Save: %v", err)
	}
	return r
}

func TestSendBackQuarantinedToPlanningPrunesTicketPinsKeepsSpecPin(t *testing.T) {
	dataDir := t.TempDir()
	newQuarantinedRequestWithPlan(t, dataDir, "req-1", StateQuarantined, true, true)

	got, err := SendBack(dataDir, "req-1", "alice", "diff_scope: allow contract_matrix_phase2_test.go", SendBackToPlan, fixedNow)
	if err != nil {
		t.Fatalf("SendBack: %v", err)
	}
	if got.State != StatePlanning {
		t.Errorf("State = %q, want %q", got.State, StatePlanning)
	}
	if _, ok := got.ApprovedSHA256[specFileName]; !ok {
		t.Errorf("ApprovedSHA256 lost %q, want it kept", specFileName)
	}
	for relPath := range got.ApprovedSHA256 {
		if strings.HasPrefix(relPath, "tickets/") {
			t.Errorf("ApprovedSHA256 still has %q, want ticket pins pruned", relPath)
		}
	}
	if got.Tickets != nil || got.TicketIndex != 0 || got.TicketCount != 0 {
		t.Errorf("Tickets/TicketIndex/TicketCount = %v/%d/%d, want all reset", got.Tickets, got.TicketIndex, got.TicketCount)
	}
	// The audit trail keeps the literal state the request left; ForStage
	// alone routes the note to the plan redraft.
	if len(got.Rejections) != 1 || got.Rejections[0].FromState != StateQuarantined || got.Rejections[0].ForStage != StatePlanReview {
		t.Errorf("Rejections = %+v, want one entry with from_state %q and for_stage %q", got.Rejections, StateQuarantined, StatePlanReview)
	}
}

func TestSendBackNoteReachesPlanFeedback(t *testing.T) {
	dataDir := t.TempDir()
	newQuarantinedRequestWithPlan(t, dataDir, "req-1", StateQuarantined, true, true)

	got, err := SendBack(dataDir, "req-1", "alice", "allow contract_matrix_phase2_test.go", SendBackToPlan, fixedNow)
	if err != nil {
		t.Fatalf("SendBack: %v", err)
	}
	if fb := PlanFeedback(got); !strings.Contains(fb, "allow contract_matrix_phase2_test.go") {
		t.Errorf("PlanFeedback = %q, want it to contain the send-back reason", fb)
	}
	if fb := SpecFeedback(got); fb != "" {
		t.Errorf("SpecFeedback = %q, want empty -- a plan send-back must never leak into the spec drafter", fb)
	}
}

func TestSendBackToSpecDropsSpecAndTicketPinsAndNoteReachesSpecFeedback(t *testing.T) {
	dataDir := t.TempDir()
	newQuarantinedRequestWithPlan(t, dataDir, "req-1", StateHalted, true, true)

	got, err := SendBack(dataDir, "req-1", "alice", "reword criterion 8, it is unsatisfiable at handler level", SendBackToSpec, fixedNow)
	if err != nil {
		t.Fatalf("SendBack: %v", err)
	}
	if got.State != StateSpecDrafting {
		t.Errorf("State = %q, want %q", got.State, StateSpecDrafting)
	}
	if len(got.ApprovedSHA256) != 0 {
		t.Errorf("ApprovedSHA256 = %+v, want every pin dropped", got.ApprovedSHA256)
	}
	if fb := SpecFeedback(got); !strings.Contains(fb, "reword criterion 8") {
		t.Errorf("SpecFeedback = %q, want it to contain the send-back reason", fb)
	}
	if fb := PlanFeedback(got); fb != "" {
		t.Errorf("PlanFeedback = %q, want empty -- a spec send-back must never leak into the plan drafter", fb)
	}
}

func TestSendBackRefusedWhenATicketIsAccepted(t *testing.T) {
	dataDir := t.TempDir()
	r := newQuarantinedRequestWithPlan(t, dataDir, "req-1", StateQuarantined, true, true)
	r.Tickets = []Ticket{{Index: 1, Branch: "factoryd/req-1-ticket1"}}
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}

	_, err := SendBack(dataDir, "req-1", "alice", "try a different plan", SendBackToPlan, fixedNow)
	if !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("err = %v, want ErrIllegalTransition", err)
	}
	if !strings.Contains(err.Error(), "cancel") {
		t.Errorf("err = %v, want it to name `factoryd cancel` as the alternative", err)
	}
}

func TestSendBackRefusedWithoutReason(t *testing.T) {
	dataDir := t.TempDir()
	newQuarantinedRequestWithPlan(t, dataDir, "req-1", StateQuarantined, true, true)

	_, err := SendBack(dataDir, "req-1", "alice", "", SendBackToPlan, fixedNow)
	if err == nil || !strings.Contains(err.Error(), "reason") {
		t.Fatalf("err = %v, want a reason-required error", err)
	}
}

func TestSendBackHaltedAlsoWorks(t *testing.T) {
	dataDir := t.TempDir()
	newQuarantinedRequestWithPlan(t, dataDir, "req-1", StateHalted, true, true)

	got, err := SendBack(dataDir, "req-1", "alice", "relay flaked mid-build", SendBackToPlan, fixedNow)
	if err != nil {
		t.Fatalf("SendBack: %v", err)
	}
	if got.State != StatePlanning {
		t.Errorf("State = %q, want %q", got.State, StatePlanning)
	}
}

func TestSendBackToPlanRefusedWithoutApprovedSpec(t *testing.T) {
	dataDir := t.TempDir()
	newQuarantinedRequestWithPlan(t, dataDir, "req-1", StateQuarantined, false, false)

	_, err := SendBack(dataDir, "req-1", "alice", "try again", SendBackToPlan, fixedNow)
	if err == nil || !strings.Contains(err.Error(), "spec") {
		t.Fatalf("err = %v, want an error naming spec.md as missing", err)
	}

	// SendBackToSpec is the one target left valid in this shape.
	got, err := SendBack(dataDir, "req-1", "alice", "start over", SendBackToSpec, fixedNow)
	if err != nil {
		t.Fatalf("SendBack to spec: %v", err)
	}
	if got.State != StateSpecDrafting {
		t.Errorf("State = %q, want %q", got.State, StateSpecDrafting)
	}
}

func TestSendBackRefusedForHaltOracleMaterialize(t *testing.T) {
	dataDir := t.TempDir()
	r := newQuarantinedRequestWithPlan(t, dataDir, "req-1", StateHalted, true, false)
	r.HaltKind = HaltOracleMaterialize
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}

	_, err := SendBack(dataDir, "req-1", "alice", "try again", SendBackToPlan, fixedNow)
	if !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("err = %v, want ErrIllegalTransition", err)
	}
	if !strings.Contains(err.Error(), "factoryd retry") {
		t.Errorf("err = %v, want it to name `factoryd retry` as the actual recovery", err)
	}
}

func TestSendBackInvalidTargetRefused(t *testing.T) {
	dataDir := t.TempDir()
	_, err := SendBack(dataDir, "does-not-exist", "alice", "a reason", SendBackTarget("bogus"), fixedNow)
	if err == nil || !strings.Contains(err.Error(), "bogus") {
		t.Fatalf("err = %v, want it to name the invalid target -- checked before any file is even loaded", err)
	}
}

// An adversarial review of SendBack (2026-09-26) found: a send-back to
// spec must drop the request-level oracle/* pins too, or the redrafted
// oracle fails every later VerifyApprovedHashes and cancel is the only exit.
func TestSendBackToSpecDropsRequestOraclePinsAndSkipWarning(t *testing.T) {
	dataDir := t.TempDir()
	r := newQuarantinedRequestWithPlan(t, dataDir, "req-1", StateQuarantined, true, true)
	r.DraftOracles = true
	r.ApprovedSHA256[requestOracleRelPrefix+"test_missed_days.py"] = "0ac1e"
	r.OracleSkipWarning = "the oracle stage is being skipped"
	if err := r.Save(dataDir); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := SendBack(dataDir, "req-1", "alice", "reword criterion 8", SendBackToSpec, fixedNow)
	if err != nil {
		t.Fatalf("SendBack: %v", err)
	}
	if len(got.ApprovedSHA256) != 0 {
		t.Errorf("ApprovedSHA256 = %v, want every pin (spec, tickets, oracle) dropped", got.ApprovedSHA256)
	}
	if got.OracleSkipWarning != "" {
		t.Errorf("OracleSkipWarning = %q, want cleared", got.OracleSkipWarning)
	}
}

// The same adversarial review found: a -draft-oracles request whose
// oracle review was never approved for the current spec must not reach
// planning (it would build with no oracle and no skip warning).
func TestSendBackToPlanRequiresOracleApprovalForCurrentSpec(t *testing.T) {
	specApproved := Transition{From: StateSpecReview, To: StateOracleDrafting}
	oracleApproved := Transition{From: StateOracleReview, To: StatePlanning}
	cases := []struct {
		name    string
		history []Transition
		wantErr bool
	}{
		{"oracle never reviewed", []Transition{specApproved}, true},
		{"oracle approved after spec", []Transition{specApproved, oracleApproved}, false},
		{"oracle approved only for an earlier spec", []Transition{specApproved, oracleApproved, specApproved}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dataDir := t.TempDir()
			r := newQuarantinedRequestWithPlan(t, dataDir, "req-1", StateHalted, true, false)
			r.DraftOracles = true
			r.History = append(r.History, tc.history...)
			if err := r.Save(dataDir); err != nil {
				t.Fatalf("Save: %v", err)
			}
			_, err := SendBack(dataDir, "req-1", "alice", "replan", SendBackToPlan, fixedNow)
			if tc.wantErr {
				if !errors.Is(err, ErrIllegalTransition) {
					t.Fatalf("SendBack err = %v, want ErrIllegalTransition", err)
				}
				if got, _ := Load(dataDir, "req-1"); got.State != StateHalted {
					t.Errorf("State = %q, want unchanged %q", got.State, StateHalted)
				}
				return
			}
			if err != nil {
				t.Fatalf("SendBack: %v", err)
			}
		})
	}
}

// The same adversarial review also found: plan notes written before a
// send-back to spec target the old spec and must not reach the new plan
// drafter; later plan notes still do.
func TestPlanFeedbackDropsNotesFromBeforeASpecSendBack(t *testing.T) {
	r := &Request{Rejections: []Rejection{
		{By: "alice", At: "t1", Reason: "split ticket 2", FromState: StatePlanReview},
		{By: "alice", At: "t2", Reason: "reword criterion 8", FromState: StateQuarantined, ForStage: StateSpecReview},
		{By: "alice", At: "t3", Reason: "keep dispatch_mux.go in scope", FromState: StatePlanReview},
	}}
	got := PlanFeedback(r)
	if strings.Contains(got, "split ticket 2") {
		t.Errorf("PlanFeedback kept a note from before the spec send-back:\n%s", got)
	}
	if !strings.Contains(got, "keep dispatch_mux.go in scope") {
		t.Errorf("PlanFeedback lost the note recorded after the spec send-back:\n%s", got)
	}
	if spec := SpecFeedback(r); !strings.Contains(spec, "reword criterion 8") || strings.Contains(spec, "split ticket 2") {
		t.Errorf("SpecFeedback = %q, want only the spec send-back note", spec)
	}
}

// A round-2 review found: a send-back to spec moves the oracle files
// drafted against the old spec aside and forgets the draft, so a
// failed redraft can't leave them in oracle/ to be approved and pinned.
func TestSendBackToSpecSupersedesOldOracleFiles(t *testing.T) {
	dataDir := t.TempDir()
	r := newQuarantinedRequestWithPlan(t, dataDir, "req-1", StateQuarantined, true, true)
	r.DraftOracles = true
	r.OracleDraft = &OracleDraft{Status: "drafted", Files: []string{"test_missed_days.py"}}
	if err := r.Save(dataDir); err != nil {
		t.Fatalf("Save: %v", err)
	}
	oracleDir := filepath.Join(Dir(dataDir, "req-1"), RequestOracleDirName)
	if err := os.MkdirAll(oracleDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oracleDir, "test_missed_days.py"), []byte("old oracle"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := SendBack(dataDir, "req-1", "alice", "reword criterion 8", SendBackToSpec, fixedNow)
	if err != nil {
		t.Fatalf("SendBack: %v", err)
	}
	if _, err := os.Stat(oracleDir); !os.IsNotExist(err) {
		t.Errorf("oracle/ still present (err=%v), want moved aside", err)
	}
	moved, err := os.ReadFile(filepath.Join(Dir(dataDir, "req-1"), RequestOracleDirName+"-rejected", "test_missed_days.py"))
	if err != nil || string(moved) != "old oracle" {
		t.Errorf("oracle-rejected/test_missed_days.py = %q, %v; want the old oracle kept for reference", moved, err)
	}
	if got.OracleDraft != nil {
		t.Errorf("OracleDraft = %+v, want nil", got.OracleDraft)
	}
}

// The same round-2 review also found: SendBackPlanAllowed is the one
// rule the hint, SendBack and the API's can_send_back_to_plan share.
func TestSendBackPlanAllowed(t *testing.T) {
	specApproved := Transition{From: StateSpecReview, To: StateOracleDrafting}
	oracleApproved := Transition{From: StateOracleReview, To: StatePlanning}
	cases := []struct {
		name         string
		specPinned   bool
		draftOracles bool
		history      []Transition
		want         bool
	}{
		{"no approved spec", false, false, nil, false},
		{"approved spec, no oracles", true, false, nil, true},
		{"oracles never reviewed", true, true, []Transition{specApproved}, false},
		{"oracles reviewed after spec", true, true, []Transition{specApproved, oracleApproved}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &Request{DraftOracles: tc.draftOracles, History: tc.history, ApprovedSHA256: map[string]string{}}
			if tc.specPinned {
				r.ApprovedSHA256[specFileName] = "deadbeef"
			}
			if got := r.SendBackPlanAllowed(); got != tc.want {
				t.Errorf("SendBackPlanAllowed = %v, want %v", got, tc.want)
			}
		})
	}
}

// The factory's note about a refused draft goes when a draft reaches review,
// and when an operator sends the request back (their feedback drives the redraft).
func TestDraftHaltNoteClearedWhenTheDraftReachesReview(t *testing.T) {
	for _, tc := range []struct {
		name  string
		from  State
		stage State
		step  func(r *Request) error
	}{
		{"spec", StateSpecDrafting, StateSpecDrafting, func(r *Request) error { return r.CompleteSpecDrafting(fixedNow) }},
		{"plan", StatePlanning, StatePlanning, func(r *Request) error { return r.CompletePlanning(fixedNow) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &Request{State: tc.from, DraftHalt: &DraftHalt{Stage: tc.stage, Reason: "missing ## Scope", At: "t"}}
			if err := tc.step(r); err != nil {
				t.Fatal(err)
			}
			if r.DraftHalt != nil {
				t.Errorf("DraftHalt = %+v, want cleared once the draft reached review", r.DraftHalt)
			}
		})
	}
	// A halt and a retry keep it.
	r := &Request{State: StatePlanning, DraftHalt: &DraftHalt{Stage: StatePlanning, Reason: "wrong verify command", At: "t"}}
	if err := r.Halt("plan invalid", fixedNow); err != nil {
		t.Fatal(err)
	}
	if err := r.ResumeDrafting(StatePlanning, "alice", "", fixedNow); err != nil {
		t.Fatal(err)
	}
	if r.DraftHalt == nil || r.DraftHalt.Reason != "wrong verify command" {
		t.Errorf("DraftHalt = %+v, want it kept across halted and retry", r.DraftHalt)
	}
	if r.PlanAsHandedOver() || r.SpecAsHandedOver() {
		t.Error("a draft note must not change the handed-over answers")
	}
}

func TestDraftHaltNoteClearedBySendBack(t *testing.T) {
	dataDir := t.TempDir()
	r := newQuarantinedRequestWithPlan(t, dataDir, "req-1", StateHalted, true, true)
	r.DraftHalt = &DraftHalt{Stage: StatePlanning, Reason: "ticket 001.spec.md declares Verify-Command \"make wrong-xyz\"", At: "t"}
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	got, err := SendBack(dataDir, "req-1", "alice", "reword criterion 3", SendBackToSpec, fixedNow)
	if err != nil {
		t.Fatalf("SendBack: %v", err)
	}
	if got.DraftHalt != nil {
		t.Errorf("DraftHalt = %+v, want cleared by the send-back", got.DraftHalt)
	}
}
