package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"buildgate/internal/evidence"
	"buildgate/internal/run"
	"buildgate/internal/sandbox"
	wsisolation "buildgate/internal/workspace"
)

// RoundStateFileName is build_app.py's round-state file at the worktree root
// (ROUND_STATE_FILE in agent/pi/scripts/build_app.py).
const RoundStateFileName = ".pi-build-round-state.json"

// ResumeFrom names a halted run whose kept worktree a new run adopts instead
// of preparing a fresh one. The halted run's worktree holds the round state
// build_app.py resumes from. Nothing here is trusted over the halted run's
// own record: adoption refuses unless every field agrees with it and with its
// isolation marker.
type ResumeFrom struct {
	// RunID is the halted run (run.Run.ID) whose worktree is adopted.
	RunID string `json:"run_id"`
	// WorktreePath/Branch are that run's isolated worktree and branch.
	WorktreePath string `json:"worktree_path"`
	Branch       string `json:"branch"`
	// BaseSHA is the commit the halted run's worktree branched from; the
	// resumed run's gates diff against it, not against the shared
	// checkout's HEAD at resume time. It must equal the halted run's
	// recorded BaseSHA.
	BaseSHA string `json:"base_sha"`
	// InstructionBaseSHA is the instruction base the halted run recorded
	// (its diff base, else its base, for a record that predates the field):
	// the resumed run's reviews trust that commit's instruction files, not
	// the halted attempt's tip.
	InstructionBaseSHA string `json:"instruction_base_sha,omitempty"`
}

// roundState is the part of build_app.py's round-state file the host reads:
// the HEAD it recorded and the rounds it completed.
type roundState struct {
	Head               *string `json:"head"`
	LastCompletedRound int     `json:"last_completed_round"`
}

// readRoundState reads the round-state file in worktree. found is false when
// the file does not exist; a file that exists but cannot be parsed is an
// error. head is "" when the file recorded none.
func readRoundState(worktree string) (head string, lastRound int, found bool, err error) {
	b, err := os.ReadFile(filepath.Join(worktree, RoundStateFileName))
	if errors.Is(err, os.ErrNotExist) {
		return "", 0, false, nil
	}
	if err != nil {
		return "", 0, true, err
	}
	var state roundState
	if err := json.Unmarshal(b, &state); err != nil {
		return "", 0, true, err
	}
	if state.Head != nil {
		head = strings.TrimSpace(*state.Head)
	}
	return head, state.LastCompletedRound, true, nil
}

// NewResumeFrom builds the resume input for halted, which must already have
// passed CheckResumePreconditions: its kept worktree, branch and base commit.
func NewResumeFrom(halted *run.Run) *ResumeFrom {
	return &ResumeFrom{
		RunID:              halted.ID,
		WorktreePath:       halted.WorkspacePath,
		Branch:             halted.Branch,
		BaseSHA:            halted.BaseSHA,
		InstructionBaseSHA: firstNonEmpty(halted.InstructionBaseSHA, halted.DiffBaseSHA, halted.BaseSHA),
	}
}

// CarriedSpend is the relay spend a run resumed from haltedID inherits: what
// the halted run itself inherited (its ResumeSpendCarried) plus its whole
// relay ledger. Chained resumes (A, then B resuming A, then C resuming B)
// therefore each carry the whole chain's spend. An unreadable ledger is an
// error, never zero.
func CarriedSpend(dataDir, haltedID string) (run.MeterSpend, error) {
	halted, err := run.Load(dataDir, haltedID)
	if err != nil {
		return run.MeterSpend{}, fmt.Errorf("load run %s: %w", haltedID, err)
	}
	tokens, cost, err := sandbox.RunRelaySpend(dataDir, haltedID)
	if err != nil {
		return run.MeterSpend{}, fmt.Errorf("read the relay ledger of run %s: %w", haltedID, err)
	}
	carried := run.MeterSpend{Tokens: tokens, CostMicroUSD: cost}
	if halted.ResumeSpendCarried != nil {
		carried.Tokens += halted.ResumeSpendCarried.Tokens
		carried.CostMicroUSD += halted.ResumeSpendCarried.CostMicroUSD
	}
	return carried, nil
}

