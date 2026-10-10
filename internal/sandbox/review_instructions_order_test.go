package sandbox

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// withReviewInstructionTimeout sets the snapshot's own deadline for a test.
func withReviewInstructionTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	old := reviewInstructionTimeout
	reviewInstructionTimeout = d
	t.Cleanup(func() { reviewInstructionTimeout = old })
}

func isCostRefusal(err error) bool {
	return err != nil && strings.Contains(err.Error(), "too costly to compare")
}

func entriesUnder(t *testing.T, dir string) int {
	t.Helper()
	list, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return len(list)
}

// A refusal for cost leaves the worktree untouched, wherever in the snapshot
// the deadline falls: nothing is removed until everything that can refuse has
// run, and from the first removal on the deadline no longer applies. Each
// shape is snapshotted under a row of deadlines spread over the time one full
// snapshot of it takes.
func TestReviewInstructionRefusalForCostLeavesTheWorktreeUntouched(t *testing.T) {
	if testing.Short() {
		t.Skip("snapshots two costly worktrees a dozen times each")
	}
	shapes := []struct {
		name      string
		build     func(r *instructionRepo)
		untracked string // the path a snapshot removes
		plant     func(r *instructionRepo)
		planted   func(r *instructionRepo) bool
	}{
		{
			// The diff of every changed file is written after the worktree was
			// read: 150 of them, and one untracked instruction file.
			name: "many changed instruction files and one untracked",
			build: func(r *instructionRepo) {
				for i := 0; i < 150; i++ {
					r.write(fmt.Sprintf(".claude/skills/s%04d/SKILL.md", i), fmt.Sprintf("skill %d\n", i))
				}
			},
			untracked: "pkg/AGENTS.md",
			plant:     func(r *instructionRepo) { r.write("pkg/AGENTS.md", "steer\n") },
			planted:   func(r *instructionRepo) bool { return !r.gone("pkg/AGENTS.md") },
		},
		{
			// One untracked directory whose removal alone outlasts the deadline.
			name:      "one untracked directory of 30,000 files",
			build:     func(r *instructionRepo) { r.write("main.go", "package main // changed\n") },
			untracked: ".claude/cache",
			plant: func(r *instructionRepo) {
				if err := os.MkdirAll(r.abs(".claude/cache"), 0o755); err != nil {
					r.t.Fatal(err)
				}
				for i := entriesUnder(r.t, r.abs(".claude/cache")); i < 30000; i++ {
					if err := os.WriteFile(r.abs(fmt.Sprintf(".claude/cache/f%05d", i)), nil, 0o644); err != nil {
						r.t.Fatal(err)
					}
				}
			},
			planted: func(r *instructionRepo) bool { return entriesUnder(r.t, r.abs(".claude/cache")) == 30000 },
		},
	}
	for _, shape := range shapes {
		t.Run(shape.name, func(t *testing.T) {
			r := newInstructionRepo(t, map[string]string{".claude/keep": "k\n", ".gitignore": "pkg/\n.claude/cache/\n"})
			shape.build(r)
			r.commit()
			snapshotUnder := func(deadline time.Duration) (ReviewInstructionSnapshot, time.Duration, error) {
				shape.plant(r)
				withReviewInstructionTimeout(t, deadline)
				start := time.Now()
				snap, _, err := r.snapshot()
				return snap, time.Since(start), err
			}
			snap, full, err := snapshotUnder(5 * time.Minute)
			if err != nil || !reflect.DeepEqual(snap.Removed, []string{shape.untracked}) {
				t.Fatalf("with no deadline in the way: Removed = %v, err = %v", snap.Removed, err)
			}
			refused, removed := 0, 0
			for step := 1; step <= 12; step++ {
				deadline := full * time.Duration(step) / 12
				snap, took, err := snapshotUnder(deadline)
				switch {
				case isCostRefusal(err):
					refused++
					if !shape.planted(r) {
						t.Errorf("deadline %v of a %v snapshot: refused for cost after %v (%.100v), and %s was removed from the worktree", deadline.Round(time.Millisecond), full.Round(time.Millisecond), took.Round(time.Millisecond), err, shape.untracked)
					}
					if len(snap.Removed) != 0 {
						t.Errorf("deadline %v: a refusal for cost reports removals %v", deadline, snap.Removed)
					}
				case err != nil:
					t.Fatalf("deadline %v: %v", deadline, err)
				default:
					removed++
					if !reflect.DeepEqual(snap.Removed, []string{shape.untracked}) || shape.planted(r) {
						t.Errorf("deadline %v: Removed = %v, want %s gone and listed", deadline, snap.Removed, shape.untracked)
					}
				}
			}
			t.Logf("a full snapshot took %v; under 12 deadlines up to that: %d refused for cost with the worktree untouched, %d removed %s", full.Round(time.Millisecond), refused, removed, shape.untracked)
		})
	}
}

