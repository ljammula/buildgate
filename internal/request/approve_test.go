package request

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// wellFormedApprovableTicket is a plan ticket that passes both
// ValidateTicketSpecContent checks (ValidateTicketPlan's ## Goal/## Plan/
// ## Out of scope prose skeleton and policy.TicketStructureBrownfield's
// Verify-Command:/Allowed-Files: header shape) --
// the shape Approve's own plan_review branch now requires of every ticket
// it approves (found live 2026-09-25). Used wherever a test fixture needs
// a ticket Approve will actually accept, not just a placeholder byte
// string.
const wellFormedApprovableTicket = "Verify-Command: true\nAllowed-Files: a.go\n\n## Goal\n\ng\n\n## Plan\n\n### Files to touch\n\n- a.go\n\n### Steps\n\n1. s\n\n### Tests to add\n\n- t\n\n### Acceptance criteria covered\n\n- 1\n\n## Out of scope\n\nnone\n"

// newApprovableRequest writes a fully-formed request directory (request.md,
// request.json, spec.md, and -- when withTickets is true -- two ticket
// spec files) so Approve/Reject have real files to hash against.
func newApprovableRequest(t *testing.T, dataDir, id string, state State, withTickets bool) *Request {
	t.Helper()
	if err := SaveText(dataDir, id, "the original request text"); err != nil {
		t.Fatalf("SaveText: %v", err)
	}
	r := New(id, "/repos/app", "app", Source{Kind: SourceText}, fixedNow)
	r.State = state
	if err := os.WriteFile(filepath.Join(Dir(dataDir, id), specFileName), []byte("# Spec\n"), 0o600); err != nil {
		t.Fatalf("write spec.md: %v", err)
	}
	if withTickets {
		if err := os.MkdirAll(filepath.Join(Dir(dataDir, id), "tickets"), 0o750); err != nil {
			t.Fatalf("mkdir tickets: %v", err)
		}
		for _, name := range []string{"001.spec.md", "002.spec.md"} {
			if err := os.WriteFile(filepath.Join(Dir(dataDir, id), "tickets", name), []byte(wellFormedApprovableTicket), 0o600); err != nil {
				t.Fatalf("write %s: %v", name, err)
			}
		}
	}
	if err := r.Save(dataDir); err != nil {
		t.Fatalf("Save: %v", err)
	}
	return r
}

// TestApproveFromEachState covers Approve called from every state: only
// spec_review and plan_review succeed (recording approved_by/at/sha256
// and moving state); every other state is refused, naming the current
// state.
func TestApproveFromEachState(t *testing.T) {
	allStates := []State{
		StateSubmitted, StateSpecDrafting, StateSpecReview,
		StatePlanning, StatePlanReview, StateBuilding, StatePRReview,
		StateDone, StateClosed, StateQuarantined, StateHalted, StateCancelled,
	}
	for _, state := range allStates {
		t.Run(string(state), func(t *testing.T) {
			dataDir := t.TempDir()
			newApprovableRequest(t, dataDir, "req-1", state, true)

			r, err := Approve(dataDir, "req-1", "alice", fixedNow, nil)
			switch state {
			case StateSpecReview:
				if err != nil {
					t.Fatalf("Approve from spec_review: %v", err)
				}
				if r.State != StatePlanning {
					t.Errorf("State = %q, want %q", r.State, StatePlanning)
				}
				if r.ApprovedBy != "alice" {
					t.Errorf("ApprovedBy = %q, want %q", r.ApprovedBy, "alice")
				}
				if _, ok := r.ApprovedSHA256[specFileName]; !ok {
					t.Errorf("ApprovedSHA256 missing %q: %+v", specFileName, r.ApprovedSHA256)
				}
			case StatePlanReview:
				if err != nil {
					t.Fatalf("Approve from plan_review: %v", err)
				}
				if r.State != StateBuilding {
					t.Errorf("State = %q, want %q", r.State, StateBuilding)
				}
				for _, name := range []string{"tickets/001.spec.md", "tickets/002.spec.md"} {
					if _, ok := r.ApprovedSHA256[name]; !ok {
						t.Errorf("ApprovedSHA256 missing %q: %+v", name, r.ApprovedSHA256)
					}
				}
			default:
				if err == nil {
					t.Fatalf("Approve from %q: want an error, got nil", state)
				}
				if !strings.Contains(err.Error(), string(state)) {
					t.Errorf("Approve from %q: error %q does not name the current state", state, err.Error())
				}
			}
		})
	}
}

