package request

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"buildgate/internal/oraclecanary"
)

// Server-side plumbing for the oracle review API (GET/PUT
// /requests/{id}/oracle...): list, read and edit the request-level oracle/
// directory at oracle_review. Lives beside approve.go, not in it, and applies
// the same rules approval applies (flat, regular files only, no stray files,
// RUN_COMMAND.txt passing ValidateOracleRunCommand, oraclecanary.CheckDir) so
// what the operator sees here is what approval would accept or refuse.

// MaxOracleFileReadBytes caps the content GetOracleFile will return.
const MaxOracleFileReadBytes = 256 << 10

// MaxOracleRunCommandBytes caps a RUN_COMMAND.txt written through the API.
const MaxOracleRunCommandBytes = 8 << 10

var (
	// ErrOracleFileName: the name is not a servable oracle file name.
	ErrOracleFileName = errors.New("invalid oracle file name")
	// ErrOracleFileNotFound: no such regular file in oracle/.
	ErrOracleFileNotFound = errors.New("oracle file not found")
	// ErrOracleFileTooLarge: the file exceeds MaxOracleFileReadBytes.
	ErrOracleFileTooLarge = errors.New("oracle file too large to serve")
	// ErrOracleNotEditable: only RUN_COMMAND.txt is writable through the API.
	ErrOracleNotEditable = errors.New("only RUN_COMMAND.txt is editable through the API")
	// ErrOracleWrongState: the edit is only allowed at oracle_review.
	ErrOracleWrongState = errors.New("oracle files can only be edited at oracle_review")
	// ErrOracleInvalid: the proposed content failed validation (message says why).
	ErrOracleInvalid = errors.New("invalid oracle content")
	// ErrOracleRunCommandStale: SetOracleRunCommand's own baseSHA256 argument
	// didn't match RUN_COMMAND.txt's current sha256, checked under the same
	// request lock the write itself takes (fixed after an adversarial
	// review, 2026-09-24) -- see SetOracleRunCommand's own doc comment.
	// errors.As against *OracleRunCommandStaleError recovers the real current sha256
	// (OracleRunCommandStaleError.Current) for a 409 response body, the same
	// shape internal/api's own verifyBaseSHA256 returns for the spec/ticket
	// PUT routes.
	ErrOracleRunCommandStale = errors.New("oracle run command changed since it was fetched")
)

// OracleRunCommandStaleError is ErrOracleRunCommandStale's own concrete
// type, carrying the file's real current sha256 at the moment of the
// mismatch (computed under the same lock as the check, so it is never
// stale itself by the time it reaches the caller).
type OracleRunCommandStaleError struct {
	Current string
}

func (e *OracleRunCommandStaleError) Error() string {
	return fmt.Sprintf("%s (current sha256 %s)", ErrOracleRunCommandStale, e.Current)
}

func (e *OracleRunCommandStaleError) Is(target error) bool {
	return target == ErrOracleRunCommandStale
}

// OracleFile is one servable file of the request-level oracle directory.
type OracleFile struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// OracleListing is the read-only view of oracle/ an operator reviews.
type OracleListing struct {
	Files []OracleFile `json:"files"`
	// Problems names everything approval would refuse, so the operator sees
	// why; nothing the approval would refuse is dropped silently.
	Problems []string `json:"problems"`
}

// ValidOracleFileName reports whether name may be served or written: one path
// element, no NUL, no leading dot, nothing approval's stray-file check refuses.
func ValidOracleFileName(name string) bool {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\\x00") {
		return false
	}
	return !strayOracleFileName(name)
}

func oracleDirPath(dataDir, id string) string {
	return filepath.Join(Dir(dataDir, id), RequestOracleDirName)
}

// readRegularNoFollow reads path only if it is a regular file, never
// following a symlink (O_NOFOLLOW plus an fstat on the opened descriptor).
func readRegularNoFollow(path string, limit int64) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrOracleFileNotFound
		}
		return nil, fmt.Errorf("%w: %v", ErrOracleFileNotFound, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, ErrOracleFileNotFound
	}
	if limit > 0 && info.Size() > limit {
		return nil, ErrOracleFileTooLarge
	}
	return readAtMost(f, limit)
}

