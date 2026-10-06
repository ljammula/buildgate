package sandbox

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"gopkg.in/yaml.v3"

	"buildgate/internal/evidence"
	"buildgate/internal/sanitize"
	"buildgate/internal/workerskills"
)

// MaxSkillsBundleBytes caps the total size of all snapshotted skills.
const MaxSkillsBundleBytes = 2 << 20

// projectSkillDirs are the directories, relative to a workspace, where
// coding-agent harnesses look for repo-level skills.
var projectSkillDirs = []string{".github/skills", ".agents/skills", ".claude/skills", ".pi/skills"}

// SkillSource is one operator skill to snapshot: its name and the folder
// that holds its SKILL.md (a symlink is allowed at the root only).
// Builtin marks one of buildgate's embedded skills (internal/workerskills):
// it is copied straight from this binary, never from a host folder, and
// Dir is empty.
type SkillSource struct {
	Name    string `json:"name"`
	Dir     string `json:"dir,omitempty"`
	Builtin bool   `json:"builtin,omitempty"`
}

// ProjectSkillShadows returns the workspace-relative paths of project
// skills that would collide with any of names: a repo skill with the
// operator's name would silently replace it (Copilot) or sit beside it
// (Codex). A harness names a skill by its SKILL.md front matter, not its
// folder, so both count. Sorted.
func ProjectSkillShadows(workDir string, names []string) []string {
	want := map[string]bool{}
	for _, n := range names {
		want[n] = true
	}
	var out []string
	for _, rel := range projectSkillEntries(workDir) {
		if want[filepath.Base(rel)] || want[projectSkillName(workDir, rel)] {
			out = append(out, rel)
		}
	}
	return out
}

// maxProjectSkillHeaderBytes bounds how much of a worker-written project
// SKILL.md the host reads to find its front-matter name.
const maxProjectSkillHeaderBytes = 64 << 10

// projectSkillEntries is every entry (folder or symlink) under the project
// skill directories, workspace-relative and sorted. The workspace is
// worker-written: a skill directory that resolves outside it is not
// listed from (the host never enumerates a folder the worker pointed it
// at); it is returned itself so it still shows in evidence.
func projectSkillEntries(workDir string) []string {
	var out []string
	for _, d := range projectSkillDirs {
		resolved, err := filepath.EvalSymlinks(filepath.Join(workDir, d))
		if err != nil {
			continue
		}
		if !insideWorkspace(workDir, resolved) {
			out = append(out, d)
			continue
		}
		entries, err := os.ReadDir(resolved)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || e.Type()&fs.ModeSymlink != 0 {
				out = append(out, filepath.Join(d, e.Name()))
			}
		}
	}
	sort.Strings(out)
	return out
}

// insideWorkspace reports whether resolved (symlink-free) is workDir or
// below it.
func insideWorkspace(workDir, resolved string) bool {
	root, err := filepath.EvalSymlinks(workDir)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(root, resolved)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// projectSkillName is the front-matter name of the project skill at rel,
// or "" when its SKILL.md resolves outside the workspace, is not a regular
// file, or has no parseable name. Read without following a final symlink
// and non-blocking, so a planted link or FIFO cannot make the host read
// elsewhere or hang, and only its first maxProjectSkillHeaderBytes.
func projectSkillName(workDir, rel string) string {
	dir, err := filepath.EvalSymlinks(filepath.Join(workDir, rel))
	if err != nil || !insideWorkspace(workDir, dir) {
		return ""
	}
	path := filepath.Join(dir, "SKILL.md")
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return ""
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return ""
	}
	defer f.Close()
	body, err := io.ReadAll(io.LimitReader(f, maxProjectSkillHeaderBytes))
	if err != nil {
		return ""
	}
	name, err := frontMatterName(body)
	if err != nil {
		return ""
	}
	return name
}

// maxRecordedProjectSkills bounds the repo skills recorded per attempt: the
// list is worker-controlled and lands in the run record.
const maxRecordedProjectSkills = 64

