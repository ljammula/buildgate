package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"buildgate/internal/handoff"
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
	for dir, ok := range map[string]bool{"": true, ".": true, "web": true, "we": false, "../web": false, "other": false} {
		manifest := filepath.Join(t.TempDir(), "held.json")
		body := fmt.Sprintf(`[{"path":"web/a.test.ts","dir":%q,"sha256":%q,"host_path":%q}]`, dir, sha, good)
		if err := os.WriteFile(manifest, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadChallengeHeld(dataDir, manifest); (err == nil) != ok {
			t.Errorf("project directory %q: err %v, want accepted %v", dir, err, ok)
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

// Files that ran together under the verify command, whose run failed, are
// named under the group's own sentence, never under "Held tests that fail" as
// files that each failed; a file a runner judged red stays under it.
func TestRenderChallengeMarkdownNamesARedGroupAsOneRun(t *testing.T) {
	groupFailure := []run.ChallengeFailure{{Test: "verify command", Assertion: "exit status 1"}}
	files := []run.ChallengeFile{{Path: "p/own_test.go", Criterion: "AC-1", Verdict: run.ChallengeRed, Runner: "go-test-json"}}
	for _, p := range []string{"web/a.test.js", "web/b.test.js", "web/c.test.js"} {
		files = append(files, run.ChallengeFile{Path: p, Criterion: "AC-2", Verdict: run.ChallengeRed, Failures: groupFailure, Group: true})
	}
	body := renderChallengeMarkdown(&run.ChallengeRecord{Verdict: run.ChallengeRed, Files: files})
	own, group, ok := strings.Cut(body, handoff.GroupFailedLead)
	if !ok {
		t.Fatalf("body lacks the group's sentence:\n%s", body)
	}
	if !strings.Contains(own, "Held tests that fail on this change") || !strings.Contains(own, "p/own_test.go") || strings.Contains(own, "web/") {
		t.Errorf("the files a runner judged:\n%s", own)
	}
	for _, p := range []string{"web/a.test.js", "web/b.test.js", "web/c.test.js"} {
		if !strings.Contains(group, "- `"+p+"` (AC-2)") {
			t.Errorf("the group lacks %s:\n%s", p, group)
		}
	}
	onlyGroup := renderChallengeMarkdown(&run.ChallengeRecord{Verdict: run.ChallengeRed, Files: files[1:]})
	if strings.Contains(onlyGroup, "Held tests that fail") || !strings.Contains(onlyGroup, handoff.GroupFailedLead) {
		t.Errorf("a red group alone:\n%s", onlyGroup)
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

// The first carved ticket's pull request body lists the mixed run's red and
// broken challenge files with the note that no round ran; no other run's does.
func TestRenderCarvedChallengeMarkdown(t *testing.T) {
	rp := &run.Replay{Carve: &run.Carve{Notes: []string{"challenge test `p/x_test.go` (AC-1) fails on the mixed change; no challenge round ran for it"}}}
	got := renderCarvedChallengeMarkdown(rp)
	if !strings.Contains(got, "## Challenge of the mixed change") || !strings.Contains(got, "p/x_test.go") || !strings.Contains(got, "no challenge round ran") {
		t.Errorf("rendered = %q", got)
	}
	for name, none := range map[string]*run.Replay{"no replay": nil, "not carved": {}, "no notes": {Carve: &run.Carve{}}} {
		if out := renderCarvedChallengeMarkdown(none); out != "" {
			t.Errorf("%s rendered %q", name, out)
		}
	}
}
