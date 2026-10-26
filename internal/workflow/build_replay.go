package workflow

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"time"

	"go.temporal.io/sdk/temporal"

	"buildgate/internal/release"
	"buildgate/internal/run"
	"buildgate/internal/runner"
)

// replayStage is the progress stage of ApplyReplayActivity.
const replayStage = "replay"

// ApplyReplayActivity is a replay run's build step. It builds nothing and
// launches nothing: it checks on the host, from git and from run records the
// worker cannot write, that the commit to judge is the one the host replayed
// from the ticket's own accepted runs (checkReplay), and returns one build
// attempt carried from the run whose commits were replayed
// (carriedBuildAttempt, ReplayOf set) with exit code 0. Every gate and review
// after it runs on that commit as for any build. A check that fails is a
// non-retryable ReplayFailureType error, so the run halts
// (run.HaltReasonReplayRefused) before any gate.
func (a *Activities) ApplyReplayActivity(ctx context.Context, input RunWorkflowInput) (BuildActivityResult, error) {
	logDir := a.logDirFor(input)
	progressMark(ctx, logDir, replayStage, "start", "", "")
	of, rp, err := checkReplay(ctx, a.dataDirFor(input), input)
	if err != nil {
		progressMark(ctx, logDir, replayStage, "end", "fail", err.Error())
		return BuildActivityResult{}, temporal.NewNonRetryableApplicationError("replay run refused: "+err.Error(), ReplayFailureType, nil)
	}
	detail := fmt.Sprintf("judging %s, the %d commit(s) of run %s replayed onto %s: nothing is built", shortSHA(rp.HeadSHA), len(rp.Commits), of.ID, shortSHA(rp.OntoSHA))
	progressMark(ctx, logDir, replayStage, "note", "", detail)
	progressMark(ctx, logDir, replayStage, "end", "pass", "")
	now := time.Now().UTC()
	attempt := carriedBuildAttempt(of, now)
	attempt.ReplayOf = of.ID
	result := runner.Result{Command: attempt.Command, ExitCode: 0, StartedAt: now, FinishedAt: now, ImageDigest: attempt.ImageDigest}
	return BuildActivityResult{Result: result, Attempts: []run.Attempt{attempt}}, nil
}

// checkReplay checks a replay run against what the host can re-derive and
// returns the run whose commits were replayed (Replay.OfRunID) and the
// replay record. The record is the run's own run.Replay, written by the host
// when the run was created, never the workflow input alone, which must agree
// with it. Nothing the replay file claims is passed without being derived
// again here:
//
//   - the run names the request ticket its replay names (run.Run.RequestID,
//     TicketIndex);
//   - the worktree is clean and its HEAD is Replay.HeadSHA, with OntoSHA
//     under it (checkReplayedWorktree);
//   - every source run is an accepted, sandboxed run of this repository, of
//     this run's request and ticket, whose release decision allows it, is
//     not invalidated, and records no override (checkReplaySource);
//   - SourceHeadSHA is OfRunID's ResultSHA, and UpstreamSHA is OfRunID's
//     stack base (run.StackBase);
//   - the commit pairs are exactly UpstreamSHA..SourceHeadSHA and
//     OntoSHA..HeadSHA, in order, with no merge commit, and each pair's patch
//     ids, computed here, are equal (checkReplayedCommits);
//   - every original is in a range some source run's review judged
//     (checkReplayProvenance).
func checkReplay(ctx context.Context, dataDir string, input RunWorkflowInput) (*run.Run, *run.Replay, error) {
	if input.Replay == nil {
		return nil, nil, errors.New("the run carries no replay")
	}
	self, err := run.Load(dataDir, input.RunID)
	if err != nil {
		return nil, nil, fmt.Errorf("load the run's own record: %w", err)
	}
	rp := self.Replay
	if err := checkReplayRecord(self, input.Replay); err != nil {
		return nil, nil, err
	}
	if err := checkReplayedWorktree(ctx, input.WorkspacePath, rp); err != nil {
		return nil, nil, err
	}
	sources := make([]*run.Run, 0, len(rp.SourceRunIDs))
	var of *run.Run
	for _, id := range rp.SourceRunIDs {
		source, err := checkReplaySource(dataDir, id, input.IsolatedRepoDir, self)
		if err != nil {
			return nil, nil, err
		}
		sources = append(sources, source)
		if id == rp.OfRunID {
			of = source
		}
	}
	if of == nil {
		return nil, nil, fmt.Errorf("run %s is not among the runs replayed from", rp.OfRunID)
	}
	if of.ResultSHA == "" || rp.SourceHeadSHA != of.ResultSHA {
		return nil, nil, fmt.Errorf("the replayed commits end at %s, not at %s, the result of run %s", shortSHA(rp.SourceHeadSHA), shortSHA(of.ResultSHA), of.ID)
	}
	if base := run.StackBase(dataDir, of); base == "" || rp.UpstreamSHA != base {
		return nil, nil, fmt.Errorf("the replayed commits start after %s, not after %s, the earlier ticket's tip run %s sits on", shortSHA(rp.UpstreamSHA), shortSHA(base), of.ID)
	}
	if err := checkReplayedCommits(ctx, input.WorkspacePath, rp); err != nil {
		return nil, nil, err
	}
	if err := checkReplayProvenance(ctx, dataDir, input.WorkspacePath, rp, sources); err != nil {
		return nil, nil, err
	}
	return of, rp, nil
}

