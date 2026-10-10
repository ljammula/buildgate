package sandbox

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
)

// A renamed ancestor directory: the filesystem calls the two Cherokee letters
// one name, the fold does not, so only the same-file check can tell.
func TestReviewInstructionRenamedAncestorKeepsTrackedFile(t *testing.T) {
	r := newInstructionRepo(t, nil)
	r.write("Ꭰ/AGENTS.md", "tracked\n")
	r.commit()
	if err := os.Rename(r.abs("Ꭰ"), r.abs("ꭰ")); err != nil {
		t.Fatal(err)
	}
	a, errA := os.Lstat(r.abs("Ꭰ"))
	b, errB := os.Lstat(r.abs("ꭰ"))
	if errA != nil || errB != nil || !os.SameFile(a, b) {
		t.Skip("the filesystem keeps U+13A0 and U+AB70 apart")
	}
	r.mustFail("under another spelling")
	if readFile(t, r.abs("Ꭰ/AGENTS.md")) != "tracked\n" {
		t.Fatal("the tracked file was removed")
	}
}

func TestAncestorsAreTrackedSameDirectoryOtherSpelling(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(root+"/d", 0o750); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(root + "/d")
	if err != nil {
		t.Fatal(err)
	}
	d := &diskState{root: root, dirsByLen: map[int][]kidFile{1: {{"D", info}}}}
	err = d.ancestorsAreTracked("d/AGENTS.md")
	if err == nil || !strings.Contains(err.Error(), `"d" is the committed path "D" under another spelling`) {
		t.Fatalf("err = %v", err)
	}
	d = &diskState{root: root, dirsByLen: map[int][]kidFile{1: {{"d", info}}}}
	if err := d.ancestorsAreTracked("d/AGENTS.md"); err != nil {
		t.Fatalf("same spelling: %v", err)
	}
}

func TestApplyRemovalsFailsWhenATrackedEntryIsGone(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(root+"/AGENTS.md", []byte("x\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	removed := []removal{{path: "AGENTS.md", abs: root + "/AGENTS.md"}}
	done, err := applyRemovals(context.Background(), root, removed, []treeEntry{{path: "AGENTS.md"}})
	if err == nil || !strings.Contains(err.Error(), `removed the committed "AGENTS.md"`) || len(done) != 1 || done[0] != "AGENTS.md" {
		t.Fatalf("done = %v, err = %v", done, err)
	}
	if _, err := applyRemovals(context.Background(), root, nil, nil); err != nil {
		t.Fatal(err)
	}
}

func TestReviewInstructionManyUntrackedStillAllRemoved(t *testing.T) {
	r := newInstructionRepo(t, nil)
	r.write(".gitignore", "node_modules/\n")
	r.commit()
	for i := 0; i < 250; i++ {
		r.write(fmt.Sprintf("node_modules/p%d/AGENTS.md", i), "pkg\n")
	}
	snap, _ := r.mustSnap()
	if len(snap.Removed) != 250 {
		t.Fatalf("removed %d, want 250", len(snap.Removed))
	}
}

func TestReviewInstructionRetainedEntriesAreBounded(t *testing.T) {
	old := maxReviewInstructionRetained
	maxReviewInstructionRetained = 20
	t.Cleanup(func() { maxReviewInstructionRetained = old })
	const want = "more than 20 instruction-path, link or submodule entries in commit "
	t.Run("files under .claude", func(t *testing.T) {
		r := newInstructionRepo(t, nil)
		for i := 0; i < 30; i++ {
			r.write(fmt.Sprintf(".claude/f%d.txt", i), "x\n")
		}
		r.mustFail(want)
	})
	t.Run("symlinks outside the table", func(t *testing.T) {
		r := newInstructionRepo(t, nil)
		for i := 0; i < 30; i++ {
			r.link(fmt.Sprintf("src/l%d", i), "../main.go")
		}
		r.mustFail(want)
	})
	t.Run("ordinary files and one edited AGENTS.md", func(t *testing.T) {
		r := newInstructionRepo(t, map[string]string{"AGENTS.md": "a\n"})
		for i := 0; i < 30; i++ {
			r.write(fmt.Sprintf("src/f%d.go", i), "package x\n")
		}
		r.write("AGENTS.md", "b\n")
		r.mustSnap()
	})
}