// worktreeGit runs git in dir and returns trimmed stdout.
func worktreeGit(dir string, args ...string) (string, error) {
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
	return strings.TrimSpace(string(out)), err
}

// CheckResumePreconditions reports whether the kept worktree of haltedRunID
// is safe to adopt. ok is true only with no reasons; each reason is a plain
// sentence for an operator. err is for a check that could not be made at all
// (the run record or Docker unreadable), never for a refusal. The checks:
//
//	(a) no container is labelled with the run and data dir
//	(b) the run is KeptForResume and its worktree path exists
//	(c) the worktree is on the run's own branch, and its HEAD equals the
//	    round state's recorded head or descends from it (a commit the agent
//	    made in the lost round must not block a resume); with no round-state
//	    file the run's base SHA stands in for the recorded head
//	(d) specSHA256, when non-empty, equals the halted run's recorded ticket
//	    spec hash: a resume continues the same ticket, not an edited one
//	(e) maxRounds, when positive, exceeds the round state's completed rounds:
//	    build_app.py refuses a resume with no round left
func CheckResumePreconditions(ctx context.Context, dataDir, dockerBinary, haltedRunID, specSHA256 string, maxRounds int) (ok bool, reasons []string, err error) {
	r, err := run.Load(dataDir, haltedRunID)
	if err != nil {
		return false, nil, fmt.Errorf("load run %s: %w", haltedRunID, err)
	}
	ids, err := sandbox.RunContainerIDs(ctx, dockerBinary, dataDir, haltedRunID)
	if err != nil {
		return false, nil, fmt.Errorf("check run %s for containers: %w", haltedRunID, err)
	}
	if len(ids) > 0 {
		reasons = append(reasons, fmt.Sprintf("%d sandbox container(s) labelled for run %s still exist (%s); remove them before resuming, a live worker could still be editing the worktree", len(ids), haltedRunID, strings.Join(ids, ", ")))
	}
	if !r.KeptForResume {
		reasons = append(reasons, fmt.Sprintf("run %s was not kept for a resume: its worktree was not preserved when it halted", haltedRunID))
		return false, reasons, nil
	}
	if specSHA256 != "" && r.SpecSHA256 != specSHA256 {
		reasons = append(reasons, fmt.Sprintf("the ticket spec changed since run %s halted (hash %s, now %s); a resume continues the same spec, rebuild instead", haltedRunID, shortSHA(r.SpecSHA256), shortSHA(specSHA256)))
	}
	worktree := r.WorkspacePath
	if info, statErr := os.Stat(worktree); worktree == "" || statErr != nil || !info.IsDir() {
		reasons = append(reasons, fmt.Sprintf("the kept worktree of run %s (%q) no longer exists", haltedRunID, worktree))
		return false, reasons, nil
	}
	branch, branchErr := worktreeGit(worktree, "symbolic-ref", "--short", "-q", "HEAD")
	if branchErr != nil || branch != r.Branch {
		reasons = append(reasons, fmt.Sprintf("the worktree of run %s is on %q, not the run's own branch %q", haltedRunID, branch, r.Branch))
	}
	head, headErr := worktreeGit(worktree, "rev-parse", "HEAD")
	if headErr != nil {
		reasons = append(reasons, fmt.Sprintf("the worktree of run %s has no readable HEAD: %v", haltedRunID, headErr))
		return false, reasons, nil
	}
	recorded, lastRound, found, stateErr := readRoundState(worktree)
	recordedWhat := "the HEAD its round state recorded"
	switch {
	case stateErr != nil:
		reasons = append(reasons, fmt.Sprintf("the round state of run %s is unreadable (%v); a resume cannot continue from it", haltedRunID, stateErr))
		return false, reasons, nil
	case !found || recorded == "":
		recorded, recordedWhat = r.BaseSHA, "the run's base commit (it wrote no round state)"
	}
	if maxRounds > 0 && found && lastRound >= maxRounds {
		reasons = append(reasons, fmt.Sprintf("the round state of run %s records %d completed round(s), but the resumed run allows only %d: no round is left to run; rebuild instead", haltedRunID, lastRound, maxRounds))
	}
	if recorded == "" {
		reasons = append(reasons, fmt.Sprintf("run %s records no commit to compare the worktree HEAD against", haltedRunID))
		return false, reasons, nil
	}
	if head != recorded {
		// Exit 0: recorded is an ancestor of HEAD. Any other exit (1 not an
		// ancestor, 128 an unknown object) means the history was rewritten.
		if mergeErr := exec.Command("git", "-C", worktree, "merge-base", "--is-ancestor", recorded, head).Run(); mergeErr != nil {
			var exitErr *exec.ExitError
			if !errors.As(mergeErr, &exitErr) {
				return false, nil, fmt.Errorf("compare worktree HEAD of run %s: %w", haltedRunID, mergeErr)
			}
			reasons = append(reasons, fmt.Sprintf("the worktree HEAD %s of run %s is not %s (%s) or a descendant of it: the history was rewritten", shortSHA(head), haltedRunID, shortSHA(recorded), recordedWhat))
		}
	}
	return len(reasons) == 0, reasons, nil
}

