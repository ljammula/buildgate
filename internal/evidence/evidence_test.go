package evidence

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
	"time"
)

func TestUnionSortedDeduplicatesAndSorts(t *testing.T) {
	got := UnionSorted([]string{"b.go", "a.go"}, []string{"a.go", "c.go"})
	want := []string{"a.go", "b.go", "c.go"}
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestUnionSortedBothEmptyIsEmptyNotNil(t *testing.T) {
	got := UnionSorted(nil, nil)
	if got == nil || len(got) != 0 {
		t.Errorf("got %v, want a non-nil empty slice", got)
	}
}

func TestDependencyLockfilesTouchedMatchesRoot(t *testing.T) {
	got := DependencyLockfilesTouched([]string{"package-lock.json"})
	want := []string{"package-lock.json"}
	if !slices.Equal(got, want) {
		t.Errorf("DependencyLockfilesTouched() = %v, want %v", got, want)
	}
}

func TestDependencyLockfilesTouchedMatchesSubdirectory(t *testing.T) {
	got := DependencyLockfilesTouched([]string{"frontend/pubspec.lock"})
	want := []string{"frontend/pubspec.lock"}
	if !slices.Equal(got, want) {
		t.Errorf("DependencyLockfilesTouched() = %v, want %v", got, want)
	}
}

func TestDependencyLockfilesTouchedRejectsSubstring(t *testing.T) {
	got := DependencyLockfilesTouched([]string{
		"not-a-package-lock.json.bak",
		"mypackage-lock.json",
	})
	if got == nil || len(got) != 0 {
		t.Errorf("DependencyLockfilesTouched() = %v, want non-nil empty slice", got)
	}
}

func TestDependencyLockfilesTouchedEmptyInputIsNonNil(t *testing.T) {
	got := DependencyLockfilesTouched(nil)
	if got == nil || len(got) != 0 {
		t.Errorf("DependencyLockfilesTouched() = %v, want non-nil empty slice", got)
	}
}

func TestDependencyLockfilesTouchedNoMatchesIsNonNil(t *testing.T) {
	got := DependencyLockfilesTouched([]string{"main.go", "docs/README.md"})
	if got == nil || len(got) != 0 {
		t.Errorf("DependencyLockfilesTouched() = %v, want non-nil empty slice", got)
	}
}

func TestSHA256File(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.txt")
	content := []byte("hello evidence\n")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	got, err := SHA256File(path)
	if err != nil {
		t.Fatalf("SHA256File: %v", err)
	}

	sum := sha256.Sum256(content)
	want := hex.EncodeToString(sum[:])
	if got != want {
		t.Errorf("SHA256File = %q, want %q", got, want)
	}
}

func TestSHA256FileMissing(t *testing.T) {
	if _, err := SHA256File(filepath.Join(t.TempDir(), "missing.txt")); err == nil {
		t.Error("expected an error for a missing file")
	}
}

func TestSHA256FileChangesWithContent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.txt")
	if err := os.WriteFile(path, []byte("v1"), 0o644); err != nil {
		t.Fatalf("write v1: %v", err)
	}
	h1, err := SHA256File(path)
	if err != nil {
		t.Fatalf("SHA256File v1: %v", err)
	}

	if err := os.WriteFile(path, []byte("v2"), 0o644); err != nil {
		t.Fatalf("write v2: %v", err)
	}
	h2, err := SHA256File(path)
	if err != nil {
		t.Fatalf("SHA256File v2: %v", err)
	}

	if h1 == h2 {
		t.Error("hash did not change when file content changed")
	}
}

