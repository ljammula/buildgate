package workflow

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/testsuite"

	"buildgate/internal/policy"
	"buildgate/internal/run"
	"buildgate/internal/sandbox"
)

// gateBaseFixture is a real repository with a base commit and the build's
// commit on top of it, whose gate launches go through the package's fake
// sandbox runtime. exits decides each launch's exit code from where its
// /workspace is: the run's worktree (the gate itself) or the rerun's scratch
// worktree of the base commit.
type gateBaseFixture struct {
	*factoryDirFixture
	base, result string
	// launched is the /workspace source of every launch, in order.
	launched []string
	// images is each launch's image.
	images []string
	// onBaseLaunch runs when the rerun's launch is created.
	onBaseLaunch func(scratch string)
	// resultLines and baseLines are what the gate's command prints on the
	// result and on the base commit; the runtime's own when nil.
	resultLines, baseLines []string
}

func newGateBaseFixture(t *testing.T, resultExit, baseExit int) *gateBaseFixture {
	t.Helper()
	f := &gateBaseFixture{factoryDirFixture: newFactoryDirFixture(t, map[string]string{".factory/x.sh": "echo committed\n"})}
	f.base = f.commit
	writeFile(t, filepath.Join(f.repo, "feature.go"), "package main\n\nfunc feature() {}\n")
	f.result = trustedCommit(t, f.repo)
	f.input.BaseSHA = f.base
	recordMounts := f.rt.onCreate
	f.rt.onCreate = func(req sandbox.SandboxRequest) {
		recordMounts(req)
		workspace := ""
		for _, m := range req.Mounts {
			if m.Target == "/workspace" {
				workspace = m.Source
			}
		}
		f.launched = append(f.launched, workspace)
		f.images = append(f.images, req.Image)
		f.rt.ExitCode = resultExit
		if f.resultLines != nil {
			f.rt.Lines = f.resultLines
		}
		if f.onScratch(workspace) {
			f.rt.ExitCode = baseExit
			if f.baseLines != nil {
				f.rt.Lines = f.baseLines
			}
			if f.onBaseLaunch != nil {
				f.onBaseLaunch(workspace)
			}
		}
	}
	return f
}

func (f *gateBaseFixture) onScratch(workspace string) bool {
	return strings.Contains(workspace, string(filepath.Separator)+gateBaseDirName+string(filepath.Separator))
}

// runGate executes the gate's Activity once with its own checkpoint
// directory, or with checkpointDir when it is set (a retry of the same one).
func (f *gateBaseFixture) runGate(check, checkpointDir string) VerifyActivityResult {
	f.t.Helper()
	res, err := f.tryGate(check, checkpointDir)
	if err != nil {
		f.t.Fatalf("%s gate: %v", check, err)
	}
	return res
}

func (f *gateBaseFixture) tryGate(check, checkpointDir string) (VerifyActivityResult, error) {
	f.t.Helper()
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(f.acts.RunNamedGateActivity)
	in := f.input
	in.CheckpointDir = checkpointDir
	if in.CheckpointDir == "" {
		in.CheckpointDir = f.t.TempDir()
	}
	val, err := env.ExecuteActivity(f.acts.RunNamedGateActivity, NamedGateActivityInput{RunWorkflowInput: in, Check: check, Command: "sh .factory/x.sh"})
	if err != nil {
		return VerifyActivityResult{}, err
	}
	var res VerifyActivityResult
	if err := val.Get(&res); err != nil {
		f.t.Fatal(err)
	}
	return res, nil
}

// assertRunUntouched: the run's worktree is where the build left it, and the
// rerun left neither a scratch worktree nor a registration of one.
func (f *gateBaseFixture) assertRunUntouched() {
	f.t.Helper()
	if head := gitIn(f.t, f.repo, "rev-parse", "HEAD"); head != f.result {
		f.t.Errorf("the run's worktree is at %s, want the result commit %s", head, f.result)
	}
	if status := gitIn(f.t, f.repo, "status", "--porcelain"); status != "" {
		f.t.Errorf("the run's worktree is dirty after the gate:\n%s", status)
	}
	if list := gitIn(f.t, f.repo, "worktree", "list", "--porcelain"); strings.Contains(list, gateBaseDirName) {
		f.t.Errorf("a scratch worktree is still registered:\n%s", list)
	}
	if _, err := os.Lstat(filepath.Join(f.acts.LogDir, gateBaseDirName)); err == nil {
		f.t.Errorf("the run directory still holds %s", gateBaseDirName)
	}
	if left := f.stagedSnapshots(); len(left) > 0 {
		f.t.Errorf("factory-dir snapshots left behind: %v", left)
	}
}

