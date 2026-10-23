package main

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"buildgate/internal/request"
	"buildgate/internal/run"
	"buildgate/internal/workflow"
)

// -baseline-check reaches RunWorkflowInput: the workflow stops on it.
func TestBaselineCheckReachesTheWorkflow(t *testing.T) {
	if !(runOptions{BaselineCheck: true}).workflowInput().BaselineCheck {
		t.Fatal("runOptions.BaselineCheck did not reach RunWorkflowInput.BaselineCheck")
	}
	if (runOptions{}).workflowInput().BaselineCheck {
		t.Fatal("an ordinary run asked for a baseline check")
	}
}

// A check takes a workspace and nothing that names a build, and moves its
// records into a data directory of its own beside the configured one.
func TestPrepareBaselineCheck(t *testing.T) {
	newRun := func(args ...string) *ticketRun {
		tr := &ticketRun{}
		tr.flags, tr.rf = newRunFlags()
		if err := tr.flags.Parse(args); err != nil {
			t.Fatal(err)
		}
		tr.ticket, tr.spec, tr.workspace, tr.dataDir = tr.rf.ticket, tr.rf.spec, tr.rf.workspace, tr.rf.dataDir
		tr.repository, tr.priorRun, tr.onBranch, tr.skipProjectCheck, tr.fullSuiteCadence = tr.rf.repository, tr.rf.priorRun, tr.rf.onBranch, tr.rf.skipProjectCheck, tr.rf.fullSuiteCadence
		return tr
	}
	for _, args := range [][]string{
		{"-baseline-check", "-workspace", "w", "-ticket", "t"},
		{"-baseline-check", "-workspace", "w", "-repository", "r"},
	} {
		if err := newRun(args...).prepareBaselineCheck(); err == nil {
			t.Errorf("%v accepted", args)
		}
	}
	data := filepath.Join(t.TempDir(), "data")
	tr := newRun("-baseline-check", "-workspace", "w", "-data-dir", data)
	if err := tr.prepareBaselineCheck(); err != nil {
		t.Fatal(err)
	}
	defer tr.runDeferred()
	if *tr.ticket != "baseline-check" || !strings.HasPrefix(*tr.dataDir, filepath.Join(data, "baseline-checks")+string(os.PathSeparator)) {
		t.Fatalf("ticket %q, data dir %q, want the check's own beside %s", *tr.ticket, *tr.dataDir, data)
	}
	if body, err := os.ReadFile(*tr.spec); err != nil || !strings.Contains(string(body), "## Goal") {
		t.Fatalf("spec %s: %v", *tr.spec, err)
	}
}

