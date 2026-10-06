// Package operatorskill embeds the "buildgate" agent skill (SKILL.md, in
// agentskills.io format) that GitHub Copilot, Claude Code, and Codex load
// from a user-level skills directory to act as the operator's front end to
// factoryd. Operators run the released `factoryd` binary without a clone of
// this repo, so the skill's source of truth lives here, versioned with the
// exact CLI it describes, and `factoryd install-skill` copies it out to
// wherever the operator's agent expects it.
package operatorskill

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// FS holds the embedded buildgate/ skill tree (currently just SKILL.md).
//
//go:embed buildgate
var FS embed.FS

// Install writes the embedded buildgate/ skill tree to
// <skillsDir>/buildgate/, creating directories as needed. It overwrites
// every file the skill ships -- so re-running it after a factoryd upgrade
// picks up any SKILL.md changes -- but never touches other files an
// operator or a different skill has placed in skillsDir. It returns the
// installed directory path.
//
// A symlinked <skillsDir>/buildgate is refused rather than written through:
// skills directories are commonly symlinks into a dotfiles repo (this
// author's own ~/.claude/skills is), and silently overwriting a tracked
// file there is worse than asking the operator to update it at the source.
// Each file is written to a temp file and renamed into place, so an agent
// loading skills mid-install never reads a truncated SKILL.md.
func Install(skillsDir string) (string, error) {
	dest := filepath.Join(skillsDir, "buildgate")
	if info, err := os.Lstat(dest); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("%s is a symlink; update the skill at its target or remove the link, then re-run", dest)
	}
	err := fs.WalkDir(FS, "buildgate", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		target := filepath.Join(skillsDir, path)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := FS.ReadFile(path)
		if err != nil {
			return err
		}
		return writeFileAtomic(target, data)
	})
	if err != nil {
		return "", err
	}
	return dest, nil
}

// writeFileAtomic writes data to a temp file beside target, then renames it
// over target. rename replaces a symlink at target with a regular file
// instead of following it.
func writeFileAtomic(target string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(target), ".install-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op once the rename succeeds
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), target)
}
