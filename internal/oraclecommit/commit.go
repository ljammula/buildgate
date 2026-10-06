package oraclecommit

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"buildgate/internal/run"
	"buildgate/internal/runner"
	"buildgate/internal/sandbox"
)

// IndexRow is one row of the committed .buildgate/oracles.json index.
type IndexRow struct {
	TargetPath     string `json:"target_path"`
	SHA256         string `json:"sha256"`
	RequestID      string `json:"request_id"`
	CriterionIndex int    `json:"criterion_index"`
}

// ParseIndex decodes an index. Strict: an unreadable index is an error, not
// "no protection" -- only an ABSENT index means no extra protection.
func ParseIndex(data []byte) ([]IndexRow, error) {
	var rows []IndexRow
	if err := json.Unmarshal(data, &rows); err != nil {
		return nil, fmt.Errorf("parse %s: %w", IndexPath, err)
	}
	for i, r := range rows {
		if r.TargetPath == "" {
			return nil, fmt.Errorf("%s row %d has no target_path", IndexPath, i)
		}
	}
	return rows, nil
}

// MarshalIndex renders rows deterministically (sorted by path).
func MarshalIndex(rows []IndexRow) ([]byte, error) {
	sorted := append([]IndexRow{}, rows...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].TargetPath < sorted[j].TargetPath })
	out, err := json.MarshalIndent(sorted, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

// MergeIndex returns base with rows for superseded paths and for paths
// being re-written removed, plus add. Other base rows are kept.
func MergeIndex(base, add []IndexRow, superseded []string) []IndexRow {
	drop := map[string]bool{}
	for _, s := range superseded {
		drop[s] = true
	}
	for _, a := range add {
		drop[a.TargetPath] = true
	}
	var out []IndexRow
	for _, b := range base {
		if !drop[b.TargetPath] {
			out = append(out, b)
		}
	}
	return append(out, add...)
}

// BaseIndex reads .buildgate/oracles.json from commit in dir. No index at
// that commit is (nil, nil): no extra protection, not an error.
func BaseIndex(dir, commit string) ([]IndexRow, error) {
	content, existed, err := runner.GitShowFile(dir, commit, IndexPath)
	if err != nil {
		return nil, fmt.Errorf("read %s at %s: %w", IndexPath, commit, err)
	}
	if !existed {
		return nil, nil
	}
	return ParseIndex([]byte(content))
}

// SnapshotPlan takes a fresh immutable snapshot of sourceDir, requires its
// tree hash to equal pinnedTreeHash (the hash the reference_oracle gate
// recorded for what it actually ran against), loads the plan from that
// snapshot and removes it. A mismatch is an error: the bytes about to be
// committed would not be the bytes that were verified. (nil, nil) is the
// inert case (no target_path declared).
func SnapshotPlan(workDir, sourceDir, snapshotDir, pinnedTreeHash string, extraProtected []string) (*Plan, error) {
	if sourceDir == "" || pinnedTreeHash == "" {
		return nil, nil
	}
	got, err := sandbox.SnapshotReferenceOracle(workDir, sourceDir, snapshotDir)
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(snapshotDir)
	plan, err := LoadPlan(snapshotDir, extraProtected)
	if errors.Is(err, ErrMalformedManifest) {
		// Cannot declare a target_path: inert, like a manifest without one.
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if plan == nil {
		// Inert whatever the hash: a directory that changed after the gate
		// ran but declares no target_path commits nothing.
		return nil, nil
	}
	if got != pinnedTreeHash {
		return nil, fmt.Errorf("reference-oracle content changed since the reference_oracle gate ran (gate hash %s, now %s): refusing to commit unverified oracle bytes", pinnedTreeHash, got)
	}
	return plan, nil
}

// Result reports what Apply did.
type Result struct {
	Committed bool
	Authored  []run.OracleFile
	Deleted   []string
}

// Apply writes plan's oracle files and the merged index into dir, removes
// superseded paths and commits them in one host-side commit. dir must be a
// clean worktree whose HEAD descends from baseSHA. It is idempotent: if the
// exact content is already committed at HEAD (a retried Temporal Activity)
// it commits nothing and still verifies the blobs.
//
// Fail-closed checks, all before any write:
//   - a target_path may not exist at baseSHA unless the base index lists it
//     (an oracle may add a file or replace an earlier oracle, never
//     overwrite ordinary source);
//   - every superseded path must be listed in the base index (a manifest
//     can retire oracles, not delete arbitrary files);
//   - no existing component of any written path may be a symlink.
//
// After the commit every written blob is re-read from HEAD and compared to
// the pinned hash, so a .gitattributes eol/filter rewrite cannot make the
// committed bytes differ from the verified snapshot.
func Apply(dir string, plan *Plan, baseSHA, requestID, message string) (*Result, error) {
	if plan == nil || (len(plan.Files) == 0 && len(plan.Supersedes) == 0) {
		return &Result{}, nil
	}
	base, err := BaseIndex(dir, baseSHA)
	if err != nil {
		return nil, err
	}
	var add []IndexRow
	for _, f := range plan.Files {
		add = append(add, IndexRow{TargetPath: f.TargetPath, SHA256: f.SHA256, RequestID: requestID, CriterionIndex: f.CriterionIndex})
	}
	indexBytes, err := MarshalIndex(MergeIndex(base, add, plan.Supersedes))
	if err != nil {
		return nil, err
	}
	// A retried Activity may find the residue of a crash between `git add` and
	// `git commit`: this plan's own paths staged (and written). Unstage exactly
	// those paths, then accept a remaining difference only where the worktree
	// already holds what this plan would write; anything else stays "not clean".
	residue := map[string][]byte{IndexPath: indexBytes}
	for _, f := range plan.Files {
		residue[f.TargetPath] = f.Bytes
	}
	for _, s := range plan.Supersedes {
		residue[s] = nil
	}
	if err := unstagePaths(dir, residue); err != nil {
		return nil, err
	}
	clean, err := cleanApartFromResidue(dir, residue)
	if err != nil {
		return nil, fmt.Errorf("check workspace cleanliness before oracle commit: %w", err)
	}
	if !clean {
		return nil, errors.New("workspace is not clean before oracle commit -- refusing to mix factory oracle writes with other changes")
	}
	if err := validatePlan(dir, plan, baseSHA, base, false); err != nil {
		return nil, err
	}

	var stage []string
	for _, f := range plan.Files {
		if err := writeFile(dir, f.TargetPath, f.Bytes); err != nil {
			return nil, err
		}
		stage = append(stage, f.TargetPath)
	}
	if err := writeFile(dir, IndexPath, indexBytes); err != nil {
		return nil, err
	}
	stage = append(stage, IndexPath)
	for _, s := range plan.Supersedes {
		if err := os.Remove(filepath.Join(dir, filepath.FromSlash(s))); err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("remove superseded oracle %q: %w", s, err)
		}
		stage = append(stage, s)
	}

	// -f: a target_path matched by .gitignore must still be committed.
	addArgs := append([]string{"-C", dir, "--literal-pathspecs", "-c", "core.autocrlf=false", "add", "-f", "-A", "--"}, stage...)
	if out, err := exec.Command("git", addArgs...).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("git add oracle files: %w: %s", err, out)
	}
	res := &Result{}
	if exec.Command("git", "-C", dir, "diff", "--cached", "--quiet").Run() != nil {
		if out, err := exec.Command("git", "-C", dir, "-c", "core.hooksPath=/dev/null", "commit", "-m", message).CombinedOutput(); err != nil {
			return nil, fmt.Errorf("git commit oracles: %w: %s", err, out)
		}
		res.Committed = true
	}

	for _, f := range plan.Files {
		got, existed, err := runner.GitShowFile(dir, "HEAD", f.TargetPath)
		if err != nil {
			return nil, err
		}
		if !existed || HashBytes([]byte(got)) != f.SHA256 {
			return nil, fmt.Errorf("committed blob for %q does not hash to the pinned oracle hash", f.TargetPath)
		}
		res.Authored = append(res.Authored, run.OracleFile{Path: f.TargetPath, SHA256: f.SHA256})
	}
	got, existed, err := runner.GitShowFile(dir, "HEAD", IndexPath)
	if err != nil {
		return nil, err
	}
	if !existed || HashBytes([]byte(got)) != HashBytes(indexBytes) {
		return nil, fmt.Errorf("committed %s does not match the merged index", IndexPath)
	}
	res.Authored = append(res.Authored, run.OracleFile{Path: IndexPath, SHA256: HashBytes(indexBytes)})
	for _, s := range plan.Supersedes {
		if _, existed, err := runner.GitShowFile(dir, "HEAD", s); err != nil {
			return nil, err
		} else if existed {
			return nil, fmt.Errorf("superseded oracle %q is still present after the commit", s)
		}
		res.Deleted = append(res.Deleted, s)
	}
	if clean, err := runner.GitIsClean(dir); err != nil || !clean {
		return nil, fmt.Errorf("workspace not clean after oracle commit (err=%v)", err)
	}
	return res, nil
}

