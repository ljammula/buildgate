package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"buildgate/internal/run"
)

// writeDataDirSessionConfig writes a minimal session config naming dataDir
// as data_dir, at the default lookup path isolateSessionConfig points at.
func writeDataDirSessionConfig(t *testing.T, configPath, dataDir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(configPath), 0o750); err != nil {
		t.Fatalf("create config dir: %v", err)
	}
	if err := os.WriteFile(configPath, []byte("data_dir: "+dataDir+"\n"), 0o600); err != nil {
		t.Fatalf("write session config: %v", err)
	}
}

// TestStatusMainUsesSessionConfigDataDir proves `factoryd status` reads the
// same data_dir a session config gives `factoryd worker`/`factoryd
// serve`, without an operator repeating -data-dir on every command --
// mirroring TestApplySessionConfigDataDirAppliesConfiguredValue's own
// serve-side coverage.
func TestStatusMainUsesSessionConfigDataDir(t *testing.T) {
	configPath := isolateSessionConfig(t)
	dataDir := t.TempDir()
	writeDataDirSessionConfig(t, configPath, dataDir)

	mustSaveStatusRun(t, dataDir, &run.Run{ID: "run-cfg", Project: "appa", State: run.StateAccepted, CreatedAt: "2026-09-01T00:00:00Z", UpdatedAt: "2026-09-01T00:01:00Z"})

	if err := statusMain([]string{"-json"}); err != nil {
		t.Fatalf("statusMain: %v", err)
	}
}

// TestStatusMainExplicitDataDirWinsOverSessionConfig proves an explicit
// -data-dir still overrides the session config's data_dir, the same
// explicit-flag > config-file precedence worker/serve already document.
func TestStatusMainExplicitDataDirWinsOverSessionConfig(t *testing.T) {
	configPath := isolateSessionConfig(t)
	writeDataDirSessionConfig(t, configPath, t.TempDir())

	explicitDir := t.TempDir()
	mustSaveStatusRun(t, explicitDir, &run.Run{ID: "run-explicit", Project: "appa", State: run.StateAccepted, CreatedAt: "2026-09-01T00:00:00Z", UpdatedAt: "2026-09-01T00:01:00Z"})

	if err := statusMain([]string{"-data-dir", explicitDir, "-json"}); err != nil {
		t.Fatalf("statusMain: %v", err)
	}
}

// TestWatchMainUsesSessionConfigDataDir proves `factoryd watch` resolves
// -data-dir the same way `factoryd status`/`factoryd serve` do: no explicit
// flag, session config's data_dir wins.
func TestWatchMainUsesSessionConfigDataDir(t *testing.T) {
	configPath := isolateSessionConfig(t)
	dataDir := t.TempDir()
	writeDataDirSessionConfig(t, configPath, dataDir)

	r := &run.Run{ID: "run-watch-cfg", Ticket: "T1", State: run.StateAccepted, CreatedAt: time.Now().Format(time.RFC3339)}
	if err := r.Save(dataDir); err != nil {
		t.Fatalf("save run: %v", err)
	}

	if err := watchMain([]string{"-no-follow", r.ID}); err != nil {
		t.Fatalf("watchMain: %v", err)
	}
}

// TestWatchMainExplicitDataDirWinsOverSessionConfig proves an explicit
// -data-dir still overrides the session config's data_dir for `factoryd
// watch`, mirroring TestStatusMainExplicitDataDirWinsOverSessionConfig.
func TestWatchMainExplicitDataDirWinsOverSessionConfig(t *testing.T) {
	configPath := isolateSessionConfig(t)
	writeDataDirSessionConfig(t, configPath, t.TempDir())

	explicitDir := t.TempDir()
	r := &run.Run{ID: "run-watch-explicit", Ticket: "T1", State: run.StateAccepted, CreatedAt: time.Now().Format(time.RFC3339)}
	if err := r.Save(explicitDir); err != nil {
		t.Fatalf("save run: %v", err)
	}

	if err := watchMain([]string{"-data-dir", explicitDir, "-no-follow", r.ID}); err != nil {
		t.Fatalf("watchMain: %v", err)
	}
}