func TestRetainFileCopiesContent(t *testing.T) {
	src := filepath.Join(t.TempDir(), "report.md")
	if err := os.WriteFile(src, []byte("hello evidence"), 0o644); err != nil {
		t.Fatalf("write src: %v", err)
	}
	dst := filepath.Join(t.TempDir(), "nested", "report.md")

	if err := RetainFile(src, dst); err != nil {
		t.Fatalf("RetainFile: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if string(got) != "hello evidence" {
		t.Errorf("dst content = %q, want %q", got, "hello evidence")
	}
}

func TestRetainFileMissingSourceReportsIsNotExist(t *testing.T) {
	err := RetainFile(filepath.Join(t.TempDir(), "missing.md"), filepath.Join(t.TempDir(), "dst.md"))
	if !os.IsNotExist(err) {
		t.Errorf("RetainFile error = %v, want os.IsNotExist", err)
	}
}

// TestRetainFileWritesDestinationWithRestrictivePermissions is the
// regression test for a real GitHub Codex App review finding on this PR:
// os.Create's default mode (0666 minus umask, typically 0644) let a
// retained agent report be readable by other local users sharing this
// run directory's group-traversable (0750) parent, unlike every other
// evidence file in a run's own directory (run.json, notification logs),
// which are 0600.
func TestRetainFileWritesDestinationWithRestrictivePermissions(t *testing.T) {
	src := filepath.Join(t.TempDir(), "report.md")
	if err := os.WriteFile(src, []byte("hello evidence"), 0o644); err != nil {
		t.Fatalf("write src: %v", err)
	}
	dst := filepath.Join(t.TempDir(), "report.md")

	if err := RetainFile(src, dst); err != nil {
		t.Fatalf("RetainFile: %v", err)
	}
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("stat dst: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("dst permissions = %v, want 0600", perm)
	}
}

// TestRetainFileCorrectsPermissionsOnExistingDestination covers replacing
// a destination that already exists under a looser mode (e.g. written
// before this fix, or by some other path) -- RetainFile must correct it,
// not merely leave whatever mode the file already had.
func TestRetainFileCorrectsPermissionsOnExistingDestination(t *testing.T) {
	src := filepath.Join(t.TempDir(), "report.md")
	if err := os.WriteFile(src, []byte("new content"), 0o644); err != nil {
		t.Fatalf("write src: %v", err)
	}
	dst := filepath.Join(t.TempDir(), "report.md")
	if err := os.WriteFile(dst, []byte("stale content"), 0o644); err != nil {
		t.Fatalf("seed pre-existing dst: %v", err)
	}

	if err := RetainFile(src, dst); err != nil {
		t.Fatalf("RetainFile: %v", err)
	}
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("stat dst: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("dst permissions = %v, want 0600 (corrected from the pre-existing 0644)", perm)
	}
}

// TestRetainFileLeavesNoTempFileBehindAfterSuccess covers the temp-file-
// plus-rename fix's cleanup half: the intermediate ".retain-*.tmp" file
// must not survive a successful call.
func TestRetainFileLeavesNoTempFileBehindAfterSuccess(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.md")
	if err := os.WriteFile(src, []byte("content"), 0o644); err != nil {
		t.Fatalf("write src: %v", err)
	}
	dst := filepath.Join(dir, "dst.md")

	if err := RetainFile(src, dst); err != nil {
		t.Fatalf("RetainFile: %v", err)
	}

	matches, err := filepath.Glob(filepath.Join(dir, ".retain-*.tmp"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(matches) != 0 {
		t.Errorf("leftover temp files = %v, want none", matches)
	}
}

// TestRetainFileDoesNotTouchDestinationOnCopyFailure is the regression
// test for a real GitHub Codex App review finding on this PR: writing
// directly into dst (rather than a temp file renamed into place) meant a
// failure partway through the copy could leave dst truncated, or could
// have already destroyed a complete prior copy before the new one even
// started. Forces a copy failure by pointing src at a directory (reading
// from a directory fd errors) after seeding dst with known content.
func TestRetainFileDoesNotTouchDestinationOnCopyFailure(t *testing.T) {
	dir := t.TempDir()
	srcDir := filepath.Join(dir, "src-is-a-directory")
	if err := os.Mkdir(srcDir, 0o750); err != nil {
		t.Fatalf("mkdir srcDir: %v", err)
	}
	dst := filepath.Join(dir, "dst.md")
	if err := os.WriteFile(dst, []byte("original content"), 0o600); err != nil {
		t.Fatalf("seed dst: %v", err)
	}

	if err := RetainFile(srcDir, dst); err == nil {
		t.Fatal("RetainFile(directory, dst) error = nil, want an error")
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if string(got) != "original content" {
		t.Errorf("dst content = %q after a failed retain, want it untouched", got)
	}

	matches, err := filepath.Glob(filepath.Join(dir, ".retain-*.tmp"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(matches) != 0 {
		t.Errorf("leftover temp files = %v, want none", matches)
	}
}

// TestRetainFileRefusesSymlinkSource is the regression test for a real
// GitHub Codex App review finding on this PR: os.Open follows a symlink,
// so an untrusted build replacing the expected report with one pointing
// elsewhere on the host let this function archive a file that was never
// inside the sandbox at all.
func TestRetainFileRefusesSymlinkSource(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.md")
	if err := os.WriteFile(target, []byte("host secret"), 0o644); err != nil {
		t.Fatalf("write target: %v", err)
	}
	link := filepath.Join(dir, "report.md")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	dst := filepath.Join(dir, "dst.md")

	if err := RetainFile(link, dst); err == nil {
		t.Fatal("RetainFile(symlink, dst) error = nil, want a refusal")
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Errorf("Stat(dst) error = %v, want IsNotExist -- a refused symlink source must not produce a destination file", err)
	}
}

// TestRetainFileRefusesNamedPipeSource is the regression test for the
// other half of the same finding: a named pipe (or device node) passes
// an O_NOFOLLOW open (it isn't a symlink) but must still be refused --
// otherwise a plain io.Copy against it can block this process forever,
// or (for a device like /dev/zero) fill the durable-data filesystem.
func TestRetainFileRefusesNamedPipeSource(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "report.md")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo unavailable on this platform: %v", err)
	}
	dst := filepath.Join(dir, "dst.md")

	done := make(chan error, 1)
	go func() { done <- RetainFile(fifo, dst) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("RetainFile(fifo, dst) error = nil, want a refusal")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RetainFile(fifo, dst) blocked -- a named pipe must be refused before ever being read, not opened and read")
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Errorf("Stat(dst) error = %v, want IsNotExist -- a refused pipe source must not produce a destination file", err)
	}
}

// TestRetainFileRefusesOversizedSource is the regression test for the
// size-limit half of the same finding: a maliciously (or just very)
// large regular file must be refused, not copied in full into this
// process's own durable-data filesystem.
func TestRetainFileRefusesOversizedSource(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "report.md")
	f, err := os.Create(src)
	if err != nil {
		t.Fatalf("create src: %v", err)
	}
	if err := f.Truncate(maxRetainFileSize + 1); err != nil {
		f.Close()
		t.Fatalf("truncate src to oversized length: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close src: %v", err)
	}
	dst := filepath.Join(dir, "dst.md")

	if err := RetainFile(src, dst); err == nil {
		t.Fatal("RetainFile(oversized, dst) error = nil, want a refusal")
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Errorf("Stat(dst) error = %v, want IsNotExist -- a refused oversized source must not produce a destination file", err)
	}
}

// TestReadHostileFileReadsRegularFileContent is ReadHostileFile's
// legitimate-case counterpart to TestRetainFileCopiesContent: a normal
// small regular file must read back byte-for-byte.
func TestReadHostileFileReadsRegularFileContent(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "evidence.json")
	want := `{"schema_version":1,"succeeded":true,"rounds":[]}`
	if err := os.WriteFile(src, []byte(want), 0o644); err != nil {
		t.Fatalf("write src: %v", err)
	}

	got, err := ReadHostileFile(src, maxRetainFileSize)
	if err != nil {
		t.Fatalf("ReadHostileFile: %v", err)
	}
	if string(got) != want {
		t.Errorf("ReadHostileFile content = %q, want %q", got, want)
	}
}

// TestReadHostileFileMissingSourceReportsIsNotExist mirrors
// TestRetainFileMissingSourceReportsIsNotExist: a caller (loadAgentEvidence)
// distinguishes "no BUILD_EVIDENCE.json was written at all" from a real
// read failure via os.IsNotExist.
func TestReadHostileFileMissingSourceReportsIsNotExist(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "missing.json")

	if _, err := ReadHostileFile(src, maxRetainFileSize); !os.IsNotExist(err) {
		t.Errorf("ReadHostileFile(missing) error = %v, want IsNotExist", err)
	}
}

// TestReadHostileFileRefusesSymlinkSource is ReadHostileFile's regression
// test for the same finding TestRetainFileRefusesSymlinkSource guards:
// an untrusted build substituting BUILD_EVIDENCE.json with a symlink must
// not have its target read and (if it happens to parse as valid JSON)
// persisted into this run's own durable AgentEvidence.
func TestReadHostileFileRefusesSymlinkSource(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "secret.json")
	if err := os.WriteFile(target, []byte(`{"schema_version":1,"succeeded":true,"rounds":[]}`), 0o644); err != nil {
		t.Fatalf("write target: %v", err)
	}
	link := filepath.Join(dir, "evidence.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	if _, err := ReadHostileFile(link, maxRetainFileSize); err == nil {
		t.Fatal("ReadHostileFile(symlink) error = nil, want a refusal")
	}
}