// Validate runs every fail-closed check Apply runs before it writes anything
// (target_path collisions with ordinary source or with a file the agent
// planted, supersedes, symlinks) and writes nothing. It is what a request that
// opted out of the oracle commit still gets: an agent cannot use the opt-out to
// smuggle its own bytes in at an approved target_path.
func Validate(dir string, plan *Plan, baseSHA string) error {
	if plan == nil || (len(plan.Files) == 0 && len(plan.Supersedes) == 0) {
		return nil
	}
	base, err := BaseIndex(dir, baseSHA)
	if err != nil {
		return err
	}
	return validatePlan(dir, plan, baseSHA, base, true)
}

// strict (validate-only) also refuses a byte-identical pre-existing file: Apply
// tolerates it because it commits nothing new, but under the opt-out the host
// commits nothing, so the agent's identical file would ride the ordinary
// safety-net commit.
func validatePlan(dir string, plan *Plan, baseSHA string, base []IndexRow, strict bool) error {
	inBase := map[string]bool{}
	for _, r := range base {
		inBase[r.TargetPath] = true
	}
	for _, f := range plan.Files {
		if inBase[f.TargetPath] {
			continue
		}
		if _, existed, err := runner.GitShowFile(dir, baseSHA, f.TargetPath); err != nil {
			return err
		} else if existed {
			return fmt.Errorf("target_path %q already exists at the base commit and is not a committed oracle: refusing to overwrite ordinary source", f.TargetPath)
		}
	}
	// A file the agent created this run at a target_path must not be silently
	// overwritten: the gates (canonical verify, full suite) ran against the
	// agent's file, then the host would swap it, so the accepted tree would be
	// one no gate ran (found via adversarial review; the agent can read the
	// oracle mount, so it knows the target). Only an absent path or a
	// byte-identical file is allowed. Paths already committed as oracles at
	// base are handled by base-index protection instead.
	for _, f := range plan.Files {
		if inBase[f.TargetPath] {
			continue
		}
		existing, err := os.Lstat(filepath.Join(dir, filepath.FromSlash(f.TargetPath)))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if !existing.Mode().IsRegular() {
			return fmt.Errorf("target_path %q exists in the workspace and is not a regular file: refusing to replace it", f.TargetPath)
		}
		have, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(f.TargetPath)))
		if err != nil {
			return err
		}
		if strict {
			return fmt.Errorf("target_path %q already exists in the workspace and this request opted out of committing oracles: refusing an agent-supplied file at an oracle target", f.TargetPath)
		}
		if !bytes.Equal(have, f.Bytes) {
			return fmt.Errorf("target_path %q already exists in the workspace with different bytes than the pinned oracle: refusing to overwrite a file the gates ran against", f.TargetPath)
		}
	}
	for _, s := range plan.Supersedes {
		if !inBase[s] {
			return fmt.Errorf("supersedes %q is not listed in %s at the base commit: only committed oracles can be superseded", s, IndexPath)
		}
	}

	for _, f := range plan.Files {
		if err := checkNoSymlinks(dir, f.TargetPath); err != nil {
			return err
		}
	}
	for _, s := range plan.Supersedes {
		if err := checkNoSymlinks(dir, s); err != nil {
			return err
		}
	}
	if err := checkNoSymlinks(dir, IndexPath); err != nil {
		return err
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
		return fmt.Errorf("unstage stale oracle paths: %w: %s", err, out)
	}
	return nil
}

