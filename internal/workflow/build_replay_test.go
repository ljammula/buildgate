package workflow

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"

	"buildgate/internal/meter"
	"buildgate/internal/run"
	"buildgate/internal/sandbox"
	"buildgate/internal/sandbox/sandboxtest"
)

// replayActivityFixture is a replay run's worktree, at a commit made on top
// of the commit the replay is onto, and the accepted, sandboxed run it was
// replayed from ("source").
type replayActivityFixture struct {
	t       *testing.T
	acts    *Activities
	input   RunWorkflowInput
	creates int
}

func newReplayActivityFixture(t *testing.T) *replayActivityFixture {
	t.Helper()
	rt := &inspectRuntime{WorkerRuntime: &sandboxtest.WorkerRuntime{Lines: []string{"built"}}}
	acts, input, _ := runtimeActivities(t, rt.WorkerRuntime)
	acts.Sandboxes = rt
	f := &replayActivityFixture{t: t, acts: acts, input: input}
	rt.onCreate = func(sandbox.SandboxRequest) { f.creates++ }
	onto := gitIn(t, input.WorkspacePath, "rev-parse", "HEAD")
	writeFile(t, filepath.Join(input.WorkspacePath, "main.go"), "package main\n")
	gitIn(t, input.WorkspacePath, "add", "-A")
	gitIn(t, input.WorkspacePath, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "-m", "the replayed commit")
	head := gitIn(t, input.WorkspacePath, "rev-parse", "HEAD")
	f.input.IsolatedRepoDir = input.WorkspacePath
	f.input.OnBranch = "buildgate/req-1/t2-restack1"
	f.input.DiffBaseSHA = onto
	f.input.Replay = &ReplayInput{OfRunID: "source", SourceRunIDs: []string{"source"}, OntoSHA: onto, HeadSHA: head}
	f.saveSource(&run.Run{State: run.StateAccepted})
	return f
}

// saveSource saves r as the source run, in the fixture's repository and with
// a sandboxed build attempt unless r says otherwise.
func (f *replayActivityFixture) saveSource(r *run.Run) {
	f.t.Helper()
	if r.ID == "" {
		r.ID = "source"
	}
	if r.ProjectPath == "" {
		r.ProjectPath = f.input.WorkspacePath
	}
	if r.Attempts == nil {
		r.Attempts = []run.Attempt{{Kind: "build", ExitCode: 0, ImageDigest: "sha256:worker", Harness: "codex", RelayRoute: "codex", RelayConsumedCostMicroUSD: 70, LogPath: "/source/build.log", RelayCalls: &meter.Calls{Answered: 9}}}
	}
	if err := r.Save(f.acts.dataDirFor(f.input)); err != nil {
		f.t.Fatal(err)
	}
}

func (f *replayActivityFixture) apply() (BuildActivityResult, error) {
	f.t.Helper()
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(f.acts.ApplyReplayActivity)
	val, err := env.ExecuteActivity(f.acts.ApplyReplayActivity, f.input)
	if err != nil {
		return BuildActivityResult{}, err
	}
	var res BuildActivityResult
	if err := val.Get(&res); err != nil {
		f.t.Fatal(err)
	}
	return res, nil
}

// TestApplyReplayCarriesTheSourceBuildAndLaunchesNothing: the replayed commit
// passes the host's checks, so the step returns one passed build attempt
// carried from the source run (its image, harness and route, none of its
// spend), marked ReplayOf, and no sandbox is created, however often it runs.
func TestApplyReplayCarriesTheSourceBuildAndLaunchesNothing(t *testing.T) {
	f := newReplayActivityFixture(t)
	for range 2 {
		res, err := f.apply()
		if err != nil {
			t.Fatalf("ApplyReplayActivity: %v", err)
		}
		if res.Result.ExitCode != 0 || len(res.Attempts) != 1 {
			t.Fatalf("result = %+v, want one passed attempt", res)
		}
		a := res.Attempts[0]
		if a.Kind != "build" || a.ReplayOf != "source" || a.ImageDigest != "sha256:worker" || a.Harness != "codex" || a.RelayRoute != "codex" {
			t.Errorf("attempt = %+v, want the source's build marked ReplayOf", a)
		}
		if a.RelayConsumedCostMicroUSD != 0 || a.RelayCalls != nil || a.LogPath != "" || a.KeptFromRun != "" {
			t.Errorf("attempt carries the source's spend, log or a kept mark: %+v", a)
		}
	}
	if f.creates != 0 {
		t.Errorf("sandboxes created = %d, want none", f.creates)
	}
}

