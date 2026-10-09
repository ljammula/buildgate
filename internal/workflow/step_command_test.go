package workflow

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.temporal.io/sdk/testsuite"

	"buildgate/internal/codereview"
	"buildgate/internal/runner"
	"buildgate/internal/sandbox"
	"buildgate/internal/testfixture"
)

func TestStepCommandWithoutSetupIsUnchanged(t *testing.T) {
	for _, setup := range [][]string{nil, {}} {
		got := stepCommand(setup, "go test ./... && echo 'ok'")
		want := []string{"sh", "-c", "go test ./... && echo 'ok'"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("stepCommand(%v) = %q, want %q", setup, got, want)
		}
	}
	argv := []string{"python3", "build_app.py", "--workspace", "/w"}
	if got := wrapWithSetup(nil, argv); !reflect.DeepEqual(got, argv) {
		t.Errorf("wrapWithSetup(nil) = %q, want the argv unchanged", got)
	}
}

func TestStepCommandPassesRepositoryTextAsArgumentsOnly(t *testing.T) {
	hostile := `echo "a'b"; $(touch /x) ` + "`id`"
	got := stepCommand([]string{hostile, "echo two"}, "make verify")
	const script = `c=$1; shift; for s; do sh -c "$s" || { echo "buildgate: setup failed: $s" >&2; exit 95; }; done; exec sh -c "$c"`
	want := []string{"sh", "-c", script, "buildgate-setup", "make verify", hostile, "echo two"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("stepCommand = %q, want %q", got, want)
	}
	for i, el := range got {
		if i != 2 && strings.Contains(el, "exec sh") {
			t.Errorf("element %d holds the script text", i)
		}
	}
	if strings.Contains(got[2], "touch") || strings.Contains(got[2], "make verify") {
		t.Errorf("the script element %q holds repository text", got[2])
	}
	wrapped := wrapWithSetup([]string{hostile}, []string{"python3", "build_app.py"})
	wantWrapped := []string{"sh", "-c", wrapScript, "buildgate-setup", "1", hostile, "python3", "build_app.py"}
	if !reflect.DeepEqual(wrapped, wantWrapped) {
		t.Errorf("wrapWithSetup = %q, want %q", wrapped, wantWrapped)
	}
	if strings.Contains(wrapScript, "touch") || strings.Contains(wrapScript, "python3") {
		t.Errorf("the wrap script holds repository text")
	}
}

func execStep(t *testing.T, dir string, argv []string) (int, string) {
	t.Helper()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return 0, string(out)
	case errors.As(err, &exitErr):
		return exitErr.ExitCode(), string(out)
	default:
		t.Fatalf("run %q: %v", argv, err)
		return -1, ""
	}
}

func TestStepCommandRunsSetupInOrderAndStopsAtTheFirstFailure(t *testing.T) {
	run := func(setup []string, command string) (int, string, string) {
		dir := t.TempDir()
		code, out := execStep(t, dir, stepCommand(setup, command))
		data, _ := os.ReadFile(filepath.Join(dir, "trace"))
		return code, out, string(data)
	}
	code, _, trace := run([]string{"echo one >> trace", "echo two >> trace"}, "echo command >> trace")
	if code != 0 || trace != "one\ntwo\ncommand\n" {
		t.Errorf("passing setup: exit %d, trace %q, want 0 and \"one\\ntwo\\ncommand\\n\"", code, trace)
	}
	code, out, trace := run([]string{"false", "echo two >> trace"}, "echo command >> trace")
	if code != SetupFailedExitCode || SetupFailedExitCode != 95 || trace != "" || !strings.Contains(out, "buildgate: setup failed: false") {
		t.Errorf("failing setup: exit %d, output %q, trace %q, want 95, the named command and no trace", code, out, trace)
	}
	code, _, trace = run([]string{"echo one >> trace"}, "echo command >> trace; exit 3")
	if code != 3 || trace != "one\ncommand\n" {
		t.Errorf("command exit: %d, trace %q, want 3 and both lines", code, trace)
	}
	// The build wrapper has the same semantics for an argv command.
	dir := t.TempDir()
	code, _ = execStep(t, dir, wrapWithSetup([]string{"echo one >> trace", "echo two >> trace"}, []string{"sh", "-c", "echo build >> trace; exit 4"}))
	data, _ := os.ReadFile(filepath.Join(dir, "trace"))
	if code != 4 || string(data) != "one\ntwo\nbuild\n" {
		t.Errorf("wrapped build: exit %d, trace %q", code, data)
	}
	dir = t.TempDir()
	code, out = execStep(t, dir, wrapWithSetup([]string{"echo one >> trace", "exit 1", "echo three >> trace"}, []string{"sh", "-c", "echo build >> trace"}))
	data, _ = os.ReadFile(filepath.Join(dir, "trace"))
	if code != 95 || string(data) != "one\n" || !strings.Contains(out, "buildgate: setup failed: exit 1") {
		t.Errorf("wrapped failing setup: exit %d, output %q, trace %q", code, out, data)
	}
}

