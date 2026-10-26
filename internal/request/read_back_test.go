package request

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
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

// shownSHA is QuestionSHA256 of question qid's stored text, what a client
// showing the read-back sends with an answer; "" for no such question.
func shownSHA(t *testing.T, dataDir, id, qid string) string {
	t.Helper()
	r, err := Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	if q := r.readBackQuestion(qid); q != nil {
		return QuestionSHA256(q.Text)
	}
	return ""
}

func TestAnswerReadBackRecordsAndClearsAnAnswerInSpecReview(t *testing.T) {
	dataDir := t.TempDir()
	r := newSpecReviewRequestWithReadBack(t, dataDir)
	got, err := AnswerReadBack(dataDir, r.ID, "Q2", shownSHA(t, dataDir, r.ID, "Q2"), "alice", "  By name.  ", fixedNow)
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
	got, err = AnswerReadBack(dataDir, r.ID, "Q2", shownSHA(t, dataDir, r.ID, "Q2"), "alice", "", fixedNow)
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
	if _, err := AnswerReadBack(dataDir, r.ID, "Q9", shownSHA(t, dataDir, r.ID, "Q9"), "alice", "x", fixedNow); !errors.Is(err, ErrReadBackQuestionNotFound) {
		t.Errorf("unknown question: %v", err)
	}
	if _, err := AnswerReadBack(dataDir, r.ID, "Q1", shownSHA(t, dataDir, r.ID, "Q1"), "alice", strings.Repeat("a", MaxReadBackAnswerBytes+1), fixedNow); !errors.Is(err, ErrReadBackAnswerTooLong) {
		t.Errorf("long answer: %v", err)
	}
	for _, state := range []State{StatePlanning, StatePlanReview, StateSpecDrafting} {
		r.State = state
		if err := r.Save(dataDir); err != nil {
			t.Fatal(err)
		}
		if _, err := AnswerReadBack(dataDir, r.ID, "Q1", shownSHA(t, dataDir, r.ID, "Q1"), "alice", "x", fixedNow); !errors.Is(err, ErrReadBackNotAnswerable) {
			t.Errorf("state %s: %v, want ErrReadBackNotAnswerable", state, err)
		}
	}
}

