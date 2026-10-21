package projectconfig

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
)

// approvalDir is the directory under the data dir that holds one approval
// file per repository.
const approvalDir = "sandbox-approvals"

const approvalSchemaVersion = 1

// Approval records that an operator approved one Sandbox block for one
// repository.
type Approval struct {
	SandboxSHA256 string `json:"sandbox_sha256"`
	// Commit is the commit the block was read from when it was approved.
	Commit     string `json:"commit"`
	ApprovedBy string `json:"approved_by"`
	ApprovedAt string `json:"approved_at"`
	// Block is the block as the operator saw it (Sandbox.Render).
	Block string `json:"block"`
}

type approvalFile struct {
	SchemaVersion int        `json:"schema_version"`
	RepoKey       string     `json:"repo_key"`
	Approvals     []Approval `json:"approvals"`
}

// ApprovalStore holds the operator's approvals of sandbox blocks under
// <data-dir>/sandbox-approvals/<repo key>.json (mode 0600). A repository is
// named by its key (memory.StoreKey of its project and root path). An
// approval binds that key and the block's hash: an approval for one
// repository or one hash never covers another. A missing, unreadable or
// corrupt file approves nothing.
type ApprovalStore struct {
	dir string
}

var approvalMu sync.Mutex

// OpenApprovals returns the store under dataDir. It creates nothing.
func OpenApprovals(dataDir string) ApprovalStore {
	return ApprovalStore{dir: filepath.Join(dataDir, approvalDir)}
}

func (s ApprovalStore) path(repoKey string) (string, error) {
	if repoKey == "" || repoKey == "." || repoKey == ".." || repoKey != filepath.Base(repoKey) || strings.ContainsAny(repoKey, "\\") ||
		strings.ContainsFunc(repoKey, unicode.IsControl) {
		return "", fmt.Errorf("sandbox approvals: %q is not a repository key", repoKey)
	}
	return filepath.Join(s.dir, repoKey+".json"), nil
}

// Path is the file that holds the repository's approvals.
func (s ApprovalStore) Path(repoKey string) string {
	p, err := s.path(repoKey)
	if err != nil {
		return filepath.Join(s.dir, "(invalid repository key).json")
	}
	return p
}

// readStrict returns the repository's approvals. A file that does not exist
// holds none; one that exists but cannot be read as this repository's
// approvals is an error naming it.
func (s ApprovalStore) readStrict(repoKey string) ([]Approval, error) {
	p, err := s.path(repoKey)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("sandbox approvals: read %s: %w", p, err)
	}
	var f approvalFile
	if err := json.Unmarshal(data, &f); err != nil || f.SchemaVersion != approvalSchemaVersion || f.RepoKey != repoKey {
		return nil, fmt.Errorf("sandbox approvals: %s exists but is not this repository's approval file; fix or delete it, then approve again", p)
	}
	return f.Approvals, nil
}

// read returns the repository's approvals; a file that is absent, unreadable,
// corrupt or written for another key yields none.
func (s ApprovalStore) read(repoKey string) []Approval {
	list, err := s.readStrict(repoKey)
	if err != nil {
		return nil
	}
	return list
}

// Approved returns the approval of hash for the repository, if there is one.
func (s ApprovalStore) Approved(repoKey, hash string) (Approval, bool) {
	for _, a := range s.read(repoKey) {
		if a.SandboxSHA256 == hash && hash != "" {
			return a, true
		}
	}
	return Approval{}, false
}

// List returns the repository's approvals, oldest first.
func (s ApprovalStore) List(repoKey string) []Approval { return s.read(repoKey) }

// Approve records a (replacing an earlier approval of the same hash). Older
// approvals stay valid.
func (s ApprovalStore) Approve(repoKey string, a Approval) error {
	if a.SandboxSHA256 == "" {
		return errors.New("sandbox approvals: no hash to approve")
	}
	if a.ApprovedAt == "" {
		a.ApprovedAt = time.Now().UTC().Format(time.RFC3339)
	}
	return s.update(repoKey, func(list []Approval) []Approval {
		out := make([]Approval, 0, len(list)+1)
		for _, e := range list {
			if e.SandboxSHA256 != a.SandboxSHA256 {
				out = append(out, e)
			}
		}
		return append(out, a)
	})
}

// Revoke removes the approval of hash and reports whether there was one.
func (s ApprovalStore) Revoke(repoKey, hash string) (bool, error) {
	removed := false
	err := s.update(repoKey, func(list []Approval) []Approval {
		out := make([]Approval, 0, len(list))
		for _, e := range list {
			if e.SandboxSHA256 == hash {
				removed = true
				continue
			}
			out = append(out, e)
		}
		return out
	})
	return removed, err
}

// update applies change to the repository's approvals under a process and a
// file lock, and rewrites the file atomically with mode 0600.
func (s ApprovalStore) update(repoKey string, change func([]Approval) []Approval) error {
	p, err := s.path(repoKey)
	if err != nil {
		return err
	}
	if _, err := s.readStrict(repoKey); err != nil {
		return err
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf("sandbox approvals: %w", err)
	}
	approvalMu.Lock()
	defer approvalMu.Unlock()
	lock, err := os.OpenFile(p+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("sandbox approvals: %w", err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("sandbox approvals: lock: %w", err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	current, err := s.readStrict(repoKey)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(approvalFile{SchemaVersion: approvalSchemaVersion, RepoKey: repoKey, Approvals: change(current)}, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.dir, ".approval-*")
	if err != nil {
		return fmt.Errorf("sandbox approvals: %w", err)
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), p)
}

// SandboxNotApprovedError is the refusal of a run (or a submit) whose
// committed .factory.yml carries a sandbox block the operator has not
// approved.
type SandboxNotApprovedError struct {
	Hash   string
	Commit string
	Repo   string
	// Store is the file the approval was looked for in.
	Store string
}

// SandboxNeedsRegistryProxy is the refusal of a repository whose sandbox
// block runs with the registry proxy off: its install reaches package
// registries only through the proxy. A run and a submit refuse with it.
const SandboxNeedsRegistryProxy = "sandbox: the sandbox block of .factory.yml reaches package registries only through the registry proxy, and registry_proxy is off"

// Reason says what is wrong, without the command that fixes it.
func (e *SandboxNotApprovedError) Reason() string {
	at := ""
	if e.Commit != "" {
		at = " at " + short12(e.Commit)
	}
	return fmt.Sprintf("sandbox: the sandbox block %s of %s%s is not approved (approvals are read from %s)", short12(e.Hash), FileName, at, e.Store)
}

// Command is the operator's command that approves the block.
func (e *SandboxNotApprovedError) Command() string {
	return "factoryd sandbox approve -repo " + e.Repo
}

func (e *SandboxNotApprovedError) Error() string { return e.Reason() + ": " + e.Command() }

func short12(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

// RequireSandboxApproved returns nil when block is nil or the
// operator approved its hash for repoKey, and a *SandboxNotApprovedError
// otherwise. block (Config.Sandbox) and commit must come from the committed .factory.yml
// (Load, ConfirmReadCommit), never from the worktree. repo is the
// repository path the refusal tells the operator to approve.
func RequireSandboxApproved(dataDir, repoKey, repo string, block *Sandbox, commit string) error {
	if block == nil {
		return nil
	}
	hash := block.SHA256()
	if _, ok := OpenApprovals(dataDir).Approved(repoKey, hash); ok {
		return nil
	}
	return &SandboxNotApprovedError{Hash: hash, Commit: commit, Repo: repo, Store: OpenApprovals(dataDir).Path(repoKey)}
}
