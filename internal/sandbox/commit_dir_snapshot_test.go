package sandbox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// tf is one entry of a hand-built tree: a blob with a body, or a gitlink or
// other entry with an object id.
type tf struct {
	path, mode, body, oid string
}

// gitIn runs git in the repository with stdin and returns trimmed stdout.
func (r *instructionRepo) gitIn(stdin []byte, args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", append([]string{"-C", r.dir, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Stdin = bytes.NewReader(stdin)
	out, err := cmd.Output()
	if err != nil {
		r.t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

// treeOf writes the tree objects for ents with git plumbing, which accepts
// names the index and the filesystem would not (.GIT, a backslash, bytes that
// are not UTF-8).
func (r *instructionRepo) treeOf(ents []tf, blobs map[string]string) string {
	r.t.Helper()
	groups := map[string][]tf{}
	var order []string
	for _, e := range ents {
		first, rest, nested := strings.Cut(e.path, "/")
		if _, ok := groups[first]; !ok {
			order = append(order, first)
		}
		if nested {
			e.path = rest
			groups[first] = append(groups[first], e)
		} else {
			groups[first] = append(groups[first], tf{path: "", mode: e.mode, body: e.body, oid: e.oid})
		}
	}
	var in bytes.Buffer
	for _, name := range order {
		g := groups[name]
		if len(g) == 1 && g[0].path == "" {
			e := g[0]
			oid, typ := e.oid, "blob"
			if e.mode == "160000" {
				typ = "commit"
			}
			if oid == "" {
				if oid = blobs[e.body]; oid == "" {
					oid = r.gitIn([]byte(e.body), "hash-object", "-w", "--stdin")
					blobs[e.body] = oid
				}
			}
			fmt.Fprintf(&in, "%s %s %s\t%s\x00", e.mode, typ, oid, name)
			continue
		}
		fmt.Fprintf(&in, "040000 tree %s\t%s\x00", r.treeOf(g, blobs), name)
	}
	return r.gitIn(in.Bytes(), "mktree", "-z", "--missing")
}

// commitOf commits a root tree built from ents.
func (r *instructionRepo) commitOf(ents ...tf) string {
	r.t.Helper()
	tree := r.treeOf(ents, map[string]string{})
	return r.git("commit-tree", tree, "-m", "snap")
}

func file(path, body string) tf    { return tf{path: path, mode: "100644", body: body} }
func exeFile(path, body string) tf { return tf{path: path, mode: "100755", body: body} }

var snapCounter int

// takeSnapshot runs SnapshotCommitDir into a fresh destination beside the repo.
func (r *instructionRepo) takeSnapshot(commit, name string) (CommitDirSnapshot, string, error) {
	r.t.Helper()
	snapCounter++
	dst := filepath.Join(filepath.Dir(r.dir), fmt.Sprintf("cds-%d", snapCounter))
	r.t.Cleanup(func() { unlockTree(dst) })
	snap, err := SnapshotCommitDir(context.Background(), r.dir, commit, name, dst)
	return snap, dst, err
}

// unlockTree makes a snapshot removable again.
func unlockTree(root string) {
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(p, 0o755)
		}
		return nil
	})
}

func newBareRepo(t *testing.T) *instructionRepo {
	t.Helper()
	return newInstructionRepo(t, nil)
}

// mustRefuse proves a refusal: wrapped sentinel, a message with every want,
// no snapshot and nothing left under dst.
func (r *instructionRepo) mustRefuse(commit, name string, want ...string) {
	r.t.Helper()
	snap, dst, err := r.takeSnapshot(commit, name)
	if err == nil {
		r.t.Fatalf("snapshot succeeded: %+v", snap)
	}
	if !errors.Is(err, ErrCommitDirSnapshot) {
		r.t.Fatalf("error does not wrap ErrCommitDirSnapshot: %v", err)
	}
	for _, w := range want {
		if !strings.Contains(err.Error(), w) {
			r.t.Fatalf("error %q lacks %q", err, w)
		}
	}
	if snap.Mask != nil || snap.SHA256 != "" || snap.Files != 0 {
		r.t.Fatalf("refusal returned a snapshot: %+v", snap)
	}
	if _, serr := os.Lstat(dst); !errors.Is(serr, fs.ErrNotExist) {
		r.t.Fatalf("destination %s left behind (%v)", dst, serr)
	}
}