// A named gate and a repository gate that fail on the result and on the base
// commit record "fails": the rerun is a second launch of the same image on a
// scratch worktree holding the base commit, with `.factory/` read-only from
// the trusted commit, and the gate's own result is what it was.
func TestFailedGateIsRerunOnTheBaseCommitAndRecordsThatItFailsThereToo(t *testing.T) {
	for _, check := range []string{"lint", policy.RepoGateCheck("docs")} {
		t.Run(check, func(t *testing.T) {
			f := newGateBaseFixture(t, 3, 7)
			var sawBaseTree bool
			f.onBaseLaunch = func(scratch string) {
				_, featureErr := os.Lstat(filepath.Join(scratch, "feature.go"))
				_, mainErr := os.Lstat(filepath.Join(scratch, "main.go"))
				sawBaseTree = os.IsNotExist(featureErr) && mainErr == nil
				if head := gitIn(t, scratch, "rev-parse", "HEAD"); head != f.base {
					t.Errorf("the scratch worktree is at %s, want the base commit %s", head, f.base)
				}
			}
			res := f.runGate(check, "")

			if res.Result.ExitCode != 3 {
				t.Errorf("the gate's exit code = %d, want its own 3: the rerun must not change the gate's result", res.Result.ExitCode)
			}
			if len(res.Attempts) != 1 || res.Attempts[0].Kind != check {
				t.Errorf("attempts = %+v, want the gate's one attempt: the rerun is not an attempt of the gate", res.Attempts)
			}
			bc := res.BaseCheck
			if bc == nil || bc.Outcome != run.GateBaseFails || bc.BaseSHA != f.base || bc.ExitCode != 7 || bc.Reason != "" {
				t.Fatalf("base check = %+v, want fails with exit 7 on %s", bc, f.base)
			}
			if bc.LogSHA256 == "" || bc.LogPath == res.Result.LogPath {
				t.Errorf("base check log %q (hash %q), want a log of its own beside the gate's %q", bc.LogPath, bc.LogSHA256, res.Result.LogPath)
			}
			if len(f.launched) != 2 || f.launched[0] != f.repo || !f.onScratch(f.launched[1]) {
				t.Fatalf("launches on %v, want the run's worktree then a scratch worktree", f.launched)
			}
			if !sawBaseTree {
				t.Error("the scratch worktree did not hold the base commit's tree")
			}
			if f.images[0] != f.images[1] {
				t.Errorf("the rerun's image %q differs from the gate's %q", f.images[1], f.images[0])
			}
			for i, m := range f.mounts {
				if !m.mounted || !m.readOnly || m.files["x.sh"] != "echo committed\n" {
					t.Errorf("launch %d: .factory mount = %+v, want the trusted commit's, read-only", i, m)
				}
			}
			f.assertRunUntouched()
		})
	}
}

// A gate that fails on the result and passes on the base commit (the build
// broke it, or it is flaky) records "passes".
func TestFailedGateThatPassesOnTheBaseCommitRecordsThatItPasses(t *testing.T) {
	f := newGateBaseFixture(t, 1, 0)
	res := f.runGate("lint", "")
	if res.Result.ExitCode != 1 {
		t.Errorf("the gate's exit code = %d, want 1: a passing rerun must not turn the gate into a pass", res.Result.ExitCode)
	}
	if bc := res.BaseCheck; bc == nil || bc.Outcome != run.GateBasePasses || bc.ExitCode != 0 || bc.BaseSHA != f.base {
		t.Fatalf("base check = %+v, want passes on %s", bc, f.base)
	}
	f.assertRunUntouched()
}

// The green path costs nothing: a passing gate launches once and records no
// base check; neither does the reference oracle, whose gate is not rerun.
func TestPassingGateAndReferenceOracleAreNotRerunOnTheBaseCommit(t *testing.T) {
	t.Run("a passing gate", func(t *testing.T) {
		f := newGateBaseFixture(t, 0, 9)
		res := f.runGate("lint", "")
		if res.BaseCheck != nil || len(f.launched) != 1 {
			t.Errorf("base check = %+v after %d launch(es), want none after one", res.BaseCheck, len(f.launched))
		}
	})
	t.Run("a failed reference oracle", func(t *testing.T) {
		f := newGateBaseFixture(t, 1, 9)
		res := f.runGate(policy.ReferenceOracleGateID, "")
		if res.BaseCheck != nil || len(f.launched) != 1 {
			t.Errorf("base check = %+v after %d launch(es), want none after one", res.BaseCheck, len(f.launched))
		}
	})
}

