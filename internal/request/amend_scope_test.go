package request

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newAmendScopeFixture writes a fully-formed, quarantined, one-ticket
// request (request.md, request.json, tickets/001.spec.md with the given
// content) with that ticket spec's ApprovedSHA256 pin already recorded, as
// if plan_review had approved it -- the shape AmendScope requires
// (StateQuarantined, VerifyApprovedHashes passing) before it will touch
// anything.
func newAmendScopeFixture(t *testing.T, dataDir, id, ticketContent string) *Request {
	t.Helper()
	if err := SaveText(dataDir, id, "the original request text"); err != nil {
		t.Fatalf("SaveText: %v", err)
	}
	r := New(id, "/repos/app", "app", Source{Kind: SourceText}, fixedNow)
	if err := os.MkdirAll(filepath.Join(Dir(dataDir, id), "tickets"), 0o750); err != nil {
		t.Fatalf("mkdir tickets: %v", err)
	}
	specPath := filepath.Join(Dir(dataDir, id), "tickets", "001.spec.md")
	if err := os.WriteFile(specPath, []byte(ticketContent), 0o600); err != nil {
		t.Fatalf("write ticket spec: %v", err)
	}
	hash, err := HashFile(dataDir, id, "tickets/001.spec.md")
	if err != nil {
		t.Fatalf("HashFile: %v", err)
	}
	r.State = StateQuarantined
	r.QuarantineCheck = QuarantineCheckDiffScope
	r.TicketCount = 1
	r.TicketIndex = 1
	r.Tickets = []Ticket{{Index: 1, SpecPath: specPath}}
	r.ApprovedSHA256 = map[string]string{"tickets/001.spec.md": hash}
	if err := r.Save(dataDir); err != nil {
		t.Fatalf("Save: %v", err)
	}
	return r
}

func readTicketSpec(t *testing.T, dataDir, id string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(Dir(dataDir, id), "tickets", "001.spec.md"))
	if err != nil {
		t.Fatalf("read ticket spec: %v", err)
	}
	return string(b)
}

// TestAmendScopeHappyPath covers the successful widen: the Allowed-Files:
// line is rewritten to append the new file, every other line is byte-
// identical, the ticket spec's ApprovedSHA256 entry is recomputed and
// VerifyApprovedHashes still passes, a History entry is appended, and the
// request stays quarantined.
func TestAmendScopeHappyPath(t *testing.T) {
	dataDir := t.TempDir()
	newAmendScopeFixture(t, dataDir, "req-1", wellFormedApprovableTicket)
	origHash, err := HashFile(dataDir, "req-1", "tickets/001.spec.md")
	if err != nil {
		t.Fatal(err)
	}

	got, err := AmendScope(dataDir, "req-1", "alice", "route inventory test needs updating too", []string{"b_test.go"}, fixedNow.Add(time.Minute))
	if err != nil {
		t.Fatalf("AmendScope: %v", err)
	}
	if got.State != StateQuarantined {
		t.Errorf("State = %q, want unchanged %q", got.State, StateQuarantined)
	}

	wantLines := strings.Split(wellFormedApprovableTicket, "\n")
	wantLines[1] = "Allowed-Files: a.go, b_test.go"
	want := strings.Join(wantLines, "\n")
	if got := readTicketSpec(t, dataDir, "req-1"); got != want {
		t.Errorf("ticket spec content =\n%q\nwant\n%q", got, want)
	}

	newHash, err := HashFile(dataDir, "req-1", "tickets/001.spec.md")
	if err != nil {
		t.Fatal(err)
	}
	if newHash == origHash {
		t.Error("ticket spec hash did not change")
	}
	if got.ApprovedSHA256["tickets/001.spec.md"] != newHash {
		t.Errorf("ApprovedSHA256[tickets/001.spec.md] = %q, want %q", got.ApprovedSHA256["tickets/001.spec.md"], newHash)
	}
	if err := VerifyApprovedHashes(dataDir, got); err != nil {
		t.Errorf("VerifyApprovedHashes after amend: %v", err)
	}

	last := got.History[len(got.History)-1]
	if last.By != "alice" {
		t.Errorf("last History entry By = %q, want %q", last.By, "alice")
	}
	wantReason := `scope amended by alice: ticket 1 Allowed-Files += b_test.go (reason: route inventory test needs updating too)`
	if last.Reason != wantReason {
		t.Errorf("last History entry Reason = %q, want %q", last.Reason, wantReason)
	}
	if last.From != StateQuarantined || last.To != StateQuarantined {
		t.Errorf("last History entry From/To = %q/%q, want %q/%q", last.From, last.To, StateQuarantined, StateQuarantined)
	}
}

