package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
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

func (r *instructionRepo) gone(rel string) bool {
	_, err := os.Lstat(r.abs(rel))
	return os.IsNotExist(err)
}

func TestReviewInstructionUntrackedEntriesAreRemoved(t *testing.T) {
	t.Run("an ignored AGENTS.md at the root", func(t *testing.T) {
		r := newInstructionRepo(t, nil)
		r.write(".gitignore", "AGENTS.md\n")
		r.commit()
		r.write("AGENTS.md", "steer the reviewer\n")
		snap, _ := r.mustSnap()
		if len(snap.Masks) != 0 || !reflect.DeepEqual(snap.Removed, []string{"AGENTS.md"}) || !r.gone("AGENTS.md") {
			t.Fatalf("snapshot = %+v, gone = %v", snap, r.gone("AGENTS.md"))
		}
		diff := readFile(t, snap.DiffPath)
		if !strings.Contains(diff, `=== "AGENTS.md" (untracked, removed before review) ===`) || !strings.Contains(diff, "+steer the reviewer\n") || snap.SHA256 == "" {
			t.Fatalf("diff = %q, SHA256 = %q", diff, snap.SHA256)
		}
	})
	t.Run("an untracked .pi holding a nested repository", func(t *testing.T) {
		r := newInstructionRepo(t, nil)
		r.commit()
		r.write(".pi/inner/x", "x\n")
		r.git("-C", r.abs(".pi/inner"), "init", "-q")
		snap, _ := r.mustSnap()
		if len(snap.Masks) != 0 || !reflect.DeepEqual(snap.Removed, []string{".pi"}) || !r.gone(".pi") {
			t.Fatalf("snapshot = %+v", snap)
		}
		if diff := readFile(t, snap.DiffPath); !strings.Contains(diff, `".pi" (untracked, removed before review)`) || !strings.Contains(diff, "[directory with ") {
			t.Fatalf("diff = %q", diff)
		}
	})
	t.Run("seventy AGENTS.md under node_modules", func(t *testing.T) {
		r := newInstructionRepo(t, nil)
		r.write(".gitignore", "node_modules/\n")
		r.commit()
		var want []string
		for i := 0; i < 70; i++ {
			r.write(fmt.Sprintf("node_modules/p%02d/AGENTS.md", i), "pkg\n")
			want = append(want, fmt.Sprintf("node_modules/p%02d/AGENTS.md", i))
		}
		snap, _ := r.mustSnap()
		if len(snap.Masks) != 0 || !reflect.DeepEqual(snap.Removed, want) {
			t.Fatalf("Masks = %v, Removed = %v", snap.Masks, snap.Removed)
		}
		for _, p := range want {
			if !r.gone(p) {
				t.Fatalf("%s is still there", p)
			}
		}
	})
	t.Run("node_modules/p/Agents.md", func(t *testing.T) {
		r := newInstructionRepo(t, nil)
		r.write(".gitignore", "node_modules/\n")
		r.commit()
		r.write("node_modules/p/Agents.md", "x\n")
		snap, _ := r.mustSnap()
		if !reflect.DeepEqual(snap.Removed, []string{"node_modules/p/Agents.md"}) || !r.gone("node_modules/p/Agents.md") {
			t.Fatalf("snapshot = %+v", snap)
		}
	})
}

func TestReviewInstructionUntrackedInsideTracked(t *testing.T) {
	t.Run("an ignored file dropped into a tracked .codex", func(t *testing.T) {
		r := newInstructionRepo(t, map[string]string{".codex/config.toml": "a = 1\n", ".gitignore": "*.cache\n"})
		r.commit()
		r.write(".codex/steer.cache", "steer\n")
		r.write(".codex/sub/deep.md", "steer\n")
		snap, _ := r.mustSnap()
		if len(snap.Masks) != 0 || !reflect.DeepEqual(snap.Removed, []string{".codex/steer.cache", ".codex/sub"}) {
			t.Fatalf("snapshot = %+v", snap)
		}
		if readFile(t, r.abs(".codex/config.toml")) != "a = 1\n" || !r.gone(".codex/steer.cache") || !r.gone(".codex/sub") {
			t.Fatal("the tracked sibling was touched or the ignored files remain")
		}
	})
}

