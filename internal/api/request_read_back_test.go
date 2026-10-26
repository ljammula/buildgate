package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"buildgate/internal/request"
)

// seedReadBackRequest seeds req-1 in spec_review with a two-question
// read-back.
func seedReadBackRequest(t *testing.T, dataDir string) {
	t.Helper()
	seedApprovableRequest(t, dataDir, "req-1", request.StateSpecReview, false)
	r, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	r.SpecImported = true
	r.ReadBack = &request.ReadBack{
		Summary: "Makes refunds idempotent.",
		Questions: []request.ReadBackQuestion{
			{ID: "Q1", Text: "How long is a key kept?"},
			{ID: "Q2", Text: "Is the key scoped per merchant?"},
		},
	}
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
}

var (
	q1Hash = request.QuestionSHA256("How long is a key kept?")
	q2Hash = request.QuestionSHA256("Is the key scoped per merchant?")
)

func firstAnswerBy(r *request.Request) string { return r.ReadBack.Questions[0].AnsweredBy }

func putAnswer(t *testing.T, dataDir, qid, body string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	NewServer(dataDir, WithOverrideToken("test-token")).ServeHTTP(recorder, requestActionFor(t, http.MethodPut, "/requests/req-1/read-back/answers/"+qid, "test-token", body))
	return recorder
}

func TestAnswerReadBackHandlerRecordsTheAnswer(t *testing.T) {
	dataDir := t.TempDir()
	seedReadBackRequest(t, dataDir)
	recorder := putAnswer(t, dataDir, "Q2", `{"answer":"Yes, per merchant.","by":"alice","question_sha256":"`+q2Hash+`"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
	}
	var view struct {
		ReadBack *request.ReadBack `json:"read_back"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.ReadBack == nil || view.ReadBack.Questions[1].Answer != "Yes, per merchant." || view.ReadBack.Questions[1].AnsweredBy != "alice" {
		t.Fatalf("response read-back = %+v", view.ReadBack)
	}
	loaded, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ReadBack.Questions[1].Answer != "Yes, per merchant." {
		t.Fatalf("saved read-back = %+v", loaded.ReadBack)
	}
	if recorder := putAnswer(t, dataDir, "Q2", `{"answer":"","question_sha256":"`+q2Hash+`"}`); recorder.Code != http.StatusOK {
		t.Fatalf("clear: status = %d: %s", recorder.Code, recorder.Body.String())
	}
	if loaded, _ := request.Load(dataDir, "req-1"); loaded.ReadBack.Questions[1].Answer != "" {
		t.Fatalf("answer not cleared: %+v", loaded.ReadBack.Questions[1])
	}
}

func TestAnswerReadBackHandlerRefusals(t *testing.T) {
	dataDir := t.TempDir()
	seedReadBackRequest(t, dataDir)
	cases := []struct {
		name, qid, body string
		want            int
	}{
		{"unknown question", "Q7", `{"answer":"x","question_sha256":"` + q1Hash + `"}`, http.StatusNotFound},
		{"too long", "Q1", `{"answer":"` + strings.Repeat("a", request.MaxReadBackAnswerBytes+1) + `","question_sha256":"` + q1Hash + `"}`, http.StatusUnprocessableEntity},
		{"no question hash", "Q1", `{"answer":"x"}`, http.StatusBadRequest},
		{"another question's text", "Q1", `{"answer":"x","question_sha256":"` + q2Hash + `"}`, http.StatusConflict},
		{"unknown field", "Q1", `{"answer":"x","verify_command":"rm -rf /"}`, http.StatusBadRequest},
		{"two objects", "Q1", `{"answer":"x"}{"answer":"y"}`, http.StatusBadRequest},
		{"claims the gate suffix", "Q1", `{"answer":"x","by":"mallory (gate token)"}`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		if recorder := putAnswer(t, dataDir, tc.qid, tc.body); recorder.Code != tc.want {
			t.Errorf("%s: status = %d, want %d: %s", tc.name, recorder.Code, tc.want, recorder.Body.String())
		}
	}
	recorder := httptest.NewRecorder()
	NewServer(dataDir, WithOverrideToken("test-token")).ServeHTTP(recorder, requestActionFor(t, http.MethodPut, "/requests/req-1/read-back/answers/Q1", "wrong-token", `{"answer":"x"}`))
	if recorder.Code != http.StatusForbidden {
		t.Errorf("wrong token: status = %d, want 403", recorder.Code)
	}
	if loaded, _ := request.Load(dataDir, "req-1"); len(loaded.ReadBack.Answered()) != 0 {
		t.Fatalf("a refused write recorded an answer: %+v", loaded.ReadBack)
	}
}

func TestAnswerReadBackHandlerWrongStateConflict(t *testing.T) {
	dataDir := t.TempDir()
	seedReadBackRequest(t, dataDir)
	r, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	r.State = request.StatePlanning
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	if recorder := putAnswer(t, dataDir, "Q1", `{"answer":"x","question_sha256":"`+q1Hash+`"}`); recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", recorder.Code, recorder.Body.String())
	}
}

// A spec approval from a client that shows the read-back must carry the
// digest of the answers it showed: other answers, or none when some are
// answered, are refused with a 409 and the request stays at spec_review.
func TestApproveRefusesReadBackAnswersTheClientDidNotShow(t *testing.T) {
	dataDir := t.TempDir()
	seedReadBackRequest(t, dataDir)
	if recorder := putAnswer(t, dataDir, "Q1", `{"answer":"A day.","question_sha256":"`+q1Hash+`"}`); recorder.Code != http.StatusOK {
		t.Fatalf("answer: %d %s", recorder.Code, recorder.Body.String())
	}
	specHash, err := request.HashFile(dataDir, "req-1", "spec.md")
	if err != nil {
		t.Fatal(err)
	}
	approve := func(expected map[string]string) *httptest.ResponseRecorder {
		body, err := json.Marshal(map[string]any{"by": "alice", "expected_sha256": expected})
		if err != nil {
			t.Fatal(err)
		}
		recorder := httptest.NewRecorder()
		NewServer(dataDir, WithOverrideToken("test-token")).ServeHTTP(recorder, requestActionFor(t, http.MethodPost, "/requests/req-1/approve", "test-token", string(body)))
		return recorder
	}
	stale := request.ReadBackShownSHA256(nil)
	for name, expected := range map[string]map[string]string{
		"not shown":  {"spec.md": specHash},
		"none shown": {"spec.md": specHash, request.ReadBackAnswersKey: stale},
	} {
		if recorder := approve(expected); recorder.Code != http.StatusConflict {
			t.Errorf("%s: status = %d, want 409: %s", name, recorder.Code, recorder.Body.String())
		}
	}
	loaded, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.State != request.StateSpecReview {
		t.Fatalf("a refused approval moved the request to %q", loaded.State)
	}
	if recorder := approve(map[string]string{"spec.md": specHash, request.ReadBackAnswersKey: request.ReadBackShownSHA256(loaded.ReadBack)}); recorder.Code != http.StatusOK {
		t.Fatalf("approve with the answers shown: %d %s", recorder.Code, recorder.Body.String())
	}
}
