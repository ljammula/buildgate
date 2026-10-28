// Package pincommit is the factory HOST's one step that commits files it
// holds onto a run's branch: each file is a repository-relative path, the
// host's own copy of its bytes and the SHA-256 that copy was pinned to. The
// commit is a child of a given result commit, and each committed file is
// re-read from that commit and compared with its pinned hash, so neither a
// .gitattributes rewrite nor a write through an agent-planted link can make
// the committed bytes differ from the pinned ones.
//
// The package decides nothing about which files to commit: a caller passes
// the list. It refuses a path that is not a new, clean, relative path outside
// the protected names, a dirty worktree, and any component of a path that is
// a link.
package pincommit

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"buildgate/internal/evidence"
	"buildgate/internal/policy"
	"buildgate/internal/projectconfig"
	"buildgate/internal/runner"
)

// maxFileBytes is the largest host copy the step reads.
const maxFileBytes = 1 << 20

// PinnedFile is one file to commit.
type PinnedFile struct {
	// Path is the repository-relative path the file is committed at.
	Path string
	// HostPath is the host's own copy of the bytes.
	HostPath string
	// SHA256 is the lowercase hex digest the copy must have.
	SHA256 string
}

// Result reports what Commit did.
type Result struct {
	// Committed is false when the files were already committed by an earlier
	// attempt at this step.
	Committed bool
	// Paths are the committed paths, sorted.
	Paths []string
}

// HashBytes is the lowercase hex SHA-256 used throughout this package.
func HashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// ValidatePath refuses any repository path the step may not write: empty,
// absolute, unclean, traversing, with a control, backslash or glob
// character, starting with ':' or '-', anything under or named like .git*
// or .buildgate (matched case-insensitively at any depth), the project config
// file and its scripts directory, harness byproducts, dependency lockfiles,
// and every path in extraProtected (matched exactly or, for a trailing "/",
// as a directory prefix).
func ValidatePath(p string, extraProtected []string) error {
	if err := validatePathShape(p); err != nil {
		return err
	}
	if strings.EqualFold(p, projectconfig.FileName) {
		return fmt.Errorf("path %q is the protected project config", p)
	}
	if first, _, _ := strings.Cut(p, "/"); strings.EqualFold(first, projectconfig.DirName) {
		return fmt.Errorf("path %q is under the protected project scripts directory %s", p, projectconfig.DirName)
	}
	if err := validatePathUnprotected(p, extraProtected); err != nil {
		return err
	}
	if len(policy.ExcludeHarnessByproducts([]string{p})) == 0 {
		return fmt.Errorf("path %q is a harness byproduct path", p)
	}
	if len(evidence.DependencyLockfilesTouched([]string{p})) > 0 {
		return fmt.Errorf("path %q is a dependency lockfile", p)
	}
	return nil
}

func validatePathShape(p string) error {
	if p == "" {
		return errors.New("empty path")
	}
	if p[0] == ':' || p[0] == '-' {
		return fmt.Errorf("path %q must not start with ':' (pathspec magic) or '-'", p)
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f || r == '\\' {
			return fmt.Errorf("path %q contains a control or backslash character", p)
		}
		if r == '*' || r == '?' || r == '[' {
			return fmt.Errorf("path %q contains a glob character", p)
		}
	}
	if strings.HasPrefix(p, "/") || filepath.IsAbs(p) || (len(p) >= 2 && p[1] == ':') {
		return fmt.Errorf("path %q must be relative", p)
	}
	if path.Clean(p) != p || p == "." {
		return fmt.Errorf("path %q is not a clean relative path (no ./, //, trailing / or ..)", p)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." || seg == "." || seg == "" {
			return fmt.Errorf("path %q traverses", p)
		}
		low := strings.ToLower(seg)
		switch {
		case strings.HasPrefix(low, ".git"):
			return fmt.Errorf("path %q is under a .git* name", p)
		case low == ".buildgate":
			return fmt.Errorf("path %q is under .buildgate", p)
		}
	}
	return nil
}

func validatePathUnprotected(p string, extraProtected []string) error {
	for _, prot := range extraProtected {
		if prot == "" {
			continue
		}
		if strings.HasSuffix(prot, "/") {
			if strings.HasPrefix(strings.ToLower(p), strings.ToLower(prot)) {
				return fmt.Errorf("path %q is under protected path %q", p, prot)
			}
			continue
		}
		if strings.EqualFold(p, prot) {
			return fmt.Errorf("path %q is a protected path", p)
		}
	}
	return nil
}

