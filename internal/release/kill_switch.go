package release

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
)

// killSwitchTransitionMu serializes transitions within one process.
// withKillSwitchLock takes it *and* a real OS-level advisory lock: flock's
// behavior for two descriptors on the same file within one process is
// platform-specific (the same caveat internal/run.WithLock's own doc
// comment records), so the mutex remains the in-process guarantee and the
// flock is the cross-process one. Neither substitutes for the other.
var killSwitchTransitionMu sync.Mutex

// KillSwitchTransition is one attributable change to a project's switch.
type KillSwitchTransition struct {
	Engaged bool   `json:"engaged"`
	By      string `json:"by"`
	Reason  string `json:"reason"`
	At      string `json:"at"`
}

// KillSwitchRecord is the durable current state and append-only transition
// history for one project.
type KillSwitchRecord struct {
	Project string                 `json:"project"`
	Engaged bool                   `json:"engaged"`
	History []KillSwitchTransition `json:"history"`
}

// Engage durably engages a project's kill switch.
func Engage(dir, project, by, reason string, now func() string) error {
	_, err := SetKillSwitch(dir, project, by, reason, true, now)
	return err
}

// Disengage durably disengages a project's kill switch.
func Disengage(dir, project, by, reason string, now func() string) error {
	_, err := SetKillSwitch(dir, project, by, reason, false, now)
	return err
}

// SetKillSwitch applies one transition and returns the resulting durable
// record, read back under the same lock that performed the transition.
//
// Callers that report a result to an operator should use this rather than a
// transition followed by a separate LoadKillSwitch: the returned record is
// the exact state this transition produced, whereas a separate read can
// return a *different* record another process wrote in between, so the
// operator would be shown a state that is not the outcome of the command
// they just ran.
//
// This is not, and cannot be, a promise that the returned state is still
// current when the caller prints it: the lock is released when this
// function returns, and another process may transition the switch
// immediately afterward — as it may one microsecond after any print,
// however long a lock were held (found via Codex review of PR #46, which
// correctly rejected an earlier version of this comment claiming
// otherwise). The guarantee is snapshot integrity of *this* transition's
// own outcome, not exclusivity over the operator's terminal.
func SetKillSwitch(dir, project, by, reason string, engaged bool, now func() string) (*KillSwitchRecord, error) {
	if by == "" {
		return nil, fmt.Errorf("kill switch by is required")
	}
	if reason == "" {
		return nil, fmt.Errorf("kill switch reason is required")
	}
	var record *KillSwitchRecord
	err := withKillSwitchLock(dir, project, func() error {
		if err := transitionLocked(dir, project, by, reason, engaged, now); err != nil {
			return err
		}
		loaded, err := LoadKillSwitch(dir, project)
		if err != nil {
			return err
		}
		record = loaded
		return nil
	})
	if err != nil {
		return nil, err
	}
	return record, nil
}

// IsEngaged reports the durable state of a project's kill switch. A project
// with no record has never been engaged and reports false.
func IsEngaged(dir, project string) (bool, error) {
	record, err := LoadKillSwitch(dir, project)
	if err != nil {
		return false, err
	}
	return record.Engaged, nil
}

// LoadKillSwitch reconstructs a project's durable switch record. A missing
// record is returned as a disengaged switch with empty history.
func LoadKillSwitch(dir, project string) (*KillSwitchRecord, error) {
	path, err := killSwitchPath(dir, project)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &KillSwitchRecord{Project: project}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read kill switch: %w", err)
	}
	var record KillSwitchRecord
	if err := json.Unmarshal(b, &record); err != nil {
		return nil, fmt.Errorf("unmarshal kill switch: %w", err)
	}
	if record.Project != project {
		return nil, fmt.Errorf("kill switch record project is %q, want %q", record.Project, project)
	}
	return &record, nil
}

