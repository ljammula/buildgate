package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"buildgate/internal/request"
	"buildgate/internal/run"
)

func TestInspectPrintsADraftGatesFilesAndItsNote(t *testing.T) {
	f := newProfileFixture(t, "default")
	root := f.roots["default"]
	saveWaitingRequest(t, root, "spec-a", "Add login", request.StateSpecReview, time.Hour, nil)
	var out bytes.Buffer
	if err := inspectRun([]string{"-data-dir", root, "spec-a"}, &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"The spec:\n  less ", "/requests/spec-a/spec.md\n", request.InspectDraftNote} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

// inspect shows a run's work from its record: the change's size and files,
// each gate's result and the review's findings. The commands to copy, and
// the note about using them, are -commands' alone.
func TestInspectShowsTheChangeGatesAndFindingsAndCommandsOnlyWhenAsked(t *testing.T) {
	f := newProfileFixture(t, "default")
	root := f.roots["default"]
	built := &run.Run{
		ID: "run-1", Ticket: "t", State: run.StateQuarantined, RepositoryRoot: "/repos/app", BaseSHA: "1111abc", ResultSHA: "2222abc",
		DiffStat:     &run.DiffStat{FilesChanged: 2, Insertions: 40, Deletions: 3},
		ChangedFiles: []string{"cmd/app/main.go", "cmd/app/main_test.go"},
		GateResults:  []run.GateResult{{Check: "canonical_verify", Passed: true}, {Check: "full_suite_verify", ExitCode: 2}},
		CodeReview: &run.CodeReviewResult{Policy: "required", Available: true, Findings: []run.CodeReviewFinding{
			{Severity: "high", File: "cmd/app/main.go", Line: 12, Summary: "the handler\x1b[31m drops the error"},
		}},
	}
	if err := built.Save(root); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := inspectRun([]string{"-data-dir", root, "run-1"}, &out); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	for _, want := range []string{
		"run run-1\n",
		"  state      quarantined",
		"  change     2 files, +40 -3\n               cmd/app/main.go\n               cmd/app/main_test.go\n",
		"  gates      1 passed, 1 failed\n",
		"canonical_verify       passed\n",
		"full_suite_verify      FAILED (exit 2)\n",
		"  review     1 findings (policy required)\n               high  cmd/app/main.go:12  the handler",
		"factoryd inspect -commands run-1 prints the commands",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("output lacks %q:\n%s", want, text)
		}
	}
	for _, unwanted := range []string{"git -C", request.InspectRunNote, "\x1b"} {
		if strings.Contains(text, unwanted) {
			t.Errorf("output without -commands has %q:\n%s", unwanted, text)
		}
	}
}

func TestInspectTakesARunIDAndPrintsItsCommandsAndTheNote(t *testing.T) {
	f := newProfileFixture(t, "default")
	root := f.roots["default"]
	built := &run.Run{ID: "run-1", Ticket: "t", RepositoryRoot: "/repos/app", WorkspacePath: "/repos/app-gone", BaseSHA: "1111abc", ResultSHA: "2222abc"}
	if err := built.Save(root); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := inspectRun([]string{"-data-dir", root, "-commands", "run-1"}, &out); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	for _, want := range []string{"run run-1\n", "  The files changed:\n    git -C /repos/app diff --stat 1111abc..2222abc --\n", request.InspectRunNote} {
		if !strings.Contains(text, want) {
			t.Errorf("output lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "worktree") && strings.Contains(text, "app-gone") {
		t.Errorf("a worktree that is gone is named:\n%s", text)
	}

	out.Reset()
	if err := inspectRun([]string{"-data-dir", root, "-json", "run-1"}, &out); err != nil {
		t.Fatal(err)
	}
	var decoded request.Inspection
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil || len(decoded.Tickets) != 1 || decoded.Tickets[0].Result != "2222abc" {
		t.Errorf("-json = %s (%v)", out.String(), err)
	}
}

func TestInspectSaysWhenThereIsNothingToShowAndRefusesAnUnknownID(t *testing.T) {
	f := newProfileFixture(t, "default")
	root := f.roots["default"]
	saveWaitingRequest(t, root, "busy", "In flight", request.StateBuilding, time.Hour, nil)
	var out bytes.Buffer
	if err := inspectRun([]string{"-data-dir", root, "busy"}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.String(), "Nothing to inspect for busy") {
		t.Errorf("output = %q", out.String())
	}
	if err := inspectRun([]string{"-data-dir", root, "nope"}, &out); err == nil || !strings.Contains(err.Error(), `no request or run matches "nope" in `+root) {
		t.Errorf("unknown id: err = %v", err)
	}
	if err := inspectRun([]string{"-data-dir", root, "../x"}, &out); err == nil || !strings.Contains(err.Error(), "is not a request or run id") {
		t.Errorf("a path as id: err = %v", err)
	}
	if err := inspectRun([]string{"-data-dir", root}, &out); err == nil {
		t.Error("no id: no error")
	}
}