// TestRejectFromEachState covers Reject called from every state: only
// spec_review and plan_review succeed (moving state back, recording the
// rejection on Rejections/revisions, and leaving request.md itself
// untouched -- confirmed by an adversarial review, 2026-09-24); every
// other state is refused.
func TestRejectFromEachState(t *testing.T) {
	allStates := []State{
		StateSubmitted, StateSpecDrafting, StateSpecReview,
		StatePlanning, StatePlanReview, StateBuilding, StatePRReview,
		StateDone, StateClosed, StateQuarantined, StateHalted, StateCancelled,
	}
	for _, state := range allStates {
		t.Run(string(state), func(t *testing.T) {
			dataDir := t.TempDir()
			newApprovableRequest(t, dataDir, "req-1", state, true)

			r, err := RejectAnchored(dataDir, "req-1", "bob", "please tighten the scope", nil, fixedNow)
			switch state {
			case StateSpecReview:
				if err != nil {
					t.Fatalf("Reject from spec_review: %v", err)
				}
				if r.State != StateSpecDrafting {
					t.Errorf("State = %q, want %q", r.State, StateSpecDrafting)
				}
			case StatePlanReview:
				if err != nil {
					t.Fatalf("Reject from plan_review: %v", err)
				}
				if r.State != StatePlanning {
					t.Errorf("State = %q, want %q", r.State, StatePlanning)
				}
			default:
				if err == nil {
					t.Fatalf("Reject from %q: want an error, got nil", state)
				}
				if !strings.Contains(err.Error(), string(state)) {
					t.Errorf("Reject from %q: error %q does not name the current state", state, err.Error())
				}
				return
			}

			// An adversarial review (2026-09-24) confirmed request.md is left
			// completely untouched by a rejection at any stage -- draft_spec.py
			// and plan_tickets.py both fold its entire content into their own
			// prompt verbatim, unframed and with no stage scoping, so a note
			// appended here for one stage would leak into another stage's
			// prompt the moment the request advanced past it. The rejection
			// reason reaches the next drafting pass through its own
			// stage-scoped feedback file instead (SpecFeedback/PlanFeedback,
			// checked below via r.Rejections).
			b, readErr := os.ReadFile(TextPath(dataDir, "req-1"))
			if readErr != nil {
				t.Fatalf("read request.md: %v", readErr)
			}
			text := string(b)
			if strings.Contains(text, "## Rejected") {
				t.Errorf("request.md contains a rejection note, want it left untouched: %q", text)
			}
			if text != "the original request text" {
				t.Errorf("request.md = %q, want it unchanged by the rejection", text)
			}

			// The structured Rejections entry must carry the same
			// by/reason as the request.md note, plus the state this
			// rejection sent the request back from.
			if len(r.Rejections) != 1 {
				t.Fatalf("Rejections = %+v, want exactly one entry", r.Rejections)
			}
			got := r.Rejections[0]
			if got.By != "bob" || got.Reason != "please tighten the scope" || got.FromState != state {
				t.Errorf("Rejections[0] = %+v, want by=bob reason=%q from_state=%q", got, "please tighten the scope", state)
			}
			if got.At == "" {
				t.Error("Rejections[0].At is empty")
			}

			// A rejection also snapshots the file(s) it applies to into
			// revisions/1/ (2.2).
			revisions, err := ListRevisions(dataDir, "req-1")
			if err != nil {
				t.Fatalf("ListRevisions: %v", err)
			}
			if len(revisions) != 1 || revisions[0].Index != 1 {
				t.Fatalf("ListRevisions = %+v, want one revision indexed 1", revisions)
			}
			if revisions[0].By != "bob" || revisions[0].Reason != "please tighten the scope" || revisions[0].FromState != state {
				t.Errorf("revision meta = %+v, want by=bob reason=%q from_state=%q", revisions[0], "please tighten the scope", state)
			}
			wantFile := specFileName
			if state == StatePlanReview {
				wantFile = filepath.Join("tickets", "001.spec.md")
			}
			if len(revisions[0].Files) == 0 || revisions[0].Files[0] != wantFile {
				t.Errorf("revision Files = %v, want it to include %q", revisions[0].Files, wantFile)
			}
			_, contents, err := LoadRevision(dataDir, "req-1", 1)
			if err != nil {
				t.Fatalf("LoadRevision: %v", err)
			}
			if contents[wantFile] == "" {
				t.Errorf("LoadRevision contents missing %q: %+v", wantFile, contents)
			}
		})
	}
}