// readAtMost reads r, refusing (ErrOracleFileTooLarge) more than limit bytes
// when limit > 0. The bound is enforced on the bytes actually read, not on an
// earlier fstat, so a file growing between the two cannot be read unbounded.
func readAtMost(r io.Reader, limit int64) ([]byte, error) {
	if limit <= 0 {
		return io.ReadAll(r)
	}
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, ErrOracleFileTooLarge
	}
	return b, nil
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// ListOracle lists the request-level oracle/ files with hashes and every
// problem approval would refuse. A missing directory is an empty listing.
// Read-only and lock-free: like GET /requests/{id}, a view of the moment.
func ListOracle(dataDir, id string) (*OracleListing, error) {
	listing, state, err := listOracleDir(dataDir, id, RequestOracleDirName)
	if err != nil || state != oracleDirPopulated {
		return listing, err
	}
	// The shared approval check is a superset of the per-file problems
	// (manifest, spec numbering, canary), so the listing shows everything
	// approval would refuse.
	tooLarge := []string{}
	for _, p := range listing.Problems {
		if strings.Contains(p, "is too large to serve") {
			tooLarge = append(tooLarge, p)
		}
	}
	listing.Problems = append(append([]string{}, ValidateRequestOracleDir(dataDir, id)...), tooLarge...)
	return listing, nil
}

type oracleDirState int

const (
	oracleDirAbsent    oracleDirState = iota // nothing to list
	oracleDirUnsafe                          // a component is a symlink or not a directory
	oracleDirPresent                         // every component is a real directory
	oracleDirEmpty                           // present, no entries
	oracleDirPopulated                       // present, has entries
)

// resolveOracleDir walks rel from the request's own directory, Lstat-ing every
// component: O_NOFOLLOW on a leaf file does not stop an intermediate symlink
// (tickets, <NNN>.oracle, oracle) from redirecting the read anywhere the daemon
// can read, so any component that is a symlink or not a directory is refused.
func resolveOracleDir(dataDir, id string, rel ...string) (string, oracleDirState, string, error) {
	path := Dir(dataDir, id)
	for _, part := range append([]string{""}, rel...) {
		path = filepath.Join(path, part)
		info, err := os.Lstat(path)
		if err != nil {
			if os.IsNotExist(err) {
				return path, oracleDirAbsent, "", nil
			}
			return path, oracleDirAbsent, "", err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return path, oracleDirUnsafe, filepath.Base(path) + " is a symlink or not a directory", nil
		}
	}
	return path, oracleDirPresent, "", nil
}

// listOracleDir lists the directory rel under the request with hashes and the
// per-file problems approval's stray-file / RUN_COMMAND.txt checks would raise.
// The state says whether the directory is absent, unsafe, empty or populated
// (only a populated one has anything more to check).
func listOracleDir(dataDir, id string, rel ...string) (*OracleListing, oracleDirState, error) {
	listing := &OracleListing{Files: []OracleFile{}, Problems: []string{}}
	dir, state, problem, err := resolveOracleDir(dataDir, id, rel...)
	if err != nil {
		return nil, state, err
	}
	if state == oracleDirUnsafe {
		listing.Problems = append(listing.Problems, problem)
		return listing, state, nil
	}
	if state == oracleDirAbsent {
		return listing, state, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, state, err
	}
	if len(entries) == 0 {
		return listing, oracleDirEmpty, nil
	}
	hasRunCommand := false
	for _, entry := range entries {
		name := entry.Name()
		switch {
		case entry.IsDir():
			listing.Problems = append(listing.Problems, fmt.Sprintf("%s is a subdirectory -- the oracle directory must be flat", name))
		case strayOracleFileName(name):
			listing.Problems = append(listing.Problems, fmt.Sprintf("%s looks like an editor backup, swap or hidden file -- remove it before approving", name))
		case !entry.Type().IsRegular():
			listing.Problems = append(listing.Problems, fmt.Sprintf("%s is not a regular file (symlink or special file)", name))
		default:
			content, err := readRegularNoFollow(filepath.Join(dir, name), readLimitFor(name))
			if errors.Is(err, ErrOracleFileTooLarge) {
				size := int64(0)
				if info, ierr := entry.Info(); ierr == nil {
					size = info.Size()
				}
				listing.Problems = append(listing.Problems, fmt.Sprintf("%s is too large to serve (%d bytes)", name, size))
				continue
			}
			if err != nil {
				listing.Problems = append(listing.Problems, fmt.Sprintf("%s could not be read as a regular file", name))
				continue
			}
			listing.Files = append(listing.Files, OracleFile{Name: name, Size: int64(len(content)), SHA256: sha256Hex(content)})
			if name == TicketOracleRunCommandFilename {
				hasRunCommand = true
				if err := ValidateOracleRunCommand(string(content)); err != nil {
					listing.Problems = append(listing.Problems, fmt.Sprintf("%s: %v", name, err))
				}
			}
		}
	}
	sort.Slice(listing.Files, func(i, j int) bool { return listing.Files[i].Name < listing.Files[j].Name })
	if !hasRunCommand {
		listing.Problems = append(listing.Problems, fmt.Sprintf("no %s -- add it, or remove the directory to skip the oracle, before approving", TicketOracleRunCommandFilename))
	}
	return listing, oracleDirPopulated, nil
}

