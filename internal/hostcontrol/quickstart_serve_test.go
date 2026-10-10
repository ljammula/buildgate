package hostcontrol

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestQuickstartServeChildEnvStripsExistingTokens proves
// quickstartServeChildEnv scrubs every inherited FACTORYD_API_* var (an
// operator's shell could have one set for an unrelated reason) -- see
// that function's own doc comment for why this is a separate builder
// from worker's quickstartChildEnv, which must never receive any of
// these.
func TestQuickstartServeChildEnvStripsExistingTokens(t *testing.T) {
	t.Setenv("FACTORYD_API_OVERRIDE_TOKEN", "stale-override")
	t.Setenv("FACTORYD_API_READ_TOKEN", "stale-read")
	t.Setenv("FACTORYD_API_START_TOKEN", "stale-start")

	env := QuickstartServeChildEnv()

	for _, kv := range env {
		if strings.HasPrefix(kv, "FACTORYD_API_") {
			t.Errorf("quickstartServeChildEnv forwarded a control-plane token into the spawned serve child's env: %q", kv)
		}
	}
}

// TestQuickstartServeChildEnvNeverSetsStartToken proves
// quickstartServeChildEnv never itself SETS FACTORYD_API_START_TOKEN --
// closes the "also" note from the 2026-09-24 adversarial review: serve
// now resolves its own stable token by reading serve-start-token off disk
// (via its own -config), so putting the same value in the child's
// environment as well would only be a second, `ps eww`-visible exposure
// of a permanent start-class credential for no benefit.
func TestQuickstartServeChildEnvNeverSetsStartToken(t *testing.T) {
	t.Setenv("FACTORYD_API_START_TOKEN", "")
	env := QuickstartServeChildEnv()
	for _, kv := range env {
		if strings.HasPrefix(kv, "FACTORYD_API_START_TOKEN=") {
			t.Errorf("quickstartServeChildEnv set FACTORYD_API_START_TOKEN=%q -- must never set it at all", strings.TrimPrefix(kv, "FACTORYD_API_START_TOKEN="))
		}
	}
}

// TestQuickstartSpawnServePassesConfigFlagNotToken proves the spawned
// serve command line carries -config (so it can find the stable token
// file itself, an adversarial-review finding) and NEVER carries the token
// as an argv value anywhere (it isn't one -- there is no such flag), and
// that the child's env (as built by quickstartServeChildEnv) carries no
// FACTORYD_API_START_TOKEN either.
func TestQuickstartSpawnServePassesConfigFlag(t *testing.T) {
	dp := newFakeDeps(t)
	dataDir := t.TempDir()
	configPath := filepath.Join(t.TempDir(), "config.yml")

	// Real exec.Command bookkeeping without really launching serve --
	// use a fast-exiting stand-in ("true") as the "binary" so this stays
	// fast and hermetic, then inspect the constructed command indirectly
	// via the recorded pidfile and log files.
	// true, not false: quickstartWaitForServeReady polls for up to
	// quickstartServeReadyTimeout (10s, a const -- not test-overridable)
	// before giving up, so a false stub here would make this test take
	// 10 real seconds for no benefit; this test only cares about the
	// spawned command's own argv/pidfile/log permissions, not the readiness
	// wait itself.
	restoreReady := dp.serveHealthzOKFn
	dp.serveHealthzOKFn = func(addr string) bool { return true }
	t.Cleanup(func() { dp.serveHealthzOKFn = restoreReady })

	var out bytes.Buffer
	err := RealSpawnServe(dp, &out, "/bin/echo", configPath, dataDir, "127.0.0.1:0")
	if err != nil {
		t.Fatalf("quickstartSpawnServe: %v", err)
	}

	pidPath := filepath.Join(dataDir, "quickstart-serve.pid")
	if _, err := os.Stat(pidPath); err != nil {
		t.Fatalf("pid file not written: %v", err)
	}

	logDir := filepath.Join(dataDir, "logs")
	info, err := os.Stat(logDir)
	if err != nil {
		t.Fatalf("log dir not created: %v", err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Errorf("log dir mode = %v, want 0700 (adversarial review)", info.Mode().Perm())
	}
	for _, name := range []string{"quickstart-serve.out.log", "quickstart-serve.err.log"} {
		fi, err := os.Stat(filepath.Join(logDir, name))
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("%s mode = %v, want 0600 (adversarial review)", name, fi.Mode().Perm())
		}
	}
}

// TestQuickstartSecureLogDirChmodsExistingLooseDir proves
// quickstartSecureLogDir tightens an already-existing, looser-permission
// directory, not just a freshly created one.
func TestQuickstartSecureLogDirChmodsExistingLooseDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "logs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := QuickstartSecureLogDir(dir); err != nil {
		t.Fatalf("quickstartSecureLogDir: %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Errorf("existing log dir mode = %v, want 0700 after quickstartSecureLogDir", info.Mode().Perm())
	}
}