// TestAmendScopeMultipleFiles covers widening with more than one file in a
// single call: both are appended, in order, to the one Allowed-Files: line.
func TestAmendScopeMultipleFiles(t *testing.T) {
	dataDir := t.TempDir()
	newAmendScopeFixture(t, dataDir, "req-1", wellFormedApprovableTicket)

	got, err := AmendScope(dataDir, "req-1", "alice", "widen for two more files", []string{"b_test.go", "c_test.go"}, fixedNow.Add(time.Minute))
	if err != nil {
		t.Fatalf("AmendScope: %v", err)
	}
	wantLines := strings.Split(wellFormedApprovableTicket, "\n")
	wantLines[1] = "Allowed-Files: a.go, b_test.go, c_test.go"
	want := strings.Join(wantLines, "\n")
	if got := readTicketSpec(t, dataDir, "req-1"); got != want {
		t.Errorf("ticket spec content =\n%q\nwant\n%q", got, want)
	}
	_ = got
}

// TestAmendScopeRefusesNotQuarantined covers every other state: AmendScope
// must refuse, wrapping ErrIllegalTransition (the same sentinel Approve/
// Reject/Retry/SendBack's own wrong-state refusals wrap), and leave the
// ticket spec untouched.
func TestAmendScopeRefusesNotQuarantined(t *testing.T) {
	dataDir := t.TempDir()
	r := newAmendScopeFixture(t, dataDir, "req-1", wellFormedApprovableTicket)
	r.State = StateBuilding
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	before := readTicketSpec(t, dataDir, "req-1")

	_, err := AmendScope(dataDir, "req-1", "alice", "reason", []string{"b_test.go"}, fixedNow.Add(time.Minute))
	if err == nil {
		t.Fatal("AmendScope: want an error, got nil")
	}
	if !errors.Is(err, ErrIllegalTransition) {
		t.Errorf("AmendScope error %v, want it to wrap ErrIllegalTransition", err)
	}
	if after := readTicketSpec(t, dataDir, "req-1"); after != before {
		t.Error("ticket spec was modified despite the refusal")
	}
}

// TestAmendScopeRefusesTicketWithPR covers the ticket-already-has-a-PR
// refusal: amend-scope only widens scope before a ticket's build has
// reached a pull request.
func TestAmendScopeRefusesTicketWithPR(t *testing.T) {
	dataDir := t.TempDir()
	r := newAmendScopeFixture(t, dataDir, "req-1", wellFormedApprovableTicket)
	r.Tickets[0].PRURL = "https://github.com/acme/widgets/pull/9"
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}

	_, err := AmendScope(dataDir, "req-1", "alice", "reason", []string{"b_test.go"}, fixedNow.Add(time.Minute))
	if err == nil {
		t.Fatal("AmendScope: want an error, got nil")
	}
	if !errors.Is(err, ErrIllegalTransition) {
		t.Errorf("AmendScope error %v, want it to wrap ErrIllegalTransition", err)
	}
}

// TestAmendScopeRefusesEmptyReason covers the reason requirement --
// whitespace-only counts as empty too.
func TestAmendScopeRefusesEmptyReason(t *testing.T) {
	for _, reason := range []string{"", "   ", "\t\n"} {
		dataDir := t.TempDir()
		newAmendScopeFixture(t, dataDir, "req-1", wellFormedApprovableTicket)
		_, err := AmendScope(dataDir, "req-1", "alice", reason, []string{"b_test.go"}, fixedNow.Add(time.Minute))
		if err == nil {
			t.Fatalf("AmendScope with reason %q: want an error, got nil", reason)
		}
	}
}

// TestAmendScopeRefusesEmptyFiles covers the "at least one file" requirement.
func TestAmendScopeRefusesEmptyFiles(t *testing.T) {
	dataDir := t.TempDir()
	newAmendScopeFixture(t, dataDir, "req-1", wellFormedApprovableTicket)
	_, err := AmendScope(dataDir, "req-1", "alice", "reason", nil, fixedNow.Add(time.Minute))
	if err == nil {
		t.Fatal("AmendScope: want an error, got nil")
	}
}

