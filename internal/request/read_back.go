package request

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// The read-back is the factory's reading of a spec the operator handed over
// (SpecImported), shown at spec_review: a short summary of what it will
// build, in the model's own words, and the questions it would otherwise
// guess at. It is model-written data shown beside the spec, never a check:
// it refuses nothing and approval never waits on it. The operator may answer
// any question (AnswerReadBack); the answered questions are frozen by the
// spec approval (ApprovedReadBackSHA256) and reach every ticket's build.

// Read-back size limits. A read-back over any of them is refused whole
// (ValidateReadBack), never cut.
const (
	MaxReadBackQuestions     = 7
	MaxReadBackSummaryBytes  = 1500
	MaxReadBackQuestionBytes = 400
	MaxReadBackAnswerBytes   = 2000
)

// ReadBack is a handed-over spec's read-back. Unavailable is set, and the
// rest empty, when no read-back could be drafted for the spec under review:
// it names the cause class (ReadBackCause*), never model text.
type ReadBack struct {
	Summary     string             `json:"summary,omitempty"`
	Questions   []ReadBackQuestion `json:"questions,omitempty"`
	Unavailable string             `json:"unavailable,omitempty"`
}

// ReadBackQuestion is one question of a read-back. ID and Text are the
// model's; Answer, AnsweredBy and AnsweredAt (RFC3339Nano) are the
// operator's, all three empty while it is unanswered.
type ReadBackQuestion struct {
	ID         string `json:"id"`
	Text       string `json:"text"`
	Answer     string `json:"answer,omitempty"`
	AnsweredBy string `json:"answered_by,omitempty"`
	AnsweredAt string `json:"answered_at,omitempty"`
}

// The causes a read-back could not be drafted, each a clause of
// ReadBackUnavailableNote.
const (
	ReadBackCauseNoJob   = "no read-back job is configured"
	ReadBackCauseBudget  = "the request's spend ceiling is reached"
	ReadBackCauseRefused = "its output was malformed or over the size limits"
	ReadBackCauseFailed  = "the job did not finish"
)

// ReadBackUnavailableNote is the one spec-review note a read-back that could
// not be drafted leaves.
func ReadBackUnavailableNote(cause string) string {
	return fmt.Sprintf("The read-back could not be drafted: %s.", cause)
}

// ErrReadBackRefused marks a read-back refused for its structure or size.
var ErrReadBackRefused = errors.New("read-back refused")

// Errors AnswerReadBack returns, each wrapped with the request and question.
var (
	ErrReadBackNotAnswerable    = errors.New("read-back questions are answered only in spec_review")
	ErrReadBackQuestionNotFound = errors.New("no such read-back question")
	ErrReadBackAnswerTooLong    = errors.New("read-back answer too long")
)

// ValidateReadBack refuses a drafted read-back by its structure and sizes
// alone: a non-empty summary of at most MaxReadBackSummaryBytes, at most
// MaxReadBackQuestions questions, the nth with the ID Q<n> and
// a non-empty text of at most MaxReadBackQuestionBytes, no answer fields,
// and valid UTF-8 throughout. It reads no prose. The error wraps
// ErrReadBackRefused.
func ValidateReadBack(rb ReadBack) error {
	refuse := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s", ErrReadBackRefused, fmt.Sprintf(format, args...))
	}
	if strings.TrimSpace(rb.Summary) == "" {
		return refuse("the summary is empty")
	}
	if len(rb.Summary) > MaxReadBackSummaryBytes || !utf8.ValidString(rb.Summary) {
		return refuse("the summary is over %d bytes or not UTF-8", MaxReadBackSummaryBytes)
	}
	if rb.Unavailable != "" {
		return refuse("a drafted read-back has no unavailable cause")
	}
	if len(rb.Questions) > MaxReadBackQuestions {
		return refuse("%d questions, at most %d", len(rb.Questions), MaxReadBackQuestions)
	}
	for i, q := range rb.Questions {
		if q.ID != fmt.Sprintf("Q%d", i+1) {
			return refuse("question %d must have the id Q%d", i+1, i+1)
		}
		if strings.TrimSpace(q.Text) == "" || len(q.Text) > MaxReadBackQuestionBytes || !utf8.ValidString(q.Text) {
			return refuse("question %s is empty, over %d bytes or not UTF-8", q.ID, MaxReadBackQuestionBytes)
		}
		if q.Answer != "" || q.AnsweredBy != "" || q.AnsweredAt != "" {
			return refuse("question %s carries an answer", q.ID)
		}
	}
	return nil
}

