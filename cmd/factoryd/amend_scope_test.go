package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"buildgate/internal/request"
)

// amendScopeFixtureTicket is a minimal ticket spec.md with a single
// top-level Allowed-Files: line -- amendScopeMain's own success-path test
// widens it.
const amendScopeFixtureTicket = "Verify-Command: true\nAllowed-Files: a.go\nRequired-Changed-Files: a.go\n\n## Goal\n\ng\n\n## Plan\n\n### Files to touch\n\n- a.go\n\n### Steps\n\n1. s\n\n### Tests to add\n\n- t\n\n### Acceptance criteria covered\n\n- 1\n\n## Out of scope\n\nnone\n"

// newAmendScopeMainFixture writes a fully-formed, quarantined, one-ticket
// request -- request.md, request.json, tickets/001.spec.md -- with that
// ticket spec's ApprovedSHA256 pin already recorded, the shape
// amendScopeMain's own request.AmendScope call requires.
func newAmendScopeMainFixture(t *testing.T, dataDir, id string) {
	t.Helper()
	if err := request.SaveText(dataDir, id, "the original request text"); err != nil {
		t.Fatalf("SaveText: %v", err)
	}
	r := request.New(id, "/repos/app", "app", request.Source{Kind: request.SourceText}, time.Now())
	if err := os.MkdirAll(filepath.Join(request.Dir(dataDir, id), "tickets"), 0o750); err != nil {
		t.Fatalf("mkdir tickets: %v", err)
	}
	specPath := filepath.Join(request.Dir(dataDir, id), "tickets", "001.spec.md")
	if err := os.WriteFile(specPath, []byte(amendScopeFixtureTicket), 0o600); err != nil {
		t.Fatalf("write ticket spec: %v", err)
	}
	hash, err := request.HashFile(dataDir, id, "tickets/001.spec.md")
	if err != nil {
		t.Fatalf("HashFile: %v", err)
	}
	r.State = request.StateQuarantined
	r.QuarantineCheck = request.QuarantineCheckDiffScope
	r.TicketCount = 1
	r.TicketIndex = 1
	r.Tickets = []request.Ticket{{Index: 1, SpecPath: specPath}}
	r.ApprovedSHA256 = map[string]string{"tickets/001.spec.md": hash}
	if err := r.Save(dataDir); err != nil {
		t.Fatalf("Save: %v", err)
	}
}

// TestAmendScopeMainRequiresReason covers the CLI's own required -reason
// flag: refused before request.AmendScope is ever called, leaving the
// request untouched.
func TestAmendScopeMainRequiresReason(t *testing.T) {
	dp := newTestDeps(t)
	t.Parallel()
	dataDir := t.TempDir()
	newAmendScopeMainFixture(t, dataDir, "req-1")
	before, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	wantHistoryLen := len(before.History)

	err = amendScopeMain(dp, []string{"-data-dir", dataDir, "req-1", "b_test.go"})
	if err == nil || !strings.Contains(err.Error(), "-reason is required") {
		t.Fatalf("amendScopeMain err = %v, want the missing -reason refusal", err)
	}
	loaded, loadErr := request.Load(dataDir, "req-1")
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if loaded.State != request.StateQuarantined {
		t.Errorf("State = %q, want untouched %q", loaded.State, request.StateQuarantined)
	}
	if len(loaded.History) != wantHistoryLen {
		t.Errorf("History = %+v, want unchanged (len %d)", loaded.History, wantHistoryLen)
	}
}

// TestAmendScopeMainSuccess covers the success path end to end through the
// CLI entry point: the ticket spec's Allowed-Files: line is widened, and
// the CLI reports the ticket index and files.
func TestAmendScopeMainSuccess(t *testing.T) {
	dp := newTestDeps(t)
	t.Parallel()
	dataDir := t.TempDir()
	newAmendScopeMainFixture(t, dataDir, "req-1")

	if err := amendScopeMain(dp, []string{"-data-dir", dataDir, "-reason", "route inventory test needs updating too", "req-1", "b_test.go"}); err != nil {
		t.Fatalf("amendScopeMain: %v", err)
	}

	loaded, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.State != request.StateQuarantined {
		t.Errorf("State = %q, want unchanged %q", loaded.State, request.StateQuarantined)
	}
	b, err := os.ReadFile(filepath.Join(request.Dir(dataDir, "req-1"), "tickets", "001.spec.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "Allowed-Files: a.go, b_test.go") {
		t.Errorf("ticket spec content = %q, want it to contain the widened Allowed-Files line", string(b))
	}
	if err := request.VerifyApprovedHashes(dataDir, loaded); err != nil {
		t.Errorf("VerifyApprovedHashes after amend: %v", err)
	}
}