// TestAmendScopeRefusesInvalidPath covers every syntactically-impossible
// path ticketspec.ValidWorkspaceRelativePath refuses -- an absolute path,
// a ".." segment, and a "." segment.
func TestAmendScopeRefusesInvalidPath(t *testing.T) {
	for _, path := range []string{"../x", "/abs", "./a.go"} {
		dataDir := t.TempDir()
		newAmendScopeFixture(t, dataDir, "req-1", wellFormedApprovableTicket)
		_, err := AmendScope(dataDir, "req-1", "alice", "reason", []string{path}, fixedNow.Add(time.Minute))
		if err == nil {
			t.Fatalf("AmendScope with file %q: want an error, got nil", path)
		}
		if errors.Is(err, ErrIllegalTransition) {
			t.Errorf("AmendScope with file %q: error wraps ErrIllegalTransition, want a plain input error", path)
		}
	}
}

// TestAmendScopeRefusesDuplicateFile covers a file repeated within the same
// call's own files list.
func TestAmendScopeRefusesDuplicateFile(t *testing.T) {
	dataDir := t.TempDir()
	newAmendScopeFixture(t, dataDir, "req-1", wellFormedApprovableTicket)
	_, err := AmendScope(dataDir, "req-1", "alice", "reason", []string{"b_test.go", "b_test.go"}, fixedNow.Add(time.Minute))
	if err == nil {
		t.Fatal("AmendScope: want an error, got nil")
	}
}

// TestAmendScopeRefusesAlreadyAllowedFile covers a file already present in
// the ticket's current Allowed-Files -- wellFormedApprovableTicket already
// declares "a.go".
func TestAmendScopeRefusesAlreadyAllowedFile(t *testing.T) {
	dataDir := t.TempDir()
	newAmendScopeFixture(t, dataDir, "req-1", wellFormedApprovableTicket)
	_, err := AmendScope(dataDir, "req-1", "alice", "reason", []string{"a.go"}, fixedNow.Add(time.Minute))
	if err == nil {
		t.Fatal("AmendScope: want an error, got nil")
	}
}

// noAllowedFilesTicket is wellFormedApprovableTicket with its Allowed-Files:
// line removed entirely.
const noAllowedFilesTicket = "Verify-Command: true\nRequired-Changed-Files: a.go\n\n## Goal\n\ng\n\n## Plan\n\n### Files to touch\n\n- a.go\n\n### Steps\n\n1. s\n\n### Tests to add\n\n- t\n\n### Acceptance criteria covered\n\n- 1\n\n## Out of scope\n\nnone\n"

// TestAmendScopeRefusesNoAllowedFilesLine covers a ticket spec.md with no
// top-level Allowed-Files: line at all -- there is no scope to widen.
func TestAmendScopeRefusesNoAllowedFilesLine(t *testing.T) {
	dataDir := t.TempDir()
	newAmendScopeFixture(t, dataDir, "req-1", noAllowedFilesTicket)
	_, err := AmendScope(dataDir, "req-1", "alice", "reason", []string{"b_test.go"}, fixedNow.Add(time.Minute))
	if err == nil {
		t.Fatal("AmendScope: want an error, got nil")
	}
}

// twoAllowedFilesTicket declares Allowed-Files: twice at top level --
// ambiguous which one diff_scope's own gate actually enforces
// (ParseAllowedFiles silently uses only the first).
const twoAllowedFilesTicket = "Verify-Command: true\nAllowed-Files: a.go\nAllowed-Files: b.go\nRequired-Changed-Files: a.go\n\n## Goal\n\ng\n\n## Plan\n\n### Files to touch\n\n- a.go\n\n### Steps\n\n1. s\n\n### Tests to add\n\n- t\n\n### Acceptance criteria covered\n\n- 1\n\n## Out of scope\n\nnone\n"

// TestAmendScopeRefusesTwoAllowedFilesLines covers a ticket spec.md
// declaring Allowed-Files: more than once at top level.
func TestAmendScopeRefusesTwoAllowedFilesLines(t *testing.T) {
	dataDir := t.TempDir()
	newAmendScopeFixture(t, dataDir, "req-1", twoAllowedFilesTicket)
	_, err := AmendScope(dataDir, "req-1", "alice", "reason", []string{"c_test.go"}, fixedNow.Add(time.Minute))
	if err == nil {
		t.Fatal("AmendScope: want an error, got nil")
	}
}