// TestRejectFromPlanReviewWithoutTicketsDirStillSucceeds is a regression
// test: Reject from plan_review must not adopt Approve's own
// ticketSpecRelPaths precondition (missing/empty tickets/*.spec.md is a
// hard error there). A request stuck in plan_review because its plan step
// left the tickets directory missing or corrupted must still be
// rejectable -- that's the operator's only way to unstick it and send it
// back to planning. Found in review: an earlier version of this function
// called ticketSpecRelPaths unconditionally and propagated its error,
// which would have refused this exact Reject call.
func TestRejectFromPlanReviewWithoutTicketsDirStillSucceeds(t *testing.T) {
	dataDir := t.TempDir()
	newApprovableRequest(t, dataDir, "req-1", StatePlanReview, false)

	r, err := RejectAnchored(dataDir, "req-1", "bob", "plan is missing tickets", nil, fixedNow)
	if err != nil {
		t.Fatalf("Reject from plan_review with no tickets dir: %v", err)
	}
	if r.State != StatePlanning {
		t.Errorf("State = %q, want %q", r.State, StatePlanning)
	}
	if len(r.Rejections) != 1 || r.Rejections[0].FromState != StatePlanReview {
		t.Errorf("Rejections = %+v, want one entry with from_state=%q", r.Rejections, StatePlanReview)
	}
	// No ticket spec files existed to snapshot, so the revision this
	// rejection recorded legitimately has no files -- that's fine, the
	// point is Reject itself did not fail.
	revisions, err := ListRevisions(dataDir, "req-1")
	if err != nil {
		t.Fatalf("ListRevisions: %v", err)
	}
	if len(revisions) != 1 {
		t.Fatalf("ListRevisions = %+v, want exactly one revision", revisions)
	}
}

// TestRejectTwiceAppendsRejectionsAndRevisions covers a request rejected
// more than once: Rejections accumulates rather than being replaced, and
// each rejection gets its own, separately numbered revision.

