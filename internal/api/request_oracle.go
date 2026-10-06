package api

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"

	"buildgate/internal/request"
)

// requestOracleListingView is GET /requests/{id}/oracle's JSON shape.
type requestOracleListingView struct {
	Files       []request.OracleFile `json:"files"`
	Problems    []string             `json:"problems"`
	OracleDraft *request.OracleDraft `json:"oracle_draft"`
	State       request.State        `json:"state"`
}

// requestOracleWriteView is PUT /requests/{id}/oracle/RUN_COMMAND.txt's response.
type requestOracleWriteView struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
}

// loadRequestForOracle applies the neighbours' id validation and not-found
// mapping; ok=false means a response was already written.
func (s *Server) loadRequestForOracle(w http.ResponseWriter, id string) (*request.Request, bool) {
	if !validRequestID(id) {
		writeError(w, http.StatusNotFound, "request not found")
		return nil, false
	}
	loaded, err := request.Load(s.dataDir, id)
	if errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusNotFound, "request not found")
		return nil, false
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "load request")
		return nil, false
	}
	return loaded, true
}

// listRequestOracle serves GET /requests/{id}/oracle: the request-level oracle/
// files with hashes, plus every problem approval would refuse. Read token,
// like GET /requests/{id}. A missing directory is an empty listing.
func (s *Server) listRequestOracle(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeRead(r) {
		writeError(w, http.StatusForbidden, "requests endpoint is not authorized")
		return
	}
	id := r.PathValue("id")
	loaded, ok := s.loadRequestForOracle(w, id)
	if !ok {
		return
	}
	listing, err := request.ListOracle(s.dataDir, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list oracle files")
		return
	}
	writeJSON(w, http.StatusOK, requestOracleListingView{
		Files:       listing.Files,
		Problems:    listing.Problems,
		OracleDraft: loaded.OracleDraft,
		State:       loaded.State,
	})
}

// getRequestOracleFile serves GET /requests/{id}/oracle/{name}: one file's
// content as text/plain, its sha256 in X-Content-SHA256. Read token.
func (s *Server) getRequestOracleFile(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeRead(r) {
		writeError(w, http.StatusForbidden, "requests endpoint is not authorized")
		return
	}
	id := r.PathValue("id")
	if _, ok := s.loadRequestForOracle(w, id); !ok {
		return
	}
	content, sum, err := request.GetOracleFile(s.dataDir, id, r.PathValue("name"))
	writeOracleFileResponse(w, content, sum, err)
}

// writeOracleFileResponse maps a GetOracleFile / GetTicketOracleFile result
// onto the HTTP response both oracle file routes share.
func writeOracleFileResponse(w http.ResponseWriter, content []byte, sum string, err error) {
	switch {
	case errors.Is(err, request.ErrOracleFileName):
		writeError(w, http.StatusBadRequest, err.Error())
		return
	case errors.Is(err, request.ErrOracleFileNotFound):
		writeError(w, http.StatusNotFound, "oracle file not found")
		return
	case errors.Is(err, request.ErrOracleFileTooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, err.Error())
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, "read oracle file")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-SHA256", sum)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Length", strconv.Itoa(len(content)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(content)
}

// ticketOracleListingView is GET /requests/{id}/tickets/{n}/oracle's shape.
type ticketOracleListingView struct {
	Files    []request.OracleFile `json:"files"`
	Problems []string             `json:"problems"`
	State    request.State        `json:"state"`
}

// loadTicketSpecFileForOracle resolves ticket n of a request that is in
// plan_review (the only state its oracle directory is what plan approval will
// pin); ok=false means a response was already written.
func (s *Server) loadTicketSpecFileForOracle(w http.ResponseWriter, r *http.Request) (id, specFile string, state request.State, ok bool) {
	if !s.authorizeRead(r) {
		writeError(w, http.StatusForbidden, "requests endpoint is not authorized")
		return "", "", "", false
	}
	id = r.PathValue("id")
	n, convErr := strconv.Atoi(r.PathValue("n"))
	if convErr != nil || n <= 0 {
		writeError(w, http.StatusNotFound, "ticket not found")
		return "", "", "", false
	}
	loaded, found := s.loadRequestForOracle(w, id)
	if !found {
		return "", "", "", false
	}
	if loaded.State != request.StatePlanReview {
		writeError(w, http.StatusConflict, "ticket oracle files are only served at plan_review")
		return "", "", "", false
	}
	for _, ticket := range loaded.Tickets {
		if ticket.Index == n && ticket.SpecPath != "" {
			return id, filepath.Base(ticket.SpecPath), loaded.State, true
		}
	}
	writeError(w, http.StatusNotFound, "ticket not found")
	return "", "", "", false
}

// listTicketOracle serves GET /requests/{id}/tickets/{n}/oracle: the
// materialized <NNN>.oracle/ files plan approval will hash-pin. Read token.
func (s *Server) listTicketOracle(w http.ResponseWriter, r *http.Request) {
	id, specFile, state, ok := s.loadTicketSpecFileForOracle(w, r)
	if !ok {
		return
	}
	listing, err := request.ListTicketOracle(s.dataDir, id, specFile)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list ticket oracle files")
		return
	}
	writeJSON(w, http.StatusOK, ticketOracleListingView{Files: listing.Files, Problems: listing.Problems, State: state})
}

