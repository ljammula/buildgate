package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// commitLinks commits symlinks (path -> link text) on top of r's base and
// makes that commit the new base.
func commitLinks(t *testing.T, r *instructionRepo, links map[string]string) {
	t.Helper()
	for rel, text := range links {
		p := filepath.Join(r.dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(text, p); err != nil {
			t.Fatal(err)
		}
	}
	r.git("add", "-A")
	r.git("commit", "-q", "-m", "links")
	r.base = r.git("rev-parse", "HEAD")
}

func replaceWithLink(t *testing.T, r *instructionRepo, rel, text string) {
	t.Helper()
	p := filepath.Join(r.dir, filepath.FromSlash(rel))
	if err := os.RemoveAll(p); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(text, p); err != nil {
		t.Fatal(err)
	}
}

func TestReviewInstructionUnchangedBaseLinks(t *testing.T) {
	t.Run("untouched repo with CLAUDE.md -> AGENTS.md", func(t *testing.T) {
		r := newInstructionRepo(t, map[string]string{"AGENTS.md": "rules\n"})
		commitLinks(t, r, map[string]string{"CLAUDE.md": "AGENTS.md"})
		snap, _ := mustSnap(t, r)
		if len(snap.Masks) != 0 || snap.SHA256 != "" {
			t.Fatalf("snapshot = %+v", snap)
		}
	})
	t.Run("editing the target masks it, not the link", func(t *testing.T) {
		r := newInstructionRepo(t, map[string]string{"AGENTS.md": "rules\n"})
		commitLinks(t, r, map[string]string{"CLAUDE.md": "AGENTS.md"})
		r.write("AGENTS.md", "rules\nsteer\n")
		snap, _ := mustSnap(t, r)
		wantPaths(t, snap, "AGENTS.md")
		if got := readFile(t, snap.Masks[0].Source); got != "rules\n" {
			t.Fatalf("snapshot = %q", got)
		}
		if strings.Contains(readFile(t, snap.DiffPath), "CLAUDE.md") {
			t.Fatal("the unchanged link is in the diff file")
		}
	})
	t.Run("link to a file outside the table", func(t *testing.T) {
		r := newInstructionRepo(t, map[string]string{"docs/guide.md": "guide\n"})
		commitLinks(t, r, map[string]string{"CLAUDE.md": "docs/guide.md"})
		snap, _ := mustSnap(t, r)
		if len(snap.Masks) != 0 {
			t.Fatalf("masks = %+v", snap.Masks)
		}
		r.write("docs/guide.md", "guide\nsteer\n")
		mustFail(t, r, "CLAUDE.md", "docs/guide.md", "which this build changed")
	})
	t.Run("directory link into the table", func(t *testing.T) {
		r := newInstructionRepo(t, map[string]string{".agents/skills/x/SKILL.md": "skill\n"})
		commitLinks(t, r, map[string]string{".claude/skills": "../.agents/skills"})
		r.write(".agents/skills/x/SKILL.md", "skill\nsteer\n")
		snap, _ := mustSnap(t, r)
		wantPaths(t, snap, ".agents/skills")
		if !snap.Masks[0].Dir {
			t.Fatalf("mask = %+v", snap.Masks[0])
		}
		if got := readFile(t, filepath.Join(snap.Masks[0].Source, "x", "SKILL.md")); got != "skill\n" {
			t.Fatalf("snapshot = %q", got)
		}
	})
	t.Run("absolute target", func(t *testing.T) {
		// A link to an absolute path can re-enter the workspace inside the
		// container, so it is refused whether or not anything changed.
		r := newInstructionRepo(t, nil)
		commitLinks(t, r, map[string]string{"CLAUDE.md": "/etc/hostname"})
		mustFail(t, r, "CLAUDE.md", "not a plain relative path")
	})
	t.Run("chains are refused", func(t *testing.T) {
		// A link whose target is itself a link is a chain; none is followed.
		r := newInstructionRepo(t, map[string]string{"real.md": "r\n"})
		commitLinks(t, r, map[string]string{"CLAUDE.md": "l1", "l1": "real.md"})
		mustFail(t, r, "CLAUDE.md", "l1", "symlink")
	})
}

func TestReviewInstructionChangedLinksAreRefused(t *testing.T) {
	t.Run("retargeted", func(t *testing.T) {
		r := newInstructionRepo(t, map[string]string{"AGENTS.md": "rules\n", "evil.md": "evil\n"})
		commitLinks(t, r, map[string]string{"CLAUDE.md": "AGENTS.md"})
		replaceWithLink(t, r, "CLAUDE.md", "evil.md")
		mustFail(t, r, "CLAUDE.md", "retargeted")
	})
	t.Run("replaced by a regular file", func(t *testing.T) {
		r := newInstructionRepo(t, map[string]string{"AGENTS.md": "rules\n"})
		commitLinks(t, r, map[string]string{"CLAUDE.md": "AGENTS.md"})
		if err := os.Remove(filepath.Join(r.dir, "CLAUDE.md")); err != nil {
			t.Fatal(err)
		}
		r.write("CLAUDE.md", "rules\n")
		mustFail(t, r, "CLAUDE.md", "symlink at base")
	})
	t.Run("new link", func(t *testing.T) {
		r := newInstructionRepo(t, nil)
		replaceWithLink(t, r, "GEMINI.md", "x")
		mustFail(t, r, "GEMINI.md", "symlink")
	})
	t.Run("parent swapped for a link", func(t *testing.T) {
		symlinkedParentCase(t)
	})
}
