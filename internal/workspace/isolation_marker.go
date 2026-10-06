package workspace

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const IsolationMarkerVersion = 1

// IsolationMarker is the durable ownership proof for one factory-created
// isolated worktree. It is written before Prepare so a crash cannot leave a
// worktree with no factory-owned identity to reconcile.
type IsolationMarker struct {
	Version       int    `json:"version"`
	RunID         string `json:"run_id"`
	WorktreeID    string `json:"worktree_id"`
	Mode          string `json:"mode"`
	RepoDir       string `json:"repo_dir"`
	CommonDir     string `json:"common_dir"`
	DataDir       string `json:"data_dir"`
	ParentDir     string `json:"parent_dir"`
	WorktreePath  string `json:"worktree_path"`
	Branch        string `json:"branch"`
	Prepared      bool   `json:"prepared"`
	WorkflowID    string `json:"workflow_id,omitempty"`
	ActivityRunID string `json:"activity_run_id,omitempty"`
	ActivityID    string `json:"activity_id,omitempty"`
	CheckpointDir string `json:"checkpoint_dir,omitempty"`
}

func IsolationMarkerPath(dataDir, runID string) string {
	return filepath.Join(dataDir, "isolation-markers", runID+".json")
}

// ValidateIsolationRunID accepts only a single safe filesystem component.
// Callers must invoke it before constructing IsolationMarkerPath from
// workflow-supplied input.
func ValidateIsolationRunID(runID string) error {
	if runID == "" || runID == "." || runID == ".." || strings.ContainsAny(runID, `/\`) {
		return fmt.Errorf("invalid isolation run ID %q", runID)
	}
	return nil
}

// WriteIsolationMarker atomically replaces one marker. Callers write the
// unprepared marker before Prepare and the prepared marker immediately after
// it succeeds.
func WriteIsolationMarker(path string, marker IsolationMarker) error {
	if marker.Version == 0 {
		marker.Version = IsolationMarkerVersion
	}
	b, err := json.MarshalIndent(marker, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal isolation marker: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("create isolation marker directory: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".isolation-marker-*.tmp")
	if err != nil {
		return fmt.Errorf("create isolation marker temporary file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("protect isolation marker temporary file: %w", err)
	}
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write isolation marker: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close isolation marker temporary file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("install isolation marker: %w", err)
	}
	return nil
}

func LoadIsolationMarker(path string) (IsolationMarker, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return IsolationMarker{}, err
	}
	var marker IsolationMarker
	if err := json.Unmarshal(b, &marker); err != nil {
		return IsolationMarker{}, fmt.Errorf("unmarshal isolation marker %q: %w", path, err)
	}
	return marker, nil
}

func RemoveIsolationMarker(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func ListIsolationMarkerPaths(dataDir string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(dataDir, "isolation-markers"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		paths = append(paths, filepath.Join(dataDir, "isolation-markers", entry.Name()))
	}
	return paths, nil
}

// CanonicalPath resolves path to an absolute, symlink-free form: unlike a
// bare filepath.EvalSymlinks, it first makes a relative path absolute
// (resolved against the current process's working directory, matching
// filepath.Abs), then walks up to the nearest existing ancestor to resolve
// symlinks -- so it also works on a path (or a path's trailing component)
// that doesn't exist on disk yet, reattaching the missing suffix afterward.
//
// Exists because callers on both sides of a marker's lifetime disagree
// about which form they hand in unless something normalizes both: the
// marker is written with a value already produced by this same resolution
// (via factoryd's own -data-dir/-workspace canonicalization, done once,
// early, before any run artifact exists), but a later reconciliation pass
// re-deriving "the current data dir" from a bare relative flag default
// (-data-dir's own default is the literal string "data", never made
// absolute before reaching here) got only filepath.EvalSymlinks, which
// leaves a relative input relative -- so every comparison against the
// marker's stored absolute path failed, permanently and on every
// invocation, exactly the "preserving inconclusive marker" case
// ValidateIsolationMarker's own doc comment treats as an operator-visible
// but resolvable ambiguity, not this: a marker that was always exactly
// this repository's own, misreported as inconclusive by a string-format
// mismatch neither side could see (observed live, 2026-09-06: every
// run logged this for every marker -- reconciliation of stale isolation
// markers was silently inert).
func CanonicalPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	current := filepath.Clean(abs)
	var suffix []string
	for {
		if _, statErr := os.Lstat(current); statErr == nil {
			resolved, err := filepath.EvalSymlinks(current)
			if err != nil {
				return "", err
			}
			for i := len(suffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, suffix[i])
			}
			return filepath.Clean(resolved), nil
		} else if !os.IsNotExist(statErr) {
			return "", statErr
		}
		parent := filepath.Dir(current)
		if parent == current {
			return current, nil
		}
		suffix = append(suffix, filepath.Base(current))
		current = parent
	}
}

// ValidateIsolationMarker proves that marker identifies exactly one
// factory-owned worktree in repoDir. It intentionally rejects ambiguity.
func ValidateIsolationMarker(marker IsolationMarker, dataDir, repoDir string) error {
	if marker.Version != IsolationMarkerVersion {
		return fmt.Errorf("unsupported isolation marker version %d", marker.Version)
	}
	if err := ValidateIsolationRunID(marker.RunID); err != nil {
		return err
	}
	if marker.Mode != "direct" && marker.Mode != "temporal" {
		return fmt.Errorf("invalid isolation marker mode %q", marker.Mode)
	}
	if err := ValidateIsolationRunID(marker.WorktreeID); err != nil {
		return fmt.Errorf("invalid isolation marker worktree ID: %w", err)
	}
	if marker.Branch != "factoryd/"+marker.WorktreeID {
		return fmt.Errorf("isolation marker branch %q does not match worktree ID %q", marker.Branch, marker.WorktreeID)
	}
	canonicalRepo, err := CanonicalPath(repoDir)
	if err != nil {
		return fmt.Errorf("resolve repository: %w", err)
	}
	markerRepo, err := CanonicalPath(marker.RepoDir)
	if err != nil || filepath.Clean(markerRepo) != filepath.Clean(canonicalRepo) {
		return fmt.Errorf("isolation marker repository %q does not match %q", marker.RepoDir, canonicalRepo)
	}
	commonDir, err := GitCommonDir(repoDir)
	if err != nil {
		return err
	}
	if filepath.Clean(marker.CommonDir) != filepath.Clean(commonDir) {
		return fmt.Errorf("isolation marker common directory %q does not match %q", marker.CommonDir, commonDir)
	}
	canonicalDataDir, err := CanonicalPath(dataDir)
	if err != nil {
		return fmt.Errorf("resolve data directory: %w", err)
	}
	markerDataDir, err := CanonicalPath(marker.DataDir)
	if err != nil || filepath.Clean(markerDataDir) != filepath.Clean(canonicalDataDir) {
		return fmt.Errorf("isolation marker data directory %q does not match %q", marker.DataDir, canonicalDataDir)
	}
	parent := comparablePath(marker.ParentDir)
	expectedParent := comparablePath(filepath.Join(canonicalDataDir, "workspaces"))
	if parent != expectedParent {
		return fmt.Errorf("isolation marker parent directory %q does not match %q", marker.ParentDir, expectedParent)
	}
	worktree := filepath.Clean(marker.WorktreePath)
	if !filepath.IsAbs(worktree) || comparablePath(filepath.Join(parent, marker.WorktreeID)) != comparablePath(worktree) {
		return fmt.Errorf("isolation marker worktree path %q is not the expected path under %q", marker.WorktreePath, parent)
	}
	return nil
}

// ReapIsolationMarker removes only the exact registered worktree and branch
// described by a validated marker. An existing path not registered by Git or
// a registration with another branch is deliberately left untouched.
func ReapIsolationMarker(repoDir string, marker IsolationMarker) error {
	unlock, err := lockGitMetadata(repoDir)
	if err != nil {
		return err
	}
	defer unlock()
	registered, err := registeredWorktreeBranch(repoDir, marker.WorktreePath)
	if err != nil {
		return err
	}
	if registered != "" && registered != "refs/heads/"+marker.Branch {
		return fmt.Errorf("worktree %q is registered to %q, not factory branch %q", marker.WorktreePath, registered, marker.Branch)
	}
	if registered == "" {
		if _, err := os.Stat(marker.WorktreePath); err == nil {
			return fmt.Errorf("expected worktree path %q exists but is not registered by Git", marker.WorktreePath)
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("stat expected worktree path %q: %w", marker.WorktreePath, err)
		}
	}
	if registered != "" {
		out, err := gitMutate(repoDir, "worktree", "remove", "--force", marker.WorktreePath)
		if err != nil {
			return fmt.Errorf("remove isolated worktree: %w: %s", err, out)
		}
	}
	if _, err := exec.Command("git", "-C", repoDir, "show-ref", "--verify", "--quiet", "refs/heads/"+marker.Branch).CombinedOutput(); err != nil {
		return nil
	}
	out, err := gitMutate(repoDir, "branch", "-D", marker.Branch)
	if err != nil {
		if strings.Contains(string(out), "not found") || strings.Contains(string(out), "not a branch") {
			return nil
		}
		return fmt.Errorf("remove isolated branch: %w: %s", err, out)
	}
	return nil
}

func registeredWorktreeBranch(repoDir, wantedPath string) (string, error) {
	out, err := exec.Command("git", "-C", repoDir, "worktree", "list", "--porcelain").Output()
	if err != nil {
		return "", fmt.Errorf("list Git worktrees: %w", err)
	}
	wanted := comparablePath(wantedPath)
	var currentPath string
	for _, line := range strings.Split(string(out), "\n") {
		if path, ok := strings.CutPrefix(line, "worktree "); ok {
			currentPath = comparablePath(path)
			continue
		}
		if branch, ok := strings.CutPrefix(line, "branch "); ok && currentPath == wanted {
			return strings.TrimSpace(branch), nil
		}
	}
	return "", nil
}

// comparablePath is CanonicalPath with a best-effort fallback instead of an
// error return, for callers (worktree-path comparisons below) that only
// need "as canonical as achievable" and have no error path of their own to
// report a resolution failure through.
func comparablePath(path string) string {
	if canonical, err := CanonicalPath(path); err == nil {
		return canonical
	}
	if abs, err := filepath.Abs(path); err == nil {
		return filepath.Clean(abs)
	}
	return filepath.Clean(path)
}