// TestReadHostileFileRefusesNamedPipeSource is ReadHostileFile's
// regression test for the same finding TestRetainFileRefusesNamedPipeSource
// guards: a FIFO with no writer must be refused before ever being opened
// for a blocking read, not hang this process indefinitely.
func TestReadHostileFileRefusesNamedPipeSource(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "evidence.json")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo unavailable on this platform: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := ReadHostileFile(fifo, maxRetainFileSize)
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("ReadHostileFile(fifo) error = nil, want a refusal")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ReadHostileFile(fifo) blocked -- a named pipe must be refused before ever being read, not opened and read")
	}
}

// TestReadHostileFileRefusesOversizedSource is ReadHostileFile's
// regression test for the same finding TestRetainFileRefusesOversizedSource
// guards: a maliciously (or just very) large regular file must be refused,
// not read in full into this process's own memory.
func TestReadHostileFileRefusesOversizedSource(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "evidence.json")
	f, err := os.Create(src)
	if err != nil {
		t.Fatalf("create src: %v", err)
	}
	if err := f.Truncate(maxRetainFileSize + 1); err != nil {
		f.Close()
		t.Fatalf("truncate src to oversized length: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close src: %v", err)
	}

	if _, err := ReadHostileFile(src, maxRetainFileSize); err == nil {
		t.Fatal("ReadHostileFile(oversized) error = nil, want a refusal")
	}
}