// TestSnapshotRevisionIsIdempotentForARetry covers SnapshotRevision's own
// retry-safety: Reject calls this before its own durable r.Save, so a
// crash or error between them means a retried Reject call re-runs
// SnapshotRevision too. Calling it twice
// with the exact same (by, reason, fromState) must publish only one
// revision, reusing the first call's index, not create a second,
// identical one.
func TestSnapshotRevisionIsIdempotentForARetry(t *testing.T) {
	dataDir := t.TempDir()
	newApprovableRequest(t, dataDir, "req-1", StateSpecReview, false)

	n1, err := SnapshotRevision(dataDir, "req-1", "bob", "too broad", StateSpecReview, []string{specFileName}, fixedNow)
	if err != nil {
		t.Fatalf("first SnapshotRevision: %v", err)
	}
	n2, err := SnapshotRevision(dataDir, "req-1", "bob", "too broad", StateSpecReview, []string{specFileName}, fixedNow)
	if err != nil {
		t.Fatalf("second SnapshotRevision: %v", err)
	}
	if n1 != n2 {
		t.Errorf("indexes = %d, %d, want the same index reused", n1, n2)
	}
	revisions, err := ListRevisions(dataDir, "req-1")
	if err != nil {
		t.Fatalf("ListRevisions: %v", err)
	}
	if len(revisions) != 1 {
		t.Fatalf("ListRevisions = %+v, want exactly one revision", revisions)
	}

	// A genuinely different rejection (different reason) must still get
	// its own new revision, not be deduped against the earlier one.
	n3, err := SnapshotRevision(dataDir, "req-1", "bob", "now also too vague", StateSpecReview, []string{specFileName}, fixedNow)
	if err != nil {
		t.Fatalf("third SnapshotRevision: %v", err)
	}
	if n3 == n1 {
		t.Errorf("third SnapshotRevision reused index %d, want a new one", n3)
	}
	revisions, err = ListRevisions(dataDir, "req-1")
	if err != nil {
		t.Fatalf("ListRevisions: %v", err)
	}
	if len(revisions) != 2 {
		t.Fatalf("ListRevisions = %+v, want exactly two revisions", revisions)
	}
}

// TestRejectTwiceWithSameReasonStillGetsTwoRevisions is a regression test:
// two GENUINE, separately-completed Reject calls that happen to share
// the same (by, reason, from_state) must each get their own revision --
// SnapshotRevision's retry-dedup check must not mistake "an operator gave
// the same feedback twice" for "this is a crash-retry of the first Reject
// call". The distinguishing signal is durability: by the time the second
// Reject's SnapshotRevision
// call runs, the first rejection is already committed to
// Request.Rejections on disk, so there's no "orphan" revision to reuse.
func TestRejectTwiceWithSameReasonStillGetsTwoRevisions(t *testing.T) {
	dataDir := t.TempDir()
	newApprovableRequest(t, dataDir, "req-1", StateSpecReview, false)

	if _, err := RejectAnchored(dataDir, "req-1", "bob", "too broad", nil, fixedNow); err != nil {
		t.Fatalf("first Reject: %v", err)
	}
	r, err := Load(dataDir, "req-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := r.CompleteSpecDrafting(fixedNow); err != nil {
		t.Fatalf("CompleteSpecDrafting: %v", err)
	}
	if err := r.Save(dataDir); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := RejectAnchored(dataDir, "req-1", "bob", "too broad", nil, fixedNow)
	if err != nil {
		t.Fatalf("second Reject (same reason): %v", err)
	}
	if len(got.Rejections) != 2 {
		t.Fatalf("Rejections = %+v, want two entries", got.Rejections)
	}

	revisions, err := ListRevisions(dataDir, "req-1")
	if err != nil {
		t.Fatalf("ListRevisions: %v", err)
	}
	if len(revisions) != 2 || revisions[0].Index == revisions[1].Index {
		t.Fatalf("ListRevisions = %+v, want two distinct revisions, not one reused", revisions)
	}
}

