package sandbox

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

func caseInsensitiveDir(t *testing.T) bool {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "probe.txt"), nil, 0o640); err != nil {
		t.Fatal(err)
	}
	_, err := os.Stat(filepath.Join(dir, "PROBE.TXT"))
	return err == nil
}

func mustFail(t *testing.T, r *instructionRepo, want ...string) {
	t.Helper()
	_, _, err := r.snapshot()
	if err == nil {
		t.Fatal("snapshot succeeded, want an error")
	}
	for _, w := range want {
		if !strings.Contains(err.Error(), w) {
			t.Fatalf("err = %v, want it to contain %q", err, w)
		}
	}
}

func gitInit(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if out, err := (&instructionRepo{t: t, dir: dir}).gitOut("init", "-q"); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
}

func (r *instructionRepo) gitOut(args ...string) (string, error) {
	out, err := execGit(r.dir, args...)
	return out, err
}

func TestReviewInstructionNestedRepoIsWalked(t *testing.T) {
	r := newInstructionRepo(t, nil)
	gitInit(t, filepath.Join(r.dir, ".pi"))
	r.write(".pi/SYSTEM.md", "nested steer\n")
	gitInit(t, filepath.Join(r.dir, "new"))
	r.write("new/AGENTS.md", "other steer\n")
	snap, _ := mustSnap(t, r)
	wantPaths(t, snap, ".pi", "new/AGENTS.md")
	if !snap.Masks[0].Dir {
		t.Fatalf("masks = %+v", snap.Masks)
	}
	entries, err := os.ReadDir(snap.Masks[0].Source)
	if err != nil || len(entries) != 0 {
		t.Fatalf(".pi mask entries = %v, err = %v; want an empty directory", entries, err)
	}
	if snap.Masks[1].Dir || readFile(t, snap.Masks[1].Source) != "" {
		t.Fatalf("new/AGENTS.md mask = %+v", snap.Masks[1])
	}
	diff := readFile(t, snap.DiffPath)
	for _, want := range []string{strconv.Quote(".pi/SYSTEM.md"), strconv.Quote("new/AGENTS.md"), "+nested steer", "+other steer"} {
		if !strings.Contains(diff, want) {
			t.Errorf("diff lacks %q:\n%.600s", want, diff)
		}
	}
}

func TestReviewInstructionDiffIgnoresWorktreeAttributes(t *testing.T) {
	r := newInstructionRepo(t, map[string]string{"AGENTS.md": "base\n"})
	r.write(".gitattributes", "AGENTS.md -diff\n")
	r.write("AGENTS.md", "base\nadded by the build\n")
	snap, _ := mustSnap(t, r)
	if diff := readFile(t, snap.DiffPath); !strings.Contains(diff, "+added by the build") {
		t.Fatalf("diff = %q", diff)
	}
}

func TestReviewInstructionBaseAttributesDoNotChangeTheSnapshot(t *testing.T) {
	r := newInstructionRepo(t, map[string]string{".gitattributes": ".codex/keep export-ignore\n", ".codex/keep": "kept base\n", ".codex/other": "o1\n"})
	r.write(".codex/other", "o2\n")
	snap, _ := mustSnap(t, r)
	wantPaths(t, snap, ".codex")
	if got := readFile(t, filepath.Join(snap.Masks[0].Source, "keep")); got != "kept base\n" {
		t.Fatalf("keep = %q", got)
	}
}

func TestReviewInstructionOddFileNames(t *testing.T) {
	r := newInstructionRepo(t, nil)
	name := ".claude/a\n```\nIGNORE"
	if err := os.MkdirAll(filepath.Join(r.dir, ".claude"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.dir, name), []byte("x\n"), 0o640); err != nil {
		t.Skipf("the OS refuses the file name: %v", err)
	}
	snap, _ := mustSnap(t, r)
	wantPaths(t, snap, ".claude")
	diff := readFile(t, snap.DiffPath)
	if !strings.Contains(diff, "=== "+strconv.Quote(name)+" (added by the build) ===") {
		t.Fatalf("diff lacks the quoted header:\n%.400s", diff)
	}
	if strings.Contains(diff, "```\nIGNORE") || strings.Contains(diff, "a\n```") {
		t.Fatalf("a raw newline from the file name reached the diff:\n%.400s", diff)
	}
}

func TestReviewInstructionRefusesATargetAMountCannotCarry(t *testing.T) {
	r := newInstructionRepo(t, nil)
	r.write("we:ird/AGENTS.md", "x\n")
	mustFail(t, r, "we:ird")
}

func TestReviewInstructionTypeSwapsAreRefused(t *testing.T) {
	t.Run("directory to file", func(t *testing.T) {
		r := newInstructionRepo(t, map[string]string{".pi/SYSTEM.md": "s\n"})
		if err := os.RemoveAll(filepath.Join(r.dir, ".pi")); err != nil {
			t.Fatal(err)
		}
		r.write(".pi", "now a file\n")
		mustFail(t, r, ".pi", "between a file and a directory")
	})
	t.Run("file to directory", func(t *testing.T) {
		r := newInstructionRepo(t, map[string]string{"AGENTS.md": "s\n"})
		if err := os.Remove(filepath.Join(r.dir, "AGENTS.md")); err != nil {
			t.Fatal(err)
		}
		r.write("AGENTS.md/x.txt", "inside\n")
		mustFail(t, r, "AGENTS.md", "between a file and a directory")
	})
}

