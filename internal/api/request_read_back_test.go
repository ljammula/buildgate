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
	recorder := putAnswer(t, dataDir, "Q2", `{"answer":"Yes, per merchant.","by":"alice"}`)
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
	if recorder := putAnswer(t, dataDir, "Q2", `{"answer":""}`); recorder.Code != http.StatusOK {
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
		{"unknown question", "Q7", `{"answer":"x"}`, http.StatusNotFound},
		{"too long", "Q1", `{"answer":"` + strings.Repeat("a", request.MaxReadBackAnswerBytes+1) + `"}`, http.StatusUnprocessableEntity},
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
	if recorder := putAnswer(t, dataDir, "Q1", `{"answer":"x"}`); recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", recorder.Code, recorder.Body.String())
	}
}
