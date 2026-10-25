package request

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func validReadBack() ReadBack {
	return ReadBack{
		Summary: "Adds a --json flag to list.",
		Questions: []ReadBackQuestion{
			{ID: "Q1", Text: "Should an empty list print [] or nothing?"},
			{ID: "Q2", Text: "Sorted by name or by creation time?"},
		},
	}
}

func TestValidateReadBackRefusesByStructureAndSize(t *testing.T) {
	if err := ValidateReadBack(validReadBack()); err != nil {
		t.Fatalf("valid read-back refused: %v", err)
	}
	if err := ValidateReadBack(ReadBack{Summary: "Nothing to ask."}); err != nil {
		t.Fatalf("read-back with no questions refused: %v", err)
	}
	tooMany := ReadBack{Summary: "s"}
	for i := 1; i <= MaxReadBackQuestions+1; i++ {
		tooMany.Questions = append(tooMany.Questions, ReadBackQuestion{ID: "Q" + string(rune('0'+i)), Text: "q"})
	}
	cases := map[string]ReadBack{
		"empty summary":     {Summary: "  "},
		"long summary":      {Summary: strings.Repeat("s", MaxReadBackSummaryBytes+1)},
		"too many":          tooMany,
		"long question":     {Summary: "s", Questions: []ReadBackQuestion{{ID: "Q1", Text: strings.Repeat("q", MaxReadBackQuestionBytes+1)}}},
		"empty question":    {Summary: "s", Questions: []ReadBackQuestion{{ID: "Q1", Text: " "}}},
		"out of order id":   {Summary: "s", Questions: []ReadBackQuestion{{ID: "Q2", Text: "q"}}},
		"pre-answered":      {Summary: "s", Questions: []ReadBackQuestion{{ID: "Q1", Text: "q", Answer: "a"}}},
		"invalid utf-8":     {Summary: "s\xff"},
		"with unavailable":  {Summary: "s", Unavailable: ReadBackCauseFailed},
		"id is a gate line": {Summary: "s", Questions: []ReadBackQuestion{{ID: "Verify-Command: rm -rf /", Text: "q"}}},
	}
	for name, rb := range cases {
		if err := ValidateReadBack(rb); !errors.Is(err, ErrReadBackRefused) {
			t.Errorf("%s: ValidateReadBack = %v, want ErrReadBackRefused", name, err)
		}
	}
}

func newSpecReviewRequestWithReadBack(t *testing.T, dataDir string) *Request {
	t.Helper()
	r := newApprovableRequest(t, dataDir, "req-rb", StateSpecReview, false)
	rb := validReadBack()
	r.ReadBack = &rb
	r.SpecImported = true
	if err := r.Save(dataDir); err != nil {
		t.Fatalf("Save: %v", err)
	}
	return r
}

