package sandbox

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestReviewInstructionBaseLinks(t *testing.T) {
	t.Run("CLAUDE.md -> AGENTS.md untouched", func(t *testing.T) {
		r := newInstructionRepoWithLinks(t, map[string]string{"AGENTS.md": "rules\n"}, map[string]string{"CLAUDE.md": "AGENTS.md"})
		snap, _ := r.mustSnap()
		wantNothing(t, snap)
	})
	t.Run("CLAUDE.md -> AGENTS.md with AGENTS.md edited", func(t *testing.T) {
		r := newInstructionRepoWithLinks(t, map[string]string{"AGENTS.md": "rules\n"}, map[string]string{"CLAUDE.md": "AGENTS.md"})
		r.write("AGENTS.md", "steer\n")
		snap, _ := r.mustSnap()
		wantPaths(t, snap, "AGENTS.md")
	})
	t.Run("CLAUDE.md -> docs/guide.md, guide unchanged", func(t *testing.T) {
		r := newInstructionRepoWithLinks(t, map[string]string{"docs/guide.md": "guide\n"}, map[string]string{"CLAUDE.md": "docs/guide.md"})
		snap, _ := r.mustSnap()
		wantNothing(t, snap)
	})
	t.Run("CLAUDE.md -> docs/guide.md, guide changed", func(t *testing.T) {
		r := newInstructionRepoWithLinks(t, map[string]string{"docs/guide.md": "guide\n"}, map[string]string{"CLAUDE.md": "docs/guide.md"})
		r.write("docs/guide.md", "steer\n")
		r.mustFail("CLAUDE.md is a link to docs/guide.md, which this build changed")
	})
	t.Run(".claude/skills -> ../.agents/skills with a skill edited", func(t *testing.T) {
		r := newInstructionRepoWithLinks(t, map[string]string{".agents/skills/x/SKILL.md": "skill\n"}, map[string]string{".claude/skills": "../.agents/skills"})
		r.write(".agents/skills/x/SKILL.md", "steer\n")
		snap, _ := r.mustSnap()
		wantPaths(t, snap, ".agents/skills")
		if m := snap.Masks[0]; !m.Dir || readFile(t, filepath.Join(m.Source, "x", "SKILL.md")) != "skill\n" {
			t.Fatalf("mask = %+v", m)
		}
	})
	t.Run("an unchanged link inside a masked directory is recreated as a link", func(t *testing.T) {
		// The link's target is one regular file outside the table (a directory
		// target is refused: see TestReviewInstructionLinkTargetRules).
		r := newInstructionRepoWithLinks(t, map[string]string{".codex/config.toml": "a = 1\n", "docs/prompts.md": "p\n"}, map[string]string{".codex/prompts": "../docs/prompts.md"})
		r.write(".codex/config.toml", "a = 2\n")
		snap, _ := r.mustSnap()
		wantPaths(t, snap, ".codex")
		text, err := os.Readlink(filepath.Join(snap.Masks[0].Source, "prompts"))
		if err != nil || text != "../docs/prompts.md" {
			t.Fatalf("Readlink = %q, %v", text, err)
		}
		if got := filesUnder(t, snap.Masks[0].Source); !reflect.DeepEqual(got, []string{"config.toml", "prompts"}) {
			t.Fatalf("files = %v", got)
		}
	})
}

