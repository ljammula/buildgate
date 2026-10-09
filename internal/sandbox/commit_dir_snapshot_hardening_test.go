package sandbox

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// rawTree writes a tree object with entries exactly as given (duplicates and
// all), which git mktree refuses.
func (r *instructionRepo) rawTree(entries ...[3]string) string {
	r.t.Helper()
	var b bytes.Buffer
	for _, e := range entries { // mode, name, oid
		raw, err := hex.DecodeString(e[2])
		if err != nil {
			r.t.Fatal(err)
		}
		b.WriteString(e[0] + " " + e[1] + "\x00")
		b.Write(raw)
	}
	return r.gitIn(b.Bytes(), "hash-object", "-t", "tree", "-w", "--literally", "--stdin")
}

func (r *instructionRepo) commitTree(tree string) string {
	r.t.Helper()
	return r.git("commit-tree", tree, "-m", "snap")
}

func (r *instructionRepo) blob(body string) string {
	r.t.Helper()
	return r.gitIn([]byte(body), "hash-object", "-w", "--stdin")
}

func TestCommitDirSnapshotIgnoresReplaceRefs(t *testing.T) {
	r := newBareRepo(t)
	c := r.commitOf(file(".factory/gate.sh", "real\n"), file("src/a", "x"))
	blobID := r.git("rev-parse", c+":.factory/gate.sh")
	treeID := r.git("rev-parse", c+":.factory")
	evil := r.blob("evil\n")
	r.git("update-ref", "refs/replace/"+blobID, evil)
	_, dst := r.mustSnapshot(c)
	if got := readFile(t, filepath.Join(dst, ".factory", "gate.sh")); got != "real\n" {
		t.Fatalf("a replace ref changed the blob: %q", got)
	}
	// Replacing the commit swaps the whole tree.
	other := r.commitOf(file(".factory/gate.sh", "other commit\n"))
	r.git("update-ref", "refs/replace/"+c, other)
	snap, dst2 := r.mustSnapshot(c)
	if got := readFile(t, filepath.Join(dst2, ".factory", "gate.sh")); got != "real\n" || snap.TreeOID != treeID {
		t.Fatalf("a replaced commit changed the tree: %q %s", got, snap.TreeOID)
	}
}

func TestCommitDirSnapshotRunsNoCommandFromRepositoryConfig(t *testing.T) {
	r := newBareRepo(t)
	missing := strings.Repeat("ab", 20)
	c := r.commitOf(file(".factory/ok", "x"), tf{path: ".factory/missing", mode: "100644", oid: missing})
	marker := filepath.Join(filepath.Dir(r.dir), "lazy-fetch-ran")
	r.git("config", "core.repositoryformatversion", "1")
	r.git("config", "extensions.partialClone", "origin")
	r.git("config", "remote.origin.promisor", "true")
	r.git("config", "remote.origin.url", filepath.Join(filepath.Dir(r.dir), "nowhere"))
	r.git("config", "remote.origin.uploadpack", "touch "+marker+"; false")
	r.git("config", "remote.origin.partialclonefilter", "blob:none")
	snap, _, err := r.takeSnapshot(c, ".factory")
	if err == nil || !errors.Is(err, ErrCommitDirSnapshot) || snap.Mask != nil {
		t.Fatalf("snapshot of a missing blob = %+v, %v", snap, err)
	}
	if _, serr := os.Lstat(marker); !errors.Is(serr, fs.ErrNotExist) {
		t.Fatal("git ran a command from the repository config (lazy fetch)")
	}
}