// readLimitFor is the read cap for an oracle file: MANIFEST.json may be as
// large as validation accepts, everything else is capped at MaxOracleFileReadBytes.
func readLimitFor(name string) int64 {
	if name == ManifestFileName {
		return maxOracleManifestBytes
	}
	return MaxOracleFileReadBytes
}

// readOracleFile reads one file of the directory rel under the request, after
// resolveOracleDir has vetted every component.
func readOracleFile(dataDir, id, name string, rel ...string) ([]byte, string, error) {
	if !ValidOracleFileName(name) {
		return nil, "", ErrOracleFileName
	}
	dir, state, _, err := resolveOracleDir(dataDir, id, rel...)
	if err != nil {
		return nil, "", err
	}
	if state != oracleDirPresent {
		return nil, "", ErrOracleFileNotFound
	}
	content, err := readRegularNoFollow(filepath.Join(dir, name), readLimitFor(name))
	if err != nil {
		return nil, "", err
	}
	return content, sha256Hex(content), nil
}

// GetOracleFile returns one oracle file's bytes and sha256. Refuses names
// approval would refuse, symlinks (the file or any directory above it),
// non-regular files and files over the read cap.
func GetOracleFile(dataDir, id, name string) ([]byte, string, error) {
	return readOracleFile(dataDir, id, name, RequestOracleDirName)
}

// validateOracleRunCommandText applies the write-time checks: non-empty, no
// NUL, size-capped, one logical command (backslash continuations join lines;
// blank and comment-only lines do not count), then the same static check
// approval runs.
func validateOracleRunCommandText(content string) error {
	if len(content) > MaxOracleRunCommandBytes {
		return fmt.Errorf("%w: command is over %d bytes", ErrOracleInvalid, MaxOracleRunCommandBytes)
	}
	if strings.ContainsRune(content, 0) {
		return fmt.Errorf("%w: command contains a NUL byte", ErrOracleInvalid)
	}
	if strings.TrimSpace(content) == "" {
		return fmt.Errorf("%w: command is empty", ErrOracleInvalid)
	}
	if err := ValidateOracleRunCommand(content); err != nil {
		return fmt.Errorf("%w: %v", ErrOracleInvalid, err)
	}
	return nil
}

