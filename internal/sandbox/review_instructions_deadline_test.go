package sandbox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// countingCtx is a context that never fires Done and whose Err is nil until
// it was asked expireAt times (0: never): a deadline measured in checks, so a
// test can place it inside any loop that checks its context.
type countingCtx struct {
	context.Context
	calls    atomic.Int64
	expireAt int64
}

func (c *countingCtx) Err() error {
	if n := c.calls.Add(1); c.expireAt > 0 && n >= c.expireAt {
		return context.DeadlineExceeded
	}
	return nil
}

// snapshotSteps runs the steps of a snapshot that take a context, in order,
// and returns the first error.
func snapshotSteps(ctx context.Context, r *instructionRepo) error {
	dst := filepath.Join(filepath.Dir(r.dir), "snap")
	if err := os.RemoveAll(dst); err != nil {
		return err
	}
	plan, cands, err := planSnapshot(ctx, r.dir, r.base, r.result)
	if err != nil {
		return err
	}
	defer plan.closeBlobs()
	_, diffs, err := plan.stage(ctx, r.dir, dst, cands)
	if err != nil {
		return err
	}
	removed, err := plan.reconcileDisk(ctx, r.dir)
	if err != nil {
		return err
	}
	return writeReviewInstructionDiff(ctx, dst, diffs, removed)
}

// Every loop of a snapshot whose length the repository decides checks its
// context: a snapshot of a few thousand entries asks at least once for each,
// and a deadline that falls in any of them ends the snapshot with it.
func TestReviewInstructionLoopsCheckTheirContext(t *testing.T) {
	const files, tracked, untracked = 1000, 40, 300
	base := map[string]string{".gitignore": "node_modules/\n", "docs/guide.md": "g\n", ".claude/keep": "k\n"}
	for i := 0; i < files; i++ {
		base[fmt.Sprintf("src/d%02d/f%04d.go", i%50, i)] = "package x\n"
	}
	r := newInstructionRepoWithLinks(t, base, map[string]string{".claude/guide": "../docs/guide.md"})
	for i := 0; i < tracked; i++ {
		r.write(fmt.Sprintf(".claude/skills/s%03d/SKILL.md", i), "skill\n")
	}
	r.commit()
	for i := 0; i < untracked; i++ {
		r.write(fmt.Sprintf("node_modules/p%03d/AGENTS.md", i), "pkg\n")
	}
	full := &countingCtx{Context: context.Background()}
	if err := snapshotSteps(full, r); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	total := full.calls.Load()
	// Both listings, the tracked entries verified on disk, and the worktree walk.
	if want := int64(2*files + tracked + files + untracked); total < want {
		t.Fatalf("the snapshot checked its context %d times over two trees of %d files, %d tracked instruction files and a worktree of %d more; want at least %d", total, files, tracked, untracked, want)
	}
	t.Logf("the snapshot checked its context %d times", total)
	for _, at := range []int64{1, total / 8, total / 4, total / 2, 3 * total / 4, total - 1, total} {
		ctx := &countingCtx{Context: context.Background(), expireAt: max(at, 1)}
		if err := snapshotSteps(ctx, r); !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("a deadline at check %d of %d: err = %v, want the deadline", at, total, err)
		}
	}
}

// A result commit of 600 files named .github, each in its own directory 2,001
// levels down, once took eight seconds of string building that no deadline
// could interrupt. The caller's deadline of two seconds now bounds it.
func TestReviewInstructionSnapshotReturnsByTheCallersDeadline(t *testing.T) {
	if testing.Short() {
		t.Skip("imports a commit of 600 files 2,001 levels down")
	}
	const n, deadline, slack = 600, 2 * time.Second, 3 * time.Second
	deep := strings.Repeat("a/", 2000)
	paths := make([]string, n)
	for i := range paths {
		paths[i] = fmt.Sprintf("%sd%04d/.github", deep, i)
	}
	r := newInstructionRepo(t, nil)
	r.result = r.importPaths("lead", paths)
	r.git("update-ref", "--no-deref", "HEAD", r.result)
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()
	start := time.Now()
	_, err := SnapshotReviewInstructions(ctx, r.dir, r.base, r.result, filepath.Join(filepath.Dir(r.dir), "snap"))
	took := time.Since(start)
	t.Logf("%d files named .github 2,001 directories down, deadline %v: returned after %v (err: %.120v)", n, deadline, took.Round(time.Millisecond), err)
	if err == nil {
		t.Error("the snapshot succeeded on a worktree that holds none of the result's files")
	}
	if took > deadline+slack {
		t.Errorf("the snapshot returned %v after a deadline of %v", took.Round(time.Millisecond), deadline)
	}
}

