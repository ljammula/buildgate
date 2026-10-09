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
	s := newPlan()
	for i := 0; i < entries; i++ {
		e := treeEntry{path: path(i), mode: "100644", oid: strings.Repeat("0", 40)}
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
		// 1,000 chains of 1,500 directories: 1.5 million directories.
		{"many deep directories", 1000, func(i int) string { return deepLeadPath(i, 1500) }, "directories that lead to or lie under an instruction path"},
		// 2,000 directories, each under a name of 500,000 bytes: 1 GB of paths.
		{"long names", 2000, func(i int) string { return fmt.Sprintf("%06d", i) + strings.Repeat("n", 500000) + "/.github/x" }, "bytes of paths that lead to or lie under an instruction path"},
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
	for _, chains := range []int{20, 40, 60} {
		before := heapNow()
		s, took, refusal := indexUntilRefused(chains, func(i int) string { return deepLeadPath(i, 1500) })
		kept := int64(heapNow()) - int64(before)
		if refusal != "" || took != chains {
			t.Fatalf("%d chains of 1,500 directories: refused after %d: %s", chains, took, refusal)
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
	cmd := exec.Command("git", "-C", r.dir, "fast-import", "--quiet")
	cmd.Stdin = &in
	if out, err := cmd.CombinedOutput(); err != nil {
		r.t.Fatalf("git fast-import: %v: %s", err, out)
	}
	return r.git("rev-parse", "refs/heads/"+branch)
}

// The refusal reaches the caller of the snapshot: a real result commit of 150
// chains of 1,500 directories (225,000 of them) launches no review, and
// nothing is left under the destination.
func TestReviewInstructionSnapshotRefusesTooManyRecordedDirectories(t *testing.T) {
	if testing.Short() {
		t.Skip("imports a commit of 225,000 trees")
	}
	r := newInstructionRepo(t, nil)
	paths := make([]string, 150)
	for i := range paths {
		paths[i] = deepLeadPath(i, 1500)
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
