package workflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	temporalworkflow "go.temporal.io/sdk/workflow"

	"buildgate/internal/reproduction"
	"buildgate/internal/run"
	"buildgate/internal/sandbox"
	"buildgate/internal/sandbox/sandboxtest"
)

const reproContent = "package p\n\nimport \"testing\"\n\nfunc TestRepro(t *testing.T) {\n\tif Add(1, 2) != 3 {\n\t\tt.Fatal(\"Add(1, 2)\")\n\t}\n}\n"

const reproFailReport = `{"Action":"run","Package":"example.com/p","Test":"TestRepro"}
{"Action":"fail","Package":"example.com/p","Test":"TestRepro","Elapsed":0}
{"Action":"fail","Package":"example.com/p","Elapsed":0.1}
`

func sha256Of(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// reproFixture is a real Go repository the review judged, its approved spec,
// and Activities that launch through the package's fake runtime.
type reproFixture struct {
	t       *testing.T
	rt      *inspectRuntime
	acts    *Activities
	input   RunWorkflowInput
	repo    string
	commit  string
	mu      sync.Mutex
	creates []sandbox.SandboxRequest
	// mounted is what each launch mounted at the reproduction's path:
	// whether read-only, and the hash of its bytes at launch.
	mounted []string
	report  string
}

func newReproFixture(t *testing.T) *reproFixture {
	t.Helper()
	rt := &inspectRuntime{WorkerRuntime: &sandboxtest.WorkerRuntime{Lines: []string{"ok"}}}
	acts, input, _ := runtimeActivities(t, rt.WorkerRuntime)
	acts.Sandboxes = rt
	f := &reproFixture{t: t, rt: rt, acts: acts, repo: input.WorkspacePath, report: reproFailReport}
	writeFile(t, filepath.Join(f.repo, "go.mod"), "module example.com/p\n\ngo 1.22\n")
	writeFile(t, filepath.Join(f.repo, "p", "add.go"), "package p\n\nfunc Add(a, b int) int { return a + b }\n")
	f.commit = trustedCommit(t, f.repo)
	input.CheckpointDir = t.TempDir()
	input.BaseSHA = f.commit
	f.input = input
	rt.onCreate = func(req sandbox.SandboxRequest) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.creates = append(f.creates, req)
		for _, m := range req.Mounts {
			switch m.Target {
			case "/workspace":
				if err := os.WriteFile(filepath.Join(m.Source, reproductionReportName), []byte(f.report), 0o644); err != nil {
					t.Errorf("write the report: %v", err)
				}
			case "/workspace/p/repro_test.go":
				body, err := os.ReadFile(m.Source)
				if err != nil {
					t.Errorf("read the mounted reproduction: %v", err)
				}
				f.mounted = append(f.mounted, map[bool]string{true: "ro ", false: "rw "}[m.ReadOnly]+sha256Of(body))
			}
		}
	}
	return f
}

// held holds one valid test file with holdOne and returns it.
func (f *reproFixture) held() run.HeldReproduction {
	f.t.Helper()
	dir := run.ReviewReproductionDir(f.acts.logDirFor(f.input))
	h, err := holdOne(dir, 0, reproduction.Accepted{Path: "p/repro_test.go", Runner: "go-test-json", TestIDs: []string{"TestRepro"}}, []byte(reproContent), true)
	if err != nil {
		f.t.Fatalf("holdOne: %v", err)
	}
	return h
}

// launch runs RunReviewReproductionActivity for one pass.
func (f *reproFixture) launch(h run.HeldReproduction, pass, commit string) (ReproductionRunResult, error) {
	f.t.Helper()
	in := f.input
	in.CheckpointDir = f.t.TempDir()
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(f.acts.RunReviewReproductionActivity)
	val, err := env.ExecuteActivity(f.acts.RunReviewReproductionActivity, ReproductionRunInput{RunWorkflowInput: in, Held: h, Pass: pass, Commit: commit})
	if err != nil {
		return ReproductionRunResult{}, err
	}
	var res ReproductionRunResult
	if err := val.Get(&res); err != nil {
		f.t.Fatal(err)
	}
	return res, nil
}