// writeTreeFiles writes each name -> content pair under dir, creating
// parent directories as needed -- shared by SHA256Tree's own tests below.
func writeTreeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", name, err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
}

// TestSHA256TreeDeterministicRegardlessOfWriteOrder is the core
// correctness guarantee: two directories with identical (path, content)
// pairs produce the same digest, even though creation order differs and
// the filesystem's own directory-entry order is not guaranteed.
func TestSHA256TreeDeterministicRegardlessOfWriteOrder(t *testing.T) {
	dirA, dirB := t.TempDir(), t.TempDir()
	writeTreeFiles(t, dirA, map[string]string{"a.go": "package a\n", "sub/b.go": "package b\n"})
	writeTreeFiles(t, dirB, map[string]string{"sub/b.go": "package b\n", "a.go": "package a\n"})

	hashA, err := SHA256Tree(dirA)
	if err != nil {
		t.Fatalf("SHA256Tree(dirA): %v", err)
	}
	hashB, err := SHA256Tree(dirB)
	if err != nil {
		t.Fatalf("SHA256Tree(dirB): %v", err)
	}
	if hashA != hashB {
		t.Errorf("SHA256Tree differs for identical (path, content) pairs written in a different order: %q vs %q", hashA, hashB)
	}
}

// TestSHA256TreeChangesOnContentChange proves the hash actually reflects
// content, not just the set of paths present.
func TestSHA256TreeChangesOnContentChange(t *testing.T) {
	dir := t.TempDir()
	writeTreeFiles(t, dir, map[string]string{"oracle_test.go": "original\n"})
	before, err := SHA256Tree(dir)
	if err != nil {
		t.Fatalf("SHA256Tree before: %v", err)
	}
	writeTreeFiles(t, dir, map[string]string{"oracle_test.go": "tampered\n"})
	after, err := SHA256Tree(dir)
	if err != nil {
		t.Fatalf("SHA256Tree after: %v", err)
	}
	if before == after {
		t.Error("SHA256Tree did not change after the only file's content changed")
	}
}

// TestSHA256TreeChangesOnRename proves the hash binds content to its
// path, not just to the multiset of file contents present -- a file
// renamed but otherwise byte-identical must still change the digest.
func TestSHA256TreeChangesOnRename(t *testing.T) {
	dirOriginal, dirRenamed := t.TempDir(), t.TempDir()
	writeTreeFiles(t, dirOriginal, map[string]string{"oracle_test.go": "same content\n"})
	writeTreeFiles(t, dirRenamed, map[string]string{"renamed_test.go": "same content\n"})

	hashOriginal, err := SHA256Tree(dirOriginal)
	if err != nil {
		t.Fatalf("SHA256Tree(dirOriginal): %v", err)
	}
	hashRenamed, err := SHA256Tree(dirRenamed)
	if err != nil {
		t.Fatalf("SHA256Tree(dirRenamed): %v", err)
	}
	if hashOriginal == hashRenamed {
		t.Error("SHA256Tree unchanged after renaming the only file -- path is not actually bound into the digest")
	}
}

