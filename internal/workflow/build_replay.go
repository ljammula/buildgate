package workflow

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"time"

	"go.temporal.io/sdk/temporal"

	"buildgate/internal/run"
	"buildgate/internal/runner"
)

// replayStage is the progress stage of ApplyReplayActivity.
const replayStage = "replay"

// ApplyReplayActivity is a replay run's build step. It builds nothing and
// launches nothing: it checks on the host, from git and from run records the
// worker cannot write, that the commit to judge is the one the host replayed
// (checkReplay), and returns one build attempt carried from the run whose
// commits were replayed (carriedBuildAttempt, ReplayOf set) with exit code 0.
// Every gate and review after it runs on that commit as for any build. A
// check that fails is a non-retryable ReplayFailureType error, so the run
// halts (run.HaltReasonReplayRefused) before any gate.
func (a *Activities) ApplyReplayActivity(ctx context.Context, input RunWorkflowInput) (BuildActivityResult, error) {
	logDir := a.logDirFor(input)
	progressMark(ctx, logDir, replayStage, "start", "", "")
	of, err := checkReplay(a.dataDirFor(input), input)
	if err != nil {
		progressMark(ctx, logDir, replayStage, "end", "fail", err.Error())
		return BuildActivityResult{}, temporal.NewNonRetryableApplicationError("replay run refused: "+err.Error(), ReplayFailureType, nil)
	}
	detail := fmt.Sprintf("judging %s, the commits of run %s replayed onto %s: nothing is built", shortSHA(input.Replay.HeadSHA), of.ID, shortSHA(input.Replay.OntoSHA))
	progressMark(ctx, logDir, replayStage, "note", "", detail)
	progressMark(ctx, logDir, replayStage, "end", "pass", "")
	now := time.Now().UTC()
	attempt := carriedBuildAttempt(of, now)
	attempt.ReplayOf = of.ID
	result := runner.Result{Command: attempt.Command, ExitCode: 0, StartedAt: now, FinishedAt: now, ImageDigest: attempt.ImageDigest}
	return BuildActivityResult{Result: result, Attempts: []run.Attempt{attempt}}, nil
}

// checkReplay checks a replay run's worktree and source runs and returns the
// run whose commits were replayed (Replay.OfRunID):
//
//   - the worktree is clean and its HEAD is Replay.HeadSHA;
//   - Replay.OntoSHA is an ancestor of that HEAD;
//   - OfRunID is one of SourceRunIDs, and each of them is an accepted,
//     sandboxed run of this repository. A source that is missing, or lacks
//     any of these, refuses: nothing it lacks is ever passed.
func checkReplay(dataDir string, input RunWorkflowInput) (*run.Run, error) {
	rp := input.Replay
	if rp == nil {
		return nil, errors.New("the run carries no replay")
	}
	if err := checkReplayedWorktree(input.WorkspacePath, rp); err != nil {
		return nil, err
	}
	if !slices.Contains(rp.SourceRunIDs, rp.OfRunID) {
		return nil, fmt.Errorf("run %s is not among the runs replayed from", rp.OfRunID)
	}
	var of *run.Run
	for _, id := range rp.SourceRunIDs {
		source, err := checkReplaySource(dataDir, id, input.IsolatedRepoDir)
		if err != nil {
			return nil, err
		}
		if id == rp.OfRunID {
			of = source
		}
	}
	return of, nil
}

// checkReplayedWorktree checks that dir is clean, at rp.HeadSHA, with
// rp.OntoSHA under it.
func checkReplayedWorktree(dir string, rp *ReplayInput) error {
	clean, err := runner.GitIsClean(dir)
	if err != nil {
		return fmt.Errorf("read the worktree's state: %w", err)
	}
	if !clean {
		return errors.New("the worktree has changes the replay did not make")
	}
	head, err := runner.GitRevParseRef(dir, "HEAD")
	if err != nil {
		return fmt.Errorf("read the worktree's HEAD: %w", err)
	}
	if head != rp.HeadSHA {
		return fmt.Errorf("the worktree is at %s, not at the replayed head %s", shortSHA(head), shortSHA(rp.HeadSHA))
	}
	onto, err := runner.GitIsAncestor(dir, rp.OntoSHA, head)
	if err != nil {
		return fmt.Errorf("check %s is under the replayed head: %w", shortSHA(rp.OntoSHA), err)
	}
	if !onto {
		return fmt.Errorf("the new base %s is not an ancestor of the replayed head %s", shortSHA(rp.OntoSHA), shortSHA(head))
	}
	return nil
}

// checkReplaySource loads source run id and checks it is accepted, ran its
// build in the sandbox, and belongs to the repository at repoDir.
func checkReplaySource(dataDir, id, repoDir string) (*run.Run, error) {
	source, err := run.Load(dataDir, id)
	switch {
	case err != nil:
		return nil, fmt.Errorf("load run %s replayed from: %w", id, err)
	case source.State != run.StateAccepted:
		return nil, fmt.Errorf("run %s replayed from is %s, not accepted", id, source.State)
	case !source.Sandboxed():
		return nil, fmt.Errorf("run %s replayed from has no build attempt that ran in the sandbox", id)
	case !sameDirectory(source.ProjectPath, repoDir):
		return nil, fmt.Errorf("run %s replayed from was made in %s, not in this repository", id, source.ProjectPath)
	}
	return source, nil
}

// sameDirectory reports whether a and b name one directory once symlinks are
// resolved. A path that cannot be resolved is not the same.
func sameDirectory(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	return errA == nil && errB == nil && ra == rb
}