func failureType(err error) string {
	var appErr *temporal.ApplicationError
	if errors.As(err, &appErr) {
		return appErr.Type()
	}
	return ""
}

// SC-012: every launch checks the held copy against the hash the review
// recorded before it mounts it: a launch mounts exactly the held bytes,
// read-only, and a copy changed between two launches is refused before the
// second launch, as a failure to hold.
func TestReproductionHashCheckedBeforeEachMount(t *testing.T) {
	f := newReproFixture(t)
	h := f.held()
	res, err := f.launch(h, reproductionPassHead1, f.commit)
	if err != nil {
		t.Fatalf("head1: %v", err)
	}
	if len(f.mounted) != 1 || f.mounted[0] != "ro "+h.SHA256 {
		t.Fatalf("mounted = %v, want the held bytes read-only", f.mounted)
	}
	if res.Run.ReproductionSHA256 != h.SHA256 {
		t.Errorf("run records hash %q, want %q", res.Run.ReproductionSHA256, h.SHA256)
	}
	if err := os.Chmod(h.HostPath, 0o644); err != nil {
		t.Fatal(err)
	}
	writeFile(t, h.HostPath, strings.Replace(reproContent, "!= 3", "!= 4", 1))
	_, err = f.launch(h, reproductionPassHead2, f.commit)
	if failureType(err) != ReproductionUnheldFailureType {
		t.Fatalf("head2 after the copy changed = %v, want %s", err, ReproductionUnheldFailureType)
	}
	if len(f.creates) != 1 {
		t.Errorf("launches = %d, want none after the copy changed", len(f.creates))
	}
}

// The reproduction runs in a scratch worktree of the commit it judges under
// the run's directory, never in the run's own worktree, which it leaves as
// it was; the scratch is removed afterwards. The launch is a gate's: no
// route, the runner's helpers read-only at /inputs/reproduction, and the
// report classified by the runner.
func TestReproductionNeverRunsOnTheRunWorktree(t *testing.T) {
	f := newReproFixture(t)
	h := f.held()
	res, err := f.launch(h, reproductionPassHead1, f.commit)
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	if len(f.creates) != 1 {
		t.Fatalf("launches = %d, want 1", len(f.creates))
	}
	req := f.creates[0]
	var workspace string
	helpers := false
	for _, m := range req.Mounts {
		switch m.Target {
		case "/workspace":
			workspace = m.Source
		case reproductionHelperDir:
			helpers = m.ReadOnly
		}
	}
	scratchRoot := filepath.Join(f.acts.LogDir, reproductionRunDirName)
	if workspace == "" || workspace == f.repo || !strings.HasPrefix(workspace, scratchRoot+string(filepath.Separator)) {
		t.Errorf("the launch's /workspace is %q, want a scratch worktree under %s", workspace, scratchRoot)
	}
	if !helpers {
		t.Error("the runner's helper directory is not mounted read-only at /inputs/reproduction")
	}
	if exists(workspace) {
		t.Errorf("the scratch worktree %s was left behind", workspace)
	}
	if exists(filepath.Join(f.repo, "p", "repro_test.go")) || exists(filepath.Join(f.repo, reproductionReportName)) {
		t.Error("the run's own worktree gained the reproduction or its report")
	}
	if res.Run.Outcome != string(reproduction.OutcomeFailed) || res.Run.Tests["TestRepro"] != string(reproduction.KindFail) || res.Run.Commit != f.commit || res.Run.ReportSHA256 != sha256Of([]byte(reproFailReport)) {
		t.Errorf("run = %+v, want a failed TestRepro on %s with the report's hash", res.Run, f.commit)
	}
	if len(res.Attempts) != 1 || res.Attempts[0].Kind != reproductionAttemptKind {
		t.Errorf("attempts = %+v, want one review_reproduction attempt", res.Attempts)
	}
}

