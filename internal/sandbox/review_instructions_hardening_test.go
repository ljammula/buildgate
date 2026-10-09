package sandbox

import (
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestFoldPathAgreesWithEqualFold(t *testing.T) {
	for _, c := range []struct {
		a, b string
		same bool
	}{
		{"AGENTS.md", "agents.md", true},
		{"AGENTS.md", "AGENTſ.md", true},
		{".git", ".GIT", true},
		{"k", "\u212a", true},
		{"AGENTS.md", "AGENT.md", false},
		{"a/b", "a\\b", false},
	} {
		if got := foldPath(c.a) == foldPath(c.b); got != c.same {
			t.Errorf("foldPath(%q) == foldPath(%q) is %v, want %v", c.a, c.b, got, c.same)
		}
		if strings.EqualFold(c.a, c.b) != c.same {
			t.Errorf("EqualFold(%q, %q) disagrees with the table", c.a, c.b)
		}
	}
}

func renameVia(t *testing.T, r *instructionRepo, from, to string) {
	t.Helper()
	mid := filepath.Join(r.dir, "intermediate.tmp")
	if err := os.Rename(filepath.Join(r.dir, from), mid); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(mid, filepath.Join(r.dir, to)); err != nil {
		t.Fatal(err)
	}
}

func TestReviewInstructionSpellingMustBeTheTables(t *testing.T) {
	t.Run("renamed to another case and edited", func(t *testing.T) {
		r := newInstructionRepo(t, map[string]string{"AGENTS.md": "rules\n"})
		renameVia(t, r, "AGENTS.md", "agents.md")
		r.write("agents.md", "rules\nsteer\n")
		mustFail(t, r, "agents.md", "AGENTS.md")
	})
	t.Run("new lower-case claude.md at the root", func(t *testing.T) {
		r := newInstructionRepo(t, nil)
		r.write("claude.md", "steer\n")
		mustFail(t, r, "claude.md", "CLAUDE.md")
	})
	t.Run("a base file spelled otherwise, untouched and edited", func(t *testing.T) {
		r := newInstructionRepo(t, map[string]string{"docs/agents.md": "lower\n"})
		if snap, _ := mustSnap(t, r); len(snap.Masks) != 0 || snap.SHA256 != "" {
			t.Fatalf("snapshot = %+v, want none", snap)
		}
		r.write("docs/agents.md", "lower\nsteer\n")
		mustFail(t, r, "docs/agents.md", "AGENTS.md")
	})
	t.Run("base link removed and a lower-case file written", func(t *testing.T) {
		r := newInstructionRepo(t, map[string]string{"AGENTS.md": "rules\n"})
		commitLinks(t, r, map[string]string{"CLAUDE.md": "AGENTS.md"})
		if err := os.Remove(filepath.Join(r.dir, "CLAUDE.md")); err != nil {
			t.Fatal(err)
		}
		r.write("claude.md", "rules\n")
		mustFail(t, r, "claude.md")
	})
	t.Run("a long-s spelling next to the base file is never two masks", func(t *testing.T) {
		r := newInstructionRepo(t, map[string]string{"AGENTS.md": "rules\n"})
		r.write("AGENT\u017f.md", "steer\n")
		snap, _, err := r.snapshot()
		if err != nil {
			return // the fold rule refused it, which is the point
		}
		// Where the file system treats both names as one file the write was
		// an edit of AGENTS.md: exactly one mask, at the table's spelling.
		wantPaths(t, snap, "AGENTS.md")
	})
}

func TestReviewInstructionLinkRules(t *testing.T) {
	refused := []struct {
		name  string
		files map[string]string
		links map[string]string
		want  []string
	}{
		{"non-leading dot-dot", nil, map[string]string{"sub": "deep/a/b", "CLAUDE.md": "sub/../guide.md"}, []string{"CLAUDE.md", "not leading"}},
		{"absolute re-entering the workspace", map[string]string{"docs/guide.md": "g\n"}, map[string]string{"CLAUDE.md": "/workspace/docs/guide.md"}, []string{"CLAUDE.md", "plain relative"}},
		{"relative re-entering the workspace", map[string]string{"docs/guide.md": "g\n"}, map[string]string{"CLAUDE.md": "../workspace/docs/guide.md"}, []string{"CLAUDE.md", "outside"}},
		{"absolute outside", nil, map[string]string{"CLAUDE.md": "/etc/hostname"}, []string{"CLAUDE.md", "plain relative"}},
		{"symlink component in the target", nil, map[string]string{"lnk": "real", "CLAUDE.md": "lnk/x"}, []string{"CLAUDE.md", "lnk", "symlink"}},
		{"pathspec magic", nil, map[string]string{"CLAUDE.md": ":/foo"}, []string{"CLAUDE.md"}},
		{"into .git", nil, map[string]string{"CLAUDE.md": ".git/config"}, []string{"CLAUDE.md", ".git"}},
		{"trailing slash", map[string]string{"docs/x": "x\n"}, map[string]string{"CLAUDE.md": "docs/"}, []string{"CLAUDE.md"}},
	}
	for _, c := range refused {
		t.Run(c.name, func(t *testing.T) {
			r := newInstructionRepo(t, c.files)
			commitLinks(t, r, c.links)
			mustFail(t, r, c.want...)
		})
	}
	t.Run("link to a file, untouched, then edited, then made executable", func(t *testing.T) {
		r := newInstructionRepo(t, map[string]string{"docs/guide.md": "guide\n"})
		commitLinks(t, r, map[string]string{"CLAUDE.md": "docs/guide.md"})
		if snap, _ := mustSnap(t, r); len(snap.Masks) != 0 {
			t.Fatalf("masks = %+v", snap.Masks)
		}
		if err := os.Chmod(filepath.Join(r.dir, "docs", "guide.md"), 0o755); err != nil {
			t.Fatal(err)
		}
		mustFail(t, r, "CLAUDE.md", "docs/guide.md", "which this build changed")
		if err := os.Chmod(filepath.Join(r.dir, "docs", "guide.md"), 0o644); err != nil {
			t.Fatal(err)
		}
		r.write("docs/guide.md", "guide\nsteer\n")
		mustFail(t, r, "CLAUDE.md", "docs/guide.md", "which this build changed")
	})
	t.Run("leading dot-dot links, untouched then the target edited", func(t *testing.T) {
		r := newInstructionRepo(t, map[string]string{"AGENTS.md": "rules\n", ".agents/skills/x/SKILL.md": "skill\n"})
		commitLinks(t, r, map[string]string{".github/copilot-instructions.md": "../AGENTS.md", ".claude/skills": "../.agents/skills"})
		if snap, _ := mustSnap(t, r); len(snap.Masks) != 0 || snap.SHA256 != "" {
			t.Fatalf("snapshot = %+v", snap)
		}
		r.write(".agents/skills/x/SKILL.md", "skill\nsteer\n")
		snap, _ := mustSnap(t, r)
		wantPaths(t, snap, ".agents/skills")
		if !snap.Masks[0].Dir {
			t.Fatalf("mask = %+v", snap.Masks[0])
		}
	})
	t.Run("a changed target of a link to a directory outside the table", func(t *testing.T) {
		r := newInstructionRepo(t, map[string]string{"docs/a.md": "a\n", "docs/b.md": "b\n"})
		commitLinks(t, r, map[string]string{"GEMINI.md": "docs"})
		if snap, _ := mustSnap(t, r); len(snap.Masks) != 0 {
			t.Fatalf("masks = %+v", snap.Masks)
		}
		if err := os.Remove(filepath.Join(r.dir, "docs", "b.md")); err != nil {
			t.Fatal(err)
		}
		mustFail(t, r, "GEMINI.md", "docs")
	})
}

func TestReviewInstructionExecutableBit(t *testing.T) {
	r := newInstructionRepo(t, map[string]string{".github/hooks/h.sh": "#!/bin/sh\n"})
	if err := os.Chmod(filepath.Join(r.dir, ".github", "hooks", "h.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	snap, dst := mustSnap(t, r)
	wantPaths(t, snap, ".github/hooks")
	if !snap.Masks[0].Dir {
		t.Fatalf("mask = %+v", snap.Masks[0])
	}
	info, err := os.Stat(filepath.Join(snap.Masks[0].Source, "h.sh"))
	if err != nil || info.Mode().Perm() != 0o644 {
		t.Fatalf("snapshot file = %v, %v; want mode 0644 (the base mode)", info, err)
	}
	diff := readFile(t, snap.DiffPath)
	for _, want := range []string{"=== " + strconv.Quote(".github/hooks/h.sh") + " (changed) ===", "mode changed: 100644 -> 100755"} {
		if !strings.Contains(diff, want) {
			t.Errorf("diff lacks %q:\n%s", want, diff)
		}
	}
	err = filepath.WalkDir(dst, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Mode().Perm()&0o044 != 0o044 || (d.IsDir() && info.Mode().Perm()&0o011 != 0o011) {
			t.Errorf("%s is %v, not world-readable", p, info.Mode())
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestReviewInstructionExecutableSnapshotFollowsTheBaseMode(t *testing.T) {
	r := newInstructionRepo(t, nil)
	r.write(".codex/run.sh", "#!/bin/sh\n")
	r.git("add", "-A")
	r.git("update-index", "--chmod=+x", ".codex/run.sh")
	r.git("commit", "-q", "-m", "exec")
	r.base = r.git("rev-parse", "HEAD")
	r.write(".codex/run.sh", "#!/bin/sh\necho steer\n")
	if err := os.Chmod(filepath.Join(r.dir, ".codex", "run.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	snap, _ := mustSnap(t, r)
	info, err := os.Stat(filepath.Join(snap.Masks[0].Source, "run.sh"))
	if err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("snapshot file = %v, %v; want mode 0755", info, err)
	}
}

func TestReviewInstructionDiffFileHoldsNoHostPath(t *testing.T) {
	r := newInstructionRepo(t, map[string]string{"AGENTS.md": "base\n"})
	r.write("AGENTS.md", "base\nsteer\n")
	r.write("pkg/CLAUDE.md", "new\n")
	snap, dst := mustSnap(t, r)
	diff := readFile(t, snap.DiffPath)
	for _, leak := range []string{r.dir, dst, filepath.Dir(r.dir)} {
		if strings.Contains(diff, leak) {
			t.Errorf("diff contains the host path %q:\n%s", leak, diff)
		}
	}
	for _, line := range strings.Split(diff, "\n") {
		if strings.HasPrefix(line, "diff --git") || strings.HasPrefix(line, "index ") || strings.HasPrefix(line, "--- ") || strings.HasPrefix(line, "+++ ") {
			t.Errorf("diff keeps git's own header line %q", line)
		}
	}
	if !strings.Contains(diff, "+steer") || !strings.Contains(diff, "+new") {
		t.Fatalf("diff lacks the hunks:\n%s", diff)
	}
}

func lowerEntryLimit(t *testing.T, n int) {
	t.Helper()
	old := maxReviewInstructionEntries
	maxReviewInstructionEntries = n
	t.Cleanup(func() { maxReviewInstructionEntries = old })
}

func TestReviewInstructionWalkBudget(t *testing.T) {
	t.Run("the main walk", func(t *testing.T) {
		lowerEntryLimit(t, 50)
		r := newInstructionRepo(t, nil)
		for i := 0; i < 60; i++ {
			if err := os.MkdirAll(filepath.Join(r.dir, "dir"+strconv.Itoa(1000+i)), 0o750); err != nil {
				t.Fatal(err)
			}
		}
		mustFail(t, r, "more than 50 entries")
	})
	t.Run("a link-target comparison shares it", func(t *testing.T) {
		files := map[string]string{}
		for i := 0; i < 40; i++ {
			files["AAA/f"+strconv.Itoa(10+i)] = "x\n"
		}
		r := newInstructionRepo(t, files)
		commitLinks(t, r, map[string]string{"CLAUDE.md": "AAA"})
		if snap, _ := mustSnap(t, r); len(snap.Masks) != 0 {
			t.Fatalf("masks = %+v", snap.Masks)
		}
		lowerEntryLimit(t, 50)
		mustFail(t, r, "CLAUDE.md is a link to AAA", "more than 50 entries")
	})
}