func TestReviewGitEnvDropsInheritedGitVariables(t *testing.T) {
	for _, kv := range []string{"GIT_DIR=/x", "GIT_WORK_TREE=/x", "GIT_INDEX_FILE=/x", "GIT_OBJECT_DIRECTORY=/x", "GIT_ALTERNATE_OBJECT_DIRECTORIES=/x", "GIT_COMMON_DIR=/x", "GIT_NAMESPACE=x", "GIT_CEILING_DIRECTORIES=/x", "GIT_CONFIG=/x", "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=core.pager", "GIT_CONFIG_VALUE_0=x", "GIT_CONFIG_PARAMETERS='core.pager=x'", "GIT_REPLACE_REF_BASE=refs/x/", "GIT_GRAFT_FILE=/x", "GIT_SHALLOW_FILE=/x", "GIT_CONFIG_GLOBAL=/x", "GIT_EXTERNAL_DIFF=/x"} {
		k, v, _ := strings.Cut(kv, "=")
		t.Setenv(k, v)
	}
	env := map[string]string{}
	for _, kv := range reviewGitEnv() {
		k, v, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "GIT_") {
			env[k] = v
		}
	}
	want := map[string]string{"GIT_LITERAL_PATHSPECS": "1", "GIT_OPTIONAL_LOCKS": "0", "GIT_TERMINAL_PROMPT": "0", "GIT_ATTR_NOSYSTEM": "1", "GIT_CONFIG_NOSYSTEM": "1", "GIT_NO_REPLACE_OBJECTS": "1", "GIT_NO_LAZY_FETCH": "1", "GIT_CONFIG_GLOBAL": "/dev/null"}
	if fmt.Sprint(env) != fmt.Sprint(want) {
		t.Fatalf("GIT_ environment = %v, want %v", env, want)
	}
}

func TestCommitDirSnapshotIgnoresInheritedGitEnvironment(t *testing.T) {
	r := newBareRepo(t)
	c := r.commitOf(file(".factory/a.sh", "real\n"))
	other := newBareRepo(t)
	t.Setenv("GIT_DIR", filepath.Join(other.dir, ".git"))
	t.Setenv("GIT_WORK_TREE", other.dir)
	t.Setenv("GIT_INDEX_FILE", filepath.Join(other.dir, "nope-index"))
	t.Setenv("GIT_OBJECT_DIRECTORY", filepath.Join(other.dir, ".git", "objects"))
	t.Setenv("GIT_NAMESPACE", "ns")
	t.Setenv("GIT_REPLACE_REF_BASE", "refs/nowhere/")
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "core.bare")
	t.Setenv("GIT_CONFIG_VALUE_0", "true")
	snap, dst, err := r.takeSnapshot(c, ".factory")
	if err != nil {
		t.Fatal(err)
	}
	if snap.Files != 1 || readFile(t, filepath.Join(dst, ".factory", "a.sh")) != "real\n" {
		t.Fatalf("snapshot = %+v", snap)
	}
}

func TestCommitDirSnapshotRefusesAnExponentialTreeQuickly(t *testing.T) {
	r := newBareRepo(t)
	tree := r.treeOf(nil, map[string]string{}) // the empty tree
	for i := 0; i < 28; i++ {
		tree = r.rawTree([3]string{"40000", "a", tree}, [3]string{"40000", "b", tree})
	}
	c := r.commitOf(tf{path: "x", mode: "100644", body: "x"})
	root := r.rawTree([3]string{"40000", ".factory", tree}, [3]string{"100644", "x", r.blob("x")})
	c = r.commitTree(root)
	start := time.Now()
	r.mustRefuse(c, ".factory", "more than")
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("refusal took %v", d)
	}
}

func TestCommitDirSnapshotDeadlineIsARefusal(t *testing.T) {
	r := newBareRepo(t)
	c := r.commitOf(file(".factory/a", "x"))
	old := commitDirTimeout
	commitDirTimeout = time.Nanosecond
	t.Cleanup(func() { commitDirTimeout = old })
	snap, dst, err := r.takeSnapshot(c, ".factory")
	if err == nil || !errors.Is(err, ErrCommitDirSnapshot) || !strings.Contains(err.Error(), "deadline") || snap.Mask != nil {
		t.Fatalf("snap = %+v, err = %v", snap, err)
	}
	if _, serr := os.Lstat(dst); !errors.Is(serr, fs.ErrNotExist) {
		t.Fatal("destination left behind")
	}
}

