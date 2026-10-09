package sandbox

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReviewInstructionArguments(t *testing.T) {
	r := newInstructionRepo(t, map[string]string{"AGENTS.md": "rules\n"})
	r.write("AGENTS.md", "steer\n")
	r.commit()
	dst := filepath.Join(filepath.Dir(r.dir), "snap")
	call := func(base, result, to string) error {
		_, err := SnapshotReviewInstructions(context.Background(), r.dir, base, result, to)
		return err
	}
	if err := call(r.base, r.result, dst); err != nil {
		t.Fatalf("control call: %v", err)
	}
	cases := []struct {
		name, base, result, dst, want string
	}{
		{"HEAD is not the result", r.base, r.base, dst, "not the result commit"},
		{"a short SHA", "abc", r.result, dst, "not a full 40-hex"},
		{"an upper-case SHA", strings.ToUpper(r.base), r.result, dst, "not a full 40-hex"},
		{"an empty result", r.base, "", dst, "not a full 40-hex"},
		{"a branch name", "HEAD", r.result, dst, "not a full 40-hex"},
		{"dst inside the worktree", r.base, r.result, filepath.Join(r.dir, "snap"), "overlaps the workspace"},
		{"dst is the worktree", r.base, r.result, r.dir, "overlaps the workspace"},
		{"a relative dst", r.base, r.result, "snap", "absolute"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := call(tc.base, tc.result, tc.dst)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestReviewInstructionCaps(t *testing.T) {
	t.Run("a 3 MiB blob under .codex", func(t *testing.T) {
		r := newInstructionRepo(t, nil)
		r.write(".codex/big", strings.Repeat("x", 3<<20))
		r.mustFail("is over 2097152 bytes", ".codex/big")
	})
	t.Run("2001 files", func(t *testing.T) {
		r := newInstructionRepo(t, nil)
		for i := 0; i <= 2000; i++ {
			r.write(fmt.Sprintf(".codex/f%04d", i), "x")
		}
		r.mustFail("more than 2000 files", ".codex")
	})
	t.Run("65 masks", func(t *testing.T) {
		r := newInstructionRepo(t, nil)
		for i := 0; i < 65; i++ {
			r.write(fmt.Sprintf("pkg%02d/AGENTS.md", i), "x")
		}
		r.mustFail("65 instruction paths changed, over the limit of 64")
	})
	t.Run("64 masks pass", func(t *testing.T) {
		r := newInstructionRepo(t, nil)
		for i := 0; i < 64; i++ {
			r.write(fmt.Sprintf("pkg%02d/AGENTS.md", i), "x")
		}
		snap, _ := r.mustSnap()
		if len(snap.Masks) != 64 {
			t.Fatalf("Masks = %d", len(snap.Masks))
		}
	})
}

func TestReviewInstructionDiffFile(t *testing.T) {
	t.Run("holds no host path", func(t *testing.T) {
		r := newInstructionRepo(t, map[string]string{"AGENTS.md": "rules\n", ".gitignore": "GEMINI.md\n", ".github/hooks/h.sh": "x\n"})
		r.write("AGENTS.md", "rules\nmore\n")
		r.chmod(".github/hooks/h.sh", 0o755)
		r.commit()
		r.write("GEMINI.md", "steer\n")
		snap, dst := r.mustSnap()
		diff := readFile(t, snap.DiffPath)
		for _, host := range []string{r.dir, dst, filepath.Dir(r.dir), "/private", "/tmp", "/var"} {
			if strings.Contains(diff, host) {
				t.Fatalf("diff holds %q:\n%s", host, diff)
			}
		}
		if !strings.Contains(diff, "+more") || !strings.Contains(diff, "mode changed") || !strings.Contains(diff, "+steer") {
			t.Fatalf("diff = %q", diff)
		}
	})
	t.Run("is bounded", func(t *testing.T) {
		r := newInstructionRepo(t, nil)
		line := strings.Repeat("y", 99) + "\n"
		for i := 0; i < 12; i++ {
			r.write(fmt.Sprintf("p%02d/AGENTS.md", i), strings.Repeat(line, 4000)) // 400 KB each
		}
		snap, _ := r.mustSnap()
		info, err := os.Stat(snap.DiffPath)
		if err != nil {
			t.Fatal(err)
		}
		diff := readFile(t, snap.DiffPath)
		if info.Size() > 2<<20+4096 || strings.Count(diff, "=== ") != 12 || !strings.Contains(diff, "[truncated: ") {
			t.Fatalf("diff size %d, headers %d", info.Size(), strings.Count(diff, "=== "))
		}
	})
	t.Run("worktree attributes decide nothing", func(t *testing.T) {
		r := newInstructionRepo(t, map[string]string{"AGENTS.md": "rules\n", ".gitattributes": "AGENTS.md -diff\n"})
		r.write("AGENTS.md", "rules\nmore\n")
		snap, _ := r.mustSnap()
		if !strings.Contains(readFile(t, snap.DiffPath), "+more") {
			t.Fatal("-diff attribute hid the change")
		}
	})
}

func TestReviewInstructionHashCoversRemoved(t *testing.T) {
	build := func(untracked string) string {
		r := newInstructionRepo(t, map[string]string{"AGENTS.md": "rules\n"})
		r.write("AGENTS.md", "steer\n")
		r.commit()
		if untracked != "" {
			r.write(untracked, "x\n")
		}
		snap, _ := r.mustSnap()
		return snap.SHA256
	}
	same, none, other := build(".pi/a"), build(""), build(".codex/a")
	if same == "" || same == none || same == other || same != build(".pi/a") {
		t.Fatalf("hashes: with .pi/a %q, none %q, with .codex/a %q", same, none, other)
	}
}
