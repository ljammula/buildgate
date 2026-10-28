package workspace

import (
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
)

var commitID = regexp.MustCompile(`^[0-9a-f]{40,64}$`)

// ErrBranchExists is returned by CreateBranchAt for a branch that already
// points at another commit.
var ErrBranchExists = errors.New("branch already exists at another commit")

// ErrRefExists is returned by CreateRef for a ref that already points at
// another commit.
var ErrRefExists = errors.New("ref already exists at another commit")

// CreateRef creates ref (a full name, such as refs/buildgate/rounds/...) at sha
// in repoDir's local repository, under the git metadata lock. A ref that
// exists is never moved: at sha it is left as it is, at another commit it is
// ErrRefExists, so the commit it keeps stays reachable. It writes only the
// local ref database: nothing is pushed.
func CreateRef(repoDir, ref, sha string) error {
	if !strings.HasPrefix(ref, "refs/") || strings.ContainsAny(ref, " \t\n") {
		return fmt.Errorf("update-ref: %q is not a full ref name", ref)
	}
	if !commitID.MatchString(sha) {
		return fmt.Errorf("update-ref: %q is not a full commit id", sha)
	}
	unlock, err := lockGitMetadata(repoDir)
	if err != nil {
		return err
	}
	defer unlock()
	if out, err := exec.Command("git", "-C", repoDir, "rev-parse", "--verify", "--quiet", ref+"^{commit}").Output(); err == nil {
		if strings.TrimSpace(string(out)) == sha {
			return nil
		}
		return fmt.Errorf("%w: %s", ErrRefExists, ref)
	}
	// The empty old value makes git refuse a ref created meanwhile.
	if out, err := gitMutate(repoDir, "update-ref", ref, sha, ""); err != nil {
		return fmt.Errorf("git update-ref %s: %w: %s", ref, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// CreateBranchAt creates the local branch name at sha in repoDir, under the
// git metadata lock. A branch already at sha is left as it is; one at another
// commit is ErrBranchExists, never moved: it may be checked out, or hold work.
func CreateBranchAt(repoDir, name, sha string) error {
	if !commitID.MatchString(sha) || strings.HasPrefix(name, "-") {
		return fmt.Errorf("create branch %q at %q: not a branch name and a full commit id", name, sha)
	}
	unlock, err := lockGitMetadata(repoDir)
	if err != nil {
		return err
	}
	defer unlock()
	if out, err := exec.Command("git", "-C", repoDir, "rev-parse", "--verify", "--quiet", "refs/heads/"+name+"^{commit}").Output(); err == nil {
		if strings.TrimSpace(string(out)) == sha {
			return nil
		}
		return fmt.Errorf("%w: %s", ErrBranchExists, name)
	}
	if out, err := gitMutate(repoDir, "branch", name, sha); err != nil {
		return fmt.Errorf("git branch %s %s: %w: %s", name, sha, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// PruneRoundRefs deletes the round refs of request id (refs/buildgate/rounds/<id>/)
// from repoDir and its local branches buildgate/<id>/t<n>-from-r<m> that start
// with none of keepPrefixes (the branches of tickets that were pushed). A branch
// that cannot be deleted (checked out in a worktree) is left and reported in
// the returned error after the rest are done.
func PruneRoundRefs(repoDir, id string, keepPrefixes []string) error {
	if id == "" || strings.ContainsAny(id, " \t\n/") {
		return fmt.Errorf("prune round refs: %q is not a request id", id)
	}
	unlock, err := lockGitMetadata(repoDir)
	if err != nil {
		return err
	}
	defer unlock()
	var failed []string
	for _, prefix := range []string{"refs/buildgate/rounds/" + id + "/", "refs/heads/buildgate/" + id + "/"} {
		out, err := exec.Command("git", "-C", repoDir, "for-each-ref", "--format=%(refname)", prefix).Output()
		if err != nil {
			return fmt.Errorf("git for-each-ref %s: %w", prefix, err)
		}
		for _, ref := range strings.Fields(string(out)) {
			if strings.HasPrefix(ref, "refs/heads/") && keptBranch(strings.TrimPrefix(ref, "refs/heads/"), keepPrefixes) {
				continue
			}
			if msg, err := gitMutate(repoDir, "update-ref", "-d", ref); err != nil {
				failed = append(failed, ref+": "+strings.TrimSpace(string(msg)))
			}
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("prune round refs: could not delete %s", strings.Join(failed, "; "))
	}
	return nil
}

func keptBranch(branch string, keepPrefixes []string) bool {
	for _, prefix := range keepPrefixes {
		if strings.HasPrefix(branch, prefix) {
			return true
		}
	}
	return false
}

// RestoreBranch points the local branch back at sha in repoDir, under the git
// metadata lock, when it is ahead of it: a build that ran on the branch and
// whose commits are not to be kept moved it. Only a branch at a descendant of
// sha is moved, by a compare-and-swap (`git update-ref` with the head just
// read as the old value), so a branch moved meanwhile is never overwritten;
// one already at sha is left as it is. ahead reports whether the branch still
// points past sha when the call returns: true for a move that failed, false
// for a branch at sha, and false for one that could not be read or is
// elsewhere, which the call leaves as it is and reports as an error. It
// writes only the local ref database: nothing is pushed.
func RestoreBranch(repoDir, branch, sha string) (ahead bool, err error) {
	if !commitID.MatchString(sha) || strings.HasPrefix(branch, "-") || branch == "" {
		return false, fmt.Errorf("restore branch %q to %q: not a branch name and a full commit id", branch, sha)
	}
	unlock, err := lockGitMetadata(repoDir)
	if err != nil {
		return false, err
	}
	defer unlock()
	ref := "refs/heads/" + branch
	out, err := exec.Command("git", "-C", repoDir, "rev-parse", "--verify", "--quiet", ref+"^{commit}").Output()
	if err != nil {
		return false, fmt.Errorf("read %s: %w", ref, err)
	}
	head := strings.TrimSpace(string(out))
	if head == sha {
		return false, nil
	}
	if err := exec.Command("git", "-C", repoDir, "merge-base", "--is-ancestor", sha, head).Run(); err != nil {
		return false, fmt.Errorf("%s is at %s, which does not descend from %s: left as it is", ref, head, sha)
	}
	if out, err := gitMutate(repoDir, "update-ref", "-m", "buildgate: round not pushed", ref, sha, head); err != nil {
		return true, fmt.Errorf("git update-ref %s %s %s: %w: %s", ref, sha, head, err, strings.TrimSpace(string(out)))
	}
	return false, nil
}
