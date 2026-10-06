package release

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"buildgate/internal/run"
	wsisolation "buildgate/internal/workspace"
)

// ClearKeptForResume ends a kept worktree's wait for a human decision: it
// reaps the worktree, branch and isolation marker through the same
// ReapIsolationMarker path reconcile uses, then clears the run's
// KeptForResume flag. Rebuild, retry, cancel and an override to halted call
// it. A reap the safety checks refuse leaves the flag set and returns the
// error, so the worktree is never orphaned with its protection removed. A run
// with no flag set is a no-op.
func ClearKeptForResume(dataDir, runID string) error {
	return run.WithLock(dataDir, runID, func() error {
		r, err := run.Load(dataDir, runID)
		if err != nil {
			return err
		}
		if !r.KeptForResume {
			return nil
		}
		markerPath := wsisolation.IsolationMarkerPath(dataDir, runID)
		marker, markerErr := wsisolation.LoadIsolationMarker(markerPath)
		switch {
		case markerErr == nil:
			if err := wsisolation.ReapIsolationMarker(marker.RepoDir, marker); err != nil {
				return fmt.Errorf("reap kept worktree of run %s: %w", runID, err)
			}
			if err := wsisolation.RemoveIsolationMarker(markerPath); err != nil {
				return fmt.Errorf("remove isolation marker of run %s: %w", runID, err)
			}
		case !errors.Is(markerErr, os.ErrNotExist):
			return fmt.Errorf("load isolation marker of run %s: %w", runID, markerErr)
		case r.WorkspacePath != "" && r.WorkspacePath != r.ProjectPath && r.Branch != "":
			// No marker (an adoption or a reap crashed between moving it and
			// finishing): the run's own record still names the worktree, and
			// clearing the flag without removing it would leak it. A
			// worktree that is already gone leaves nothing to remove.
			// Unless another run's marker claims it: an adoption that moved
			// the marker to the new run but crashed before clearing this
			// flag leaves the worktree live under that run.
			if claimedByAnotherMarker(dataDir, runID, r.WorkspacePath) {
				break
			}
			if err := Rollback(r.ProjectPath, r.WorkspacePath, r.Branch); err != nil {
				if _, statErr := os.Stat(r.WorkspacePath); !os.IsNotExist(statErr) {
					return fmt.Errorf("remove kept worktree of run %s (no isolation marker): %w", runID, err)
				}
			}
		}
		r.KeptForResume = false
		return r.Persist(dataDir)
	})
}

// claimedByAnotherMarker reports whether an isolation marker other than
// runID's names worktreePath.
func claimedByAnotherMarker(dataDir, runID, worktreePath string) bool {
	paths, err := wsisolation.ListIsolationMarkerPaths(dataDir)
	if err != nil {
		return true // cannot prove it is unclaimed: leave the worktree
	}
	own := wsisolation.IsolationMarkerPath(dataDir, runID)
	want, wantErr := wsisolation.CanonicalPath(worktreePath)
	for _, p := range paths {
		if p == own {
			continue
		}
		m, loadErr := wsisolation.LoadIsolationMarker(p)
		if loadErr != nil {
			continue
		}
		got, gotErr := wsisolation.CanonicalPath(m.WorktreePath)
		if (wantErr == nil && gotErr == nil && got == want) || m.WorktreePath == worktreePath {
			return true
		}
	}
	return false
}

// ClearKeptRunsOfRequest reaps the kept worktree of every run of requestID
// that was kept for a resume decision: cancelling the request is that
// decision. It tries every run and returns the joined failures.
func ClearKeptRunsOfRequest(dataDir, requestID string) error {
	entries, err := os.ReadDir(filepath.Join(dataDir, "runs"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var errs []error
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		r, loadErr := run.Load(dataDir, entry.Name())
		if loadErr != nil || r.RequestID != requestID || !r.KeptForResume {
			continue
		}
		if err := ClearKeptForResume(dataDir, r.ID); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
