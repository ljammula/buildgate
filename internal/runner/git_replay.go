package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"buildgate/internal/workspace"
)

// replayGit runs one git command of a restack in dir with
// workspace.ReplayGitArgs and HardenedGitEnv: no hooks, pager, external
// diff, system or user configuration, no replace refs, and attributes from
// the empty tree, so what it reads is the repository's objects and no
// `.gitattributes` in them changes it. stdin, when non-nil, is the
// command's input.
func replayGit(ctx context.Context, dir string, stdin []byte, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", workspace.ReplayGitArgs(dir, args...)...)
	cmd.Env = workspace.HardenedGitEnv()
	cmd.WaitDelay = time.Second
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// GitRevListRange returns the commits of from..to in dir, oldest first
// (`git rev-list --reverse`), and those of them with more than one parent
// (merge commits), in git's order.
func GitRevListRange(ctx context.Context, dir, from, to string) (commits, merges []string, err error) {
	span := from + ".." + to
	out, err := replayGit(ctx, dir, nil, "rev-list", "--reverse", span, "--")
	if err != nil {
		return nil, nil, err
	}
	mergeOut, err := replayGit(ctx, dir, nil, "rev-list", "--min-parents=2", span, "--")
	if err != nil {
		return nil, nil, err
	}
	return strings.Fields(string(out)), strings.Fields(string(mergeOut)), nil
}

// GitPatchID is the one patch id of a restack, the driver's and the replay
// run's: `git patch-id --verbatim` of commit sha's change against its first
// parent, `git diff-tree -p -U0 --binary`, computed in dir from the
// repository's objects (no diff configuration, no attributes). With no
// context lines, a commit replayed beside a change made under it (an
// adjacent hunk) keeps its id; --verbatim counts every whitespace change,
// and --binary every byte of a binary file. It is "" for a commit that
// changes nothing, for which git yields no patch id, and for a merge
// commit, whose diff-tree prints no patch.
func GitPatchID(ctx context.Context, dir, sha string) (string, error) {
	patch, err := replayGit(ctx, dir, nil, "diff-tree", "-p", "-U0", "--binary", "--no-color", "--full-index", "--no-renames", "--no-ext-diff", "--no-textconv", sha, "--")
	if err != nil {
		return "", err
	}
	out, err := replayGit(ctx, dir, patch, "patch-id", "--verbatim")
	if err != nil {
		return "", err
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		return "", nil
	}
	return fields[0], nil
}

// GitReplayHead is HEAD of the replay run's worktree dir, read as a
// restack's git calls are (replayGit).
func GitReplayHead(ctx context.Context, dir string) (string, error) {
	out, err := replayGit(ctx, dir, nil, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// GitReplayIsClean reports whether the replay run's worktree dir has no
// change and no untracked file (`git status --porcelain`), read with no
// attribute and no filter (replayGit).
func GitReplayIsClean(ctx context.Context, dir string) (bool, error) {
	out, err := replayGit(ctx, dir, nil, "status", "--porcelain")
	if err != nil {
		return false, err
	}
	return len(bytes.TrimSpace(out)) == 0, nil
}

// GitReplayIsAncestor reports whether ancestor is an ancestor of descendant
// in dir (`git merge-base --is-ancestor`, replayGit).
func GitReplayIsAncestor(ctx context.Context, dir, ancestor, descendant string) (bool, error) {
	_, err := replayGit(ctx, dir, nil, "merge-base", "--is-ancestor", ancestor, descendant)
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false, nil
	}
	return false, err
}
