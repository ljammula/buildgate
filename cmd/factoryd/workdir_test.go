package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"buildgate/internal/sessionconfig"
)

func TestAbsolutePathsResolvesAgainstTheStartingDirectory(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	rel, abs, empty := "data", "/srv/data", ""
	if err := absolutePaths(&rel, &abs, &empty); err != nil {
		t.Fatal(err)
	}
	if rel != filepath.Join(wd, "data") || abs != "/srv/data" || empty != "" {
		t.Errorf("absolutePaths = %q, %q, %q", rel, abs, empty)
	}
}

func TestAbsoluteConfigArgKeepsTheDefaultAndResolvesAProfile(t *testing.T) {
	isolateSessionConfig(t)
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for in, want := range map[string]string{
		"":            "",
		"walk":        sessionconfig.ProfilePath("walk"),
		"conf/x.yml":  filepath.Join(wd, "conf", "x.yml"),
		"/etc/fd.yml": "/etc/fd.yml",
	} {
		got, err := absoluteConfigArg(in)
		if err != nil || got != want {
			t.Errorf("absoluteConfigArg(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

// TestLoadWorkerConfigMakesItsPathsAbsolute proves the worker's data dir
// and script paths name the same files after it leaves the directory it
// was started in.
func TestLoadWorkerConfigMakesItsPathsAbsolute(t *testing.T) {
	dp := newTestDeps(t)
	path := isolateSessionConfig(t)
	writeSessionConfig(t, path, "sandbox_image: img@sha256:aaaa\n")
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()
	rel, err := filepath.Rel(wd, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	cfg, gotDataDir, err := loadTestWorkerConfig(dp, []string{"-data-dir", rel, "-skip-doctor", "-build-app-script", "scripts/build_app.py",
		"-draft-spec-script", "scripts/draft_spec.py", "-plan-tickets-script", "scripts/plan_tickets.py"})
	if err != nil {
		t.Fatalf("loadWorkerConfig: %v", err)
	}
	if gotDataDir != dataDir {
		t.Errorf("data dir = %q, want %q", gotDataDir, dataDir)
	}
	if cfg.BuildAppScript != filepath.Join(wd, "scripts", "build_app.py") || !filepath.IsAbs(cfg.SessionConfigPath) {
		t.Errorf("build script %q, session config %q: want both absolute", cfg.BuildAppScript, cfg.SessionConfigPath)
	}
	if cfg.DraftSpecScript != filepath.Join(wd, "scripts", "draft_spec.py") || cfg.PlanTicketsScript != filepath.Join(wd, "scripts", "plan_tickets.py") {
		t.Errorf("draft script %q, plan script %q: want both absolute against %s", cfg.DraftSpecScript, cfg.PlanTicketsScript, wd)
	}
}

// TestServeMovesToItsDataDir proves serve leaves the directory it was
// started in for its data dir before it serves anything.
func TestServeMovesToItsDataDir(t *testing.T) {
	isolateSessionConfig(t)
	dp := newTestDeps(t)
	var moved []string
	fakeHostOf(dp).chdirFn = func(dir string) error {
		moved = append(moved, dir)
		return nil
	}
	dataDir := t.TempDir()
	if _, err := serveMainUntilBindWith(t, dp, dataDir); err == nil {
		t.Fatal("serveMain bound an occupied address")
	}
	if len(moved) != 1 || moved[0] != dataDir {
		t.Errorf("serve moved to %q, want its data dir %q", moved, dataDir)
	}
}

func TestEnterStableWorkDirFallsBackToHome(t *testing.T) {
	dp := newTestDeps(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	var moved string
	fakeHostOf(dp).chdirFn = func(dir string) error { moved = dir; return nil }
	if err := enterStableWorkDir(dp, filepath.Join(home, "missing")); err != nil {
		t.Fatal(err)
	}
	if moved != home {
		t.Errorf("moved to %q, want $HOME %q", moved, home)
	}
	fakeHostOf(dp).chdirFn = func(string) error { return os.ErrPermission }
	if err := enterStableWorkDir(dp, home); err == nil || !strings.Contains(err.Error(), home) {
		t.Errorf("err = %v, want it to name the directory", err)
	}
}
