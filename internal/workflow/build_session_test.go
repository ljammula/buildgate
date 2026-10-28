package workflow

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.temporal.io/sdk/testsuite"

	"buildgate/internal/runner"
	"buildgate/internal/testfixture"
)

// writeSession fills a build session folder as a build leaves it: the
// harness's state, a saved prompt, a failed round's output, the agent's
// notes, a credential-named file and a link.
func writeSession(t *testing.T, dir string) {
	t.Helper()
	for rel, body := range map[string]string{
		"2026-10-27.jsonl":                  "transcript",
		"session-origin.json":               `{"harness":"pi","model":"luna"}`,
		"codex-home/sessions/rollout.jsonl": "rollout",
		"codex-home/auth.json":              `{"key":"secret"}`,
		"codex-home/.tmp/plugin/x":          "download",
		"prompts/build-round-1.txt":         "the first prompt",
		"feedback/round-1/agent.log":        "failed round",
		"handoff-notes.md":                  "notes for the next attempt",
	} {
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, path, body)
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
}

// TestKeepBuildSessionCopiesOnlyTheHarnessState: the kept copy holds the
// harness's own session state, never a saved prompt, a failed round's
// output, the agent's notes, a credential-named file, a download or a link.
func TestKeepBuildSessionCopiesOnlyTheHarnessState(t *testing.T) {
	src, dst := t.TempDir(), filepath.Join(t.TempDir(), BuildSessionDirName)
	writeSession(t, src)
	if err := keepBuildSession(src, dst); err != nil {
		t.Fatalf("keepBuildSession: %v", err)
	}
	for _, want := range []string{"2026-10-27.jsonl", "session-origin.json", "codex-home/sessions/rollout.jsonl"} {
		if _, err := os.Stat(filepath.Join(dst, want)); err != nil {
			t.Errorf("the copy lacks %s: %v", want, err)
		}
	}
	for _, unwanted := range []string{"codex-home/auth.json", "codex-home/.tmp", "prompts", "feedback", "handoff-notes.md", "link"} {
		if _, err := os.Lstat(filepath.Join(dst, unwanted)); err == nil {
			t.Errorf("the copy carries %s", unwanted)
		}
	}
}

// TestKeepBuildSessionRefusesAnOversizeSession: nothing is kept for a
// session over the cap.
func TestKeepBuildSessionRefusesAnOversizeSession(t *testing.T) {
	src, dst := t.TempDir(), filepath.Join(t.TempDir(), BuildSessionDirName)
	if err := os.WriteFile(filepath.Join(src, "big"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(filepath.Join(src, "big"), maxKeptBuildSessionBytes+1); err != nil {
		t.Fatal(err)
	}
	if err := keepBuildSession(src, dst); err == nil {
		t.Fatal("an oversize session was kept")
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Errorf("a partial copy was left: %v", err)
	}
}

// TestRunBuildActivityCarriesTheFollowedRunsSession: a round given the kept
// session of the run it follows finds it in its worktree's session folder
// and is passed --resume-session; a build given none is passed neither, and
// a kept session is written to the run's directory only when asked.
func TestRunBuildActivityCarriesTheFollowedRunsSession(t *testing.T) {
	kept := filepath.Join(t.TempDir(), BuildSessionDirName)
	writeSession(t, kept)
	for name, tc := range map[string]struct {
		resume, keep bool
	}{
		"a round that resumes and keeps": {true, true},
		"a build that does neither":      {false, false},
	} {
		t.Run(name, func(t *testing.T) {
			repo := testfixture.NewGitRepo(t)
			logDir := t.TempDir()
			var gotArgs []string
			var sawTranscript bool
			activities := &Activities{
				LogDir: logDir,
				runWithRetries: func(_ context.Context, _ string, _ func(int) string, _ int, _ func(int, runner.Result, error), _ string, args ...string) (runner.Result, error) {
					gotArgs = args
					_, err := os.Stat(filepath.Join(repo, buildSessionDir, "2026-10-27.jsonl"))
					sawTranscript = err == nil
					// What this build's own session leaves.
					writeFile(t, filepath.Join(repo, buildSessionDir, "own.jsonl"), "this build")
					return runner.Result{}, nil
				},
			}
			input := fixtureInput()
			input.WorkspacePath = repo
			input.KeepBuildSession = tc.keep
			if tc.resume {
				input.ResumeSessionPath = kept
			}
			var suite testsuite.WorkflowTestSuite
			env := suite.NewTestActivityEnvironment()
			env.RegisterActivity(activities.RunBuildActivity)
			if _, err := env.ExecuteActivity(activities.RunBuildActivity, input); err != nil {
				t.Fatal(err)
			}
			if got := strings.Contains(strings.Join(gotArgs, " "), "--resume-session"); got != tc.resume || sawTranscript != tc.resume {
				t.Errorf("--resume-session %t, carried transcript seen %t; want %t", got, sawTranscript, tc.resume)
			}
			_, err := os.Stat(filepath.Join(logDir, BuildSessionDirName, "own.jsonl"))
			if (err == nil) != tc.keep {
				t.Errorf("kept session present %t, want %t", err == nil, tc.keep)
			}
			if _, err := os.Stat(filepath.Join(repo, buildSessionDir)); !os.IsNotExist(err) {
				t.Errorf("the session is still in the worktree: %v", err)
			}
		})
	}
}

// TestOnlyTheBuildIsGivenABuilderSession: no review, verify or gate Activity
// is handed a builder session; in this package only the build Activity's
// helper and the input's own type name it.
func TestOnlyTheBuildIsGivenABuilderSession(t *testing.T) {
	sources, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{"build_session.go": true, "workflow_types.go": true}
	for _, path := range sources {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if (strings.Contains(string(data), "ResumeSessionPath") || strings.Contains(string(data), "--resume-session")) && !allowed[path] {
			t.Errorf("%s uses a builder session: only the build Activity's helper may", path)
		}
	}
}