func TestRejectTwiceAppendsRejectionsAndRevisions(t *testing.T) {
	dataDir := t.TempDir()
	newApprovableRequest(t, dataDir, "req-1", StateSpecReview, false)

	if _, err := RejectAnchored(dataDir, "req-1", "bob", "first pass, too broad", nil, fixedNow); err != nil {
		t.Fatalf("first Reject: %v", err)
	}
	// Reject moved the request to spec_drafting; move it back to
	// spec_review so a second Reject is legal.
	r, err := Load(dataDir, "req-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := r.CompleteSpecDrafting(fixedNow); err != nil {
		t.Fatalf("CompleteSpecDrafting: %v", err)
	}
	if err := r.Save(dataDir); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := RejectAnchored(dataDir, "req-1", "carol", "second pass, still too broad", nil, fixedNow)
	if err != nil {
		t.Fatalf("second Reject: %v", err)
	}
	if len(got.Rejections) != 2 {
		t.Fatalf("Rejections = %+v, want two entries", got.Rejections)
	}
	if got.Rejections[0].By != "bob" || got.Rejections[1].By != "carol" {
		t.Errorf("Rejections = %+v, want bob then carol", got.Rejections)
	}

	revisions, err := ListRevisions(dataDir, "req-1")
	if err != nil {
		t.Fatalf("ListRevisions: %v", err)
	}
	if len(revisions) != 2 || revisions[0].Index != 1 || revisions[1].Index != 2 {
		t.Fatalf("ListRevisions = %+v, want revisions indexed 1 and 2", revisions)
	}
}

func TestRejectWithoutReasonIsRefused(t *testing.T) {
	dataDir := t.TempDir()
	newApprovableRequest(t, dataDir, "req-1", StateSpecReview, false)
	if _, err := RejectAnchored(dataDir, "req-1", "bob", "", nil, fixedNow); err == nil {
		t.Fatal("Reject with empty reason: want an error, got nil")
	}
}

// TestApproveRefusesWhenExpectedSHA256DoesNotMatchCurrentFile is a
// regression test for binding approval to the artifact actually shown:
// an operator's own confirm sheet is built from a GET /requests/{id}
// that returned spec.md's content at some hash H1.
// If spec.md is edited after that fetch but before the approve POST
// arrives, the request must refuse (naming ErrApprovalStale) rather
// than silently approving whatever spec.md now says -- content the
// operator never saw. This is a different case from
// TestApprovePlanRefusesWhenApprovedSpecHasChanged below: that one
// catches an edit after a PRIOR approval already recorded a hash: this
// one catches an edit racing the CURRENT approval, with no completed
// approval involved at all.
func TestApproveRefusesWhenExpectedSHA256DoesNotMatchCurrentFile(t *testing.T) {
	dataDir := t.TempDir()
	newApprovableRequest(t, dataDir, "req-1", StateSpecReview, false)

	// Edited after the operator's own fetch, before this call.
	if err := os.WriteFile(SpecPath(dataDir, "req-1"), []byte("# Edited after fetch\n"), 0o600); err != nil {
		t.Fatalf("edit spec.md: %v", err)
	}

	_, err := Approve(dataDir, "req-1", "alice", fixedNow, map[string]string{
		specFileName: "0000000000000000000000000000000000000000000000000000000000000000",
	})
	if err == nil {
		t.Fatal("Approve with a stale expected hash: want an error, got nil")
	}
	if !errors.Is(err, ErrApprovalStale) {
		t.Errorf("error = %v, want it to wrap ErrApprovalStale", err)
	}

	r, loadErr := Load(dataDir, "req-1")
	if loadErr != nil {
		t.Fatalf("Load: %v", loadErr)
	}
	if r.State != StateSpecReview {
		t.Errorf("State = %q, want the request untouched at %q after a refused approval", r.State, StateSpecReview)
	}
}

// TestApproveSucceedsWhenExpectedSHA256Matches confirms the happy path
// isn't broken by the check above: a caller that names the file's actual
// current hash is not treated as stale.
func TestApproveSucceedsWhenExpectedSHA256Matches(t *testing.T) {
	dataDir := t.TempDir()
	newApprovableRequest(t, dataDir, "req-1", StateSpecReview, false)

	hash, err := HashFile(dataDir, "req-1", specFileName)
	if err != nil {
		t.Fatalf("HashFile: %v", err)
	}

	r, err := Approve(dataDir, "req-1", "alice", fixedNow, map[string]string{
		specFileName: hash,
	})
	if err != nil {
		t.Fatalf("Approve with a matching expected hash: %v", err)
	}
	if r.State != StatePlanning {
		t.Errorf("State = %q, want %q", r.State, StatePlanning)
	}
}

