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
	"buildgate/internal/reviewstep"
	"buildgate/internal/runner"
	"buildgate/internal/sandbox"
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

// The notes a build left are in the run directory once the build step has
// returned, whether the build passed or not, and nowhere in the worktree a
// review explores: no review launch mounts the notes file, the run directory
// or the data directory, and the worktree it does mount holds no session
// folder.
func TestAReviewLaunchCannotSeeTheBuildAgentsNotes(t *testing.T) {
	rt := &sandboxtest.WorkerRuntime{Lines: []string{"ok"}}
	activities, input, _ := runtimeActivities(t, rt)
	routed := testRoutedActivities(sandbox.RouteSecret{})
	routed.LogDir, routed.DataDir, routed.Sandboxes = activities.LogDir, activities.DataDir, rt
	routed.MeterLedgerRoot, routed.SandboxDocker = activities.MeterLedgerRoot, activities.SandboxDocker
	input.SpecPath = ""
	input.BuildAppScript = realScripts(t)
	input.BuildAppInterpreter = "python3"
	input.RoutePolicy = testRelayPolicy()
	input.RoutePolicy.AllowUnauthenticatedUpstream = true
	input.RoutePolicy.Upstream = "http://127.0.0.1:8080"
	input.RoutePolicy.AllowPlaintextUpstream = true
	input.RoutePolicy.AllowedPathPrefix = "/v1/chat/completions"
	input.RoutePolicy.UsageFormat = "openai"
	input.RoutePolicy.WorkerModelID = "qwen"
	input.RoutePolicy.WorkerBasePath = "/v1"
	// The notes of a build that passed are where the host copy puts them.
	notes := filepath.Join(routed.LogDir, evidence.AgentNotesFileName)
	writeFile(t, notes, "Things worth knowing about this repository\n- "+notesMarker+"\n")
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(routed.RunReviewStepActivity)
	_, err := env.ExecuteActivity(routed.RunReviewStepActivity, ReviewStepInput{RunWorkflowInput: input, Step: reviewstep.Combined})
	if len(rt.Requests()) != 1 {
		t.Fatalf("the review did not launch (%v)", err)
	}
	if rel, err := filepath.Rel(input.WorkspacePath, routed.LogDir); err == nil && !strings.HasPrefix(rel, "..") {
		t.Fatalf("the run directory %s is inside the workspace %s: the mounts would carry it", routed.LogDir, input.WorkspacePath)
	}
	req := rt.Requests()[0]
	paths := append([]string{}, req.ReadOnlyPaths...)
	paths = append(paths, req.ReadWritePaths...)
	for _, m := range req.Mounts {
		paths = append(paths, m.Source)
	}
	for _, p := range paths {
		if p == "" {
			continue
		}
		abs, err := filepath.Abs(p)
		if err != nil {
			continue
		}
		// Neither the notes file nor a folder that holds it (the run
		// directory, the data directory).
		if holds, _ := filepath.Rel(abs, notes); !strings.HasPrefix(holds, "..") {
			t.Errorf("the review launch mounts %s, which is or holds the build agent's notes", p)
		}
		if found := holdingMarker(t, abs, notesMarker); len(found) != 0 {
			t.Errorf("the review launch mounts %s, which holds the build agent's notes: %v", p, found)
		}
	}
	for _, name := range []string{buildSessionDir, filepath.Join(buildSessionDir, "handoff-notes.md")} {
		if _, err := os.Lstat(filepath.Join(input.WorkspacePath, name)); !os.IsNotExist(err) {
			t.Errorf("the worktree the review mounts holds %s (%v)", name, err)
		}
	}
	for _, value := range req.Environment {
		if strings.Contains(value, notesMarker) || strings.Contains(value, evidence.AgentNotesFileName) {
			t.Errorf("the review launch's environment names the notes: %s", value)
		}
	}
}

