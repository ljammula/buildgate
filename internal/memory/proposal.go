package memory

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unicode/utf8"
)

// The proposal file is the one place this package touches the filesystem. It
// records, for one memory request, the exact AGENTS.md text the factory
// rendered when it proposed the change; the host releases a memory run only
// when the run's AGENTS.md hashes to ExpectedSHA256.
const (
	proposalSchemaVersion = 1
	// MaxProposalBytes bounds the file read back: a rendered AGENTS.md of a
	// repository plus the JSON around it.
	MaxProposalBytes = 256 << 10
	maxComponentLen  = 200
)

// Proposal is a memory request's approved AGENTS.md text.
type Proposal struct {
	SchemaVersion int      `json:"schema_version"`
	RequestID     string   `json:"request_id"`
	LessonIDs     []string `json:"lesson_ids"`
	// BaseBlobSHA256 is the hash of AGENTS.md at the commit the proposal was
	// rendered from, "" when the file did not exist there.
	BaseBlobSHA256 string `json:"base_blob_sha256"`
	ExpectedSHA256 string `json:"expected_sha256"`
	// Expected is the full AGENTS.md text.
	Expected string `json:"expected"`
}

// HashHex is the lowercase hex SHA-256 of b.
func HashHex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// safeComponent reports whether s can be one path component: not empty, not
// "." or "..", no separator, NUL or control character, valid UTF-8, bounded.
func safeComponent(s string) bool {
	if s == "" || s == "." || s == ".." || len(s) > maxComponentLen || !utf8.ValidString(s) {
		return false
	}
	if strings.ContainsAny(s, `/\`) {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// ProposalPath is <dataDir>/memory/<project>/proposals/<requestID>.json.
// project and requestID must each be one safe path component.
func ProposalPath(dataDir, project, requestID string) (string, error) {
	if dataDir == "" {
		return "", errors.New("memory: proposal path needs a data directory")
	}
	if !safeComponent(project) {
		return "", fmt.Errorf("memory: project %q is not a single safe path component", project)
	}
	if !safeComponent(requestID) {
		return "", fmt.Errorf("memory: request id %q is not a single safe path component", requestID)
	}
	return filepath.Join(dataDir, "memory", project, "proposals", requestID+".json"), nil
}

// LoadProposal reads the proposal at path, which must be named
// <request_id>.json for the request_id the file itself records (ProposalPath
// names it so). A missing file is (zero, false, nil). Everything else short of
// one usable proposal is an error: a path or a parent directory that is a
// symlink, a file that is not a regular file (a FIFO is never opened for
// reading its content), one that is too large, has an unknown field or a wrong
// schema version, whose expected_sha256 is not the hash of expected, or whose
// request_id is not the one the path names.
func LoadProposal(path string) (Proposal, bool, error) {
	if err := realDirectory(filepath.Dir(path)); errors.Is(err, os.ErrNotExist) {
		return Proposal{}, false, nil
	} else if err != nil {
		return Proposal{}, false, err
	}
	data, found, err := readRegularFile(path)
	if err != nil || !found {
		return Proposal{}, false, err
	}
	p, err := decodeProposal(data)
	if err != nil {
		return Proposal{}, false, err
	}
	if want := strings.TrimSuffix(filepath.Base(path), ".json"); p.RequestID != want || filepath.Base(path) != want+".json" {
		return Proposal{}, false, fmt.Errorf("memory: proposal records request %q, not the request %q it was read for", p.RequestID, want)
	}
	return p, true, nil
}

// LoadProposalFor reads the proposal of one request of one project under
// dataDir, as LoadProposal does, and also requires every directory between
// dataDir and the file to be a real directory.
func LoadProposalFor(dataDir, project, requestID string) (Proposal, bool, error) {
	path, err := ProposalPath(dataDir, project, requestID)
	if err != nil {
		return Proposal{}, false, err
	}
	for _, dir := range []string{filepath.Join(dataDir, "memory"), filepath.Join(dataDir, "memory", project)} {
		if err := realDirectory(dir); errors.Is(err, os.ErrNotExist) {
			return Proposal{}, false, nil
		} else if err != nil {
			return Proposal{}, false, err
		}
	}
	return LoadProposal(path)
}

// realDirectory is nil when dir is a directory and not a link to one.
func realDirectory(dir string) error {
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("memory: proposal directory: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("memory: proposal directory %s is not a real directory", filepath.Base(dir))
	}
	return nil
}

// readRegularFile reads path when it is a regular file and nothing else: it
// is checked before the open, opened without following a link and without
// blocking, and checked again on the open file. A missing path is
// (nil, false, nil).
func readRegularFile(path string) ([]byte, bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("memory: stat proposal: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, false, errors.New("memory: proposal is not a regular file")
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, false, fmt.Errorf("memory: open proposal: %w", err)
	}
	defer f.Close()
	if opened, err := f.Stat(); err != nil || !opened.Mode().IsRegular() {
		return nil, false, errors.New("memory: proposal is not a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxProposalBytes+1))
	if err != nil {
		return nil, false, fmt.Errorf("memory: read proposal: %w", err)
	}
	if len(data) > MaxProposalBytes {
		return nil, false, fmt.Errorf("memory: proposal is larger than %d bytes", MaxProposalBytes)
	}
	return data, true, nil
}

func decodeProposal(data []byte) (Proposal, error) {
	var p Proposal
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return Proposal{}, fmt.Errorf("memory: decode proposal: %w", err)
	}
	if dec.More() {
		return Proposal{}, errors.New("memory: proposal has data after its JSON object")
	}
	if p.SchemaVersion != proposalSchemaVersion {
		return Proposal{}, fmt.Errorf("memory: proposal schema_version %d, want %d", p.SchemaVersion, proposalSchemaVersion)
	}
	if want := HashHex([]byte(p.Expected)); p.ExpectedSHA256 != want {
		return Proposal{}, errors.New("memory: proposal expected_sha256 is not the hash of its expected text")
	}
	return p, nil
}

// SaveProposal writes p at path atomically (temp file and rename), 0600 in a
// 0700 directory. It fills schema_version and expected_sha256 from Expected.
func SaveProposal(path string, p Proposal) error {
	p.SchemaVersion = proposalSchemaVersion
	p.ExpectedSHA256 = HashHex([]byte(p.Expected))
	if p.LessonIDs == nil {
		p.LessonIDs = []string{}
	}
	data, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("memory: encode proposal: %w", err)
	}
	if len(data) > MaxProposalBytes {
		return fmt.Errorf("memory: proposal is larger than %d bytes", MaxProposalBytes)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("memory: create proposal directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".proposal-*.tmp")
	if err != nil {
		return fmt.Errorf("memory: create temp proposal: %w", err)
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("memory: chmod temp proposal: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("memory: write temp proposal: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("memory: sync temp proposal: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("memory: close temp proposal: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("memory: install proposal: %w", err)
	}
	return nil
}
