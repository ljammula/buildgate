package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
	"buildgate/internal/requestdriver/requestdrivertest"
)

// TestApproveMainParsesRealFlagSet proves -data-dir and the positional
// request id are parsed through approveMain's own real flag.FlagSet (see
// the plan's own "argv parsed through the real flag set" requirement),
// and that a real approval succeeds end to end.
func TestApproveMainParsesRealFlagSet(t *testing.T) {
	dp := newTestDeps(t)
	t.Parallel()
	dataDir := t.TempDir()
	requestdrivertest.NewApprovableDriverRequest(t, dataDir, "req-1", request.StateSpecReview)

	if err := approveMain(dp, []string{"-data-dir", dataDir, "req-1"}); err != nil {
		t.Fatalf("approveMain: %v", err)
	}
	loaded, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.State != request.StatePlanning {
		t.Errorf("State = %q, want %q", loaded.State, request.StatePlanning)
	}
	if loaded.ApprovedBy == "" {
		t.Error("ApprovedBy left empty")
	}
}

func TestApproveMainRequiresExactlyOnePositionalArgument(t *testing.T) {
	dp := newTestDeps(t)
	t.Parallel()
	if err := approveMain(dp, []string{"-data-dir", t.TempDir()}); err == nil {
		t.Fatal("approveMain with no request id: want an error, got nil")
	}
	if err := approveMain(dp, []string{"-data-dir", t.TempDir(), "req-1", "req-2"}); err == nil {
		t.Fatal("approveMain with two request ids: want an error, got nil")
	}
}

