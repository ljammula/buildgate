package main

import (
	"testing"

	"buildgate/internal/request"
)

// TestSubmitMainUsesSessionConfigDataDir proves `factoryd submit` resolves
// -data-dir from the session config's data_dir key when not given
// explicitly, the same way `factoryd status`/`factoryd watch`/`factoryd
// serve` already do (see TestStatusMainUsesSessionConfigDataDir) -- before
// this fix, submit ignored the session config entirely and only worked
// because the operator's cwd happened to match a configured data_dir.
func TestSubmitMainUsesSessionConfigDataDir(t *testing.T) {
	dp := newTestDeps(t)
	configPath := isolateSessionConfig(t)
	dataDir := t.TempDir()
	writeDataDirSessionConfig(t, configPath, dataDir)

	workspace := t.TempDir()
	writeTestSubmitRepo(t, workspace, `verify_command: "make ci-verify"
preflight_profile: brownfield
`)

	if err := submitMain(dp, []string{workspace, "Add idempotency keys to POST /refunds"}); err != nil {
		t.Fatalf("submitMain: %v", err)
	}

	requests, err := request.List(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) != 1 {
		t.Fatalf("len(requests) = %d, want 1 in the session-config data_dir", len(requests))
	}
}

// TestSubmitMainExplicitDataDirWinsOverSessionConfig proves an explicit
// -data-dir still overrides the session config's data_dir for `factoryd
// submit`, the same explicit-flag > config-file precedence
// resolveDataDirFromSessionConfig documents for every other caller.
func TestSubmitMainExplicitDataDirWinsOverSessionConfig(t *testing.T) {
	dp := newTestDeps(t)
	configPath := isolateSessionConfig(t)
	writeDataDirSessionConfig(t, configPath, t.TempDir())

	explicitDir := t.TempDir()
	workspace := t.TempDir()
	writeTestSubmitRepo(t, workspace, `verify_command: "make ci-verify"
preflight_profile: brownfield
`)

	if err := submitMain(dp, []string{"-data-dir", explicitDir, workspace, "Add idempotency keys to POST /refunds"}); err != nil {
		t.Fatalf("submitMain: %v", err)
	}

	requests, err := request.List(explicitDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) != 1 {
		t.Fatalf("len(requests) = %d, want 1 in the explicit -data-dir", len(requests))
	}
}

// TestRetryMainUsesSessionConfigDataDir proves `factoryd retry` resolves
// -data-dir from the session config the same way submit/status/watch/serve
// do.
func TestRetryMainUsesSessionConfigDataDir(t *testing.T) {
	dp := newTestDeps(t)
	configPath := isolateSessionConfig(t)
	dataDir := t.TempDir()
	writeDataDirSessionConfig(t, configPath, dataDir)
	saveRetryTestRequest(t, dataDir, "t1", request.StateHalted)

	if err := retryMain(dp, []string{"t1"}); err != nil {
		t.Fatalf("retryMain: %v", err)
	}

	got, err := request.Load(dataDir, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.State == request.StateHalted {
		t.Errorf("state = %q, want it moved out of halted", got.State)
	}
}

// TestRetryMainExplicitDataDirWinsOverSessionConfig proves an explicit
// -data-dir still overrides the session config's data_dir for `factoryd
// retry`.
func TestRetryMainExplicitDataDirWinsOverSessionConfig(t *testing.T) {
	dp := newTestDeps(t)
	configPath := isolateSessionConfig(t)
	writeDataDirSessionConfig(t, configPath, t.TempDir())

	explicitDir := t.TempDir()
	saveRetryTestRequest(t, explicitDir, "t1", request.StateHalted)

	if err := retryMain(dp, []string{"-data-dir", explicitDir, "t1"}); err != nil {
		t.Fatalf("retryMain: %v", err)
	}

	got, err := request.Load(explicitDir, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.State == request.StateHalted {
		t.Errorf("state = %q, want it moved out of halted", got.State)
	}
}
