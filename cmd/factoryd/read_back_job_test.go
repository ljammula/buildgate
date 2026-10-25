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
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + argsFile + "\nwhile [ $# -gt 0 ]; do case \"$1\" in --out) out=$2;; --evidence) ev=$2;; esac; shift; done\nmkdir -p \"$(dirname \"$out\")\"\n" + fakeBody
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
	rb, evidence, err := runReadBackJob(context.Background(), dataDir, r, cfg)
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
	if i := slices.Index(args, "--spec"); i < 0 || args[i+1] != requestdriver.RequestSpecPath(dataDir, r.ID) {
		t.Fatalf("read_back.py args = %q, want --spec naming spec.md", args)
	}
	if _, err := os.Stat(filepath.Join(r.Workspace, readBackScratchDirName)); !os.IsNotExist(err) {
		t.Errorf("scratch directory left behind: %v", err)
	}
}

func TestRunReadBackJobRefusedAndFailedExits(t *testing.T) {
	dataDir, r, cfg, _ := readBackJobFixture(t, "echo 'read_back: refused: 8 questions, at most 7' >&2\nexit 3\n")
	if _, _, err := runReadBackJob(context.Background(), dataDir, r, cfg); !errors.Is(err, request.ErrReadBackRefused) {
		t.Fatalf("exit 3: %v, want ErrReadBackRefused", err)
	}
	dataDir, r, cfg, _ = readBackJobFixture(t, "echo 'read_back: agent exited 1' >&2\nexit 2\n")
	_, _, err := runReadBackJob(context.Background(), dataDir, r, cfg)
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
	if err := answerMain([]string{"-data-dir", dataDir, "-q", "Q1", "-text", "24 hours.", "req-1"}); err != nil {
		t.Fatalf("answerMain: %v", err)
	}
	loaded, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	if q := loaded.ReadBack.Questions[0]; q.Answer != "24 hours." || q.AnsweredBy == "" {
		t.Fatalf("question = %+v", q)
	}
	if err := answerMain([]string{"-data-dir", dataDir, "-text", "x", "req-1"}); err == nil {
		t.Error("answerMain without -q succeeded")
	}
	loaded.State = request.StatePlanning
	if err := loaded.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	err = answerMain([]string{"-data-dir", dataDir, "-q", "Q1", "-text", "48 hours.", "req-1"})
	if !errors.Is(err, request.ErrReadBackNotAnswerable) {
		t.Fatalf("answerMain out of spec_review = %v, want ErrReadBackNotAnswerable", err)
	}
}
