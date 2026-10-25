package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"time"

	"buildgate/internal/request"
)

// maxReadBackAnswerBodyBytes caps the body PUT
// /requests/{id}/read-back/answers/{qid} reads: room for the longest answer
// (request.MaxReadBackAnswerBytes, JSON-escaped) and a by.
const maxReadBackAnswerBodyBytes = 64 << 10

// readBackAnswerBody is PUT /requests/{id}/read-back/answers/{qid}'s body:
// the answer (empty clears it) and, like the spec edit's, the operator's own
// claim of who gives it.
type readBackAnswerBody struct {
	Answer string `json:"answer"`
	By     string `json:"by,omitempty"`
}

// answerRequestReadBack serves PUT /requests/{id}/read-back/answers/{qid}:
// the console's answer to one question of a handed-over spec's read-back,
// through request.AnswerReadBack, the same function `factoryd answer`
// calls. Gated like the spec edit (authorizeRequestWrite, the loopback
// relaxation included, writePrincipal) and refused past the review gate's
// expiry; allowed only in spec_review, which AnswerReadBack checks under the
// request lock.
func (s *Server) answerRequestReadBack(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeRequestWrite(r) {
		writeError(w, http.StatusForbidden, "requests endpoint is not authorized")
		return
	}
	id := r.PathValue("id")
	if !validRequestID(id) {
		writeError(w, http.StatusNotFound, "request not found")
		return
	}
	body, ok := decodeReadBackAnswerBody(w, r)
	if !ok {
		return
	}
	by, named := s.writePrincipal(r, body.By)
	if !named {
		writeError(w, http.StatusBadRequest, badPrincipal)
		return
	}
	opts, err := s.gateOptions()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	loaded, err := request.AnswerReadBack(s.dataDir, id, r.PathValue("qid"), by, body.Answer, time.Now(), opts...)
	s.noteGateExpired(id, err)
	if err != nil {
		writeError(w, readBackAnswerStatus(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.buildRequestDetailView(s.dataDir, id, loaded))
}

// readBackAnswerStatus is the HTTP status of an AnswerReadBack refusal.
func readBackAnswerStatus(err error) int {
	switch {
	case errors.Is(err, os.ErrNotExist), errors.Is(err, request.ErrReadBackQuestionNotFound):
		return http.StatusNotFound
	case errors.Is(err, request.ErrReadBackNotAnswerable), errors.Is(err, request.ErrGateExpired):
		return http.StatusConflict
	case errors.Is(err, request.ErrReadBackAnswerTooLong):
		return http.StatusUnprocessableEntity
	default:
		return http.StatusInternalServerError
	}
}

func decodeReadBackAnswerBody(w http.ResponseWriter, r *http.Request) (readBackAnswerBody, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxReadBackAnswerBodyBytes)
	var body readBackAnswerBody
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
		} else {
			writeError(w, http.StatusBadRequest, "decode request body")
		}
		return readBackAnswerBody{}, false
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		writeError(w, http.StatusBadRequest, "request body must contain one JSON object")
		return readBackAnswerBody{}, false
	}
	if len(body.By) > request.MaxEditByLen {
		writeError(w, http.StatusBadRequest, "by is too long")
		return readBackAnswerBody{}, false
	}
	return body, true
}