// SetOracleRunCommand replaces (or creates) oracle/RUN_COMMAND.txt, only at
// oracle_review, holding the same request lock approval holds so an edit
// cannot interleave with an approval's hash-then-pin.
//
// If baseSHA256 is non-empty, it must match the current file's sha256 (an
// absent file hashes as empty content, matching putRequestOracleFile's
// own prior convention) -- checked inside this same lock, not by a
// separate read-then-lock at the API layer (an adversarial review,
// 2026-09-24, found: internal/api's own putRequestOracleFile used to
// call GetOracleFile for this check, then SetOracleRunCommand separately,
// leaving a window between the read and the lock where a concurrent
// PUT -- or an approval's own hash-then-pin -- could change the file out
// from under the check it had already passed). A mismatch returns
// *OracleRunCommandStaleError (wraps ErrOracleRunCommandStale) naming the
// real current sha256, mirroring verifyBaseSHA256's own "content changed
// since it was fetched" response for the spec/ticket PUT routes.
//
// Validation runs after the hash check; on any failure nothing is
// written. Returns the new file's sha256.
func SetOracleRunCommand(dataDir, id, name, content, baseSHA256 string) (string, error) {
	if name != TicketOracleRunCommandFilename {
		return "", ErrOracleNotEditable
	}
	unlock, err := Lock(dataDir, id)
	if err != nil {
		return "", err
	}
	defer unlock()
	r, err := Load(dataDir, id)
	if err != nil {
		return "", err
	}
	if r.State != StateOracleReview {
		return "", fmt.Errorf("%w: request %s is in state %q", ErrOracleWrongState, id, r.State)
	}
	if baseSHA256 != "" {
		current, _, err := readOracleFile(dataDir, id, name, RequestOracleDirName)
		if err != nil && !errors.Is(err, ErrOracleFileNotFound) {
			return "", err
		}
		currentSum := sha256Hex(current) // current is nil (hashes as empty) when the file doesn't exist yet
		if !strings.EqualFold(baseSHA256, currentSum) {
			return "", &OracleRunCommandStaleError{Current: currentSum}
		}
	}
	if err := validateOracleRunCommandText(content); err != nil {
		return "", err
	}
	dir := oracleDirPath(dataDir, id)
	info, err := os.Lstat(dir)
	switch {
	case err == nil && !info.IsDir():
		return "", fmt.Errorf("%w: %s is not a directory", ErrOracleInvalid, RequestOracleDirName)
	case err != nil && !os.IsNotExist(err):
		return "", err
	}
	// The directory's other files are deliberately not validated here
	// (approval enforces the canary check): the command must stay editable
	// however broken the rest of oracle/ is, so the operator can always
	// repair it through the API.
	target := filepath.Join(dir, name)
	if ti, terr := os.Lstat(target); terr == nil && ti.IsDir() {
		return "", fmt.Errorf("%w: %s is a directory", ErrOracleInvalid, name)
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", err
	}
	tmpName, err := writeOracleTemp(Dir(dataDir, id), content)
	if err != nil {
		return "", err
	}
	// Removed on every path; after a successful rename it no longer exists.
	defer os.Remove(tmpName)
	if oracleBeforeRename != nil {
		oracleBeforeRename()
	}
	if err := os.Rename(tmpName, target); err != nil {
		return "", err
	}
	return sha256Hex([]byte(content)), nil
}

// oracleBeforeRename is a test seam run between the temp write and the rename,
// the window a crash would leave a temp file in.
var oracleBeforeRename func()

// writeOracleTemp writes content (mode 0600, fsynced) to a temp file in
// requestDir -- the same filesystem as oracle/ but never inside it, so a crash
// before the rename cannot leave a stray file that approval refuses and the
// API cannot delete.
func writeOracleTemp(requestDir, content string) (string, error) {
	tmp, err := os.CreateTemp(requestDir, ".oracle-run-command-*.tmp")
	if err != nil {
		return "", err
	}
	name := tmp.Name()
	fail := func(err error) (string, error) {
		_ = tmp.Close()
		_ = os.Remove(name)
		return "", err
	}
	if _, err := tmp.WriteString(content); err != nil {
		return fail(err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		return fail(err)
	}
	if err := tmp.Sync(); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return "", err
	}
	return name, nil
}

// ticketOracleDirRel is the request-relative path components of the
// materialized oracle directory of the ticket whose spec file (bare name, e.g.
// "001.spec.md") lives in the request's tickets/.
func ticketOracleDirRel(specFile string) []string {
	return []string{"tickets", filepath.Base(TicketOracleDir(specFile))}
}

// ListTicketOracle lists a ticket's own <NNN>.oracle/ files (the ones plan
// approval hash-pins) with the problems approval would refuse for them --
// including an existing but empty directory, which approval refuses too.
func ListTicketOracle(dataDir, id, specFile string) (*OracleListing, error) {
	rel := ticketOracleDirRel(specFile)
	listing, state, err := listOracleDir(dataDir, id, rel...)
	if err == nil && state == oracleDirPopulated && len(listing.Problems) == 0 {
		// Approval's last check: the runtime canary must be buildable for
		// this directory (stray files, a helper test with no TestOracle*...).
		dir, _, _, _ := resolveOracleDir(dataDir, id, rel...)
		if cerr := oraclecanary.CheckDir(dir); cerr != nil {
			listing.Problems = append(listing.Problems, cerr.Error())
		}
	}
	if err == nil && state == oracleDirEmpty {
		listing.Problems = append(listing.Problems, fmt.Sprintf("no %s -- add it, or remove the directory to skip the oracle, before approving", TicketOracleRunCommandFilename))
	}
	return listing, err
}

// GetTicketOracleFile is GetOracleFile for a ticket's <NNN>.oracle/ directory.
func GetTicketOracleFile(dataDir, id, specFile, name string) ([]byte, string, error) {
	return readOracleFile(dataDir, id, name, ticketOracleDirRel(specFile)...)
}
