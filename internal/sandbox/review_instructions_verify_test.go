package sandbox

import (
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
)

// A link's target outside the table is one regular file, verified on disk
// like a table path. The earlier tests of a directory target as a root to
// verify and clean (an ignored skill written under it was removed) are gone:
// a directory target outside the table is an error now, see
// TestReviewInstructionLinkTargetRules.
func TestReviewInstructionLinkTargetsAreVerifiedOnDisk(t *testing.T) {
	t.Run("the target of a file link edited on disk after the commit", func(t *testing.T) {
		r := newInstructionRepoWithLinks(t, map[string]string{"docs/guide.md": "guide\n"}, map[string]string{"CLAUDE.md": "docs/guide.md"})
		r.commit()
		r.write("docs/guide.md", "steer\n")
		r.mustFail("does not match the result commit", "docs/guide.md")
	})
	t.Run("an untouched file link", func(t *testing.T) {
		r := newInstructionRepoWithLinks(t, map[string]string{"docs/guide.md": "guide\n"}, map[string]string{"CLAUDE.md": "docs/guide.md"})
		snap, _ := r.mustSnap()
		wantNothing(t, snap)
	})
}

// A mask target never sits below a link, in the result commit or on disk.
func TestReviewInstructionMaskTargetsHaveRealParents(t *testing.T) {
	for name, text := range map[string]string{"an absolute host path": "", "../..": "../.."} {
		t.Run("pkg replaced by a link to "+name, func(t *testing.T) {
			r := newInstructionRepo(t, map[string]string{"pkg/AGENTS.md": "rules\n"})
			outside := t.TempDir()
			if err := os.WriteFile(outside+"/keep", []byte("k"), 0o600); err != nil {
				t.Fatal(err)
			}
			if text == "" {
				text = outside
			}
			r.remove("pkg")
			r.link("pkg", text)
			r.mustFail("pkg", "symlink")
			if got := filesUnder(t, outside); !reflect.DeepEqual(got, []string{"keep"}) {
				t.Fatalf("outside = %v", got)
			}
		})
	}
	t.Run("pkg swapped for a link on disk after the commit", func(t *testing.T) {
		r := newInstructionRepo(t, map[string]string{"pkg/AGENTS.md": "rules\n"})
		r.write("pkg/AGENTS.md", "steer\n")
		r.commit()
		outside := t.TempDir()
		r.remove("pkg")
		r.link("pkg", outside)
		r.mustFail("pkg")
		if got := filesUnder(t, outside); len(got) != 0 {
			t.Fatalf("outside = %v", got)
		}
	})
}

func TestReviewInstructionSubmodulesAndNestedGitDirs(t *testing.T) {
	gitlinkRepo := func(t *testing.T) *instructionRepo {
		r := newInstructionRepo(t, nil)
		r.git("update-index", "--add", "--cacheinfo", "160000,"+r.base+",vendor/lib")
		r.commitStaged()
		return r
	}
	t.Run("an instruction path inside a submodule checkout", func(t *testing.T) {
		r := gitlinkRepo(t)
		r.write("vendor/lib/AGENTS.md", "steer\n")
		r.mustFail("vendor/lib/AGENTS.md", "inside the submodule checkout", `"vendor/lib"`)
		if readFile(t, r.abs("vendor/lib/AGENTS.md")) != "steer\n" {
			t.Fatal("the file inside the submodule was touched")
		}
	})
	t.Run("an empty submodule directory", func(t *testing.T) {
		r := gitlinkRepo(t)
		r.write("vendor/lib/keep.txt", "k\n")
		if err := os.Remove(r.abs("vendor/lib/keep.txt")); err != nil {
			t.Fatal(err)
		}
		snap, _ := r.mustSnap()
		wantNothing(t, snap)
	})
	t.Run("a missing submodule directory", func(t *testing.T) {
		snap, _ := gitlinkRepo(t).mustSnap()
		wantNothing(t, snap)
	})
	t.Run("a nested .git directory is walked and left alone", func(t *testing.T) {
		r := newInstructionRepo(t, nil)
		r.commit()
		r.write("d/.git/config", "inner\n")
		r.write("d/AGENTS.md", "steer\n")
		snap, _ := r.mustSnap()
		if !reflect.DeepEqual(snap.Removed, []string{"d/AGENTS.md"}) || !r.gone("d/AGENTS.md") || readFile(t, r.abs("d/.git/config")) != "inner\n" {
			t.Fatalf("snapshot = %+v", snap)
		}
	})
}

