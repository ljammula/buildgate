package run

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
)

// HaltReasonReplayRefused is Run.HaltReasonCode's value when a replay run
// halted before any gate because its replayed commit did not pass the host's
// checks: the worktree was not exactly the replayed head, the new base was
// not under it, or a run it was replayed from was not an accepted, sandboxed
// run of the project.
const HaltReasonReplayRefused = "replay_refused"

// Replay is the record of a replay run: a run that judges commits the host
// replayed onto a new base instead of building any. The commits UpstreamSHA..
// SourceHeadSHA of the ticket's current PR-head run (OfRunID) were replayed,
// one for one and in order, onto OntoSHA, ending at HeadSHA. Every gate and
// review of the run judges HeadSHA against OntoSHA. A run is a replay run
// exactly when Run.Replay is set.
type Replay struct {
	// RequestID and TicketIndex name the request ticket whose runs were
	// replayed. The run's record takes both from here when it is created
	// (run.Run.RequestID, TicketIndex), and every run replayed from must
	// name the same.
	RequestID   string `json:"request_id"`
	TicketIndex int    `json:"ticket_index"`
	// OfRunID is the ticket's current PR-head run, whose commits were
	// replayed and whose build the run's build attempt is carried from.
	OfRunID string `json:"of_run_id"`
	// SourceRunIDs are every run whose commit is in UpstreamSHA..SourceHeadSHA.
	SourceRunIDs []string `json:"source_run_ids"`
	// UpstreamSHA is the earlier ticket's tip the replayed commits sat on;
	// SourceHeadSHA is OfRunID's result, the last of them.
	UpstreamSHA   string `json:"upstream_sha"`
	SourceHeadSHA string `json:"source_head_sha"`
	// OntoSHA is the new base the commits were replayed onto; HeadSHA the
	// last replayed commit.
	OntoSHA string `json:"onto_sha"`
	HeadSHA string `json:"head_sha"`
	// Commits pairs each original commit with its replay, in order, with
	// both commits' `git patch-id --stable`.
	Commits []ReplayedCommit `json:"commits"`
}

// ReplayedCommit is one commit of a replay and its original.
type ReplayedCommit struct {
	Original        string `json:"original"`
	Replayed        string `json:"replayed"`
	OriginalPatchID string `json:"original_patch_id"`
	ReplayedPatchID string `json:"replayed_patch_id"`
}

// fullObjectID is a full 40-character hex git object id.
var fullObjectID = regexp.MustCompile(`^[0-9a-f]{40}$`)

// maxReplayFileBytes bounds a replay file: a few hundred commits at most.
const maxReplayFileBytes = 1 << 20

// ReadReplayFile reads and checks the replay file the host wrote (Validate).
// An unknown field is refused, so a misspelled one never reads as empty.
func ReadReplayFile(path string) (*Replay, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("replay file: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() > maxReplayFileBytes {
		return nil, fmt.Errorf("replay file %s must be a regular file of at most %d bytes", path, maxReplayFileBytes)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("replay file: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var r Replay
	if err := dec.Decode(&r); err != nil {
		return nil, fmt.Errorf("replay file %s: %w", path, err)
	}
	if err := r.Validate(); err != nil {
		return nil, fmt.Errorf("replay file %s: %w", path, err)
	}
	return &r, nil
}

// Validate checks the record's shape: the request ticket and run ids named, every commit a full
// object id (ReplayedCommit.validate), at least one commit pair, the last replayed commit HeadSHA, the
// last original SourceHeadSHA, and OfRunID among SourceRunIDs.
func (r *Replay) Validate() error {
	if r.RequestID == "" || r.TicketIndex < 1 {
		return errors.New("request_id and ticket_index must name the request ticket replayed")
	}
	if r.OfRunID == "" || len(r.SourceRunIDs) == 0 || slices.Contains(r.SourceRunIDs, "") {
		return errors.New("of_run_id and source_run_ids must name runs")
	}
	if !slices.Contains(r.SourceRunIDs, r.OfRunID) {
		return fmt.Errorf("source_run_ids must include of_run_id %q", r.OfRunID)
	}
	for _, sha := range []string{r.UpstreamSHA, r.SourceHeadSHA, r.OntoSHA, r.HeadSHA} {
		if !fullObjectID.MatchString(sha) {
			return fmt.Errorf("%q is not a full 40-character commit id", sha)
		}
	}
	if len(r.Commits) == 0 {
		return errors.New("commits must pair at least one replayed commit with its original")
	}
	for _, c := range r.Commits {
		if err := c.validate(); err != nil {
			return err
		}
	}
	last := r.Commits[len(r.Commits)-1]
	if last.Replayed != r.HeadSHA || last.Original != r.SourceHeadSHA {
		return errors.New("the last commit pair must be source_head_sha replayed as head_sha")
	}
	return nil
}

// validate checks one pair: both commits full object ids, and the two patch
// ids full ids, or both empty for an empty commit, which has none.
func (c ReplayedCommit) validate() error {
	ids := []string{c.Original, c.Replayed}
	if c.OriginalPatchID != "" || c.ReplayedPatchID != "" {
		ids = append(ids, c.OriginalPatchID, c.ReplayedPatchID)
	}
	for _, id := range ids {
		if !fullObjectID.MatchString(id) {
			return fmt.Errorf("commit pair %q: %q is not a full 40-character id", c.Original, id)
		}
	}
	return nil
}
