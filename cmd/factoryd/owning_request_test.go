package main

import (
	"testing"
	"time"

	"buildgate/internal/request"
	"buildgate/internal/run"
)

// TestFindOwningRequestFastPath proves the run.Run.RequestID path: a run
// that carries its own RequestID is resolved via a single run.Load, with
// no request.json anywhere on disk for it to fall back to.
func TestFindOwningRequestFastPath(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	r := &run.Run{ID: "run1", State: run.StateReady, RequestID: "req1"}
	if err := r.Save(dataDir); err != nil {
		t.Fatalf("save run: %v", err)
	}

	if got := findOwningRequest(dataDir, "run1"); got != "req1" {
		t.Errorf("findOwningRequest() = %q, want %q", got, "req1")
	}
}

// TestFindOwningRequestScanFallback proves a run predating run.Run.RequestID
// (empty RequestID, or no run record at all) still resolves via the
// request.List directory scan over each request's own Tickets.
func TestFindOwningRequestScanFallback(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	r := request.New("req1", "workspace", "project", request.Source{}, time.Now())
	r.Tickets = []request.Ticket{{Index: 1, RunID: "run1"}}
	if err := r.Save(dataDir); err != nil {
		t.Fatalf("save request: %v", err)
	}

	// No run.json for run1 at all -- the run.Load fast path must fail
	// closed (not error out) and fall back to the scan.
	if got := findOwningRequest(dataDir, "run1"); got != "req1" {
		t.Errorf("findOwningRequest() with no run record = %q, want %q", got, "req1")
	}

	// A run record that exists but predates RequestID (empty) must also
	// fall back rather than stopping at the fast path's "" result.
	old := &run.Run{ID: "run1", State: run.StateReady}
	if err := old.Save(dataDir); err != nil {
		t.Fatalf("save legacy run: %v", err)
	}
	if got := findOwningRequest(dataDir, "run1"); got != "req1" {
		t.Errorf("findOwningRequest() with legacy run record = %q, want %q", got, "req1")
	}
}

// TestFindOwningRequestNone proves the "belongs to no request" case stays
// "", not an error, for a run that is neither self-tagged nor named by any
// request's own Tickets.
func TestFindOwningRequestNone(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	if got := findOwningRequest(dataDir, "run1"); got != "" {
		t.Errorf("findOwningRequest() = %q, want \"\"", got)
	}
}