// adoptKeptWorktree is PrepareIsolatedWorkspaceActivity's resume branch: it
// adopts the halted run's worktree instead of creating one. It returns a
// non-nil error without worktree details when it refused or failed before
// anything changed (the worktree stays kept under the halted run), and an
// error carrying the worktree (attachIsolatedWorkspaceDetail) once the
// marker has moved, so the caller can flag the new run KeptForResume.
//
// The whole adoption holds the halted run's lock, so a second concurrent
// resume of the same run sees KeptForResume already false and is refused.
// In order, under that lock:
//
//  1. re-check CheckResumePreconditions and the input against the halted
//     run's record and isolation marker
//  2. remove the halted run's marker (a failure aborts, nothing else changed)
//  3. write the new run's marker (a failure restores the halted run's)
//  4. clear the halted run's KeptForResume and record the carried relay spend
//     on the new run (adoptFinish)
//
// A crash after step 3 leaves the new run's own marker on disk: the
// redispatched Activity finds it, and finishes steps 4 onward, instead of
// refusing because the halted run's flag is gone.
func (a *Activities) adoptKeptWorktree(ctx context.Context, input PrepareIsolatedWorkspaceInput, checkpointDir, activityRunID, activityID string) (PrepareIsolatedWorkspaceResult, error) {
	resume := input.Resume
	if input.DataDir == "" || input.DurableRunID == "" {
		return PrepareIsolatedWorkspaceResult{}, errors.New("resume needs the run's data dir and id")
	}
	if err := wsisolation.ValidateIsolationRunID(resume.RunID); err != nil {
		return PrepareIsolatedWorkspaceResult{}, fmt.Errorf("resume run id: %w", err)
	}
	if err := wsisolation.ValidateIsolationRunID(input.DurableRunID); err != nil {
		return PrepareIsolatedWorkspaceResult{}, fmt.Errorf("validate isolation marker run ID: %w", err)
	}
	var result PrepareIsolatedWorkspaceResult
	var moved bool
	var adoptErr error
	lockErr := run.WithLock(input.DataDir, resume.RunID, func() error {
		result, moved, adoptErr = a.adoptLocked(ctx, input, checkpointDir, activityRunID, activityID)
		return nil
	})
	if lockErr != nil {
		return PrepareIsolatedWorkspaceResult{}, lockErr
	}
	if adoptErr != nil && moved {
		return result, attachIsolatedWorkspaceDetail(adoptErr.Error(), IsolationFailureType, adoptErr, result.WorktreePath, result.Branch)
	}
	return result, adoptErr
}

