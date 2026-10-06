package request

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"
)

// revisionsDirName is the directory, under a request's own directory
// (Dir), that holds one subdirectory per snapshot SnapshotRevision takes.
const revisionsDirName = "revisions"

// revisionMetaFileName is the metadata file SnapshotRevision writes
// alongside the snapshotted file(s) in each revisions/<n>/ directory.
const revisionMetaFileName = "meta.json"

// Revision is the metadata SnapshotRevision records for one rejected-spec/
// plan snapshot. Index is 1-based, matching the "revisions/<n>/" directory
// naming. Files lists the
// snapshotted files' paths, relative to the request's own directory (the
// same relative-path convention ApprovedSHA256 uses) -- LoadRevision reads
// their content back by these same paths.
type Revision struct {
	Index     int      `json:"index"`
	At        string   `json:"at"`
	By        string   `json:"by"`
	Reason    string   `json:"reason"`
	FromState State    `json:"from_state"`
	Files     []string `json:"files"`
	// FeedbackSupplied records whether Reason was actually fed back to the
	// redrafting pass that followed this rejection -- true for every
	// spec_review/oracle_review/plan_review rejection, since Reject only
	// snapshots a revision for those three states and each one's reason
	// now reaches its own stage's drafter (SpecFeedback/OracleFeedback/
	// PlanFeedback). Recorded explicitly, not left implicit, so an
	// operator reviewing an older revision (snapshotted before this field
	// existed, so it decodes as false) can tell a redraft that never saw
	// their note apart from one that did.
	FeedbackSupplied bool `json:"feedback_supplied"`
}

func revisionsDir(dataDir, id string) string {
	return filepath.Join(Dir(dataDir, id), revisionsDirName)
}

func revisionDir(dataDir, id string, n int) string {
	return filepath.Join(revisionsDir(dataDir, id), strconv.Itoa(n))
}

// SnapshotRevision copies each of relPaths (relative to the request's own
// directory, e.g. "spec.md" or "tickets/001.spec.md") into a new
// revisions/<n>/ directory, and records by/reason/fromState alongside them
// in that directory's own meta.json, so a later operator reviewing a
// redraft can see exactly what an earlier rejection was reacting to.
// Returns the new revision's index. A relPath that does not currently
// exist is skipped, not an error -- a request rejected out of spec_review
// has no tickets/*.spec.md yet to skip, but this keeps the function
// tolerant of that shape without every caller having to know it.
func SnapshotRevision(dataDir, id, by, reason string, fromState State, relPaths []string, now time.Time) (int, error) {
	// Idempotent retry check: Reject calls this before its own durable
	// r.Save, so a crash or error between this call succeeding and Save
	// committing means a retried Reject call
	// re-runs SnapshotRevision too. Without this check that retry would
	// allocate and publish a second, identical revision every time,
	// presenting an operator with duplicate "reject actions" for what
	// was really one rejection.
	//
	// A bare (by, reason, fromState) metadata match isn't enough to tell
	// a retry apart from two genuinely separate rejections that happen to
	// share the same reason text and state (found in review: an operator
	// rejecting two successive redrafts with the same feedback is a real
	// case, not just a retry). The distinguishing fact is durability: a
	// retry's earlier SnapshotRevision call succeeded but the Reject call
	// it was part of never reached r.Save, so the request's own durable
	// Rejections slice -- read fresh here, not the in-memory copy Reject
	// is about to append to -- has one FEWER matching entry than there
	// are matching revisions on disk. A genuine second rejection, by
	// contrast, is only ever attempted after the first one's Reject call
	// (SnapshotRevision and Save both) already succeeded, so Rejections
	// and revisions stay in lockstep. Only reuse a revision when disk
	// currently has more matches than the durable record does.
	existing, err := ListRevisions(dataDir, id)
	if err != nil {
		return 0, err
	}
	matchesRevision := func(rev Revision) bool {
		return rev.By == by && rev.Reason == reason && rev.FromState == fromState
	}
	revisionMatches := 0
	var lastMatch Revision
	for _, rev := range existing {
		if matchesRevision(rev) {
			revisionMatches++
			lastMatch = rev
		}
	}
	if revisionMatches > 0 {
		durableRejectionMatches := 0
		if r, loadErr := Load(dataDir, id); loadErr == nil {
			for _, rej := range r.Rejections {
				if rej.By == by && rej.Reason == reason && rej.Stage() == fromState {
					durableRejectionMatches++
				}
			}
		}
		// loadErr != nil (a request that somehow can't be loaded here)
		// treats durableRejectionMatches as 0 -- the conservative
		// direction, since it can only make this look like more of a
		// retry than it is, never less, and Reject's own Load call right
		// before this one would already have failed loudly if the
		// request were genuinely unreadable.
		if revisionMatches > durableRejectionMatches {
			return lastMatch.Index, nil
		}
	}
	n, err := nextRevisionIndex(dataDir, id)
	if err != nil {
		return 0, err
	}
	dir := revisionDir(dataDir, id, n)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return 0, fmt.Errorf("request %s: create revision dir: %w", id, err)
	}
	var files []string
	for _, relPath := range relPaths {
		b, err := os.ReadFile(filepath.Join(Dir(dataDir, id), relPath))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return 0, fmt.Errorf("request %s: read %s for revision: %w", id, relPath, err)
		}
		dst := filepath.Join(dir, relPath)
		if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
			return 0, fmt.Errorf("request %s: create revision file dir: %w", id, err)
		}
		if err := os.WriteFile(dst, b, 0o600); err != nil {
			return 0, fmt.Errorf("request %s: write revision file: %w", id, err)
		}
		files = append(files, relPath)
	}
	meta := Revision{
		Index:     n,
		At:        now.UTC().Format(time.RFC3339Nano),
		By:        by,
		Reason:    reason,
		FromState: fromState,
		Files:     files,
		// See Revision.FeedbackSupplied's own doc comment: true for every
		// state Reject actually snapshots a revision from.
		FeedbackSupplied: fromState == StateSpecReview || fromState == StateOracleReview || fromState == StatePlanReview,
	}
	b, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return 0, fmt.Errorf("request %s: marshal revision meta: %w", id, err)
	}
	// Write-then-rename, same as Request.Save: a crash or concurrent
	// ListRevisions read between the two steps must never observe a
	// missing or truncated meta.json.
	metaPath := filepath.Join(dir, revisionMetaFileName)
	tmp := metaPath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return 0, fmt.Errorf("request %s: write revision meta: %w", id, err)
	}
	if err := os.Rename(tmp, metaPath); err != nil {
		return 0, fmt.Errorf("request %s: finalize revision meta: %w", id, err)
	}
	return n, nil
}

