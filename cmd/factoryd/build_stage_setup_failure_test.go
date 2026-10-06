package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"buildgate/internal/progress"
)

// TestIntegrationBuildStageSetupFailureEmitsEndMark is the regression test
// for the sibling of the evidence-stage bug this same change fixes: a
// failure inside runSandboxWithRetries' own pre-attempt setup (staging the
// build script, here forced by pointing -build-app-script at a path that
// doesn't exist) returns before onAttempt/buildAttempt is ever invoked --
// the only place that used to emit the "build" stage's own "end" progress
// mark. Without the buildAttemptRan guard, the "start" mark this run's own
// preflight already wrote would be left open in progress.jsonl forever,
// exactly the stuck-progress symptom `factoryd status`/the console would
// then show as "stuck on build" even though the run is long since halted.
func TestIntegrationBuildStageSetupFailureEmitsEndMark(t *testing.T) {
	// not parallel-safe: newFixtureRepo calls t.Setenv.
	ws := newFixtureRepo(t)
	specPath := filepath.Join(t.TempDir(), "spec.md")
	if err := os.WriteFile(specPath, []byte("# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n"), 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	dataDir := t.TempDir()
	missingScript := filepath.Join(t.TempDir(), "does-not-exist.sh")

	cmd := factorydCommand(t,
		"-ticket", "fixture-ticket",
		"-sandbox-image", fakeSandboxImage,
		"-workspace", ws,
		"-spec", specPath,
		"-build-app-interpreter", "/bin/sh",
		"-build-app-script", missingScript,
		"-verify-command", "true",
		"-data-dir", dataDir,
	)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected a nonzero exit for a missing -build-app-script, got success: %s", out)
	}

	entries, err := os.ReadDir(filepath.Join(dataDir, "runs"))
	if err != nil {
		t.Fatalf("read runs dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected exactly 1 run directory, got %d: %v", len(entries), entries)
	}
	runID := entries[0].Name()

	b, err := os.ReadFile(filepath.Join(dataDir, "runs", runID, "run.json"))
	if err != nil {
		t.Fatalf("read run.json: %v", err)
	}
	var r struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatalf("unmarshal run.json: %v", err)
	}
	if r.State != "halted" {
		t.Fatalf("state = %q, want %q", r.State, "halted")
	}

	events, err := progress.Read(progress.Path(dataDir, runID))
	if err != nil {
		t.Fatalf("read progress feed: %v", err)
	}
	var sawBuildStart, sawBuildEnd bool
	for _, e := range events {
		if e.Source != "factory" || e.Stage != "build" {
			continue
		}
		switch e.Event {
		case "start":
			sawBuildStart = true
		case "end":
			sawBuildEnd = true
		}
	}
	if !sawBuildStart {
		t.Fatal("progress feed never recorded a \"build\" start mark")
	}
	if !sawBuildEnd {
		t.Fatalf("progress feed has a \"build\" start mark with no matching end mark -- stuck open (feed: %+v)", events)
	}
	// The Temporal path reports a staging failure as the build activity's
	// infrastructure failure, naming the missing script (RunBuildActivity,
	// internal/workflow/activities_build.go).
	want := regexp.MustCompile(`build subprocess infrastructure failure.*lstat ` + regexp.QuoteMeta(missingScript))
	if !want.Match(out) {
		t.Errorf("output = %q, want it to match %q", out, want)
	}
}
