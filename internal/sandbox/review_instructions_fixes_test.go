package sandbox

import (
	"context"
	"crypto/sha1" //nolint:gosec // a git blob id
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Only the worktree's own root .git is skipped; any other directory spelled
// .git, or a spelling that folds to it, is an ordinary entry.
func TestReviewInstructionDotGitNamedDirectories(t *testing.T) {
	tracked := map[string]string{".claude/skills/x/SKILL.md": "skill\n"}
	t.Run("an untracked .git under a table match is removed", func(t *testing.T) {
		r := newInstructionRepo(t, tracked)
		r.commit()
		r.write(".claude/skills/.git/SKILL.md", "steer\n")
		snap, _ := r.mustSnap()
		if !reflect.DeepEqual(snap.Removed, []string{".claude/skills/.git"}) || !r.gone(".claude/skills/.git") || readFile(t, r.abs(".claude/skills/x/SKILL.md")) != "skill\n" {
			t.Fatalf("snapshot = %+v", snap)
		}
	})
	t.Run("a fullwidth-g spelling under a table match is removed", func(t *testing.T) {
		r := newInstructionRepo(t, tracked)
		r.commit()
		if _, err := os.Lstat(r.abs(".ｇit")); err == nil {
			t.Skip("the filesystem treats .ｇit as .git")
		}
		r.write(".claude/skills/.ｇit/SKILL.md", "steer\n")
		r.write(".ｇit/AGENTS.md", "steer\n")
		snap, _ := r.mustSnap()
		if !reflect.DeepEqual(snap.Removed, []string{".claude/skills/.ｇit", ".ｇit/AGENTS.md"}) || !r.gone(".claude/skills/.ｇit") || !r.gone(".ｇit/AGENTS.md") {
			t.Fatalf("snapshot = %+v", snap)
		}
	})
	t.Run("an instruction path inside a nested .git is an error", func(t *testing.T) {
		for _, p := range []string{"d/.git/AGENTS.md", "d/.git/refs/heads/AGENTS.md"} {
			r := newInstructionRepo(t, nil)
			r.commit()
			r.write(p, "inner\n")
			r.mustFail("instruction path", p, "is inside a nested repository's .git")
			if readFile(t, r.abs(p)) != "inner\n" {
				t.Fatalf("%s was touched", p)
			}
		}
	})
	t.Run("a nested .git without an instruction path is untouched", func(t *testing.T) {
		r := newInstructionRepo(t, nil)
		r.commit()
		r.write("d/.git/config", "inner\n")
		snap, _ := r.mustSnap()
		wantNothing(t, snap)
		if readFile(t, r.abs("d/.git/config")) != "inner\n" {
			t.Fatal("d/.git/config was touched")
		}
	})
	t.Run("the root .git directory is untouched", func(t *testing.T) {
		r := newInstructionRepo(t, nil)
		r.commit()
		r.write("AGENTS.md", "steer\n")
		snap, _ := r.mustSnap()
		if !reflect.DeepEqual(snap.Removed, []string{"AGENTS.md"}) {
			t.Fatalf("snapshot = %+v", snap)
		}
		if info, err := os.Lstat(r.abs(".git")); err != nil || !info.IsDir() {
			t.Fatalf(".git: %v", err)
		}
	})
	t.Run("the .git file of a linked worktree is untouched", func(t *testing.T) {
		main := newInstructionRepo(t, nil)
		wt := filepath.Join(filepath.Dir(main.dir), "linked")
		main.git("worktree", "add", "-q", "--detach", wt, "HEAD")
		r := &instructionRepo{t: t, dir: wt, base: main.base}
		r.write(".pi/x", "x\n")
		r.commit()
		r.write(".pi/y", "y\n")
		snap, _ := r.mustSnap()
		if info, err := os.Lstat(r.abs(".git")); err != nil || !info.Mode().IsRegular() {
			t.Fatalf(".git: %v", err)
		}
		if len(snap.Masks) != 1 || !reflect.DeepEqual(snap.Removed, []string{".pi/y"}) {
			t.Fatalf("snapshot = %+v", snap)
		}
	})
}

func sharpSInsensitive(t *testing.T) bool {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "probess"), nil, 0o640); err != nil {
		t.Fatal(err)
	}
	_, err := os.Stat(filepath.Join(dir, "probeß"))
	return err == nil
}

