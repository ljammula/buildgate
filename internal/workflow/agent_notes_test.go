package workflow

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.temporal.io/sdk/testsuite"

	"buildgate/internal/evidence"
	"buildgate/internal/runner"
	"buildgate/internal/sandbox/sandboxtest"
	"buildgate/internal/testfixture"
)

const notesMarker = "MARKER-the-agent-wrote-this-for-the-next-attempt"

// runBuildLeavingNotes runs the build Activity with a build that leaves
// plant(session folder) behind, and returns the worktree and the log dir.
func runBuildLeavingNotes(t *testing.T, plant func(session string)) (repo, logDir string) {
	t.Helper()
	repo = testfixture.NewGitRepo(t)
	logDir = t.TempDir()
	activities := &Activities{
		LogDir: logDir,
		runWithRetries: func(_ context.Context, _ string, _ func(int) string, _ int, _ func(int, runner.Result, error), _ string, _ ...string) (runner.Result, error) {
			writeFile(t, filepath.Join(repo, buildSessionDir, "session.jsonl"), "the first prompt\n")
			plant(filepath.Join(repo, buildSessionDir))
			return runner.Result{ExitCode: 1}, nil
		},
	}
	input := fixtureInput()
	input.WorkspacePath = repo
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(activities.RunBuildActivity)
	if _, err := env.ExecuteActivity(activities.RunBuildActivity, input); err != nil {
		t.Fatal(err)
	}
	return repo, logDir
}

func TestRunBuildActivityCarriesTheNotesOutBeforeAnyLaterStep(t *testing.T) {
	t.Run("a plain notes file", func(t *testing.T) {
		repo, logDir := runBuildLeavingNotes(t, func(session string) {
			writeFile(t, filepath.Join(session, "handoff-notes.md"), "What I did\n- "+notesMarker+"\n")
		})
		err := filepath.WalkDir(repo, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || d.Type()&fs.ModeSymlink != 0 {
				return nil
			}
			if data, readErr := os.ReadFile(path); readErr == nil && strings.Contains(string(data), notesMarker) {
				t.Errorf("%s still holds the notes after the build step returned", path)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		got, readErr := os.ReadFile(filepath.Join(logDir, evidence.AgentNotesFileName))
		if readErr != nil || string(got) != "What I did\n- "+notesMarker+"\n" {
			t.Errorf("retained notes = %q, %v, want the reply copied out", got, readErr)
		}
		if info, _ := os.Stat(filepath.Join(logDir, evidence.AgentNotesFileName)); info == nil || info.Mode().Perm() != 0o600 {
			t.Errorf("retained notes mode = %v, want 0600", info)
		}
	})
	t.Run("a symlinked notes file retains nothing", func(t *testing.T) {
		outside := filepath.Join(t.TempDir(), "secret.md")
		writeFile(t, outside, notesMarker)
		_, logDir := runBuildLeavingNotes(t, func(session string) {
			if err := os.Symlink(outside, filepath.Join(session, "handoff-notes.md")); err != nil {
				t.Fatal(err)
			}
		})
		if _, err := os.Stat(filepath.Join(logDir, evidence.AgentNotesFileName)); !os.IsNotExist(err) {
			t.Errorf("a symlinked notes file was retained (%v)", err)
		}
	})
	t.Run("an oversized notes file retains nothing", func(t *testing.T) {
		_, logDir := runBuildLeavingNotes(t, func(session string) {
			writeFile(t, filepath.Join(session, "handoff-notes.md"), notesMarker+strings.Repeat("x", 17<<10))
		})
		if _, err := os.Stat(filepath.Join(logDir, evidence.AgentNotesFileName)); !os.IsNotExist(err) {
			t.Errorf("a 17 KiB notes file was retained (%v)", err)
		}
	})
}

func TestRetainAgentNotesRefusesALinkedParent(t *testing.T) {
	ws := t.TempDir()
	elsewhere := t.TempDir()
	writeFile(t, filepath.Join(elsewhere, "handoff-notes.md"), notesMarker)
	if err := os.Symlink(elsewhere, filepath.Join(ws, buildSessionDir)); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), evidence.AgentNotesFileName)
	if ok, err := evidence.RetainAgentNotes(ws, dst); ok || err == nil {
		t.Errorf("RetainAgentNotes through a linked session folder = %v, %v, want a refusal", ok, err)
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Errorf("notes were retained through a linked parent (%v)", err)
	}
}

// Only the handoff reads the retained notes: the file's name appears in the
// evidence package that copies it, in the handoff that parses it and in the
// Activity that calls the copy, nowhere else.
func TestOnlyTheHandoffReadsTheAgentsNotes(t *testing.T) {
	seen := 0
	err := filepath.WalkDir("..", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		text := string(data)
		if !strings.Contains(text, "AgentNotesFileName") && !strings.Contains(text, "agent-notes.md") &&
			!strings.Contains(text, "handoff-notes.md") && !strings.Contains(text, "ReadRetainedAgentNotes") && !strings.Contains(text, "RetainAgentNotes") {
			return nil
		}
		seen++
		dir := filepath.Dir(path)
		okDir := dir == "../evidence" || dir == "../handoff" || path == "../workflow/activity_handoff.go"
		if !okDir {
			t.Errorf("%s names the build agent's notes: only internal/evidence, internal/handoff and activity_handoff.go may", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if seen < 3 {
		t.Errorf("found the notes in %d files, want the three that carry them", seen)
	}
}

func TestBuildLaunchCarriesItsDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	for name, tc := range map[string]struct {
		ctx  context.Context
		want bool
	}{"build launch": {forBuildLaunch(ctx), true}, "review launch": {ctx, false}} {
		t.Run(name, func(t *testing.T) {
			rt := &sandboxtest.WorkerRuntime{Lines: []string{"ok"}}
			activities, input, logPath := runtimeActivities(t, rt)
			before := time.Now()
			if _, err := activities.runSandboxWithRetries(tc.ctx, input, logPath, 1, nil, nil, nil, nil, nil, "", "", []string{"K=V"}, nil, "sh", "-c", "true"); err != nil {
				t.Fatal(err)
			}
			reqs := rt.Requests()
			if len(reqs) != 1 {
				t.Fatalf("launches = %d, want 1", len(reqs))
			}
			var got string
			for _, e := range reqs[0].Environment {
				if v, ok := strings.CutPrefix(e, "FACTORY_BUILD_DEADLINE_EPOCH="); ok {
					got = v
				}
			}
			if !tc.want {
				if got != "" {
					t.Errorf("a non-build launch carries FACTORY_BUILD_DEADLINE_EPOCH=%s", got)
				}
				return
			}
			epoch, err := strconv.ParseInt(got, 10, 64)
			if err != nil {
				t.Fatalf("FACTORY_BUILD_DEADLINE_EPOCH = %q, want integer epoch seconds", got)
			}
			// The launch's timeout is the 30 minutes minus the teardown
			// margin: the deadline lies after now and no later than 30 min on.
			if d := time.Unix(epoch, 0); !d.After(before) || d.After(before.Add(30*time.Minute+time.Second)) {
				t.Errorf("deadline %v not within the 30 minute launch from %v", d, before)
			}
		})
	}
	data, err := os.ReadFile("activities_build.go")
	if err != nil || !strings.Contains(string(data), "forBuildLaunch(runCtx)") {
		t.Errorf("the build Activity does not mark its launch as the build's (%v)", err)
	}
	review, err := os.ReadFile("activities_review.go")
	if err != nil || strings.Contains(string(review), "forBuildLaunch") {
		t.Errorf("the review Activity marks its launch as the build's (%v)", err)
	}
}