// A removal that fails midway is an error that says how many paths were
// removed, and the record still lists each of them: the operator can see what
// the snapshot took from the worktree.
func TestReviewInstructionFailedRemovalStillRecordsWhatWasRemoved(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root removes from a read-only directory")
	}
	r := newInstructionRepo(t, map[string]string{".gitignore": "a/\nzz/\n"})
	r.write("main.go", "package main // changed\n")
	r.commit()
	r.write("a/AGENTS.md", "first\n")
	r.write("zz/AGENTS.md", "second\n")
	r.chmod("zz", 0o555)
	t.Cleanup(func() { _ = os.Chmod(r.abs("zz"), 0o755) })
	snap, dst, err := r.snapshot()
	if err == nil {
		t.Fatalf("the snapshot removed a file from a read-only directory: %+v", snap)
	}
	if !strings.Contains(err.Error(), "after removing 1 of 2 untracked instruction paths") || !strings.Contains(err.Error(), `"zz/AGENTS.md"`) {
		t.Errorf("err = %v, want it to say 1 of 2 was removed and name the one that failed", err)
	}
	if !reflect.DeepEqual(snap.Removed, []string{"a/AGENTS.md"}) || len(snap.Masks) != 0 || snap.DiffPath != "" || snap.SHA256 != "" {
		t.Errorf("snapshot = %+v, want only Removed = [a/AGENTS.md]", snap)
	}
	if !r.gone("a/AGENTS.md") || r.gone("zz/AGENTS.md") {
		t.Errorf("a/AGENTS.md gone = %v, zz/AGENTS.md gone = %v", r.gone("a/AGENTS.md"), r.gone("zz/AGENTS.md"))
	}
	if _, serr := os.Stat(dst); !os.IsNotExist(serr) {
		t.Errorf("the failed snapshot left %s behind", dst)
	}
}

// countGit puts a git on PATH that logs each start and runs the real one, and
// returns the number of git processes started since.
func countGit(t *testing.T) func() int {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	log := filepath.Join(dir, "starts")
	script := "#!/bin/sh\necho x >> '" + log + "'\nexec '" + real + "' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return func() int {
		raw, err := os.ReadFile(log)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		return strings.Count(string(raw), "\n")
	}
}

// A snapshot reads every blob and every link text through one git process,
// not one or two for each: 120 changed instruction files and 120 links at
// instruction paths cost the listings, that reader and one diff a changed
// file, where they once cost three processes a file and two a link.
func TestReviewInstructionBlobsAreReadThroughOneProcess(t *testing.T) {
	const files, links = 120, 120
	linkMap := map[string]string{}
	for i := 0; i < links; i++ {
		linkMap[fmt.Sprintf(".claude/l%03d", i)] = "keep"
	}
	base := map[string]string{".claude/keep": "k\n"}
	for i := 0; i < files; i++ {
		base[fmt.Sprintf(".codex/f%03d.md", i)] = "old\n"
	}
	r := newInstructionRepoWithLinks(t, base, linkMap)
	for i := 0; i < files; i++ {
		r.write(fmt.Sprintf(".codex/f%03d.md", i), "new\n")
	}
	r.commit()
	started := countGit(t)
	snap, _ := r.mustSnap()
	n := started()
	if len(snap.Masks) != 1 || strings.Count(readFile(t, snap.DiffPath), "+new\n") != files {
		t.Fatalf("masks = %d, diff of %d files incomplete", len(snap.Masks), files)
	}
	t.Logf("%d changed instruction files and %d links: %d git processes", files, links, n)
	if n > files+12 {
		t.Errorf("%d git processes for %d changed files and %d links, want one diff a changed file and a handful more", n, files, links)
	}
}

// The snapshot's deadline is the commit-directory snapshot's.
func TestReviewInstructionDeadlineIsTheCommitDirectorySnapshots(t *testing.T) {
	if reviewInstructionTimeout != commitDirTimeout {
		t.Fatalf("the review snapshot's deadline is %v, the commit-directory snapshot's %v", reviewInstructionTimeout, commitDirTimeout)
	}
}

// A link text over its limit is refused before it is read, and a link text
// within it is read whole: the tree holds a link of 4,502 bytes that no
// filesystem would take, beside an ordinary one.
func TestReviewInstructionLinkTextIsReadWithinItsLimit(t *testing.T) {
	r := newInstructionRepoWithLinks(t, map[string]string{".claude/keep": "k\n"}, map[string]string{".claude/a": "keep"})
	r.stage("120000", ".claude/b", "./"+strings.Repeat("x/../", 900)+"keep")
	r.commitStaged()
	r.base = r.result
	r.stage("100644", "main.go", "package main // changed\n")
	r.commitStaged()
	_, _, err := r.snapshot()
	if err == nil || !strings.Contains(err.Error(), "is over 4096 bytes") {
		t.Fatalf("err = %.300v, want the link text over its limit refused", err)
	}
}
