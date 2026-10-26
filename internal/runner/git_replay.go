package runner

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"buildgate/internal/workspace"
)

// hardenedGit runs one git command in dir with workspace.HardenedGitArgs and
// HardenedGitEnv: no hooks, pager, external diff, system or user
// configuration and no replace refs, so what it reads is the repository's
// objects. stdin, when non-nil, is the command's input.
func hardenedGit(ctx context.Context, dir string, stdin []byte, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", workspace.HardenedGitArgs(dir, args...)...)
	cmd.Env = workspace.HardenedGitEnv()
	cmd.WaitDelay = time.Second
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// GitRevListRange returns the commits of from..to in dir, oldest first
// (`git rev-list --reverse`), and those of them with more than one parent
// (merge commits), in git's order.
func GitRevListRange(ctx context.Context, dir, from, to string) (commits, merges []string, err error) {
	span := from + ".." + to
	out, err := hardenedGit(ctx, dir, nil, "rev-list", "--reverse", span, "--")
	if err != nil {
		return nil, nil, err
	}
	mergeOut, err := hardenedGit(ctx, dir, nil, "rev-list", "--min-parents=2", span, "--")
	if err != nil {
		return nil, nil, err
	}
	return strings.Fields(string(out)), strings.Fields(string(mergeOut)), nil
}

// GitPatchID returns `git patch-id --stable` of commit sha's change against
// its first parent, computed in dir from the repository's objects (`git
// diff-tree -p`, which reads no diff configuration). It is "" for a commit
// that changes nothing, for which git yields no patch id, and for a merge
// commit, whose diff-tree prints no patch.
func GitPatchID(ctx context.Context, dir, sha string) (string, error) {
	patch, err := hardenedGit(ctx, dir, nil, "diff-tree", "-p", "--no-color", "--full-index", "--no-renames", "--no-ext-diff", "--no-textconv", sha, "--")
	if err != nil {
		return "", err
	}
	out, err := hardenedGit(ctx, dir, patch, "patch-id", "--stable")
	if err != nil {
		return "", err
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		return "", nil
	}
	return fields[0], nil
}