// cleanApartFromResidue reports whether the worktree is clean except for
// paths in residue whose worktree state already equals what Apply would write
// (the given bytes; nil means the file must be absent). Any other dirty path,
// any rename/copy entry, or a residue path holding different content is dirty.
// Untracked files are listed individually (-uall) so a new directory cannot
// hide a stray file.
func cleanApartFromResidue(dir string, residue map[string][]byte) (bool, error) {
	out, err := exec.Command("git", "-C", dir, "status", "--porcelain", "-z", "-uall").Output()
	if err != nil {
		return false, fmt.Errorf("git status: %w", err)
	}
	fields := strings.Split(strings.TrimRight(string(out), "\x00"), "\x00")
	for i := 0; i < len(fields); i++ {
		entry := fields[i]
		if entry == "" {
			continue
		}
		if len(entry) < 4 {
			return false, nil
		}
		// Anything still staged is unrelated work (the plan's own paths were
		// just unstaged); renames and copies are never plan residue.
		if (entry[0] != ' ' && entry[0] != '?') || entry[1] == 'R' || entry[1] == 'C' {
			return false, nil
		}
		want, ok := residue[entry[3:]]
		if !ok {
			return false, nil
		}
		have, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(entry[3:])))
		switch {
		case want == nil && os.IsNotExist(err):
		case want != nil && err == nil && bytes.Equal(have, want):
		default:
			return false, nil
		}
	}
	return true, nil
}