func TestFoldNameFullCaseFolding(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"ß", "ss"}, {"ẞ", "ss"}, {"addreß.md", "address.md"}, {"ﬆ", "st"}, {"ＡＧＥＮＴＳ.md", "agents.md"},
		{"AGENTS.md", "agents.md"}, {"Kills", "kills"},
		// Turkish letters have no language-independent fold to ASCII: the dotted
		// capital becomes i plus a combining dot, the dotless small stays.
		{"İ", "i̇"}, {"ı", "ı"}, {"I", "i"},
	} {
		if got := foldName(tc.in); got != tc.want {
			t.Errorf("foldName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestReviewInstructionSharpSIsTheSameName(t *testing.T) {
	t.Run("a tracked skill renamed to a sharp-s spelling on disk", func(t *testing.T) {
		if !sharpSInsensitive(t) {
			t.Skip("the filesystem keeps ß and ss apart")
		}
		r := newInstructionRepo(t, map[string]string{".claude/skills/assess/SKILL.md": "skill\n"})
		r.commit()
		if err := os.Rename(r.abs(".claude/skills/assess"), r.abs(".claude/skills/aßess")); err != nil {
			t.Fatal(err)
		}
		r.mustFail("aßess", "assess")
		if readFile(t, r.abs(".claude/skills/aßess/SKILL.md")) != "skill\n" {
			t.Fatal("the skill was removed")
		}
		for _, line := range strings.Split(r.git("status", "--porcelain"), "\n") {
			if strings.Contains(strings.TrimSpace(line), "D ") && strings.HasPrefix(strings.TrimLeft(line, " "), "D") {
				t.Fatalf("git status shows a deletion: %q", line)
			}
		}
	})
	// The same-file check alone, with no fold involved: a hard link under any
	// name is the tracked file, so it is an error and not a removal.
	t.Run("an untracked hard link of a tracked file is refused", func(t *testing.T) {
		r := newInstructionRepo(t, map[string]string{".claude/commands/address.md": "cmd\n"})
		r.commit()
		if err := os.Link(r.abs(".claude/commands/address.md"), r.abs(".claude/commands/zz.md")); err != nil {
			t.Skipf("hard links: %v", err)
		}
		r.mustFail(`".claude/commands/zz.md" is the tracked path ".claude/commands/address.md" under another spelling`)
		if readFile(t, r.abs(".claude/commands/address.md")) != "cmd\n" || readFile(t, r.abs(".claude/commands/zz.md")) != "cmd\n" {
			t.Fatal("a file was removed")
		}
	})
}

func TestReviewInstructionRetainsOnlyWhatItNeeds(t *testing.T) {
	r := newInstructionRepo(t, map[string]string{"AGENTS.md": "rules\n"})
	var in strings.Builder
	blob := r.git("hash-object", "-w", r.abs("main.go"))
	for i := 0; i < 200000; i++ {
		fmt.Fprintf(&in, "100644 %s 0\tsrc/d%03d/f%06d.txt\n", blob, i%500, i)
	}
	cmd := exec.Command("git", "-C", r.dir, "update-index", "--add", "--index-info")
	cmd.Stdin = strings.NewReader(in.String())
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("update-index: %v: %s", err, out)
	}
	r.git("-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "-m", "many")
	r.base = r.git("rev-parse", "HEAD")
	r.write("AGENTS.md", "steer\n")
	r.git("add", "AGENTS.md")
	r.commitStaged()
	plan, cands, err := planSnapshot(context.Background(), r.dir, r.base, r.result)
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 || plan.retained >= 100 {
		t.Fatalf("candidates = %d, retained entries = %d", len(cands), plan.retained)
	}
	snap, _ := r.mustSnap()
	wantPaths(t, snap, "AGENTS.md")
}

func blobOID(content string) string {
	h := sha1.New() //nolint:gosec
	fmt.Fprintf(h, "blob %d\x00%s", len(content), content)
	return hex.EncodeToString(h.Sum(nil))
}

func TestFileHashesToLineEndings(t *testing.T) {
	for _, tc := range []struct {
		name, blob, disk string
		want             bool
	}{
		{"identical", "x\ny\n", "x\ny\n", true},
		{"a CRLF checkout", "x\ny\n", "x\r\ny\r\n", true},
		{"a doubled carriage return", "x\r\ny\n", "x\r\r\ny\n", false},
		{"mixed endings", "x\ny\n", "x\r\ny\n", false},
		{"a lone carriage return kept", "x\ry\n", "x\ry\r\n", true},
		{"a lone carriage return dropped", "x\ry\n", "xy\r\n", false},
		{"a trailing carriage return kept", "x\ny\r", "x\r\ny\r", true},
		{"a lone carriage return added", "x\ny\n", "x\r\ny\r\n\r", false},
		{"an extra line", "x\n", "x\r\n\r\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "f")
			if err := os.WriteFile(p, []byte(tc.disk), 0o640); err != nil {
				t.Fatal(err)
			}
			if got := fileHashesTo(p, int64(len(tc.disk)), blobOID(tc.blob)); got != tc.want {
				t.Fatalf("fileHashesTo(blob %q, disk %q) = %v, want %v", tc.blob, tc.disk, got, tc.want)
			}
		})
	}
}

func TestReviewInstructionErrorClearsDestination(t *testing.T) {
	r := newInstructionRepo(t, map[string]string{"AGENTS.md": "rules\n"})
	r.write("AGENTS.md", "steer\n")
	r.commit()
	snap, dst := r.mustSnap()
	if len(snap.Masks) != 1 {
		t.Fatalf("snapshot = %+v", snap)
	}
	if _, err := os.Stat(dst); err != nil {
		t.Fatalf("first call left no snapshot: %v", err)
	}
	r.write("AGENTS.md", "edited after the commit\n")
	if _, _, err := r.snapshot(); err == nil {
		t.Fatal("the second call succeeded")
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatalf("dst after an error: %v", err)
	}
	// An argument error after the destination was accepted clears it too.
	r.write("AGENTS.md", "steer\n")
	if _, _, err := r.snapshot(); err != nil {
		t.Fatal(err)
	}
	if _, err := SnapshotReviewInstructions(context.Background(), r.dir, r.base, r.base, dst); err == nil {
		t.Fatal("a wrong result commit was accepted")
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatalf("dst after a bad commit: %v", err)
	}
}

func TestReviewInstructionFilterHint(t *testing.T) {
	r := newInstructionRepo(t, map[string]string{"AGENTS.md": "rules\n"})
	r.commit()
	r.write("AGENTS.md", "other\n")
	r.mustFail("does not match the result commit; if this repository converts files on checkout (working-tree-encoding, ident, a filter such as LFS) for this path, a review of it cannot run")
}
