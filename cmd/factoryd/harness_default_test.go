package main

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"buildgate/internal/harness"
)

// TestResolveHarnessScriptExplicitWins covers the explicit-flag branch:
// resolveHarnessScript must return the caller's own path untouched, never
// touching the harness cache.
func TestResolveHarnessScriptExplicitWins(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	got, err := resolveHarnessScript("/some/explicit/build_app.py", "build_app.py")
	if err != nil {
		t.Fatalf("resolveHarnessScript: %v", err)
	}
	if got != "/some/explicit/build_app.py" {
		t.Fatalf("got %q, want the explicit path unchanged", got)
	}
}

// TestResolveHarnessScriptDefaultsToEmbeddedHarness covers the unset-flag
// branch: an empty explicit value resolves to the embedded harness's
// copy of the named script, extracted to the cache directory.
func TestResolveHarnessScriptDefaultsToEmbeddedHarness(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	dir, err := harness.Ensure()
	if err != nil {
		t.Fatalf("harness.Ensure: %v", err)
	}
	got, err := resolveHarnessScript("", "build_app.py")
	if err != nil {
		t.Fatalf("resolveHarnessScript: %v", err)
	}
	if want := filepath.Join(dir, "build_app.py"); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestResolveHarnessScriptGoalPilot(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	dir, err := harness.Ensure()
	if err != nil {
		t.Fatalf("harness.Ensure: %v", err)
	}
	got, err := resolveHarnessScript("", "goal_pilot.py")
	if err != nil {
		t.Fatalf("resolveHarnessScript: %v", err)
	}
	if want := filepath.Join(dir, "goal_pilot.py"); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestVersionMainPrintsVersion(t *testing.T) {
	if err := versionMain(); err != nil {
		t.Fatalf("versionMain: %v", err)
	}
}

// TestServeMainWithoutDaemonStartsWithUnwritableHarnessCache is the
// regression for Codex review of PR #83, round 2: serveMain used to
// resolve -daemon-build-app-script unconditionally at flag parse, so a
// read-only/API-only serve deployment with no -daemon-temporal-address
// (the daemon supervisor path is the only thing on this command that
// ever touches build_app.py) failed to start over a harness cache it was
// never going to use. This proves serveMain gets past flag resolution
// fine with an unwritable XDG_CACHE_HOME as long as -daemon-temporal-
// address is left unset -- it only needs to fail for an unrelated reason
// (an already-occupied -addr, so it never actually serves) to prove that.
func TestServeMainWithoutDaemonStartsWithUnwritableHarnessCache(t *testing.T) {
	dp := newTestDeps(t)
	// serveMain now loads the default session config (loadDefaultSettings,
	// via its own baseSettings assignment) unconditionally on every call --
	// isolate HOME/XDG_CONFIG_HOME so a real ~/.config/factoryd/config.yml
	// on the machine running this test cannot change its behavior.
	isolateSessionConfig(t)

	// A file, not a directory, as XDG_CACHE_HOME: anything that tried to
	// extract into $XDG_CACHE_HOME/factoryd/harness/... under it would
	// fail outright (MkdirAll against a path that has a file as one of
	// its own ancestors).
	unwritable := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(unwritable, []byte("x"), 0o644); err != nil {
		t.Fatalf("create unwritable XDG_CACHE_HOME stand-in: %v", err)
	}
	t.Setenv("XDG_CACHE_HOME", unwritable)

	// Occupy a real address so serveMain's own ListenAndServe fails fast
	// with "address already in use" instead of actually serving -- this
	// only needs to prove serveMain got past flag resolution without a
	// harness error, not that it serves a live HTTP server.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a test listener: %v", err)
	}
	defer listener.Close()

	// serveMain reaches its own tier2SettingsOverride assignment before
	// ever getting to ListenAndServe below, unlike a real `serve` process
	// (never invoked twice in one process) this shares its test binary
	// with every other test in this package -- reset it once this test is
	// done so it does not leak into an unrelated later test.
	t.Cleanup(func() { tier2SettingsOverride = nil })

	err = serveMain(dp, []string{"-data-dir", t.TempDir(), "-addr", listener.Addr().String()})
	if err == nil {
		t.Fatal("serveMain unexpectedly succeeded (it should have failed to bind the already-occupied address)")
	}
	if strings.Contains(err.Error(), "resolve embedded") {
		t.Fatalf("serveMain failed resolving the embedded harness even though -daemon-temporal-address was never set: %v", err)
	}
}
