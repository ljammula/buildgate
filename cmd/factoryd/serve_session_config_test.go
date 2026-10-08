package main

import (
	"flag"
	"os"
	"path/filepath"
	"testing"

	"buildgate/internal/sessionconfig"
)

// newDataDirFlagSet builds the minimal flag.FlagSet resolveDataDirFromSessionConfig
// needs: just -data-dir, defaulted the same way serveMain's own flag is.
func newDataDirFlagSet() (*flag.FlagSet, *string) {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	dataDir := flags.String("data-dir", "data", "directory containing durable run records")
	return flags, dataDir
}

// TestApplySessionConfigDataDirNoConfigLeavesDefault proves `factoryd serve`
// keeps working standalone (no session config file, no -data-dir) exactly
// as before this function existed -- unlike worker's own
// applySessionConfig, a missing file is not an error here.
func TestApplySessionConfigDataDirNoConfigLeavesDefault(t *testing.T) {
	isolateSessionConfig(t)
	flags, dataDir := newDataDirFlagSet()
	if err := flags.Parse(nil); err != nil {
		t.Fatalf("parse flags: %v", err)
	}
	if err := resolveDataDirFromSessionConfig(flags, dataDir, ""); err != nil {
		t.Fatalf("applySessionConfigDataDir: %v", err)
	}
	if *dataDir != "data" {
		t.Errorf("dataDir = %q, want default %q", *dataDir, "data")
	}
}

// TestApplySessionConfigDataDirAppliesConfiguredValue proves `factoryd
// serve` reads the same data_dir a session config file gives `factoryd
// worker`, so both commands agree on the durable-record directory
// without repeating -data-dir on each command line.
func TestApplySessionConfigDataDirAppliesConfiguredValue(t *testing.T) {
	configPath := isolateSessionConfig(t)
	if err := os.MkdirAll(filepath.Dir(configPath), 0o750); err != nil {
		t.Fatalf("create config dir: %v", err)
	}
	if err := os.WriteFile(configPath, []byte("data_dir: /var/factoryd/data\n"), 0o600); err != nil {
		t.Fatalf("write session config: %v", err)
	}

	flags, dataDir := newDataDirFlagSet()
	if err := flags.Parse(nil); err != nil {
		t.Fatalf("parse flags: %v", err)
	}
	if err := resolveDataDirFromSessionConfig(flags, dataDir, ""); err != nil {
		t.Fatalf("applySessionConfigDataDir: %v", err)
	}
	if *dataDir != "/var/factoryd/data" {
		t.Errorf("dataDir = %q, want %q", *dataDir, "/var/factoryd/data")
	}
}

// TestApplySessionConfigDataDirExplicitFlagWins proves an explicit
// -data-dir on the command line always overrides the session config's
// data_dir, the same explicit-flag > config-file precedence worker's
// own applySessionConfig documents.
func TestApplySessionConfigDataDirExplicitFlagWins(t *testing.T) {
	configPath := isolateSessionConfig(t)
	if err := os.MkdirAll(filepath.Dir(configPath), 0o750); err != nil {
		t.Fatalf("create config dir: %v", err)
	}
	if err := os.WriteFile(configPath, []byte("data_dir: /var/factoryd/data\n"), 0o600); err != nil {
		t.Fatalf("write session config: %v", err)
	}

	flags, dataDir := newDataDirFlagSet()
	if err := flags.Parse([]string{"-data-dir", "/explicit/data"}); err != nil {
		t.Fatalf("parse flags: %v", err)
	}
	if err := resolveDataDirFromSessionConfig(flags, dataDir, ""); err != nil {
		t.Fatalf("applySessionConfigDataDir: %v", err)
	}
	if *dataDir != "/explicit/data" {
		t.Errorf("dataDir = %q, want explicit %q", *dataDir, "/explicit/data")
	}
}

// TestSessionConfigWithNoDataDirKeyMeansOneFixedDataDir: a session config
// that never sets data_dir gives every command the same absolute data dir,
// whatever directory the command runs in. The relative "data" default is
// left for a machine with no session config at all.
func TestSessionConfigWithNoDataDirKeyMeansOneFixedDataDir(t *testing.T) {
	configPath := isolateSessionConfig(t)
	if err := os.MkdirAll(filepath.Dir(configPath), 0o750); err != nil {
		t.Fatalf("create config dir: %v", err)
	}
	if err := os.WriteFile(configPath, []byte("sandbox_image: localhost:5050/worker@sha256:abc\n"), 0o600); err != nil {
		t.Fatalf("write session config: %v", err)
	}
	resolve := func(args ...string) string {
		t.Helper()
		flags, dataDir := newDataDirFlagSet()
		if err := flags.Parse(args); err != nil {
			t.Fatalf("parse flags: %v", err)
		}
		if err := resolveDataDirFromSessionConfig(flags, dataDir, ""); err != nil {
			t.Fatalf("resolveDataDirFromSessionConfig: %v", err)
		}
		return *dataDir
	}
	want := sessionconfig.DefaultDataDir()
	if got := resolve(); got != want || !filepath.IsAbs(got) {
		t.Fatalf("dataDir = %q, want the profile default %q", got, want)
	}
	// From another directory: the same records.
	t.Chdir(t.TempDir())
	if got := resolve(); got != want {
		t.Errorf("from another directory dataDir = %q, want %q", got, want)
	}
	// An explicit flag still wins, and no config at all keeps the flag's default.
	if got := resolve("-data-dir", "/elsewhere"); got != "/elsewhere" {
		t.Errorf("explicit -data-dir = %q", got)
	}
	if err := os.Remove(configPath); err != nil {
		t.Fatal(err)
	}
	if got := resolve(); got != "data" {
		t.Errorf("with no session config dataDir = %q, want the flag default", got)
	}
}