// adoptLocked is adoptKeptWorktree's body, run under the halted run's lock.
// moved reports whether the new run's marker exists by the time it returns.
func (a *Activities) adoptLocked(ctx context.Context, input PrepareIsolatedWorkspaceInput, checkpointDir, activityRunID, activityID string) (result PrepareIsolatedWorkspaceResult, moved bool, err error) {
	resume := input.Resume
	newPath := wsisolation.IsolationMarkerPath(input.DataDir, input.DurableRunID)
	oldPath := wsisolation.IsolationMarkerPath(input.DataDir, resume.RunID)

	// A crashed earlier dispatch of this adoption already moved the marker.
	if existing, loadErr := wsisolation.LoadIsolationMarker(newPath); loadErr == nil &&
		existing.RunID == input.DurableRunID && existing.Branch == resume.Branch && samePath(existing.WorktreePath, resume.WorktreePath) {
		existing.Prepared = true
		existing.WorkflowID = input.WorkflowID
		existing.ActivityRunID = activityRunID
		existing.ActivityID = activityID
		existing.CheckpointDir = checkpointDir
		result = PrepareIsolatedWorkspaceResult{WorktreePath: existing.WorktreePath, Branch: existing.Branch}
		if err := wsisolation.WriteIsolationMarker(newPath, existing); err != nil {
			return result, true, fmt.Errorf("refresh the adopted worktree's isolation marker: %w", err)
		}
		if err := wsisolation.EnableWorkerGroupWrite(existing.WorktreePath, os.Getgid()); err != nil {
			return result, true, fmt.Errorf("grant worker write on the adopted worktree: %w", err)
		}
		// A crash between writing the new marker and removing the old one
		// leaves both: finish the removal.
		if err := wsisolation.RemoveIsolationMarker(oldPath); err != nil {
			return result, true, fmt.Errorf("remove the isolation marker of run %s: %w", resume.RunID, err)
		}
		return result, true, a.adoptFinish(input)
	}

	specSHA := ""
	if input.SpecPath != "" {
		h, hashErr := evidence.SHA256File(input.SpecPath)
		if hashErr != nil {
			return PrepareIsolatedWorkspaceResult{}, false, fmt.Errorf("hash the resumed run's ticket spec: %w", hashErr)
		}
		specSHA = h
	}
	maxRounds := input.MaxRounds
	if maxRounds == 0 {
		maxRounds = a.MaxRounds
	}
	ok, reasons, checkErr := CheckResumePreconditions(ctx, input.DataDir, a.SandboxDocker, resume.RunID, specSHA, maxRounds)
	if checkErr != nil {
		return PrepareIsolatedWorkspaceResult{}, false, checkErr
	}
	if !ok {
		return PrepareIsolatedWorkspaceResult{}, false, fmt.Errorf("cannot resume run %s: %s", resume.RunID, strings.Join(reasons, "; "))
	}
	halted, err := run.Load(input.DataDir, resume.RunID)
	if err != nil {
		return PrepareIsolatedWorkspaceResult{}, false, fmt.Errorf("load run %s: %w", resume.RunID, err)
	}
	if resume.BaseSHA != halted.BaseSHA {
		return PrepareIsolatedWorkspaceResult{}, false, fmt.Errorf("resume input does not match run %s: base commit %s, the run recorded %s", resume.RunID, shortSHA(resume.BaseSHA), shortSHA(halted.BaseSHA))
	}
	marker, err := wsisolation.LoadIsolationMarker(oldPath)
	if err != nil {
		return PrepareIsolatedWorkspaceResult{}, false, fmt.Errorf("load the isolation marker of run %s: %w", resume.RunID, err)
	}
	if err := wsisolation.ValidateIsolationMarker(marker, input.DataDir, input.RepoDir); err != nil {
		return PrepareIsolatedWorkspaceResult{}, false, fmt.Errorf("isolation marker of run %s: %w", resume.RunID, err)
	}
	if !marker.Prepared || marker.RunID != resume.RunID || marker.Branch != resume.Branch ||
		!samePath(marker.WorktreePath, resume.WorktreePath) ||
		!samePath(marker.WorktreePath, halted.WorkspacePath) || marker.Branch != halted.Branch {
		return PrepareIsolatedWorkspaceResult{}, false, fmt.Errorf("resume input does not match the isolation marker or record of run %s", resume.RunID)
	}
	result = PrepareIsolatedWorkspaceResult{WorktreePath: marker.WorktreePath, Branch: marker.Branch}
	adopted := marker
	adopted.RunID = input.DurableRunID
	adopted.Mode = "temporal"
	adopted.Prepared = true
	adopted.WorkflowID = input.WorkflowID
	adopted.ActivityRunID = activityRunID
	adopted.ActivityID = activityID
	adopted.CheckpointDir = checkpointDir

	// The new marker is written first and the old one removed second, so the
	// worktree always has at least one marker: a crash between the two leaves
	// both (the redispatch continues), never none. A failure to write the new
	// marker changes nothing; a failure to remove the old one takes the new
	// one back out, so the abort leaves exactly the starting state.
	if err := wsisolation.WriteIsolationMarker(newPath, adopted); err != nil {
		return PrepareIsolatedWorkspaceResult{}, false, fmt.Errorf("record isolation ownership marker: %w", err)
	}
	if err := wsisolation.RemoveIsolationMarker(oldPath); err != nil {
		if undoErr := wsisolation.RemoveIsolationMarker(newPath); undoErr != nil {
			// Both markers stay, the worktree stays owned: report it as moved
			// so the caller flags the new run kept.
			return result, true, fmt.Errorf("remove the isolation marker of run %s: %w (and could not withdraw the new marker: %v)", resume.RunID, err, undoErr)
		}
		return PrepareIsolatedWorkspaceResult{}, false, fmt.Errorf("remove the isolation marker of run %s: %w", resume.RunID, err)
	}
	// From here the worktree belongs to the new run: any failure keeps it
	// (rule R) and is reported with the worktree attached.
	if err := wsisolation.EnableWorkerGroupWrite(marker.WorktreePath, os.Getgid()); err != nil {
		return result, true, fmt.Errorf("grant worker write on the adopted worktree: %w", err)
	}
	return result, true, a.adoptFinish(input)
}