func (r *instructionRepo) mustSnapshot(commit string) (CommitDirSnapshot, string) {
	r.t.Helper()
	snap, dst, err := r.takeSnapshot(commit, ".factory")
	if err != nil {
		r.t.Fatal(err)
	}
	return snap, dst
}

func TestCommitDirSnapshotWritesFilesWithModes(t *testing.T) {
	r := newBareRepo(t)
	c := r.commitOf(
		file("src/main.go", "package main\n"),
		file(".factory/lint.txt", "plain\n"),
		exeFile(".factory/lint.sh", "#!/bin/sh\nexit 0\n"),
		file(".factory/a/b/c/deep.txt", "deep\n"),
		exeFile(".factory/a/run", "run\n"),
		file(".factory/empty", ""),
	)
	snap, dst := r.mustSnapshot(c)
	if snap.Absent || snap.Files != 5 || snap.SHA256 == "" {
		t.Fatalf("snapshot = %+v", snap)
	}
	if want := r.git("rev-parse", c+":.factory"); snap.TreeOID != want {
		t.Fatalf("TreeOID = %s, want %s", snap.TreeOID, want)
	}
	root := filepath.Join(dst, ".factory")
	if snap.Mask == nil || snap.Mask.Source != root || snap.Mask.Target != ".factory" || !snap.Mask.Dir || snap.Mask.AbsentInWorktree {
		t.Fatalf("mask = %+v", snap.Mask)
	}
	if err := validateWorkspaceMask(*snap.Mask, ""); err != nil {
		t.Fatal(err)
	}
	if err := validateWorkspaceMasks([]WorkspaceMask{*snap.Mask}, ""); err != nil {
		t.Fatal(err)
	}
	wantFiles := map[string]struct {
		body string
		mode os.FileMode
	}{
		"lint.txt":       {"plain\n", 0o444},
		"lint.sh":        {"#!/bin/sh\nexit 0\n", 0o555},
		"a/b/c/deep.txt": {"deep\n", 0o444},
		"a/run":          {"run\n", 0o555},
		"empty":          {"", 0o444},
	}
	got := filesUnder(t, root)
	if len(got) != len(wantFiles) {
		t.Fatalf("files = %v", got)
	}
	for rel, w := range wantFiles {
		p := filepath.Join(root, filepath.FromSlash(rel))
		info, err := os.Lstat(p)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode() != w.mode {
			t.Errorf("%s mode = %v, want %v", rel, info.Mode(), w.mode)
		}
		if readFile(t, p) != w.body {
			t.Errorf("%s body = %q, want %q", rel, readFile(t, p), w.body)
		}
	}
	for _, d := range []string{"", "a", "a/b", "a/b/c"} {
		info, err := os.Lstat(filepath.Join(root, filepath.FromSlash(d)))
		if err != nil || !info.IsDir() || info.Mode().Perm() != 0o555 {
			t.Errorf("dir %q = %v, %v; want 0555 directory", d, info, err)
		}
	}
	// Nothing but the directory is written under dst.
	if entries, _ := os.ReadDir(dst); len(entries) != 1 || entries[0].Name() != ".factory" {
		t.Fatalf("dst entries = %v", entries)
	}
}

func TestCommitDirSnapshotAbsent(t *testing.T) {
	r := newBareRepo(t)
	c := r.commitOf(file("src/main.go", "x"), file("factory/a", "x"), file(".factoryx/a", "x"))
	snap, dst, err := r.takeSnapshot(c, ".factory")
	if err != nil {
		t.Fatal(err)
	}
	if !snap.Absent || snap.Mask != nil || snap.SHA256 != "" || snap.Files != 0 {
		t.Fatalf("snapshot = %+v", snap)
	}
	if _, err := os.Lstat(dst); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("absent wrote %s", dst)
	}
}