// Whatever stops the rerun from reaching an exit code leaves the gate the
// failure it was, with "not checked" and the reason on the record; the
// Activity itself does not fail.
func TestBaseRerunThatCannotRunRecordsNotCheckedAndLeavesTheGateFailed(t *testing.T) {
	cases := []struct {
		name       string
		arrange    func(f *gateBaseFixture)
		wantReason string
		wantBase   func(f *gateBaseFixture) string
		launches   int
	}{
		{"the run recorded no base commit", func(f *gateBaseFixture) { f.input.BaseSHA = "" }, "no base commit", func(*gateBaseFixture) string { return "" }, 1},
		{"the base commit is not in the repository", func(f *gateBaseFixture) { f.input.BaseSHA = strings.Repeat("0", 40) }, "check out the base commit in a scratch worktree", func(*gateBaseFixture) string { return strings.Repeat("0", 40) }, 1},
		{"the sandbox cannot be created", func(f *gateBaseFixture) {
			f.onBaseLaunch = func(string) { f.rt.CreateErr = errors.New("gateway unavailable") }
		}, "the gate could not be run on the base commit", func(f *gateBaseFixture) string { return f.base }, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newGateBaseFixture(t, 4, 4)
			tc.arrange(f)
			res := f.runGate("lint", "")
			if res.Result.ExitCode != 4 {
				t.Errorf("the gate's exit code = %d, want 4", res.Result.ExitCode)
			}
			bc := res.BaseCheck
			if bc == nil || bc.Outcome != run.GateBaseNotChecked || !strings.Contains(bc.Reason, tc.wantReason) || bc.BaseSHA != tc.wantBase(f) {
				t.Fatalf("base check = %+v, want not_checked with a reason containing %q", bc, tc.wantReason)
			}
			if strings.ContainsAny(bc.Reason, "\n\r") {
				t.Errorf("the reason is not one line: %q", bc.Reason)
			}
			if len(f.launched) != tc.launches {
				t.Errorf("%d launch(es), want %d", len(f.launched), tc.launches)
			}
			f.assertRunUntouched()
		})
	}
}

// A run that continues an earlier run's branch (a corrective round, a
// PR-review round) is rerun on the ticket's own base, its diff base, not on
// the commit the round started from.
func TestBaseRerunUsesTheDiffBaseOfARunThatContinuesABranch(t *testing.T) {
	f := newGateBaseFixture(t, 1, 1)
	f.input.DiffBaseSHA, f.input.BaseSHA = f.base, f.result
	var head string
	f.onBaseLaunch = func(scratch string) { head = gitIn(t, scratch, "rev-parse", "HEAD") }
	res := f.runGate("lint", "")
	if bc := res.BaseCheck; bc == nil || bc.BaseSHA != f.base || head != f.base {
		t.Errorf("base check = %+v on a scratch worktree at %s, want the diff base %s", bc, head, f.base)
	}
}

// Temporal may execute the Activity again. A completed gate is returned from
// its checkpoint, base check included, without another launch.
func TestRetriedGateActivityReturnsTheCheckpointedBaseCheckWithoutRerunning(t *testing.T) {
	f := newGateBaseFixture(t, 2, 2)
	checkpointDir := t.TempDir()
	first := f.runGate("lint", checkpointDir)
	second := f.runGate("lint", checkpointDir)
	if len(f.launched) != 2 {
		t.Errorf("%d launches over two executions, want the first execution's two", len(f.launched))
	}
	if first.BaseCheck == nil || second.BaseCheck == nil || *first.BaseCheck != *second.BaseCheck || second.BaseCheck.Outcome != run.GateBaseFails {
		t.Errorf("base check: first %+v, again %+v; want the same fails record", first.BaseCheck, second.BaseCheck)
	}
}