// A held file whose runner runs in a project directory below the root (Dir)
// launches its command there, with the package relative to it, and its
// report is classified as for a file at the root.
func TestReproductionInAProjectDirectoryRunsThere(t *testing.T) {
	f := newReproFixture(t)
	h := f.held()
	h.Dir = "p"
	res, err := f.launch(h, reproductionPassHead1, f.commit)
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	if len(f.creates) != 1 {
		t.Fatalf("launches = %d, want 1", len(f.creates))
	}
	argv := f.creates[0].Command
	script := argv[len(argv)-1]
	if len(f.input.SetupCommands) > 0 {
		script = argv[4]
	}
	if !strings.HasPrefix(script, "cd 'p' || exit 125; go test -json") || !strings.Contains(script, " '.' > '/workspace/"+reproductionReportName+"'") {
		t.Errorf("script = %q, want the package run from p", script)
	}
	if res.Run.Outcome != string(reproduction.OutcomeFailed) || res.Run.Tests["TestRepro"] != string(reproduction.KindFail) {
		t.Errorf("run = %+v, want a failed TestRepro", res.Run)
	}
}

// A recheck mounts the factory's held copy over the path in a scratch
// checkout of the result commit: what the run's worktree holds at that path
// (here an untracked edit) is never what runs.
func TestRecheckIgnoresWorktreeEditOfReproductionPath(t *testing.T) {
	f := newReproFixture(t)
	h := f.held()
	writeFile(t, filepath.Join(f.repo, "p", "repro_test.go"), "package p\n\nimport \"testing\"\n\nfunc TestRepro(t *testing.T) {}\n")
	res, err := f.launch(h, run.ReproductionPassRecheck, f.commit)
	if err != nil {
		t.Fatalf("recheck: %v", err)
	}
	if res.PathInCommit {
		t.Error("an untracked worktree file counted as the result commit holding the path")
	}
	if len(f.mounted) != 1 || f.mounted[0] != "ro "+h.SHA256 {
		t.Errorf("mounted = %v, want the held copy read-only", f.mounted)
	}
	if d := reproduction.Recheck(runResultOf(res.Run), h.TestIDs, res.PathInCommit); d.Verdict != reproduction.VerdictStillFailing {
		t.Errorf("recheck verdict = %+v, want still failing on the held copy's failure", d)
	}
}

// A result commit that holds the reproduction's path, in any case, is still
// failing with nothing launched.
func TestRecheckOfACommitHoldingThePathLaunchesNothing(t *testing.T) {
	f := newReproFixture(t)
	h := f.held()
	writeFile(t, filepath.Join(f.repo, "P", "Repro_Test.go"), "package p\n")
	commit := trustedCommit(t, f.repo)
	res, err := f.launch(h, run.ReproductionPassRecheck, commit)
	if err != nil {
		t.Fatalf("recheck: %v", err)
	}
	if !res.PathInCommit || len(f.creates) != 0 {
		t.Errorf("PathInCommit = %v, launches = %d; want true and none", res.PathInCommit, len(f.creates))
	}
	if d := reproduction.Recheck(nil, h.TestIDs, res.PathInCommit); d.Verdict != reproduction.VerdictStillFailing {
		t.Errorf("verdict = %+v, want still failing", d)
	}
}

// A pass on a commit where a parent of the reproduction's path is a link (here
// the base tracks p as a symlink to a directory outside the scratch) or a
// file makes no mountpoint and launches nothing: the host writes nothing
// through the link, and the pass is unstaged, which reproduction.Decide reads
// as advice ("base not checked" for the base).
func TestReproductionWithALinkedParentIsNotStaged(t *testing.T) {
	cases := []struct {
		name  string
		plant func(t *testing.T, repo, outside string)
	}{
		{"symlinked parent", func(t *testing.T, repo, outside string) {
			if err := os.Symlink(outside, filepath.Join(repo, "p")); err != nil {
				t.Fatal(err)
			}
		}},
		{"file parent", func(t *testing.T, repo, _ string) {
			writeFile(t, filepath.Join(repo, "p"), "not a directory\n")
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newReproFixture(t)
			h := f.held()
			outside := t.TempDir()
			if err := os.RemoveAll(filepath.Join(f.repo, "p")); err != nil {
				t.Fatal(err)
			}
			c.plant(t, f.repo, outside)
			base := trustedCommit(t, f.repo)
			for _, pass := range []string{reproductionPassBase, reproductionPassHead1} {
				res, err := f.launch(h, pass, base)
				if err != nil {
					t.Fatalf("%s: %v", pass, err)
				}
				if res.Run.Outcome != string(reproduction.OutcomeUnstaged) || len(f.creates) != 0 {
					t.Errorf("%s: outcome %q, launches %d; want unstaged and none", pass, res.Run.Outcome, len(f.creates))
				}
			}
			entries, err := os.ReadDir(outside)
			if err != nil || len(entries) != 0 {
				t.Errorf("the directory the link points to holds %v (%v), want nothing written", entries, err)
			}
			fail := &reproduction.RunResult{Outcome: reproduction.OutcomeFailed, Tests: map[string]reproduction.Kind{"TestRepro": reproduction.KindFail}}
			unstaged := &reproduction.RunResult{Outcome: reproduction.OutcomeUnstaged}
			if d := reproduction.Decide(reproduction.Evidence{Head1: fail, Head2: fail, Base: unstaged}); d.Verdict != reproduction.VerdictAdvice || d.Reason != "base not checked" {
				t.Errorf("Decide with an unstaged base = %+v, want advice: base not checked", d)
			}
		})
	}
}