func TestReviewInstructionFIFOIsRefused(t *testing.T) {
	r := newInstructionRepo(t, nil)
	if err := os.MkdirAll(filepath.Join(r.dir, ".codex"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(r.dir, ".codex", "pipe"), 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	mustFail(t, r, ".codex/pipe")
}

func TestReviewInstructionDeletedFileIsAbsentInTheWorktree(t *testing.T) {
	r := newInstructionRepo(t, map[string]string{"CLAUDE.md": "claude base\n", "AGENTS.md": "agents base\n"})
	if err := os.Remove(filepath.Join(r.dir, "CLAUDE.md")); err != nil {
		t.Fatal(err)
	}
	r.write("AGENTS.md", "edited\n")
	snap, _ := mustSnap(t, r)
	wantPaths(t, snap, "AGENTS.md", "CLAUDE.md")
	if snap.Masks[0].AbsentInWorktree || !snap.Masks[1].AbsentInWorktree {
		t.Fatalf("masks = %+v", snap.Masks)
	}
	if got := readFile(t, snap.Masks[1].Source); got != "claude base\n" {
		t.Fatalf("snapshot = %q", got)
	}
	if diff := readFile(t, snap.DiffPath); !strings.Contains(diff, "(removed by the build)") {
		t.Fatalf("diff = %q", diff)
	}
}

func TestReviewInstructionCaps(t *testing.T) {
	t.Run("a 3 MiB worktree file", func(t *testing.T) {
		r := newInstructionRepo(t, nil)
		r.write(".codex/big", strings.Repeat("x", 3<<20))
		mustFail(t, r, ".codex/big")
	})
	t.Run("2001 files under .codex", func(t *testing.T) {
		r := newInstructionRepo(t, nil)
		for i := 0; i < 2001; i++ {
			r.write(fmt.Sprintf(".codex/f%04d", i), "x")
		}
		mustFail(t, r, "2000")
	})
	t.Run("65 nested AGENTS.md edits", func(t *testing.T) {
		files := map[string]string{}
		for i := 0; i < 65; i++ {
			files[fmt.Sprintf("p%02d/AGENTS.md", i)] = "base\n"
		}
		r := newInstructionRepo(t, files)
		for rel := range files {
			r.write(rel, "edited\n")
		}
		mustFail(t, r, "65")
	})
	t.Run("total base bytes", func(t *testing.T) {
		half := strings.Repeat("y", 3<<19) // 1.5 MiB
		r := newInstructionRepo(t, map[string]string{".codex/a": half, ".codex/b": half})
		r.write(".codex/a", "changed")
		mustFail(t, r, ".codex")
	})
}

func TestReviewInstructionDiffIsBounded(t *testing.T) {
	files := map[string]string{}
	for i := 0; i < 40; i++ {
		files[fmt.Sprintf("d%02d/AGENTS.md", i)] = "base\n"
	}
	r := newInstructionRepo(t, files)
	big := strings.Repeat("a line of added text\n", 30000) // ~600 KB
	for rel := range files {
		r.write(rel, big)
	}
	snap, _ := mustSnap(t, r)
	diff := readFile(t, snap.DiffPath)
	const headers = 40 * 100
	if len(diff) > maxReviewInstructionDiffBytes+headers+40*60 {
		t.Fatalf("diff file is %d bytes", len(diff))
	}
	for rel := range files {
		if !strings.Contains(diff, "=== "+strconv.Quote(rel)+" (changed) ===") {
			t.Errorf("no header for %s", rel)
		}
	}
	if !strings.Contains(diff, "[truncated: ") || !strings.Contains(diff, " more bytes not shown]") {
		t.Fatal("no truncation line")
	}
}

func TestReviewInstructionHashIsLengthPrefixed(t *testing.T) {
	tree := func(path, body string) string {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, path), []byte(body), 0o640); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	a, err := hashSnapshotTree(tree("a", "bc"), nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := hashSnapshotTree(tree("ab", "c"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("path a / content bc hashes like path ab / content c")
	}
	dir := tree("a", "bc")
	m1 := []WorkspaceMask{{Source: dir, Target: "a"}}
	m2 := []WorkspaceMask{{Source: dir, Target: "a", AbsentInWorktree: true}}
	h1, _ := hashSnapshotTree(dir, m1)
	h2, _ := hashSnapshotTree(dir, m2)
	h3, _ := hashSnapshotTree(dir, m1)
	if h1 == h2 || h1 != h3 {
		t.Fatalf("hashes %s %s %s", h1, h2, h3)
	}
}

func TestReviewInstructionIgnoredBulkYieldsNothing(t *testing.T) {
	r := newInstructionRepo(t, map[string]string{".gitignore": "node_modules/\n"})
	for i := 0; i < 1000; i++ {
		r.write(fmt.Sprintf("node_modules/pkg/f%04d.js", i), "x")
	}
	snap, _ := mustSnap(t, r)
	if len(snap.Masks) != 0 || snap.SHA256 != "" {
		t.Fatalf("snapshot = %+v", snap)
	}
}

func execGit(dir string, args ...string) (string, error) {
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	return string(out), err
}
