package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func writeSkill(t *testing.T, root, name string, extra map[string]string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{"SKILL.md": "---\nname: " + name + "\ndescription: d\n---\nbody\n"}
	for k, v := range extra {
		files[k] = v
	}
	for k, v := range files {
		p := filepath.Join(dir, k)
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(v), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestSnapshotSkillsHappyPathStableHash(t *testing.T) {
	src, work := t.TempDir(), t.TempDir()
	a := writeSkill(t, src, "alpha", map[string]string{"ref/notes.md": "n"})
	b := writeSkill(t, src, "beta", nil)
	skills := []SkillSource{{Name: "alpha", Dir: a}, {Name: "beta", Dir: b}}
	dst := filepath.Join(t.TempDir(), "snap")
	h1, err := SnapshotSkills(work, dst, skills)
	if err != nil || h1 == "" {
		t.Fatalf("hash %q err %v", h1, err)
	}
	for _, p := range []string{"alpha/SKILL.md", "alpha/ref/notes.md", "beta/SKILL.md"} {
		if _, err := os.Stat(filepath.Join(dst, p)); err != nil {
			t.Errorf("missing %s: %v", p, err)
		}
	}
	h2, err := SnapshotSkills(work, dst, skills)
	if err != nil || h2 != h1 {
		t.Fatalf("second run hash %q err %v, want %q", h2, err, h1)
	}
}

func TestSnapshotSkillsEmptyCreatesNothing(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "snap")
	h, err := SnapshotSkills(t.TempDir(), dst, nil)
	if h != "" || err != nil {
		t.Fatalf("got %q, %v", h, err)
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatalf("dst exists: %v", err)
	}
}

func TestSnapshotSkillsRootSymlinkResolved(t *testing.T) {
	src, work := t.TempDir(), t.TempDir()
	real := writeSkill(t, src, "real", nil)
	link := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "snap")
	if _, err := SnapshotSkills(work, dst, []SkillSource{{Name: "real", Dir: link}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dst, "real", "SKILL.md")); err != nil {
		t.Fatal(err)
	}
}

func TestSnapshotSkillsRefusals(t *testing.T) {
	big := strings.Repeat("x", MaxSkillsBundleBytes+1)
	cases := []struct {
		name  string
		setup func(t *testing.T, src, work string) SkillSource
		want  string
	}{
		{"nested symlink", func(t *testing.T, src, work string) SkillSource {
			d := writeSkill(t, src, "s", nil)
			if err := os.Symlink("/etc/hosts", filepath.Join(d, "link")); err != nil {
				t.Fatal(err)
			}
			return SkillSource{Name: "s", Dir: d}
		}, "symlink"},
		{"executable file", func(t *testing.T, src, work string) SkillSource {
			d := writeSkill(t, src, "s", map[string]string{"run.sh": "#!/bin/sh"})
			if err := os.Chmod(filepath.Join(d, "run.sh"), 0o750); err != nil {
				t.Fatal(err)
			}
			return SkillSource{Name: "s", Dir: d}
		}, "executable"},
		{"oversize", func(t *testing.T, src, work string) SkillSource {
			return SkillSource{Name: "s", Dir: writeSkill(t, src, "s", map[string]string{"big": big})}
		}, "exceeds"},
		{"name mismatch", func(t *testing.T, src, work string) SkillSource {
			d := writeSkill(t, src, "s", nil)
			if err := os.WriteFile(filepath.Join(d, "SKILL.md"), []byte("---\nname: other\n---\n"), 0o640); err != nil {
				t.Fatal(err)
			}
			return SkillSource{Name: "s", Dir: d}
		}, "front-matter name"},
		{"no front matter", func(t *testing.T, src, work string) SkillSource {
			d := writeSkill(t, src, "s", nil)
			if err := os.WriteFile(filepath.Join(d, "SKILL.md"), []byte("just text"), 0o640); err != nil {
				t.Fatal(err)
			}
			return SkillSource{Name: "s", Dir: d}
		}, "front matter"},
		{"missing SKILL.md", func(t *testing.T, src, work string) SkillSource {
			d := filepath.Join(src, "s")
			if err := os.MkdirAll(d, 0o750); err != nil {
				t.Fatal(err)
			}
			return SkillSource{Name: "s", Dir: d}
		}, "SKILL.md"},
		{"source inside workDir", func(t *testing.T, src, work string) SkillSource {
			return SkillSource{Name: "s", Dir: writeSkill(t, filepath.Join(work, "tools"), "s", nil)}
		}, "inside the workspace"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			src, work := t.TempDir(), t.TempDir()
			s := c.setup(t, src, work)
			dst := filepath.Join(t.TempDir(), "snap")
			h, err := SnapshotSkills(work, dst, []SkillSource{s})
			if err == nil || !strings.Contains(err.Error(), c.want) || !strings.HasPrefix(err.Error(), "skills: ") {
				t.Fatalf("err = %v, want skills: ... %q", err, c.want)
			}
			if h != "" {
				t.Errorf("hash %q on error", h)
			}
			if _, err := os.Stat(dst); !os.IsNotExist(err) {
				t.Errorf("dst left behind: %v", err)
			}
		})
	}
}