// withKillSwitchLock runs fn while holding both the in-process mutex and an
// exclusive OS-level advisory lock (syscall.Flock) on this project's kill
// switch, blocking until it acquires one.
//
// Found via Codex review of PR #46: `factoryd kill-switch` made
// transitions reachable from separate processes for the first time (before
// it, the primitives below had no operator surface at all), and the
// in-process mutex alone cannot see another process. Two concurrent
// invocations could each load the same record, each append one transition,
// and the later save would silently drop the earlier operator's
// attributable entry from the audit history — with an emergency engage as
// plausibly the losing writer. Mirrors internal/run.WithLock's precedent
// for the same class of problem on run records, including its Unix-only
// (syscall.Flock) assumption.
//
// Unlike run.WithLock, this does create the project directory when it is
// missing: a project's first-ever engage legitimately precedes any other
// record under it, and there is nowhere else to put the lock. A transition
// that turns out to be an idempotent no-op therefore also leaves an empty
// project directory behind, which run.WithLock deliberately avoids for run
// ids — the difference is that a project name here is an operator-supplied
// identifier already constrained by killSwitchPath to a single path
// component under dir/projects, not an arbitrary run id an authenticated
// caller can mint unboundedly.
func withKillSwitchLock(dir, project string, fn func() error) error {
	path, err := KillSwitchLockPath(dir, project)
	if err != nil {
		return err
	}
	killSwitchTransitionMu.Lock()
	defer killSwitchTransitionMu.Unlock()

	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("create kill switch dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open kill switch lock file: %w", err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("acquire kill switch lock: %w", err)
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return fn()
}

// KillSwitchLockPath returns the advisory lock file withKillSwitchLock uses
// for one project. Exported so a test outside this package can hold the
// same lock from its own process and prove a `factoryd kill-switch`
// subprocess really waits for it.
func KillSwitchLockPath(dir, project string) (string, error) {
	path, err := killSwitchPath(dir, project)
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(path), ".kill_switch.lock"), nil
}

// transitionLocked is the load-modify-save half of a transition. It must
// only be called with this project's kill-switch lock already held.
func transitionLocked(dir, project, by, reason string, engaged bool, now func() string) error {
	record, err := LoadKillSwitch(dir, project)
	if err != nil {
		return err
	}
	// Repeated requests are idempotent successes. They are not state
	// transitions, so recording them would add noise to the audit timeline.
	if record.Engaged == engaged {
		return nil
	}
	record.Engaged = engaged
	record.History = append(record.History, KillSwitchTransition{
		Engaged: engaged,
		By:      by,
		Reason:  reason,
		At:      now(),
	})
	return record.save(dir)
}

func (r *KillSwitchRecord) save(dir string) error {
	path, err := killSwitchPath(dir, r.Project)
	if err != nil {
		return err
	}
	// 0o750/0o600: an engage/disengage record is attributable audit
	// evidence (who, why, when), not something other local users need to
	// read, matching run.Run.Save's own permission choice.
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("create kill switch dir: %w", err)
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal kill switch: %w", err)
	}
	// A per-writer temp file, not a fixed path + ".tmp" (found via Codex
	// review of PR #46): every writer previously staged through the same
	// name, so two of them could tear each other's partially-written
	// content before either rename published it. The lock above already
	// excludes concurrent writers; this keeps the atomic-rename pattern
	// correct on its own merits rather than relying on that.
	tmp, err := os.CreateTemp(filepath.Dir(path), ".kill_switch-*.json.tmp")
	if err != nil {
		return fmt.Errorf("create kill switch temp file: %w", err)
	}
	// os.CreateTemp already creates with 0o600, matching the mode this
	// record is published under.
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return fmt.Errorf("write kill switch: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close kill switch temp file: %w", err)
	}
	return os.Rename(tmp.Name(), path)
}

func killSwitchPath(dir, project string) (string, error) {
	if project == "" {
		return "", fmt.Errorf("project is required")
	}
	if project == "." || project == ".." || strings.ContainsAny(project, `/\\`) {
		return "", fmt.Errorf("project %q must be a single path component", project)
	}
	return filepath.Join(dir, "projects", project, "kill_switch.json"), nil
}