// Answered returns the questions of rb the operator answered, in order. A
// nil rb has none.
func (rb *ReadBack) Answered() []ReadBackQuestion {
	if rb == nil {
		return nil
	}
	var answered []ReadBackQuestion
	for _, q := range rb.Questions {
		if q.Answer != "" {
			answered = append(answered, q)
		}
	}
	return answered
}

// ReadBackAnswersSHA256 is the hash a spec approval pins for rb's answered
// questions (their IDs, texts and answers, in order), or "" when none is
// answered, so a request with no read-back pins nothing.
func ReadBackAnswersSHA256(rb *ReadBack) string {
	answered := rb.Answered()
	if len(answered) == 0 {
		return ""
	}
	type pinned struct {
		ID     string `json:"id"`
		Text   string `json:"text"`
		Answer string `json:"answer"`
	}
	items := make([]pinned, len(answered))
	for i, q := range answered {
		items[i] = pinned{ID: q.ID, Text: q.Text, Answer: q.Answer}
	}
	data, err := json.Marshal(items)
	if err != nil {
		// Strings only: Marshal cannot fail.
		panic(err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// verifyApprovedReadBack refuses a request whose answered read-back
// questions differ from what its spec approval pinned. A request whose spec
// approval is not in force checks nothing.
func verifyApprovedReadBack(r *Request) error {
	if r.ApprovedSHA256[specFileName] == "" {
		return nil
	}
	if ReadBackAnswersSHA256(r.ReadBack) != r.ApprovedReadBackSHA256 {
		return fmt.Errorf("request %s: the answers to the read-back's questions have changed since %s approved the spec at %s -- re-approve before continuing", r.ID, r.ApprovedBy, r.ApprovedAt)
	}
	return nil
}

// takeReadBackAnswers returns the answered questions of r's read-back and
// drops the read-back: a rejection of the spec it read carries the answers
// into the redraft's feedback, and the redraft drafts a new read-back.
func takeReadBackAnswers(r *Request) []ReadBackQuestion {
	answered := r.ReadBack.Answered()
	r.ReadBack = nil
	return answered
}

// AnswerReadBack records by's answer to read-back question qid of request
// id, which must be in spec_review; an answer that is empty after trimming
// clears it. The answer is at most MaxReadBackAnswerBytes of UTF-8. The
// request's review gate must not have lapsed (WithGateExpiry).
func AnswerReadBack(dataDir, id, qid, by, answer string, now time.Time, opts ...DecisionOption) (*Request, error) {
	answer = strings.TrimSpace(answer)
	if len(answer) > MaxReadBackAnswerBytes || !utf8.ValidString(answer) {
		return nil, fmt.Errorf("request %s: %s: %w (at most %d bytes of UTF-8)", id, qid, ErrReadBackAnswerTooLong, MaxReadBackAnswerBytes)
	}
	unlock, err := Lock(dataDir, id)
	if err != nil {
		return nil, err
	}
	defer unlock()
	r, err := Load(dataDir, id)
	if err != nil {
		return nil, err
	}
	if err := r.refuseLapsed(dataDir, now, opts); err != nil {
		return nil, err
	}
	if r.State != StateSpecReview {
		return nil, fmt.Errorf("request %s: %w (it is %q)", id, ErrReadBackNotAnswerable, r.State)
	}
	q := r.readBackQuestion(qid)
	if q == nil {
		return nil, fmt.Errorf("request %s: %w %q", id, ErrReadBackQuestionNotFound, qid)
	}
	q.Answer, q.AnsweredBy, q.AnsweredAt = answer, by, now.UTC().Format(time.RFC3339Nano)
	if answer == "" {
		q.AnsweredBy, q.AnsweredAt = "", ""
	}
	if err := r.Save(dataDir); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *Request) readBackQuestion(qid string) *ReadBackQuestion {
	if r.ReadBack == nil {
		return nil
	}
	for i := range r.ReadBack.Questions {
		if r.ReadBack.Questions[i].ID == qid {
			return &r.ReadBack.Questions[i]
		}
	}
	return nil
}

// readBackAnswersFeedback renders answered read-back questions for a
// redraft's feedback: each question and answer on one line (oneLine), so
// neither can open a heading of its own.
func readBackAnswersFeedback(answered []ReadBackQuestion) string {
	if len(answered) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Answers the operator gave to the read-back's questions:\n\n")
	for _, q := range answered {
		fmt.Fprintf(&b, "- %s: %s\n  Answer: %s\n", q.ID, oneLine(q.Text), oneLine(q.Answer))
	}
	b.WriteString("\n")
	return b.String()
}
