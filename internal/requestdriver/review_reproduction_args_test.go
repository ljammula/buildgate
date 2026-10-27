package requestdriver_test

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
	"buildgate/internal/requestdriver/requestdrivertest"
	"buildgate/internal/sessionconfig"
)

// A request's ticket build whose spec spec_review approved by hash gets
// -review-reproduction and -review-spec naming that spec.md (SC-022); a
// request with no approved spec hash gets neither, and a spec edited since
// its approval refuses the build.
func TestRequestTicketBuildCarriesTheApprovedSpecForReproductions(t *testing.T) {
	dataDir := t.TempDir()
	specPath := filepath.Join(t.TempDir(), "001.spec.md")
	if err := os.WriteFile(specPath, []byte("Verify-Command: pytest\n\n## Goal\n\ndo the thing\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ticket := request.Ticket{Index: 1, SpecPath: specPath}
	approved := filepath.Join(request.Dir(dataDir, "req-1"), "spec.md")
	if err := os.MkdirAll(filepath.Dir(approved), 0o750); err != nil {
		t.Fatal(err)
	}
	body := []byte("# Spec\n\n- `Parse` returns a map.\n")
	if err := os.WriteFile(approved, body, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)

	r := &request.Request{ID: "req-1", Workspace: "/repos/app", Project: "app"}
	on := requestdriver.WorkerConfig{Settings: sessionconfig.Settings{ReviewReproductions: true}}
	args, err := requestdriver.BuildRequestBuildArgs(dataDir, r, ticket, on)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(args, "-review-reproduction") || slices.Contains(args, "-review-spec") {
		t.Errorf("args with no approved spec hash = %v, want no reproduction flags", args)
	}

	r.ApprovedSHA256 = map[string]string{"spec.md": hex.EncodeToString(sum[:])}
	args, err = requestdriver.BuildRequestBuildArgs(dataDir, r, ticket, on)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.Index(args, "-review-spec")
	if !slices.Contains(args, "-review-reproduction") || i < 0 || args[i+1] != approved {
		t.Errorf("args = %v, want -review-reproduction and -review-spec %s", args, approved)
	}

	r.ApprovedSHA256["spec.md"] = strings.Repeat("0", 64)
	if _, err := requestdriver.BuildRequestBuildArgs(dataDir, r, ticket, on); err == nil || !strings.Contains(err.Error(), "changed since spec_review approved it") {
		t.Errorf("a spec edited since approval = %v, want the build refused", err)
	}
}

// With review_reproductions off, a request's ticket build keeps -review-spec
// (its review still cites the approved spec) but gets neither
// -review-reproduction nor -recheck-reproductions: every finding is advice.
// A ticket whose list already holds a reproduction is still rechecked
// against it (SC-022). Off is the default.
func TestReviewReproductionsOffKeepsTheSpecButHoldsNothing(t *testing.T) {
	dataDir := t.TempDir()
	specPath := filepath.Join(t.TempDir(), "001.spec.md")
	if err := os.WriteFile(specPath, []byte("Verify-Command: pytest\n\n## Goal\n\ndo the thing\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	approved := filepath.Join(request.Dir(dataDir, "req-1"), "spec.md")
	if err := os.MkdirAll(filepath.Dir(approved), 0o750); err != nil {
		t.Fatal(err)
	}
	body := []byte("# Spec\n\n- `Parse` returns a map.\n")
	if err := os.WriteFile(approved, body, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	ticket := request.Ticket{Index: 1, SpecPath: specPath}
	r := &request.Request{ID: "req-1", Workspace: "/repos/app", Project: "app", Tickets: []request.Ticket{ticket}, ApprovedSHA256: map[string]string{"spec.md": hex.EncodeToString(sum[:])}}
	off := requestdriver.WorkerConfig{Settings: sessionconfig.DefaultSettings()}
	on := requestdriver.WorkerConfig{Settings: sessionconfig.Settings{ReviewReproductions: true}}

	args, err := requestdriver.BuildRequestBuildArgs(dataDir, r, ticket, off)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.Index(args, "-review-spec")
	if i < 0 || args[i+1] != approved || slices.Contains(args, "-review-reproduction") || slices.Contains(args, "-recheck-reproductions") {
		t.Errorf("default (off): args = %v, want -review-spec %s and no reproduction flags", args, approved)
	}

	args, err = requestdriver.BuildRequestBuildArgs(dataDir, r, ticket, on)
	if err != nil {
		t.Fatal(err)
	}
	i = slices.Index(args, "-review-spec")
	if !slices.Contains(args, "-review-reproduction") || i < 0 || args[i+1] != approved || slices.Contains(args, "-recheck-reproductions") {
		t.Errorf("on: args = %v, want -review-reproduction and -review-spec %s", args, approved)
	}

	const origin = "run-origin"
	requestdrivertest.ReproductionRefusedOn(t, dataDir, origin, "", "", "")
	r.Tickets[0].NoteReproductionRefusal(requestdrivertest.HeldEntry(t, dataDir, origin), origin)
	args, err = requestdriver.BuildRequestBuildArgs(dataDir, r, r.Tickets[0], off)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(args, "-review-reproduction") || !slices.Contains(args, "-recheck-reproductions") || !slices.Contains(args, "-review-spec") {
		t.Errorf("off with a reproduction on the ticket's list: args = %v, want it rechecked", args)
	}
}
