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

// UpdateRef points ref (a full name, such as refs/buildgate/rounds/...) at sha
// in repoDir's local repository, under the git metadata lock. It writes only
// the local ref database: nothing is pushed.
func UpdateRef(repoDir, ref, sha string) error {
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
	if out, err := gitMutate(repoDir, "update-ref", ref, sha); err != nil {
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
