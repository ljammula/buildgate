package api

import (
	"context"
	"log"
	"net/http"
)

// ProjectMemory is what GET /projects/{project}/memory answers: whether
// repository memory is on for the project's repository, the section's budget
// and use, the lines in force (the fenced section of root AGENTS.md at the
// checkout's HEAD) and the candidate lines only the operator sees. A
// candidate carries its line and counts, never a run's log or notes.
type ProjectMemory struct {
	Project string `json:"project"`
	On      bool   `json:"on"`
	// OffReason says which of the kill switch, the session-config switch and
	// the stop marker has memory off; empty when On.
	OffReason   string `json:"off_reason,omitempty"`
	BudgetLines int    `json:"budget_lines"`
	BudgetChars int    `json:"budget_chars"`
	UsedLines   int    `json:"used_lines"`
	UsedChars   int    `json:"used_chars"`
	// InForce is the section's lines at HEAD, in order; SectionError says why
	// they could not be read.
	InForce      []string          `json:"in_force"`
	SectionError string            `json:"section_error,omitempty"`
	Candidates   []MemoryCandidate `json:"candidates"`
}

// MemoryCandidate is one candidate line: State is candidate, proposed or
// dropped, Source agent or operator, Seen the number of runs that said it.
type MemoryCandidate struct {
	ID          string `json:"id"`
	Line        string `json:"line"`
	Source      string `json:"source"`
	State       string `json:"state"`
	Seen        int    `json:"seen"`
	FirstSeenAt string `json:"first_seen_at,omitempty"`
	LastSeenAt  string `json:"last_seen_at,omitempty"`
	RequestID   string `json:"request_id,omitempty"`
}

// ProjectMemoryProvider reads a project's memory without changing anything.
// found is false for a project no request was ever submitted for.
type ProjectMemoryProvider func(ctx context.Context, project string) (memory ProjectMemory, found bool, err error)

// WithProjectMemory supplies the implementation of GET
// /projects/{project}/memory; without it the route answers 404. The reader
// lives in cmd/factoryd because the lines in force are read from git.
func WithProjectMemory(provider ProjectMemoryProvider) Option {
	return func(s *Server) {
		s.projectMemoryProvider = provider
	}
}

// getProjectMemory serves GET /projects/{project}/memory, gated like the
// project's observations. It is a read: it collects no candidate and writes
// nothing.
func (s *Server) getProjectMemory(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeRead(r) {
		writeError(w, http.StatusForbidden, "read endpoint is not authorized")
		return
	}
	project := r.PathValue("project")
	if !validRunID(project) {
		writeError(w, http.StatusBadRequest, "project must be a single path component")
		return
	}
	if s.projectMemoryProvider == nil {
		writeError(w, http.StatusNotFound, "memory endpoint is not configured")
		return
	}
	view, found, err := s.projectMemoryProvider(r.Context(), project)
	if err != nil {
		log.Printf("memory: project %s: %v", project, err)
		writeError(w, http.StatusInternalServerError, "read project memory")
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "no request was submitted for this project")
		return
	}
	if view.InForce == nil {
		view.InForce = []string{}
	}
	if view.Candidates == nil {
		view.Candidates = []MemoryCandidate{}
	}
	writeJSON(w, http.StatusOK, view)
}
