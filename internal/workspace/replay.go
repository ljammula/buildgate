package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// ErrReplayConflict is ReplayCommits' error for commits that do not apply
// cleanly onto the new base.
var ErrReplayConflict = errors.New("the commits do not apply cleanly onto the new base")

// replayIdentity is the committer of a replayed commit; each keeps its
// original author.
var replayIdentity = []string{"-c", "user.name=buildgate", "-c", "user.email=buildgate@localhost"}

// ReplayCommits replays commits, oldest first, onto onto on the new local
// branch branch, in a scratch worktree at worktreePath, and returns the
// replayed commits in order. It holds the git metadata lock throughout. The
// checkout and the cherry-pick run with nothing from configuration that runs
// a program or changes the result, and no attribute from the commits
// (ReplayGitArgs, HardenedGitEnv), rerere off and no signing: `git
// cherry-pick --allow-empty --empty=keep`, which applies each named commit,
// an already-applied one included, squashes nothing and moves no other ref.
// A commit that becomes empty is kept, so the replay stays one for one; the
// caller refuses a replay in which a commit that changed something became
// empty.
//
// The worktree is removed on every outcome; the branch is kept only when the
// replay produced exactly one commit per original. A conflict aborts the
// cherry-pick, deletes the branch and returns ErrReplayConflict. A branch
// that already exists is ErrBranchExists, and nothing is changed. A worktree
// left at worktreePath by an interrupted replay is removed first.
func ReplayCommits(ctx context.Context, repoDir, worktreePath, branch, onto string, commits []string) (replayed []string, err error) {
	if !commitID.MatchString(onto) || strings.HasPrefix(branch, "-") || len(commits) == 0 {
		return nil, fmt.Errorf("replay onto %q on %q: not a full commit id, a branch name and commits", onto, branch)
	}
	for _, c := range commits {
		if !commitID.MatchString(c) {
			return nil, fmt.Errorf("replay %q: not a full commit id", c)
		}
	}
	unlock, err := lockGitMetadataContext(ctx, repoDir)
	if err != nil {
		return nil, err
	}
	defer unlock()
	removeScratchWorktreeLocked(ctx, repoDir, worktreePath)
	if out, err := replayGit(ctx, repoDir, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch); err == nil && len(out) > 0 {
		return nil, fmt.Errorf("%w: %s", ErrBranchExists, branch)
	}
	if out, err := replayGit(ctx, repoDir, "worktree", "add", "-q", "-b", branch, worktreePath, onto); err != nil {
		return nil, fmt.Errorf("git worktree add -b %s: %w: %s", branch, err, strings.TrimSpace(string(out)))
	}
	// Deferred in this order so the worktree is gone before a failed
	// replay's branch is deleted: git keeps a checked-out branch.
	defer func() {
		if err != nil {
			_, _ = replayGit(ctx, repoDir, "branch", "-D", branch)
		}
	}()
	defer removeScratchWorktreeLocked(ctx, repoDir, worktreePath)
	pick := append(append(append([]string{}, replayIdentity...), "-c", "rerere.enabled=false", "-c", "commit.gpgsign=false", "cherry-pick", "--allow-empty", "--empty=keep", "--no-rerere-autoupdate"), commits...)
	if out, pickErr := worktreeGit(ctx, worktreePath, pick...); pickErr != nil {
		_, conflictErr := worktreeGit(ctx, worktreePath, "rev-parse", "--verify", "--quiet", "CHERRY_PICK_HEAD")
		_, _ = worktreeGit(ctx, worktreePath, "cherry-pick", "--abort")
		if conflictErr == nil {
			return nil, fmt.Errorf("%w: %s", ErrReplayConflict, strings.TrimSpace(string(out)))
		}
		return nil, fmt.Errorf("git cherry-pick: %w: %s", pickErr, strings.TrimSpace(string(out)))
	}
	out, err := worktreeGit(ctx, worktreePath, "rev-list", "--reverse", onto+"..HEAD", "--")
	if err != nil {
		return nil, fmt.Errorf("list the replayed commits: %w: %s", err, strings.TrimSpace(string(out)))
	}
	replayed = strings.Fields(string(out))
	if len(replayed) != len(commits) {
		return nil, fmt.Errorf("the replay made %d commit(s) of %d", len(replayed), len(commits))
	}
	return replayed, nil
}

// worktreeGit is replayGit run in a worktree rather than the repository.
func worktreeGit(ctx context.Context, worktreePath string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", ReplayGitArgs(worktreePath, args...)...)
	cmd.Env = HardenedGitEnv()
	return cmd.CombinedOutput()
}

// removeScratchWorktreeLocked is RemoveScratchWorktree for a restack's
// scratch worktree (replayGit), by a caller that holds the git metadata
// lock: a worktree at worktreePath is removed, and a registration for one
// that is gone is pruned. Failures are left for the next prune.
func removeScratchWorktreeLocked(ctx context.Context, repoDir, worktreePath string) {
	if _, err := os.Lstat(worktreePath); err == nil {
		if _, err := replayGit(ctx, repoDir, "worktree", "remove", "--force", worktreePath); err != nil {
			_ = os.RemoveAll(worktreePath)
		}
	}
	_, _ = replayGit(ctx, repoDir, "worktree", "prune")
}

// MoveBranch points the local branch at sha in repoDir under the git
// metadata lock, creating it when it does not exist. A worktree that has the
// branch checked out (an accepted run's preserved evidence worktree, never
// built in again) is left as it is, as a round's commits leave it
// (PrepareOnBranch).
func MoveBranch(repoDir, branch, sha string) error {
	if !commitID.MatchString(sha) || strings.HasPrefix(branch, "-") || branch == "" {
		return fmt.Errorf("move branch %q to %q: not a branch name and a full commit id", branch, sha)
	}
	unlock, err := lockGitMetadata(repoDir)
	if err != nil {
		return err
	}
	defer unlock()
	if out, err := gitMutate(repoDir, "update-ref", "-m", "buildgate: restacked", "refs/heads/"+branch, sha); err != nil {
		return fmt.Errorf("git update-ref refs/heads/%s: %w: %s", branch, err, strings.TrimSpace(string(out)))
	}
	return nil
}