func TestSnapshotSkillsShadowRefusedPerProjectDir(t *testing.T) {
	for _, d := range []string{".github/skills", ".agents/skills", ".claude/skills", ".pi/skills"} {
		t.Run(d, func(t *testing.T) {
			src, work := t.TempDir(), t.TempDir()
			s := writeSkill(t, src, "s", nil)
			if err := os.MkdirAll(filepath.Join(work, d, "s"), 0o750); err != nil {
				t.Fatal(err)
			}
			dst := filepath.Join(t.TempDir(), "snap")
			_, err := SnapshotSkills(work, dst, []SkillSource{{Name: "s", Dir: s}})
			if err == nil || !strings.Contains(err.Error(), work) || !strings.Contains(err.Error(), filepath.Join(d, "s")) {
				t.Fatalf("err = %v", err)
			}
			if got := ProjectSkillShadows(work, []string{"s", "other"}); len(got) != 1 || got[0] != filepath.Join(d, "s") {
				t.Errorf("shadows = %v", got)
			}
		})
	}
}

func TestSnapshotSkillsClearsStaleDst(t *testing.T) {
	src, work := t.TempDir(), t.TempDir()
	s := writeSkill(t, src, "s", nil)
	dst := filepath.Join(t.TempDir(), "snap")
	if err := os.MkdirAll(filepath.Join(dst, "stale"), 0o750); err != nil {
		t.Fatal(err)
	}
	if _, err := SnapshotSkills(work, dst, []SkillSource{{Name: "s", Dir: s}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dst, "stale")); !os.IsNotExist(err) {
		t.Fatalf("stale content survived: %v", err)
	}
}

func TestSnapshotSkillsRemovesDstOnLaterError(t *testing.T) {
	src, work := t.TempDir(), t.TempDir()
	good := writeSkill(t, src, "good", nil)
	dst := filepath.Join(t.TempDir(), "snap")
	if err := os.MkdirAll(dst, 0o750); err != nil {
		t.Fatal(err)
	}
	_, err := SnapshotSkills(work, dst, []SkillSource{{Name: "good", Dir: good}, {Name: "bad", Dir: filepath.Join(src, "missing")}})
	if err == nil {
		t.Fatal("want error")
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatalf("dst left behind: %v", err)
	}
}

