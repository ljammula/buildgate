// Package evidence provides small, factory-owned helpers for producing
// tamper-evident evidence — currently just content hashing. Keeping this
// separate from internal/run (the record shape) and internal/runner (the
// subprocess/git wrapper) matches the plan's internal/evidence/ layout for
// "logs, reports, hashes, artifacts".
package evidence

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"syscall"
)

// maxRetainFileSize bounds RetainFile's copy. src is agent-controlled
// content from an untrusted workspace (see RetainFile's own doc comment
// on why that source is never trusted with more than a byte copy); an
// unbounded copy of a maliciously huge regular file would let that same
// untrusted content fill this process's own durable-data filesystem.
// 10 MiB is generous for a free-text build report — real ones are a few
// KB — with headroom for one written unusually verbosely.
const maxRetainFileSize = 10 << 20

// KnownLockfileNames lists dependency-lockfile basenames recognized by
// filename alone, without parsing their contents. It is intentionally not
// exhaustive; add ecosystems when a real need arises.
var KnownLockfileNames = []string{
	"package-lock.json",
	"yarn.lock",
	"pnpm-lock.yaml",
	"pubspec.lock",
	"go.sum",
	"Gemfile.lock",
	"poetry.lock",
	"Cargo.lock",
	"composer.lock",
}

// DependencyLockfilesTouched returns changed paths whose exact basename is a
// known dependency lockfile. This is filename-based evidence only: it does not
// inspect the diff, claim what changed inside, or affect a run's outcome.
func DependencyLockfilesTouched(changedFiles []string) []string {
	known := make(map[string]bool, len(KnownLockfileNames))
	for _, name := range KnownLockfileNames {
		known[name] = true
	}

	touched := make([]string, 0)
	for _, path := range changedFiles {
		if known[filepath.Base(path)] {
			touched = append(touched, path)
		}
	}
	return touched
}

