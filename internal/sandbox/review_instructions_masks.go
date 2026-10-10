package sandbox

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Destination and mask validation: a snapshot's destination lies outside the
// workspace, and a workspace mask (a snapshot's or any launch's) has a target
// and a source a bind mount can carry, with no two targets overlapping.

// requireDestinationOutside refuses a dst that is, lies inside, or contains
// workDir, after resolving the symlinks of its nearest existing ancestor.
func requireDestinationOutside(workDir, dst string) error {
	if !filepath.IsAbs(dst) || filepath.Clean(dst) == string(filepath.Separator) {
		return fmt.Errorf("review instructions: destination %q must be an absolute, non-root path", dst)
	}
	root, err := filepath.EvalSymlinks(workDir)
	if err != nil {
		return fmt.Errorf("review instructions: resolve workspace: %w", err)
	}
	resolved, err := resolveExistingPrefix(dst)
	if err != nil {
		return fmt.Errorf("review instructions: resolve destination: %w", err)
	}
	if pathWithin(root, resolved) || pathWithin(resolved, root) {
		return fmt.Errorf("review instructions: destination %s overlaps the workspace %s", dst, workDir)
	}
	return nil
}

func resolveExistingPrefix(p string) (string, error) {
	p = filepath.Clean(p)
	rest := ""
	for {
		resolved, err := filepath.EvalSymlinks(p)
		if err == nil {
			return filepath.Join(resolved, rest), nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(p)
		if parent == p {
			return "", err
		}
		rest = filepath.Join(filepath.Base(p), rest)
		p = parent
	}
}

// pathWithin reports whether p is root or below it.
func pathWithin(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// mountUnsafe reports a control character or one of : , " \ (they break a bind
// argument).
func mountUnsafe(s string) bool {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c < 0x20 || c == 0x7f || strings.IndexByte(`:,"\`, c) >= 0 {
			return true
		}
	}
	return false
}

// validateMaskTarget checks a mask target as a launch and a snapshot both
// need it: nothing a mount argument cannot carry, no empty, "." or ".."
// component, and no component that folds to .git.
func validateMaskTarget(target string) error {
	if target == "" {
		return errors.New("workspace mask target is empty")
	}
	if mountUnsafe(target) {
		return fmt.Errorf("workspace mask target %q has a character a mount argument cannot carry", target)
	}
	for _, part := range strings.Split(target, "/") {
		if part == "" || part == "." || part == ".." || foldName(part) == foldedGit {
			return fmt.Errorf("workspace mask target %q may not use component %q", target, part)
		}
	}
	return nil
}

// validateWorkspaceMask checks one mask: its source is an absolute path a
// mount argument can carry to an existing regular file (or directory when Dir)
// that is not a symlink, and its target is a safe relative path that does not
// overlap the reference-oracle mount (oraclePath, "" when none), compared by
// fold.
func validateWorkspaceMask(m WorkspaceMask, oraclePath string) error {
	if err := validateMaskTarget(m.Target); err != nil {
		return err
	}
	if oraclePath != "" {
		oracle := foldName(filepath.ToSlash(filepath.Clean(oraclePath)))
		if t := foldName(m.Target); pathWithin(oracle, t) || pathWithin(t, oracle) {
			return fmt.Errorf("workspace mask target %q overlaps the reference-oracle mount %q", m.Target, oraclePath)
		}
	}
	if !filepath.IsAbs(m.Source) {
		return fmt.Errorf("workspace mask source %q must be absolute", m.Source)
	}
	if mountUnsafe(m.Source) {
		return fmt.Errorf("workspace mask source %q has a character a mount argument cannot carry", m.Source)
	}
	info, err := os.Lstat(m.Source)
	if err != nil {
		return fmt.Errorf("workspace mask source for %s: %w", m.Target, err)
	}
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		return fmt.Errorf("workspace mask source for %s is a symlink", m.Target)
	case m.Dir && !info.IsDir():
		return fmt.Errorf("workspace mask source for %s is not a directory", m.Target)
	case !m.Dir && !info.Mode().IsRegular():
		return fmt.Errorf("workspace mask source for %s is not a regular file", m.Target)
	}
	return nil
}

// validateWorkspaceMasks checks every mask and that no two overlap, by fold.
func validateWorkspaceMasks(masks []WorkspaceMask, oraclePath string) error {
	for i, m := range masks {
		if err := validateWorkspaceMask(m, oraclePath); err != nil {
			return err
		}
		for _, other := range masks[:i] {
			a, b := foldName(other.Target), foldName(m.Target)
			if pathWithin(a, b) || pathWithin(b, a) {
				return fmt.Errorf("workspace mask targets %q and %q overlap", other.Target, m.Target)
			}
		}
	}
	return nil
}
