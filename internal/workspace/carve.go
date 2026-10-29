package workspace

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// carveTreeFilesLimit bounds the paths of one carved commit.
const carveTreeFilesLimit = 5000

// CarveTree is the tree id of parent's tree with each path of paths set to
// what result's tree holds at it: its blob, link or submodule entry, or no
// entry where result has none (a deletion). It is git plumbing on the
// repository's objects (a scratch index file, read-tree, update-index,
// write-tree), never the worktree, an index of the repository, or a ref, and
// reads no attribute from any commit (ReplayGitArgs, HardenedGitEnv).
func CarveTree(ctx context.Context, repoDir, parent, result string, paths []string) (string, error) {
	if !commitID.MatchString(parent) || !commitID.MatchString(result) {
		return "", fmt.Errorf("carve a tree of %q and %q: not full commit ids", parent, result)
	}
	if len(paths) == 0 || len(paths) > carveTreeFilesLimit {
		return "", fmt.Errorf("carve a tree of %d paths: need 1 to %d", len(paths), carveTreeFilesLimit)
	}
	dir, err := os.MkdirTemp("", "buildgate-carve-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	index := filepath.Join(dir, "index")
	git := func(stdin []byte, args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, "git", ReplayGitArgs(repoDir, args...)...)
		cmd.Env = append(HardenedGitEnv(), "GIT_INDEX_FILE="+index)
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
	if _, err := git(nil, "read-tree", parent+"^{tree}"); err != nil {
		return "", err
	}
	var info bytes.Buffer
	for _, p := range paths {
		if p == "" || strings.ContainsAny(p, "\x00\n") {
			return "", fmt.Errorf("carve a tree: %q is not a path", p)
		}
		entry, err := git(nil, "ls-tree", "-z", result, "--", p)
		if err != nil {
			return "", err
		}
		if len(entry) == 0 {
			fmt.Fprintf(&info, "0 %s\t%s\x00", strings.Repeat("0", 40), p)
			continue
		}
		meta, name, ok := strings.Cut(strings.TrimSuffix(string(entry), "\x00"), "\t")
		fields := strings.Fields(meta)
		if !ok || name != p || len(fields) != 3 || fields[1] == "tree" {
			return "", fmt.Errorf("carve a tree: %q is not a file of %s", p, result)
		}
		fmt.Fprintf(&info, "%s %s\t%s\x00", fields[0], fields[2], p)
	}
	if _, err := git(info.Bytes(), "update-index", "-z", "--index-info"); err != nil {
		return "", err
	}
	out, err := git(nil, "write-tree")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// TreeOf is the tree id of commit in repoDir.
func TreeOf(ctx context.Context, repoDir, commit string) (string, error) {
	out, err := replayGit(ctx, repoDir, "rev-parse", "--verify", commit+"^{tree}")
	if err != nil {
		return "", fmt.Errorf("tree of %s: %w: %s", commit, err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// CarveCommit makes a commit with parent as its only parent and CarveTree's
// tree, and returns its id. It writes objects only: no ref moves and no
// worktree changes.
func CarveCommit(ctx context.Context, repoDir, parent, result string, paths []string, message string) (string, error) {
	tree, err := CarveTree(ctx, repoDir, parent, result, paths)
	if err != nil {
		return "", err
	}
	args := append(append([]string{}, replayIdentity...), "-c", "commit.gpgsign=false", "commit-tree", tree, "-p", parent, "-m", message)
	out, err := replayGit(ctx, repoDir, args...)
	if err != nil {
		return "", fmt.Errorf("git commit-tree: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// CarveChain carves result's change from base into one commit per part: the
// first a child of base, each later a child of the one before, commit k's
// tree base's tree with the files of the first k parts as result holds them
// (CarveCommit). It returns the commits, and an error unless the last tree is
// result's own tree: the carve then holds every change of result and nothing
// else. It writes objects only.
func CarveChain(ctx context.Context, repoDir, base, result string, parts [][]string) ([]string, error) {
	var commits []string
	parent := base
	for i, files := range parts {
		commit, err := CarveCommit(ctx, repoDir, parent, result, files, fmt.Sprintf("pr_order carve %d of %d", i+1, len(parts)))
		if err != nil {
			return nil, err
		}
		commits, parent = append(commits, commit), commit
	}
	if len(commits) == 0 {
		return nil, fmt.Errorf("carve %s from %s: no parts", result, base)
	}
	last, err := TreeOf(ctx, repoDir, commits[len(commits)-1])
	if err != nil {
		return nil, err
	}
	want, err := TreeOf(ctx, repoDir, result)
	if err != nil {
		return nil, err
	}
	if last != want {
		return nil, fmt.Errorf("the last carved tree %s is not the result's %s: the parts do not hold the whole change", last, want)
	}
	return commits, nil
}
