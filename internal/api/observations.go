package api

import (
	"errors"
	"log"
	"net/http"
	"os"
	"path/filepath"

	"buildgate/internal/evidence"
	"buildgate/internal/observation"
	"buildgate/internal/release"
	"buildgate/internal/request"
	"buildgate/internal/run"
	"buildgate/internal/triage"
)

// roundLogReadBytes is how much of a retained round log is read for an
// observation's excerpt. A failure reported further in than this is not
// in the excerpt; the run's page and the file itself have it. Only the
// observations a report lists read a log, one each.
const roundLogReadBytes = 256 << 10

// getProjectObservations serves GET /projects/{project}/observations: what
// this data directory's finished runs of one project say happened
// (observation.Report). It is computed from the run records on each read,
// stores nothing, and is gated like GET /runs, whose records it is made
// from. A run record that cannot be read is left out, not an error: one
// damaged record must not hide what the others say.
func (s *Server) getProjectObservations(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeRead(r) {
		writeError(w, http.StatusForbidden, "read endpoint is not authorized")
		return
	}
	project := r.PathValue("project")
	if !validRunID(project) {
		writeError(w, http.StatusBadRequest, "project must be a single path component")
		return
	}
	entries, err := os.ReadDir(filepath.Join(s.dataDir, "runs"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusInternalServerError, "read runs")
		return
	}
	var runs []*run.Run
	for _, entry := range entries {
		if !entry.IsDir() || !validRunID(entry.Name()) {
			continue
		}
		loaded, err := run.Load(s.dataDir, entry.Name())
		// A record that names another run than its own directory is left
		// out: its id is what the round log is looked up by.
		if err != nil || loaded.ID != entry.Name() || release.ProjectOf(loaded) != project {
			continue
		}
		runs = append(runs, loaded)
	}
	// A request list that cannot be read leaves the request-derived
	// observations out, like a damaged run record.
	listed, err := request.List(s.dataDir)
	if err != nil {
		log.Printf("observations: list requests: %v", err)
	}
	var requests []*request.Request
	for _, req := range listed {
		if req.Project == project {
			requests = append(requests, req)
		}
	}
	writeJSON(w, http.StatusOK, observation.FromRuns(project, runs, requests, observation.Sources{
		RoundLog:  s.retainedRoundLog,
		Sentences: s.failedCheckSentences,
	}))
}

// failedCheckSentences is the observation.Sources sentence lookup: the
// sentence the factory gives each failed check of a run in the handoff to a
// later build attempt (triage.FailedGates).
func (s *Server) failedCheckSentences(r *run.Run) map[string]string {
	out := map[string]string{}
	for _, f := range triage.FailedGates(r, s.dataDir) {
		out[f.Check] = f.Sentence
	}
	return out
}

// retainedRoundLog is the observation.RoundLog over this data directory:
// the start of the output run.RetainBuildArtifacts kept for a run's round,
// and its name relative to the run's directory.
func (s *Server) retainedRoundLog(runID string, round int) (name, text string) {
	if !validRunID(runID) {
		return "", ""
	}
	name, data := evidence.ReadRetainedRoundLog(run.Dir(s.dataDir, runID), round, roundLogReadBytes)
	return name, string(data)
}