func TestReviewInstructionUntrackedSpecialEntries(t *testing.T) {
	t.Run("a FIFO named AGENTS.md", func(t *testing.T) {
		r := newInstructionRepo(t, nil)
		r.commit()
		if err := os.MkdirAll(r.abs("sub"), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := syscall.Mkfifo(r.abs("sub/AGENTS.md"), 0o600); err != nil {
			t.Skipf("mkfifo: %v", err)
		}
		snap, _ := r.mustSnap()
		if !reflect.DeepEqual(snap.Removed, []string{"sub/AGENTS.md"}) || !r.gone("sub/AGENTS.md") || !strings.Contains(readFile(t, snap.DiffPath), "[special file]") {
			t.Fatalf("snapshot = %+v", snap)
		}
	})
	t.Run("an untracked symlink named CLAUDE.md is removed, not followed", func(t *testing.T) {
		r := newInstructionRepo(t, map[string]string{"secret.txt": "s\n"})
		r.commit()
		r.link("CLAUDE.md", "secret.txt")
		snap, _ := r.mustSnap()
		if !reflect.DeepEqual(snap.Removed, []string{"CLAUDE.md"}) || !r.gone("CLAUDE.md") || readFile(t, r.abs("secret.txt")) != "s\n" {
			t.Fatalf("snapshot = %+v", snap)
		}
	})
	t.Run("an untracked symlink where a fixed path's parent is", func(t *testing.T) {
		r := newInstructionRepo(t, nil)
		r.commit()
		r.link(".github", "docs")
		r.mustFail("symlink the result commit does not hold")
	})
	t.Run("a deleted tracked file recreated untracked is removed", func(t *testing.T) {
		r := newInstructionRepo(t, map[string]string{"CLAUDE.md": "rules\n", ".gitignore": ""})
		r.remove("CLAUDE.md")
		r.write(".gitignore", "CLAUDE.md\n")
		r.commit()
		r.write("CLAUDE.md", "steer\n")
		snap, _ := r.mustSnap()
		wantPaths(t, snap, "CLAUDE.md")
		if !snap.Masks[0].AbsentInWorktree || !reflect.DeepEqual(snap.Removed, []string{"CLAUDE.md"}) || !r.gone("CLAUDE.md") {
			t.Fatalf("snapshot = %+v", snap)
		}
	})
}

func TestReviewInstructionTrackedFilesMustMatchTheResultCommit(t *testing.T) {
	t.Run("modified after the commit", func(t *testing.T) {
		r := newInstructionRepo(t, map[string]string{"AGENTS.md": "rules\n"})
		r.commit()
		r.write("AGENTS.md", "steer!\n") // same size as before
		r.mustFail(`tracked instruction path "AGENTS.md" does not match the result commit`)
	})
	t.Run("only the exec bit changed after the commit", func(t *testing.T) {
		r := newInstructionRepo(t, map[string]string{".codex/run.sh": "x\n"})
		r.commit()
		r.chmod(".codex/run.sh", 0o755)
		r.mustFail("does not match the result commit")
	})
	t.Run("deleted after the commit", func(t *testing.T) {
		r := newInstructionRepo(t, map[string]string{".codex/a.md": "a\n"})
		r.commit()
		r.remove(".codex/a.md")
		r.mustFail("does not match the result commit")
	})
	t.Run("replaced by a symlink after the commit", func(t *testing.T) {
		r := newInstructionRepo(t, map[string]string{".codex/a.md": "a\n", "elsewhere/a.md": "a\n"})
		r.commit()
		r.remove(".codex")
		r.link(".codex", "elsewhere")
		r.mustFail("does not match the result commit")
	})
	t.Run("the same file through another case", func(t *testing.T) {
		if !caseInsensitiveDir(t) {
			t.Skip("the filesystem is case-sensitive: claude.md is a different file from CLAUDE.md")
		}
		r := newInstructionRepo(t, map[string]string{"CLAUDE.md": "rules\n"})
		r.commit()
		r.write("claude.md", "steer\n")
		r.mustFail("does not match the result commit")
		if got := readFile(t, r.abs("CLAUDE.md")); got != "steer\n" {
			t.Fatalf("the tracked file was removed or reverted: %q", got)
		}
	})
}

func TestReviewInstructionRerunIsIdempotent(t *testing.T) {
	r := newInstructionRepo(t, map[string]string{"AGENTS.md": "rules\n"})
	r.write("AGENTS.md", "steer\n")
	r.commit()
	r.write(".pi/x", "x\n")
	first, _ := r.mustSnap()
	second, _ := r.mustSnap()
	if !reflect.DeepEqual(first.Paths, []string{"AGENTS.md"}) || !reflect.DeepEqual(first.Removed, []string{".pi"}) {
		t.Fatalf("first = %+v", first)
	}
	if !reflect.DeepEqual(second.Paths, first.Paths) || second.Removed != nil || !reflect.DeepEqual(second.Masks[0].Target, "AGENTS.md") {
		t.Fatalf("second = %+v", second)
	}
}

func TestReviewInstructionWalkIsBounded(t *testing.T) {
	t.Run("a large tree without table matches", func(t *testing.T) {
		r := newInstructionRepo(t, nil)
		r.write(".gitignore", "big/\n")
		r.commit()
		const dirs, perDir = 150, 1000
		for d := 0; d < dirs; d++ {
			dir := r.abs(fmt.Sprintf("big/d%03d", d))
			if err := os.MkdirAll(dir, 0o750); err != nil {
				t.Fatal(err)
			}
			for f := 0; f < perDir; f++ {
				if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%04d.txt", f)), nil, 0o640); err != nil {
					t.Fatal(err)
				}
			}
		}
		start := time.Now()
		snap, _ := r.mustSnap()
		wantNothing(t, snap)
		if took := time.Since(start); took > 30*time.Second {
			t.Fatalf("snapshot of %d entries took %v", dirs*perDir, took)
		}
	})
	t.Run("more entries than the budget", func(t *testing.T) {
		old := maxReviewInstructionWalk
		maxReviewInstructionWalk = 50
		t.Cleanup(func() { maxReviewInstructionWalk = old })
		r := newInstructionRepo(t, nil)
		r.write(".gitignore", "many/\n")
		r.commit()
		for i := 0; i < 60; i++ {
			r.write(fmt.Sprintf("many/f%02d", i), "x")
		}
		r.mustFail("more than 50 entries")
	})
}
