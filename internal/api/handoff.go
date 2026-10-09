package api

import (
	"errors"
	"net/http"
	"os"

	"buildgate/internal/handoff"
	"buildgate/internal/run"
)

// getRunHandoff serves GET /runs/{id}/handoff: the record a stopped run
// left for a later attempt (internal/handoff.Document), as the run's own
// save wrote it. Gated like GET /runs/{id}, whose record it restates.
//
// A run with no handoff answers 409, like a run with no diff yet: it is in
// a state that has none (running, accepted), was stopped by a path that
// writes none, or predates the handoff. A handoff that does not match the
// hash the run recorded, or that describes a state the run has left, is
// not served: 409 too, with a message that says which.
func (s *Server) getRunHandoff(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeRead(r) {
		writeError(w, http.StatusForbidden, "read endpoint is not authorized")
		return
	}
	loaded, err := s.loadRun(r.PathValue("id"))
	if errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusNotFound, "run not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "load run")
		return
	}
	// A record that names another run than the one asked for is not
	// served another run's handoff.
	if loaded.ID != r.PathValue("id") {
		writeError(w, http.StatusConflict, "run has no handoff")
		return
	}
	if loaded.HandoffSHA256 == "" {
		writeError(w, http.StatusConflict, "run has no handoff")
		return
	}
	doc, err := handoff.Load(run.Dir(s.dataDir, loaded.ID), loaded.HandoffSHA256, loaded.State)
	if err != nil {
		writeError(w, http.StatusConflict, "run's handoff cannot be used: it is missing, was changed after the run recorded it, or describes a state the run has left")
		return
	}
	writeJSON(w, http.StatusOK, doc)
}