// A passed check leaves nothing; a halted one keeps its logs and loses its
// run record, so nothing that counts runs counts it.
func TestFinishBaselineCheck(t *testing.T) {
	write := func(checkDir string, state run.State) string {
		dataDir := filepath.Join(checkDir, "data")
		r := &run.Run{ID: "baseline-check-1", State: state}
		if err := r.Save(dataDir); err != nil {
			t.Fatal(err)
		}
		logPath := filepath.Join(dataDir, "runs", r.ID, "baseline_verify.log")
		if err := os.WriteFile(logPath, []byte("exit 3\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return logPath
	}
	passed := filepath.Join(t.TempDir(), "baseline-checks", "check-a")
	write(passed, run.StateSliceRunning)
	finishBaselineCheck(passed)
	if _, err := os.Stat(passed); !os.IsNotExist(err) {
		t.Errorf("a passed check left %s: %v", passed, err)
	}
	failed := filepath.Join(t.TempDir(), "baseline-checks", "check-b")
	logPath := write(failed, run.StateHalted)
	finishBaselineCheck(failed)
	if _, err := os.Stat(logPath); err != nil {
		t.Errorf("a failed check lost its log: %v", err)
	}
	if records, _ := filepath.Glob(filepath.Join(failed, "data", "runs", "*", "run.json")); len(records) != 0 {
		t.Errorf("a failed check kept run records %v", records)
	}
}

func TestBaselineCheckPassPrintsCommandTimings(t *testing.T) {
	dataDir := t.TempDir()
	const id = "baseline-check-pass"
	if err := run.SaveBaselineVerify(run.Dir(dataDir, id), &run.BaselineVerify{Passed: true, DurationMs: 1234}); err != nil {
		t.Fatal(err)
	}
	result := workflow.RunWorkflowResult{BaselineChecked: true, Attempts: []run.Attempt{
		{Kind: run.BaselineVerifyAttemptKind, StartedAt: "2026-10-23T12:00:00Z", FinishedAt: "2026-10-23T12:00:01Z", ExitCode: 0},
		{Kind: run.FullSuiteVerifyAttemptKind, StartedAt: "2026-10-23T12:00:02Z", FinishedAt: "2026-10-23T12:00:04Z", ExitCode: 0},
	}}
	out := captureStdout(t, func() {
		if err := finishRunWorkflow(nil, nil, runOptions{DataDir: dataDir, ID: id}, "", result); err != nil {
			t.Fatalf("finishRunWorkflow: %v", err)
		}
	})
	if !strings.Contains(out, "baseline check: verify 1.234s: passed") || !strings.Contains(out, "baseline check: full suite 2s: passed") {
		t.Fatalf("output=%q, want both command timings", out)
	}
}

func TestBaselineCheckFailurePrintsDetailsAndTimings(t *testing.T) {
	checkDir := filepath.Join(t.TempDir(), "baseline-checks", "check-f")
	dataDir := filepath.Join(checkDir, "data")
	const id = "baseline-check-fail"
	r := &run.Run{
		ID:             id,
		State:          run.StateHalted,
		HaltReasonCode: run.HaltReasonBaselineVerifyFailed,
		Triage:         "full suite failed its tests",
		Attempts: []run.Attempt{
			{Kind: run.BaselineVerifyAttemptKind, StartedAt: "2026-10-23T12:00:00Z", FinishedAt: "2026-10-23T12:00:01Z", ExitCode: 1},
			{Kind: run.FullSuiteVerifyAttemptKind, StartedAt: "2026-10-23T12:00:02Z", FinishedAt: "2026-10-23T12:00:03.500Z", ExitCode: 2},
		},
	}
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	if err := run.SaveBaselineVerify(run.Dir(dataDir, id), &run.BaselineVerify{
		Command:      "make verify",
		ExitCode:     1,
		FailingTests: []string{"TestSSL"},
		FailingCount: 1,
		DurationMs:   2345,
	}); err != nil {
		t.Fatal(err)
	}
	out := captureStderr(t, func() { finishBaselineCheck(checkDir) })
	for _, want := range []string{
		"baseline check: verify 2.345s: failed: TestSSL",
		"baseline check: full suite 1.5s: failed: exit 2; full suite failed its tests",
		"baseline check failed; its logs are in " + checkDir,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output=%q, want %q", out, want)
		}
	}
}

func TestBaselineCheckFailurePrintsAFullSuiteLeftover(t *testing.T) {
	checkDir := filepath.Join(t.TempDir(), "baseline-checks", "check-l")
	dataDir := filepath.Join(checkDir, "data")
	r := &run.Run{
		ID:             "baseline-check-leftover",
		State:          run.StateHalted,
		HaltReasonCode: run.HaltReasonBaselineVerifyFailed,
		HaltError:      "temporal workflow did not complete: " + workflow.FullSuiteLeftoverPrefix + "passed, but the command leaves coverage.xml outside the ticket's Allowed-Files. Ignore it",
		Attempts: []run.Attempt{
			{Kind: run.BaselineVerifyAttemptKind, StartedAt: "2026-10-23T12:00:00Z", FinishedAt: "2026-10-23T12:00:01Z"},
			{Kind: run.FullSuiteVerifyAttemptKind, StartedAt: "2026-10-23T12:00:02Z", FinishedAt: "2026-10-23T12:00:04Z"},
		},
	}
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	out := captureStderr(t, func() { finishBaselineCheck(checkDir) })
	if want := "baseline check: full suite 2s: passed, but the command leaves coverage.xml"; !strings.Contains(out, want) {
		t.Errorf("output=%q, want %q", out, want)
	}
}

// doctor's row runs the check only on an otherwise healthy setup, and shows
// a failed check's sentence without the Temporal wrapping.
func TestDoctorCheckBaseline(t *testing.T) {
	ran := false
	in := doctorInputs{targetRepo: "/repo", baselineCheck: func(string) error { ran = true; return nil }}
	if c := doctorCheckBaseline(in, 1); ran || c.Err == nil || !c.Advisory {
		t.Fatalf("with an earlier failure: ran=%v row=%+v, want skipped", ran, c)
	}
	if c := doctorCheckBaseline(in, 0); !ran || c.Err != nil {
		t.Fatalf("healthy: ran=%v row=%+v, want a passed check", ran, c)
	}
	in.baselineCheck = func(string) error {
		return errors.New("temporal workflow did not complete: baseline verify activity: activity error (type: RunBaselineVerifyActivity): baseline verify failed: exit 3. No model call was made (type: BaselineVerifyFailure, retryable: false)")
	}
	c := doctorCheckBaseline(in, 0)
	if c.Err == nil || c.Advisory || c.Err.Error() != "baseline verify failed: exit 3. No model call was made" {
		t.Fatalf("failed check row = %+v", c)
	}
}

// quickstart runs the check before a repository's first request only, and a
// failed check stops it with the check's sentence.
func TestQuickstartBaselineCheck(t *testing.T) {
	dp := newTestDeps(t)
	var calls [][]string
	host := dp.host.(*fakeHost)
	host.baselineCheckFn = func(args []string) error {
		calls = append(calls, args)
		return errors.New("workflow error: baseline verify failed: exit 1. No model call was made (type: BaselineVerifyFailure, retryable: false)")
	}
	dataDir, repo := t.TempDir(), t.TempDir()
	err := quickstartBaselineCheck(dp, io.Discard, "/cfg.yml", dataDir, repo, "", "make test", true)
	if err == nil || !strings.HasPrefix(err.Error(), "baseline verify failed: exit 1") || len(calls) != 1 {
		t.Fatalf("first request: err=%v calls=%v, want the check run and its sentence", err, calls)
	}
	if !slices.Equal(calls[0], []string{"-baseline-check", "-workspace", repo, "-data-dir", dataDir, "-config", "/cfg.yml", "-verify-command", "make test"}) {
		t.Errorf("args = %v", calls[0])
	}
	if err := request.New("req-1", repo, "p", request.Source{}, time.Now()).Save(dataDir); err != nil {
		t.Fatal(err)
	}
	if err := quickstartBaselineCheck(dp, io.Discard, "/cfg.yml", dataDir, repo, "", "", false); err != nil || len(calls) != 1 {
		t.Fatalf("later request: err=%v calls=%d, want no check", err, len(calls))
	}
}