var diffHeaderPattern = regexp.MustCompile(`(?m)^=== "`)
var diffNotListedPattern = regexp.MustCompile(`(?m)^\[(\d+) more instruction paths not listed\]\n\z`)

// The diff file is bounded with its headers: 12,000 untracked files with long
// names under a tracked .claude are all removed, and the file lists as many
// as fit in its bound and then counts the rest in one last line.
func TestReviewInstructionDiffFileIsBoundedWithItsHeaders(t *testing.T) {
	if testing.Short() {
		t.Skip("writes 12,000 files")
	}
	const n = 12000
	r := newInstructionRepo(t, map[string]string{".claude/keep": "k\n"})
	r.write("AGENTS.md", "rules\n")
	r.commit()
	long := strings.Repeat("n", 200)
	for i := 0; i < n; i++ {
		if err := os.WriteFile(r.abs(fmt.Sprintf(".claude/u%05d-%s", i, long)), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	snap, _ := r.mustSnap()
	if len(snap.Removed) != n || !r.gone(snap.Removed[0]) || !r.gone(snap.Removed[n-1]) {
		t.Fatalf("removed %d of %d", len(snap.Removed), n)
	}
	diff := readFile(t, snap.DiffPath)
	headers := len(diffHeaderPattern.FindAllString(diff, -1))
	t.Logf("instructions.diff: %d bytes, %d headers for 1 added and %d removed paths (bound %d)", len(diff), headers, n, maxReviewInstructionDiffBytes)
	if len(diff) > maxReviewInstructionDiffBytes {
		t.Errorf("instructions.diff is %d bytes, over its bound of %d", len(diff), maxReviewInstructionDiffBytes)
	}
	if !strings.HasPrefix(diff, `=== "AGENTS.md" (added by the build) ===`+"\n") || !strings.Contains(diff, "+rules\n") {
		t.Errorf("the changed tracked file does not come first: %.80q", diff)
	}
	m := diffNotListedPattern.FindStringSubmatch(diff)
	if m == nil {
		t.Fatalf("no last line counts the paths not listed: %.120q", diff[max(len(diff)-120, 0):])
	}
	if rest := 0; func() bool { fmt.Sscan(m[1], &rest); return headers+rest != n+1 }() {
		t.Errorf("%d headers and %s not listed, want %d in all", headers, m[1], n+1)
	}
}

// More untracked instruction paths than a snapshot may hold in memory are a
// refusal, and nothing is removed.
func TestReviewInstructionRemovalsAreBounded(t *testing.T) {
	if testing.Short() {
		t.Skip("writes 50,001 files")
	}
	const n = 50001
	r := newInstructionRepo(t, map[string]string{".claude/keep": "k\n"})
	r.write("main.go", "package main // changed\n")
	r.commit()
	for i := 0; i < n; i++ {
		if err := os.WriteFile(r.abs(fmt.Sprintf(".claude/u%05d", i)), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	snap, _, err := r.snapshot()
	if err == nil {
		t.Fatalf("the snapshot removed %d untracked instruction paths, want a refusal", len(snap.Removed))
	}
	if !strings.Contains(err.Error(), "more than 50000 untracked instruction paths") {
		t.Fatalf("err = %v", err)
	}
	if r.gone(".claude/u00000") || r.gone(fmt.Sprintf(".claude/u%05d", n-1)) {
		t.Fatal("a refused snapshot removed files")
	}
}

// naiveMatch is the table rule written the slow way: the number of leading
// components of the outermost entry covering parts, 0 for none. ASCII only.
func naiveMatch(parts []string) int {
	best := 0
	for i := range parts {
		for _, entry := range append(append(append([]string(nil), reviewInstructionBaseNames...), reviewInstructionDirs...), reviewInstructionFiles...) {
			want := strings.Split(entry, "/")
			if i+len(want) <= len(parts) && strings.EqualFold(strings.Join(parts[i:i+len(want)], "/"), entry) && (best == 0 || i+len(want) < best) {
				best = i + len(want)
			}
		}
	}
	return best
}

// Random pairs of commits through real git, in every mode a tree holds: a
// plan that is not refused has a candidate, in the table's spelling, over
// every instruction path the two commits hold differently. The seed is fixed
// and the digest of every outcome is logged, so two versions of the code can
// be shown to plan alike.
func TestReviewInstructionPlansAgreeWithANaiveMatcher(t *testing.T) {
	if testing.Short() {
		t.Skip("plans 600 pairs of commits")
	}
	const iterations = 600
	rng := rand.New(rand.NewSource(45))
	comps := []string{"a", "b", "pkg", ".github", ".GitHub", "mcp.json", "MCP.json", "instructions", "hooks", "agents", "skills", "copilot-instructions.md", ".claude", ".Claude", ".pi", ".codex", ".vscode", ".agents", ".mcp.json", "AGENTS.md", "agents.md", "CLAUDE.md", "GEMINI.md", "CLAUDE.local.md", "workflows", "x.md", "y"}
	modes := []string{"100644", "100644", "100644", "100644", "100644", "100644", "100644", "100644", "100644", "100644", "100644", "100755", "100755", "120000", "160000"}
	genPath := func() string {
		parts := make([]string, 1+rng.Intn(5))
		for i := range parts {
			parts[i] = comps[rng.Intn(len(comps))]
		}
		return strings.Join(parts, "/")
	}
	// Marks 1 and 2 are file bodies, 3 and 4 link texts (a file outside the
	// table, and an instruction file).
	const blobs = "blob\nmark :1\ndata 2\nx\n\nblob\nmark :2\ndata 2\ny\n\nblob\nmark :3\ndata 10\n../../y/zz\nblob\nmark :4\ndata 9\nAGENTS.md\n"
	line := func() string {
		mode := modes[rng.Intn(len(modes))]
		ref := fmt.Sprintf(":%d", 1+rng.Intn(2))
		switch mode {
		case "120000":
			ref = fmt.Sprintf(":%d", 3+rng.Intn(2))
		case "160000":
			ref = strings.Repeat("1", 40)
		}
		return fmt.Sprintf("M %s %s %s\n", mode, ref, genPath())
	}
	r := newInstructionRepo(t, nil)
	var in bytes.Buffer
	in.WriteString(blobs)
	for it := 0; it < iterations; it++ {
		fmt.Fprintf(&in, "commit refs/heads/fb%d\ncommitter t <t@example.com> 0 +0000\ndata 5\ntree\n\nM 100644 :1 keep\n", it)
		for i, n := 0, rng.Intn(8); i < n; i++ {
			in.WriteString(line())
		}
		fmt.Fprintf(&in, "\ncommit refs/heads/fr%d\ncommitter t <t@example.com> 0 +0000\ndata 5\ntree\n\nfrom refs/heads/fb%d\n", it, it)
		for i, n := 0, 1+rng.Intn(6); i < n; i++ {
			if rng.Intn(5) == 0 {
				fmt.Fprintf(&in, "D %s\n", genPath())
			} else {
				in.WriteString(line())
			}
		}
		in.WriteString("\n")
	}
	r.fastImport(&in)
	refs := map[string]string{}
	for _, rec := range strings.Split(r.git("for-each-ref", "--format=%(refname:short) %(objectname)", "refs/heads/f*"), "\n") {
		name, sha, _ := strings.Cut(rec, " ")
		refs[name] = sha
	}
	list := func(commit string) map[string]string {
		out := map[string]string{}
		for _, rec := range strings.Split(strings.TrimSuffix(r.gitRaw("ls-tree", "-r", "-z", "--full-tree", commit), "\x00"), "\x00") {
			meta, p, _ := strings.Cut(rec, "\t")
			out[p] = meta
		}
		return out
	}
	digest := sha256.New()
	planned, masks := 0, 0
	for it := 0; it < iterations; it++ {
		bc, rc := refs[fmt.Sprintf("fb%d", it)], refs[fmt.Sprintf("fr%d", it)]
		plan, cands, err := planSnapshot(context.Background(), r.dir, bc, rc)
		plan.closeBlobs()
		if err != nil {
			fmt.Fprintf(digest, "%d refused: %s\n", it, strings.ReplaceAll(err.Error(), r.dir, "<work>"))
			continue
		}
		planned++
		masks += len(cands)
		canon := map[string]bool{}
		for _, c := range cands {
			canon[c.canon] = true
			fmt.Fprintf(digest, "%d mask %s %d %d\n", it, c.canon, len(c.base), len(c.res))
		}
		bt, rt := list(bc), list(rc)
		var differing []string
		for p := range rt {
			if bt[p] != rt[p] {
				differing = append(differing, p)
			}
		}
		for p := range bt {
			if _, ok := rt[p]; !ok {
				differing = append(differing, p)
			}
		}
		sort.Strings(differing)
		for _, p := range differing {
			parts := strings.Split(p, "/")
			n := naiveMatch(parts)
			if n == 0 {
				continue
			}
			// The table's spelling: a plan that is not refused spells every
			// differing path as the table does.
			if want := strings.Join(parts[:n], "/"); !canon[want] {
				t.Errorf("pair %d: %q differs (base %q, result %q) and no candidate masks %q", it, p, bt[p], rt[p], want)
			}
		}
	}
	t.Logf("%d pairs: %d planned (%d masks), %d refused; digest of every outcome %x", iterations, planned, masks, iterations-planned, digest.Sum(nil)[:8])
	if planned < iterations/10 || masks == 0 {
		t.Fatalf("only %d of %d pairs planned, with %d masks: the generator no longer exercises the plan", planned, iterations, masks)
	}
}

// The snapshot's own deadline is a refusal that says the repository is too
// costly to compare, whatever the caller's deadline; the caller's own
// cancellation is not. A deadline of a millisecond is over before the first
// listing is read.
func TestReviewInstructionSnapshotHasItsOwnDeadline(t *testing.T) {
	r := newInstructionRepo(t, map[string]string{".claude/keep": "k\n", ".gitignore": "pkg/\n"})
	r.write("AGENTS.md", "rules\n")
	r.commit()
	r.write("pkg/AGENTS.md", "steer\n")
	dst := filepath.Join(filepath.Dir(r.dir), "snap")
	old := reviewInstructionTimeout
	reviewInstructionTimeout = time.Millisecond
	t.Cleanup(func() { reviewInstructionTimeout = old })

	start := time.Now()
	snap, err := SnapshotReviewInstructions(context.Background(), r.dir, r.base, r.result, dst)
	took := time.Since(start)
	t.Logf("a deadline of 1ms: returned after %v: %.200v", took.Round(time.Millisecond), err)
	if err == nil || !strings.Contains(err.Error(), "the repository is too costly to compare: the snapshot of its instruction paths did not finish in 1ms") {
		t.Fatalf("err = %v, want the refusal of a repository too costly to compare", err)
	}
	if took > 4*time.Second {
		t.Errorf("returned %v after a deadline of 1ms", took.Round(time.Millisecond))
	}
	if _, serr := os.Stat(dst); !os.IsNotExist(serr) {
		t.Errorf("the refused snapshot left %s behind (%v)", dst, serr)
	}
	if len(snap.Removed) != 0 || r.gone("pkg/AGENTS.md") {
		t.Errorf("the refused snapshot removed %v from the worktree", snap.Removed)
	}

	parent, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := SnapshotReviewInstructions(parent, r.dir, r.base, r.result, dst); err == nil || strings.Contains(err.Error(), "too costly") {
		t.Fatalf("a cancelled caller: err = %v, want an error that is not the refusal", err)
	}
}

// naiveTrackedChildren is trackedChildren as it was first written: every
// prefix of every entry, spelled out.
func naiveTrackedChildren(tracked []treeEntry) map[string][]string {
	seen := map[string]map[string]bool{}
	for _, e := range tracked {
		parts := strings.Split(e.path, "/")
		for k := range parts {
			dir := strings.Join(parts[:k], "/")
			if seen[dir] == nil {
				seen[dir] = map[string]bool{}
			}
			seen[dir][parts[k]] = true
		}
	}
	out := make(map[string][]string, len(seen))
	for dir, names := range seen {
		for n := range names {
			out[dir] = append(out[dir], n)
		}
		sort.Strings(out[dir])
	}
	return out
}

// The worktree pass builds its tables of tracked directories from each entry
// upwards, stopping at what an earlier entry recorded. They are the tables
// the plain way builds: every prefix of every entry.
func TestReviewInstructionTrackedTablesMatchThePlainWay(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	names := []string{"a", "b", "c", ".claude", ".github", "AGENTS.md", "x"}
	for round := 0; round < 300; round++ {
		root := t.TempDir()
		s := newPlan()
		seen := map[string]bool{}
		for i, n := 0, rng.Intn(12); i < n; i++ {
			parts := make([]string, 1+rng.Intn(6))
			for k := range parts {
				parts[k] = names[rng.Intn(len(names))]
			}
			p := strings.Join(parts, "/")
			if seen[p] {
				continue
			}
			seen[p] = true
			s.tracked = append(s.tracked, treeEntry{path: p, mode: "100644", oid: strings.Repeat("0", 40)})
			// The directories exist on disk for some entries only, and never
			// where an earlier entry made a file.
			if rng.Intn(3) > 0 {
				_ = os.MkdirAll(filepath.Join(root, filepath.FromSlash(strings.Join(parts[:len(parts)-1], "/"))), 0o755)
				_ = os.WriteFile(filepath.Join(root, filepath.FromSlash(p)), nil, 0o644)
			}
		}
		kids, err := s.trackedChildren(context.Background())
		if want := naiveTrackedChildren(s.tracked); err != nil || !reflect.DeepEqual(kids, want) {
			t.Fatalf("round %d: trackedChildren = %v (%v), want %v", round, kids, err, want)
		}
		d := &diskState{planState: s, root: root, dirsOK: map[string]bool{}}
		wantDirs, wantOK := map[int][]string{}, map[string]bool{}
		dirSeen := map[string]bool{}
		for _, e := range s.tracked {
			parts := strings.Split(e.path, "/")
			checking := true
			for k := 1; k < len(parts); k++ {
				dir := strings.Join(parts[:k], "/")
				info, err := os.Lstat(filepath.Join(root, filepath.FromSlash(dir)))
				if !dirSeen[dir] {
					dirSeen[dir] = true
					if err == nil {
						wantDirs[k] = append(wantDirs[k], dir)
					}
				}
				if checking = checking && err == nil && info.IsDir(); checking {
					wantOK[dir] = true
				}
			}
			_ = d.verify(context.Background(), e)
		}
		gotDirs := map[int][]string{}
		for k, list := range d.trackedDirs() {
			for _, kf := range list {
				gotDirs[k] = append(gotDirs[k], kf.name)
			}
		}
		if !reflect.DeepEqual(gotDirs, wantDirs) {
			t.Fatalf("round %d: trackedDirs = %v, want %v (entries %v)", round, gotDirs, wantDirs, s.tracked)
		}
		if !reflect.DeepEqual(d.dirsOK, wantOK) {
			t.Fatalf("round %d: checked directories = %v, want %v (entries %v)", round, d.dirsOK, wantOK, s.tracked)
		}
	}
}