func TestReviewInstructionChangedLinksAreRefused(t *testing.T) {
	t.Run("retargeted", func(t *testing.T) {
		r := newInstructionRepoWithLinks(t, map[string]string{"AGENTS.md": "rules\n", "other.md": "o\n"}, map[string]string{"CLAUDE.md": "AGENTS.md"})
		r.remove("CLAUDE.md")
		r.link("CLAUDE.md", "other.md")
		r.mustFail("symlink CLAUDE.md", "not the same in both trees")
	})
	t.Run("added at a table path", func(t *testing.T) {
		r := newInstructionRepo(t, map[string]string{"AGENTS.md": "rules\n"})
		r.link("GEMINI.md", "AGENTS.md")
		r.mustFail("symlink GEMINI.md", "not the same in both trees")
	})
	t.Run("a link inside .codex deleted while its target changes", func(t *testing.T) {
		r := newInstructionRepoWithLinks(t, map[string]string{".codex/c": "c\n", "docs/prompts.md": "p\n"}, map[string]string{".codex/prompts": "../docs/prompts.md"})
		r.remove(".codex/prompts")
		r.write("docs/prompts.md", "steer\n")
		r.mustFail("symlink .codex/prompts")
	})
	t.Run("all of .codex deleted while its link target changes", func(t *testing.T) {
		r := newInstructionRepoWithLinks(t, map[string]string{".codex/c": "c\n", "docs/prompts.md": "p\n"}, map[string]string{".codex/prompts": "../docs/prompts.md"})
		r.remove(".codex")
		r.write("docs/prompts.md", "steer\n")
		r.mustFail("symlink .codex/prompts")
	})
	t.Run("a link above a fixed path whose target changes", func(t *testing.T) {
		r := newInstructionRepoWithLinks(t, map[string]string{"docs/gh/hooks/pre.sh": "x\n"}, map[string]string{".github": "docs/gh"})
		r.write("docs/gh/hooks/pre.sh", "steer\n")
		// A link to a directory outside the table is refused whether or not the
		// build changed it (the rule that replaced the verified roots).
		r.mustFail(".github links an instruction path to the directory docs/gh")
	})
}

func TestReviewInstructionLinkTextRules(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		links map[string]string
		want  string
	}{
		{"dot-dot in the middle", map[string]string{"guide.md": "g\n", "sub/x": "x\n"}, map[string]string{"CLAUDE.md": "sub/../guide.md"}, ".. that is not leading"},
		{"absolute", nil, map[string]string{"CLAUDE.md": "/etc/passwd"}, "not a plain relative path"},
		{"colon", nil, map[string]string{"CLAUDE.md": ":/foo"}, "not a plain relative path"},
		{"comma", nil, map[string]string{"CLAUDE.md": "a,b"}, "not a plain relative path"},
		{"dot component", map[string]string{"guide.md": "g\n"}, map[string]string{"CLAUDE.md": "./guide.md"}, "empty or . component"},
		{"outside the workspace", nil, map[string]string{"CLAUDE.md": "../x"}, "outside it"},
		{"into .git", nil, map[string]string{"CLAUDE.md": ".git/config"}, "into .git"},
		{"into .GIT in another case", nil, map[string]string{"CLAUDE.md": ".GIT/config"}, "into .git"},
		{"the workspace itself", nil, map[string]string{"sub/CLAUDE.md": ".."}, "root or outside"},
		{"through a symlinked component", map[string]string{"real/guide.md": "g\n"}, map[string]string{"alias": "real", "CLAUDE.md": "alias/guide.md"}, "which is itself a symlink"},
		{"to a symlink", map[string]string{"guide.md": "g\n"}, map[string]string{"alias": "guide.md", "CLAUDE.md": "alias"}, "which is itself a symlink"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newInstructionRepoWithLinks(t, tc.files, tc.links)
			r.mustFail(tc.want)
		})
	}
}

// addGitlinkToBase puts a submodule entry into the base commit.
func (r *instructionRepo) addGitlinkToBase(rel string) {
	r.t.Helper()
	r.git("update-index", "--add", "--cacheinfo", "160000,"+r.base+","+rel)
	r.git("commit", "-q", "--amend", "-m", "base")
	r.base = r.git("rev-parse", "HEAD")
}