func TestReviewInstructionRemovedBodiesAreBounded(t *testing.T) {
	r := newInstructionRepo(t, nil)
	r.commit()
	for i := 0; i < 40; i++ {
		r.write(fmt.Sprintf("d%02d/AGENTS.md", i), strings.Repeat("z", 200<<10))
	}
	snap, _ := r.mustSnap()
	if len(snap.Removed) != 40 {
		t.Fatalf("Removed = %d", len(snap.Removed))
	}
	for _, p := range snap.Removed {
		if !r.gone(p) {
			t.Fatalf("%s is still there", p)
		}
	}
	diff := readFile(t, snap.DiffPath)
	if n := strings.Count(diff, "(untracked, removed before review)"); n != 40 || len(diff) > 2<<20+16<<10 {
		t.Fatalf("headers = %d, diff size = %d", n, len(diff))
	}
	if !strings.Contains(diff, "[regular file of 204800 bytes, not shown]") {
		t.Fatal("a body past the budget was not recorded by its header only")
	}
}

func TestReviewInstructionNothingIsRemovedBeforeEveryCheckPassed(t *testing.T) {
	r := newInstructionRepo(t, nil)
	r.commit()
	r.write("-x/AGENTS.md", "steer\n") // walked before .github
	r.link(".github", "docs")
	r.mustFail("symlink the result commit does not hold")
	if r.gone("-x/AGENTS.md") {
		t.Fatal("an entry was removed although the snapshot failed")
	}
	r.remove(".github")
	snap, _ := r.mustSnap()
	if !reflect.DeepEqual(snap.Removed, []string{"-x/AGENTS.md"}) || !r.gone("-x/AGENTS.md") {
		t.Fatalf("snapshot = %+v", snap)
	}
}

func TestReviewInstructionTwoCallsAgree(t *testing.T) {
	r := newInstructionRepo(t, map[string]string{"AGENTS.md": "rules\n"})
	r.write("AGENTS.md", "steer\n")
	r.commit()
	r.write(".pi/x", "x\n")
	first, _ := r.mustSnap()
	second, _ := r.mustSnap()
	if len(first.Removed) != 1 || len(second.Removed) != 0 || first.SHA256 == "" || first.SHA256 != second.SHA256 || !reflect.DeepEqual(first.Masks, second.Masks) {
		t.Fatalf("first = %+v, second = %+v", first, second)
	}
}

func TestReviewInstructionLineEndingsOfACheckout(t *testing.T) {
	r := newInstructionRepo(t, map[string]string{"AGENTS.md": "rules\nmore\n", ".gitattributes": "* text eol=crlf\n"})
	r.remove("AGENTS.md")
	r.git("checkout", "--", "AGENTS.md")
	if !strings.Contains(readFile(t, r.abs("AGENTS.md")), "\r\n") {
		t.Fatal("the checkout has no CRLF")
	}
	r.commit()
	if st := r.git("status", "--porcelain"); st != "" {
		t.Fatalf("status = %q", st)
	}
	snap, _ := r.mustSnap()
	wantNothing(t, snap)
	r.write("AGENTS.md", "steer\r\nmore\r\n")
	r.mustFail(`tracked instruction path "AGENTS.md" does not match the result commit`)
}

func TestReviewInstructionBlobSizes(t *testing.T) {
	t.Run("an untouched 3 MiB tracked file", func(t *testing.T) {
		r := newInstructionRepo(t, map[string]string{".claude/big": strings.Repeat("x", 3<<20)})
		r.commit()
		snap, _ := r.mustSnap()
		wantNothing(t, snap)
	})
	t.Run("two 1.5 MiB files beside an edited settings file", func(t *testing.T) {
		a, b := strings.Repeat("a", 3<<19), strings.Repeat("b", 3<<19)
		r := newInstructionRepo(t, map[string]string{".claude/a": a, ".claude/b": b, ".claude/settings.json": "{}\n"})
		r.write(".claude/settings.json", "{\"x\":1}\n")
		snap, _ := r.mustSnap()
		wantPaths(t, snap, ".claude")
		if m := snap.Masks[0]; !m.Dir || readFile(t, m.Source+"/a") != a || readFile(t, m.Source+"/b") != b || readFile(t, m.Source+"/settings.json") != "{}\n" {
			t.Fatalf("mask = %+v", m)
		}
	})
}