// ProjectSkills lists the target repo's own project skills for evidence:
// projectSkillEntries with each path made safe to print (worker-chosen
// names) and at most maxRecordedProjectSkills entries, the rest counted.
func ProjectSkills(workDir string) []string {
	entries := projectSkillEntries(workDir)
	extra := 0
	if len(entries) > maxRecordedProjectSkills {
		extra = len(entries) - maxRecordedProjectSkills
		entries = entries[:maxRecordedProjectSkills]
	}
	out := make([]string, 0, len(entries)+1)
	for _, e := range entries {
		out = append(out, sanitize.Line(e))
	}
	if extra > 0 {
		out = append(out, fmt.Sprintf("(%d more)", extra))
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// StageSkills snapshots skills into a fresh directory under parent for one
// launch and returns it, its SHA-256, and a cleanup. No skills returns
// "", "", nil, nil. parent must be outside workDir (callers pass their log
// directory under the data dir).
func StageSkills(workDir, parent string, skills []SkillSource) (dir, sha256Hash string, cleanup func(), err error) {
	if len(skills) == 0 {
		return "", "", nil, nil
	}
	if ReferenceOracleSourceContained(workDir, parent) {
		return "", "", nil, errors.New("skills: staging directory must not be inside the workspace")
	}
	if err := os.MkdirAll(parent, 0o750); err != nil {
		return "", "", nil, fmt.Errorf("skills: create staging parent: %w", err)
	}
	holder, err := os.MkdirTemp(parent, "skills-")
	if err != nil {
		return "", "", nil, fmt.Errorf("skills: create staging directory: %w", err)
	}
	dir = filepath.Join(holder, "skills")
	sha256Hash, err = SnapshotSkills(workDir, dir, skills)
	if err != nil {
		_ = os.RemoveAll(holder)
		return "", "", nil, err
	}
	return dir, sha256Hash, func() { _ = os.RemoveAll(holder) }, nil
}

// SkillNames lists skills' names in order, for evidence.
func SkillNames(skills []SkillSource) []string {
	if len(skills) == 0 {
		return nil
	}
	names := make([]string, len(skills))
	for i, s := range skills {
		names[i] = s.Name
	}
	return names
}

// SnapshotSkills copies the operator skills into dst (one folder per skill
// name) and returns the SHA-256 of the snapshot, the one fixed tree both
// the recorded hash and the read-only worker mount act on. dst is removed
// first (a retried Temporal Activity can find a stale one, as in
// SnapshotReferenceOracle) and must be absolute and outside workDir; on
// any error nothing is left behind. No skills means "", nil and nothing
// is created.
//
// Refused: a project skill of the same name in workDir, a source inside
// workDir, a SKILL.md that is not regular or whose front-matter name
// differs, any symlink or non-regular file below the root, any executable
// file (evidence.SnapshotTree writes 0640, so a script would land
// non-executable on the read-only mount; v1 refuses rather than silently
// changing behaviour), and a bundle over MaxSkillsBundleBytes.
func SnapshotSkills(workDir, dst string, skills []SkillSource) (sha256Hash string, err error) {
	if len(skills) == 0 {
		return "", nil
	}
	if !filepath.IsAbs(dst) {
		return "", fmt.Errorf("skills: snapshot destination %q must be absolute", dst)
	}
	if err := os.RemoveAll(dst); err != nil {
		return "", fmt.Errorf("skills: clear stale snapshot: %w", err)
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(dst)
		}
	}()

	names := make([]string, len(skills))
	for i, s := range skills {
		names[i] = s.Name
	}
	if shadows := ProjectSkillShadows(workDir, names); len(shadows) > 0 {
		return "", fmt.Errorf("skills: repository %s already has project skill(s) %v with the same name as an operator skill", workDir, shadows)
	}

	type resolved struct {
		name, dir string
		builtin   bool
	}
	var plan []resolved
	var total int64
	seen := map[string]bool{}
	for _, s := range skills {
		if seen[s.Name] {
			return "", fmt.Errorf("skills: %s: listed twice", s.Name)
		}
		seen[s.Name] = true
		var dir string
		var size int64
		var err error
		if s.Builtin {
			size, err = checkBuiltinSkill(s.Name)
		} else {
			dir, size, err = checkSkill(workDir, s)
		}
		if err != nil {
			return "", err
		}
		total += size
		if total > MaxSkillsBundleBytes {
			return "", fmt.Errorf("skills: %s: bundle exceeds %d bytes", s.Name, MaxSkillsBundleBytes)
		}
		plan = append(plan, resolved{s.Name, dir, s.Builtin})
	}

	if err := os.MkdirAll(dst, 0o750); err != nil {
		return "", fmt.Errorf("skills: create snapshot directory: %w", err)
	}
	for _, p := range plan {
		var err error
		if p.builtin {
			err = copyBuiltinSkill(p.name, filepath.Join(dst, p.name))
		} else {
			err = evidence.SnapshotTree(p.dir, filepath.Join(dst, p.name))
		}
		if err != nil {
			return "", fmt.Errorf("skills: %s: snapshot: %w", p.name, err)
		}
	}
	hash, err := evidence.SHA256Tree(dst)
	if err != nil {
		return "", fmt.Errorf("skills: hash snapshot: %w", err)
	}
	return hash, nil
}

// checkSkill validates one source and returns its symlink-resolved root
// and total file bytes.
func checkSkill(workDir string, s SkillSource) (string, int64, error) {
	root, err := filepath.EvalSymlinks(s.Dir)
	if err != nil {
		return "", 0, fmt.Errorf("skills: %s: resolve %s: %w", s.Name, s.Dir, err)
	}
	if ReferenceOracleSourceContained(workDir, root) {
		return "", 0, fmt.Errorf("skills: %s: source %s must not be inside the workspace", s.Name, root)
	}
	info, err := os.Lstat(filepath.Join(root, "SKILL.md"))
	if err != nil {
		return "", 0, fmt.Errorf("skills: %s: SKILL.md: %w", s.Name, err)
	}
	if !info.Mode().IsRegular() {
		return "", 0, fmt.Errorf("skills: %s: SKILL.md is not a regular file", s.Name)
	}
	body, err := os.ReadFile(filepath.Join(root, "SKILL.md"))
	if err != nil {
		return "", 0, fmt.Errorf("skills: %s: read SKILL.md: %w", s.Name, err)
	}
	got, err := frontMatterName(body)
	if err != nil {
		return "", 0, fmt.Errorf("skills: %s: SKILL.md front matter: %w", s.Name, err)
	}
	if got != s.Name {
		return "", 0, fmt.Errorf("skills: %s: SKILL.md front-matter name is %q, want %q", s.Name, got, s.Name)
	}

	var size int64
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if path == root {
			return nil
		}
		fi, err := os.Lstat(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		switch {
		case fi.Mode()&fs.ModeSymlink != 0:
			return fmt.Errorf("skills: %s: %s is a symlink", s.Name, rel)
		case fi.IsDir():
			return nil
		case !fi.Mode().IsRegular():
			return fmt.Errorf("skills: %s: %s is not a regular file", s.Name, rel)
		case fi.Mode()&0o111 != 0:
			return fmt.Errorf("skills: %s: %s is executable (v1 snapshots are non-executable; remove the exec bit)", s.Name, rel)
		}
		size += fi.Size()
		return nil
	})
	if err != nil {
		return "", 0, err
	}
	return root, size, nil
}