// A link is allowed when its target is at or under the table, or is one
// regular file that both trees hold identically; nothing else is verifiable.
func TestReviewInstructionLinkTargetRules(t *testing.T) {
	t.Run("a file target beside an untracked file keeps the untracked file", func(t *testing.T) {
		r := newInstructionRepoWithLinks(t, map[string]string{"docs/guide.md": "guide\n"}, map[string]string{"CLAUDE.md": "docs/guide.md"})
		r.commit()
		r.write("docs/notes.md", "notes\n")
		snap, _ := r.mustSnap()
		wantNothing(t, snap)
		if readFile(t, r.abs("docs/notes.md")) != "notes\n" {
			t.Fatal("an untracked file beside the link target was touched")
		}
	})
	t.Run("a file target whose mode changed", func(t *testing.T) {
		r := newInstructionRepoWithLinks(t, map[string]string{"docs/guide.md": "guide\n"}, map[string]string{"CLAUDE.md": "docs/guide.md"})
		r.chmod("docs/guide.md", 0o755)
		r.mustFail("CLAUDE.md is a link to docs/guide.md, which this build changed")
	})
	t.Run("a directory outside the table, untouched", func(t *testing.T) {
		r := newInstructionRepoWithLinks(t, map[string]string{"shared/ok.md": "ok\n"}, map[string]string{".claude/skills": "../shared"})
		r.mustFail(".claude/skills links an instruction path to the directory shared, which is not an instruction path: a review cannot verify it")
	})
	t.Run("a dangling target", func(t *testing.T) {
		r := newInstructionRepoWithLinks(t, nil, map[string]string{".claude/skills": "../a/b"})
		r.mustFail(".claude/skills", "a/b", "neither commit holds")
	})
	t.Run("a target added by the build", func(t *testing.T) {
		r := newInstructionRepoWithLinks(t, nil, map[string]string{"CLAUDE.md": "docs/guide.md"})
		r.write("docs/guide.md", "steer\n")
		r.mustFail("which this build changed")
	})
	t.Run("a submodule as the target", func(t *testing.T) {
		r := newInstructionRepoWithLinks(t, nil, map[string]string{".claude/skills": "../vendor/skills"})
		r.addGitlinkToBase("vendor/skills")
		r.mustFail(".claude/skills is a link into the submodule vendor/skills")
	})
	t.Run("a file under a submodule as the target", func(t *testing.T) {
		r := newInstructionRepoWithLinks(t, nil, map[string]string{"CLAUDE.md": "vendor/lib/docs/GUIDE.md"})
		r.addGitlinkToBase("vendor/lib")
		r.mustFail("CLAUDE.md is a link into the submodule vendor/lib")
	})
	t.Run("a directory that holds a submodule as the target", func(t *testing.T) {
		r := newInstructionRepoWithLinks(t, nil, map[string]string{"CLAUDE.md": "vendor"})
		r.addGitlinkToBase("vendor/lib")
		r.mustFail("CLAUDE.md links an instruction path to the directory vendor")
	})
	t.Run("a refused directory target takes nothing out of that directory", func(t *testing.T) {
		r := newInstructionRepoWithLinks(t, map[string]string{"src/a.go": "package a\n", "src/AGENTS.md": "x\n"}, map[string]string{".claude/skills": "../src"})
		r.commit()
		r.write("src/extra.txt", "e\n")
		r.mustFail("links an instruction path to the directory src")
		for rel, want := range map[string]string{"src/a.go": "package a\n", "src/AGENTS.md": "x\n", "src/extra.txt": "e\n"} {
			if readFile(t, r.abs(rel)) != want {
				t.Fatalf("%s was changed or removed", rel)
			}
		}
	})
	t.Run("the target file is a symlink's neighbour edited on disk", func(t *testing.T) {
		r := newInstructionRepoWithLinks(t, map[string]string{"docs/guide.md": "guide\n"}, map[string]string{".github/copilot-instructions.md": "../docs/guide.md"})
		r.commit()
		r.write("docs/guide.md", "steer\n")
		r.mustFail("does not match the result commit")
	})
}
