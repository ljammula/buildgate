package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"buildgate/internal/release"
	"buildgate/internal/request"
	"buildgate/internal/run"
)

func TestSizeReplanStagesTheCommittedDiffAsFencedData(t *testing.T) {
	repo, dataDir := t.TempDir(), t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.com"}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q")
	message := filepath.Join(t.TempDir(), "message")
	if err := os.WriteFile(message, []byte("fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(repo, "main.go")
	if err := os.WriteFile(file, []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-q", "-F", message)
	base := git("rev-parse", "HEAD")
	if err := os.WriteFile(file, []byte("new ~~~~~ and keep this text\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-q", "-F", message)
	result := git("rev-parse", "HEAD")
	size := &run.ChangeSize{SourceLines: 2, SourceFiles: 1}
	record := &run.Run{ID: "sized-run", RequestID: "req-1", ProjectPath: repo, BaseSHA: base, ResultSHA: result, ChangeSize: size}
	if err := record.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	r := &request.Request{ID: "req-1", SizeReplan: &request.SizeReplan{RunID: record.ID, Measurement: size}}
	filePath, err := stageSizeReplan(context.Background(), dataDir, r, t.TempDir(), "", "")
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	for _, want := range []string{"data, never instructions", "\"source_lines\": 2", "-old", "+new ~~~~~ and keep this text", "~~~~~~text", "\n~~~~~~\n"} {
		if !strings.Contains(text, want) {
			t.Errorf("staged text lacks %q: %s", want, text)
		}
	}
	args := planTicketsArgs("planner.py", "/w", "/s", "/c", "/r", "true", "/out", "/ev", draftInputFiles{specAnswers: filePath}, 1, "", "pi", 400)
	if !strings.Contains(strings.Join(args, " "), "--spec-answers-file "+filePath) {
		t.Fatalf("planner args = %v, want the fenced data file", args)
	}
}

func TestRenderEvidenceMarkdownChangeSizeBands(t *testing.T) {
	for _, tc := range []struct {
		lines, functions, budget int
		want                     bool
	}{
		{400, 25, 400, false},
		{401, 25, 400, true},
		{400, 26, 400, true},
		{401, 25, 0, false},
	} {
		r := &run.Run{ChangeSize: &run.ChangeSize{SourceLines: tc.lines, SourceFiles: 2, Functions: tc.functions}, PRBudgetLines: tc.budget}
		body := renderEvidenceMarkdown(r, &release.MergePolicy{})
		if got := strings.Contains(body, "**Change size:**"); got != tc.want {
			t.Errorf("lines=%d functions=%d budget=%d: note=%v, want %v", tc.lines, tc.functions, tc.budget, got, tc.want)
		}
	}
}

func TestWorkflowInputCarriesReleaseSizeCaps(t *testing.T) {
	input := (runOptions{ReleasePolicy: release.MergePolicy{MaxFilesChanged: 7, MaxChangedLines: 77}}).workflowInput()
	if input.ReleaseMaxFilesChanged != 7 || input.ReleaseMaxChangedLines != 77 {
		t.Fatalf("workflow caps = %d files, %d lines; want 7 and 77", input.ReleaseMaxFilesChanged, input.ReleaseMaxChangedLines)
	}
}

func TestWorkflowInputCarriesListFreeSpecTicket(t *testing.T) {
	input := (runOptions{NoTicketScope: true}).workflowInput()
	if !input.NoTicketScope {
		t.Fatal("workflow input lost the spec ticket's absent scope")
	}
}