// A worker that dies during the rerun has already checkpointed the gate's own
// result, with the rerun recorded as interrupted. The Activity's retry
// returns that, launches nothing, and removes the scratch worktree the dead
// worker left.
func TestGateActivityRetriedAfterItsWorkerDiedMidRerunKeepsTheGateAndSweepsTheScratchWorktree(t *testing.T) {
	f := newGateBaseFixture(t, 2, 2)
	checkpointDir := t.TempDir()
	// The dead worker: the rerun's sandbox never comes back, and the worker
	// is gone before it can clean up. Stood in for by a launch that fails
	// after leaving a scratch worktree of its own registered, then restoring
	// the "interrupted" checkpoint the first save wrote.
	var interrupted []byte
	var checkpointPath string
	f.onBaseLaunch = func(string) {
		all, _ := filepath.Glob(filepath.Join(checkpointDir, "activity-checkpoints", "*.json"))
		var matches []string
		for _, m := range all {
			if !strings.Contains(filepath.Base(m), ".attempt-") {
				matches = append(matches, m)
			}
		}
		if len(matches) != 1 {
			t.Fatalf("checkpoints before the rerun's launch: %v, want the gate's own", all)
		}
		checkpointPath = matches[0]
		var err error
		if interrupted, err = os.ReadFile(checkpointPath); err != nil {
			t.Fatal(err)
		}
	}
	f.runGate("lint", checkpointDir)
	if !strings.Contains(string(interrupted), gateBaseInterrupted) || !strings.Contains(string(interrupted), `"exit_code": 2`) && !strings.Contains(string(interrupted), `"exit_code":2`) {
		t.Fatalf("the checkpoint saved before the rerun does not hold the failed gate with an interrupted base check:\n%s", interrupted)
	}
	if err := os.WriteFile(checkpointPath, interrupted, 0o600); err != nil {
		t.Fatal(err)
	}
	scratch, err := gateBaseWorktreePath(f.acts.LogDir, "lint")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(scratch), 0o750); err != nil {
		t.Fatal(err)
	}
	gitIn(t, f.repo, "worktree", "add", "-q", "--detach", scratch, f.base)

	launchedBefore := len(f.launched)
	res := f.runGate("lint", checkpointDir)
	if len(f.launched) != launchedBefore {
		t.Errorf("the retry launched %d more sandbox(es), want none", len(f.launched)-launchedBefore)
	}
	if res.Result.ExitCode != 2 {
		t.Errorf("the gate's exit code = %d, want 2", res.Result.ExitCode)
	}
	if bc := res.BaseCheck; bc == nil || bc.Outcome != run.GateBaseNotChecked || bc.Reason != gateBaseInterrupted {
		t.Errorf("base check = %+v, want not_checked: interrupted", bc)
	}
	f.assertRunUntouched()
}

// The rerun is bounded by what is left of the gate's own time limit. With too
// little left to launch and tear down a sandbox it is not started, and
// nothing is checked out.
func TestBaseRerunIsNotStartedWithTooLittleOfTheGatesTimeLimitLeft(t *testing.T) {
	f := newGateBaseFixture(t, 1, 1)
	ctx, cancel := context.WithTimeout(context.Background(), gateBaseCheckReserve+sandboxAttemptTeardownMargin-time.Second)
	defer cancel()
	bc := f.acts.checkGateOnBase(ctx, NamedGateActivityInput{RunWorkflowInput: f.input, Check: "lint", Command: "true"}, []string{"sh", "-c", "true"}, nil)
	if bc.Outcome != run.GateBaseNotChecked || !strings.Contains(bc.Reason, "time limit") || bc.BaseSHA != f.base {
		t.Errorf("base check = %+v, want not_checked for lack of time", bc)
	}
	if len(f.launched) != 0 {
		t.Errorf("%d launch(es), want none", len(f.launched))
	}
	f.assertRunUntouched()
}

// The rerun is made only when the run's record proves which commit the
// ticket's work started from. A run that adopts a halted run's worktree, or
// that checks out an existing branch with no diff base, starts from a commit
// that may already hold the ticket's work (a resumed corrective round starts
// from the failed attempt's own commit): a gate red there says nothing about
// the base, so nothing is launched and the gate stays corrective.
func TestBaseRerunIsNotMadeWhenTheRunsBaseMayAlreadyHoldTheTicketsWork(t *testing.T) {
	for _, tc := range []struct {
		name    string
		arrange func(f *gateBaseFixture)
	}{
		{"a resumed run", func(f *gateBaseFixture) {
			f.input.BaseSHA = f.result
			f.input.ResumeFrom = &ResumeFrom{RunID: "lost-run", WorktreePath: f.repo, Branch: "factoryd/t", BaseSHA: f.result}
		}},
		{"a run on an existing branch with no diff base", func(f *gateBaseFixture) {
			f.input.BaseSHA, f.input.OnBranch = f.result, "factoryd/t"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newGateBaseFixture(t, 5, 5)
			tc.arrange(f)
			res := f.runGate("lint", "")
			bc := res.BaseCheck
			if bc == nil || bc.Outcome != run.GateBaseNotChecked || !strings.Contains(bc.Reason, "may already hold the ticket's work") {
				t.Fatalf("base check = %+v, want not_checked: the run's base may already hold the ticket's work", bc)
			}
			if len(f.launched) != 1 {
				t.Errorf("%d launch(es), want the gate's own only", len(f.launched))
			}
			if res.Result.ExitCode != 5 {
				t.Errorf("the gate's exit code = %d, want 5", res.Result.ExitCode)
			}
			f.assertRunUntouched()
		})
	}
}

