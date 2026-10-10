package main

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"buildgate/internal/evidence"
	"buildgate/internal/requestdriver/requestdrivertest"
	"buildgate/internal/run"
)

// A prompt the factory sent to a build, with the record of an earlier attempt
// in it, reaches the operator's own readers (the saved file, the prompts
// route, `factoryd logs -prompt`) and nothing else: not run.json, the
// triage sentence, the pull request body, the project's observations, the
// run and request routes, any MCP read tool, the log tails, or any file left
// in the worktree the review launches share.
func TestASavedBuildPromptReachesOnlyTheOperatorsReaders(t *testing.T) {
	const marker = "PROMPT-MARKER-5b1d-earlier-attempt-record"
	dp := newTestDeps(t)
	dataDir, id := requestdrivertest.BuildingFixture(dp, t, 1)
	runID := requestdrivertest.TicketRunID(id, 1)
	runDir := run.Dir(dataDir, runID)
	if err := os.MkdirAll(runDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "build_app.log"), []byte("a build log line\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// The worktree the build ran in: its first prompt, with the record of
	// the earlier attempt a corrective build is given, was saved in the
	// harness session folder. The host copy takes it out and drops it.
	worktree := t.TempDir()
	session := filepath.Join(worktree, ".pi-build-session", "prompts")
	if err := os.MkdirAll(session, 0o755); err != nil {
		t.Fatal(err)
	}
	prompt := "Fix the cache.\n\nWhat the earlier attempt left:\n- " + marker + "\n"
	if err := os.WriteFile(filepath.Join(session, "build-round-1.md"), []byte(prompt), 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(runDir, evidence.PromptsDirName, evidence.PromptAttemptDir("build", 1))
	if n, err := evidence.RetainPrompts(worktree, []string{".pi-build-session"}, dst); n != 1 || err != nil {
		t.Fatalf("RetainPrompts = %d, %v", n, err)
	}
	if err := os.RemoveAll(filepath.Join(worktree, ".pi-build-session")); err != nil {
		t.Fatal(err)
	}

	rr := requestdrivertest.QuarantinedOn(t, dataDir, runID, "factoryd/"+runID, strings.Repeat("1", 40), strings.Repeat("2", 40), "lint")
	rr.Project = "prompts-project"
	if err := rr.Save(dataDir); err != nil {
		t.Fatal(err)
	}

	checkPromptSurfaces(t, dataDir, id, runID, rr, dst, marker)
	// What a review launch mounts is the worktree.
	err := filepath.WalkDir(worktree, func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if data, readErr := os.ReadFile(path); readErr == nil && bytes.Contains(data, []byte(marker)) {
				t.Errorf("%s in the worktree holds the saved prompt", path)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// A drafting job's prompts land in the request's own directory, one folder
// per job, and leave the drafting worktree.
func TestRetainDraftPromptsKeepsEachJobsPromptsInTheRequestDirectory(t *testing.T) {
	dataDir := t.TempDir()
	for _, text := range []string{"first draft prompt", "second draft prompt"} {
		worktree := t.TempDir()
		session := filepath.Join(worktree, specDraftScratchDirName, "session", "prompts")
		if err := os.MkdirAll(session, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(session, "draft-spec.md"), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
		retainDraftPrompts(dataDir, "req-1", "spec", worktree, []string{specDraftScratchDirName + "/session", specDraftScratchDirName + "/check-session"})
		if _, err := os.Stat(session); !os.IsNotExist(err) {
			t.Errorf("the drafting worktree still holds its prompts (%v)", err)
		}
	}
	dir := filepath.Join(dataDir, "requests", "req-1")
	for attempt, want := range map[string]string{"spec-1": "first draft prompt", "spec-2": "second draft prompt"} {
		got, err := os.ReadFile(filepath.Join(dir, "prompts", attempt, "draft-spec.md"))
		if err != nil || string(got) != want {
			t.Errorf("%s = %q, %v, want %q", attempt, got, err, want)
		}
	}
}

// checkPromptSurfaces asserts where the marker is (the saved file, the prompts
// route, `logs -prompt`) and where it is not (every other surface).
func checkPromptSurfaces(t *testing.T, dataDir, id, runID string, rr *run.Run, dst, marker string) {
	t.Helper()
	saved, err := os.ReadFile(filepath.Join(dst, "build-round-1.md"))
	if err != nil || !bytes.Contains(saved, []byte(marker)) {
		t.Fatalf("the saved prompt (%v) lacks the marker", err)
	}
	absent := localSurfaces(t, dataDir, id, runID, rr)
	get := apiSurfaces(t, dataDir, id, runID, rr, absent)
	for _, name := range []string{"GET /runs/{id}", "GET /runs", "observations", "GET /requests/{id}"} {
		if absent[name] == "" || !strings.Contains(absent[name], "prompts-project") && !strings.Contains(absent[name], runID) && !strings.Contains(absent[name], id) {
			t.Errorf("%s returned nothing about the run (%q): its absence check would prove nothing", name, absent[name])
		}
	}
	if body := get("/runs/" + runID + "/prompts/build-1/build-round-1"); !strings.Contains(body, marker) {
		t.Errorf("GET /runs/{id}/prompts/{attempt}/{name} lacks the prompt: %s", body)
	}
	if body := get("/runs/" + runID + "/prompts"); !strings.Contains(body, "build-round-1") || strings.Contains(body, marker) {
		t.Errorf("GET /runs/{id}/prompts = %s, want the name and none of the text", body)
	}
	var printed bytes.Buffer
	if err := runLogsPrompt(&printed, dataDir, runID, "build-round-1"); err != nil || !strings.Contains(printed.String(), marker) {
		t.Errorf("logs -prompt = %q, %v, want the prompt", printed.String(), err)
	}

	// The listing names the prompt; nothing else shows its text.
	if !strings.Contains(absent["factoryd logs listing"], "prompts/build-1/build-round-1.md") || !strings.Contains(absent["factoryd logs listing"], "[prompt, as saved by the build]") {
		t.Errorf("the log listing lacks the prompt:\n%s", absent["factoryd logs listing"])
	}
	for name, text := range absent {
		if strings.Contains(text, marker) {
			t.Errorf("%s carries the saved prompt", name)
		}
	}
}
