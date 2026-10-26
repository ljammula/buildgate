package requestdriver

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// RealPushWithLease pushes sha to refs/heads/<branch> on origin from
// workspaceDir, replacing the branch only while origin still has it at
// expected (`--force-with-lease=refs/heads/<branch>:<expected>`): the one
// forced update the factory makes, a restack's. A branch someone moved since
// expected was read fails the push, and nothing is pushed.
func RealPushWithLease(ctx context.Context, workspaceDir, sha, branch, expected string) error {
	ref := "refs/heads/" + branch
	cmd := exec.CommandContext(ctx, "git", "push", "--force-with-lease="+ref+":"+expected, "origin", sha+":"+ref)
	cmd.Dir = workspaceDir
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git push --force-with-lease=%s:%s origin %s:%s: %w: %s", ref, expected, sha, ref, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// RealFetchBranchCommit makes commit sha, origin's tip of branch, present in
// workspaceDir's repository: nothing when it is there already, else `git
// fetch --no-tags origin refs/heads/<branch>`, after which it must be.
func RealFetchBranchCommit(ctx context.Context, workspaceDir, branch, sha string) error {
	has := func() bool {
		return exec.CommandContext(ctx, "git", "-C", workspaceDir, "cat-file", "-e", sha+"^{commit}").Run() == nil
	}
	if has() {
		return nil
	}
	cmd := exec.CommandContext(ctx, "git", "fetch", "--no-tags", "origin", "refs/heads/"+branch)
	cmd.Dir = workspaceDir
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git fetch origin refs/heads/%s: %w: %s", branch, err, strings.TrimSpace(string(out)))
	}
	if !has() {
		return fmt.Errorf("commit %s of origin's %s is not in the repository after fetching it", sha, branch)
	}
	return nil
}