// TestApproveRefusesStaleExpectedSHA256AcrossAStateChange is a
// regression test: a client that fetched the request during
// spec_review, and so built an expectedSHA256 shaped like {"spec.md":
// H}, must not be able to use that same stale map to approve the
// request after it has since moved
// to plan_review (another operator/process approved the spec in
// between). The old partial check found no key in common between the
// stale map and plan_review's own relPaths ({"tickets/*.spec.md": ...})
// and treated that as "nothing to disagree about" -- silently approving
// every ticket plan the original operator never even looked at.
func TestApproveRefusesStaleExpectedSHA256AcrossAStateChange(t *testing.T) {
	dataDir := t.TempDir()
	// Already in plan_review by the time this call arrives -- as if
	// another actor approved the spec after this caller's own stale
	// fetch.
	newApprovableRequest(t, dataDir, "req-1", StatePlanReview, true)

	specHash, err := HashFile(dataDir, "req-1", specFileName)
	if err != nil {
		t.Fatalf("HashFile: %v", err)
	}

	_, err = Approve(dataDir, "req-1", "alice", fixedNow, map[string]string{
		specFileName: specHash,
	})
	if err == nil {
		t.Fatal("Approve with a spec_review-shaped expected hash against a plan_review request: want an error, got nil")
	}
	if !errors.Is(err, ErrApprovalStale) {
		t.Errorf("error = %v, want it to wrap ErrApprovalStale", err)
	}

	r, loadErr := Load(dataDir, "req-1")
	if loadErr != nil {
		t.Fatalf("Load: %v", loadErr)
	}
	if r.State != StatePlanReview {
		t.Errorf("State = %q, want the request untouched at %q after a refused approval", r.State, StatePlanReview)
	}
}

