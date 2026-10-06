package request

import (
	"os"
	"path/filepath"
	"testing"
)

// TestRejectPlanPrunesStaleTicketApprovals: after approve-then-reject, a
// re-plan may delete files that were pinned; their keys must be dropped
// (spec.md stays) or VerifyApprovedHashes fails forever on the missing file.
func TestRejectPlanPrunesStaleTicketApprovals(t *testing.T) {
	dataDir := t.TempDir()
	r := newApprovableRequest(t, dataDir, "req-1", StateSpecReview, true)
	if _, err := Approve(dataDir, r.ID, "alice", fixedNow, nil); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(dataDir, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Move to plan_review with tickets pinned, as a real plan approval would
	// have left them, then send it back.
	loaded.State = StatePlanReview
	loaded.ApprovedSHA256["tickets/002.spec.md"] = "deadbeef"
	loaded.ApprovedSHA256["tickets/001.oracle/RUN_COMMAND.txt"] = "deadbeef"
	if err := loaded.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	if _, err := Reject(dataDir, r.ID, "alice", "shrink the plan", fixedNow); err != nil {
		t.Fatalf("Reject: %v", err)
	}
	// The re-plan deleted the old tickets.
	if err := os.RemoveAll(filepath.Join(Dir(dataDir, r.ID), "tickets")); err != nil {
		t.Fatal(err)
	}
	after, err := Load(dataDir, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := after.ApprovedSHA256[specFileName]; !ok {
		t.Errorf("spec.md pin was dropped: %v", after.ApprovedSHA256)
	}
	for key := range after.ApprovedSHA256 {
		if key != specFileName {
			t.Errorf("stale key %q survived re-entering planning", key)
		}
	}
	if err := VerifyApprovedHashes(dataDir, after); err != nil {
		t.Errorf("VerifyApprovedHashes after re-plan deleted tickets: %v", err)
	}
}

// TestResumeDraftingIntoPlanningPrunesTicketApprovals covers the retry path.
func TestResumeDraftingIntoPlanningPrunesTicketApprovals(t *testing.T) {
	r := New("req-1", "/repos/app", "app", Source{Kind: SourceText}, fixedNow)
	r.State = StateHalted
	r.ApprovedSHA256 = map[string]string{specFileName: "a", "tickets/001.spec.md": "b"}
	if err := r.ResumeDrafting(StatePlanning, "op", "", fixedNow); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.ApprovedSHA256["tickets/001.spec.md"]; ok {
		t.Errorf("ticket key survived: %v", r.ApprovedSHA256)
	}
	if r.ApprovedSHA256[specFileName] != "a" {
		t.Errorf("spec.md pin lost: %v", r.ApprovedSHA256)
	}
}

// Per-ticket pins must survive every transition that is not a re-entry to
// planning: a multi-ticket request keeps building after ticket 1, and a halted
// oracle ticket is retried, both against the pins recorded at plan approval
// (found via review: pruning on every transition dropped them, so ticket 2's
// oracle halted with "never approved" and `factoryd retry` of an oracle ticket
// could never recover).
func TestTicketApprovalsSurviveBuildingHaltRetryAndPRReview(t *testing.T) {
	pins := func() map[string]string {
		return map[string]string{
			"spec.md":                            "aa",
			"tickets/001.spec.md":                "bb",
			"tickets/002.spec.md":                "cc",
			"tickets/002.oracle/RUN_COMMAND.txt": "dd",
		}
	}
	assertKept := func(t *testing.T, r *Request, step string) {
		t.Helper()
		for k, v := range pins() {
			if r.ApprovedSHA256[k] != v {
				t.Fatalf("%s: ApprovedSHA256[%q] = %q, want %q kept", step, k, r.ApprovedSHA256[k], v)
			}
		}
	}

	r := &Request{ID: "req-1", State: StatePlanReview, ApprovedSHA256: pins(), TicketCount: 2, TicketIndex: 1}
	if err := r.ApprovePlan("alice", fixedNow); err != nil {
		t.Fatal(err)
	}
	assertKept(t, r, "ApprovePlan")
	if err := r.StartPRReview(fixedNow); err != nil {
		t.Fatal(err)
	}
	assertKept(t, r, "StartPRReview")
	if err := r.ResumeBuilding(fixedNow); err != nil {
		t.Fatal(err)
	}
	assertKept(t, r, "ResumeBuilding")
	if err := r.Halt("boom", fixedNow); err != nil {
		t.Fatal(err)
	}
	assertKept(t, r, "Halt")
	if err := r.Retry("op", "", fixedNow); err != nil {
		t.Fatal(err)
	}
	assertKept(t, r, "Retry")
	if err := r.Quarantine("gate failed", fixedNow); err != nil {
		t.Fatal(err)
	}
	if err := r.Retry("op", "", fixedNow); err != nil {
		t.Fatal(err)
	}
	assertKept(t, r, "Quarantine+Retry")
}