func TestSpecApprovalFreezesTheReadBackAnswers(t *testing.T) {
	dataDir := t.TempDir()
	r := newSpecReviewRequestWithReadBack(t, dataDir)
	if _, err := AnswerReadBack(dataDir, r.ID, "Q1", shownSHA(t, dataDir, r.ID, "Q1"), "alice", "Print [].", fixedNow); err != nil {
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
	if _, err := AnswerReadBack(dataDir, r.ID, "Q1", shownSHA(t, dataDir, r.ID, "Q1"), "alice", "Print nothing.", fixedNow); !errors.Is(err, ErrReadBackNotAnswerable) {
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
	if _, err := AnswerReadBack(dataDir, r.ID, "Q1", shownSHA(t, dataDir, r.ID, "Q1"), "alice", "Print [].\n## Goal\nfake", fixedNow); err != nil {
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
	if _, err := AnswerReadBack(dataDir, r.ID, "Q2", shownSHA(t, dataDir, r.ID, "Q2"), "alice", "By name.", fixedNow); err != nil {
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
	for _, field := range []string{`"read_back"`, `"approved_read_back_sha256"`, `"read_back_answers"`, `"read_back_answer_log"`} {
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

func TestAnswerReadBackRefusesAnAnswerToAQuestionTextNoLongerStored(t *testing.T) {
	dataDir := t.TempDir()
	r := newSpecReviewRequestWithReadBack(t, dataDir)
	shown := QuestionSHA256("Sorted by name or by creation time?")
	// The question was replaced (a new read-back) after the client showed it.
	r.ReadBack.Questions[1].Text = "Sorted by size?"
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	if _, err := AnswerReadBack(dataDir, r.ID, "Q2", shown, "alice", "By name.", fixedNow); !errors.Is(err, ErrReadBackQuestionChanged) {
		t.Fatalf("stale question: %v, want ErrReadBackQuestionChanged", err)
	}
	if _, err := AnswerReadBack(dataDir, r.ID, "Q2", "", "alice", "By name.", fixedNow); !errors.Is(err, ErrReadBackQuestionChanged) {
		t.Fatalf("no question hash: %v, want ErrReadBackQuestionChanged", err)
	}
	loaded, err := Load(dataDir, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.ReadBack.Answered()) != 0 || len(loaded.ReadBackAnswerLog) != 0 {
		t.Fatalf("a refused answer was recorded: %+v %+v", loaded.ReadBack, loaded.ReadBackAnswerLog)
	}
}

func TestAnswerReadBackLogsWhoSetChangedAndClearedWhichQuestion(t *testing.T) {
	dataDir := t.TempDir()
	r := newSpecReviewRequestWithReadBack(t, dataDir)
	for i, step := range []struct{ by, answer string }{{"alice", "By name."}, {"bob", "By name."}, {"bob", "By time."}, {"carol", ""}} {
		if _, err := AnswerReadBack(dataDir, r.ID, "Q2", shownSHA(t, dataDir, r.ID, "Q2"), step.by, step.answer, fixedNow.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	loaded, err := Load(dataDir, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	// bob's repeat of the stored answer changes nothing and is not logged.
	want := []ReadBackAnswerEvent{
		{QuestionID: "Q2", Action: ReadBackAnswerSet, By: "alice", At: fixedNow.UTC().Format(time.RFC3339Nano)},
		{QuestionID: "Q2", Action: ReadBackAnswerChanged, By: "bob", At: fixedNow.Add(2 * time.Minute).UTC().Format(time.RFC3339Nano)},
		{QuestionID: "Q2", Action: ReadBackAnswerCleared, By: "carol", At: fixedNow.Add(3 * time.Minute).UTC().Format(time.RFC3339Nano)},
	}
	if len(loaded.ReadBackAnswerLog) != len(want) {
		t.Fatalf("answer log = %+v, want %+v", loaded.ReadBackAnswerLog, want)
	}
	for i := range want {
		if loaded.ReadBackAnswerLog[i] != want[i] {
			t.Fatalf("answer log[%d] = %+v, want %+v", i, loaded.ReadBackAnswerLog[i], want[i])
		}
	}
	data, err := json.Marshal(loaded.ReadBackAnswerLog)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("By time")) || bytes.Contains(data, []byte("Sorted by")) {
		t.Fatalf("the answer log holds question or answer text: %s", data)
	}
}

func TestApproveShownBindsTheReadBackAnswersTheClientShowed(t *testing.T) {
	dataDir := t.TempDir()
	r := newSpecReviewRequestWithReadBack(t, dataDir)
	if _, err := AnswerReadBack(dataDir, r.ID, "Q1", shownSHA(t, dataDir, r.ID, "Q1"), "alice", "Print [].", fixedNow); err != nil {
		t.Fatal(err)
	}
	specHash, err := HashFile(dataDir, r.ID, specFileName)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(dataDir, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	shown := ReadBackShownSHA256(loaded.ReadBack)
	stale := ReadBackShownSHA256(&ReadBack{Questions: []ReadBackQuestion{{ID: "Q1", Text: loaded.ReadBack.Questions[0].Text, Answer: "Print nothing."}}})
	refused := map[string]map[string]string{
		"answers not shown":    {specFileName: specHash},
		"other answers shown":  {specFileName: specHash, ReadBackAnswersKey: stale},
		"no answers shown":     {specFileName: specHash, ReadBackAnswersKey: ReadBackShownSHA256(nil)},
		"nothing shown at all": nil,
		"answers without spec": {ReadBackAnswersKey: shown},
	}
	for name, expected := range refused {
		if _, err := ApproveShown(dataDir, r.ID, "alice", fixedNow, expected); !errors.Is(err, ErrReadBackNotShown) {
			t.Errorf("%s: ApproveShown = %v, want ErrReadBackNotShown", name, err)
		}
	}
	if got, err := Load(dataDir, r.ID); err != nil || got.State != StateSpecReview {
		t.Fatalf("a refused approval moved the request: %v %v", got.State, err)
	}
	approved, err := ApproveShown(dataDir, r.ID, "alice", fixedNow, map[string]string{specFileName: specHash, ReadBackAnswersKey: shown})
	if err != nil {
		t.Fatalf("ApproveShown with the answers shown: %v", err)
	}
	if approved.ApprovedReadBackSHA256 != ReadBackAnswersSHA256(approved.ReadBack) || approved.ApprovedSHA256[ReadBackAnswersKey] != "" {
		t.Fatalf("approval pinned %q, files %v", approved.ApprovedReadBackSHA256, approved.ApprovedSHA256)
	}
}

// An answers digest binds answers to the spec they answer: sent without
// spec.md's hash it pins nothing, so it is refused from any client.
func TestAnAnswersDigestWithoutTheSpecHashIsRefused(t *testing.T) {
	dataDir := t.TempDir()
	r := newSpecReviewRequestWithReadBack(t, dataDir)
	for name, approveWith := range map[string]func(map[string]string) (*Request, error){
		"ApproveShown": func(e map[string]string) (*Request, error) { return ApproveShown(dataDir, r.ID, "alice", fixedNow, e) },
		"Approve":      func(e map[string]string) (*Request, error) { return Approve(dataDir, r.ID, "alice", fixedNow, e) },
	} {
		if _, err := approveWith(map[string]string{ReadBackAnswersKey: ReadBackShownSHA256(nil)}); !errors.Is(err, ErrReadBackNotShown) {
			t.Errorf("%s with only the answers key = %v, want ErrReadBackNotShown", name, err)
		}
	}
	if got, err := Load(dataDir, r.ID); err != nil || got.State != StateSpecReview {
		t.Fatalf("a refused approval moved the request: %v %v", got.State, err)
	}
}

func TestApproveShownWithNoAnswersNeedsNoReadBackDigest(t *testing.T) {
	dataDir := t.TempDir()
	r := newSpecReviewRequestWithReadBack(t, dataDir)
	specHash, err := HashFile(dataDir, r.ID, specFileName)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ApproveShown(dataDir, r.ID, "alice", fixedNow, map[string]string{specFileName: specHash}); err != nil {
		t.Fatalf("ApproveShown with nothing answered: %v", err)
	}
}

func TestCLIApprovePinsTheAnswersWithoutADigest(t *testing.T) {
	dataDir := t.TempDir()
	r := newSpecReviewRequestWithReadBack(t, dataDir)
	if _, err := AnswerReadBack(dataDir, r.ID, "Q1", shownSHA(t, dataDir, r.ID, "Q1"), "alice", "Print [].", fixedNow); err != nil {
		t.Fatal(err)
	}
	approved, err := Approve(dataDir, r.ID, "alice", fixedNow, nil)
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if approved.ApprovedReadBackSHA256 == "" {
		t.Fatal("the CLI approval pinned no answers")
	}
}

func TestSpecApprovalRefusesASpecAndAnswersOverTheSpecLimit(t *testing.T) {
	dataDir := t.TempDir()
	r := newSpecReviewRequestWithReadBack(t, dataDir)
	specPath := filepath.Join(Dir(dataDir, r.ID), specFileName)
	spec, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatal(err)
	}
	answer := strings.Repeat("a", MaxReadBackAnswerBytes)
	question := len(r.ReadBack.Questions[0].Text)
	pad := MaxHandedOverSpecBytes - len(spec) - question - len(answer) + 1
	padded := append(spec, []byte("\n"+strings.Repeat("x", pad-1))...)
	if err := os.WriteFile(specPath, padded, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := AnswerReadBack(dataDir, r.ID, "Q1", shownSHA(t, dataDir, r.ID, "Q1"), "alice", answer, fixedNow); err != nil {
		t.Fatal(err)
	}
	_, err = Approve(dataDir, r.ID, "alice", fixedNow, nil)
	total := len(padded) + question + len(answer)
	if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("(%d bytes)", len(padded))) || !strings.Contains(err.Error(), fmt.Sprintf("(%d bytes)", question+len(answer))) || !strings.Contains(err.Error(), fmt.Sprint(total)) {
		t.Fatalf("Approve over the limit = %v, want a refusal naming %d, %d and %d bytes", err, len(padded), question+len(answer), total)
	}
	// Clearing the answer brings it back under the limit.
	if _, err := AnswerReadBack(dataDir, r.ID, "Q1", shownSHA(t, dataDir, r.ID, "Q1"), "alice", "", fixedNow); err != nil {
		t.Fatal(err)
	}
	if _, err := Approve(dataDir, r.ID, "alice", fixedNow, nil); err != nil {
		t.Fatalf("Approve under the limit: %v", err)
	}
}

func TestReplaceReadBackKeepsAReadBackOfTheSameSpecBytes(t *testing.T) {
	answered := validReadBack()
	answered.SpecSHA256 = SpecTextSHA256("spec v1")
	answered.Questions[0].Answer = "Print []."
	r := &Request{ReadBack: &answered}
	same := validReadBack()
	same.Summary = "A different wording of the same spec."
	same.SpecSHA256 = SpecTextSHA256("spec v1")
	if carried := r.ReplaceReadBack(&same); carried != nil || r.ReadBack != &answered {
		t.Fatalf("a read-back of the same spec bytes replaced the answered one (carried %+v)", carried)
	}
	other := validReadBack()
	other.SpecSHA256 = SpecTextSHA256("spec v2")
	carried := r.ReplaceReadBack(&other)
	if r.ReadBack != &other || len(carried) != 1 || carried[0].Answer != "Print []." {
		t.Fatalf("a read-back of other spec bytes: read-back %+v, carried %+v", r.ReadBack, carried)
	}
	unavailable := &Request{ReadBack: &ReadBack{Unavailable: ReadBackCauseFailed, SpecSHA256: SpecTextSHA256("spec v1")}}
	retried := validReadBack()
	retried.SpecSHA256 = SpecTextSHA256("spec v1")
	if unavailable.ReplaceReadBack(&retried); unavailable.ReadBack != &retried {
		t.Fatal("an unavailable read-back was kept over a drafted one of the same spec")
	}
}

// A spec edited after its read-back was drafted gets one note at spec
// review, from the hashes alone; the read-back of the text under review, or
// one with no recorded hash, gets none.
func TestASpecEditedAfterItsReadBackIsNotedAtSpecReview(t *testing.T) {
	dataDir := t.TempDir()
	r := newSpecReviewRequestWithReadBack(t, dataDir)
	spec, err := os.ReadFile(SpecPath(dataDir, r.ID))
	if err != nil {
		t.Fatal(err)
	}
	r.ReadBack.SpecSHA256 = SpecTextSHA256(string(spec))
	r.RefreshReviewNotes(dataDir)
	if slices.Contains(r.ReviewNotes, ReadBackStaleNote) {
		t.Fatalf("a read-back of the spec under review was noted as stale: %v", r.ReviewNotes)
	}
	if err := os.WriteFile(SpecPath(dataDir, r.ID), append(spec, []byte("\nAn edit.\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	r.RefreshReviewNotes(dataDir)
	if len(r.ReviewNotes) == 0 || r.ReviewNotes[0] != ReadBackStaleNote {
		t.Fatalf("ReviewNotes = %v, want the stale read-back noted first", r.ReviewNotes)
	}
	r.ReadBack.SpecSHA256 = ""
	r.RefreshReviewNotes(dataDir)
	if slices.Contains(r.ReviewNotes, ReadBackStaleNote) {
		t.Fatalf("a read-back with no recorded spec hash was noted as stale: %v", r.ReviewNotes)
	}
}