// TestApprovePlanRefusesWhenApprovedSpecHasChanged is the approve/reject
// flow's own hash-mismatch requirement: approving the plan re-verifies
// the spec's own already-recorded hash first, and an edit to spec.md
// after its approval must refuse the plan approval too, naming
// spec.md.
func TestApprovePlanRefusesWhenApprovedSpecHasChanged(t *testing.T) {
	dataDir := t.TempDir()
	newApprovableRequest(t, dataDir, "req-1", StateSpecReview, true)

	if _, err := Approve(dataDir, "req-1", "alice", fixedNow, nil); err != nil {
		t.Fatalf("Approve spec: %v", err)
	}
	if err := os.WriteFile(filepath.Join(Dir(dataDir, "req-1"), specFileName), []byte("# Spec (edited after approval)\n"), 0o600); err != nil {
		t.Fatalf("edit spec.md: %v", err)
	}
	r, err := Load(dataDir, "req-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	r.State = StatePlanReview // simulate planning having completed
	if err := r.Save(dataDir); err != nil {
		t.Fatalf("Save: %v", err)
	}

	_, err = Approve(dataDir, "req-1", "alice", fixedNow, nil)
	if err == nil {
		t.Fatal("Approve plan after spec.md changed: want an error, got nil")
	}
	if !strings.Contains(err.Error(), specFileName) {
		t.Errorf("error %q does not name %q", err.Error(), specFileName)
	}
}

// TestVerifyApprovedHashesCatchesEditAfterApproval is the direct
// hash-mismatch test for the function the request driver calls before
// advancing a request out of planning or building on the strength of an
// earlier approval (see cmd/factoryd's own verifyApprovedHashes).
func TestVerifyApprovedHashesCatchesEditAfterApproval(t *testing.T) {
	dataDir := t.TempDir()
	newApprovableRequest(t, dataDir, "req-1", StateSpecReview, false)

	r, err := Approve(dataDir, "req-1", "alice", fixedNow, nil)
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if err := VerifyApprovedHashes(dataDir, r); err != nil {
		t.Fatalf("VerifyApprovedHashes before any edit: %v", err)
	}

	if err := os.WriteFile(filepath.Join(Dir(dataDir, "req-1"), specFileName), []byte("# Spec (edited)\n"), 0o600); err != nil {
		t.Fatalf("edit spec.md: %v", err)
	}
	err = VerifyApprovedHashes(dataDir, r)
	if err == nil {
		t.Fatal("VerifyApprovedHashes after edit: want an error, got nil")
	}
	if !strings.Contains(err.Error(), specFileName) {
		t.Errorf("error %q does not name %q", err.Error(), specFileName)
	}
}

func TestVerifyApprovedHashesPassesWithNoRecordedHashes(t *testing.T) {
	dataDir := t.TempDir()
	r := New("req-1", "/repos/app", "app", Source{Kind: SourceText}, fixedNow)
	if err := VerifyApprovedHashes(dataDir, r); err != nil {
		t.Errorf("VerifyApprovedHashes with no recorded hashes: %v", err)
	}
}

func TestApprovePlanRefusesWithoutTicketFiles(t *testing.T) {
	dataDir := t.TempDir()
	newApprovableRequest(t, dataDir, "req-1", StatePlanReview, false)
	if _, err := Approve(dataDir, "req-1", "alice", fixedNow, nil); err == nil {
		t.Fatal("Approve plan with no tickets directory: want an error, got nil")
	}
}

// TestApprovePlanRefusesHandEditedTicketMissingVerifyCommand is the
// regression test for the live gap found 2026-09-25: the console's own
// ticket editor (PUT /requests/{id}/tickets/{n}, internal/api/server.go)
// validates every edit with request.ValidateTicketSpecContent, but an
// operator who edits tickets/NNN.spec.md directly on disk (bypassing the
// console) and then runs `factoryd approve` got no validation at all --
// the malformed edit only failed much later, at build start, via
// -request-ticket's own fail-closed preflight. Approve's own plan_review
// branch must now refuse it before any state change, naming the reason.
// The factory's spec ticket is the approved spec byte for byte; a spec handed
// over in its own shape is approved as that ticket, and the same content in a
// ticket that is not the spec is refused.
func TestApprovePlanAcceptsTheSpecTicketOfAHandedOverSpec(t *testing.T) {
	const spec = "# Spec\n\n## Problem\n\np\n\n## Wanted behaviour\n\n- one\n- two\n"
	for _, tc := range []struct {
		name, ticket string
		wantErr      bool
	}{
		{"the spec itself", spec, false},
		{"not the spec", spec + "\n- three\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dataDir := t.TempDir()
			newApprovableRequest(t, dataDir, "req-1", StatePlanReview, true)
			dir := Dir(dataDir, "req-1")
			if err := os.WriteFile(filepath.Join(dir, specFileName), []byte(spec), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(filepath.Join(dir, "tickets", "002.spec.md")); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "tickets", "001.spec.md"), []byte(tc.ticket), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := Approve(dataDir, "req-1", "alice", fixedNow, nil)
			if (err != nil) != tc.wantErr {
				t.Fatalf("Approve: %v, want error %v", err, tc.wantErr)
			}
		})
	}
}

