package sandbox

import (
	"bytes"
	"compress/zlib"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// countGitProcesses puts a git on PATH that records each start and then runs
// the real one, and returns how many were started since the last call.
func countGitProcesses(t *testing.T) func() int {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	log := filepath.Join(dir, "starts")
	script := fmt.Sprintf("#!/bin/sh\necho start >> '%s'\nexec '%s' \"$@\"\n", log, realGit)
	if strings.Contains(log+realGit, "'") {
		t.Skipf("a path with a quote cannot be written into the counting script: %s %s", log, realGit)
	}
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return func() int {
		t.Helper()
		body, err := os.ReadFile(log)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		if err := os.RemoveAll(log); err != nil {
			t.Fatal(err)
		}
		return bytes.Count(body, []byte("\n"))
	}
}

// The number of git processes a snapshot starts does not grow with the
// number of files: every blob is read through one process.
func TestCommitDirSnapshotStartsAFixedNumberOfGitProcesses(t *testing.T) {
	r := newBareRepo(t)
	commitWith := func(n int) string {
		var ents []tf
		for i := 0; i < n; i++ {
			ents = append(ents, file(fmt.Sprintf(".factory/d%d/f%03d", i%3, i), fmt.Sprintf("body %d\n", i)))
		}
		return r.commitOf(ents...)
	}
	few, many := commitWith(2), commitWith(60)
	started := countGitProcesses(t)
	if snap, _ := r.mustSnapshot(few); snap.Files != 2 {
		t.Fatalf("files = %d", snap.Files)
	}
	forFew := started()
	if snap, _ := r.mustSnapshot(many); snap.Files != 60 {
		t.Fatalf("files = %d", snap.Files)
	}
	forMany := started()
	if forFew == 0 {
		t.Fatal("no git process was counted: the counting git is not the one on PATH")
	}
	if forMany != forFew {
		t.Fatalf("a snapshot of 60 files started %d git processes, one of 2 files %d", forMany, forFew)
	}
	if forFew > 5 {
		t.Fatalf("a snapshot started %d git processes, want at most 5", forFew)
	}
}

// variedCommitDir is a tree of every kind of entry a snapshot takes.
func variedCommitDir() []tf {
	binary := string([]byte{0x00, 0x01, 0xff, 0xfe, '\n', 0x00, '\r', '\n', 0x80})
	large := strings.Repeat("0123456789abcdef", 200<<10/16) + "tail" // over one 64 KiB read
	return []tf{
		file(".factory/README.md", "# gates\n"),
		exeFile(".factory/lint.sh", "#!/bin/sh\nexit 0\n"),
		file(".factory/empty", ""),
		file(".factory/no-newline", "no newline at the end"),
		file(".factory/crlf.txt", "one\r\ntwo\r\n"),
		file(".factory/binary.bin", binary),
		file(".factory/large.txt", large),
		file(".factory/a/b/c/deep.txt", "deep\n"),
		exeFile(".factory/a/b/run", "#!/bin/sh\necho run\n"),
		file(".factory/a/same-1", "the same bytes\n"),
		file(".factory/z/same-2", "the same bytes\n"),
		file(".factory/z/with space é.txt", "named\n"),
		file(".factory/z/.hidden", "hidden\n"),
		{path: ".factory/z/mode664", mode: "100664", body: "group writable\n"},
		file("src/outside.go", "package outside\n"),
	}
}

// The snapshot of a varied tree is the one recorded here: the same tree id,
// content hash, file count, bytes and modes.
func TestCommitDirSnapshotOfAVariedTreeIsTheRecordedOne(t *testing.T) {
	const (
		wantTree = "53b7190f6507f6cc5312ddf3887fc20357e43fd2"
		wantSum  = "d2d0fbd1c49f7a365aee35cc32ee750092ffc1d8b2280aaa767e22b703bff396"
	)
	r := newBareRepo(t)
	ents := variedCommitDir()
	snap, dst := r.mustSnapshot(r.commitOf(ents...))
	if snap.TreeOID != wantTree || snap.SHA256 != wantSum || snap.Files != len(ents)-1 || snap.Absent {
		t.Fatalf("snapshot = tree %s, sha256 %s, %d files, absent %v; want tree %s, sha256 %s, %d files", snap.TreeOID, snap.SHA256, snap.Files, snap.Absent, wantTree, wantSum, len(ents)-1)
	}
	if snap.Mask == nil || snap.Mask.Source != filepath.Join(dst, ".factory") || snap.Mask.Target != ".factory" || !snap.Mask.Dir {
		t.Fatalf("mask = %+v", snap.Mask)
	}
	for _, e := range ents {
		rel, below := strings.CutPrefix(e.path, ".factory/")
		if !below {
			continue
		}
		p := filepath.Join(dst, ".factory", filepath.FromSlash(rel))
		info, err := os.Lstat(p)
		if err != nil {
			t.Fatal(err)
		}
		wantMode := os.FileMode(0o444)
		if e.mode == "100755" {
			wantMode = 0o555
		}
		if info.Mode() != wantMode {
			t.Errorf("%s: mode %v, want %v", rel, info.Mode(), wantMode)
		}
		if got := readFile(t, p); got != e.body {
			t.Errorf("%s: %d bytes written, want the %d of the blob", rel, len(got), len(e.body))
		}
	}
	for _, d := range []string{"", "a", "a/b", "a/b/c", "z"} {
		info, err := os.Lstat(filepath.Join(dst, ".factory", d))
		if err != nil || info.Mode() != os.ModeDir|0o555 {
			t.Errorf("directory %q: %v, %v", d, info, err)
		}
	}
}

// What a snapshot refuses, word for word.
func TestCommitDirSnapshotRefusalsAreTheRecordedOnes(t *testing.T) {
	sub := strings.Repeat("b", 40)
	for _, tc := range []struct {
		name string
		ents []tf
		want string
	}{
		{"symlink", []tf{file(".factory/ok", "x"), {path: ".factory/x", mode: "120000", body: "../src/x"}}, `commit directory snapshot: ".factory/x" is a symlink`},
		{"gitlink", []tf{file(".factory/ok", "x"), {path: ".factory/a/sub", mode: "160000", oid: sub}}, `commit directory snapshot: ".factory/a/sub" is a gitlink (submodule)`},
		{"folds to .git", []tf{file(".factory/.GIT/x", "x")}, `commit directory snapshot: ".factory/.GIT" has a component that folds to .git`},
		{"siblings fold alike", []tf{file(".factory/A", "1"), file(".factory/a", "2")}, `commit directory snapshot: ".factory/A" and ".factory/a" fold to the same name`},
		{"blob over the limit", []tf{file(".factory/ok", "x"), file(".factory/big", strings.Repeat("a", maxCommitDirBlobBytes+1))}, `commit directory snapshot: ".factory/big" is 4194305 bytes, over 4194304`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newBareRepo(t)
			_, _, err := r.takeSnapshot(r.commitOf(tc.ents...), ".factory")
			if err == nil || err.Error() != tc.want {
				t.Fatalf("error = %v, want %s", err, tc.want)
			}
			r.mustRefuse(r.commitOf(tc.ents...), ".factory")
		})
	}
}

