package workflow

import (
	"buildgate/internal/evidence"
	"buildgate/internal/run"
	"buildgate/internal/runner"
	"context"
	"fmt"
	"path/filepath"
	"time"

	"go.temporal.io/sdk/activity"
)

// buildKindName is run.Attempt.Kind of a build attempt.
const buildKindName = "build"

// keptFinishedBuild returns the result of the build step for a run that
// adopted the worktree of a run whose build had already finished (a step
// after it ended with no verdict: a review that got no answer, a verify lost
// with its worker), without launching anything. ok is false, and the build
// runs as for any resume, unless every one of these holds, each read by the
// host and none from the worker:
//
//   - the adopted run's record has a result commit and its last build
//     attempt exited 0;
//   - the worktree is clean;
//   - its HEAD is that result commit.
//
// The model is not asked again for work it finished, and nothing is accepted
// on the strength of this: every gate and the review still run on that
// commit. The adopted run's build evidence is copied to this run's
// directory, so the record shows the build that produced the commit.
func (a *Activities) keptFinishedBuild(ctx context.Context, input RunWorkflowInput) (BuildActivityResult, bool) {
	if activity.GetInfo(ctx).Attempt > 1 {
		return BuildActivityResult{}, false
	}
	earlier, ok := finishedBuildToKeep(a.dataDirFor(input), input)
	if !ok {
		return BuildActivityResult{}, false
	}
	logDir := a.logDirFor(input)
	if err := evidence.RetainFile(filepath.Join(run.Dir(a.dataDirFor(input), earlier.ID), evidence.BuildEvidenceFileName), filepath.Join(logDir, evidence.BuildEvidenceFileName)); err != nil {
		activity.GetLogger(ctx).Warn("the kept build's evidence was not copied to the resumed run", "error", err)
	}
	detail := fmt.Sprintf("build kept from run %s: it finished at %s and the worktree is unchanged", earlier.ID, shortSHA(earlier.ResultSHA))
	progressMark(ctx, logDir, buildKindName, "start", "", "")
	progressMark(ctx, logDir, buildKindName, "note", "", detail)
	progressMark(ctx, logDir, buildKindName, "end", "pass", "")
	now := time.Now().UTC()
	result := runner.Result{Command: []string{detail}, ExitCode: 0, StartedAt: now, FinishedAt: now}
	attempt := run.Attempt{
		Kind: buildKindName, Command: result.Command, ExitCode: 0,
		StartedAt: now.Format(time.RFC3339), FinishedAt: now.Format(time.RFC3339),
		KeptFromRun: earlier.ID,
	}
	return BuildActivityResult{Result: result, Attempts: []run.Attempt{attempt}}, true
}

// finishedBuildToKeep is the run whose finished build input's run keeps:
// the run it adopted, when that run's build finished and the worktree is
// clean at the commit it left.
func finishedBuildToKeep(dataDir string, input RunWorkflowInput) (*run.Run, bool) {
	if input.ResumeFrom == nil {
		return nil, false
	}
	earlier, err := run.Load(dataDir, input.ResumeFrom.RunID)
	if err != nil || !buildFinished(earlier) || !worktreeAt(input.WorkspacePath, earlier.ResultSHA) {
		return nil, false
	}
	return earlier, true
}

// buildFinished reports whether r's build step ran to a successful exit and
// r recorded the commit it left.
func buildFinished(r *run.Run) bool {
	if r.ResultSHA == "" {
		return false
	}
	for i := len(r.Attempts) - 1; i >= 0; i-- {
		if r.Attempts[i].Kind == buildKindName {
			return r.Attempts[i].ExitCode == 0
		}
	}
	return false
}

// worktreeAt reports whether dir is a clean worktree whose HEAD is sha. A
// check that cannot be made is false.
func worktreeAt(dir, sha string) bool {
	clean, err := runner.GitIsClean(dir)
	if err != nil || !clean {
		return false
	}
	head, err := runner.GitRevParseRef(dir, "HEAD")
	return err == nil && head == sha
}
