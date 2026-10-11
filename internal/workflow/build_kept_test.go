package workflow

import (
	"os"
	"path/filepath"
	"testing"

	"buildgate/internal/evidence"
	"buildgate/internal/run"
	"buildgate/internal/sandbox"
	"buildgate/internal/sandbox/sandboxtest"

	"go.temporal.io/sdk/testsuite"
)

// keptBuildFixture is a run that adopted the worktree of an earlier run,
// "earlier", whose record says its build finished at the worktree's HEAD.
type keptBuildFixture struct {
	t       *testing.T
	acts    *Activities
	input   RunWorkflowInput
	creates int
	head    string
}

func newKeptBuildFixture(t *testing.T) *keptBuildFixture {
	t.Helper()
	rt := &inspectRuntime{WorkerRuntime: &sandboxtest.WorkerRuntime{Lines: []string{"built"}}}
	acts, input, _ := runtimeActivities(t, rt.WorkerRuntime)
	acts.Sandboxes = rt
	f := &keptBuildFixture{t: t, acts: acts, input: input}
	rt.onCreate = func(sandbox.SandboxRequest) { f.creates++ }
	writeFile(t, filepath.Join(input.WorkspacePath, "main.go"), "package main\n")
	gitIn(t, input.WorkspacePath, "add", "-A")
	gitIn(t, input.WorkspacePath, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "-m", "the build's work")
	f.head = gitIn(t, input.WorkspacePath, "rev-parse", "HEAD")
	f.input.ResumeFrom = &ResumeFrom{RunID: "earlier", WorktreePath: input.WorkspacePath}
	f.input.CheckpointDir = t.TempDir()
	return f
}

func (f *keptBuildFixture) saveEarlier(r *run.Run) {
	f.t.Helper()
	r.ID = "earlier"
	if err := r.Save(f.acts.dataDirFor(f.input)); err != nil {
		f.t.Fatal(err)
	}
}

func (f *keptBuildFixture) build() (BuildActivityResult, error) {
	f.t.Helper()
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(f.acts.RunBuildActivity)
	val, err := env.ExecuteActivity(f.acts.RunBuildActivity, f.input)
	if err != nil {
		return BuildActivityResult{}, err
	}
	var res BuildActivityResult
	if err := val.Get(&res); err != nil {
		f.t.Fatal(err)
	}
	return res, nil
}

// A resume after a review that gave no verdict must not ask the model to
// build finished work again: the worktree is clean at the commit the earlier
// run's build left, so the build step returns that build and launches
// nothing. Every gate still runs afterwards, on that commit.
func TestAResumedRunKeepsAFinishedBuildInsteadOfBuildingAgain(t *testing.T) {
	f := newKeptBuildFixture(t)
	f.saveEarlier(&run.Run{State: run.StateQuarantined, ResultSHA: f.head, Attempts: []run.Attempt{{Kind: "build", ExitCode: 0}, {Kind: "verify", ExitCode: 0}, {Kind: "review", ExitCode: 1}}})
	earlierEvidence := filepath.Join(run.Dir(f.acts.dataDirFor(f.input), "earlier"), evidence.BuildEvidenceFileName)
	writeFile(t, earlierEvidence, `{"rounds":[{"round":1}]}`)
	res, err := f.build()
	if err != nil {
		t.Fatalf("RunBuildActivity: %v", err)
	}
	if f.creates != 0 {
		t.Errorf("sandboxes created = %d, want none: the build was finished", f.creates)
	}
	if res.Result.ExitCode != 0 || len(res.Attempts) != 1 || res.Attempts[0].KeptFromRun != "earlier" || res.Attempts[0].Kind != "build" {
		t.Errorf("result = %+v, want one passed build attempt kept from the earlier run", res)
	}
	if got, err := os.ReadFile(filepath.Join(f.acts.logDirFor(f.input), evidence.BuildEvidenceFileName)); err != nil || string(got) != `{"rounds":[{"round":1}]}` {
		t.Errorf("the kept build's evidence in the resumed run's directory = %q, %v", got, err)
	}
}

// Anything short of "finished, and the worktree still stands at it" builds:
// each case is read by the host from the record and from git.
func TestAResumedRunBuildsWhenTheEarlierBuildIsNotKnownToStand(t *testing.T) {
	for name, arrange := range map[string]func(f *keptBuildFixture){
		"the earlier build did not pass": func(f *keptBuildFixture) {
			f.saveEarlier(&run.Run{State: run.StateHalted, ResultSHA: f.head, Attempts: []run.Attempt{{Kind: "build", ExitCode: 1}}})
		},
		"the earlier run recorded no result commit": func(f *keptBuildFixture) {
			f.saveEarlier(&run.Run{State: run.StateHalted, Attempts: []run.Attempt{{Kind: "build", ExitCode: 0}}})
		},
		"the earlier run never built": func(f *keptBuildFixture) {
			f.saveEarlier(&run.Run{State: run.StateHalted, ResultSHA: f.head, Attempts: []run.Attempt{{Kind: "baseline_verify", ExitCode: -1}}})
		},
		"the worktree has changes": func(f *keptBuildFixture) {
			f.saveEarlier(&run.Run{State: run.StateQuarantined, ResultSHA: f.head, Attempts: []run.Attempt{{Kind: "build", ExitCode: 0}}})
			writeFile(f.t, filepath.Join(f.input.WorkspacePath, "main.go"), "package main // edited\n")
		},
		"the worktree is at another commit": func(f *keptBuildFixture) {
			f.saveEarlier(&run.Run{State: run.StateQuarantined, ResultSHA: "0000000000000000000000000000000000000000", Attempts: []run.Attempt{{Kind: "build", ExitCode: 0}}})
		},
		"there is no record of the earlier run": func(*keptBuildFixture) {},
	} {
		t.Run(name, func(t *testing.T) {
			f := newKeptBuildFixture(t)
			arrange(f)
			if _, ok := finishedBuildToKeep(f.acts.dataDirFor(f.input), f.input); ok {
				t.Error("the build was kept")
			}
		})
	}
	// A run that is not a resume never looks.
	f := newKeptBuildFixture(t)
	f.saveEarlier(&run.Run{State: run.StateQuarantined, ResultSHA: f.head, Attempts: []run.Attempt{{Kind: "build", ExitCode: 0}}})
	f.input.ResumeFrom = nil
	if _, ok := finishedBuildToKeep(f.acts.dataDirFor(f.input), f.input); ok {
		t.Error("a run that adopted nothing kept a build")
	}
}
