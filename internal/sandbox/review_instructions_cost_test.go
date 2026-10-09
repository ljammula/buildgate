package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// flatTree writes one git tree of files blobs and returns its id.
func (r *instructionRepo) flatTree(files int) string {
	r.t.Helper()
	r.write("blob.txt", "x\n")
	oid := r.git("hash-object", "-w", r.abs("blob.txt"))
	r.remove("blob.txt")
	var in bytes.Buffer
	for i := 0; i < files; i++ {
		fmt.Fprintf(&in, "100644 blob %s\tf%07d\n", oid, i)
	}
	cmd := exec.Command("git", "-C", r.dir, "mktree")
	cmd.Stdin = &in
	out, err := cmd.Output()
	if err != nil {
		r.t.Fatalf("git mktree: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// importTree commits, on top of the base commit, the tree at dir (a
// slash-separated directory) and returns the commit. Only git's object store
// is written (git fast-import): the worktree keeps the base checkout, which is
// all a snapshot of paths outside the table reads.
func (r *instructionRepo) importTree(branch, dir, tree string) string {
	r.t.Helper()
	in := fmt.Sprintf("commit refs/heads/%s\ncommitter t <t@example.com> 0 +0000\ndata 5\ntree\n\nfrom %s\nM 040000 %s %s\n", branch, r.base, tree, dir)
	cmd := exec.Command("git", "-C", r.dir, "fast-import", "--quiet")
	cmd.Stdin = strings.NewReader(in)
	if out, err := cmd.CombinedOutput(); err != nil {
		r.t.Fatalf("git fast-import: %v: %s", err, out)
	}
	return r.git("rev-parse", "refs/heads/"+branch)
}

// timeSnapshot moves HEAD alone to result and times one snapshot of it.
func (r *instructionRepo) timeSnapshot(result string) time.Duration {
	r.t.Helper()
	r.git("update-ref", "--no-deref", "HEAD", result)
	start := time.Now()
	snap, err := SnapshotReviewInstructions(context.Background(), r.dir, r.base, result, filepath.Join(filepath.Dir(r.dir), "snap"))
	took := time.Since(start)
	if err != nil {
		r.t.Fatalf("snapshot: %v", err)
	}
	wantNothing(r.t, snap)
	return took
}

// A result tree of 100,000 files in one directory 1,500 levels down: when
// that directory is a .github (a proper prefix of table entries, none of the
// files an instruction path) the snapshot must cost what it costs for a
// directory of any other name. The spellings of the directories above are
// recorded once per directory, never once per file.
func TestReviewInstructionDeepLeadDirectoryCostsWhatAnyDeepDirectoryCosts(t *testing.T) {
	if testing.Short() {
		t.Skip("snapshots two trees of 100,000 deep paths")
	}
	const depth, files = 1500, 100000
	deep := strings.TrimSuffix(strings.Repeat("a/", depth), "/")
	r := newInstructionRepo(t, nil)
	tree := r.flatTree(files)
	control := r.timeSnapshot(r.importTree("control", deep+"/src", tree))
	lead := r.timeSnapshot(r.importTree("lead", deep+"/.github", tree))
	t.Logf("%d files %d levels down: under src %v, under .github %v", files, depth, control, lead)
	if lead > 3*control {
		t.Errorf("a snapshot of %d files under a deep .github took %v, over three times the %v of the same tree under src", files, lead, control)
	}
}