func TestApprovePlanRefusesHandEditedTicketMissingVerifyCommand(t *testing.T) {
	dataDir := t.TempDir()
	newApprovableRequest(t, dataDir, "req-1", StatePlanReview, true)
	// Simulate a hand-edit that drops the required Verify-Command: header
	// -- everything else about wellFormedApprovableTicket stays intact.
	handEdited := strings.Replace(wellFormedApprovableTicket, "Verify-Command: true\n", "", 1)
	ticketPath := filepath.Join(Dir(dataDir, "req-1"), "tickets", "001.spec.md")
	if err := os.WriteFile(ticketPath, []byte(handEdited), 0o600); err != nil {
		t.Fatal(err)
	}

	before, err := Load(dataDir, "req-1")
	if err != nil {
		t.Fatal(err)
	}

	_, err = Approve(dataDir, "req-1", "alice", fixedNow, nil)
	if err == nil {
		t.Fatal("Approve plan with a hand-edited ticket missing Verify-Command: succeeded, want it refused")
	}
	if !strings.Contains(err.Error(), "Verify-Command:") {
		t.Errorf("err = %v, want it to name the missing Verify-Command: header", err)
	}
	if !strings.Contains(err.Error(), "001.spec.md") {
		t.Errorf("err = %v, want it to name the offending ticket file", err)
	}

	after, err := Load(dataDir, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	if after.State != before.State {
		t.Errorf("State = %q after a refused approval, want unchanged %q", after.State, before.State)
	}
	if len(after.ApprovedSHA256) != 0 {
		t.Errorf("ApprovedSHA256 = %+v after a refused approval, want empty (no partial pin)", after.ApprovedSHA256)
	}
}

// TestApprovePlanAcceptsUntouchedPlannerShapedTicket proves the new
// plan_review validation is not a regression against the ordinary path: an
// untouched, planner-drafted ticket (the exact shape
// writeAndValidateDraftedTickets already writes, mirrored here by
// wellFormedApprovableTicket) still approves cleanly.
func TestApprovePlanAcceptsUntouchedPlannerShapedTicket(t *testing.T) {
	dataDir := t.TempDir()
	newApprovableRequest(t, dataDir, "req-1", StatePlanReview, true)

	r, err := Approve(dataDir, "req-1", "alice", fixedNow, nil)
	if err != nil {
		t.Fatalf("Approve an untouched planner-shaped ticket: %v", err)
	}
	if r.State != StateBuilding {
		t.Errorf("State = %q, want %q", r.State, StateBuilding)
	}
	if _, ok := r.ApprovedSHA256["tickets/001.spec.md"]; !ok {
		t.Errorf("ApprovedSHA256 missing tickets/001.spec.md: %+v", r.ApprovedSHA256)
	}
}

// TestApproveClearsReminderState is the HITL reminder's own dedupe
// requirement: once a request leaves a review state via Approve, its
// reminder bookkeeping (WaitingSince/LastNotifiedAt/NotifyCount) must
// be reset, so worker's reminder ticker has nothing stale left to
// match against even before it notices the state itself has changed.
func TestApproveClearsReminderState(t *testing.T) {
	dataDir := t.TempDir()
	r := newApprovableRequest(t, dataDir, "req-1", StateSpecReview, true)
	r.WaitingSince = "2026-09-11T10:00:00Z"
	r.LastNotifiedAt = "2026-09-11T10:15:00Z"
	r.NotifyCount = 2
	if err := r.Save(dataDir); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := Approve(dataDir, "req-1", "alice", fixedNow, nil)
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if got.WaitingSince != "" || got.LastNotifiedAt != "" || got.NotifyCount != 0 {
		t.Errorf("reminder state after Approve = (%q, %q, %d), want all cleared", got.WaitingSince, got.LastNotifiedAt, got.NotifyCount)
	}
}

// TestRejectClearsReminderState mirrors TestApproveClearsReminderState for
// Reject.
func TestRejectClearsReminderState(t *testing.T) {
	dataDir := t.TempDir()
	r := newApprovableRequest(t, dataDir, "req-1", StatePlanReview, true)
	r.WaitingSince = "2026-09-11T10:00:00Z"
	r.LastNotifiedAt = "2026-09-11T10:15:00Z"
	r.NotifyCount = 2
	if err := r.Save(dataDir); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := RejectAnchored(dataDir, "req-1", "bob", "needs another pass", nil, fixedNow)
	if err != nil {
		t.Fatalf("Reject: %v", err)
	}
	if got.WaitingSince != "" || got.LastNotifiedAt != "" || got.NotifyCount != 0 {
		t.Errorf("reminder state after Reject = (%q, %q, %d), want all cleared", got.WaitingSince, got.LastNotifiedAt, got.NotifyCount)
	}
}