// TestAmendScopeRefusesWhenApprovedHashStale covers another approved file's
// post-approval edit: AmendScope must refuse via VerifyApprovedHashes and
// leave the ticket spec (and everything else) untouched -- amend must never
// bless some other edit riding along with it.
func TestAmendScopeRefusesWhenApprovedHashStale(t *testing.T) {
	dataDir := t.TempDir()
	r := newAmendScopeFixture(t, dataDir, "req-1", wellFormedApprovableTicket)
	// Pin a second, unrelated file's approval, then edit it after the fact --
	// exactly the tamper VerifyApprovedHashes exists to catch.
	specPath := SpecPath(dataDir, "req-1")
	if err := os.WriteFile(specPath, []byte("# Spec\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	specHash, err := HashFile(dataDir, "req-1", "spec.md")
	if err != nil {
		t.Fatal(err)
	}
	r.ApprovedSHA256["spec.md"] = specHash
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(specPath, []byte("# Spec (edited after approval)\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := readTicketSpec(t, dataDir, "req-1")
	beforeRequest, loadErr := Load(dataDir, "req-1")
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	wantHistoryLen := len(beforeRequest.History)

	_, err = AmendScope(dataDir, "req-1", "alice", "reason", []string{"b_test.go"}, fixedNow.Add(time.Minute))
	if err == nil {
		t.Fatal("AmendScope: want an error, got nil")
	}
	if after := readTicketSpec(t, dataDir, "req-1"); after != before {
		t.Error("ticket spec was modified despite the stale-hash refusal")
	}
	reloaded, loadErr := Load(dataDir, "req-1")
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if len(reloaded.History) != wantHistoryLen {
		t.Errorf("History = %+v, want unchanged (len %d)", reloaded.History, wantHistoryLen)
	}
}

// TestAmendScopeThenRetrySucceeds is the end-to-end-ish regression: after
// AmendScope widens the ticket's scope, `factoryd retry`'s own Retry still
// accepts the request (VerifyApprovedHashes over the now-current pin still
// passes, and the rewritten ticket spec is exactly what a rebuild would
// read).
func TestAmendScopeThenRetrySucceeds(t *testing.T) {
	dataDir := t.TempDir()
	newAmendScopeFixture(t, dataDir, "req-1", wellFormedApprovableTicket)

	amended, err := AmendScope(dataDir, "req-1", "alice", "route inventory test needs updating too", []string{"b_test.go"}, fixedNow.Add(time.Minute))
	if err != nil {
		t.Fatalf("AmendScope: %v", err)
	}
	if err := VerifyApprovedHashes(dataDir, amended); err != nil {
		t.Fatalf("VerifyApprovedHashes after amend: %v", err)
	}

	retried, err := Retry(dataDir, "req-1", "alice", "", fixedNow.Add(2*time.Minute), nil)
	if err != nil {
		t.Fatalf("Retry: %v", err)
	}
	if retried.State != StateBuilding {
		t.Errorf("State = %q, want %q", retried.State, StateBuilding)
	}
	if err := VerifyApprovedHashes(dataDir, retried); err != nil {
		t.Errorf("VerifyApprovedHashes after retry: %v", err)
	}
}

// TestNextActionDiffScopeNamesAmendScope covers quarantinedNextAction's own
// DiffScope branch: it must name `factoryd amend-scope` as the widen path,
// alongside `factoryd retry`.
func TestNextActionDiffScopeNamesAmendScope(t *testing.T) {
	dataDir := t.TempDir()
	r := newAmendScopeFixture(t, dataDir, "req-1", wellFormedApprovableTicket)
	next := r.NextAction()
	if !strings.Contains(next, "diff_scope quarantined") {
		t.Errorf("NextAction() = %q, want it to name diff_scope", next)
	}
	if !strings.Contains(next, "factoryd amend-scope -reason") {
		t.Errorf("NextAction() = %q, want it to name `factoryd amend-scope -reason`", next)
	}
	if !strings.Contains(next, "factoryd retry req-1") {
		t.Errorf("NextAction() = %q, want it to name `factoryd retry req-1`", next)
	}
}