func TestCommitDirSnapshotRefusesOtherSpellingsOfTheName(t *testing.T) {
	r := newBareRepo(t)
	for _, spelling := range []string{".Factory", ".FACTORY", ".factorY", "．factory", "．ｆactory", ".factorý"[:0] + ".FaCtOrY"} {
		c := r.commitOf(file(spelling+"/x.sh", "x"))
		r.mustRefuse(c, ".factory", "different spelling", spelling)
	}
}

func TestCommitDirSnapshotRefusesTwoEntriesThatFoldToTheName(t *testing.T) {
	r := newBareRepo(t)
	c := r.commitOf(file(".factory/a.sh", "good"), file(".Factory/a.sh", "evil"))
	r.mustRefuse(c, ".factory", "2 root entries fold to")
	c = r.commitOf(file(".FACTORY/a", "1"), file(".Factory/a", "2"))
	r.mustRefuse(c, ".factory", "root entries fold to")
}

func TestCommitDirSnapshotRefusesAnEntryThatIsNotATree(t *testing.T) {
	sub := strings.Repeat("a", 40)
	for _, tc := range []struct {
		name string
		e    tf
		want string
	}{
		{"blob", file(".factory", "not a dir"), "not a directory"},
		{"executable blob", exeFile(".factory", "x"), "not a directory"},
		{"symlink", tf{path: ".factory", mode: "120000", body: "src"}, "not a directory"},
		{"gitlink", tf{path: ".factory", mode: "160000", oid: sub}, "not a directory"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rr := newBareRepo(t)
			c := rr.commitOf(tc.e, file("src/a", "x"))
			rr.mustRefuse(c, ".factory", `".factory"`, tc.want)
		})
	}
}

func TestCommitDirSnapshotRefusesSymlinksAndGitlinksBelow(t *testing.T) {
	sub := strings.Repeat("b", 40)
	for _, tc := range []struct {
		name string
		ents []tf
		path string
		want string
	}{
		{"symlink", []tf{file(".factory/ok", "x"), {path: ".factory/x", mode: "120000", body: "../src/x"}}, ".factory/x", "symlink"},
		{"deep symlink", []tf{{path: ".factory/a/b/c", mode: "120000", body: "/etc/passwd"}}, "a/b/c", "symlink"},
		{"symlink to a directory", []tf{{path: ".factory/d", mode: "120000", body: "."}, file(".factory/e", "x")}, "d", "symlink"},
		{"gitlink", []tf{file(".factory/ok", "x"), {path: ".factory/sub", mode: "160000", oid: sub}}, "sub", "gitlink"},
		{"deep gitlink", []tf{{path: ".factory/a/b/sub", mode: "160000", oid: sub}}, "a/b/sub", "gitlink"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newBareRepo(t)
			r.mustRefuse(r.commitOf(tc.ents...), ".factory", tc.path, tc.want)
		})
	}
}

func TestCommitDirSnapshotRefusesSubmoduleAddedToTheIndex(t *testing.T) {
	r := newBareRepo(t)
	r.stage("100644", ".factory/ok.sh", "ok\n")
	r.git("update-index", "--add", "--cacheinfo", "160000,"+strings.Repeat("c", 40)+",.factory/sub")
	r.commitStaged()
	r.mustRefuse(r.result, ".factory", ".factory/sub", "gitlink")
}

func TestCommitDirSnapshotRefusesSymlinkAddedToTheIndex(t *testing.T) {
	r := newBareRepo(t)
	r.stage("100644", "src/x", "x")
	r.stage("120000", ".factory/x", "../src/x")
	r.commitStaged()
	r.mustRefuse(r.result, ".factory", ".factory/x", "symlink")
}

func TestCommitDirSnapshotRefusesAComponentThatFoldsToDotGit(t *testing.T) {
	for _, name := range []string{".git", ".GIT", ".Git", ".gIt", "．git", ".git​"[:4]} {
		for _, tc := range []struct{ label, path string }{
			{"file", ".factory/" + name},
			{"directory", ".factory/" + name + "/config"},
			{"nested", ".factory/a/b/" + name + "/hooks/pre-commit"},
		} {
			t.Run(tc.label+"/"+name, func(t *testing.T) {
				r := newBareRepo(t)
				c := r.commitOf(file(".factory/ok", "x"), file(tc.path, "x"))
				r.mustRefuse(c, ".factory", "folds to .git")
			})
		}
	}
}

