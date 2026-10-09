package api

import (
	"errors"
	"net/http"
	"os"
	"time"

	"buildgate/internal/evidence"
	"buildgate/internal/run"
)

// PromptView is one entry of GET /runs/{id}/prompts: a prompt a launch of the
// run saved, as its session folder held it when the host copied it
// (evidence.PromptsDirName), kept for the operator.
type PromptView struct {
	Name    string    `json:"name"`
	Attempt string    `json:"attempt"`
	Bytes   int64     `json:"bytes"`
	Time    time.Time `json:"time"`
}

// PromptsView is GET /runs/{id}/prompts' response.
type PromptsView struct {
	Prompts []PromptView `json:"prompts"`
}

// getRunPrompts serves GET /runs/{id}/prompts: the prompts the run's launches
// saved, as the host copied them (redacted by the copy), oldest first. Operator
// only (SC-018): they quote the ticket, the record of an earlier attempt and
// failing output. Gated like GET /runs/{id}; no MCP tool replays it. A run
// with none answers an empty list.
func (s *Server) getRunPrompts(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeRead(r) {
		writeError(w, http.StatusForbidden, "read endpoint is not authorized")
		return
	}
	loaded, ok := s.loadRunForPrompts(w, r)
	if !ok {
		return
	}
	view := PromptsView{Prompts: []PromptView{}}
	for _, p := range evidence.ListSavedPrompts(run.Dir(s.dataDir, loaded.ID)) {
		view.Prompts = append(view.Prompts, PromptView{Name: p.Name, Attempt: p.Attempt, Bytes: p.Bytes, Time: p.Modified.UTC()})
	}
	writeJSON(w, http.StatusOK, view)
}

// getRunPrompt serves GET /runs/{id}/prompts/{attempt}/{name}: one saved
// prompt as text/plain. An attempt or name that is not the shape the host
// copy writes, and a prompt that is not there, are both 404.
func (s *Server) getRunPrompt(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeRead(r) {
		writeError(w, http.StatusForbidden, "read endpoint is not authorized")
		return
	}
	loaded, ok := s.loadRunForPrompts(w, r)
	if !ok {
		return
	}
	data, ok := evidence.ReadSavedPrompt(run.Dir(s.dataDir, loaded.ID), r.PathValue("attempt"), r.PathValue("name"))
	if !ok {
		writeError(w, http.StatusNotFound, "prompt not found")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// loadRunForPrompts loads the run the request names, answering 404 or 500
// itself when it cannot.
func (s *Server) loadRunForPrompts(w http.ResponseWriter, r *http.Request) (*run.Run, bool) {
	loaded, err := s.loadRun(r.PathValue("id"))
	if errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusNotFound, "run not found")
		return nil, false
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "load run")
		return nil, false
	}
	if loaded.ID != r.PathValue("id") {
		writeError(w, http.StatusNotFound, "run not found")
		return nil, false
	}
	return loaded, true
}