// launchRecorder is an Activities whose launches are recorded, each as
// interpreter plus arguments, and succeed.
func launchRecorder(t *testing.T) (*Activities, *[][]string) {
	t.Helper()
	var launches [][]string
	a := &Activities{
		LogDir: t.TempDir(),
		runWithRetriesChecked: func(_ context.Context, _ string, logPath func(int) string, _ int, before func(int) error, after func(int, runner.Result, error) error, name string, args ...string) (runner.Result, error) {
			argv := append([]string{name}, args...)
			launches = append(launches, argv)
			if err := before(1); err != nil {
				return runner.Result{}, err
			}
			log := logPath(1)
			if err := os.MkdirAll(filepath.Dir(log), 0o750); err != nil {
				return runner.Result{}, err
			}
			if err := os.WriteFile(log, []byte("ok\n"), 0o600); err != nil {
				return runner.Result{}, err
			}
			started := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
			res := runner.Result{Command: argv, StartedAt: started, FinishedAt: started.Add(time.Second), LogPath: log}
			return res, after(1, res, nil)
		},
	}
	return a, &launches
}

func execActivity(t *testing.T, fn any, input any) {
	t.Helper()
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(fn)
	if _, err := env.ExecuteActivity(fn, input); err != nil {
		t.Fatalf("execute Activity: %v", err)
	}
}

func isSetupForm(argv []string, command string, setup []string) bool {
	return reflect.DeepEqual(argv, stepCommand(setup, command))
}

func TestEveryVerifyClassLaunchRunsSetupFirst(t *testing.T) {
	setups := [][]string{{"echo one"}, nil}
	for _, setup := range setups {
		check := func(name string, launches [][]string, command string) {
			t.Helper()
			if len(launches) != 1 {
				t.Errorf("%s: %d launches, want 1", name, len(launches))
				return
			}
			if !isSetupForm(launches[0], command, setup) {
				t.Errorf("%s with setup %v: argv %q, want %q", name, setup, launches[0], stepCommand(setup, command))
			}
			if len(setup) == 0 && !reflect.DeepEqual(launches[0], []string{"sh", "-c", command}) {
				t.Errorf("%s without setup: argv %q is not the plain form", name, launches[0])
			}
			if len(setup) > 0 && (launches[0][3] != "buildgate-setup" || launches[0][len(launches[0])-1] != "echo one") {
				t.Errorf("%s: argv %q is not the setup form carrying echo one", name, launches[0])
			}
		}
		base := func() RunWorkflowInput {
			in := fixtureInput()
			in.VerifyCommand = "make verify"
			in.FullSuiteCommand = "make full"
			in.SetupCommands = setup
			return in
		}

		a, l := launchRecorder(t)
		execActivity(t, a.RunVerifyActivity, base())
		check("canonical verify", *l, "make verify")

		a, l = launchRecorder(t)
		execActivity(t, a.RunFullSuiteVerifyActivity, base())
		check("full suite", *l, "make full")

		for _, gate := range []string{"lint", "repo-licenses"} {
			a, l = launchRecorder(t)
			execActivity(t, a.RunNamedGateActivity, NamedGateActivityInput{RunWorkflowInput: base(), Check: gate, Command: "make " + gate})
			check("gate "+gate, *l, "make "+gate)
		}

		a, l = launchRecorder(t)
		bin := base()
		bin.WorkspacePath = baselineRepo(t)
		execActivity(t, a.RunBaselineVerifyActivity, bin)
		check("baseline verify", *l, "make verify")

		pa, pl, in := postSetupFixture(t, setup)
		_ = pa
		if _, err := execPostVerify(t, pa, in, 1); err != nil {
			t.Fatal(err)
		}
		if len(*pl) != 2 {
			t.Fatalf("post-oracle-commit reruns: %d launches, want 2", len(*pl))
		}
		check("post-oracle-commit verify", (*pl)[:1], "make verify")
		check("post-oracle-commit full suite", (*pl)[1:], "make full")

		_, dockerArgs, _ := canaryGateWithSetup(t, map[string]string{"oracle_test.go": canaryTestOracleSrc}, 0, 1, "", setup)
		if has := strings.Contains(dockerArgs, "buildgate-setup") && strings.Contains(dockerArgs, "echo one"); has != (len(setup) > 0) {
			t.Errorf("oracle canary with setup %v: docker args carry the setup form = %v:\n%s", setup, has, dockerArgs)
		}
		if n := strings.Count(dockerArgs, "buildgate-setup"); len(setup) > 0 && n < 2 {
			t.Errorf("oracle canary: %d launches carry the setup form, want both the real run and the canary", n)
		}
	}
}