// A run on an existing branch that names its diff base (a corrective build, a
// PR-review round, a retry continuing on the failed attempt's commit) is
// rerun on that diff base: the ticket's own base.
func TestBaseRerunOfARunOnABranchWithADiffBaseUsesTheDiffBase(t *testing.T) {
	f := newGateBaseFixture(t, 1, 0)
	f.input.DiffBaseSHA, f.input.BaseSHA, f.input.OnBranch = f.base, f.result, "factoryd/t"
	res := f.runGate("lint", "")
	if bc := res.BaseCheck; bc == nil || bc.Outcome != run.GateBasePasses || bc.BaseSHA != f.base || len(f.launched) != 2 {
		t.Errorf("base check = %+v after %d launch(es), want passes on the diff base %s", bc, len(f.launched), f.base)
	}
}

// holdGitMetadataLock takes the repository's git metadata lock, as another
// run of the same repository does while it creates or removes a worktree,
// and returns its release.
func holdGitMetadataLock(t *testing.T, repo string) (release func()) {
	t.Helper()
	lock, err := os.OpenFile(filepath.Join(repo, ".git", "factoryd-git.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	released := false
	release = func() {
		if !released {
			released = true
			_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
			_ = lock.Close()
		}
	}
	t.Cleanup(release)
	return release
}

// A retried gate Activity returns its checkpointed result before it waits on
// anything: with the repository's git metadata lock held by another run and a
// scratch worktree left by the dead attempt, the result still comes back at
// once, and the leftover is removed without the lock.
func TestRetriedGateActivityReturnsItsCheckpointWhileTheGitMetadataLockIsHeld(t *testing.T) {
	f := newGateBaseFixture(t, 2, 2)
	checkpointDir := t.TempDir()
	first := f.runGate("lint", checkpointDir)
	scratch, err := gateBaseWorktreePath(f.acts.LogDir, "lint")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(scratch), 0o750); err != nil {
		t.Fatal(err)
	}
	gitIn(t, f.repo, "worktree", "add", "-q", "--detach", scratch, f.base)
	release := holdGitMetadataLock(t, f.repo)

	type outcome struct {
		res VerifyActivityResult
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := f.tryGate("lint", checkpointDir)
		done <- outcome{res, err}
	}()
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("the retried gate: %v", got.err)
		}
		if got.res.Result.ExitCode != 2 || got.res.BaseCheck == nil || first.BaseCheck == nil || *got.res.BaseCheck != *first.BaseCheck {
			t.Errorf("the retried gate returned %+v (base check %+v), want the checkpointed result with %+v", got.res.Result, got.res.BaseCheck, first.BaseCheck)
		}
	case <-time.After(10 * time.Second):
		release()
		<-done
		t.Fatal("the retried gate did not return its checkpointed result while the git metadata lock was held: its sweep waited on the lock before the checkpoint's early return")
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Lstat(scratch); os.IsNotExist(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the scratch worktree the dead attempt left is still there, with the lock still held")
		}
		time.Sleep(20 * time.Millisecond)
	}
	release()
}

