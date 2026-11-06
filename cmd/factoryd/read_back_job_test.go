package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
	"buildgate/internal/requestdriver/requestdrivertest"
	"buildgate/internal/sandbox/sandboxtest"
)

// readBackJobFixture is a handed-over request with spec.md written, a plain
// (non-git) workspace, and the fake sandbox runtime a launch goes through: the
// runtime exits with exitCode, and the test plays the script by writing its
// outputs (see writeReadBackAfterLaunch).
func readBackJobFixture(t *testing.T, exitCode int) (string, *request.Request, requestdriver.WorkerConfig, *sandboxtest.WorkerRuntime) {
	t.Helper()
	t.Setenv("REQUEST_JOB_ROLE_TEST_KEY", "sk-test")
	dataDir := t.TempDir()
	r := requestdrivertest.HandedOverRequest(t, dataDir, requestdrivertest.CanonicalValidSpec)
	if err := os.WriteFile(requestdriver.RequestSpecPath(dataDir, r.ID), []byte(requestdrivertest.CanonicalValidSpec), 0o600); err != nil {
		t.Fatal(err)
	}
	workspace, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r.Workspace = workspace
	scriptDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(scriptDir, readBackScriptName), []byte("# fake\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rt := &sandboxtest.WorkerRuntime{ExitCode: exitCode, DiskFree: []uint64{1 << 40}}
	cfg := requestdriver.WorkerConfig{
		DraftSpecScript:      filepath.Join(scriptDir, "draft_spec.py"),
		DraftSpecInterpreter: "python3",
		SandboxImage:         agentsDraftTestImage,
		Settings:             routesModeExecutionOnlySettings(),
		Sandboxes:            rt,
		MeterLedgerRoot:      filepath.Join(dataDir, "meter-ledgers"),
	}
	return dataDir, r, cfg, rt
}

// readBackLaunch is what writeReadBackAfterLaunch saw while the launch was
// live: the text of the staged input the sandbox mounts at /inputs/run, and
// the error, if any, of playing the script.
type readBackLaunch struct {
	staged string
	err    error
}

// writeReadBackAfterLaunch waits for the sandbox launch, reads the staged
// input mounted at /inputs/run (it is removed with the job), and writes the
// script's outputs into the /workspace mount's scratch directory.
func writeReadBackAfterLaunch(rt *sandboxtest.WorkerRuntime, stagedName, readBack, evidence string) <-chan readBackLaunch {
	done := make(chan readBackLaunch, 1)
	go func() {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			requests := rt.Requests()
			if len(requests) == 0 {
				time.Sleep(time.Millisecond)
				continue
			}
			var out readBackLaunch
			for _, mount := range requests[0].Mounts {
				switch mount.Target {
				case "/inputs/run":
					data, err := os.ReadFile(filepath.Join(mount.Source, stagedName))
					out.staged = string(data)
					out.err = errors.Join(out.err, err)
				case "/workspace":
					scratch := filepath.Join(mount.Source, readBackScratchDirName)
					out.err = errors.Join(out.err, os.MkdirAll(scratch, 0o750),
						os.WriteFile(filepath.Join(scratch, "read-back.json"), []byte(readBack), 0o644),
						os.WriteFile(filepath.Join(scratch, "evidence.json"), []byte(evidence), 0o644))
				}
			}
			done <- out
			return
		}
		done <- readBackLaunch{err: os.ErrDeadlineExceeded}
	}()
	return done
}

func TestRunReadBackJobReturnsTheScriptsReadBack(t *testing.T) {
	dataDir, r, cfg, rt := readBackJobFixture(t, 0)
	launched := writeReadBackAfterLaunch(rt, readBackInputFileName,
		`{"summary": "Makes refunds idempotent.", "questions": [{"id": "Q1", "text": "How long is a key kept?"}]}`,
		`{"schema_version": 1, "usage": null, "agent_exit_code": 0, "duration_s": 1.5, "agents_md_used": false}`)
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
	seen := <-launched
	if seen.err != nil {
		t.Fatalf("playing read_back.py: %v", seen.err)
	}
	requests := rt.Requests()
	if len(requests) != 1 {
		t.Fatalf("sandbox requests = %d, want one", len(requests))
	}
	args := requests[0].Command
	if i := slices.Index(args, "--spec"); i < 0 || args[i+1] != "/inputs/run/"+readBackInputFileName {
		t.Fatalf("read_back.py args = %q, want --spec naming the staged input at /inputs/run", args)
	}
	if seen.staged != drafted {
		t.Fatalf("read_back.py was given %q, want the text it was handed, not spec.md", seen.staged)
	}
	staged := filepath.Join(request.Dir(dataDir, r.ID), readBackInputFileName)
	if _, err := os.Stat(staged); !os.IsNotExist(err) {
		t.Errorf("staged spec left behind: %v", err)
	}
	if _, err := os.Stat(filepath.Join(r.Workspace, readBackScratchDirName)); !os.IsNotExist(err) {
		t.Errorf("scratch directory left behind: %v", err)
	}
}