// loaded is a pinned file with its bytes read and checked.
type loaded struct {
	PinnedFile
	bytes []byte
}

// load reads every host copy and requires it to hash to its pinned value.
func load(files []PinnedFile, extraProtected []string) ([]loaded, error) {
	seen := map[string]bool{}
	var out []loaded
	for _, f := range files {
		if err := ValidatePath(f.Path, extraProtected); err != nil {
			return nil, err
		}
		key := strings.ToLower(f.Path)
		if seen[key] {
			return nil, fmt.Errorf("path %q is listed more than once", f.Path)
		}
		seen[key] = true
		data, err := evidence.ReadHostileFile(f.HostPath, maxFileBytes)
		if err != nil {
			return nil, fmt.Errorf("read host copy of %q: %w", f.Path, err)
		}
		if got := HashBytes(data); got != f.SHA256 {
			return nil, fmt.Errorf("host copy of %q hashes to %s, not the pinned %s", f.Path, got, f.SHA256)
		}
		out = append(out, loaded{PinnedFile: f, bytes: data})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// Commit writes files into dir and commits them in one host-side commit that
// is a child of resultSHA. dir must be a clean worktree whose HEAD is
// resultSHA. It is idempotent: when HEAD is already a child of resultSHA that
// changed exactly these paths to the pinned bytes (a retried Activity), it
// commits nothing and still verifies the blobs.
//
// Fail-closed checks, all before any write: no path exists at resultSHA (the
// step adds files, it never overwrites ordinary source), no existing
// component of any path is a link, and the worktree is clean apart from the
// residue of an earlier attempt's own paths.
//
// After the commit every file is re-read from HEAD and compared with its
// pinned hash.
func Commit(dir string, files []PinnedFile, resultSHA, message string, extraProtected []string) (*Result, error) {
	if len(files) == 0 {
		return &Result{}, nil
	}
	loaded, err := load(files, extraProtected)
	if err != nil {
		return nil, err
	}
	var paths []string
	residue := map[string][]byte{}
	for _, f := range loaded {
		paths = append(paths, f.Path)
		residue[f.Path] = f.bytes
	}
	done, err := alreadyCommitted(dir, resultSHA, paths)
	if err != nil {
		return nil, err
	}
	res := &Result{Paths: paths}
	if !done {
		if err := unstagePaths(dir, residue); err != nil {
			return nil, err
		}
		clean, err := cleanApartFromResidue(dir, residue)
		if err != nil {
			return nil, fmt.Errorf("check workspace cleanliness before the commit: %w", err)
		}
		if !clean {
			return nil, errors.New("workspace is not clean before the commit -- refusing to mix pinned files with other changes")
		}
		if err := validate(dir, loaded, resultSHA); err != nil {
			return nil, err
		}
		if err := write(dir, loaded, message); err != nil {
			return nil, err
		}
		res.Committed = true
	}
	for _, f := range loaded {
		got, existed, err := runner.GitShowFile(dir, "HEAD", f.Path)
		if err != nil {
			return nil, err
		}
		if !existed || HashBytes([]byte(got)) != f.SHA256 {
			return nil, fmt.Errorf("committed blob for %q does not hash to the pinned hash", f.Path)
		}
	}
	if clean, err := runner.GitIsClean(dir); err != nil || !clean {
		return nil, fmt.Errorf("workspace not clean after the commit (err=%v)", err)
	}
	return res, nil
}

// alreadyCommitted reports whether HEAD is a child of resultSHA that changed
// exactly paths. HEAD at resultSHA is not committed yet; any other HEAD is an
// error.
func alreadyCommitted(dir, resultSHA string, paths []string) (bool, error) {
	head, err := gitOut(dir, "rev-parse", "HEAD")
	if err != nil {
		return false, err
	}
	if head == resultSHA {
		return false, nil
	}
	parent, err := gitOut(dir, "rev-parse", "HEAD^")
	if err != nil || parent != resultSHA {
		return false, fmt.Errorf("HEAD %s is neither the result commit %s nor its child", head, resultSHA)
	}
	changed, err := gitOut(dir, "diff", "--name-only", "-z", resultSHA, "HEAD")
	if err != nil {
		return false, err
	}
	got := strings.Split(strings.TrimRight(changed, "\x00"), "\x00")
	sort.Strings(got)
	want := append([]string{}, paths...)
	sort.Strings(want)
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		return false, fmt.Errorf("HEAD %s is a child of the result commit that changed other paths than the pinned files", head)
	}
	return true, nil
}

func gitOut(dir string, args ...string) (string, error) {
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w", args[0], err)
	}
	return strings.TrimSpace(string(out)), nil
}