// TestApplyReplayRefuses: each check the host makes, missed, refuses the
// replay with ReplayFailureType and launches nothing; a source run that
// lacks what the check reads never passes.
func TestApplyReplayRefuses(t *testing.T) {
	for name, arrange := range map[string]func(f *replayActivityFixture){
		"a dirty worktree": func(f *replayActivityFixture) {
			writeFile(t, filepath.Join(f.input.WorkspacePath, "stray.txt"), "x\n")
		},
		"HEAD is not the replayed head": func(f *replayActivityFixture) {
			f.input.Replay.HeadSHA = f.input.Replay.OntoSHA
		},
		"the base is not under HEAD": func(f *replayActivityFixture) {
			gitIn(t, f.input.WorkspacePath, "checkout", "-q", "--orphan", "other")
			gitIn(t, f.input.WorkspacePath, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "-m", "unrelated")
			f.input.Replay.HeadSHA = gitIn(t, f.input.WorkspacePath, "rev-parse", "HEAD")
		},
		"the source is missing": func(f *replayActivityFixture) {
			f.input.Replay.OfRunID, f.input.Replay.SourceRunIDs = "gone", []string{"gone"}
		},
		"the source is not accepted": func(f *replayActivityFixture) {
			f.saveSource(&run.Run{State: run.StateQuarantined})
		},
		"the source was not sandboxed": func(f *replayActivityFixture) {
			f.saveSource(&run.Run{State: run.StateAccepted, Attempts: []run.Attempt{{Kind: "build", ExitCode: 0}}})
		},
		"the source has no build attempt": func(f *replayActivityFixture) {
			f.saveSource(&run.Run{State: run.StateAccepted, Attempts: []run.Attempt{{Kind: "verify", ImageDigest: "sha256:worker"}}})
		},
		"the source is another repository's": func(f *replayActivityFixture) {
			f.saveSource(&run.Run{State: run.StateAccepted, ProjectPath: t.TempDir()})
		},
		"a second source is not accepted": func(f *replayActivityFixture) {
			f.saveSource(&run.Run{ID: "second", State: run.StateHalted})
			f.input.Replay.SourceRunIDs = append(f.input.Replay.SourceRunIDs, "second")
		},
		"the run replayed is not a source": func(f *replayActivityFixture) {
			f.saveSource(&run.Run{ID: "second", State: run.StateAccepted})
			f.input.Replay.SourceRunIDs = []string{"second"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newReplayActivityFixture(t)
			arrange(f)
			_, err := f.apply()
			var appErr *temporal.ApplicationError
			if !errors.As(err, &appErr) || appErr.Type() != ReplayFailureType || !appErr.NonRetryable() {
				t.Fatalf("error = %v, want a non-retryable %s", err, ReplayFailureType)
			}
			if !strings.Contains(err.Error(), "replay run refused") {
				t.Errorf("error = %v, want it to say the replay was refused", err)
			}
			if f.creates != 0 {
				t.Errorf("sandboxes created = %d, want none", f.creates)
			}
		})
	}
}

// TestBaselineVerifyOfAReplayRunLaunchesNothing: a replay run takes no
// baseline on the new base: with no record of the source's to carry, the
// step records none and creates no sandbox.
func TestBaselineVerifyOfAReplayRunLaunchesNothing(t *testing.T) {
	f := newReplayActivityFixture(t)
	f.input.VerifyCommand = "make verify"
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(f.acts.RunBaselineVerifyActivity)
	val, err := env.ExecuteActivity(f.acts.RunBaselineVerifyActivity, f.input)
	if err != nil {
		t.Fatalf("RunBaselineVerifyActivity: %v", err)
	}
	var res BaselineVerifyResult
	if err := val.Get(&res); err != nil {
		t.Fatal(err)
	}
	if f.creates != 0 || len(res.Attempts) != 0 || res.BuildNotePath != "" {
		t.Errorf("baseline of a replay run: %d sandboxes, result %+v; want nothing launched or recorded", f.creates, res)
	}
}