// nextRevisionIndex returns 1 plus the highest existing revisions/<n>
// subdirectory's own n, or 1 if none exist yet.
func nextRevisionIndex(dataDir, id string) (int, error) {
	entries, err := os.ReadDir(revisionsDir(dataDir, id))
	if err != nil {
		if os.IsNotExist(err) {
			return 1, nil
		}
		return 0, fmt.Errorf("request %s: read revisions dir: %w", id, err)
	}
	max := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		n, err := strconv.Atoi(entry.Name())
		if err != nil || n <= 0 {
			continue
		}
		if n > max {
			max = n
		}
	}
	return max + 1, nil
}

// ListRevisions returns every revision recorded for id, sorted by Index
// ascending -- GET /requests/{id}/revisions. An empty slice, not an error,
// for a request that has never been rejected.
func ListRevisions(dataDir, id string) ([]Revision, error) {
	entries, err := os.ReadDir(revisionsDir(dataDir, id))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("request %s: read revisions dir: %w", id, err)
	}
	var revisions []Revision
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		n, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		rev, err := loadRevisionMeta(dataDir, id, n)
		if err != nil {
			// A revision directory that exists but has no meta.json yet
			// (or a torn one) means SnapshotRevision is still mid-write
			// for that one entry -- with the tmp+rename above this is
			// only a window of a few syscalls, but a concurrent read can
			// still land inside it. Skip that entry rather than failing
			// the whole listing for every other, already-durable
			// revision.
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, err
		}
		revisions = append(revisions, rev)
	}
	sort.Slice(revisions, func(i, j int) bool { return revisions[i].Index < revisions[j].Index })
	return revisions, nil
}

func loadRevisionMeta(dataDir, id string, n int) (Revision, error) {
	b, err := os.ReadFile(filepath.Join(revisionDir(dataDir, id, n), revisionMetaFileName))
	if err != nil {
		return Revision{}, fmt.Errorf("request %s: read revision %d meta: %w", id, n, err)
	}
	var rev Revision
	if err := json.Unmarshal(b, &rev); err != nil {
		return Revision{}, fmt.Errorf("request %s: parse revision %d meta: %w", id, n, err)
	}
	return rev, nil
}

// LoadRevision returns revision n's own metadata plus the content of
// every file it recorded, keyed by the same relative path Revision.Files
// lists -- GET /requests/{id}/revisions/{n}. Returns an error (wrapping
// os.ErrNotExist for a missing revision) rather than a partial result if
// any recorded file cannot be read back.
func LoadRevision(dataDir, id string, n int) (Revision, map[string]string, error) {
	rev, err := loadRevisionMeta(dataDir, id, n)
	if err != nil {
		return Revision{}, nil, err
	}
	contents := make(map[string]string, len(rev.Files))
	for _, relPath := range rev.Files {
		b, err := os.ReadFile(filepath.Join(revisionDir(dataDir, id, n), relPath))
		if err != nil {
			return Revision{}, nil, fmt.Errorf("request %s: read revision %d file %s: %w", id, n, relPath, err)
		}
		contents[relPath] = string(b)
	}
	return rev, contents, nil
}