// The rerun's host steps wait on the git metadata lock under the rerun's own
// deadline and heartbeat while they wait: a lock another run holds for longer
// than the Activity's heartbeat timeout costs the base check ("not checked"),
// never the Activity.
func TestBaseRerunWaitingOnTheGitMetadataLockHeartbeatsAndEndsNotChecked(t *testing.T) {
	f := newGateBaseFixture(t, 1, 1)
	release := holdGitMetadataLock(t, f.repo)
	// Longer than one heartbeat interval, so a wait with no heartbeat shows.
	wait := activityHeartbeatInterval + 3*time.Second

	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	var heartbeats atomic.Int32
	env.SetOnActivityHeartbeatListener(func(*activity.Info, converter.EncodedValues) { heartbeats.Add(1) })
	env.RegisterActivityWithOptions(func(ctx context.Context, in NamedGateActivityInput) (*run.GateBaseCheck, error) {
		ctx, cancel := context.WithTimeout(ctx, gateBaseCheckReserve+wait)
		defer cancel()
		return f.acts.checkGateOnBase(ctx, in, []string{"sh", "-c", "true"}, nil), nil
	}, activity.RegisterOptions{Name: "checkGateOnBase"})
	in := f.input
	in.CheckpointDir = t.TempDir()

	type outcome struct {
		bc  *run.GateBaseCheck
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		val, err := env.ExecuteActivity("checkGateOnBase", NamedGateActivityInput{RunWorkflowInput: in, Check: "lint", Command: "true"})
		var bc *run.GateBaseCheck
		if err == nil {
			err = val.Get(&bc)
		}
		done <- outcome{bc, err}
	}()
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatal(got.err)
		}
		if got.bc == nil || got.bc.Outcome != run.GateBaseNotChecked || !strings.Contains(got.bc.Reason, "lock") {
			t.Errorf("base check = %+v, want not_checked naming the lock it waited for", got.bc)
		}
	case <-time.After(wait + 10*time.Second):
		release()
		<-done
		t.Errorf("the rerun was still waiting on the git metadata lock %s after its own deadline", 10*time.Second)
	}
	if heartbeats.Load() == 0 {
		t.Errorf("no heartbeat was recorded while the rerun waited %s on the git metadata lock", wait)
	}
	if len(f.launched) != 0 {
		t.Errorf("%d launch(es), want none", len(f.launched))
	}
	release()
	f.assertRunUntouched()
}

// A gate that is red on the base commit is the operator's only when it fails
// there the same way as on the result: the same exit code and the same
// failing lines of output (observation.Excerpt, which the handoff already
// uses to pick them). A ticket whose job is to turn that gate green fails it
// differently after a partial fix, and keeps its corrective build.
func TestBaseRerunTellsTheSameFailureFromADifferentOne(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		resultExit, baseExit  int
		resultLines, baseLine []string
		want                  string
	}{
		{"the same exit code and the same failing lines", 1, 1,
			[]string{"=== RUN TestSum", "--- FAIL: TestSum", "    sum_test.go:9: got 3, want 4", "FAIL"},
			[]string{"=== RUN TestSum", "--- FAIL: TestSum", "    sum_test.go:9: got 3, want 4", "FAIL"}, "fails_same"},
		{"other failing lines: the ticket's partial fix", 1, 1,
			[]string{"--- FAIL: TestSumOfNegatives", "    sum_test.go:21: got -1, want -3", "FAIL"},
			[]string{"--- FAIL: TestSum", "    sum_test.go:9: got 3, want 4", "--- FAIL: TestSumOfNegatives", "    sum_test.go:21: got 0, want -3", "FAIL"}, "fails_differently"},
		{"the same lines with another exit code", 2, 1,
			[]string{"--- FAIL: TestSum", "FAIL"}, []string{"--- FAIL: TestSum", "FAIL"}, "fails_differently"},
		// What the excerpt already leaves out or cleans: terminal colour,
		// trailing spaces, and lines that report no failure (a passing
		// package's timing, a progress line).
		{"a difference the excerpt already normalises", 1, 1,
			[]string{"ok  \tacme/api\t0.31s", "\x1b[31m--- FAIL: TestSum\x1b[0m   ", "    sum_test.go:9: got 3, want 4", "FAIL", "collected in 12 files"},
			[]string{"ok  \tacme/api\t0.52s", "--- FAIL: TestSum", "    sum_test.go:9: got 3, want 4", "FAIL", "collected in 11 files"}, "fails_same"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newGateBaseFixture(t, tc.resultExit, tc.baseExit)
			f.resultLines, f.baseLines = tc.resultLines, tc.baseLine
			res := f.runGate("lint", "")
			if bc := res.BaseCheck; bc == nil || bc.Outcome != tc.want || bc.ExitCode != tc.baseExit || bc.BaseSHA != f.base {
				t.Fatalf("base check = %+v, want %s with exit %d", bc, tc.want, tc.baseExit)
			}
			if res.Result.ExitCode != tc.resultExit {
				t.Errorf("the gate's exit code = %d, want %d", res.Result.ExitCode, tc.resultExit)
			}
			f.assertRunUntouched()
		})
	}
}
