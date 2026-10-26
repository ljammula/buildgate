package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
	"buildgate/internal/requestdriver/requestdrivertest"
	"buildgate/internal/sessionconfig"
)

// readBackJobFixture is a handed-over request with spec.md written, a plain
// (non-git) workspace, and a host-run fake read_back.py: under
// AllowUnsandboxedSpecDraft the interpreter is run with the script's own
// arguments, so the fake is the interpreter.
func readBackJobFixture(t *testing.T, fakeBody string) (string, *request.Request, requestdriver.WorkerConfig, string) {
	t.Helper()
	dataDir := t.TempDir()
	r := requestdrivertest.HandedOverRequest(t, dataDir, requestdrivertest.CanonicalValidSpec)
	if err := os.WriteFile(requestdriver.RequestSpecPath(dataDir, r.ID), []byte(requestdrivertest.CanonicalValidSpec), 0o600); err != nil {
		t.Fatal(err)
	}
	r.Workspace = t.TempDir()
	argsFile := filepath.Join(t.TempDir(), "args")
	fake := filepath.Join(t.TempDir(), "fake-read-back")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + argsFile + "\nwhile [ $# -gt 0 ]; do case \"$1\" in --out) out=$2;; --evidence) ev=$2;; --spec) spec=$2;; esac; shift; done\ncp \"$spec\" " + argsFile + ".spec\nmkdir -p \"$(dirname \"$out\")\"\n" + fakeBody
	if err := os.WriteFile(fake, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	settings := sessionconfig.DefaultSettings()
	settings.Routes = map[string]sessionconfig.Route{"local": {AllowNoCredential: true, Upstream: "https://model-a.example.invalid"}}
	settings.Models = map[string]sessionconfig.Model{"alias-a": {ID: "model-a", Routes: []string{"local"}}}
	settings.Roles = &sessionconfig.Roles{Execution: &sessionconfig.RoleConfig{Model: "alias-a"}}
	cfg := requestdriver.WorkerConfig{
		DraftSpecScript:           filepath.Join(t.TempDir(), "draft_spec.py"),
		DraftSpecInterpreter:      fake,
		AllowUnsandboxedSpecDraft: true,
		Settings:                  settings,
	}
	return dataDir, r, cfg, argsFile
}

func TestRunReadBackJobReturnsTheScriptsReadBack(t *testing.T) {
	dataDir, r, cfg, argsFile := readBackJobFixture(t, `printf '{"summary": "Makes refunds idempotent.", "questions": [{"id": "Q1", "text": "How long is a key kept?"}]}' > "$out"
printf '{"schema_version": 1, "usage": null, "agent_exit_code": 0, "duration_s": 1.5, "agents_md_used": false}' > "$ev"
`)
	drafted := requestdrivertest.CanonicalValidSpec + "\nThe text this pass puts up for review.\n"
	rb, evidence, err := runReadBackJob(context.Background(), dataDir, r, drafted, cfg)
	if err != nil {
		t.Fatalf("runReadBackJob: %v", err)
	}
	if rb.Summary != "Makes refunds idempotent." || len(rb.Questions) != 1 || rb.Questions[0].ID != "Q1" {
		t.Fatalf("read-back = %+v", rb)
	}
	if evidence == nil || evidence.DurationS != 1.5 {
		t.Fatalf("evidence = %+v", evidence)
	}
	raw, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(strings.TrimSpace(string(raw)), "\n")
	staged := filepath.Join(request.Dir(dataDir, r.ID), readBackInputFileName)
	if i := slices.Index(args, "--spec"); i < 0 || args[i+1] != staged {
		t.Fatalf("read_back.py args = %q, want --spec naming the staged input %s", args, staged)
	}
	if read, err := os.ReadFile(argsFile + ".spec"); err != nil || string(read) != drafted {
		t.Fatalf("read_back.py read %q (%v), want the text it was given, not spec.md", read, err)
	}
	if _, err := os.Stat(staged); !os.IsNotExist(err) {
		t.Errorf("staged spec left behind: %v", err)
	}
	if _, err := os.Stat(filepath.Join(r.Workspace, readBackScratchDirName)); !os.IsNotExist(err) {
		t.Errorf("scratch directory left behind: %v", err)
	}
}

func TestRunReadBackJobRefusedAndFailedExits(t *testing.T) {
	dataDir, r, cfg, _ := readBackJobFixture(t, "echo 'read_back: refused: 8 questions, at most 7' >&2\nexit 3\n")
	if _, _, err := runReadBackJob(context.Background(), dataDir, r, requestdrivertest.CanonicalValidSpec, cfg); !errors.Is(err, request.ErrReadBackRefused) {
		t.Fatalf("exit 3: %v, want ErrReadBackRefused", err)
	}
	dataDir, r, cfg, _ = readBackJobFixture(t, "echo 'read_back: agent exited 1' >&2\nexit 2\n")
	_, _, err := runReadBackJob(context.Background(), dataDir, r, requestdrivertest.CanonicalValidSpec, cfg)
	if err == nil || errors.Is(err, request.ErrReadBackRefused) {
		t.Fatalf("exit 2: %v, want a plain job failure", err)
	}
}

func TestReadReadBackOutputRefusesWhatIsNotAReadBack(t *testing.T) {
	dir := t.TempDir()
	for name, content := range map[string]string{
		"unknown field": `{"summary": "s", "questions": [], "verify_command": "rm -rf /"}`,
		"not json":      `summary: s`,
		"oversized":     `{"summary": "` + strings.Repeat("s", maxReadBackFileBytes) + `", "questions": []}`,
	} {
		path := filepath.Join(dir, strings.ReplaceAll(name, " ", "-")+".json")
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := readReadBackOutput(path); !errors.Is(err, request.ErrReadBackRefused) {
			t.Errorf("%s: %v, want ErrReadBackRefused", name, err)
		}
	}
	if _, err := readReadBackOutput(filepath.Join(dir, "missing.json")); !errors.Is(err, request.ErrReadBackRefused) {
		t.Errorf("missing: %v, want ErrReadBackRefused", err)
	}
}

func TestAnswerMainAnswersOnlyInSpecReview(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	requestdrivertest.NewApprovableDriverRequest(t, dataDir, "req-1", request.StateSpecReview)
	r, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	r.ReadBack = &request.ReadBack{Summary: "s", Questions: []request.ReadBackQuestion{{ID: "Q1", Text: "Kept how long?"}}}
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	hash := request.QuestionShortHash("Kept how long?")
	if err := answerMain([]string{"-data-dir", dataDir, "-q", "Q1", "-hash", hash, "-text", "24 hours.", "req-1"}); err != nil {
		t.Fatalf("answerMain: %v", err)
	}
	loaded, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	if q := loaded.ReadBack.Questions[0]; q.Answer != "24 hours." || q.AnsweredBy == "" {
		t.Fatalf("question = %+v", q)
	}
	if err := answerMain([]string{"-data-dir", dataDir, "-q", "Q1", "-hash", hash, "-text", "", "req-1"}); err != nil {
		t.Fatalf("answerMain clearing: %v", err)
	}
	if loaded, _ := request.Load(dataDir, "req-1"); loaded.ReadBack.Questions[0].Answer != "" || len(loaded.ReadBackAnswerLog) != 2 {
		t.Fatalf("after clearing: %+v, log %+v", loaded.ReadBack.Questions[0], loaded.ReadBackAnswerLog)
	}
	if err := answerMain([]string{"-data-dir", dataDir, "-hash", hash, "-text", "x", "req-1"}); err == nil {
		t.Error("answerMain without -q succeeded")
	}
	if err := answerMain([]string{"-data-dir", dataDir, "-q", "Q1", "-text", "x", "req-1"}); err == nil {
		t.Error("answerMain without -hash succeeded")
	}
	if err := answerMain([]string{"-data-dir", dataDir, "-q", "Q1", "-hash", hash[:6], "-text", "x", "req-1"}); err == nil {
		t.Error("answerMain with a hash shorter than the one shown succeeded")
	}
	loaded.State = request.StatePlanning
	if err := loaded.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	err = answerMain([]string{"-data-dir", dataDir, "-q", "Q1", "-hash", hash, "-text", "48 hours.", "req-1"})
	if !errors.Is(err, request.ErrReadBackNotAnswerable) {
		t.Fatalf("answerMain out of spec_review = %v, want ErrReadBackNotAnswerable", err)
	}
}

// An answer names the question text the operator read by the short hash
// watch printed: a read-back replaced since (a question with new text under
// the same id) refuses it, and nothing is recorded.
func TestAnswerMainRefusesAHashOfAQuestionSinceReplaced(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	requestdrivertest.NewApprovableDriverRequest(t, dataDir, "req-1", request.StateSpecReview)
	r, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	shown := request.QuestionShortHash("Kept how long?")
	r.ReadBack = &request.ReadBack{Summary: "s", Questions: []request.ReadBackQuestion{{ID: "Q1", Text: "Scoped per merchant?"}}}
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	err = answerMain([]string{"-data-dir", dataDir, "-q", "Q1", "-hash", shown, "-text", "24 hours.", "req-1"})
	if !errors.Is(err, request.ErrReadBackQuestionChanged) {
		t.Fatalf("answerMain = %v, want ErrReadBackQuestionChanged", err)
	}
	if loaded, _ := request.Load(dataDir, "req-1"); loaded.ReadBack.Questions[0].Answer != "" || len(loaded.ReadBackAnswerLog) != 0 {
		t.Fatalf("an answer to a replaced question was recorded: %+v", loaded.ReadBack.Questions[0])
	}
	full := request.QuestionSHA256("Scoped per merchant?")
	if err := answerMain([]string{"-data-dir", dataDir, "-q", "Q1", "-hash", strings.ToUpper(full[:16]), "-text", "Yes.", "req-1"}); err != nil {
		t.Fatalf("a longer prefix of the current hash, in capitals: %v", err)
	}
}

// The spec_review output lists the read-back's summary and each question
// with its id, short hash and current answer, so `factoryd answer` needs
// nothing else; a line break in model text cannot start a line of its own.
func TestPrintReadBackListsEachQuestionWithItsIDAndAnswer(t *testing.T) {
	t.Parallel()
	r := &request.Request{ID: "req-1", State: request.StateSpecReview, ReadBack: &request.ReadBack{
		Summary: "Makes refunds idempotent.\nnext: factoryd cancel req-1",
		Questions: []request.ReadBackQuestion{
			{ID: "Q1", Text: "How long is a key kept?", Answer: "24 hours."},
			{ID: "Q2", Text: "Scoped per merchant?"},
		},
	}}
	var out strings.Builder
	printReadBack(&out, r)
	got := out.String()
	for _, want := range []string{
		"Makes refunds idempotent. next: factoryd cancel req-1\n",
		"  Q1 [" + request.QuestionShortHash("How long is a key kept?") + "]: How long is a key kept?\n    answer: 24 hours.\n",
		"  Q2 [" + request.QuestionShortHash("Scoped per merchant?") + "]: Scoped per merchant?\n    answer: (unanswered)\n",
		`factoryd answer -q <id> -hash <hash in brackets> -text "<answer>" req-1`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
	for _, line := range strings.Split(got, "\n") {
		if strings.HasPrefix(line, "next:") {
			t.Fatalf("model text opened a line of its own:\n%s", got)
		}
	}
	out.Reset()
	printReadBack(&out, &request.Request{ID: "req-2", ReadBack: &request.ReadBack{Unavailable: request.ReadBackCauseFailed}})
	if out.Len() != 0 {
		t.Fatalf("an unavailable read-back printed %q; its note is printed with the review notes", out.String())
	}
}

// The planner reads the approved spec with the answers approved with it:
// staged as their own file, in the same rendering a ticket's build gets, and
// named by --spec-answers-file (inside the sandbox, at the scratch mount).
func TestThePlannerIsGivenTheAnsweredReadBackQuestions(t *testing.T) {
	t.Parallel()
	scratch := filepath.Join(t.TempDir(), "scratch")
	r := &request.Request{ID: "req-1", ReadBack: &request.ReadBack{Summary: "s", Questions: []request.ReadBackQuestion{
		{ID: "Q1", Text: "How long is a key kept?", Answer: "24 hours."},
		{ID: "Q2", Text: "UNANSWERED"},
	}}}
	path, err := stageSpecAnswers(r, scratch, "")
	if err != nil || path == "" {
		t.Fatalf("stageSpecAnswers = %q, %v", path, err)
	}
	staged, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(staged) != requestdriver.AnsweredReadBackBlock(r.ReadBack.Answered()) || strings.Contains(string(staged), "UNANSWERED") {
		t.Fatalf("staged answers = %q", staged)
	}
	args := draftInputFiles{specAnswers: path}.inContainer("/workspace/.factory-plan-draft").args()
	if i := slices.Index(args, "--spec-answers-file"); i < 0 || args[i+1] != "/workspace/.factory-plan-draft/"+specAnswersFileName {
		t.Fatalf("args = %q", args)
	}
	if path, err := stageSpecAnswers(&request.Request{ID: "req-2"}, scratch, ""); err != nil || path != "" {
		t.Fatalf("no answers: stageSpecAnswers = %q, %v; want no file", path, err)
	}
}