// checkReplayRecord checks that the run's own record carries a replay that
// is well formed (run.Replay.Validate, which requires a request ticket), that
// the workflow input names the same one, and that the run is of that request
// ticket.
func checkReplayRecord(self *run.Run, in *ReplayInput) error {
	rp := self.Replay
	if rp == nil {
		return errors.New("the run's record carries no replay")
	}
	if err := rp.Validate(); err != nil {
		return fmt.Errorf("the run's replay record: %w", err)
	}
	if in.OfRunID != rp.OfRunID || in.OntoSHA != rp.OntoSHA || in.HeadSHA != rp.HeadSHA || !slices.Equal(in.SourceRunIDs, rp.SourceRunIDs) {
		return errors.New("the workflow's replay is not the one the run's record holds")
	}
	if self.RequestID != rp.RequestID || self.TicketIndex != rp.TicketIndex {
		return fmt.Errorf("the run is of request %q ticket %d, not of the request ticket its replay names", self.RequestID, self.TicketIndex)
	}
	return nil
}

// checkReplayedWorktree checks that dir is clean, at rp.HeadSHA, with
// rp.OntoSHA under it, read with a restack's git calls (no attribute from
// the worktree, runner.GitReplayIsClean).
func checkReplayedWorktree(ctx context.Context, dir string, rp *run.Replay) error {
	clean, err := runner.GitReplayIsClean(ctx, dir)
	if err != nil {
		return fmt.Errorf("read the worktree's state: %w", err)
	}
	if !clean {
		return errors.New("the worktree has changes the replay did not make")
	}
	head, err := runner.GitReplayHead(ctx, dir)
	if err != nil {
		return fmt.Errorf("read the worktree's HEAD: %w", err)
	}
	if head != rp.HeadSHA {
		return fmt.Errorf("the worktree is at %s, not at the replayed head %s", shortSHA(head), shortSHA(rp.HeadSHA))
	}
	onto, err := runner.GitReplayIsAncestor(ctx, dir, rp.OntoSHA, head)
	if err != nil {
		return fmt.Errorf("check %s is under the replayed head: %w", shortSHA(rp.OntoSHA), err)
	}
	if !onto {
		return fmt.Errorf("the new base %s is not an ancestor of the replayed head %s", shortSHA(rp.OntoSHA), shortSHA(head))
	}
	return nil
}

// checkReplaySource loads source run id and checks it is accepted, ran its
// build in the sandbox, belongs to the repository at repoDir and to self's
// request and ticket, and that its release decision allows it, is not
// invalidated, and that it records no override. A source that lacks any of
// these refuses: nothing it lacks is ever passed.
func checkReplaySource(dataDir, id, repoDir string, self *run.Run) (*run.Run, error) {
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
	case source.RequestID != self.RequestID || source.TicketIndex != self.TicketIndex:
		return nil, fmt.Errorf("run %s replayed from is of request %q ticket %d, not of this run's request %q ticket %d", id, source.RequestID, source.TicketIndex, self.RequestID, self.TicketIndex)
	case len(source.Overrides) > 0:
		return nil, fmt.Errorf("run %s replayed from records %d override(s): an overridden run is never replayed", id, len(source.Overrides))
	}
	decision, err := release.LoadDecision(dataDir, release.ProjectOf(source), id)
	switch {
	case err != nil:
		return nil, fmt.Errorf("load the release decision of run %s replayed from: %w", id, err)
	case decision == nil:
		return nil, fmt.Errorf("run %s replayed from has no release decision", id)
	case decision.Invalidated:
		return nil, fmt.Errorf("run %s replayed from has an invalidated release decision", id)
	case !decision.Allowed:
		return nil, fmt.Errorf("run %s replayed from was denied by release policy", id)
	}
	return source, nil
}

