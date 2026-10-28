package workflow

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.temporal.io/sdk/testsuite"

	"buildgate/internal/run"
	"buildgate/internal/runner"
	"buildgate/internal/testfixture"
)

// writeSession fills a build session folder as a build leaves it: the
// harness's state, a saved prompt, a failed round's output, the agent's
// notes, a credential-named file and a link.
func writeSession(t *testing.T, dir string) {
	t.Helper()
	for rel, body := range map[string]string{
		"2026-10-27.jsonl":                   "transcript",
		"session-origin.json":                `{"harness":"pi","model":"luna"}`,
		"codex-home/sessions/rollout.jsonl":  "rollout",
		"codex-home/auth.json":               `{"key":"secret"}`,
		"codex-home/.tmp/plugin/x":           "download",
		"prompts/build-round-1.txt":          "the first prompt",
		"feedback/round-1/agent.log":         "failed round",
		"handoff-notes.md":                   "notes for the next attempt",
		"codex-home/config.toml":             "model_provider = \"relay\"",
		"copilot-home/config.json":           "{}",
		"codex-home/skills/planted/SKILL.md": "planted skill",
		"pre-notes/2026-10-27.jsonl":         "set aside",
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
	src, dst := t.TempDir(), filepath.Join(t.TempDir(), run.BuildSessionDirName)
	writeSession(t, src)
	if err := keepBuildSession(src, dst); err != nil {
		t.Fatalf("keepBuildSession: %v", err)
	}
	for _, want := range []string{"2026-10-27.jsonl", "session-origin.json", "codex-home/sessions/rollout.jsonl"} {
		if _, err := os.Stat(filepath.Join(dst, want)); err != nil {
			t.Errorf("the copy lacks %s: %v", want, err)
		}
	}
	for _, unwanted := range []string{"codex-home/auth.json", "codex-home/.tmp", "prompts", "feedback", "handoff-notes.md", "link", "codex-home/config.toml", "copilot-home/config.json", "codex-home/skills", "pre-notes"} {
		if _, err := os.Lstat(filepath.Join(dst, unwanted)); err == nil {
			t.Errorf("the copy carries %s", unwanted)
		}
	}
}

// TestKeepBuildSessionRefusesAnOversizeSession: nothing is kept for a
// session over the cap.
func TestKeepBuildSessionRefusesAnOversizeSession(t *testing.T) {
	src, dst := t.TempDir(), filepath.Join(t.TempDir(), run.BuildSessionDirName)
	if err := os.WriteFile(filepath.Join(src, "big"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(filepath.Join(src, "big"), maxKeptBuildSessionBytes+1); err != nil {
		t.Fatal(err)
	}
	assertNotKept(t, src, dst)
}

// assertNotKept checks keepBuildSession keeps nothing of src but the reason.
func assertNotKept(t *testing.T, src, dst string) {
	t.Helper()
	if err := keepBuildSession(src, dst); err == nil {
		t.Fatal("the session was kept")
	}
	entries, err := os.ReadDir(dst)
	if err != nil || len(entries) != 1 || entries[0].Name() != notKeptFile {
		t.Errorf("the kept folder holds %v (%v), want only %s", entries, err, notKeptFile)
	}
}

// TestKeepBuildSessionRefusesTooManyEntriesOrLevels: a session over the
// entry or depth limit is not kept.
func TestKeepBuildSessionRefusesTooManyEntriesOrLevels(t *testing.T) {
	t.Run("depth", func(t *testing.T) {
		src := t.TempDir()
		deep := src
		for i := 0; i <= maxKeptBuildSessionDepth; i++ {
			deep = filepath.Join(deep, "d")
		}
		if err := os.MkdirAll(deep, 0o755); err != nil {
			t.Fatal(err)
		}
		assertNotKept(t, src, filepath.Join(t.TempDir(), run.BuildSessionDirName))
	})
	t.Run("entries", func(t *testing.T) {
		src := t.TempDir()
		for i := 0; i <= maxKeptBuildSessionEntries; i++ {
			if err := os.WriteFile(filepath.Join(src, fmt.Sprintf("f%05d", i)), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		assertNotKept(t, src, filepath.Join(t.TempDir(), run.BuildSessionDirName))
	})
}

// TestCopySessionFileRefusesALinkAndAFileThatGrew: a file opened by the copy
// must be a regular file at its path, not a link swapped in after the walk
// (nor under a linked folder that leads out of the session), and must hold
// no more than the bytes left under the cap when it is read.
func TestCopySessionFileRefusesALinkAndAFileThatGrew(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "secret"), "outside the session")
	writeFile(t, filepath.Join(src, "grown"), "0123456789")
	if err := os.Symlink(filepath.Join(outside, "secret"), filepath.Join(src, "swapped")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(src, "linked-dir")); err != nil {
		t.Fatal(err)
	}
	srcRoot, err := os.OpenRoot(src)
	if err != nil {
		t.Fatal(err)
	}
	defer srcRoot.Close()
	dstRoot, err := os.OpenRoot(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer dstRoot.Close()
	for _, rel := range []string{"swapped", "linked-dir/secret"} {
		if _, err := copySessionFile(srcRoot, dstRoot, rel, 1<<20); err == nil {
			t.Errorf("%s was copied", rel)
		}
	}
	// The walk counted 4 bytes left; the file holds 10 when it is read.
	if _, err := copySessionFile(srcRoot, dstRoot, "grown", 4); !errors.Is(err, errSessionTooLarge) {
		t.Errorf("a file that grew past the cap = %v, want errSessionTooLarge", err)
	}
}

// TestRunBuildActivityCarriesTheFollowedRunsSession: a round given the kept
// session of the run it follows finds it in its worktree's session folder
// and is passed --resume-session; a build given none is passed neither, and
// a kept session is written to the run's directory only when asked.
func TestRunBuildActivityCarriesTheFollowedRunsSession(t *testing.T) {
	kept := filepath.Join(t.TempDir(), run.BuildSessionDirName)
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
			_, err := os.Stat(filepath.Join(logDir, run.BuildSessionDirName, "own.jsonl"))
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