// checkNoSymlinks refuses when any existing component of rel under dir is a
// symlink or (for the final component) a non-regular file: a host write
// through an agent-planted symlink would escape the worktree.
func checkNoSymlinks(dir, rel string) error {
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

func writeFile(dir, rel string, data []byte) error {
	full := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return fmt.Errorf("create directory for %q: %w", rel, err)
	}
	return os.WriteFile(full, data, 0o644)
}

// CollectEvidence builds the run.OracleEvidence for a finished run from the
// committed state: the index at baseSHA, the factory writes just committed
// (authored/deleted, nil when none), and the blob hashes at baseSHA and
// resultSHA of every relevant path this run changed. It returns nil when
// there is nothing oracle-related (no base index, no factory writes, no
// changed .buildgate/ path), which keeps every ordinary run's record and
// gate inputs identical to before this mechanism existed.
func CollectEvidence(dir, baseSHA, resultSHA string, changed []string, authored []run.OracleFile, deleted []string) (*run.OracleEvidence, error) {
	base, err := BaseIndex(dir, baseSHA)
	if err != nil {
		return nil, err
	}
	candidates := map[string]bool{}
	ev := &run.OracleEvidence{Authored: authored, Deleted: deleted}
	for _, r := range base {
		ev.BaseIndexPaths = append(ev.BaseIndexPaths, r.TargetPath)
		if r.SHA256 != "" {
			if ev.BaseIndexSHA256 == nil {
				ev.BaseIndexSHA256 = map[string]string{}
			}
			ev.BaseIndexSHA256[r.TargetPath] = r.SHA256
		}
	}
	sort.Strings(ev.BaseIndexPaths)
	for _, p := range ev.BaseIndexPaths {
		candidates[p] = true
	}
	for _, a := range authored {
		candidates[a.Path] = true
	}
	for _, d := range deleted {
		candidates[d] = true
	}
	changedSet := map[string]bool{}
	underIndexDir := false
	for _, c := range changed {
		changedSet[c] = true
		if c == IndexDir || strings.HasPrefix(c, IndexDir+"/") {
			candidates[c] = true
			underIndexDir = true
		}
	}
	if len(base) == 0 && len(authored) == 0 && len(deleted) == 0 && !underIndexDir {
		return nil, nil
	}
	authoredSet := map[string]bool{}
	for _, a := range authored {
		authoredSet[a.Path] = true
	}
	for _, d := range deleted {
		authoredSet[d] = true
	}
	for p := range candidates {
		if !changedSet[p] && !authoredSet[p] {
			continue
		}
		if h, ok, err := blobHash(dir, resultSHA, p); err != nil {
			return nil, err
		} else if ok {
			if ev.ResultSHA256 == nil {
				ev.ResultSHA256 = map[string]string{}
			}
			ev.ResultSHA256[p] = h
		}
		if h, ok, err := blobHash(dir, baseSHA, p); err != nil {
			return nil, err
		} else if ok {
			if ev.BaseSHA256 == nil {
				ev.BaseSHA256 = map[string]string{}
			}
			ev.BaseSHA256[p] = h
		}
	}
	return ev, nil
}

func blobHash(dir, commit, p string) (string, bool, error) {
	content, existed, err := runner.GitShowFile(dir, commit, p)
	if err != nil {
		return "", false, err
	}
	if !existed {
		return "", false, nil
	}
	return HashBytes([]byte(content)), true, nil
}