// TestSHA256TreeRefusesSymlink is the security-relevant case: a symlink
// under the tree could otherwise let the hashed content diverge from
// what a later read of the same path actually returns (e.g. the sandbox
// mount resolving through it to something outside the intended source).
func TestSHA256TreeRefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	writeTreeFiles(t, dir, map[string]string{"real.go": "package real\n"})
	if err := os.Symlink(filepath.Join(dir, "real.go"), filepath.Join(dir, "link.go")); err != nil {
		t.Fatalf("create symlink: %v", err)
	}
	if _, err := SHA256Tree(dir); err == nil {
		t.Fatal("SHA256Tree(dir with a symlink) error = nil, want a refusal")
	}
}

// TestSHA256TreeRefusesSymlinkedRoot is the regression test for a real
// finding (adversarial /code-review, 2026-09-15): filepath.WalkDir never
// descends into a symlinked root at all, so the walk callback's own
// "path == dir" branch used to return nil for the root entry before ever
// reaching the symlink check every other entry gets -- SHA256Tree(a
// symlink to a real directory) silently returned the hash of an EMPTY
// tree, no error, rather than either hashing the real target or
// refusing. This is exactly the case internal/sandbox.LaunchSpec.Run
// resolves via filepath.EvalSymlinks before mounting -reference-oracle-dir
// (the sandbox always sees the real, resolved content), so the bug would
// have persisted a hash matching no actual content -- defeating SC-012's
// whole point of proving which content produced a result.
func TestSHA256TreeRefusesSymlinkedRoot(t *testing.T) {
	real := t.TempDir()
	writeTreeFiles(t, real, map[string]string{"real.go": "package real\n"})
	parent := t.TempDir()
	link := filepath.Join(parent, "link-to-real")
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("create symlink: %v", err)
	}
	if _, err := SHA256Tree(link); err == nil {
		t.Fatal("SHA256Tree(a symlinked root) error = nil, want a refusal -- not a silent empty-tree hash")
	}
}

// TestSHA256TreeDoesNotCollideAcrossAmbiguousLayouts is the regression
// test for a real finding (adversarial /code-review, 2026-09-15): the
// original newline-delimited framing fed the identical byte stream
// "a\nx\nb\ny\n" into the hash for two genuinely different trees -- one
// file "a" containing literal bytes "x\nb\ny", and two files "a"="x" plus
// "b"="y" -- so different oracle layouts could share a digest without
// SHA-256 itself ever colliding, defeating the claimed path/content
// binding. The fixed length-prefixed framing must tell these apart.
func TestSHA256TreeDoesNotCollideAcrossAmbiguousLayouts(t *testing.T) {
	oneFile, twoFiles := t.TempDir(), t.TempDir()
	writeTreeFiles(t, oneFile, map[string]string{"a": "x\nb\ny"})
	writeTreeFiles(t, twoFiles, map[string]string{"a": "x", "b": "y"})

	hashOneFile, err := SHA256Tree(oneFile)
	if err != nil {
		t.Fatalf("SHA256Tree(oneFile): %v", err)
	}
	hashTwoFiles, err := SHA256Tree(twoFiles)
	if err != nil {
		t.Fatalf("SHA256Tree(twoFiles): %v", err)
	}
	if hashOneFile == hashTwoFiles {
		t.Error("SHA256Tree produced the same digest for two genuinely different tree layouts -- the framing is still ambiguous")
	}
}

// TestSnapshotTreeCopiesContent proves the basic contract: every regular
// file under src exists at the same relative path under dst, with the
// same bytes.
func TestSnapshotTreeCopiesContent(t *testing.T) {
	src := t.TempDir()
	writeTreeFiles(t, src, map[string]string{"a.go": "package a\n", "sub/b.go": "package b\n"})
	dst := filepath.Join(t.TempDir(), "snapshot")

	if err := SnapshotTree(src, dst); err != nil {
		t.Fatalf("SnapshotTree: %v", err)
	}
	for _, rel := range []string{"a.go", "sub/b.go"} {
		got, err := os.ReadFile(filepath.Join(dst, rel))
		if err != nil {
			t.Fatalf("read snapshot copy of %s: %v", rel, err)
		}
		want, err := os.ReadFile(filepath.Join(src, rel))
		if err != nil {
			t.Fatalf("read original %s: %v", rel, err)
		}
		if string(got) != string(want) {
			t.Errorf("snapshot content for %s = %q, want %q", rel, got, want)
		}
	}
}

