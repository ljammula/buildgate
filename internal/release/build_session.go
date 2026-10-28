package release

import (
	"errors"
	"fmt"
	"os"
	"path"

	"buildgate/internal/request"
	"buildgate/internal/run"
)

// PruneBuildSessions removes the kept build session (run.BuildSessionDirName)
// of every run of r that no corrective or advice round of its ticket can
// still follow: the runs of a ticket lower than the one r is on, and every
// run of r once r is in a state other than building or resume_review (an
// operator's resume may relaunch a round that continues the current
// ticket's session). Whatever r's state, a session stays while a round that
// follows its run is in flight: one of r's runs not yet terminal names it as
// EarlierAttemptOf, or a ticket's advice round not yet started names it as
// Advice.Of. Each removal goes through an os.Root on the runs directory, so
// no link, the run's directory included, leads it outside that directory,
// and it removes only the build-session directory itself. It tries every
// run and returns the joined failures.
func PruneBuildSessions(dataDir string, r *request.Request) error {
	root, err := os.OpenRoot(run.Dir(dataDir, ""))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer root.Close()
	ids, runs, err := sessionsOfRequest(dataDir, root, r.ID)
	if err != nil || len(ids) == 0 {
		return err
	}
	keepFrom := buildSessionsKeptFrom(r)
	followed := followedByARoundInFlight(r, runs)
	var errs []error
	for _, id := range ids {
		rec := runs[id]
		if followed[id] || (keepFrom >= 0 && (rec.TicketIndex == 0 || rec.TicketIndex >= keepFrom)) {
			continue
		}
		if err := root.RemoveAll(path.Join(id, run.BuildSessionDirName)); err != nil {
			errs = append(errs, fmt.Errorf("run %s: remove its build session: %w", id, err))
		}
	}
	return errors.Join(errs...)
}

// sessionsOfRequest returns the ids of requestID's runs that keep a build
// session as a real directory, and every run of requestID by id. A run whose
// record does not load is left alone. It loads the run records only when some
// run directory holds a build session at all.
func sessionsOfRequest(dataDir string, root *os.Root, requestID string) ([]string, map[string]*run.Run, error) {
	entries, err := os.ReadDir(run.Dir(dataDir, ""))
	if err != nil {
		return nil, nil, err
	}
	var withSession []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		info, statErr := root.Lstat(path.Join(entry.Name(), run.BuildSessionDirName))
		if statErr == nil && info.IsDir() {
			withSession = append(withSession, entry.Name())
		}
	}
	if len(withSession) == 0 {
		return nil, nil, nil
	}
	runs := map[string]*run.Run{}
	for _, entry := range entries {
		rec, loadErr := run.Load(dataDir, entry.Name())
		if entry.IsDir() && loadErr == nil && rec.RequestID == requestID {
			runs[rec.ID] = rec
		}
	}
	var ids []string
	for _, id := range withSession {
		if _, ok := runs[id]; ok {
			ids = append(ids, id)
		}
	}
	return ids, runs, nil
}

// followedByARoundInFlight is the set of runs whose session a round of r may
// still be about to continue: the EarlierAttemptOf of each of r's runs not
// yet terminal, and the Advice.Of of each ticket's advice round in flight.
func followedByARoundInFlight(r *request.Request, runs map[string]*run.Run) map[string]bool {
	followed := map[string]bool{}
	for _, rec := range runs {
		if rec.EarlierAttemptOf != "" && !rec.TerminalConfirmed() {
			followed[rec.EarlierAttemptOf] = true
		}
	}
	for _, t := range r.Tickets {
		if t.Advice != nil && t.Advice.Of != "" {
			followed[t.Advice.Of] = true
		}
	}
	return followed
}

// buildSessionsKeptFrom is the lowest ticket index whose runs keep their build
// sessions in r's current state, or -1 when none does. A run recorded with no
// ticket index is kept while any is.
func buildSessionsKeptFrom(r *request.Request) int {
	switch r.State {
	case request.StateBuilding, request.StateResumeReview:
		return r.TicketIndex
	default:
		return -1
	}
}