func TestProjectSkills(t *testing.T) {
	work := t.TempDir()
	for _, p := range []string{".claude/skills/b", ".github/skills/a", ".pi/skills/c"} {
		if err := os.MkdirAll(filepath.Join(work, p), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(work, ".claude/skills/file"), nil, 0o640); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(ProjectSkills(work), ",")
	want := ".claude/skills/b,.github/skills/a,.pi/skills/c"
	if got != want {
		t.Fatalf("got %s want %s", got, want)
	}
	if len(ProjectSkills(t.TempDir())) != 0 {
		t.Error("expected none")
	}
}

// A harness names a skill by its SKILL.md front matter, so a repo folder
// with another name still shadows the operator's skill.
func TestProjectSkillShadowsMatchFrontMatterName(t *testing.T) {
	work := t.TempDir()
	writeSkill(t, filepath.Join(work, ".agents", "skills"), "innocent", nil)
	if err := os.WriteFile(filepath.Join(work, ".agents", "skills", "innocent", "SKILL.md"), []byte("---\nname: tdd\ndescription: d\n---\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if got := ProjectSkillShadows(work, []string{"tdd"}); len(got) != 1 || got[0] != filepath.Join(".agents", "skills", "innocent") {
		t.Fatalf("shadows = %v, want the renamed folder", got)
	}
	if got := ProjectSkillShadows(work, []string{"other"}); len(got) != 0 {
		t.Fatalf("shadows = %v, want none", got)
	}
}

// The workspace is worker-written: a symlinked skill folder counts by its
// name, and a SKILL.md that is a symlink or a FIFO is never read (a FIFO
// read would block the host).
func TestProjectSkillShadowsNeverFollowOrBlock(t *testing.T) {
	work, outside := t.TempDir(), t.TempDir()
	writeSkill(t, outside, "tdd", nil)
	skills := filepath.Join(work, ".github", "skills")
	if err := os.MkdirAll(filepath.Join(skills, "fifo"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(skills, "fifo", "SKILL.md"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(skills, "linked-md"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "tdd", "SKILL.md"), filepath.Join(skills, "linked-md", "SKILL.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "tdd"), filepath.Join(skills, "tdd")); err != nil {
		t.Fatal(err)
	}
	got := ProjectSkillShadows(work, []string{"tdd"})
	if len(got) != 1 || got[0] != filepath.Join(".github", "skills", "tdd") {
		t.Fatalf("shadows = %v, want only the symlinked folder named tdd", got)
	}
	all := ProjectSkills(work)
	if len(all) != 3 {
		t.Fatalf("ProjectSkills = %v, want fifo, linked-md and the tdd symlink", all)
	}
}

func TestStageSkillsSnapshotsAndCleansUp(t *testing.T) {
	src, work, logs := t.TempDir(), t.TempDir(), t.TempDir()
	a := writeSkill(t, src, "alpha", nil)
	dir, hash, cleanup, err := StageSkills(work, logs, []SkillSource{{Name: "alpha", Dir: a}})
	if err != nil || hash == "" || cleanup == nil {
		t.Fatalf("dir %q hash %q err %v", dir, hash, err)
	}
	if !strings.HasPrefix(dir, logs) {
		t.Fatalf("dir %q not under %q", dir, logs)
	}
	if _, err := os.Stat(filepath.Join(dir, "alpha", "SKILL.md")); err != nil {
		t.Fatal(err)
	}
	cleanup()
	if entries, _ := os.ReadDir(logs); len(entries) != 0 {
		t.Fatalf("cleanup left %v", entries)
	}
	dir, hash, cleanup, err = StageSkills(work, logs, nil)
	if dir != "" || hash != "" || cleanup != nil || err != nil {
		t.Fatalf("no skills: got %q %q %v", dir, hash, err)
	}
	writeSkill(t, filepath.Join(work, ".claude", "skills"), "alpha", nil)
	if _, _, _, err := StageSkills(work, logs, []SkillSource{{Name: "alpha", Dir: a}}); err == nil {
		t.Fatal("shadowed skill staged")
	}
	if entries, _ := os.ReadDir(logs); len(entries) != 0 {
		t.Fatalf("failed stage left %v", entries)
	}
}

// A project skill directory the worker symlinked outside the workspace is
// never listed or read from; one symlinked inside it (a repo's
// .claude/skills -> ../.agents/skills) is.
func TestProjectSkillsStayInsideTheWorkspace(t *testing.T) {
	work, outside := t.TempDir(), t.TempDir()
	writeSkill(t, outside, "tdd", nil)
	if err := os.MkdirAll(filepath.Join(work, ".github"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(work, ".github", "skills")); err != nil {
		t.Fatal(err)
	}
	writeSkill(t, filepath.Join(work, ".agents", "skills"), "local", nil)
	if err := os.MkdirAll(filepath.Join(work, ".claude"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..", ".agents", "skills"), filepath.Join(work, ".claude", "skills")); err != nil {
		t.Fatal(err)
	}
	got := ProjectSkills(work)
	want := []string{filepath.Join(".agents", "skills", "local"), filepath.Join(".claude", "skills", "local"), filepath.Join(".github", "skills")}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("ProjectSkills = %v, want %v", got, want)
	}
	if shadows := ProjectSkillShadows(work, []string{"tdd"}); len(shadows) != 0 {
		t.Fatalf("shadows = %v: the outside folder must not be read", shadows)
	}
}

// The recorded list is worker-controlled: bounded, and printable.
func TestProjectSkillsBoundedAndSanitized(t *testing.T) {
	work := t.TempDir()
	dir := filepath.Join(work, ".agents", "skills")
	for i := 0; i < maxRecordedProjectSkills+3; i++ {
		if err := os.MkdirAll(filepath.Join(dir, fmt.Sprintf("s%03d", i)), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	got := ProjectSkills(work)
	if len(got) != maxRecordedProjectSkills+1 || got[len(got)-1] != "(3 more)" {
		t.Fatalf("got %d entries ending %q", len(got), got[len(got)-1])
	}
	work = t.TempDir()
	if err := os.MkdirAll(filepath.Join(work, ".agents", "skills", "evil\x1b[2Jname"), 0o750); err != nil {
		t.Fatal(err)
	}
	if got := ProjectSkills(work); len(got) != 1 || strings.ContainsRune(got[0], '\x1b') {
		t.Fatalf("unsanitized entry: %q", got)
	}
}

// A built-in skill snapshots from the binary itself with the same refusals
// as a disk skill: a repo skill of its name still refuses the launch, and an
// unknown name is not a built-in.
func TestSnapshotSkillsBuiltin(t *testing.T) {
	work := t.TempDir()
	dst := filepath.Join(t.TempDir(), "snap")
	h1, err := SnapshotSkills(work, dst, []SkillSource{{Name: "buildgate-tdd", Builtin: true}})
	if err != nil || h1 == "" {
		t.Fatalf("hash %q err %v", h1, err)
	}
	info, err := os.Stat(filepath.Join(dst, "buildgate-tdd", "SKILL.md"))
	if err != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("snapshot file: %v %v", info, err)
	}
	if h2, _ := SnapshotSkills(work, dst, []SkillSource{{Name: "buildgate-tdd", Builtin: true}}); h2 != h1 {
		t.Fatalf("hash not stable: %q vs %q", h2, h1)
	}
	if _, err := SnapshotSkills(work, dst, []SkillSource{{Name: "no-such-skill", Builtin: true}}); err == nil {
		t.Fatal("unknown built-in accepted")
	}
	writeSkill(t, filepath.Join(work, ".agents", "skills"), "buildgate-tdd", nil)
	if _, err := SnapshotSkills(work, dst, []SkillSource{{Name: "buildgate-tdd", Builtin: true}}); err == nil || !strings.Contains(err.Error(), "same name") {
		t.Fatalf("shadowed built-in: %v", err)
	}
}

// Under a strict umask the built-in copy still gets group-readable modes:
// the worker reads /inputs/skills as another user through the group bit.
// Process-wide umask: not t.Parallel.
func TestSnapshotSkillsBuiltinModesSurviveStrictUmask(t *testing.T) {
	old := syscall.Umask(0o077)
	defer syscall.Umask(old)
	dst := filepath.Join(t.TempDir(), "snap")
	if _, err := SnapshotSkills(t.TempDir(), dst, []SkillSource{{Name: "buildgate-tdd", Builtin: true}}); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]os.FileMode{
		filepath.Join(dst, "buildgate-tdd"):             0o750,
		filepath.Join(dst, "buildgate-tdd", "SKILL.md"): 0o640,
	} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != want {
			t.Errorf("%s: mode %v, want %v (%v)", path, info.Mode().Perm(), want, err)
		}
	}
}
