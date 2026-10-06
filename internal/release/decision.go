package release

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"buildgate/internal/run"
)

// Decision is the factory-owned, fail-closed release decision for one run.
// It records the reasons even when release is denied so an operator can audit
// the decision without reconstructing transient policy state.
type Decision struct {
	RunID     string   `json:"run_id"`
	Project   string   `json:"project"`
	Allowed   bool     `json:"allowed"`
	Reasons   []string `json:"reasons,omitempty"`
	Evaluated string   `json:"evaluated_at"`
	// Invalidated is true once InvalidateDecision has denied this run's
	// decision in response to cross-run attribution recorded after the
	// fact (see its own doc comment). RecordDecision checks this under the
	// same lock before saving a fresh evaluation, so a later re-evaluation
	// can never silently clear an invalidation it doesn't know about.
	Invalidated bool `json:"invalidated,omitempty"`
	// TestsRequiredOptOut mirrors run.Run.TestsRequiredOptOut: the
	// ticket's declared reason for skipping the tests_added gate on
	// this run, or "" when no opt-out was declared. Recorded here (not
	// just on the run record) so the release decision itself -- the
	// durable, auditable "why was this allowed" document -- shows the
	// opt-out without a reader having to cross-reference run.json too.
	TestsRequiredOptOut string `json:"tests_required_opt_out,omitempty"`
}

// EvaluateDecision combines the pure merge policy with the durable project
// kill switch. It performs no merge, push, or deploy side effect.
func EvaluateDecision(dir, project string, candidate run.Run, cfg MergePolicy, evaluated string) (Decision, error) {
	if candidate.ID == "" {
		return Decision{}, fmt.Errorf("run id is required")
	}
	engaged, err := IsEngaged(dir, project)
	if err != nil {
		return Decision{}, err
	}
	allowed, reasons := MergePolicyCheck(candidate, cfg)
	if engaged {
		allowed = false
		reasons = append(reasons, fmt.Sprintf("kill switch is engaged for project %q", project))
	}
	return Decision{RunID: candidate.ID, Project: project, Allowed: allowed, Reasons: reasons, Evaluated: evaluated, TestsRequiredOptOut: candidate.TestsRequiredOptOut}, nil
}

// SaveDecision durably records a decision under the project directory.
func SaveDecision(dir string, decision Decision) error {
	path, err := decisionPath(dir, decision.Project, decision.RunID)
	if err != nil {
		return err
	}
	return writeJSONAtomic(path, ".decision-*.json.tmp", decision, "release decision")
}

// writeJSONAtomic marshals v as indented JSON and writes it to path via a
// temp file plus rename, never a direct os.WriteFile (found via Codex
// review of this PR): os.WriteFile truncates the destination before
// writing its contents, so a reader racing this save, or a crash
// mid-write, could observe or permanently keep a truncated document --
// matching kill_switch.go's own fix for the same class of bug (PR #46).
// tmpPattern is os.CreateTemp's own pattern argument (e.g.
// ".decision-*.json.tmp"); label appears only in wrapped error messages.
// Shared by SaveDecision and saveDecisionFailure below (found via Codex
// review of PR #173: these two used to hand-duplicate this exact
// sequence, risking the crash-safety invariant drifting out of sync
// between them if one were ever fixed without the other).
func writeJSONAtomic(path, tmpPattern string, v any, label string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("create %s directory: %w", label, err)
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal %s: %w", label, err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), tmpPattern)
	if err != nil {
		return fmt.Errorf("create %s temp file: %w", label, err)
	}
	// os.CreateTemp already creates with 0o600, matching the mode this
	// record is published under.
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return fmt.Errorf("write %s: %w", label, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s temp file: %w", label, err)
	}
	return os.Rename(tmp.Name(), path)
}