func validate(dir string, files []loaded, resultSHA string) error {
	for _, f := range files {
		if _, existed, err := runner.GitShowFile(dir, resultSHA, f.Path); err != nil {
			return err
		} else if existed {
			return fmt.Errorf("path %q already exists at the result commit: refusing to overwrite ordinary source", f.Path)
		}
		existing, err := os.Lstat(filepath.Join(dir, filepath.FromSlash(f.Path)))
		if err == nil {
			have, rerr := os.ReadFile(filepath.Join(dir, filepath.FromSlash(f.Path)))
			if !existing.Mode().IsRegular() || rerr != nil || !bytes.Equal(have, f.bytes) {
				return fmt.Errorf("path %q already exists in the workspace with other content: refusing to overwrite a file the gates ran against", f.Path)
			}
		} else if !os.IsNotExist(err) {
			return err
		}
		if err := checkNoLinks(dir, f.Path); err != nil {
			return err
		}
	}
	return nil
}

func write(dir string, files []loaded, message string) error {
	var stage []string
	for _, f := range files {
		full := filepath.Join(dir, filepath.FromSlash(f.Path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return fmt.Errorf("create directory for %q: %w", f.Path, err)
		}
		if err := os.WriteFile(full, f.bytes, 0o644); err != nil {
			return err
		}
		stage = append(stage, f.Path)
	}
	// -f: a path matched by .gitignore must still be committed.
	addArgs := append([]string{"-C", dir, "--literal-pathspecs", "-c", "core.autocrlf=false", "add", "-f", "-A", "--"}, stage...)
	if out, err := exec.Command("git", addArgs...).CombinedOutput(); err != nil {
		return fmt.Errorf("git add pinned files: %w: %s", err, out)
	}
	if exec.Command("git", "-C", dir, "diff", "--cached", "--quiet").Run() == nil {
		return errors.New("the pinned files changed nothing")
	}
	if out, err := exec.Command("git", "-C", dir, "-c", "core.hooksPath=/dev/null", "commit", "-m", message).CombinedOutput(); err != nil {
		return fmt.Errorf("git commit pinned files: %w: %s", err, out)
	}
	return nil
}

// unstagePaths resets the index entries of exactly the given paths to HEAD
// (`git reset -q -- <paths>`), never touching the worktree or any other path.
func unstagePaths(dir string, paths map[string][]byte) error {
	args := []string{"-C", dir, "--literal-pathspecs", "reset", "-q", "--"}
	for p := range paths {
		args = append(args, p)
	}
	if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("unstage stale paths: %w: %s", err, out)
	}
	return nil
}

// cleanApartFromResidue reports whether the worktree is clean except for
// paths in residue whose worktree state already equals the given bytes. Any
// other dirty path, any rename/copy entry, or a residue path holding
// different content is dirty. Untracked files are listed individually
// (-uall) so a new directory cannot hide a stray file.
func cleanApartFromResidue(dir string, residue map[string][]byte) (bool, error) {
	out, err := exec.Command("git", "-C", dir, "status", "--porcelain", "-z", "-uall").Output()
	if err != nil {
		return false, fmt.Errorf("git status: %w", err)
	}
	for _, entry := range strings.Split(strings.TrimRight(string(out), "\x00"), "\x00") {
		if entry == "" {
			continue
		}
		if len(entry) < 4 {
			return false, nil
		}
		if (entry[0] != ' ' && entry[0] != '?') || entry[1] == 'R' || entry[1] == 'C' {
			return false, nil
		}
		want, ok := residue[entry[3:]]
		if !ok {
			return false, nil
		}
		have, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(entry[3:])))
		if err != nil || !bytes.Equal(have, want) {
			return false, nil
		}
	}
	return true, nil
}

// checkNoLinks refuses when any existing component of rel under dir is a
// symlink or (for the final component) a non-regular file: a host write
// through an agent-planted symlink would escape the worktree.
func checkNoLinks(dir, rel string) error {
	cur := dir
	segs := strings.Split(rel, "/")
	for i, seg := range segs {
		cur = filepath.Join(cur, seg)
		info, err := os.Lstat(cur)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("path %q traverses a symlink at %q", rel, seg)
		}
		if i < len(segs)-1 && !info.IsDir() {
			return fmt.Errorf("path %q has a non-directory component %q", rel, seg)
		}
		if i == len(segs)-1 && !info.Mode().IsRegular() {
			return fmt.Errorf("path %q exists and is not a regular file", rel)
		}
	}
	return nil
}