// A base commit that tracks the reproduction's path as a regular file (the
// build deleted it) is mounted over, with no stub made.
func TestReproductionOverATrackedBaseFileLaunches(t *testing.T) {
	f := newReproFixture(t)
	h := f.held()
	writeFile(t, filepath.Join(f.repo, "p", "repro_test.go"), "package p\n")
	base := trustedCommit(t, f.repo)
	res, err := f.launch(h, reproductionPassBase, base)
	if err != nil {
		t.Fatalf("base: %v", err)
	}
	if len(f.creates) != 1 || len(f.mounted) != 1 || f.mounted[0] != "ro "+h.SHA256 || res.Run.Outcome != string(reproduction.OutcomeFailed) {
		t.Errorf("launches %d, mounted %v, outcome %q; want one launch of the held copy", len(f.creates), f.mounted, res.Run.Outcome)
	}
}

// A launch whose sandbox started the reproduction's command and whose wait
// then ended on the launch's deadline is a pass with every test a timeout
// (cap_exceeded), fed to the decision as any other timeout. A sandbox that never started its command, or a wait lost for any
// other reason, stays the launch's own failure and gives no verdict.
func TestReproductionThatRunsPastItsCapIsATimeout(t *testing.T) {
	f := newReproFixture(t)
	h := f.held()
	f.rt.WaitErr = context.DeadlineExceeded
	findings := liveReproduce(t, f.acts, f.input, []run.HeldReproduction{h}, f.commit)
	if len(findings) != 1 {
		t.Fatalf("findings = %+v", findings)
	}
	rep := findings[0].Reproduction
	if len(rep.Runs) != 3 {
		t.Fatalf("runs = %+v; want head1, head2 and base", rep.Runs)
	}
	for _, r := range rep.Runs {
		if !r.CapExceeded || r.Outcome != string(reproduction.OutcomeFailed) || r.Tests["TestRepro"] != string(reproduction.KindTimeout) || r.ReportSHA256 != "" {
			t.Errorf("run %s = %+v; want every test a timeout, cap exceeded, no report read", r.Pass, r)
		}
	}
	// The base hangs the same way; the cited line's literals are in the
	// reproduction and not in the base, so it blocks on that.
	if rep.Verdict != string(reproduction.VerdictBlocks) || !strings.Contains(rep.Reason, "fails the same way on the base and holds a literal") {
		t.Errorf("verdict = %s (%s); want blocks on the same timeout and the literal", rep.Verdict, rep.Reason)
	}

	for name, rt := range map[string]*sandboxtest.WorkerRuntime{
		"never started": {CreateErr: context.DeadlineExceeded},
		"wait lost":     {WaitErr: errors.New("the gateway closed the stream")},
	} {
		g := newReproFixture(t)
		hg := g.held()
		g.rt.WorkerRuntime = rt
		if _, err := g.launch(hg, reproductionPassHead1, g.commit); err == nil || failureType(err) == ReproductionUnheldFailureType {
			t.Errorf("%s: err = %v (%s); want the launch's own failure", name, err, failureType(err))
		} else if name == "never started" && failureType(err) != SandboxLaunchFailureType {
			t.Errorf("%s: type %s, want %s", name, failureType(err), SandboxLaunchFailureType)
		}
	}
}