// UnionSorted returns the sorted, deduplicated union of a and b. Used to
// combine a committed-changes inventory (e.g. git diff --name-only)
// with an uncommitted-changes inventory (e.g. git status) into one
// changed-file list — shared, not duplicated, between cmd/factoryd's
// direct-supervisor path and a Temporal Activity, since both need the
// exact same combination for their evidence to agree.
func UnionSorted(a, b []string) []string {
	set := make(map[string]bool, len(a)+len(b))
	for _, f := range a {
		set[f] = true
	}
	for _, f := range b {
		set[f] = true
	}
	out := make([]string, 0, len(set))
	for f := range set {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

// SHA256File returns the hex-encoded SHA-256 digest of path's contents.
// Streams the file through the hash rather than reading it whole into
// memory first, so hashing a large verify.log can't add its own
// unbounded allocation on top of the copy already held by the OS/disk
// cache.
func SHA256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// openHostileRegularFile opens src (agent-controlled content from an
// untrusted sandboxed worker's own workspace) safely for reading, shared by
// every caller in this package that touches such a path: O_NOFOLLOW|
// O_NONBLOCK refuses a symlink/named-pipe/device-node substitution (see
// RetainFile's own doc comment for the exact live finding this closes,
// found via a real GitHub Codex App review), and the returned *os.File is
// confirmed to stat as a regular file no larger than maxSize before any
// caller reads a byte from it. The caller still owns closing it, and
// still needs its own io.LimitReader over the returned file when actually
// reading (this Stat-time check alone doesn't close the race against src
// growing between here and that read -- see RetainFile's own comment on
// exactly that race).
func openHostileRegularFile(src string, maxSize int64) (*os.File, error) {
	in, err := os.OpenFile(src, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	return checkHostileRegularFile(in, src, maxSize)
}

// openHostileRegularFileIn is openHostileRegularFile for a source named
// relative to root, a directory the untrusted worker could write (its
// workspace) or one that holds copies of what it wrote (a run's directory):
// the open goes through root, so no link anywhere in src leads out of it,
// follows no link at src itself and does not block on a pipe, and the opened
// file must be a regular file of at most maxSize bytes. It is the one way
// this package opens such a file: what is then done with it (a byte copy, a
// redacted copy, a read) and the cap are the caller's. The open's own error
// is returned as it is, so os.IsNotExist tells a missing file apart.
func openHostileRegularFileIn(root *os.Root, src string, maxSize int64) (*os.File, error) {
	in, err := root.OpenFile(src, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	return checkHostileRegularFile(in, src, maxSize)
}

// checkHostileRegularFile is openHostileRegularFile's check on a file the
// caller opened itself (with O_NOFOLLOW|O_NONBLOCK): it closes in and
// returns an error unless in is a regular file no larger than maxSize.
func checkHostileRegularFile(in *os.File, src string, maxSize int64) (*os.File, error) {
	info, err := in.Stat()
	if err != nil {
		in.Close()
		return nil, fmt.Errorf("stat %s: %w", src, err)
	}
	if !info.Mode().IsRegular() {
		in.Close()
		return nil, fmt.Errorf("%s is not a regular file (mode %s) -- refusing to read a symlink, device, or named pipe", src, info.Mode())
	}
	if info.Size() > maxSize {
		in.Close()
		return nil, fmt.Errorf("%s is %d bytes, exceeding the %d byte limit", src, info.Size(), maxSize)
	}
	return in, nil
}

// RetainFile copies src to dst byte-for-byte, streamed rather than read
// whole into memory first (same reasoning as SHA256File), creating dst's
// parent directory if needed. It never inspects, parses, or otherwise
// interprets src's content — a generic archival copy, not a reader of any
// specific evidence shape. Returns the *src os.Stat error unchanged (so a
// caller can distinguish "nothing there to retain" via os.IsNotExist) when
// src cannot be opened; any failure once copying has started is wrapped.
// Used to move a workspace-produced artifact (e.g. an agent's own report)
// into a run's durable evidence directory before that workspace is
// overwritten by a later run or discarded by a rollback — see
// cmd/factoryd's own caller for why this generic byte copy lives here
// rather than in main.go directly: package evidence never assigns meaning
// to what it copies, so main.go's own static guard against reading
// agent-authored content to decide anything can keep checking main.go
// alone without this legitimate archival copy tripping it.
//
// Writes through a temp file in dst's own directory, renamed into place
// only after a fully successful copy and close — not a direct write to
// dst (found via a real GitHub Codex App review of this PR): a crash or
// I/O failure partway through an in-place write would leave dst holding a
// truncated, partial copy, or would have already destroyed a complete
// prior copy before the new one finished, either way undermining the
// archival purpose this function exists for. Mirrors
// internal/release.SaveDecision's and kill_switch.go's own identical
// temp-file-plus-rename fix for the same class of bug.
//
// src is treated as hostile: every caller of this function today passes
// a path inside a sandboxed worker's own workspace, content this factory
// otherwise never trusts with more than a byte copy (see cmd/factoryd's
// own static guard against reading agent-authored content to decide
// anything). Opened with O_NOFOLLOW|O_NONBLOCK and rejected outright
// unless it stats as a regular file (found via a real GitHub Codex App
// review of this PR): an untrusted build replacing the expected report
// with a symlink would otherwise let this function follow it anywhere
// this process can read (archiving a host file that was never inside the
// sandbox at all); a named pipe or device node (e.g. /dev/zero) would let
// a plain io.Copy block this process forever or fill its durable-data
// filesystem -- O_NONBLOCK closes the same problem one step earlier,
// since a bare open() of a FIFO with no writer already blocks
// indefinitely on its own, before any read is even attempted (found by
// this fix's own test actually hanging without it). The copy itself is
// also bounded at maxRetainFileSize independent of the regular-file
// check, since a legitimate-looking regular file can still be
// maliciously large.
func RetainFile(src, dst string) error {
	in, err := openHostileRegularFile(src, maxRetainFileSize)
	if err != nil {
		return err
	}
	defer in.Close()
	return retainOpened(in, src, dst)
}

// retainOpened is RetainFile's copy of an already opened and checked src.
func retainOpened(in *os.File, src, dst string) error {

	dstDir := filepath.Dir(dst)
	if err := os.MkdirAll(dstDir, 0o750); err != nil {
		return fmt.Errorf("create directory for %s: %w", dst, err)
	}
	// os.CreateTemp already creates with 0o600 (matching decision.go's own
	// comment on the same point) -- restrictive by default, same
	// reasoning as every other file in a run's evidence directory
	// (run.json, notification logs): sensitive paths and run detail
	// should not be readable by other local users sharing the
	// group-traversable run directory (0750).
	tmp, err := os.CreateTemp(dstDir, ".retain-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp file for %s: %w", dst, err)
	}
	tmpName := tmp.Name()
	// Removed unconditionally on any early return; the rename below moves
	// it away first on the success path, so this Remove then just no-ops
	// (matching SaveDecision's own defer-then-rename pattern).
	defer os.Remove(tmpName)

	// LimitReader, not a bare io.Copy(tmp, in): bounds the read even if
	// src grows between the Stat() above and this copy (e.g. still open
	// for append by a not-yet-terminated writer) -- the size check above
	// guards the common case cheaply, this guards the race. +1 so a file
	// that lands exactly on the limit isn't silently truncated into
	// looking like a smaller, complete report.
	written, err := io.Copy(tmp, io.LimitReader(in, maxRetainFileSize+1))
	if err != nil {
		tmp.Close()
		return fmt.Errorf("copy %s to %s: %w", src, dst, err)
	}
	if written > maxRetainFileSize {
		tmp.Close()
		return fmt.Errorf("%s grew past the %d byte retention limit while being copied", src, maxRetainFileSize)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file for %s: %w", dst, err)
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return fmt.Errorf("rename temp file into %s: %w", dst, err)
	}
	return nil
}

// ReadHostileFile reads src (agent-controlled content from an untrusted
// sandboxed worker's own workspace) into memory, bounded at maxSize and
// safe against the same symlink/named-pipe/device-node substitution
// RetainFile's own doc comment documents finding live -- unlike RetainFile,
// this returns the content itself rather than archiving a copy, for a
// caller that needs to parse it (e.g. cmd/factoryd's own loadAgentEvidence,
// json.Unmarshal-ing BUILD_EVIDENCE.json into run.AgentEvidence): that file
// sits in the exact same untrusted workspace BUILD_REPORT.md does, written
// by the same untrusted build, but its own read path was a bare
// os.ReadFile with none of RetainFile's protections until this function
// existed -- an untrusted build could substitute it with a symlink to an
// arbitrary host-readable file (silently persisted into this run's own
// durable AgentEvidence if that target happened to parse as valid JSON), a
// FIFO with no writer (hanging this process indefinitely, the same bare
// open() hang RetainFile's own O_NONBLOCK fix closes), or a many-gigabyte
// regular file (an unbounded memory read).
func ReadHostileFile(src string, maxSize int64) ([]byte, error) {
	in, err := openHostileRegularFile(src, maxSize)
	if err != nil {
		return nil, err
	}
	defer in.Close()

	// LimitReader, not a bare io.ReadAll(in): bounds the read even if src
	// grows between the Stat() inside openHostileRegularFile and this read
	// (the same TOCTOU race RetainFile's own identical +1/limit-check
	// pairing guards against). +1 so a file landing exactly on the limit
	// is reported as over it, not silently truncated into looking like a
	// smaller, complete file.
	data, err := io.ReadAll(io.LimitReader(in, maxSize+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", src, err)
	}
	if int64(len(data)) > maxSize {
		return nil, fmt.Errorf("%s grew past the %d byte limit while being read", src, maxSize)
	}
	return data, nil
}

// SHA256Bytes returns the hex-encoded SHA-256 digest of content already
// held in memory. Prefer this over SHA256File whenever the caller has
// already read the file once: hashing a second, independent read of the
// same path races a concurrent modification between the two reads, so the
// persisted digest could describe different bytes than the ones actually
// used (see cmd/factoryd/main.go's ProjectCheckResult.SHA256, whose whole
// purpose is binding a check result to the exact content that produced it).
func SHA256Bytes(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

// SHA256Tree returns a single hex-encoded SHA-256 digest over every
// regular file under dir, added for the reference-oracle content hash
// (safety-contract.md SC-012: "oracle ... hashes are recorded with the
// attempt" -- follow-up from the reference_oracle gate's PR #151 review,
// round 2). Deterministic regardless of the filesystem's own directory
// iteration order: every relative path is collected first, sorted, then
// each file's path and content are fed into one running hash in that
// fixed order — the same two-pass shape SHA256File already uses per
// file, extended across a directory instead of assuming a single one.
//
// A path and a length-prefix-style newline separator go into the hash
// alongside each file's bytes so that (for example) a file renamed but
// otherwise byte-identical produces a different digest than the
// original layout — the whole point is proving WHICH content produced a
// result, not just that some content with this byte sum exists
// somewhere under dir.
//
// Refuses (rather than silently skips) anything that is not a regular
// file or plain directory — a symlink in particular could otherwise let
// the hashed tree diverge from what a later read of the same path
// actually returns.
func SHA256Tree(dir string) (string, error) {
	// dir itself needs the same symlink refusal every descendant gets
	// below, checked explicitly here rather than folded into the
	// WalkDir callback: filepath.WalkDir never descends into a symlinked
	// root at all (found via review) -- the callback's own "path == dir"
	// branch used to return nil unconditionally for the root entry,
	// before ever reaching the symlink check that runs for every other
	// entry, so a symlinked dir silently hashed as an empty tree instead
	// of being refused. Lstat, not Stat: Stat would follow the symlink
	// and see the real target's mode, defeating the exact check this
	// guards.
	if info, err := os.Lstat(dir); err != nil {
		return "", fmt.Errorf("stat %s: %w", dir, err)
	} else if info.Mode()&fs.ModeSymlink != 0 {
		return "", fmt.Errorf("%s is a symlink, not a plain directory -- resolve it (e.g. filepath.EvalSymlinks) before hashing", dir)
	}

	var relPaths []string
	if err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == dir {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%s is a symlink, not a regular file or directory", path)
		}
		if d.IsDir() {
			return nil
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%s is not a regular file", path)
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		relPaths = append(relPaths, rel)
		return nil
	}); err != nil {
		return "", fmt.Errorf("walk %s: %w", dir, err)
	}
	sort.Strings(relPaths)

	// Each entry is framed with explicit, fixed-width length prefixes
	// (path length, then content length) rather than newline delimiters
	// (found via review): a newline-delimited scheme is ambiguous --
	// e.g. one file "a" containing the bytes "x\nb\ny" and two files "a"
	// = "x" / "b" = "y" both feed the identical byte stream "a\nx\nb\ny\n"
	// into the hash, so genuinely different trees could share a digest
	// without SHA-256 itself ever colliding. Lengths make every entry's
	// boundary unambiguous regardless of what bytes the path or content
	// contain.
	h := sha256.New()
	var lenPrefix [8]byte
	writeLenPrefixed := func(b []byte) error {
		binary.BigEndian.PutUint64(lenPrefix[:], uint64(len(b)))
		if _, err := h.Write(lenPrefix[:]); err != nil {
			return err
		}
		_, err := h.Write(b)
		return err
	}
	for _, rel := range relPaths {
		if err := writeLenPrefixed([]byte(rel)); err != nil {
			return "", fmt.Errorf("hash path %s: %w", rel, err)
		}
		full := filepath.Join(dir, rel)
		info, err := os.Stat(full)
		if err != nil {
			return "", fmt.Errorf("stat %s: %w", rel, err)
		}
		size := info.Size()
		binary.BigEndian.PutUint64(lenPrefix[:], uint64(size))
		if _, err := h.Write(lenPrefix[:]); err != nil {
			return "", fmt.Errorf("hash content length for %s: %w", rel, err)
		}
		f, err := os.Open(full)
		if err != nil {
			return "", fmt.Errorf("open %s: %w", rel, err)
		}
		// CopyN, not Copy: enforces that exactly the size just prefixed
		// is what gets hashed, rather than silently hashing however many
		// bytes happen to be there if the file changed size between the
		// Stat above and this read.
		_, copyErr := io.CopyN(h, f, size)
		closeErr := f.Close()
		if copyErr != nil {
			return "", fmt.Errorf("read %s: %w", rel, copyErr)
		}
		if closeErr != nil {
			return "", fmt.Errorf("close %s: %w", rel, closeErr)
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// SnapshotTree copies every regular file under src into dst (creating
// dst and any needed parent directories, preserving relative paths),
// refusing symlinks the same way SHA256Tree does — added to close a
// real TOCTOU gap (found via review): hashing src and then separately
// mounting src leaves a window, potentially spanning the mount's whole
// lifetime, where an external host-side process could change src's
// content out from under either operation. A hash proven against one
// set of bytes and a mount exposing a different set could then both be
// true of the same nominal "-reference-oracle-dir" at once, silently
// breaking the guarantee this whole mechanism exists to provide. The
// caller hashes and mounts the COPY SnapshotTree produces, not src, so
// both operations act on one fixed, immutable snapshot instead of two
// independent reads of a live directory.
//
// dst must not already exist (refuses rather than merging into or
// silently overwriting a caller's existing directory) -- callers create
// a fresh, unique staging path (e.g. under a run's own log directory or
// via os.MkdirTemp) precisely so this can never collide with anything
// else.
//
// Every directory and file the copy creates is chmod'd explicitly
// (rather than relying on the mode passed to MkdirAll/OpenFile, which
// the process umask can silently strip bits from) to snapshotDirMode /
// snapshotFileMode: owner rwx/rw plus group r-x/r, nothing for other.
// The snapshot only ever needs to be *read*, by the sandboxed gate
// container that mounts it -- that container runs as
// sandbox.DefaultWorkerUID with the factoryd host process's own primary
// group (sandbox.ResolveDefaultWorkerIdentity), the same group under
// which factoryd itself creates these files/directories by default, so
// group-read is sufficient without any explicit chown (found via
// review, PR #152 round 2: the original 0700/0600 modes left the
// snapshot readable only by factoryd's own UID, so the container -- a
// different UID by deliberate worker/host separation -- got a
// permission-denied reading its own reference-oracle mount on every
// real run).
func SnapshotTree(src, dst string) error {
	const (
		snapshotDirMode  = 0o750
		snapshotFileMode = 0o640
	)
	if _, err := os.Stat(dst); err == nil {
		return fmt.Errorf("snapshot destination %s already exists", dst)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat %s: %w", dst, err)
	}
	if info, err := os.Lstat(src); err != nil {
		return fmt.Errorf("stat %s: %w", src, err)
	} else if info.Mode()&fs.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink, not a plain directory -- resolve it (e.g. filepath.EvalSymlinks) before snapshotting", src)
	} else if !info.IsDir() {
		// Without this check, WalkDir below still "succeeds": called
		// against a non-directory src, it invokes the walk callback
		// exactly once for src itself, which the path == src branch
		// then skips (that branch exists to skip re-creating the root
		// as a child of itself) -- producing an empty dst and a nil
		// error instead of any indication -reference-oracle-dir named
		// the wrong kind of thing (found via review, PR #152 round 2).
		return fmt.Errorf("%s is not a directory", src)
	}
	mkdir := func(path string) error {
		if err := os.MkdirAll(path, snapshotDirMode); err != nil {
			return err
		}
		return os.Chmod(path, snapshotDirMode)
	}
	if err := mkdir(dst); err != nil {
		return fmt.Errorf("create snapshot destination %s: %w", dst, err)
	}
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == src {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%s is a symlink, not a regular file or directory", path)
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return mkdir(target)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%s is not a regular file", path)
		}
		if err := mkdir(filepath.Dir(target)); err != nil {
			return err
		}
		in, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("open %s: %w", path, err)
		}
		defer in.Close()
		out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, snapshotFileMode)
		if err != nil {
			return fmt.Errorf("create %s: %w", target, err)
		}
		_, copyErr := io.Copy(out, in)
		closeErr := out.Close()
		if copyErr != nil {
			return fmt.Errorf("copy %s: %w", rel, copyErr)
		}
		if closeErr != nil {
			return closeErr
		}
		// Explicit chmod, not just the mode passed to OpenFile above:
		// the process umask can silently strip the group-read bit
		// OpenFile requested, the same reason mkdir above chmod's
		// directories explicitly.
		return os.Chmod(target, snapshotFileMode)
	})
}
