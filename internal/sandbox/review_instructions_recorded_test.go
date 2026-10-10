package sandbox

import (
	"bytes"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"testing"
)

// heapNow is the live heap after a collection.
func heapNow() uint64 {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}

// deepLeadPath is file x in a .github directory depth levels below its own
// top directory c<i>: no instruction path, so nothing of it is retained as an
// entry, and every directory above it is one register records.
func deepLeadPath(i, depth int) string {
	return fmt.Sprintf("c%06d/", i) + strings.Repeat("a/", depth) + ".github/x"
}

// indexUntilRefused feeds one commit's listing to a plan and returns the
// plan, the number of entries it took and the refusal ("" when it took all).
func indexUntilRefused(entries int, path func(i int) string) (*planState, int, string) {
	return indexModeUntilRefused("100644", entries, path)
}

func indexModeUntilRefused(mode string, entries int, path func(i int) string) (*planState, int, string) {
	s := newPlan()
	for i := 0; i < entries; i++ {
		e := treeEntry{path: path(i), mode: mode, oid: strings.Repeat("0", 40)}
		if err := s.index(s.res, e, 1); err != nil {
			return s, i, err.Error()
		}
	}
	return s, entries, ""
}

// What a plan keeps of a listing is bounded by a constant whatever the shape
// of the tree: neither many deep directories (each a map entry) nor a few
// directories under very long names (each a retained path) can grow it past
// the bound, and a listing over either limit is refused.
func TestReviewInstructionRecordedDirectoriesBoundMemory(t *testing.T) {
	const boundBytes = 256 << 20
	cases := []struct {
		name    string
		entries int
		path    func(i int) string
		want    string
	}{
		// 2,000 chains of 100 directories: 200,000 directories under little text.
		{"many directories", 2000, func(i int) string { return deepLeadPath(i, 100) }, "directories that lead to or lie under an instruction path"},
		// 1,000 chains of 1,500 directories: 1.5 million directories, and every
		// directory's prefix is text the worktree check spells out.
		{"many deep directories", 1000, func(i int) string { return deepLeadPath(i, 1500) }, "bytes of instruction-path, link or submodule paths in commit"},
		// 2,000 directories, each under a name of 500,000 bytes: 1 GB of paths.
		{"long names", 2000, func(i int) string { return fmt.Sprintf("%06d", i) + strings.Repeat("n", 500000) + "/.github/x" }, "bytes of instruction-path, link or submodule paths in commit"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := heapNow()
			s, took, refusal := indexUntilRefused(tc.entries, tc.path)
			kept := int64(heapNow()) - int64(before)
			t.Logf("took %d of %d entries, recorded %d spellings, plan keeps %d MiB; refusal: %q", took, tc.entries, len(s.spell), kept>>20, refusal)
			if !strings.Contains(refusal, tc.want) {
				t.Errorf("refusal = %q, want it to contain %q", refusal, tc.want)
			}
			if kept > boundBytes {
				t.Errorf("the plan keeps %d MiB, over the bound of %d MiB", kept>>20, boundBytes>>20)
			}
			runtime.KeepAlive(s)
		})
	}
}

// The memory a plan keeps without a refusal grows with the directories it
// records; the measurement behind the limits.
func TestReviewInstructionRecordedDirectoryCost(t *testing.T) {
	for _, chains := range []int{300, 600, 900} {
		before := heapNow()
		s, took, refusal := indexUntilRefused(chains, func(i int) string { return deepLeadPath(i, 100) })
		kept := int64(heapNow()) - int64(before)
		if refusal != "" || took != chains {
			t.Fatalf("%d chains of 100 directories: refused after %d: %s", chains, took, refusal)
		}
		t.Logf("%d directories: plan keeps %d KiB (%d bytes a directory)", len(s.resDirs), kept>>10, kept/int64(len(s.resDirs)))
		runtime.KeepAlive(s)
	}
}

// importPaths commits, on top of the base commit, one blob at each path (git
// fast-import: only the object store is written) and returns the commit.
func (r *instructionRepo) importPaths(branch string, paths []string) string {
	r.t.Helper()
	var in bytes.Buffer
	fmt.Fprintf(&in, "blob\nmark :1\ndata 2\nx\n\ncommit refs/heads/%s\ncommitter t <t@example.com> 0 +0000\ndata 5\ntree\n\nfrom %s\n", branch, r.base)
	for _, p := range paths {
		fmt.Fprintf(&in, "M 100644 :1 %s\n", p)
	}
	r.fastImport(&in)
	return r.git("rev-parse", "refs/heads/"+branch)
}

