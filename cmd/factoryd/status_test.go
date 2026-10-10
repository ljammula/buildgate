package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"buildgate/internal/daemonheartbeat"
	"buildgate/internal/progress"
	"buildgate/internal/request"
	"buildgate/internal/run"
)

// TestRolesStatusLinesAbsentIsUnchanged covers the "roles: absent renders
// nothing" half of the P0e status design: no config at all, and a config
// with no roles: key, both produce a nil slice -- so `factoryd status`'s
// output is byte-identical to before this feature existed in either case.
func TestRolesStatusLinesAbsentIsUnchanged(t *testing.T) {
	if got := rolesStatusLines(""); got != nil {
		t.Errorf("rolesStatusLines(\"\") = %v, want nil (no session config found)", got)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	if err := os.WriteFile(path, []byte("relay_credential_mode: static\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if got := rolesStatusLines(path); got != nil {
		t.Errorf("rolesStatusLines(config without roles:) = %v, want nil", got)
	}
}

// TestRolesStatusLinesOnePerConfiguredRole covers the populated case: one
// line per role: entry, in planning/execution/review order, naming the
// alias, its worker model id, and its thinking level.
func TestRolesStatusLinesOnePerConfiguredRole(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	config := `routes:
  local:
    allow_no_credential: true
    upstream: https://model-a.example.invalid
models:
  luna:
    id: gpt-5.6-luna
    api: openai-completions
    routes: [local]
roles:
  planning:
    model: luna
    thinking: max
  execution:
    model: luna
    thinking: medium
  review:
    model: luna
    thinking: max
`
	if err := os.WriteFile(path, []byte(config), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	got := rolesStatusLines(path)
	want := []string{
		"planning: luna (gpt-5.6-luna, thinking max, harness pi)",
		"execution: luna (gpt-5.6-luna, thinking medium, harness pi)",
		"review: luna (gpt-5.6-luna, thinking max, harness pi)",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("rolesStatusLines() = %v, want %v", got, want)
	}
}

func mustSaveStatusRun(t *testing.T, dataDir string, r *run.Run) {
	t.Helper()
	if err := r.Save(dataDir); err != nil {
		t.Fatalf("Save %q: %v", r.ID, err)
	}
}

// TestLoadStatusRunsSortsNewestFirst covers the ordering listRuns already
// relies on: runs come back sorted by CreatedAt descending regardless of
// directory read order.
func TestLoadStatusRunsSortsNewestFirst(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	mustSaveStatusRun(t, dataDir, &run.Run{ID: "run-old", ProjectPath: "/repos/app/workspace", CreatedAt: "2026-09-01T00:00:00Z"})
	mustSaveStatusRun(t, dataDir, &run.Run{ID: "run-new", ProjectPath: "/repos/app/workspace", CreatedAt: "2026-09-05T00:00:00Z"})

	runs, err := loadStatusRuns(dataDir, func(string, ...any) {})
	if err != nil {
		t.Fatalf("loadStatusRuns: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("len(runs) = %d, want 2", len(runs))
	}
	if runs[0].ID != "run-new" || runs[1].ID != "run-old" {
		t.Errorf("runs = [%s, %s], want [run-new, run-old]", runs[0].ID, runs[1].ID)
	}
}

// TestLoadStatusRunsUnparseableCreatedAtSortsLast is the regression test
// for sorting by parsed time rather than lexicographic string: a run with
// an unparseable CreatedAt must sort after every run whose timestamp
// parses, even one that would lexicographically compare higher.
func TestLoadStatusRunsUnparseableCreatedAtSortsLast(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	mustSaveStatusRun(t, dataDir, &run.Run{ID: "run-good", ProjectPath: "/repos/app/workspace", CreatedAt: "2026-09-01T00:00:00Z"})
	mustSaveStatusRun(t, dataDir, &run.Run{ID: "run-bad", ProjectPath: "/repos/app/workspace", CreatedAt: "not-a-timestamp"})

	runs, err := loadStatusRuns(dataDir, func(string, ...any) {})
	if err != nil {
		t.Fatalf("loadStatusRuns: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("len(runs) = %d, want 2", len(runs))
	}
	if runs[0].ID != "run-good" || runs[1].ID != "run-bad" {
		t.Errorf("runs = [%s, %s], want [run-good, run-bad]", runs[0].ID, runs[1].ID)
	}
}

// TestLoadStatusRunsWarnsAndContinuesOnCorruptRecord is the regression
// test for the tolerate-corrupt-records requirement: one unreadable
// run.json must not hide every other run's status.
func TestLoadStatusRunsWarnsAndContinuesOnCorruptRecord(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	mustSaveStatusRun(t, dataDir, &run.Run{ID: "run-good", ProjectPath: "/repos/app/workspace", CreatedAt: "2026-09-05T00:00:00Z"})

	corruptDir := filepath.Join(dataDir, "runs", "run-corrupt")
	if err := os.MkdirAll(corruptDir, 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(corruptDir, "run.json"), []byte("{not valid json"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	var warnings []string
	runs, err := loadStatusRuns(dataDir, func(format string, args ...any) {
		warnings = append(warnings, format)
	})
	if err != nil {
		t.Fatalf("loadStatusRuns: %v", err)
	}
	if len(runs) != 1 || runs[0].ID != "run-good" {
		t.Fatalf("runs = %v, want just run-good", runs)
	}
	if len(warnings) != 1 {
		t.Fatalf("len(warnings) = %d, want 1", len(warnings))
	}
}

// TestLoadStatusRunsSkipsMissingRunJSONSilently is the regression test for
// Regression: a runs/<id>/ directory that exists but has no run.json yet (e.g. a
// workspace created before the run record is written) is not corruption --
// it must be skipped without a warning, while a genuinely corrupt run.json
// still warns.
func TestLoadStatusRunsSkipsMissingRunJSONSilently(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	mustSaveStatusRun(t, dataDir, &run.Run{ID: "run-good", ProjectPath: "/repos/app/workspace", CreatedAt: "2026-09-05T00:00:00Z"})

	emptyDir := filepath.Join(dataDir, "runs", "run-no-record")
	if err := os.MkdirAll(emptyDir, 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	var warnings []string
	runs, err := loadStatusRuns(dataDir, func(format string, args ...any) {
		warnings = append(warnings, format)
	})
	if err != nil {
		t.Fatalf("loadStatusRuns: %v", err)
	}
	if len(runs) != 1 || runs[0].ID != "run-good" {
		t.Fatalf("runs = %v, want just run-good", runs)
	}
	if len(warnings) != 0 {
		t.Errorf("warnings = %v, want none for a missing run.json", warnings)
	}
}

// TestBuildStatusEntryPullRequestURLTakesPrecedenceOverReason covers an
// accepted run that opened a PR: the PR URL is shown, and no reason
// (there would be nothing useful to say).
func TestBuildStatusEntryPullRequestURLTakesPrecedenceOverReason(t *testing.T) {
	t.Parallel()
	r := &run.Run{
		ID:             "run-1",
		State:          run.StateAccepted,
		CreatedAt:      "2026-09-05T00:00:00Z",
		UpdatedAt:      "2026-09-05T00:00:00Z", // must be ignored -- see the Duration assertion below
		Attempts:       []run.Attempt{{Kind: "build", FinishedAt: "2026-09-05T00:05:00Z"}},
		PullRequestURL: "https://github.com/example/app/pull/1",
		HaltError:      "should not be shown",
	}
	entry := buildStatusEntry(r, time.Now())
	if entry.PullRequestURL != r.PullRequestURL {
		t.Errorf("PullRequestURL = %q, want %q", entry.PullRequestURL, r.PullRequestURL)
	}
	if entry.Reason != "" {
		t.Errorf("Reason = %q, want empty when a PR URL is present", entry.Reason)
	}
	if want := (5 * time.Minute).String(); entry.Duration != want {
		t.Errorf("Duration = %q, want %q (derived from the attempt's FinishedAt, not UpdatedAt)", entry.Duration, want)
	}
}

// TestBuildStatusEntryUnconfirmedHaltStaysInElapsedMode is the regression
// test for the terminal predicate: a Halted run whose HaltConfirmed is
// still false may genuinely still be running, so it must report elapsed
// time, not a finished duration -- same distinction runTerminalConfirmed
// exists to make for the reclaim path.
func TestBuildStatusEntryUnconfirmedHaltStaysInElapsedMode(t *testing.T) {
	t.Parallel()
	unconfirmed := &run.Run{ID: "run-unconfirmed", State: run.StateHalted, CreatedAt: "2026-09-05T00:00:00Z", HaltConfirmed: false}
	entry := buildStatusEntry(unconfirmed, time.Now())
	if entry.Duration != "" {
		t.Errorf("Duration = %q, want empty for an unconfirmed halt", entry.Duration)
	}
	if entry.Elapsed == "" {
		t.Error("Elapsed is empty, want a non-empty elapsed time for an unconfirmed halt")
	}

	confirmed := &run.Run{ID: "run-confirmed", State: run.StateHalted, CreatedAt: "2026-09-05T00:00:00Z", HaltConfirmed: true}
	entry = buildStatusEntry(confirmed, time.Now())
	if entry.Elapsed != "" {
		t.Errorf("Elapsed = %q, want empty for a confirmed halt", entry.Elapsed)
	}
	if entry.Duration == "" {
		t.Error("Duration is empty, want a non-empty value (even \"?\") for a confirmed halt")
	}
}

// TestBuildStatusEntryDurationUnknownWithoutAttemptEvidence covers a
// terminal run with no attempt carrying a FinishedAt: rather than
// fabricate a duration from an untrustworthy source, this prints "?".
func TestBuildStatusEntryDurationUnknownWithoutAttemptEvidence(t *testing.T) {
	t.Parallel()
	noAttempts := &run.Run{ID: "run-no-attempts", State: run.StateAccepted, CreatedAt: "2026-09-05T00:00:00Z"}
	if got := buildStatusEntry(noAttempts, time.Now()).Duration; got != "?" {
		t.Errorf("Duration = %q, want %q", got, "?")
	}

	emptyFinishedAt := &run.Run{
		ID:        "run-empty-finished-at",
		State:     run.StateAccepted,
		CreatedAt: "2026-09-05T00:00:00Z",
		Attempts:  []run.Attempt{{Kind: "build", FinishedAt: ""}},
	}
	if got := buildStatusEntry(emptyFinishedAt, time.Now()).Duration; got != "?" {
		t.Errorf("Duration = %q, want %q", got, "?")
	}
}

// TestBuildStatusEntryDurationIgnoresEarlierFinishedAttempt is the
// regression test for round 2's finding: a finished build attempt
// followed by a still-running verify attempt must not have its duration
// derived from the build attempt's own FinishedAt -- only the LAST
// attempt's FinishedAt counts, so an unfinished final attempt on an
// otherwise-confirmed-halted run reports "?", not the build's finish
// time.
func TestBuildStatusEntryDurationIgnoresEarlierFinishedAttempt(t *testing.T) {
	t.Parallel()
	r := &run.Run{
		ID:            "run-partial",
		State:         run.StateHalted,
		HaltConfirmed: true,
		CreatedAt:     "2026-09-05T00:00:00Z",
		Attempts: []run.Attempt{
			{Kind: "build", FinishedAt: "2026-09-05T00:05:00Z"},
			{Kind: "verify", FinishedAt: ""},
		},
	}
	if got := buildStatusEntry(r, time.Now()).Duration; got != "?" {
		t.Errorf("Duration = %q, want %q (the last attempt has no FinishedAt)", got, "?")
	}
}

// TestBuildStatusEntryRepositoryFallsBackToProject covers the repository
// column: r.Repository when set, else the same derived project id shown
// in the Project field.
func TestBuildStatusEntryRepositoryFallsBackToProject(t *testing.T) {
	t.Parallel()
	withRepository := &run.Run{ID: "run-repo", ProjectPath: "/repos/myapp/workspace", Repository: "org/myapp", CreatedAt: "2026-09-05T00:00:00Z"}
	if got, want := buildStatusEntry(withRepository, time.Now()).Repository, "org/myapp"; got != want {
		t.Errorf("Repository = %q, want %q", got, want)
	}

	withoutRepository := &run.Run{ID: "run-no-repo", ProjectPath: "/repos/myapp/workspace", CreatedAt: "2026-09-05T00:00:00Z"}
	entry := buildStatusEntry(withoutRepository, time.Now())
	if entry.Repository != entry.Project {
		t.Errorf("Repository = %q, want it to fall back to Project %q", entry.Repository, entry.Project)
	}
}

// TestBuildStatusEntryReasonFallsBackToHaltReasonCodeThenGateResult covers
// a halted run with no PullRequestURL: it must show HaltError, or when
// that's absent HaltReasonCode, or when both are absent the first failed
// gate's name.
func TestBuildStatusEntryReasonFallsBackToHaltReasonCodeThenGateResult(t *testing.T) {
	t.Parallel()
	haltError := &run.Run{ID: "run-2", State: run.StateHalted, CreatedAt: "2026-09-05T00:00:00Z", UpdatedAt: "2026-09-05T00:01:00Z", HaltError: "boom", HaltReasonCode: "relay_ceiling_exceeded"}
	if got := buildStatusEntry(haltError, time.Now()).Reason; got != "boom" {
		t.Errorf("Reason = %q, want HaltError %q", got, "boom")
	}

	reasonCodeOnly := &run.Run{ID: "run-3", State: run.StateHalted, CreatedAt: "2026-09-05T00:00:00Z", UpdatedAt: "2026-09-05T00:01:00Z", HaltReasonCode: "relay_ceiling_exceeded"}
	if got := buildStatusEntry(reasonCodeOnly, time.Now()).Reason; got != "relay_ceiling_exceeded" {
		t.Errorf("Reason = %q, want HaltReasonCode %q", got, "relay_ceiling_exceeded")
	}

	gateOnly := &run.Run{
		ID:          "run-4",
		State:       run.StateQuarantined,
		CreatedAt:   "2026-09-05T00:00:00Z",
		UpdatedAt:   "2026-09-05T00:01:00Z",
		GateResults: []run.GateResult{{Check: "full_suite_verify", Passed: false}},
	}
	if got := buildStatusEntry(gateOnly, time.Now()).Reason; got != "gate failed: full_suite_verify" {
		t.Errorf("Reason = %q, want gate failure message", got)
	}
}

// TestBuildStatusEntryCostFromAttempts covers summing relay spend across
// attempts into a dollar string, and that a zero-spend run reports no
// cost at all rather than "$0.0000".
func TestBuildStatusEntryCostFromAttempts(t *testing.T) {
	t.Parallel()
	spent := &run.Run{
		ID:        "run-5",
		State:     run.StateAccepted,
		CreatedAt: "2026-09-05T00:00:00Z",
		UpdatedAt: "2026-09-05T00:01:00Z",
		Attempts: []run.Attempt{
			{Kind: "build", RelayConsumedCostMicroUSD: 1500},
			{Kind: "build", RelayConsumedCostMicroUSD: 500},
		},
	}
	if got, want := buildStatusEntry(spent, time.Now()).CostUSD, "0.0020"; got != want {
		t.Errorf("CostUSD = %q, want %q", got, want)
	}

	noSpend := &run.Run{ID: "run-6", State: run.StateAccepted, CreatedAt: "2026-09-05T00:00:00Z", UpdatedAt: "2026-09-05T00:01:00Z"}
	if got := buildStatusEntry(noSpend, time.Now()).CostUSD; got != "" {
		t.Errorf("CostUSD = %q, want empty with no relay spend evidence", got)
	}
}

// TestBuildStatusEntryLabelsSubscriptionCost covers a bug in
// `factoryd status`'s CostUSD column: it must not present a
// chatgpt-codex/github-copilot-routed run's relay-ledger dollar figure
// as though it were actually charged -- nothing was billed to the
// operator in dollars for a subscription route.
func TestBuildStatusEntryLabelsSubscriptionCost(t *testing.T) {
	t.Parallel()
	subscription := &run.Run{
		ID: "run-7", State: run.StateAccepted, CreatedAt: "2026-09-05T00:00:00Z", UpdatedAt: "2026-09-05T00:01:00Z",
		Attempts: []run.Attempt{{Kind: "build", RelayCredentialMode: "chatgpt-codex", RelayConsumedCostMicroUSD: 2000}},
	}
	if got, want := buildStatusEntry(subscription, time.Now()).CostUSD, "0.0020"+subscriptionCostSuffix; got != want {
		t.Errorf("CostUSD = %q, want %q", got, want)
	}

	metered := &run.Run{
		ID: "run-8", State: run.StateAccepted, CreatedAt: "2026-09-05T00:00:00Z", UpdatedAt: "2026-09-05T00:01:00Z",
		Attempts: []run.Attempt{{Kind: "build", RelayCredentialMode: "static", RelayConsumedCostMicroUSD: 2000}},
	}
	if got, want := buildStatusEntry(metered, time.Now()).CostUSD, "0.0020"; got != want {
		t.Errorf("CostUSD = %q, want %q (no subscription label for a metered run)", got, want)
	}
}

// TestStatusMainFiltersByProjectStateAndLimit exercises statusMain end to
// end against a temp data dir with several saved runs, covering -project,
// -state, and -n together.
func TestStatusMainFiltersByProjectStateAndLimit(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	mustSaveStatusRun(t, dataDir, &run.Run{ID: "run-a1", ProjectPath: "/repos/appa/workspace", Project: "appa", State: run.StateAccepted, CreatedAt: "2026-09-01T00:00:00Z", UpdatedAt: "2026-09-01T00:01:00Z"})
	mustSaveStatusRun(t, dataDir, &run.Run{ID: "run-a2", ProjectPath: "/repos/appa/workspace", Project: "appa", State: run.StateQuarantined, CreatedAt: "2026-09-02T00:00:00Z", UpdatedAt: "2026-09-02T00:01:00Z"})
	mustSaveStatusRun(t, dataDir, &run.Run{ID: "run-b1", ProjectPath: "/repos/appb/workspace", Project: "appb", State: run.StateAccepted, CreatedAt: "2026-09-03T00:00:00Z", UpdatedAt: "2026-09-03T00:01:00Z"})

	if err := statusMain([]string{"-data-dir", dataDir, "-project", "appa", "-json"}); err != nil {
		t.Fatalf("statusMain -project: %v", err)
	}
	if err := statusMain([]string{"-data-dir", dataDir, "-state", "quarantined", "-json"}); err != nil {
		t.Fatalf("statusMain -state: %v", err)
	}
	if err := statusMain([]string{"-data-dir", dataDir, "-n", "1", "-json"}); err != nil {
		t.Fatalf("statusMain -n: %v", err)
	}
}

// TestStatusMainRejectsNonpositiveN covers the -n validation: zero and
// negative values are rejected up front, before anything is loaded from
// -data-dir.
func TestStatusMainRejectsNonpositiveN(t *testing.T) {
	t.Parallel()
	for _, n := range []string{"0", "-1"} {
		err := statusMain([]string{"-data-dir", t.TempDir(), "-n", n})
		if err == nil {
			t.Errorf("statusMain -n %s: want error, got nil", n)
		}
	}
}

// TestStatusMainMissingDataDirIsNotAnError covers the same convention
// listRuns follows: a -data-dir with no runs subdirectory yet is an empty
// result, not a failure.
func TestStatusMainMissingDataDirIsNotAnError(t *testing.T) {
	t.Parallel()
	if err := statusMain([]string{"-data-dir", t.TempDir(), "-json"}); err != nil {
		t.Fatalf("statusMain: %v, want no error for a data dir with no runs yet", err)
	}
}

// TestBuildStatusEntriesProjectFilterUsesRecordedIDOverDerivation pins
// -project's filter to the recorded id for run records, with the derivation from the path only for a record written
// before the field existed.
func TestBuildStatusEntriesProjectFilterUsesRecordedIDOverDerivation(t *testing.T) {
	t.Parallel()
	runs := []*run.Run{
		{ID: "run-payments", ProjectPath: "/home/u/code/payments", Project: "payments", State: run.StateAccepted, CreatedAt: "2026-09-10T15:30:10Z"},
		{ID: "run-legacy", ProjectPath: "/repos/myapp", State: run.StateAccepted, CreatedAt: "2026-09-10T15:30:11Z"},
	}
	now := time.Now()

	ids := func(project string) []string {
		var out []string
		for _, e := range buildStatusEntries(runs, project, "", 20, now) {
			out = append(out, e.ID+"="+e.Project)
		}
		return out
	}
	if got := ids("payments"); fmt.Sprint(got) != "[run-payments=payments]" {
		t.Errorf("-project payments listed %v, want the recorded-id run only", got)
	}
	if got := ids("code"); len(got) != 0 {
		t.Errorf("-project code listed %v, want nothing -- the legacy derivation must not shadow the recorded id", got)
	}
	if got := ids("myapp"); fmt.Sprint(got) != "[run-legacy=myapp]" {
		t.Errorf("-project myapp listed %v, want the legacy run via the fallback derivation", got)
	}
}

// TestBuildRequestStatusEntriesOldestFirstReversedToNewestFirst covers
// the request table's own ordering: request.List returns oldest-submitted
// first, and buildRequestStatusEntries reverses that to match the
// newest-first convention the run/queue table already uses.
func TestBuildRequestStatusEntriesOldestFirstReversedToNewestFirst(t *testing.T) {
	t.Parallel()
	requests := []*request.Request{
		{ID: "older", Project: "app", State: request.StateSubmitted, SubmittedAt: "2026-09-01T00:00:00Z"},
		{ID: "newer", Project: "app", State: request.StateSpecReview, SubmittedAt: "2026-09-05T00:00:00Z"},
	}
	entries := buildRequestStatusEntries(requests, nil, 1, "", time.Now())
	if len(entries) != 2 || entries[0].ID != "newer" || entries[1].ID != "older" {
		t.Fatalf("entries = %v, want [newer, older]", entries)
	}
}

// TestBuildRequestStatusEntriesFiltersByProject covers -project applying
// to the request table the same way it applies to the run/queue table.
func TestBuildRequestStatusEntriesFiltersByProject(t *testing.T) {
	t.Parallel()
	requests := []*request.Request{
		{ID: "a", Project: "appa", State: request.StateSubmitted, SubmittedAt: "2026-09-01T00:00:00Z"},
		{ID: "b", Project: "appb", State: request.StateSubmitted, SubmittedAt: "2026-09-02T00:00:00Z"},
	}
	entries := buildRequestStatusEntries(requests, nil, 1, "appa", time.Now())
	if len(entries) != 1 || entries[0].ID != "a" {
		t.Fatalf("entries = %v, want only the appa request", entries)
	}
}

// TestStatusMainShowsRequestsAboveRuns is the submit/status
// round-trip: a request written by submitMain against a temp git repo
// with a committed .factory.yml must be visible to statusMain (state,
// project, age), listed alongside a saved run record.
func TestStatusMainShowsRequestsAboveRuns(t *testing.T) {
	dp := newTestDeps(t)
	t.Parallel()
	workspace := t.TempDir()
	writeTestSubmitRepo(t, workspace, `verify_command: "make ci-verify"
preflight_profile: brownfield
`)
	dataDir := t.TempDir()

	if err := submitMain(dp, []string{"-data-dir", dataDir, workspace, "Add idempotency keys to POST /refunds"}); err != nil {
		t.Fatalf("submitMain: %v", err)
	}
	requests, err := request.List(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) != 1 {
		t.Fatalf("len(requests) = %d, want 1", len(requests))
	}

	mustSaveStatusRun(t, dataDir, &run.Run{ID: "run-a", ProjectPath: "/repos/app/workspace", Project: "app", State: run.StateAccepted, CreatedAt: "2026-09-01T00:00:00Z", UpdatedAt: "2026-09-01T00:01:00Z"})

	if err := statusMain([]string{"-data-dir", dataDir}); err != nil {
		t.Fatalf("statusMain: %v", err)
	}
	if err := statusMain([]string{"-data-dir", dataDir, "-json"}); err != nil {
		t.Fatalf("statusMain -json: %v", err)
	}

	entries := buildRequestStatusEntries(requests, nil, 1, "", time.Now())
	if len(entries) != 1 {
		t.Fatalf("buildRequestStatusEntries = %v, want 1 entry", entries)
	}
	if entries[0].State != string(request.StateSubmitted) {
		t.Errorf("entries[0].State = %q, want %q", entries[0].State, request.StateSubmitted)
	}
	if entries[0].Project != "" && entries[0].Project != filepath.Base(workspace) {
		t.Errorf("entries[0].Project = %q, want %q", entries[0].Project, filepath.Base(workspace))
	}
	if entries[0].Age == "" {
		t.Error("entries[0].Age is empty")
	}
}

// TestStatusPlainRowStalledRun covers a non-terminal run whose progress
// feed has gone quiet past progress.StallAfter: the plain-table column
// shows a STALLED prefix and the run's current stage. The stall
// computation itself (progress.Stalled) is now covered directly by
// internal/progress's own TestStalled -- applyProgressStatus is just its
// caller here.
func TestStatusPlainRowStalledRun(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	created := time.Now().Add(-10 * time.Minute)
	r := &run.Run{ID: "run-stalled", ProjectPath: "/repos/app/workspace", State: run.StateSliceRunning, CreatedAt: created.Format(time.RFC3339)}
	mustSaveStatusRun(t, dataDir, r)

	path := progress.Path(dataDir, r.ID)
	if err := progress.Append(path, progress.Event{
		Ts:     time.Now().Add(-7 * time.Minute).UTC().Format("2006-01-02T15:04:05.000Z07:00"),
		Source: "factory", Stage: "build", Event: "start",
	}); err != nil {
		t.Fatalf("append: %v", err)
	}

	now := time.Now()
	entries := buildStatusEntries([]*run.Run{r}, "", "", 20, now)
	applyProgressStatus(entries, []*run.Run{r}, dataDir, now)
	if len(entries) != 1 {
		t.Fatalf("entries = %v, want 1", entries)
	}
	entry := entries[0]
	if !entry.Stalled {
		t.Errorf("Stalled = false, want true")
	}
	if entry.Stage != "build" {
		t.Errorf("Stage = %q, want %q", entry.Stage, "build")
	}
	if got, want := statusProgressColumn(entry), "STALLED 7m build"; got != want {
		t.Errorf("statusProgressColumn = %q, want %q", got, want)
	}
}

// TestStatusPlainRowWaitingRun covers a non-terminal run whose progress
// feed's latest line is a "queued" note: the plain-table column shows
// "waiting: <reason>" and the run is not (yet) considered stalled.
func TestStatusPlainRowWaitingRun(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	created := time.Now().Add(-1 * time.Minute)
	r := &run.Run{ID: "run-waiting", ProjectPath: "/repos/app/workspace", State: run.StateReady, CreatedAt: created.Format(time.RFC3339)}
	mustSaveStatusRun(t, dataDir, r)

	path := progress.Path(dataDir, r.ID)
	if err := progress.Append(path, progress.Event{Source: "factory", Stage: "queued", Event: "note", Detail: "behind 2 run(s) on calc-app"}); err != nil {
		t.Fatalf("append: %v", err)
	}

	now := time.Now()
	entries := buildStatusEntries([]*run.Run{r}, "", "", 20, now)
	applyProgressStatus(entries, []*run.Run{r}, dataDir, now)
	if len(entries) != 1 {
		t.Fatalf("entries = %v, want 1", entries)
	}
	entry := entries[0]
	if entry.Stalled {
		t.Errorf("Stalled = true, want false for a run created 1m ago")
	}
	if entry.WaitingReason != "behind 2 run(s) on calc-app" {
		t.Errorf("WaitingReason = %q, want %q", entry.WaitingReason, "behind 2 run(s) on calc-app")
	}
	if got, want := statusProgressColumn(entry), " · waiting: behind 2 run(s) on calc-app"; got != want {
		t.Errorf("statusProgressColumn = %q, want %q", got, want)
	}
	if strings.Contains(statusProgressColumn(entry), "STALLED") {
		t.Errorf("statusProgressColumn = %q, should not be stalled", statusProgressColumn(entry))
	}
}

func TestTemporalUIURL(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name                       string
		address, workflowID, runID string
		want                       string
	}{
		{
			name:       "default port maps to UI port",
			address:    "localhost:7233",
			workflowID: "run-123",
			runID:      "abc",
			want:       "http://localhost:8233/namespaces/default/workflows/run-123/abc",
		},
		{
			name:       "non-default port yields no URL",
			address:    "temporal.example.com:1234",
			workflowID: "run-123",
			runID:      "abc",
			want:       "",
		},
		{
			name:       "empty workflow ID yields no URL",
			address:    "localhost:7233",
			workflowID: "",
			runID:      "abc",
			want:       "",
		},
		{
			name:       "unparseable address yields no URL",
			address:    "not-a-host-port",
			workflowID: "run-123",
			runID:      "abc",
			want:       "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := TemporalUIURL(c.address, c.workflowID, c.runID); got != c.want {
				t.Errorf("TemporalUIURL(%q, %q, %q) = %q, want %q", c.address, c.workflowID, c.runID, got, c.want)
			}
		})
	}
}

// A request halted only because an accepted ticket has no pull request is
// labelled as accepted-awaiting-PR with the exact next command, not left as a
// bare "halted" failure.
func TestBuildRequestStatusEntryLabelsAcceptedAwaitingPullRequest(t *testing.T) {
	t.Parallel()
	r := &request.Request{
		ID: "req-1", Project: "app", State: request.StateHalted, HaltKind: request.HaltAcceptedNoPR,
		Error:   "ticket 1/1: run run-9 was accepted but no pull request was opened",
		Tickets: []request.Ticket{{Index: 1, RunID: "run-9"}},
	}
	e := buildRequestStatusEntry(r, "", time.Now())
	if !e.AwaitingPullRequest {
		t.Fatalf("entry = %+v, want AwaitingPullRequest", e)
	}
	for _, want := range []string{"accepted, awaiting pull request", "factoryd/run-9", "factoryd retry req-1"} {
		if !strings.Contains(e.Label, want) {
			t.Errorf("label %q missing %q", e.Label, want)
		}
	}
	plain := buildRequestStatusEntry(&request.Request{ID: "r2", State: request.StateHalted, Error: "boom"}, "", time.Now())
	if plain.AwaitingPullRequest || plain.Label != "" {
		t.Errorf("an ordinary halt must not be relabelled: %+v", plain)
	}
}

// TestBuildRequestStatusEntriesSetsWaitingOnForARequestQueuedBehindAnother
// is C5's regression test: of two building requests, the older is the one
// the driver advances and the newer shows "queued behind" it. The running
// ticket's run id has the real shape (<ticket>-<timestamp>-<pid>), not the
// queue-entry id, which an earlier version wrongly matched on.
func TestBuildRequestStatusEntriesSetsWaitingOnForARequestQueuedBehindAnother(t *testing.T) {
	t.Parallel()
	requests := []*request.Request{
		{
			ID: "req-running", Project: "app", State: request.StateBuilding,
			SubmittedAt: "2026-09-01T00:00:00Z", TicketIndex: 1, TicketCount: 1,
			Tickets: []request.Ticket{{Index: 1, RunID: "ticket-1-20260901-000100-4242"}},
		},
		{
			ID: "req-waiting", Project: "app", State: request.StateBuilding,
			SubmittedAt: "2026-09-02T00:00:00Z", TicketIndex: 1, TicketCount: 1,
			Tickets: []request.Ticket{{Index: 1}},
		},
	}

	entries := buildRequestStatusEntries(requests, nil, 1, "", time.Now())
	byID := map[string]requestStatusEntry{}
	for _, e := range entries {
		byID[e.ID] = e
	}
	if got := byID["req-running"].WaitingOn; got != "" {
		t.Errorf("req-running's WaitingOn = %q, want empty (it is the one being built)", got)
	}
	if got := byID["req-waiting"].WaitingOn; got != "req-running" {
		t.Errorf("req-waiting's WaitingOn = %q, want %q", got, "req-running")
	}
}

// TestStatusMainPrintsRouteLine is route-visibility's own regression test
// (2026-09-25): a fresh worker heartbeat carrying a route must produce
// `factoryd status`'s one-line "which subscription gets billed" summary.
func TestStatusMainPrintsRouteLine(t *testing.T) {
	dataDir := t.TempDir()
	hb := daemonheartbeat.Heartbeat{
		UpdatedAt:           time.Now().Format(time.RFC3339Nano),
		RouteCredentialMode: "chatgpt-codex",
		RouteWorkerModel:    "gpt-5.6-luna",
	}
	if err := daemonheartbeat.Write(workerHeartbeatPath(dataDir), hb); err != nil {
		t.Fatal(err)
	}
	out := captureStdout(t, func() {
		if err := statusMain([]string{"-data-dir", dataDir}); err != nil {
			t.Fatalf("statusMain: %v", err)
		}
	})
	if !strings.Contains(out, "worker route: chatgpt-codex · gpt-5.6-luna") {
		t.Errorf("statusMain output = %q, want it to contain the route line", out)
	}
}

// TestStatusMainJSONIncludesRouteFields proves -json carries the same
// route additively alongside the existing queue_run_heartbeat field.
func TestStatusMainJSONIncludesRouteFields(t *testing.T) {
	dataDir := t.TempDir()
	hb := daemonheartbeat.Heartbeat{
		UpdatedAt:           time.Now().Format(time.RFC3339Nano),
		RouteCredentialMode: "chatgpt-codex",
		RouteWorkerModel:    "gpt-5.6-luna",
	}
	if err := daemonheartbeat.Write(workerHeartbeatPath(dataDir), hb); err != nil {
		t.Fatal(err)
	}
	out := captureStdout(t, func() {
		if err := statusMain([]string{"-data-dir", dataDir, "-json"}); err != nil {
			t.Fatalf("statusMain: %v", err)
		}
	})
	var decoded struct {
		WorkerHeartbeat  string `json:"queue_run_heartbeat"`
		WorkerRouteMode  string `json:"queue_run_route_credential_mode"`
		WorkerRouteModel string `json:"queue_run_route_worker_model"`
	}
	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		t.Fatalf("unmarshal status -json output: %v", err)
	}
	if decoded.WorkerHeartbeat != "" {
		t.Errorf("WorkerHeartbeat = %q, want \"\" for a fresh heartbeat", decoded.WorkerHeartbeat)
	}
	if decoded.WorkerRouteMode != "chatgpt-codex" {
		t.Errorf("WorkerRouteMode = %q, want %q", decoded.WorkerRouteMode, "chatgpt-codex")
	}
	if decoded.WorkerRouteModel != "gpt-5.6-luna" {
		t.Errorf("WorkerRouteModel = %q, want %q", decoded.WorkerRouteModel, "gpt-5.6-luna")
	}
}

// TestStatusMainStarsTheQueuedRequestsOwnState: a request queued behind
// another shows its own state starred ("submitted*"), not "building*".
func TestStatusMainStarsTheQueuedRequestsOwnState(t *testing.T) {
	dataDir := t.TempDir()
	building := request.New("req-a", "/repos/app", "app", request.Source{Kind: request.SourceText}, time.Now().Add(-time.Minute))
	building.State = request.StateBuilding
	queued := request.New("req-b", "/repos/app", "app", request.Source{Kind: request.SourceText}, time.Now())
	for _, r := range []*request.Request{building, queued} {
		if err := r.Save(dataDir); err != nil {
			t.Fatal(err)
		}
	}
	out := captureStdout(t, func() {
		if err := statusMain([]string{"-data-dir", dataDir}); err != nil {
			t.Fatalf("statusMain: %v", err)
		}
	})
	if !strings.Contains(out, "submitted*") || !strings.Contains(out, "queued behind req-a") {
		t.Errorf("status output = %q, want req-b as submitted* queued behind req-a", out)
	}
	if strings.Contains(out, "building*") {
		t.Errorf("status output = %q, must not label the submitted request building*", out)
	}
	// Full ids: approve/reject take only a full id.
	if !strings.Contains(out, "req-a ") || !strings.Contains(out, "req-b ") {
		t.Errorf("status output = %q, want both full request ids", out)
	}
}

func TestStatusMainPrintsFullRequestIDsThatShareAPrefix(t *testing.T) {
	dataDir := t.TempDir()
	for i, id := range []string{"add-get-api-v1-habits-completion-rate-1", "add-get-api-v1-mood-weekly-averages-2"} {
		r := request.New(id, "/repos/app", "app", request.Source{Kind: request.SourceText}, time.Now().Add(time.Duration(i)*time.Second))
		if err := r.Save(dataDir); err != nil {
			t.Fatal(err)
		}
	}
	out := captureStdout(t, func() {
		if err := statusMain([]string{"-data-dir", dataDir}); err != nil {
			t.Fatalf("statusMain: %v", err)
		}
	})
	for _, id := range []string{"add-get-api-v1-habits-completion-rate-1", "add-get-api-v1-mood-weekly-averages-2"} {
		if !strings.Contains(out, id) {
			t.Errorf("status output = %q, want full id %q", out, id)
		}
	}
}