// A build that passed leaves its notes the same way as one that did not.
func TestRunBuildActivityCarriesTheNotesOfABuildThatPassedOut(t *testing.T) {
	repo := testfixture.NewGitRepo(t)
	logDir := t.TempDir()
	activities := &Activities{
		LogDir: logDir,
		runWithRetries: func(_ context.Context, _ string, _ func(int) string, _ int, _ func(int, runner.Result, error), _ string, _ ...string) (runner.Result, error) {
			writeFile(t, filepath.Join(repo, buildSessionDir, "handoff-notes.md"), "Things worth knowing about this repository\n- "+notesMarker+"\n")
			return runner.Result{ExitCode: 0}, nil
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
	if got, err := os.ReadFile(filepath.Join(logDir, evidence.AgentNotesFileName)); err != nil || !strings.Contains(string(got), notesMarker) {
		t.Errorf("retained notes = %q, %v, want the reply copied out", got, err)
	}
	if _, err := os.Lstat(filepath.Join(repo, buildSessionDir)); !os.IsNotExist(err) {
		t.Errorf("the session folder is still in the worktree (%v)", err)
	}
}

func TestRunBuildActivityRetainsNothingFromASessionFolderThatIsALink(t *testing.T) {
	repo := testfixture.NewGitRepo(t)
	logDir := t.TempDir()
	writeFile(t, filepath.Join(repo, "docs", "handoff-notes.md"), notesMarker)
	activities := &Activities{
		LogDir: logDir,
		runWithRetries: func(_ context.Context, _ string, _ func(int) string, _ int, _ func(int, runner.Result, error), _ string, _ ...string) (runner.Result, error) {
			if err := os.Symlink("docs", filepath.Join(repo, buildSessionDir)); err != nil {
				t.Fatal(err)
			}
			return runner.Result{ExitCode: 1}, nil
		},
	}
	// A notes file of an earlier attempt must not survive either.
	writeFile(t, filepath.Join(logDir, evidence.AgentNotesFileName), "an earlier attempt")
	input := fixtureInput()
	input.WorkspacePath = repo
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(activities.RunBuildActivity)
	_, err := env.ExecuteActivity(activities.RunBuildActivity, input)
	if err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("RunBuildActivity err = %v, want the step to fail as the session is not a plain directory", err)
	}
	if _, statErr := os.Stat(filepath.Join(logDir, evidence.AgentNotesFileName)); !os.IsNotExist(statErr) {
		t.Errorf("notes were retained through a linked session folder (%v)", statErr)
	}
}

func TestRetainAgentNotesRemovesWhatAnEarlierAttemptLeftWheneverItRetainsNothing(t *testing.T) {
	setup := func(t *testing.T) (ws, dst string) {
		ws = t.TempDir()
		dst = filepath.Join(t.TempDir(), evidence.AgentNotesFileName)
		writeFile(t, dst, "EARLIER-ATTEMPT")
		return ws, dst
	}
	t.Run("no source", func(t *testing.T) {
		ws, dst := setup(t)
		if ok, err := evidence.RetainAgentNotes(ws, dst); ok || err != nil {
			t.Errorf("= %v, %v, want false, nil", ok, err)
		}
		if _, err := os.Stat(dst); !os.IsNotExist(err) {
			t.Errorf("the earlier attempt's notes remain (%v)", err)
		}
	})
	t.Run("symlink source", func(t *testing.T) {
		ws, dst := setup(t)
		outside := filepath.Join(t.TempDir(), "x.md")
		writeFile(t, outside, notesMarker)
		writeFile(t, filepath.Join(t.TempDir(), "unused"), "")
		if err := os.MkdirAll(filepath.Join(ws, buildSessionDir), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(ws, buildSessionDir, "handoff-notes.md")); err != nil {
			t.Fatal(err)
		}
		if ok, err := evidence.RetainAgentNotes(ws, dst); ok || err == nil {
			t.Errorf("= %v, %v, want false and an error", ok, err)
		}
		if _, err := os.Stat(dst); !os.IsNotExist(err) {
			t.Errorf("the earlier attempt's notes remain (%v)", err)
		}
	})
	t.Run("valid source", func(t *testing.T) {
		ws, dst := setup(t)
		writeFile(t, filepath.Join(ws, buildSessionDir, "handoff-notes.md"), "NEW")
		if ok, err := evidence.RetainAgentNotes(ws, dst); !ok || err != nil {
			t.Fatalf("= %v, %v, want true, nil", ok, err)
		}
		if got, _ := os.ReadFile(dst); string(got) != "NEW" {
			t.Errorf("notes = %q, want NEW", got)
		}
	})
}

func TestTheNotesFileCapAgreesWithWhatTheScriptWrites(t *testing.T) {
	// build_app.py writes at most 12,000 bytes (HANDOFF_NOTES_MAX_BYTES);
	// the host keeps up to 16 KiB.
	ws := t.TempDir()
	dst := filepath.Join(t.TempDir(), evidence.AgentNotesFileName)
	writeFile(t, filepath.Join(ws, buildSessionDir, "handoff-notes.md"), strings.Repeat("a", 12_000+1))
	if ok, err := evidence.RetainAgentNotes(ws, dst); !ok || err != nil {
		t.Errorf("a 12,001 byte file = %v, %v, want retained", ok, err)
	}
	writeFile(t, filepath.Join(ws, buildSessionDir, "handoff-notes.md"), strings.Repeat("a", 16<<10+1))
	if ok, err := evidence.RetainAgentNotes(ws, dst); ok || err == nil {
		t.Errorf("a file over 16 KiB = %v, %v, want refused", ok, err)
	}
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
	// internal/, cmd/ and scripts/ (the repository's other code). logs_cmd.go
	// names the file only to skip it.
	for _, root := range []string{"..", "../../cmd", "../../scripts"} {
		seen += scanForAgentNotes(t, root)
	}
	if seen < 4 {
		t.Errorf("found the notes in %d files, want the four that carry them", seen)
	}
}

func scanForAgentNotes(t *testing.T, root string) (seen int) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == "node_modules" || d.Name() == "testdata") {
			return fs.SkipDir
		}
		if d.IsDir() || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		if !strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, ".py") && !strings.HasSuffix(path, ".sh") {
			return nil
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
		okDir := dir == "../evidence" || dir == "../handoff" || path == "../workflow/activity_handoff.go" ||
			path == "../../cmd/factoryd/logs_cmd.go"
		if !okDir {
			t.Errorf("%s names the build agent's notes: only internal/evidence, internal/handoff, activity_handoff.go and logs_cmd.go (which skips the file) may", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return seen
}

// launchedEnvironment returns the value of name in the environment of the
// one sandbox rt launched, and its launch timeout.
func launchedEnvironment(t *testing.T, rt *sandboxtest.WorkerRuntime, name string) (value string, found bool, timeout time.Duration) {
	t.Helper()
	reqs := rt.Requests()
	if len(reqs) != 1 {
		t.Fatalf("launches = %d, want 1", len(reqs))
	}
	for _, e := range reqs[0].Environment {
		if v, ok := strings.CutPrefix(e, name+"="); ok {
			value, found = v, true
		}
	}
	return value, found, reqs[0].Timeout
}

// realScripts is the harness script the launches stage, from this checkout.
func realScripts(t *testing.T) string {
	t.Helper()
	path, err := filepath.Abs("../../agent/pi/scripts/build_app.py")
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func TestTheBuildLaunchCarriesItsTimeBudgetAndAReviewLaunchDoesNot(t *testing.T) {
	t.Run("build", func(t *testing.T) {
		rt := &sandboxtest.WorkerRuntime{Lines: []string{"ok"}}
		activities, input, _ := runtimeActivities(t, rt)
		input.SpecPath = ""
		input.BuildAppScript = realScripts(t)
		input.BuildAppInterpreter = "python3"
		var suite testsuite.WorkflowTestSuite
		env := suite.NewTestActivityEnvironment()
		env.RegisterActivity(activities.RunBuildActivity)
		if _, err := env.ExecuteActivity(activities.RunBuildActivity, input); err != nil {
			t.Fatalf("RunBuildActivity: %v", err)
		}
		got, found, timeout := launchedEnvironment(t, rt, BuildTimeBudgetEnv)
		if !found {
			t.Fatalf("the build launch carries no %s", BuildTimeBudgetEnv)
		}
		if BuildTimeBudgetEnv != "FACTORY_BUILD_TIME_BUDGET_SECONDS" {
			t.Errorf("variable name = %q", BuildTimeBudgetEnv)
		}
		if want := strconv.FormatInt(int64(timeout/time.Second), 10); got != want || timeout <= 0 {
			t.Errorf("%s = %q, want the launch timeout %v in whole seconds (%s)", BuildTimeBudgetEnv, got, timeout, want)
		}
	})
	t.Run("review", func(t *testing.T) {
		rt := &sandboxtest.WorkerRuntime{Lines: []string{"ok"}}
		activities, input, _ := runtimeActivities(t, rt)
		routed := testRoutedActivities(sandbox.RouteSecret{})
		routed.LogDir, routed.DataDir, routed.Sandboxes = activities.LogDir, activities.DataDir, rt
		routed.MeterLedgerRoot, routed.SandboxDocker = activities.MeterLedgerRoot, activities.SandboxDocker
		input.SpecPath = ""
		input.BuildAppScript = realScripts(t)
		input.BuildAppInterpreter = "python3"
		input.RoutePolicy = testRelayPolicy()
		input.RoutePolicy.AllowUnauthenticatedUpstream = true
		input.RoutePolicy.Upstream = "http://127.0.0.1:8080"
		input.RoutePolicy.AllowPlaintextUpstream = true
		input.RoutePolicy.AllowedPathPrefix = "/v1/chat/completions"
		input.RoutePolicy.UsageFormat = "openai"
		input.RoutePolicy.WorkerModelID = "qwen"
		input.RoutePolicy.WorkerBasePath = "/v1"
		var suite testsuite.WorkflowTestSuite
		env := suite.NewTestActivityEnvironment()
		env.RegisterActivity(routed.RunReviewStepActivity)
		_, err := env.ExecuteActivity(routed.RunReviewStepActivity, ReviewStepInput{RunWorkflowInput: input, Step: reviewstep.Combined})
		if len(rt.Requests()) != 1 {
			t.Fatalf("the review did not launch (%v)", err)
		}
		if got, found, _ := launchedEnvironment(t, rt, BuildTimeBudgetEnv); found {
			t.Errorf("a review launch carries %s=%s", BuildTimeBudgetEnv, got)
		}
	})
}