// ranPastLaunchCap reads only the launch's own cap: the Activity's deadline
// ending first, or a context that ended, is not a hang the factory timed.
func TestRanPastLaunchCapReadsOnlyTheLaunchsDeadline(t *testing.T) {
	act, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	launch, cancelLaunch := context.WithTimeout(act, time.Minute)
	defer cancelLaunch()
	started := fmt.Errorf("%w: sandbox x: %w", sandbox.ErrCommandStarted, context.DeadlineExceeded)
	notStarted := fmt.Errorf("%w: %w", sandbox.ErrSandboxNotStarted, context.DeadlineExceeded)
	if !ranPastLaunchCap(act, launch, started) {
		t.Error("a started command cut by the launch's cap: want true")
	}
	if ranPastLaunchCap(act, launch, notStarted) || ranPastLaunchCap(act, launch, errors.New("lost")) {
		t.Error("a sandbox that never started, or another error: want false")
	}
	// The cap passing before the command started (inside the sandbox's
	// start, or before its output appeared): sandbox.RunThroughRuntime
	// returns the bare deadline, with no ErrSandboxNotStarted and no
	// ErrCommandStarted (TestRunThroughRuntimeADeadlineWhileStartingIsNotAStartedCommand,
	// TestRunThroughRuntimeADeadlineBeforeTheOutputIsNotAStartedCommand).
	if ranPastLaunchCap(act, launch, fmt.Errorf("sandbox attempts exhausted: %w", context.DeadlineExceeded)) {
		t.Error("a cap that passed before the command started: want false")
	}
	short, cancelShort := context.WithTimeout(context.Background(), time.Second)
	defer cancelShort()
	inShort, cancelIn := context.WithTimeout(short, time.Minute)
	defer cancelIn()
	if ranPastLaunchCap(short, inShort, started) {
		t.Error("the Activity's deadline before the cap: want false")
	}
	done, cancelDone := context.WithCancel(context.Background())
	cancelDone()
	if ranPastLaunchCap(done, launch, started) {
		t.Error("the Activity's context ended: want false")
	}
}

// liveReproduce runs the retired reproduction phase (reproduceReviewFindings)
// in a test workflow environment whose launch Activity is the real one.
func liveReproduce(t *testing.T, acts *Activities, input RunWorkflowInput, held []run.HeldReproduction, resultSHA string) []run.FindingReproduction {
	t.Helper()
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.SetTestTimeout(90 * time.Minute)
	env.RegisterActivityWithOptions(acts.RunReviewReproductionActivity, activity.RegisterOptions{Name: RunHeldTestActivityName})
	env.RegisterWorkflowWithOptions(liveReproductionWorkflow, temporalworkflow.RegisterOptions{Name: "liveReproductionWorkflow"})
	env.ExecuteWorkflow("liveReproductionWorkflow", input, held, resultSHA)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("reproduction phase: %v", err)
	}
	var findings []run.FindingReproduction
	if err := env.GetWorkflowResult(&findings); err != nil {
		t.Fatal(err)
	}
	return findings
}

// liveReproductionWorkflow calls the phase with RunWorkflow's build/verify
// Activity options and explicit Activity IDs.
func liveReproductionWorkflow(ctx temporalworkflow.Context, input RunWorkflowInput, held []run.HeldReproduction, resultSHA string) ([]run.FindingReproduction, error) {
	buildVerifyCtx := temporalworkflow.WithActivityOptions(ctx, temporalworkflow.ActivityOptions{
		StartToCloseTimeout: time.Hour, HeartbeatTimeout: stepHeartbeatTimeout,
		RetryPolicy: &temporal.RetryPolicy{MaximumAttempts: 1},
	})
	ids := &activityIDs{explicit: true, seen: map[string]int{}}
	result := &RunWorkflowResult{ResultSHA: resultSHA, BaseSHA: input.BaseSHA}
	return reproduceReviewFindings(ctx, ids, buildVerifyCtx, input, &RunProgress{}, result, held, input.WorkspacePath, "")
}