func TestCommitDirSnapshotRefusesSiblingsThatFoldAlike(t *testing.T) {
	for _, tc := range []struct {
		name string
		a, b string
	}{
		{"case", ".factory/Run.sh", ".factory/run.sh"},
		{"case in a directory", ".factory/d/A", ".factory/d/a"},
		{"directories", ".factory/Tools/x", ".factory/tools/y"},
		{"file and directory", ".factory/lib", ".factory/LIB/x"},
		{"composed and decomposed", ".factory/é.sh", ".factory/é.sh"},
		{"sharp s", ".factory/straße", ".factory/STRASSE"},
		{"fullwidth", ".factory/ａ", ".factory/a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newBareRepo(t)
			r.mustRefuse(r.commitOf(file(tc.a, "1"), file(tc.b, "2")), ".factory", "fold to the same name")
		})
	}
}

func TestCommitDirSnapshotAllowsSameNameInDifferentDirectories(t *testing.T) {
	r := newBareRepo(t)
	c := r.commitOf(file(".factory/a/run", "1"), file(".factory/b/run", "2"), file(".factory/a/Run2", "3"))
	if snap, _ := r.mustSnapshot(c); snap.Files != 3 {
		t.Fatalf("snapshot = %+v", snap)
	}
}

func TestCommitDirSnapshotRefusesUnsafeNames(t *testing.T) {
	for _, tc := range []struct {
		name, path string
	}{
		{"newline", ".factory/a\nb"},
		{"backslash", ".factory/a\\b"},
		{"backslash directory", ".factory/a\\/x"},
		{"invalid utf-8", ".factory/a\xffb"},
		{"overlong utf-8", ".factory/a\xc0\xafb"},
		{"invalid utf-8 directory", ".factory/\xfe/x"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newBareRepo(t)
			c := r.commitOf(file(tc.path, "x"))
			r.mustRefuse(c, ".factory", "NUL, newline, backslash or invalid UTF-8")
		})
	}
}

func TestCommitDirSnapshotLimits(t *testing.T) {
	// The limits are what is tested here, not the deadline: 2000 files take
	// two git processes each, which under the race detector on a busy
	// machine has passed the snapshot's own 60 seconds.
	oldTimeout := commitDirTimeout
	commitDirTimeout = 10 * time.Minute
	t.Cleanup(func() { commitDirTimeout = oldTimeout })
	big := func(n int, fill byte) string { return strings.Repeat(string(fill), n) }
	t.Run("2001 files", func(t *testing.T) {
		r := newBareRepo(t)
		var ents []tf
		for i := 0; i <= maxCommitDirFiles; i++ {
			ents = append(ents, file(fmt.Sprintf(".factory/f%04d", i), "x"))
		}
		r.mustRefuse(r.commitOf(ents...), ".factory", "more than 2000 files")
	})
	t.Run("2000 files", func(t *testing.T) {
		r := newBareRepo(t)
		var ents []tf
		for i := 0; i < maxCommitDirFiles; i++ {
			ents = append(ents, file(fmt.Sprintf(".factory/f%04d", i), "x"))
		}
		if snap, _ := r.mustSnapshot(r.commitOf(ents...)); snap.Files != 2000 {
			t.Fatalf("files = %d", snap.Files)
		}
	})
	t.Run("blob over 4 MiB", func(t *testing.T) {
		r := newBareRepo(t)
		r.mustRefuse(r.commitOf(file(".factory/big", big(4<<20+1, 'a'))), ".factory", ".factory/big", "over 4194304")
	})
	t.Run("blob of exactly 4 MiB", func(t *testing.T) {
		r := newBareRepo(t)
		c := r.commitOf(file(".factory/big", big(4<<20, 'a')))
		_, dst := r.mustSnapshot(c)
		if info, err := os.Stat(filepath.Join(dst, ".factory", "big")); err != nil || info.Size() != 4<<20 {
			t.Fatalf("stat = %v, %v", info, err)
		}
	})
	t.Run("total over 16 MiB", func(t *testing.T) {
		r := newBareRepo(t)
		var ents []tf
		for i := 0; i < 4; i++ {
			ents = append(ents, file(fmt.Sprintf(".factory/b%d", i), big(4<<20, byte('a'+i))))
		}
		ents = append(ents, file(".factory/one", "x"))
		r.mustRefuse(r.commitOf(ents...), ".factory", "bytes in total")
	})
	t.Run("total of exactly 16 MiB", func(t *testing.T) {
		r := newBareRepo(t)
		var ents []tf
		for i := 0; i < 4; i++ {
			ents = append(ents, file(fmt.Sprintf(".factory/b%d", i), big(4<<20, byte('a'+i))))
		}
		if snap, _ := r.mustSnapshot(r.commitOf(ents...)); snap.Files != 4 {
			t.Fatalf("files = %d", snap.Files)
		}
	})
}