func TestCommitDirSnapshotRefusesATreeThatListsANameTwice(t *testing.T) {
	r := newBareRepo(t)
	x := r.treeOf([]tf{file("x", "x")}, map[string]string{})
	y := r.treeOf([]tf{file("y", "y")}, map[string]string{})
	for _, tc := range []struct {
		name string
		root string
	}{
		{"nested directory", r.rawTree([3]string{"40000", ".factory", r.rawTree([3]string{"40000", "d", x}, [3]string{"40000", "d", y})})},
		{"two files", r.rawTree([3]string{"40000", ".factory", r.rawTree([3]string{"100644", "f", r.blob("1")}, [3]string{"100644", "f", r.blob("2")})})},
		{"directory and file", r.rawTree([3]string{"40000", ".factory", r.rawTree([3]string{"40000", "d", x}, [3]string{"100644", "d", r.blob("2")})})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r.mustRefuse(r.commitTree(tc.root), ".factory", "more than once")
		})
	}
}

func TestCommitDirSnapshotRefusesDirectoryNamesThatFoldAlike(t *testing.T) {
	r := newBareRepo(t)
	x := r.treeOf([]tf{file("x", "x")}, map[string]string{})
	y := r.treeOf([]tf{file("y", "y")}, map[string]string{})
	root := r.rawTree([3]string{"40000", ".factory", r.rawTree([3]string{"40000", "Dir", x}, [3]string{"40000", "dir", y})})
	r.mustRefuse(r.commitTree(root), ".factory", "fold to the same name")
}

func TestCommitDirSnapshotDetectsCollisionsTheFilesystemMakes(t *testing.T) {
	probe := t.TempDir()
	if err := os.Mkdir(filepath.Join(probe, "Ᏸ"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(probe, "ᏸ"), 0o755); err == nil {
		t.Skip("this filesystem keeps U+13F0 and U+13F8 apart; the collision cannot be reproduced here")
	}
	r := newBareRepo(t)
	c := r.commitOf(file(".factory/Ᏸ/x", "1"), file(".factory/ᏸ/y", "2"))
	r.mustRefuse(c, ".factory", "already exists")
	// A file collides with a file the same way.
	c = r.commitOf(file(".factory/Ᏸ", "1"), file(".factory/ᏸ", "2"))
	r.mustRefuse(c, ".factory", "already exists")
}

// A name or path no filesystem would take is refused before any record of it
// is kept: thousands of files under one 100 KiB directory name otherwise cost
// a gigabyte before the write fails.
func TestCommitDirSnapshotRefusesOverlongNamesAndPaths(t *testing.T) {
	r := newBareRepo(t)
	x := r.treeOf([]tf{file("x", "x")}, map[string]string{})
	longName := strings.Repeat("n", 256)
	deep := x
	for i := 0; i < 21; i++ { // 21 components of 200 bytes: over 4096 with separators
		deep = r.rawTree([3]string{"40000", strings.Repeat("d", 200), deep})
	}
	for _, tc := range []struct {
		name string
		root string
	}{
		{"a 256-byte name", r.rawTree([3]string{"40000", ".factory", r.rawTree([3]string{"40000", longName, x})})},
		{"a 100 KiB name", r.rawTree([3]string{"40000", ".factory", r.rawTree([3]string{"40000", strings.Repeat("n", 100<<10), x})})},
		{"a path over 4096 bytes", r.rawTree([3]string{"40000", ".factory", deep})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r.mustRefuse(r.commitTree(tc.root), ".factory", "bytes")
		})
	}
	// A 255-byte name is taken.
	ok := r.rawTree([3]string{"40000", ".factory", r.rawTree([3]string{"40000", strings.Repeat("n", 255), x})})
	if _, _, err := r.takeSnapshot(r.commitTree(ok), ".factory"); err != nil {
		t.Fatalf("a 255-byte name was refused: %v", err)
	}
}