// A blob the tree names and the repository does not hold refuses the
// snapshot when the tree is listed, before anything is written.
func TestCommitDirSnapshotOfAMissingBlobIsRefusedByTheListing(t *testing.T) {
	r := newBareRepo(t)
	missing := strings.Repeat("ab", 20)
	c := r.commitOf(file(".factory/a", "first\n"), tf{path: ".factory/d/missing", mode: "100644", oid: missing}, file(".factory/z", "last\n"))
	_, _, err := r.takeSnapshot(c, ".factory")
	want := `commit directory snapshot: read tree: unreadable size in tree entry "100644 blob ` + missing + `     BAD` + "\\t" + `d/missing"`
	if err == nil || err.Error() != want {
		t.Fatalf("error = %v, want %s", err, want)
	}
	r.mustRefuse(c, ".factory")
}

// A blob whose size git can list and whose bytes it cannot inflate is found
// only when the blob is read: the snapshot is refused, naming the file, and
// the files written before it are removed.
func TestCommitDirSnapshotOfABlobCorruptPastItsHeaderIsRefused(t *testing.T) {
	r := newBareRepo(t)
	var body strings.Builder
	for i := 0; body.Len() < 64<<10; i++ {
		fmt.Fprintf(&body, "%x\n", sha256.Sum256([]byte{byte(i), byte(i >> 8)}))
	}
	c := r.commitOf(file(".factory/a", "first\n"), file(".factory/d/bad", body.String()), file(".factory/z", "last\n"))
	oid := r.git("rev-parse", c+":.factory/d/bad")
	obj := filepath.Join(r.dir, ".git", "objects", oid[:2], oid[2:])
	var packed bytes.Buffer
	zw := zlib.NewWriter(&packed)
	fmt.Fprintf(zw, "blob %d\x00%s", body.Len(), body.String())
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(obj, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(obj, packed.Bytes()[:packed.Len()/2], 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err := r.takeSnapshot(c, ".factory")
	if err == nil || !strings.HasPrefix(err.Error(), `commit directory snapshot: read "d/bad": git cat-file: `) {
		t.Fatalf("error = %v", err)
	}
	t.Logf("refusal: %v", err)
	r.mustRefuse(c, ".factory", `read "d/bad"`)
}