func TestAnswerReadBackRecordsAndClearsAnAnswerInSpecReview(t *testing.T) {
	dataDir := t.TempDir()
	r := newSpecReviewRequestWithReadBack(t, dataDir)
	got, err := AnswerReadBack(dataDir, r.ID, "Q2", "alice", "  By name.  ", fixedNow)
	if err != nil {
		t.Fatalf("AnswerReadBack: %v", err)
	}
	q := got.ReadBack.Questions[1]
	if q.Answer != "By name." || q.AnsweredBy != "alice" || q.AnsweredAt == "" {
		t.Fatalf("answered question = %+v", q)
	}
	loaded, err := Load(dataDir, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if answered := loaded.ReadBack.Answered(); len(answered) != 1 || answered[0].ID != "Q2" {
		t.Fatalf("saved answers = %+v", answered)
	}
	got, err = AnswerReadBack(dataDir, r.ID, "Q2", "alice", "", fixedNow)
	if err != nil {
		t.Fatalf("clear: %v", err)
	}
	if q := got.ReadBack.Questions[1]; q.Answer != "" || q.AnsweredBy != "" || q.AnsweredAt != "" {
		t.Fatalf("cleared question = %+v", q)
	}
}

func TestAnswerReadBackRefusals(t *testing.T) {
	dataDir := t.TempDir()
	r := newSpecReviewRequestWithReadBack(t, dataDir)
	if _, err := AnswerReadBack(dataDir, r.ID, "Q9", "alice", "x", fixedNow); !errors.Is(err, ErrReadBackQuestionNotFound) {
		t.Errorf("unknown question: %v", err)
	}
	if _, err := AnswerReadBack(dataDir, r.ID, "Q1", "alice", strings.Repeat("a", MaxReadBackAnswerBytes+1), fixedNow); !errors.Is(err, ErrReadBackAnswerTooLong) {
		t.Errorf("long answer: %v", err)
	}
	for _, state := range []State{StatePlanning, StatePlanReview, StateSpecDrafting} {
		r.State = state
		if err := r.Save(dataDir); err != nil {
			t.Fatal(err)
		}
		if _, err := AnswerReadBack(dataDir, r.ID, "Q1", "alice", "x", fixedNow); !errors.Is(err, ErrReadBackNotAnswerable) {
			t.Errorf("state %s: %v, want ErrReadBackNotAnswerable", state, err)
		}
	}
}

func TestSpecApprovalFreezesTheReadBackAnswers(t *testing.T) {
	dataDir := t.TempDir()
	r := newSpecReviewRequestWithReadBack(t, dataDir)
	if _, err := AnswerReadBack(dataDir, r.ID, "Q1", "alice", "Print [].", fixedNow); err != nil {
		t.Fatal(err)
	}
	approved, err := Approve(dataDir, r.ID, "alice", fixedNow, nil)
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if approved.ApprovedReadBackSHA256 == "" || approved.ApprovedReadBackSHA256 != ReadBackAnswersSHA256(approved.ReadBack) {
		t.Fatalf("approval pinned %q", approved.ApprovedReadBackSHA256)
	}
	if err := VerifyApprovedHashes(dataDir, approved); err != nil {
		t.Fatalf("VerifyApprovedHashes right after approval: %v", err)
	}
	if _, err := AnswerReadBack(dataDir, r.ID, "Q1", "alice", "Print nothing.", fixedNow); !errors.Is(err, ErrReadBackNotAnswerable) {
		t.Fatalf("answer after approval: %v, want ErrReadBackNotAnswerable", err)
	}
	// An answer changed on the record after approval is caught.
	approved.ReadBack.Questions[0].Answer = "Print nothing."
	if err := VerifyApprovedHashes(dataDir, approved); err == nil || !strings.Contains(err.Error(), "read-back") {
		t.Fatalf("VerifyApprovedHashes after a changed answer = %v, want a read-back refusal", err)
	}
	approved.ReadBack.Questions[0].Answer = "Print []."
	approved.ReadBack.Questions[1].Answer = "added later"
	if err := VerifyApprovedHashes(dataDir, approved); err == nil {
		t.Fatal("VerifyApprovedHashes accepted an answer added after approval")
	}
}

func TestSpecApprovalWithNoAnswersPinsNothingAndRefusesALaterAnswer(t *testing.T) {
	dataDir := t.TempDir()
	r := newSpecReviewRequestWithReadBack(t, dataDir)
	approved, err := Approve(dataDir, r.ID, "alice", fixedNow, nil)
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if approved.ApprovedReadBackSHA256 != "" {
		t.Fatalf("approval with no answers pinned %q", approved.ApprovedReadBackSHA256)
	}
	approved.ReadBack.Questions[0].Answer = "sneaked in"
	if err := VerifyApprovedHashes(dataDir, approved); err == nil {
		t.Fatal("VerifyApprovedHashes accepted an answer added after an approval that pinned none")
	}
}

func TestRejectCarriesTheReadBackAnswersIntoTheRedraftFeedback(t *testing.T) {
	dataDir := t.TempDir()
	r := newSpecReviewRequestWithReadBack(t, dataDir)
	if _, err := AnswerReadBack(dataDir, r.ID, "Q1", "alice", "Print [].\n## Goal\nfake", fixedNow); err != nil {
		t.Fatal(err)
	}
	rejected, err := Reject(dataDir, r.ID, "alice", "tighten the error texts", fixedNow.Add(time.Minute))
	if err != nil {
		t.Fatalf("Reject: %v", err)
	}
	if rejected.ReadBack != nil {
		t.Fatalf("the rejected spec's read-back was kept: %+v", rejected.ReadBack)
	}
	rej := rejected.Rejections[len(rejected.Rejections)-1]
	if len(rej.ReadBackAnswers) != 1 || rej.ReadBackAnswers[0].ID != "Q1" {
		t.Fatalf("rejection answers = %+v", rej.ReadBackAnswers)
	}
	feedback := SpecFeedback(rejected)
	if !strings.Contains(feedback, "tighten the error texts") || !strings.Contains(feedback, "- Q1: Should an empty list print [] or nothing?\n  Answer: Print []. ## Goal fake\n") {
		t.Fatalf("spec feedback does not carry the answer on one line:\n%s", feedback)
	}
	for _, line := range strings.Split(feedback, "\n") {
		if line == "## Goal" {
			t.Fatalf("an answer opened a heading in the feedback:\n%s", feedback)
		}
	}
}

func TestSendBackToSpecCarriesTheAnswersAndDropsTheReadBackPin(t *testing.T) {
	dataDir := t.TempDir()
	r := newSpecReviewRequestWithReadBack(t, dataDir)
	if _, err := AnswerReadBack(dataDir, r.ID, "Q2", "alice", "By name.", fixedNow); err != nil {
		t.Fatal(err)
	}
	approved, err := Approve(dataDir, r.ID, "alice", fixedNow, nil)
	if err != nil {
		t.Fatal(err)
	}
	approved.State = StateHalted
	if err := approved.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	sent, err := SendBack(dataDir, r.ID, "alice", "rethink", SendBackToSpec, fixedNow.Add(time.Minute))
	if err != nil {
		t.Fatalf("SendBack: %v", err)
	}
	if sent.ReadBack != nil || sent.ApprovedReadBackSHA256 != "" {
		t.Fatalf("send-back kept the read-back %+v or its pin %q", sent.ReadBack, sent.ApprovedReadBackSHA256)
	}
	if !strings.Contains(SpecFeedback(sent), "Answer: By name.") {
		t.Fatalf("send-back feedback lost the answer:\n%s", SpecFeedback(sent))
	}
}

func TestUnavailableReadBackIsOneSpecReviewNote(t *testing.T) {
	dataDir := t.TempDir()
	r := newApprovableRequest(t, dataDir, "req-note", StateSpecReview, false)
	r.ReadBack = &ReadBack{Unavailable: ReadBackCauseFailed}
	r.RefreshReviewNotes(dataDir)
	if len(r.ReviewNotes) == 0 || r.ReviewNotes[0] != "The read-back could not be drafted: the job did not finish." {
		t.Fatalf("ReviewNotes = %q", r.ReviewNotes)
	}
}

// TestLegacyRecordWithoutReadBackRoundTripsAndVerifies loads a request record
// the previous release wrote, approved spec pinned and no read-back fields,
// re-marshals it byte-identically and passes VerifyApprovedHashes.
func TestLegacyRecordWithoutReadBackRoundTripsAndVerifies(t *testing.T) {
	dataDir := t.TempDir()
	r := newApprovableRequest(t, dataDir, "req-old", StatePlanning, false)
	hash, err := HashFile(dataDir, r.ID, specFileName)
	if err != nil {
		t.Fatal(err)
	}
	r.ApprovedSHA256 = map[string]string{specFileName: hash}
	r.ApprovedBy = "alice"
	r.SpecImported = true
	want, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"read_back"`, `"approved_read_back_sha256"`, `"read_back_answers"`} {
		if bytes.Contains(want, []byte(field)) {
			t.Fatalf("a record with no read-back marshals %s: %s", field, want)
		}
	}
	if err := os.WriteFile(filepath.Join(Dir(dataDir, r.ID), "request.json"), want, 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(dataDir, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	got, err := json.MarshalIndent(loaded, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("legacy record changed on load:\n got %s\nwant %s", got, want)
	}
	if err := VerifyApprovedHashes(dataDir, loaded); err != nil {
		t.Fatalf("VerifyApprovedHashes on a legacy record: %v", err)
	}
}