// checkBuiltinSkill applies checkSkill's content rules to an embedded skill
// and returns its total file bytes.
func checkBuiltinSkill(name string) (int64, error) {
	tree := workerskills.FS()
	body, err := fs.ReadFile(tree, name+"/SKILL.md")
	if err != nil {
		return 0, fmt.Errorf("skills: %s: not a built-in skill: %w", name, err)
	}
	got, err := frontMatterName(body)
	if err != nil {
		return 0, fmt.Errorf("skills: %s: SKILL.md front matter: %w", name, err)
	}
	if got != name {
		return 0, fmt.Errorf("skills: %s: SKILL.md front-matter name is %q, want %q", name, got, name)
	}
	var size int64
	err = fs.WalkDir(tree, name, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		size += info.Size()
		return nil
	})
	return size, err
}

// copyBuiltinSkill writes the embedded skill name into dst with the same
// modes evidence.SnapshotTree uses (directories 0750, files 0640), set with
// an explicit chmod: the worker reads the mount as another user through the
// group bit, which a strict umask (077) would otherwise strip.
func copyBuiltinSkill(name, dst string) error {
	tree := workerskills.FS()
	return fs.WalkDir(tree, name, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(name, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			if err := os.MkdirAll(target, 0o750); err != nil {
				return err
			}
			return os.Chmod(target, 0o750)
		}
		data, err := fs.ReadFile(tree, path)
		if err != nil {
			return err
		}
		if err := os.WriteFile(target, data, 0o640); err != nil {
			return err
		}
		return os.Chmod(target, 0o640)
	})
}

// frontMatterName returns the name: value of the YAML front matter between
// the leading "---" lines of a SKILL.md.
func frontMatterName(body []byte) (string, error) {
	body = bytes.ReplaceAll(body, []byte("\r\n"), []byte("\n"))
	if !bytes.HasPrefix(body, []byte("---\n")) {
		return "", errors.New("missing leading --- line")
	}
	rest := body[4:]
	end := bytes.Index(rest, []byte("\n---"))
	if end < 0 {
		return "", errors.New("missing closing --- line")
	}
	var fm struct {
		Name string `yaml:"name"`
	}
	if err := yaml.Unmarshal(rest[:end], &fm); err != nil {
		return "", err
	}
	return fm.Name, nil
}