// getTicketOracleFile serves GET /requests/{id}/tickets/{n}/oracle/{name}:
// one file as text/plain with its sha256 in X-Content-SHA256. Read token.
func (s *Server) getTicketOracleFile(w http.ResponseWriter, r *http.Request) {
	id, specFile, _, ok := s.loadTicketSpecFileForOracle(w, r)
	if !ok {
		return
	}
	content, sum, err := request.GetTicketOracleFile(s.dataDir, id, specFile, r.PathValue("name"))
	writeOracleFileResponse(w, content, sum, err)
}

// putRequestOracleFile serves PUT /requests/{id}/oracle/{name}: writes
// RUN_COMMAND.txt only (any other name is 403), only at oracle_review (409
// otherwise), under the request lock approval holds. Body: {"content": "..."}
// like PUT /requests/{id}/spec (a "by" field is accepted and ignored: no
// operator-edit history pattern exists to record it in). Gated like
// approve/reject (override token).
func (s *Server) putRequestOracleFile(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeRequestWrite(r) {
		writeError(w, http.StatusForbidden, "requests endpoint is not authorized")
		return
	}
	id := r.PathValue("id")
	if !validRequestID(id) {
		writeError(w, http.StatusNotFound, "request not found")
		return
	}
	name := r.PathValue("name")
	if name != request.TicketOracleRunCommandFilename {
		writeError(w, http.StatusForbidden, request.ErrOracleNotEditable.Error())
		return
	}
	body, ok := decodeRequestContentBody(w, r)
	if !ok {
		return
	}
	// base_sha256 (if given) is checked inside SetOracleRunCommand
	// itself, under the same request lock as the write -- not here, and
	// not via a separate read beforehand. A separate pre-lock read (this
	// handler's own prior shape) left a window between the read and the
	// lock where a concurrent PUT or an approval's own hash-then-pin could
	// change the file out from under a check that had already passed
	// (fixed after an adversarial review, 2026-09-24); see
	// SetOracleRunCommand's own doc comment.
	sum, err := request.SetOracleRunCommand(s.dataDir, id, name, body.Content, body.BaseSHA256)
	var stale *request.OracleRunCommandStaleError
	switch {
	case errors.Is(err, os.ErrNotExist):
		writeError(w, http.StatusNotFound, "request not found")
	case errors.As(err, &stale):
		// Same response shape as verifyBaseSHA256's own mismatch --
		// spec/ticket PUT's convention for "content changed since it was
		// fetched".
		writeJSON(w, http.StatusConflict, map[string]string{
			"error":          err.Error(),
			"current_sha256": stale.Current,
		})
	case errors.Is(err, request.ErrOracleWrongState):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, request.ErrOracleInvalid):
		writeError(w, http.StatusUnprocessableEntity, err.Error())
	case err != nil:
		writeError(w, http.StatusInternalServerError, "write oracle file")
	default:
		writeJSON(w, http.StatusOK, requestOracleWriteView{Name: name, SHA256: sum})
	}
}