func TestCommitDirSnapshotRefusesABadName(t *testing.T) {
	r := newBareRepo(t)
	c := r.commitOf(file(".factory/a", "x"), file("src/.factory/a", "x"))
	for _, name := range []string{"", ".", "..", ".factory/", "a/b", "src/.factory", "/.factory", ".factory/a", "a\\b", ".git", ".GIT", "．git", "a\nb", "a\x00b", "a\xffb", "a:b"} {
		t.Run(fmt.Sprintf("%q", name), func(t *testing.T) {
			snap, dst, err := r.takeSnapshot(c, name)
			if err == nil || !errors.Is(err, ErrCommitDirSnapshot) {
				t.Fatalf("err = %v, snap = %+v", err, snap)
			}
			if snap.Mask != nil || snap.Absent {
				t.Fatalf("snap = %+v", snap)
			}
			if _, serr := os.Lstat(dst); !errors.Is(serr, fs.ErrNotExist) {
				t.Fatalf("%s left behind", dst)
			}
		})
	}
}

func TestCommitDirSnapshotTakesAnyOneComponentName(t *testing.T) {
	r := newBareRepo(t)
	c := r.commitOf(file("scripts/a", "1"), file("scripts/b", "2"))
	snap, dst, err := r.takeSnapshot(c, "scripts")
	if err != nil || snap.Files != 2 || snap.Mask.Target != "scripts" || snap.Mask.Source != filepath.Join(dst, "scripts") {
		t.Fatalf("snap = %+v, err = %v", snap, err)
	}
}

func TestCommitDirSnapshotOnlyAFullCommitIdIsTaken(t *testing.T) {
	r := newBareRepo(t)
	c := r.commitOf(file(".factory/a.sh", "x"))
	r.git("branch", "feature", c)
	r.git("tag", "light", c)
	r.git("tag", "-a", "-m", "annotated", "annot", c)
	tagObj := r.git("rev-parse", "annot")
	treeOID := r.git("rev-parse", c+"^{tree}")
	for _, ref := range []string{"feature", "light", "annot", "HEAD", "refs/heads/feature", c[:7], c[:12], c[:39], strings.ToUpper(c), "--help", "-h", "--all", c + "^{commit}", c + "~0", c + " ", " " + c, "", c + c[:10]} {
		t.Run(fmt.Sprintf("%q", ref), func(t *testing.T) {
			r.mustRefuse(ref, ".factory", "not a full object id")
		})
	}
	t.Run("a tag object id", func(t *testing.T) {
		r.mustRefuse(tagObj, ".factory", "is a tag, not a commit")
	})
	t.Run("a tree id", func(t *testing.T) {
		r.mustRefuse(treeOID, ".factory", "is a tree, not a commit")
	})
	t.Run("an unknown id", func(t *testing.T) {
		r.mustRefuse(strings.Repeat("0", 40), ".factory", "commit "+strings.Repeat("0", 40))
	})
	if snap, _ := r.mustSnapshot(c); snap.Files != 1 {
		t.Fatalf("snapshot = %+v", snap)
	}
}

