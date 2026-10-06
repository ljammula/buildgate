package workspace

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// gitMetadataLockTimeout bounds the wait for factoryd-git.lock. The lock is
// held across the git commands of one mutator, and `git worktree add` on a
// large repository (a full checkout) can take minutes, so the bound is
// generous: it exists only so a wedged holder fails a run instead of hanging it.
const gitMetadataLockTimeout = 15 * time.Minute

// gitLockRetryAttempts and gitLockRetryBackoff bound the retry of a git
// command that lost a race on one of git's own `.lock` files. They are
// variables so tests can shorten the backoff.
var (
	gitLockRetryAttempts = 5
	gitLockRetryBackoff  = 200 * time.Millisecond
)

// lockGitMetadata takes an exclusive flock on
// <git-common-dir>/factoryd-git.lock and returns its release func. Isolated
// runs of one repository execute concurrently, and each creates or removes a
// worktree, a branch and an info/exclude entry: shared Git metadata that git
// itself guards only with short-lived `.lock` files that fail rather than
// wait. This lock serializes the factory's own mutations. Hold it for the
// duration of one mutator only, and never call another function that takes it
// while holding it (flock is not re-entrant across file descriptors).
func lockGitMetadata(repoDir string) (unlock func(), err error) {
	commonDir, err := GitCommonDir(repoDir)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), gitMetadataLockTimeout)
	defer cancel()
	file, err := flockFile(ctx, filepath.Join(commonDir, "factoryd-git.lock"), syscall.LOCK_EX, true, "git metadata lock")
	if err != nil {
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
	}, nil
}

// retryOnGitLock runs fn, retrying while its output reports that git lost a
// race on a `.lock` file (another git process, outside factoryd's own
// serialization, held it). Any other outcome returns immediately.
func retryOnGitLock(fn func() ([]byte, error)) ([]byte, error) {
	out, err := fn()
	for attempt := 1; err != nil && attempt < gitLockRetryAttempts && strings.Contains(string(out), ".lock': File exists"); attempt++ {
		time.Sleep(gitLockRetryBackoff)
		out, err = fn()
	}
	return out, err
}

// gitMutate runs `git -C repoDir args...` with retryOnGitLock and returns its
// combined output.
func gitMutate(repoDir string, args ...string) ([]byte, error) {
	return retryOnGitLock(func() ([]byte, error) {
		return exec.Command("git", append([]string{"-C", repoDir}, args...)...).CombinedOutput()
	})
}
