package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"buildgate/internal/run"
	"buildgate/internal/sessionconfig"
)

func writeHeldManifest(t *testing.T, dataDir, hostPath, path, sha string) string {
	t.Helper()
	manifest := filepath.Join(t.TempDir(), "held.json")
	body := fmt.Sprintf(`[{"path":%q,"sha256":%q,"host_path":%q}]`, path, sha, hostPath)
	if err := os.WriteFile(manifest, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return manifest
}

// -challenge-held names only held copies under a run's challenge directory of
// this data dir, with a clean path and a SHA-256.
func TestLoadChallengeHeldChecksEveryEntry(t *testing.T) {
	dataDir := t.TempDir()
	good := filepath.Join(dataDir, "runs", "r1", run.ChallengeDirName, "0", "edge_test.go")
	if err := os.MkdirAll(filepath.Dir(good), 0o755); err != nil {
		t.Fatal(err)
	}
	sha := strings.Repeat("a", 64)
	if held, err := loadChallengeHeld(dataDir, writeHeldManifest(t, dataDir, good, "edge_test.go", sha)); err != nil || len(held) != 1 {
		t.Fatalf("a well-formed manifest: %v, %v", held, err)
	}
	for name, tc := range map[string]struct{ host, path, sha string }{
		"outside the data dir": {filepath.Join(t.TempDir(), "edge_test.go"), "edge_test.go", sha},
		"another directory":    {filepath.Join(dataDir, "runs", "r1", "review-repro", "0", "edge_test.go"), "edge_test.go", sha},
		"dot-dot path":         {good, "../edge_test.go", sha},
		"absolute path":        {good, "/edge_test.go", sha},
		"short hash":           {good, "edge_test.go", "abc"},
		"relative host":        {"runs/r1/challenge/0/edge_test.go", "edge_test.go", sha},
	} {
		if _, err := loadChallengeHeld(dataDir, writeHeldManifest(t, dataDir, tc.host, tc.path, tc.sha)); err == nil {
			t.Errorf("%s: the manifest was accepted", name)
		}
	}
}

func TestRenderChallengeMarkdownNamesFilesAndNotesNeverContent(t *testing.T) {
	green := &run.ChallengeRecord{Verdict: run.ChallengeGreen, Files: []run.ChallengeFile{
		{Path: "p/edge_test.go", Criterion: "AC-2", Verdict: run.ChallengeGreen},
		{Path: "p/other_test.go", Criterion: "AC-3", Verdict: run.ChallengeRed},
	}, Notes: []string{"collection not proven for 1 challenge file(s) run under the repository's verify command"}}
	body := renderChallengeMarkdown(green)
	for _, want := range []string{"## Challenge", "Tests committed on this branch", "- `p/edge_test.go` (AC-2)", "- Note: collection not proven"} {
		if !strings.Contains(body, want) {
			t.Errorf("green body lacks %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "other_test.go") {
		t.Errorf("a file that is not green is listed as committed:\n%s", body)
	}
	red := &run.ChallengeRecord{Verdict: run.ChallengeRed, Files: []run.ChallengeFile{{Path: "p/other_test.go", Criterion: "AC-3", Verdict: run.ChallengeRed, Failures: []run.ChallengeFailure{{Test: "TestX", Assertion: "SECRET-ASSERTION"}}}}}
	if body := renderChallengeMarkdown(red); !strings.Contains(body, "Held tests that fail") || !strings.Contains(body, "p/other_test.go") || strings.Contains(body, "SECRET-ASSERTION") {
		t.Errorf("red body:\n%s", body)
	}
	none := &run.ChallengeRecord{Verdict: run.ChallengeNoVerdict, Cause: "no challenger role configured"}
	if body := renderChallengeMarkdown(none); !strings.Contains(body, "No verdict: no challenger role configured") {
		t.Errorf("no-verdict body:\n%s", body)
	}
	if renderChallengeMarkdown(nil) != "" {
		t.Error("a run with no challenge renders a section")
	}
}

// A name that merely contains two dots is a clean path; a `..` component is not.
func TestHeldPathWithDotsInANameIsClean(t *testing.T) {
	for p, want := range map[string]bool{
		"parse..edge_test.go": true, "p/a..b_test.go": true, "p/edge_test.go": true,
		"../x_test.go": false, "p/../x_test.go": false, "/abs_test.go": false, "": false,
	} {
		if got := cleanHeldPath(p); got != want {
			t.Errorf("cleanHeldPath(%q) = %v, want %v", p, got, want)
		}
	}
}

// A challenger role whose route cannot be resolved never stops the build: the
// job then runs as if no role were set (SC-022: no verdict, nothing halts).
func TestUnresolvableChallengerRoleDoesNotStopTheBuild(t *testing.T) {
	yes := true
	tr := &ticketRun{rf: runFlags{challenge: &yes}}
	tr.settings.Roles = &sessionconfig.Roles{Challenger: &sessionconfig.RoleConfig{Model: "no-such-model"}}
	if err := tr.resolveChallenger(); err != nil {
		t.Fatalf("resolveChallenger = %v, want nil", err)
	}
	if tr.challengerSelection != nil {
		t.Fatalf("a failed route left a selection: %+v", tr.challengerSelection)
	}
}