// checkReplayedCommits checks rp.Commits against git in dir: the originals
// are exactly UpstreamSHA..SourceHeadSHA and the replays exactly
// OntoSHA..HeadSHA, oldest first, the same count, with no merge commit on
// either side; and each pair's patch id (runner.GitPatchID: no context
// lines, every whitespace change counted), computed here, is equal, and is
// what the record says. A pair may have no patch id only when git yields
// none for both commits (each changes nothing): an original that changes
// something whose replay is empty is refused.
func checkReplayedCommits(ctx context.Context, dir string, rp *run.Replay) error {
	originals, err := replayRange(ctx, dir, "original", rp.UpstreamSHA, rp.SourceHeadSHA)
	if err != nil {
		return err
	}
	replayed, err := replayRange(ctx, dir, "replayed", rp.OntoSHA, rp.HeadSHA)
	if err != nil {
		return err
	}
	if len(originals) != len(rp.Commits) || len(replayed) != len(rp.Commits) {
		return fmt.Errorf("the record pairs %d commit(s), but git has %d original and %d replayed", len(rp.Commits), len(originals), len(replayed))
	}
	for i, pair := range rp.Commits {
		if pair.Original != originals[i] || pair.Replayed != replayed[i] {
			return fmt.Errorf("commit pair %d is %s -> %s, but git's commit %d is %s -> %s", i+1, shortSHA(pair.Original), shortSHA(pair.Replayed), i+1, shortSHA(originals[i]), shortSHA(replayed[i]))
		}
		if err := checkReplayedPatch(ctx, dir, i+1, pair); err != nil {
			return err
		}
	}
	return nil
}

// replayRange is from..to in dir, oldest first, refused when it holds a
// merge commit.
func replayRange(ctx context.Context, dir, side, from, to string) ([]string, error) {
	commits, merges, err := runner.GitRevListRange(ctx, dir, from, to)
	if err != nil {
		return nil, fmt.Errorf("list the %s commits: %w", side, err)
	}
	if len(merges) > 0 {
		return nil, fmt.Errorf("the %s commits include merge commit %s", side, shortSHA(merges[0]))
	}
	return commits, nil
}

// checkReplayedPatch computes both patch ids of pair n and checks they are
// equal and are the ones recorded.
func checkReplayedPatch(ctx context.Context, dir string, n int, pair run.ReplayedCommit) error {
	original, err := runner.GitPatchID(ctx, dir, pair.Original)
	if err != nil {
		return fmt.Errorf("patch id of %s: %w", shortSHA(pair.Original), err)
	}
	replayed, err := runner.GitPatchID(ctx, dir, pair.Replayed)
	if err != nil {
		return fmt.Errorf("patch id of %s: %w", shortSHA(pair.Replayed), err)
	}
	if original != "" && replayed == "" {
		return fmt.Errorf("commit pair %d: %s changes something and its replay %s changes nothing: the new base already holds the change", n, shortSHA(pair.Original), shortSHA(pair.Replayed))
	}
	if original != replayed {
		return fmt.Errorf("commit pair %d changes something else: %s and its replay %s have patch ids %q and %q", n, shortSHA(pair.Original), shortSHA(pair.Replayed), original, replayed)
	}
	if pair.OriginalPatchID != original || pair.ReplayedPatchID != replayed {
		return fmt.Errorf("commit pair %d records patch ids other than git's", n)
	}
	return nil
}

// checkReplayProvenance checks that every original commit is in the range
// some source run's gates and review judged: from the run's diff base (for a
// replay source, the commit it replayed onto) to its ResultSHA. A commit no
// source judged -- a commit a person or another run put on the branch --
// refuses.
func checkReplayProvenance(ctx context.Context, dataDir, dir string, rp *run.Replay, sources []*run.Run) error {
	judged := map[string]bool{}
	for _, source := range sources {
		base := source.DiffBase()
		if source.Replay != nil {
			base = source.Replay.OntoSHA
		}
		if base == "" || source.ResultSHA == "" {
			continue
		}
		commits, _, err := runner.GitRevListRange(ctx, dir, base, source.ResultSHA)
		if err != nil {
			return fmt.Errorf("list the commits run %s judged: %w", source.ID, err)
		}
		for _, c := range commits {
			judged[c] = true
		}
	}
	for _, pair := range rp.Commits {
		if !judged[pair.Original] {
			return fmt.Errorf("commit %s is in no range a run replayed from judged", shortSHA(pair.Original))
		}
	}
	return nil
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