// fastImport feeds a stream to git fast-import in the repository.
func (r *instructionRepo) fastImport(in *bytes.Buffer) {
	r.t.Helper()
	cmd := exec.Command("git", "-C", r.dir, "fast-import", "--quiet")
	cmd.Stdin = in
	if out, err := cmd.CombinedOutput(); err != nil {
		r.t.Fatalf("git fast-import: %v: %s", err, out)
	}
}

// gitRaw is git's standard output, untrimmed.
func (r *instructionRepo) gitRaw(args ...string) string {
	r.t.Helper()
	out, err := exec.Command("git", append([]string{"-C", r.dir}, args...)...).Output()
	if err != nil {
		r.t.Fatalf("git %v: %v", args, err)
	}
	return string(out)
}

// The refusal reaches the caller of the snapshot: a real result commit of
// 1,200 chains of 100 directories (120,000 of them) launches no review.
func TestReviewInstructionSnapshotRefusesTooManyRecordedDirectories(t *testing.T) {
	if testing.Short() {
		t.Skip("imports a commit of 120,000 trees")
	}
	r := newInstructionRepo(t, nil)
	paths := make([]string, 1200)
	for i := range paths {
		paths[i] = deepLeadPath(i, 100)
	}
	r.result = r.importPaths("deep", paths)
	r.git("update-ref", "--no-deref", "HEAD", r.result)
	r.mustFail("directories that lead to or lie under an instruction path", r.result)
}

// A wide repository of ordinary shape stays far inside the limits: 20,000
// packages three levels down, each with its own .github/workflows.
func TestReviewInstructionSnapshotTakesAWideRepository(t *testing.T) {
	s, took, refusal := indexUntilRefused(20000, func(i int) string {
		return fmt.Sprintf("packages/group%03d/pkg%05d/.github/workflows/ci.yml", i%200, i)
	})
	if refusal != "" || took != 20000 {
		t.Fatalf("refused after %d entries: %s", took, refusal)
	}
	if len(s.resDirs) != 1+200+2*20000 {
		t.Fatalf("recorded %d directories, want %d", len(s.resDirs), 1+200+2*20000)
	}
}

// Every path a plan keeps from a listing is under the one byte limit, not
// only the directories register records: a symlink or a submodule anywhere in
// the tree is kept by its path, and a path of very many directories is copied
// once for each of them when the worktree is checked.
func TestReviewInstructionEveryKeptPathIsUnderTheByteLimit(t *testing.T) {
	const boundBytes = 256 << 20
	longName := func(i int) string { return fmt.Sprintf("src/%06d", i) + strings.Repeat("n", 500000) }
	cases := []struct {
		name, mode string
		entries    int
		path       func(i int) string
	}{
		// 2,000 entries outside every instruction path, 1 GB of paths.
		{"symlinks under long names", "120000", 2000, longName},
		{"submodules under long names", "160000", 2000, longName},
		// One file 99,000 directories down: under the directory limit, and
		// 10 GB of directory prefixes once each is spelled out.
		{"one path of 99,000 directories", "100644", 1, func(int) string { return strings.Repeat("a/", 99000) + "AGENTS.md" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := heapNow()
			s, took, refusal := indexModeUntilRefused(tc.mode, tc.entries, tc.path)
			kept := int64(heapNow()) - int64(before)
			t.Logf("took %d of %d entries, plan keeps %d MiB; refusal: %q", took, tc.entries, kept>>20, refusal)
			if !strings.Contains(refusal, "bytes of instruction-path, link or submodule paths in commit") {
				t.Errorf("refusal = %q, want the byte limit", refusal)
			}
			if kept > boundBytes {
				t.Errorf("the plan keeps %d MiB, over the bound of %d MiB", kept>>20, boundBytes>>20)
			}
			runtime.KeepAlive(s)
		})
	}
}

// A link at an instruction path may name a file anywhere, and the plan keeps
// that file's path and checks every directory above it: 50,000 targets of
// 4,000 bytes each are under the same byte limit.
func TestReviewInstructionLinkTargetsAreUnderTheByteLimit(t *testing.T) {
	s := newPlan()
	deep := strings.Repeat(strings.Repeat("n", 1300)+"/", 3) + "f"
	for i := 0; i < 50000; i++ {
		err := s.checkLinkTarget(fmt.Sprintf(".claude/l%05d", i), fmt.Sprintf("t%05d/", i)+deep)
		if err == nil {
			continue
		}
		t.Logf("refused after %d targets: %v", i, err)
		if !strings.Contains(err.Error(), "bytes of instruction-path, link or submodule paths in commit") {
			t.Fatalf("err = %v, want the byte limit", err)
		}
		return
	}
	t.Fatalf("the plan queued all %d link targets of %d bytes each", len(s.targets), len(deep)+7)
}