func TestCommitDirSnapshotSha256Repository(t *testing.T) {
	r := newBareRepo(t)
	dir := filepath.Join(filepath.Dir(r.dir), "work256")
	if err := os.Mkdir(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	r256 := &instructionRepo{t: t, dir: dir}
	if out, err := exec.Command("git", "-C", dir, "init", "-q", "--object-format=sha256").CombinedOutput(); err != nil {
		t.Skipf("git cannot init a sha256 repository: %v: %s", err, out)
	}
	c := r256.commitOf(exeFile(".factory/a.sh", "run\n"), file(".factory/d/b", "b\n"))
	if len(c) != 64 {
		t.Fatalf("commit id %s is not 64 hex", c)
	}
	snap, dst := r256.mustSnapshot(c)
	if snap.Files != 2 || len(snap.TreeOID) != 64 || readFile(t, filepath.Join(dst, ".factory", "a.sh")) != "run\n" {
		t.Fatalf("snapshot = %+v", snap)
	}
	r256.mustRefuse(c[:40], ".factory", "abbreviation")
}

func TestCommitDirSnapshotHashIsStableAndSensitive(t *testing.T) {
	r := newBareRepo(t)
	base := []tf{file(".factory/a.sh", "one\n"), file(".factory/d/b", "two\n"), file("src/x", "x")}
	hashOf := func(ents ...tf) string {
		t.Helper()
		snap, _ := r.mustSnapshot(r.commitOf(ents...))
		return snap.SHA256
	}
	h := hashOf(base...)
	if !hexLen(h, 64) {
		t.Fatalf("hash = %q", h)
	}
	c := r.commitOf(base...)
	for i := 0; i < 3; i++ {
		if snap, _ := r.mustSnapshot(c); snap.SHA256 != h {
			t.Fatalf("call %d hash = %s, want %s", i, snap.SHA256, h)
		}
	}
	if got := hashOf(append([]tf{file("other/y", "y")}, base...)...); got != h {
		t.Fatalf("a change outside .factory moved the hash")
	}
	seen := map[string]string{h: "base"}
	variants := map[string][]tf{
		"one byte":          {file(".factory/a.sh", "one!\n"), file(".factory/d/b", "two\n")},
		"one byte of case":  {file(".factory/a.sh", "One\n"), file(".factory/d/b", "two\n")},
		"a name":            {file(".factory/a.sh", "one\n"), file(".factory/d/c", "two\n")},
		"a directory name":  {file(".factory/a.sh", "one\n"), file(".factory/e/b", "two\n")},
		"an executable bit": {exeFile(".factory/a.sh", "one\n"), file(".factory/d/b", "two\n")},
		"a nested exec bit": {file(".factory/a.sh", "one\n"), exeFile(".factory/d/b", "two\n")},
		"an extra file":     {file(".factory/a.sh", "one\n"), file(".factory/d/b", "two\n"), file(".factory/z", "")},
		"a moved file":      {file(".factory/a.sh", "one\n"), file(".factory/b", "two\n")},
		"swapped contents":  {file(".factory/a.sh", "two\n"), file(".factory/d/b", "one\n")},
	}
	names := make([]string, 0, len(variants))
	for n := range variants {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		got := hashOf(variants[n]...)
		if prev, dup := seen[got]; dup {
			t.Errorf("%s hashes like %s", n, prev)
		}
		seen[got] = n
	}
	// The directory name is part of what is hashed (the mask target).
	a, _, err := r.takeSnapshot(c, ".factory")
	b := r.commitOf(file("tools/a.sh", "one\n"), file("tools/d/b", "two\n"))
	b2, _, err2 := r.takeSnapshot(b, "tools")
	if err != nil || err2 != nil || a.SHA256 == b2.SHA256 {
		t.Fatalf("same content under two names hashes alike: %v %v", err, err2)
	}
}

func hexLen(s string, n int) bool {
	if len(s) != n {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

func TestCommitDirSnapshotHashMatchesHandComputedStream(t *testing.T) {
	r := newBareRepo(t)
	snap, _ := r.mustSnapshot(r.commitOf(file(".factory/a", "A"), exeFile(".factory/b", "B")))
	h := sha256.New()
	put := func(kind byte, path string, body []byte) {
		h.Write([]byte{kind})
		h.Write([]byte{0, 0, 0, 0, 0, 0, 0, byte(len(path))})
		h.Write([]byte(path))
		h.Write([]byte{0, 0, 0, 0, 0, 0, 0, byte(len(body))})
		h.Write(body)
	}
	put('f', "a", []byte("A"))
	put('x', "b", []byte("B"))
	put('m', ".factory", []byte{1, 0})
	if want := hex.EncodeToString(h.Sum(nil)); snap.SHA256 != want {
		t.Fatalf("hash = %s, want %s", snap.SHA256, want)
	}
}

func TestCommitDirSnapshotIgnoresAttributesAndFilters(t *testing.T) {
	r := newBareRepo(t)
	marker := filepath.Join(filepath.Dir(r.dir), "filter-ran")
	r.git("config", "filter.evil.clean", "sh -c 'touch "+marker+"; cat'")
	r.git("config", "filter.evil.smudge", "sh -c 'touch "+marker+"; cat'")
	r.git("config", "filter.evil.required", "true")
	r.git("config", "core.autocrlf", "true")
	r.git("config", "core.attributesFile", filepath.Join(filepath.Dir(r.dir), "global-attrs"))
	if err := os.WriteFile(filepath.Join(filepath.Dir(r.dir), "global-attrs"), []byte(".factory/** filter=evil\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	attrs := ".factory/* export-ignore ident filter=evil working-tree-encoding=UTF-16 text eol=crlf\n.factory/** export-ignore ident filter=evil\n"
	body := "line1\nline2\n$Id$\n$Id: abc $\n"
	c := r.commitOf(file(".gitattributes", attrs), file(".factory/a.sh", body), file(".factory/crlf.txt", "a\r\nb\n"), exeFile(".factory/b.sh", "#!/bin/sh\n"))
	// The worktree carries the same attributes, as a checkout would.
	r.write(".gitattributes", attrs)
	info := filepath.Join(r.dir, ".git", "info")
	if err := os.MkdirAll(info, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(info, "attributes"), []byte(attrs), 0o600); err != nil {
		t.Fatal(err)
	}
	_, dst := r.mustSnapshot(c)
	for rel, want := range map[string]string{"a.sh": body, "crlf.txt": "a\r\nb\n", "b.sh": "#!/bin/sh\n"} {
		got := readFile(t, filepath.Join(dst, ".factory", rel))
		if got != want {
			t.Errorf("%s = %q, want %q", rel, got, want)
		}
		if blob := r.git("cat-file", "blob", c+":.factory/"+rel); blob != strings.TrimSpace(want) {
			t.Errorf("%s: test expectation differs from git cat-file: %q", rel, blob)
		}
	}
	if _, err := os.Lstat(marker); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a configured filter ran (%v)", err)
	}
}

// worktreeState lists every path under the worktree (not .git) with its mode
// and content hash, and the index file's hash.
func worktreeState(t *testing.T, r *instructionRepo) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(r.dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(r.dir, p)
		if rel == ".git" {
			return filepath.SkipDir
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		line := fmt.Sprintf("%s %v", rel, info.Mode())
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			l, _ := os.Readlink(p)
			line += " -> " + l
		case info.Mode().IsRegular():
			b, _ := os.ReadFile(p)
			s := sha256.Sum256(b)
			line += fmt.Sprintf(" %x %s", s, info.ModTime().UTC())
		}
		out = append(out, line)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	idx, _ := os.ReadFile(filepath.Join(r.dir, ".git", "index"))
	s := sha256.Sum256(idx)
	out = append(out, fmt.Sprintf("index %x", s))
	out = append(out, "status:"+r.git("status", "--porcelain", "--ignored"))
	return out
}

func TestCommitDirSnapshotLeavesTheWorktreeUntouched(t *testing.T) {
	r := newBareRepo(t)
	r.stage("100644", ".factory/a.sh", "committed a\n")
	r.stage("100755", ".factory/b.sh", "committed b\n")
	r.commitStaged()
	c := r.result
	// The worktree differs from the commit: an edited file, an added file, a
	// symlink, a deleted file, and a directory the commit does not have.
	r.write(".factory/a.sh", "worktree a\n")
	r.link(".factory/evil", "../../etc/passwd")
	r.write(".factory/new/x", "x")
	r.remove(".factory/b.sh")
	r.write("untracked.txt", "u")
	before := worktreeState(t, r)
	snap, dst := r.mustSnapshot(c)
	after := worktreeState(t, r)
	if strings.Join(before, "\n") != strings.Join(after, "\n") {
		t.Fatalf("worktree changed:\nbefore: %v\nafter:  %v", before, after)
	}
	if got := readFile(t, filepath.Join(dst, ".factory", "a.sh")); got != "committed a\n" {
		t.Fatalf("a.sh = %q", got)
	}
	if got := readFile(t, filepath.Join(dst, ".factory", "b.sh")); got != "committed b\n" || snap.Files != 2 {
		t.Fatalf("b.sh = %q, files = %d", got, snap.Files)
	}
	// A refusal leaves it untouched too.
	r.stage("120000", ".factory/link", "x")
	r.commitStaged()
	before = worktreeState(t, r)
	r.mustRefuse(r.result, ".factory", "symlink")
	if after = worktreeState(t, r); strings.Join(before, "\n") != strings.Join(after, "\n") {
		t.Fatalf("worktree changed by a refusal")
	}
}

func TestCommitDirSnapshotRefusesADestinationInsideTheRepository(t *testing.T) {
	r := newBareRepo(t)
	c := r.commitOf(file(".factory/a", "x"))
	for _, dst := range []string{filepath.Join(r.dir, "out"), r.dir, filepath.Join(r.dir, ".git", "x"), filepath.Dir(r.dir), "relative/out"} {
		_, err := SnapshotCommitDir(context.Background(), r.dir, c, ".factory", dst)
		if err == nil || !errors.Is(err, ErrCommitDirSnapshot) {
			t.Errorf("dst %s: err = %v", dst, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(r.dir, "out")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a destination inside the repository was written")
	}
}

func TestCommitDirSnapshotDoesNotTouchAnExistingDestination(t *testing.T) {
	r := newBareRepo(t)
	good := r.commitOf(file(".factory/a", "x"))
	bad := r.commitOf(file(".factory/a", "x"), tf{path: ".factory/l", mode: "120000", body: "a"})
	dst := filepath.Join(filepath.Dir(r.dir), "existing")
	if err := os.MkdirAll(filepath.Join(dst, ".factory"), 0o755); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(dst, "keep.txt")
	if err := os.WriteFile(keep, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { unlockTree(dst) })
	// A snapshot already there is not overwritten.
	if _, err := SnapshotCommitDir(context.Background(), r.dir, good, ".factory", dst); !errors.Is(err, ErrCommitDirSnapshot) {
		t.Fatalf("err = %v", err)
	}
	if err := os.Remove(filepath.Join(dst, ".factory")); err != nil {
		t.Fatal(err)
	}
	// A refusal removes only what it wrote.
	if _, err := SnapshotCommitDir(context.Background(), r.dir, bad, ".factory", dst); !errors.Is(err, ErrCommitDirSnapshot) {
		t.Fatalf("err = %v", err)
	}
	if readFile(t, keep) != "keep" {
		t.Fatal("pre-existing file removed")
	}
	if _, err := os.Lstat(filepath.Join(dst, ".factory")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("partial snapshot left (%v)", err)
	}
}

func TestCommitDirSnapshotCleansUpAfterAFailureMidWrite(t *testing.T) {
	r := newBareRepo(t)
	// The listing is clean, so writing starts; a blob the repository no
	// longer has fails the read after earlier files were written.
	c := r.commitOf(file(".factory/a", "x"), file(".factory/b", "y"), file(".factory/d/c", "z"))
	oid := r.git("rev-parse", c+":.factory/d/c")
	obj := filepath.Join(r.dir, ".git", "objects", oid[:2], oid[2:])
	if err := os.Chmod(obj, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(obj, []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.mustRefuse(c, ".factory")
}