// adoptFinish clears the halted run's KeptForResume and records, on the new
// run, the relay spend it inherits (the halted run's carried spend plus its
// ledger). Idempotent, so a redispatched adoption repeats it safely. The
// caller holds the halted run's lock.
func (a *Activities) adoptFinish(input PrepareIsolatedWorkspaceInput) error {
	resume := input.Resume
	carried, err := CarriedSpend(input.DataDir, resume.RunID)
	if err != nil {
		return err
	}
	halted, err := run.Load(input.DataDir, resume.RunID)
	if err != nil {
		return err
	}
	if halted.KeptForResume {
		halted.KeptForResume = false
		if err := halted.Persist(input.DataDir); err != nil {
			return fmt.Errorf("clear kept-for-resume on run %s: %w", resume.RunID, err)
		}
	}
	return run.WithLock(input.DataDir, input.DurableRunID, func() error {
		fresh, loadErr := run.Load(input.DataDir, input.DurableRunID)
		if errors.Is(loadErr, os.ErrNotExist) {
			// No record to annotate (a workflow driven without one): the
			// spend cap recomputes the carried spend from the halted run.
			return nil
		}
		if loadErr != nil {
			return fmt.Errorf("load run %s: %w", input.DurableRunID, loadErr)
		}
		fresh.ResumeSpendCarried = &carried
		return fresh.Persist(input.DataDir)
	})
}

// samePath compares two paths in canonical form, falling back to the cleaned
// paths when one cannot be resolved.
func samePath(a, b string) bool {
	ca, errA := wsisolation.CanonicalPath(a)
	cb, errB := wsisolation.CanonicalPath(b)
	if errA != nil || errB != nil {
		return filepath.Clean(a) == filepath.Clean(b)
	}
	return ca == cb
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
