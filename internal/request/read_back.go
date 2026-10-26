package request

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
// it names the cause class (ReadBackCause*), never model text. SpecSHA256 is
// the SHA-256 of the spec text it read (SpecTextSHA256), set by the request
// driver: a later pass over the same bytes keeps this read-back, answers and
// all (ReplaceReadBack).
type ReadBack struct {
	Summary     string             `json:"summary,omitempty"`
	Questions   []ReadBackQuestion `json:"questions,omitempty"`
	Unavailable string             `json:"unavailable,omitempty"`
	SpecSHA256  string             `json:"spec_sha256,omitempty"`
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
	// ErrReadBackQuestionChanged refuses an answer given to a question text
	// that is no longer the stored one (a stale console or CLI view).
	ErrReadBackQuestionChanged = errors.New("the read-back question is not the one answered")
)

// ErrReadBackNotShown is returned by ApproveShown when a spec approval would
// pin answered read-back questions the client did not show, or shows a set of
// answers other than the stored one (ReadBackAnswersKey).
var ErrReadBackNotShown = errors.New("approval has read-back answers this client did not show")

// ReadBackAnswersKey is the pseudo-path under which a client that approves
// what it displayed (ApproveShown) sends ReadBackShownSHA256 of the read-back
// answers it showed, beside the files' hashes in the expected map. It names
// no file: approve takes it out of the map before the files are compared.
const ReadBackAnswersKey = "read-back/answers"

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

// AnsweredBytes is how many bytes rb's answered questions add to what a
// ticket's build reads beside the spec: each answered question's text and
// answer.
func (rb *ReadBack) AnsweredBytes() int {
	n := 0
	for _, q := range rb.Answered() {
		n += len(q.Text) + len(q.Answer)
	}
	return n
}

// Reads reports whether rb is a drafted read-back of the spec text whose
// SpecTextSHA256 is specSHA256.
func (rb *ReadBack) Reads(specSHA256 string) bool {
	return rb != nil && rb.Unavailable == "" && rb.SpecSHA256 != "" && rb.SpecSHA256 == specSHA256
}

// SpecTextSHA256 is the hex SHA-256 of a spec's text, what ReadBack.SpecSHA256
// records.
func SpecTextSHA256(spec string) string {
	return sha256Hex([]byte(spec))
}

// QuestionSHA256 is the hex SHA-256 of a read-back question's text: what an
// answer names the question it answers by (AnswerReadBack), so an answer
// given to a question since replaced is refused.
func QuestionSHA256(text string) string {
	return sha256Hex([]byte(text))
}