// TestSnapshotTreeRefusesExistingDestination confirms SnapshotTree never
// merges into or silently overwrites a caller-supplied dst that already
// has something at that path -- callers are expected to always pass a
// fresh, unique staging path.
func TestSnapshotTreeRefusesExistingDestination(t *testing.T) {
	src := t.TempDir()
	writeTreeFiles(t, src, map[string]string{"a.go": "package a\n"})
	dst := t.TempDir() // already exists, unlike a fresh staging path
	if err := SnapshotTree(src, dst); err == nil {
		t.Fatal("SnapshotTree(existing dst) error = nil, want a refusal")
	}
}

// TestSnapshotTreeThenHashIsStableAgainstLaterSourceChanges is the
// direct regression test for the TOCTOU finding this function exists to
// close (adversarial /code-review, 2026-09-15): hashing a SnapshotTree
// copy must be unaffected by a later change to the original src, proving
// the snapshot is genuinely independent of src once taken -- unlike
// hashing src directly, which a concurrent host-side change could race.
func TestSnapshotTreeThenHashIsStableAgainstLaterSourceChanges(t *testing.T) {
	src := t.TempDir()
	writeTreeFiles(t, src, map[string]string{"oracle_test.go": "original\n"})
	dst := filepath.Join(t.TempDir(), "snapshot")
	if err := SnapshotTree(src, dst); err != nil {
		t.Fatalf("SnapshotTree: %v", err)
	}
	hashBefore, err := SHA256Tree(dst)
	if err != nil {
		t.Fatalf("SHA256Tree(dst) before source change: %v", err)
	}

	// Mutate the ORIGINAL source after the snapshot was already taken --
	// a stand-in for an external host-side process changing
	// -reference-oracle-dir out from under an in-flight gate.
	writeTreeFiles(t, src, map[string]string{"oracle_test.go": "tampered after snapshot\n"})

	hashAfter, err := SHA256Tree(dst)
	if err != nil {
		t.Fatalf("SHA256Tree(dst) after source change: %v", err)
	}
	if hashBefore != hashAfter {
		t.Error("the snapshot's own hash changed after mutating the ORIGINAL source -- the snapshot is not actually independent of src")
	}
}

// TestSnapshotTreeIsGroupReadable is the regression test for the P1
// finding on PR #152 round 2 (Codex review): the sandboxed gate
// container that mounts the snapshot runs as sandbox.DefaultWorkerUID,
// not the factoryd host process's own UID, so anything SnapshotTree
// creates must be readable/traversable by group, not just owner --
// 0700 directories and 0600 files (the original modes) would leave the
// container unable to read its own reference-oracle mount.
func TestSnapshotTreeIsGroupReadable(t *testing.T) {
	src := t.TempDir()
	writeTreeFiles(t, src, map[string]string{"sub/a.go": "package a\n"})
	dst := filepath.Join(t.TempDir(), "snapshot")
	if err := SnapshotTree(src, dst); err != nil {
		t.Fatalf("SnapshotTree: %v", err)
	}

	for _, rel := range []string{".", "sub"} {
		info, err := os.Stat(filepath.Join(dst, rel))
		if err != nil {
			t.Fatalf("stat %s: %v", rel, err)
		}
		if mode := info.Mode().Perm(); mode&0o050 != 0o050 {
			t.Errorf("directory %s mode = %o, want group r-x (0o050) bits set", rel, mode)
		}
	}
	fileInfo, err := os.Stat(filepath.Join(dst, "sub/a.go"))
	if err != nil {
		t.Fatalf("stat sub/a.go: %v", err)
	}
	if mode := fileInfo.Mode().Perm(); mode&0o040 != 0o040 {
		t.Errorf("file sub/a.go mode = %o, want group-read (0o040) bit set", mode)
	}
}

// TestSnapshotTreeRefusesNonDirectorySource is the regression test for
// the P2 finding on PR #152 round 2 (Codex review): without this check,
// filepath.WalkDir on a non-directory src invokes the walk callback
// exactly once, for src itself, which the path == src branch then
// skips -- producing an empty dst and a nil error instead of any
// indication -reference-oracle-dir named a file, not a directory.
func TestSnapshotTreeRefusesNonDirectorySource(t *testing.T) {
	src := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(src, []byte("plain file\n"), 0o600); err != nil {
		t.Fatalf("write %s: %v", src, err)
	}
	dst := filepath.Join(t.TempDir(), "snapshot")

	err := SnapshotTree(src, dst)
	if err == nil {
		t.Fatal("SnapshotTree(regular file src) error = nil, want a refusal")
	}
	if _, statErr := os.Stat(dst); !os.IsNotExist(statErr) {
		t.Errorf("SnapshotTree(regular file src) left %s behind (stat err = %v), want no destination created", dst, statErr)
	}
}