func postSetupFixture(t *testing.T, setup []string) (*Activities, *[][]string, RunWorkflowInput) {
	t.Helper()
	a, l := launchRecorder(t)
	_, input, _ := postVerifyFixture(t, func(string) int { return 0 }, nil)
	a.LogDir = input.LogDir
	input.SetupCommands = setup
	return a, l, input
}

func TestReviewLaunchRunsNoSetup(t *testing.T) {
	workspacePath := testfixture.NewGitRepo(t)
	activities := testRoutedActivities(sandbox.NewRouteSecret("worker-static-upstream-key"))
	rec, launches := launchRecorder(t)
	activities.LogDir = rec.LogDir
	activities.runWithRetriesChecked = rec.runWithRetriesChecked
	input := codeReviewFixtureInput(t, workspacePath)
	input.SetupCommands = []string{"echo one"}
	input.CodeReviewPolicy = codereview.PolicyAdvisory
	execActivity(t, func(ctx context.Context, in ReviewStepInput) (VerifyActivityResult, error) {
		return activities.RunReviewStepActivity(ctx, in)
	}, input)
	if len(*launches) != 1 {
		t.Fatalf("%d review launches, want 1", len(*launches))
	}
	for _, el := range (*launches)[0] {
		if el == "buildgate-setup" || el == "echo one" {
			t.Errorf("review argv %q carries setup", (*launches)[0])
		}
	}
	if (*launches)[0][0] == "sh" {
		t.Errorf("review argv %q is a shell step", (*launches)[0])
	}
}

func TestBuildLaunchRunsSetupAndPassesItToTheBuildScript(t *testing.T) {
	a, launches := launchRecorder(t)
	input := fixtureInput()
	input.VerifyCommand = "make verify"
	input.SetupCommands = []string{"echo one"}
	execActivity(t, a.RunBuildActivity, input)
	if len(*launches) != 1 {
		t.Fatalf("%d build launches, want 1", len(*launches))
	}
	argv := (*launches)[0]
	if len(argv) < 7 || !reflect.DeepEqual(argv[:6], []string{"sh", "-c", wrapScript, "buildgate-setup", "1", "echo one"}) {
		t.Fatalf("build argv %q does not start with the setup wrapper", argv)
	}
	rest := strings.Join(argv[6:], "\x00")
	if !strings.Contains(rest, "\x00--setup-command\x00echo one") {
		t.Errorf("the build script's arguments %q lack --setup-command echo one", argv[6:])
	}

	a, launches = launchRecorder(t)
	input.SetupCommands = nil
	execActivity(t, a.RunBuildActivity, input)
	if got := (*launches)[0]; got[0] == "sh" && len(got) > 3 && got[3] == "buildgate-setup" || strings.Contains(strings.Join(got, " "), "--setup-command") {
		t.Errorf("a build without setup commands carries setup: %q", got)
	}
}

func TestBuildActivityArgsWithoutSetupAddsNothing(t *testing.T) {
	without := buildActivityArgs("/b.py", "/ws", "/s.md", "required", 3, 45, "sha", "make verify", "", "", "", "", "pi")
	with := buildActivityArgs("/b.py", "/ws", "/s.md", "required", 3, 45, "sha", "make verify", "", "", "", "", "pi", "echo one", "echo two")
	if strings.Contains(strings.Join(without, " "), "--setup-command") {
		t.Errorf("args %q carry --setup-command", without)
	}
	if len(with) != len(without)+4 {
		t.Fatalf("with setup %q, without %q", with, without)
	}
}