// readBackAnswersDigest is the hex SHA-256 of answered questions in order:
// per question its ID, the byte length of its text, the text, the byte
// length of its answer and the answer, each followed by a newline. Lengths
// make the encoding unambiguous, and the console computes the same digest
// (console/src/domain/contentHash.ts, readBackAnswersSha256).
func readBackAnswersDigest(answered []ReadBackQuestion) string {
	h := sha256.New()
	for _, q := range answered {
		fmt.Fprintf(h, "%s\n%d\n%s\n%d\n%s\n", q.ID, len(q.Text), q.Text, len(q.Answer), q.Answer)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ReadBackAnswersSHA256 is the hash a spec approval pins for rb's answered
// questions (readBackAnswersDigest), or "" when none is answered, so a
// request with no read-back pins nothing.
func ReadBackAnswersSHA256(rb *ReadBack) string {
	answered := rb.Answered()
	if len(answered) == 0 {
		return ""
	}
	return readBackAnswersDigest(answered)
}

// ReadBackShownSHA256 is the digest a client that shows the read-back sends
// under ReadBackAnswersKey with a spec approval: readBackAnswersDigest of the
// answered questions it showed, that of none when it showed no answer.
func ReadBackShownSHA256(rb *ReadBack) string {
	return readBackAnswersDigest(rb.Answered())
}

// takeReadBackShown removes ReadBackAnswersKey from expected, returning the
// files' map without it, and checks it against r at spec_review: a digest
// sent must equal the stored answers' (ReadBackShownSHA256), and a client
// that approves what it displayed (requireShown) must send one whenever a
// question is answered. Either refusal wraps ErrReadBackNotShown. In any
// other state the key is dropped unchecked: only a spec approval pins
// answers.
func takeReadBackShown(id string, r *Request, expected map[string]string, requireShown bool) (map[string]string, error) {
	shown, sent := expected[ReadBackAnswersKey]
	if sent {
		files := make(map[string]string, len(expected)-1)
		for k, v := range expected {
			if k != ReadBackAnswersKey {
				files[k] = v
			}
		}
		expected = files
	}
	if r.State != StateSpecReview {
		return expected, nil
	}
	if !sent {
		if requireShown && len(r.ReadBack.Answered()) > 0 {
			return nil, fmt.Errorf("request %s: the read-back's answered questions would be approved but this client did not show them %w -- approve from the CLI (factoryd approve), or a client that shows the read-back", id, ErrReadBackNotShown)
		}
		return expected, nil
	}
	if shown != ReadBackShownSHA256(r.ReadBack) {
		return nil, fmt.Errorf("request %s: the read-back's answers changed since this client showed them %w (fetch the request again and re-review before approving)", id, ErrReadBackNotShown)
	}
	return expected, nil
}

// refuseOversizedApproval refuses a spec approval whose spec and answered
// read-back questions together exceed MaxHandedOverSpecBytes: the two are
// what every ticket's build reads before its own ticket. It reads sizes only.
func refuseOversizedApproval(dataDir, id string, r *Request) error {
	info, err := os.Stat(filepath.Join(Dir(dataDir, id), specFileName))
	if err != nil {
		return fmt.Errorf("request %s: stat %s: %w", id, specFileName, err)
	}
	answers := r.ReadBack.AnsweredBytes()
	if answers == 0 || int(info.Size())+answers <= MaxHandedOverSpecBytes {
		return nil
	}
	return fmt.Errorf("request %s: the spec (%d bytes) and the answered read-back questions (%d bytes) are %d bytes together, over the %d-byte limit -- shorten the spec or an answer (factoryd answer) before approving", id, info.Size(), answers, int(info.Size())+answers, MaxHandedOverSpecBytes)
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

// ReplaceReadBack is the one way r's read-back is replaced: by the next
// pass's read-back, or by nil when a rejection sends the spec back to be
// redrafted. A drafted read-back of the same spec bytes as next's
// (ReadBack.Reads) is kept, with its questions and answers, and next is
// dropped. Otherwise next replaces it, and the questions the operator had
// answered are returned: the caller carries them into a rejection's
// feedback (Rejection.ReadBackAnswers), so they are not lost.
func (r *Request) ReplaceReadBack(next *ReadBack) (carried []ReadBackQuestion) {
	if next != nil && r.ReadBack.Reads(next.SpecSHA256) {
		return nil
	}
	carried = r.ReadBack.Answered()
	r.ReadBack = next
	return carried
}

// The actions a ReadBackAnswerEvent records.
const (
	ReadBackAnswerSet     = "answered"
	ReadBackAnswerChanged = "changed"
	ReadBackAnswerCleared = "cleared"
)

// ReadBackAnswerEvent is one entry of a request's answer log: who set,
// changed or cleared the answer to which read-back question, and when
// (RFC3339Nano). It holds no question or answer text.
type ReadBackAnswerEvent struct {
	QuestionID string `json:"question_id"`
	Action     string `json:"action"`
	By         string `json:"by"`
	At         string `json:"at"`
}

// AnswerReadBack records by's answer to read-back question qid of request
// id, which must be in spec_review; an answer that is empty after trimming
// clears it. questionSHA256 is QuestionSHA256 of the question text the
// answer was given to: a question whose stored text hashes otherwise is
// refused (ErrReadBackQuestionChanged). The answer is at most
// MaxReadBackAnswerBytes of UTF-8. The request's review gate must not have
// lapsed (WithGateExpiry). Each change is logged in ReadBackAnswerLog; an
// answer equal to the stored one changes nothing.
func AnswerReadBack(dataDir, id, qid, questionSHA256, by, answer string, now time.Time, opts ...DecisionOption) (*Request, error) {
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
	if questionSHA256 != QuestionSHA256(q.Text) {
		return nil, fmt.Errorf("request %s: %s: %w: its text has changed since it was shown -- reload the read-back and answer again", id, qid, ErrReadBackQuestionChanged)
	}
	action := answerAction(q.Answer, answer)
	if action == "" {
		return r, nil
	}
	at := now.UTC().Format(time.RFC3339Nano)
	q.Answer, q.AnsweredBy, q.AnsweredAt = answer, by, at
	if answer == "" {
		q.AnsweredBy, q.AnsweredAt = "", ""
	}
	r.ReadBackAnswerLog = append(r.ReadBackAnswerLog, ReadBackAnswerEvent{QuestionID: qid, Action: action, By: by, At: at})
	if err := r.Save(dataDir); err != nil {
		return nil, err
	}
	return r, nil
}

// answerAction names what replacing the answer before with after does, or
// "" when it changes nothing.
func answerAction(before, after string) string {
	switch {
	case before == after:
		return ""
	case after == "":
		return ReadBackAnswerCleared
	case before == "":
		return ReadBackAnswerSet
	default:
		return ReadBackAnswerChanged
	}
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