// LoadDecision reads back the decision SaveDecision durably recorded for one
// run, additionally distinguishing "never evaluated" from "evaluated but
// could not be durably recorded" via a *DecisionRecordingFailedError -- see
// that type's own doc comment. A run with no recorded decision AND no
// failure marker is not an error -- nothing records a decision until a run
// is accepted -- and returns (nil, nil), which callers must render as "no
// decision", never as an allowed one.
func LoadDecision(dir, project, runID string) (*Decision, error) {
	decision, err := loadDecisionFile(dir, project, runID)
	if err != nil {
		return nil, err
	}
	if decision != nil {
		return decision, nil
	}
	failure, err := loadDecisionFailure(dir, project, runID)
	if err != nil {
		return nil, err
	}
	if failure != nil {
		return nil, &DecisionRecordingFailedError{Failure: *failure}
	}
	return nil, nil
}

// loadDecisionFile is LoadDecision's own real-file-only core: it never
// translates a missing file into a *DecisionRecordingFailedError. Used
// internally by RecordDecision/InvalidateDecision, which ask "is there an
// existing recorded decision for this run" and must keep working on a
// retry even when an earlier attempt already left a failure marker behind
// -- the public LoadDecision's marker translation would otherwise turn
// that prior marker into a spurious "load existing release decision"
// failure on every subsequent retry, even after the underlying cause
// (e.g. a corrupted kill-switch.json) is fixed.
func loadDecisionFile(dir, project, runID string) (*Decision, error) {
	path, err := decisionPath(dir, project, runID)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read release decision: %w", err)
	}
	var decision Decision
	if err := json.Unmarshal(b, &decision); err != nil {
		return nil, fmt.Errorf("unmarshal release decision: %w", err)
	}
	return &decision, nil
}

// DecisionFailure is the durable marker RecordDecision writes, alongside
// where the real Decision file for this run would have gone, when it
// cannot evaluate or save one (e.g. an unreadable/corrupted
// kill-switch.json). Never written in place of a real Decision, only
// alongside it -- see recordDecisionFailure's own doc comment -- so an
// accepted run in this state is distinguishable from one genuinely never
// evaluated, which otherwise look identical (both have no decision file).
type DecisionFailure struct {
	Error              string `json:"error"`
	At                 string `json:"at"`
	KillSwitchReadable bool   `json:"kill_switch_readable"`
}

// DecisionRecordingFailedError is LoadDecision's typed result for a run
// whose release decision could not be durably recorded and left a
// DecisionFailure marker on disk. Callers must render this distinctly
// from LoadDecision's ordinary (nil, nil) "no decision" case: this run WAS
// evaluated, but the evaluation itself could not be saved, whereas "no
// decision" means recording was never attempted (the run isn't accepted
// yet).
type DecisionRecordingFailedError struct {
	Failure DecisionFailure
}

func (e *DecisionRecordingFailedError) Error() string {
	return fmt.Sprintf("release decision recording failed: %s", e.Failure.Error)
}

// decisionFailurePath names DecisionFailure's marker file, next to the
// real decision file decisionPath names for the same run.
func decisionFailurePath(dir, project, runID string) (string, error) {
	path, err := decisionPath(dir, project, runID)
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(path, ".json") + ".error.json", nil
}

// saveDecisionFailure durably records failure as the marker LoadDecision
// checks for when no real Decision file exists yet. Same temp-file-plus-
// rename pattern SaveDecision uses and for the same reason: a GET racing
// this write, or a crash mid-write, must never observe a truncated marker.
func saveDecisionFailure(dir, project, runID string, failure DecisionFailure) error {
	path, err := decisionFailurePath(dir, project, runID)
	if err != nil {
		return err
	}
	return writeJSONAtomic(path, ".decision-failure-*.json.tmp", failure, "release decision failure marker")
}