func TestRunReadBackJobRefusedAndFailedExits(t *testing.T) {
	dataDir, r, cfg, _ := readBackJobFixture(t, readBackRefusedExitCode)
	if _, _, err := runReadBackJob(context.Background(), dataDir, r, requestdrivertest.CanonicalValidSpec, cfg); !errors.Is(err, request.ErrReadBackRefused) {
		t.Fatalf("exit 3: %v, want ErrReadBackRefused", err)
	}
	dataDir, r, cfg, _ = readBackJobFixture(t, 2)
	_, _, err := runReadBackJob(context.Background(), dataDir, r, requestdrivertest.CanonicalValidSpec, cfg)
	if err == nil || errors.Is(err, request.ErrReadBackRefused) {
		t.Fatalf("exit 2: %v, want a plain job failure", err)
	}
}

func TestRunSpecStageJobWithoutASandboxImageIsRefused(t *testing.T) {
	dataDir, r, cfg, rt := readBackJobFixture(t, 0)
	cfg.SandboxImage = ""
	_, _, err := runReadBackJob(context.Background(), dataDir, r, requestdrivertest.CanonicalValidSpec, cfg)
	if err == nil || !strings.Contains(err.Error(), "no sandbox image configured") {
		t.Fatalf("runReadBackJob without an image = %v, want the no-sandbox-image error", err)
	}
	if n := len(rt.Requests()); n != 0 {
		t.Fatalf("sandbox requests = %d, want none", n)
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
	if err := answerMain(newTestDeps(t), []string{"-data-dir", dataDir, "-q", "Q1", "-hash", hash, "-text", "24 hours.", "req-1"}); err != nil {
		t.Fatalf("answerMain: %v", err)
	}
	loaded, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	if q := loaded.ReadBack.Questions[0]; q.Answer != "24 hours." || q.AnsweredBy == "" {
		t.Fatalf("question = %+v", q)
	}
	if err := answerMain(newTestDeps(t), []string{"-data-dir", dataDir, "-q", "Q1", "-hash", hash, "-text", "", "req-1"}); err != nil {
		t.Fatalf("answerMain clearing: %v", err)
	}
	if loaded, _ := request.Load(dataDir, "req-1"); loaded.ReadBack.Questions[0].Answer != "" || len(loaded.ReadBackAnswerLog) != 2 {
		t.Fatalf("after clearing: %+v, log %+v", loaded.ReadBack.Questions[0], loaded.ReadBackAnswerLog)
	}
	if err := answerMain(newTestDeps(t), []string{"-data-dir", dataDir, "-hash", hash, "-text", "x", "req-1"}); err == nil {
		t.Error("answerMain without -q succeeded")
	}
	if err := answerMain(newTestDeps(t), []string{"-data-dir", dataDir, "-q", "Q1", "-text", "x", "req-1"}); err == nil {
		t.Error("answerMain without -hash succeeded")
	}
	if err := answerMain(newTestDeps(t), []string{"-data-dir", dataDir, "-q", "Q1", "-hash", hash[:6], "-text", "x", "req-1"}); err == nil {
		t.Error("answerMain with a hash shorter than the one shown succeeded")
	}
	loaded.State = request.StatePlanning
	if err := loaded.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	err = answerMain(newTestDeps(t), []string{"-data-dir", dataDir, "-q", "Q1", "-hash", hash, "-text", "48 hours.", "req-1"})
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
	err = answerMain(newTestDeps(t), []string{"-data-dir", dataDir, "-q", "Q1", "-hash", shown, "-text", "24 hours.", "req-1"})
	if !errors.Is(err, request.ErrReadBackQuestionChanged) {
		t.Fatalf("answerMain = %v, want ErrReadBackQuestionChanged", err)
	}
	if loaded, _ := request.Load(dataDir, "req-1"); loaded.ReadBack.Questions[0].Answer != "" || len(loaded.ReadBackAnswerLog) != 0 {
		t.Fatalf("an answer to a replaced question was recorded: %+v", loaded.ReadBack.Questions[0])
	}
	full := request.QuestionSHA256("Scoped per merchant?")
	if err := answerMain(newTestDeps(t), []string{"-data-dir", dataDir, "-q", "Q1", "-hash", strings.ToUpper(full[:16]), "-text", "Yes.", "req-1"}); err != nil {
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