// TestRejectMainParsesRealFlagSet proves -reason and -data-dir (flags
// before the positional request id, the same convention submitMain's own
// flags follow) parse through rejectMain's real flag.FlagSet, and that a
// real rejection succeeds end to end. The reason reaches the request's
// own structured Rejections/History, not request.md -- request.Reject no
// longer appends anything there for any stage (adversarial review,
// 2026-09-24; see request.Reject's own doc comment).
func TestRejectMainParsesRealFlagSet(t *testing.T) {
	dp := newTestDeps(t)
	t.Parallel()
	dataDir := t.TempDir()
	requestdrivertest.NewApprovableDriverRequest(t, dataDir, "req-1", request.StateSpecReview)

	if err := rejectMain(dp, []string{"-reason", "needs more detail", "-data-dir", dataDir, "req-1"}); err != nil {
		t.Fatalf("rejectMain: %v", err)
	}
	loaded, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.State != request.StateSpecDrafting {
		t.Errorf("State = %q, want %q", loaded.State, request.StateSpecDrafting)
	}
	if len(loaded.Rejections) != 1 || loaded.Rejections[0].Reason != "needs more detail" {
		t.Errorf("Rejections = %+v, want one entry with the -reason text", loaded.Rejections)
	}
	b, err := os.ReadFile(request.TextPath(dataDir, "req-1"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "needs more detail") {
		t.Errorf("request.md contains the rejection reason, want it left untouched: %q", string(b))
	}
}

func TestRejectMainRequiresReason(t *testing.T) {
	dp := newTestDeps(t)
	t.Parallel()
	dataDir := t.TempDir()
	requestdrivertest.NewApprovableDriverRequest(t, dataDir, "req-1", request.StateSpecReview)
	if err := rejectMain(dp, []string{"-data-dir", dataDir, "req-1"}); err == nil {
		t.Fatal("rejectMain without -reason: want an error, got nil")
	}
}

func TestRejectMainRequiresExactlyOnePositionalArgument(t *testing.T) {
	dp := newTestDeps(t)
	t.Parallel()
	dataDir := t.TempDir()
	if err := rejectMain(dp, []string{"-reason", "x", "-data-dir", dataDir}); err == nil {
		t.Fatal("rejectMain with no request id: want an error, got nil")
	}
}

// TestRejectMainSendBackFlag proves `factoryd reject -to plan|spec` routes
// through rejectMain to request.SendBack instead of request.Reject, and
// that an invalid -to value is refused before anything is touched.
func TestRejectMainSendBackFlag(t *testing.T) {
	dp := newTestDeps(t)
	t.Parallel()
	dataDir := t.TempDir()
	requestdrivertest.NewApprovableDriverRequest(t, dataDir, "req-1", request.StateQuarantined)
	loaded, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	loaded.ApprovedSHA256 = map[string]string{"spec.md": "deadbeef"}
	if err := loaded.Save(dataDir); err != nil {
		t.Fatal(err)
	}

	if err := rejectMain(dp, []string{"-reason", "send it back", "-to", "plan", "-data-dir", dataDir, "req-1"}); err != nil {
		t.Fatalf("rejectMain -to plan: %v", err)
	}
	got, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != request.StatePlanning {
		t.Errorf("State = %q, want %q", got.State, request.StatePlanning)
	}

	if err := rejectMain(dp, []string{"-reason", "x", "-to", "bogus", "-data-dir", dataDir, "req-1"}); err == nil {
		t.Fatal("rejectMain -to bogus: want an error, got nil")
	}
}

// TestVerifyApprovedHashesRefusesAndNamesFile is the driver-side
// hash-mismatch test: approve, edit spec.md, then verifyApprovedHashes
// (the function the request driver's own wiring calls before advancing a
// request out of planning or building on the strength of that approval --
// see its own doc comment) refuses, naming spec.md.
func TestVerifyApprovedHashesRefusesAndNamesFile(t *testing.T) {
	dp := newTestDeps(t)
	t.Parallel()
	dataDir := t.TempDir()
	requestdrivertest.NewApprovableDriverRequest(t, dataDir, "req-1", request.StateSpecReview)

	if err := approveMain(dp, []string{"-data-dir", dataDir, "req-1"}); err != nil {
		t.Fatalf("approveMain: %v", err)
	}
	loaded, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := requestdriver.VerifyApprovedHashes(dataDir, loaded); err != nil {
		t.Fatalf("verifyApprovedHashes before any edit: %v", err)
	}

	if err := os.WriteFile(requestdriver.RequestSpecPath(dataDir, "req-1"), []byte("# Spec (edited after approval)\n"), 0o600); err != nil {
		t.Fatalf("edit spec.md: %v", err)
	}
	err = requestdriver.VerifyApprovedHashes(dataDir, loaded)
	if err == nil {
		t.Fatal("verifyApprovedHashes after edit: want an error, got nil")
	}
	if !strings.Contains(err.Error(), filepath.Base(requestdriver.RequestSpecPath(dataDir, "req-1"))) {
		t.Errorf("error %q does not name spec.md", err.Error())
	}
}

// TestApproveAndRejectUseSessionConfigDataDir: approve/reject hardcoded
// -data-dir's "data" default and never read the session config's data_dir,
// unlike submit/retry/status/watch -- found in a 2026-09-25 onboarding
// walk, where the printed `factoryd approve <id>` failed with "stat
// data/requests/<id>: no such file or directory".
func TestApproveAndRejectUseSessionConfigDataDir(t *testing.T) {
	dp := newTestDeps(t)
	configPath := isolateSessionConfig(t)
	dataDir := t.TempDir()
	writeDataDirSessionConfig(t, configPath, dataDir)
	requestdrivertest.NewApprovableDriverRequest(t, dataDir, "req-a", request.StateSpecReview)
	requestdrivertest.NewApprovableDriverRequest(t, dataDir, "req-r", request.StateSpecReview)

	if err := approveMain(dp, []string{"req-a"}); err != nil {
		t.Fatalf("approveMain: %v", err)
	}
	if err := rejectMain(dp, []string{"-reason", "needs more detail", "req-r"}); err != nil {
		t.Fatalf("rejectMain: %v", err)
	}
	for id, want := range map[string]request.State{"req-a": request.StatePlanning, "req-r": request.StateSpecDrafting} {
		loaded, err := request.Load(dataDir, id)
		if err != nil {
			t.Fatal(err)
		}
		if loaded.State != want {
			t.Errorf("%s State = %q, want %q", id, loaded.State, want)
		}
	}
}