// loadDecisionFailure reads back a marker saveDecisionFailure wrote, or
// (nil, nil) when none exists.
func loadDecisionFailure(dir, project, runID string) (*DecisionFailure, error) {
	path, err := decisionFailurePath(dir, project, runID)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read release decision failure: %w", err)
	}
	var failure DecisionFailure
	if err := json.Unmarshal(b, &failure); err != nil {
		return nil, fmt.Errorf("unmarshal release decision failure: %w", err)
	}
	return &failure, nil
}

// removeDecisionFailure clears a stale marker once a real Decision is
// successfully recorded for the same run (e.g. after `factoryd retry`
// follows a fixed kill-switch.json). Best-effort and idempotent: a
// leftover marker is harmless since LoadDecision only consults it when no
// real Decision file exists.
func removeDecisionFailure(dir, project, runID string) error {
	path, err := decisionFailurePath(dir, project, runID)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// recordDecisionFailure is RecordDecision's own best-effort marker write
// on any failure to evaluate or save a real Decision for runID -- called
// from inside the same withDecisionLock closure that would otherwise have
// saved the real Decision, so a marker and a real Decision can never both
// be mid-write for the same run at once. A failure to write the marker
// itself is dropped: it must never mask or replace the caller's real
// error, which is what RecordDecision still returns regardless.
func recordDecisionFailure(dir, project, runID string, cause error) {
	_, killErr := IsEngaged(dir, project)
	_ = saveDecisionFailure(dir, project, runID, DecisionFailure{
		Error:              cause.Error(),
		At:                 time.Now().Format(time.RFC3339),
		KillSwitchReadable: killErr == nil,
	})
}

// decisionPath resolves where one run's decision is stored, rejecting a
// project or run id that is not a single path component -- the same
// constraint killSwitchPath enforces for the sibling record, kept here so a
// project or run id that reached this package from an HTTP path can never
// escape the durable project directory.
func decisionPath(dir, project, runID string) (string, error) {
	if err := SinglePathComponent("project", project); err != nil {
		return "", err
	}
	if err := SinglePathComponent("run id", runID); err != nil {
		return "", err
	}
	return filepath.Join(dir, "projects", project, "release-decisions", runID+".json"), nil
}

// SinglePathComponent rejects a value that could not safely be joined onto
// a data directory as one path segment: empty, ".", "..", or anything
// containing a separator. field names the input in the returned error.
func SinglePathComponent(field, value string) error {
	if value == "" {
		return fmt.Errorf("%s is required", field)
	}
	if value == "." || value == ".." || strings.ContainsAny(value, `/\`) {
		return fmt.Errorf("%s %q must be a single path component", field, value)
	}
	return nil
}

// ProjectFromWorkspace derives the one project identifier every release
// decision, kill switch, status filter, and API project listing is keyed
// by: the basename of the git repository that contains workspacePath, or
// of the symlink-resolved path itself when it is not inside a repository.
// One derivation for every entry point (`factoryd submit`, a direct
// `factoryd <run>`, POST /runs) -- a previous version let each entry
// point derive or supply its own, so the same checkout landed under two
// ids and one's kill switch never gated the other's runs (Codex review of
// PR #96, four findings with that single root cause). The repository
// root, not the path's parent: the product-spec intake layout keeps an
// empty <repo>/workspace placeholder inside the repository, and
// `factoryd submit ~/code/payments` points at the repository itself, and
// both must yield "payments" rather than "workspace" or "code".
func ProjectFromWorkspace(workspacePath string) string {
	return filepath.Base(RepositoryRoot(workspacePath))
}

// RepositoryRoot is the directory ProjectFromWorkspace takes its basename
// from: the git repository containing path, else the symlink-resolved
// path itself. Empty for an empty path.
func RepositoryRoot(path string) string {
	if path == "" {
		return ""
	}
	if top, err := exec.Command("git", "-C", path, "rev-parse", "--show-toplevel").Output(); err == nil {
		return strings.TrimSpace(string(top))
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return path
}

// RejectProjectCollision claims project for repositoryRoot, or refuses
// when a different repository already holds it: two checkouts whose
// repositories merely share a basename (/clients/acme/api,
// /clients/beta/api) must not share one kill switch and one
// release-decision directory (Codex review of PR #97). The claim is a
// durable ownership file, <dataDir>/projects/<project>/repository,
// created with O_EXCL so two processes starting first runs for
// same-basename repositories at the same moment cannot both succeed (the
// review's round 2: a scan of existing run records raced their own
// saves). Fail-closed at the operator's keyboard with both repositories
// named, rather than a hashed discriminator in an id they have to type
// into `status -project` and `kill-switch -project`; the remedy is a
// distinct checkout directory name.
func RejectProjectCollision(dataDir, project, repositoryRoot string) error {
	if err := SinglePathComponent("project", project); err != nil {
		return err
	}
	dir := filepath.Join(dataDir, "projects", project)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("claim project %q: %w", project, err)
	}
	path := filepath.Join(dir, "repository")
	// Written fully to a private temp file, then published with os.Link,
	// which fails with EEXIST atomically: a racing claimant can never see
	// a half-written or empty claim, and a creator killed mid-write
	// leaves only an unpublished temp file, never an empty claim that
	// blocks every later start (Codex review of PR #97).
	tmp, err := os.CreateTemp(dir, ".repository-claim-*")
	if err != nil {
		return fmt.Errorf("claim project %q: %w", project, err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(repositoryRoot + "\n"); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("claim project %q: %w", project, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("claim project %q: %w", project, err)
	}
	if err := os.Link(tmp.Name(), path); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("claim project %q: %w", project, err)
	}
	owner, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read project %q ownership: %w", project, err)
	}
	if held := strings.TrimSpace(string(owner)); held != repositoryRoot {
		return fmt.Errorf("project id %q is already held by repository %s; %s would share its kill switch and release decisions -- give one checkout a distinct directory name", project, held, repositoryRoot)
	}
	return nil
}

// ProjectOf returns the project identifier r's release decisions and
// kill switch are keyed by: r.Project as recorded when the run started,
// else ProjectFromWorkspace(r.ProjectPath) for a record that predates the
// field. Readers go through this rather than re-deriving so a listing
// never runs git once per record.
func ProjectOf(r *run.Run) string {
	if r.Project != "" {
		return r.Project
	}
	return ProjectFromWorkspace(r.ProjectPath)
}

// RecordDecision evaluates and durably records a release decision for one
// accepted run in a single call -- the shared core every caller that
// accepts a run (the direct/Temporal execution paths, and both the CLI and
// HTTP override paths) uses, so the same evaluate-then-save sequence isn't
// duplicated per caller. Groundwork only, per this file's own doc
// comments: it performs no merge, push, or deploy side effect.
//
// Runs under this run's own decision lock (see withDecisionLock), and
// checks for an existing Invalidated decision under that same lock before
// saving a fresh evaluation -- found via a real GitHub Codex App review
// of this PR: without serializing against InvalidateDecision, a
// successor's regression/spec-drift attribution landing between this
// run's own terminal-state save and this call reaching here could be
// silently overwritten by this fresh, stale-evidence Allowed: true
// evaluation the moment it saved. If an invalidation is already on file,
// this still saves the fresh evaluation (its reasons are real, current
// evidence an operator auditing the decision should see) but forces
// Allowed back to false and carries Invalidated forward, prepending the
// existing reasons so neither evaluation's account is lost.
func RecordDecision(dir, project string, candidate run.Run, cfg MergePolicy) (*Decision, error) {
	var recorded Decision
	err := withDecisionLock(dir, project, candidate.ID, func() error {
		decision, err := EvaluateDecision(dir, project, candidate, cfg, time.Now().Format(time.RFC3339))
		if err != nil {
			wrapped := fmt.Errorf("evaluate release decision: %w", err)
			recordDecisionFailure(dir, project, candidate.ID, wrapped)
			return wrapped
		}
		existing, err := loadDecisionFile(dir, project, candidate.ID)
		if err != nil {
			wrapped := fmt.Errorf("load existing release decision: %w", err)
			recordDecisionFailure(dir, project, candidate.ID, wrapped)
			return wrapped
		}
		if existing != nil && existing.Invalidated {
			decision.Allowed = false
			decision.Invalidated = true
			decision.Reasons = append(append([]string{}, existing.Reasons...), decision.Reasons...)
		}
		if err := SaveDecision(dir, decision); err != nil {
			wrapped := fmt.Errorf("save release decision: %w", err)
			recordDecisionFailure(dir, project, candidate.ID, wrapped)
			return wrapped
		}
		// A real Decision now exists; a marker from an earlier failed
		// attempt (e.g. before a corrupted kill-switch.json was fixed and
		// this run was retried) is stale -- see removeDecisionFailure's
		// own doc comment.
		_ = removeDecisionFailure(dir, project, candidate.ID)
		recorded = decision
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &recorded, nil
}

// InvalidateDecision durably denies the release decision for one run in
// response to cross-run attribution recorded on it after the fact --
// currently InvalidatedByRunID, a *proven* full_suite_verify regression a
// later declared successor attributed back to this run (see
// run.Run.InvalidatedByRunID's own doc comment). Runs under the same
// per-run decision lock RecordDecision uses (found via a real GitHub
// Codex App review of this PR), so the two serialize regardless of which
// one a concurrent caller reaches first: without a shared lock,
// RecordDecision could load this run's decision before this write
// applied, evaluate a fresh Allowed: true from its own stale in-memory
// evidence, and overwrite the invalidation outright when it saved.
//
// Writes a tombstone (Invalidated: true, Allowed: false) even when no
// decision has been recorded for this run yet, rather than silently
// dropping the invalidation the way an earlier version of this fix did:
// a run's own run.json is saved as accepted before recordReleaseDecision
// is called for it (see that caller's own doc comment for why), which
// leaves a real, if narrow, window where a very fast successor chain
// could reach this call before the predecessor's first decision was ever
// recorded at all. RecordDecision's own lock-protected check (above) then
// sees this tombstone and keeps the run denied instead of clearing it.
func InvalidateDecision(dir, project, runID, reason, evaluatedAt string) error {
	return withDecisionLock(dir, project, runID, func() error {
		existing, err := loadDecisionFile(dir, project, runID)
		if err != nil {
			return fmt.Errorf("load release decision to invalidate: %w", err)
		}
		decision := Decision{RunID: runID, Project: project}
		if existing != nil {
			decision = *existing
		}
		decision.Allowed = false
		decision.Invalidated = true
		decision.Reasons = append(decision.Reasons, reason)
		decision.Evaluated = evaluatedAt
		return SaveDecision(dir, decision)
	})
}

// decisionTransitionMu serializes RecordDecision/InvalidateDecision calls
// within one process; withDecisionLock takes it *and* a real OS-level
// advisory lock, mirroring withKillSwitchLock's own identical two-layer
// reasoning (see its doc comment) for the analogous cross-process race on
// a durable project record.
var decisionTransitionMu sync.Mutex

// withDecisionLock runs fn while holding both the in-process mutex and an
// exclusive OS-level advisory lock (syscall.Flock) on one run's decision
// file, blocking until it acquires one. See RecordDecision's and
// InvalidateDecision's own doc comments for the race this closes.
func withDecisionLock(dir, project, runID string, fn func() error) error {
	path, err := decisionPath(dir, project, runID)
	if err != nil {
		return err
	}
	decisionTransitionMu.Lock()
	defer decisionTransitionMu.Unlock()

	lockPath := path + ".lock"
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o750); err != nil {
		return fmt.Errorf("create release decision directory: %w", err)
	}
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open release decision lock file: %w", err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("acquire release decision lock: %w", err)
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return fn()
}
