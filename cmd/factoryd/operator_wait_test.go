package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"buildgate/internal/consolelink"
	"buildgate/internal/daemonheartbeat"
	"buildgate/internal/request"
)

// slotWaitFixture is a data dir whose worker runs two jobs at once, both
// slots held by pr_review passes (which hold a slot but are not jobs, so the
// heartbeat's ActiveRequests is empty), and a request whose plan is approved
// and whose first of three tickets has not started.
func slotWaitFixture(t *testing.T) string {
	t.Helper()
	dataDir := t.TempDir()
	now := time.Now()
	for i, id := range []string{"pr-a", "pr-b"} {
		r := request.New(id, "/repos/app", "app", request.Source{Kind: request.SourceText}, now.Add(time.Duration(i-10)*time.Minute))
		r.State = request.StatePRReview
		r.TicketIndex, r.TicketCount = 1, 1
		r.Tickets = []request.Ticket{{Index: 1, RunID: id + "-001-20261012-050000-1", PRURL: "https://github.com/o/r/pull/1"}}
		if err := r.Save(dataDir); err != nil {
			t.Fatal(err)
		}
	}
	queued := request.New("queued", "/repos/app", "app", request.Source{Kind: request.SourceText}, now.Add(-5*time.Minute))
	queued.State = request.StateBuilding
	queued.TicketCount = 3
	queued.Tickets = []request.Ticket{{Index: 1}, {Index: 2}, {Index: 3}}
	if err := queued.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	// Save stamps its own EnteredAt on a state change; pin the one under test.
	loaded, err := request.Load(dataDir, "queued")
	if err != nil {
		t.Fatal(err)
	}
	loaded.EnteredAt = now.Add(-7 * time.Minute).UTC().Format(time.RFC3339Nano)
	if err := loaded.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	hb := daemonheartbeat.Heartbeat{
		UpdatedAt:       now.Format(time.RFC3339Nano),
		JobSlots:        2,
		SlotRequests:    []string{"pr-a", "pr-b"},
		TemporalAddress: "localhost:7233",
	}
	if err := daemonheartbeat.Write(workerHeartbeatPath(dataDir), hb); err != nil {
		t.Fatal(err)
	}
	return dataDir
}

// A request waiting for a job slot that pr_review passes hold says so in
// status, with its place in line and how long it has been building.
func TestStatusSaysARequestWaitsForASlotThePRReviewPassesHold(t *testing.T) {
	dataDir := slotWaitFixture(t)
	out := captureStdout(t, func() {
		if err := statusMain([]string{"-data-dir", dataDir}); err != nil {
			t.Fatalf("statusMain: %v", err)
		}
	})
	var line string
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "queued ") {
			line = l
		}
	}
	for _, want := range []string{"building*", "0/3", "waiting for a job slot (all 2 in use), #1 in line", "7m0"} {
		if !strings.Contains(line, want) {
			t.Errorf("queued's row lacks %q: %q\n%s", want, line, out)
		}
	}

	js := captureStdout(t, func() {
		if err := statusMain([]string{"-data-dir", dataDir, "-json"}); err != nil {
			t.Fatalf("statusMain -json: %v", err)
		}
	})
	var decoded struct {
		Requests []requestStatusEntry `json:"requests"`
	}
	if err := json.Unmarshal([]byte(js), &decoded); err != nil {
		t.Fatal(err)
	}
	for _, r := range decoded.Requests {
		if r.ID != "queued" {
			continue
		}
		if r.WaitingOn == "" || r.QueuePosition != 1 || r.JobSlots != 2 || r.InState == "" {
			t.Errorf("queued in -json = %+v, want waiting_on, queue_position 1, job_slots 2, in_state", r)
		}
	}
}

// watch on a building request whose first ticket has not started reports the
// wait instead of failing on ticket index 0.
func TestWatchRequestWhoseFirstTicketHasNotStartedSaysWhatItWaitsFor(t *testing.T) {
	dataDir := slotWaitFixture(t)
	t.Setenv(consolelink.EnvVar, "")
	outFile, err := os.Create(filepath.Join(t.TempDir(), "out"))
	if err != nil {
		t.Fatal(err)
	}
	defer outFile.Close()
	if err := watchRequest(outFile, dataDir, "queued", true); err != nil {
		t.Fatalf("watchRequest: %v", err)
	}
	got, err := os.ReadFile(outFile.Name())
	if err != nil {
		t.Fatal(err)
	}
	if want := "ticket 1/3 has not started a run yet: waiting for a job slot (all 2 in use), #1 in line"; !strings.Contains(string(got), want) {
		t.Errorf("watch output lacks %q:\n%s", want, got)
	}
}

// logs on that request says the ticket has not started, rather than printing
// an earlier step's log as if it were the build's.
func TestLogsOfARequestWhoseTicketHasNotStartedSaysSo(t *testing.T) {
	dataDir := slotWaitFixture(t)
	writeLog(t, filepath.Join(request.Dir(dataDir, "queued"), "logs", "plan_tickets.log"), "", 0)
	out, err := runLogsString(t, dataDir, "queued", false, false, 40)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "ticket 1/3 has not started a run yet: waiting for a job slot") || strings.Contains(out, "==> ") {
		t.Errorf("logs output = %q, want the not-started line and no earlier log", out)
	}
}

func TestInboxNarrowsToOneProfileAndOneProject(t *testing.T) {
	f := newProfileFixture(t, "default", "work")
	t.Setenv(consolelink.EnvVar, "")
	saveWaitingRequest(t, f.roots["default"], "spec-a", "Add login page", request.StateSpecReview, time.Hour, nil)
	saveWaitingRequest(t, f.roots["work"], "plan-b", "Split the module", request.StatePlanReview, time.Hour, nil)
	saveWaitingRequest(t, f.roots["work"], "plan-c", "Other repo", request.StatePlanReview, time.Hour, func(r *request.Request) {
		r.Project = "other"
	})

	ids := func(args ...string) string {
		t.Helper()
		var out, warn bytes.Buffer
		if err := inboxRun(args, &out, &warn); err != nil {
			t.Fatalf("inboxRun %v: %v", args, err)
		}
		var got []string
		for _, line := range strings.Split(out.String(), "\n") {
			if line != "" && !strings.HasPrefix(line, " ") {
				got = append(got, strings.Fields(line)[3])
			}
		}
		return strings.Join(got, ",")
	}
	if got := ids("-config", "work"); got != "plan-b,plan-c" {
		t.Errorf("-config work lists %q, want plan-b,plan-c", got)
	}
	if got := ids("-project", "app"); got != "spec-a,plan-b" {
		t.Errorf("-project app lists %q, want spec-a,plan-b", got)
	}
	if got := ids("-config", "work", "-project", "other"); got != "plan-c" {
		t.Errorf("-config work -project other lists %q, want plan-c", got)
	}
}

func TestRunIDColumnTellsTwoBuildsOfOneTicketApart(t *testing.T) {
	t.Parallel()
	ticket := "retry-strategies-in-tenacity-retry-py-co-20261012-040851-001"
	for id, want := range map[string]string{
		ticket + "-20261012-052916-60469":             ticket + " 20261012-052916-60469",
		ticket + "-corrective1-20261012-053500-60469": ticket + " corrective1-20261012-053500-60469",
	} {
		if got := runIDColumn(statusEntry{ID: id, Ticket: ticket}); got != want {
			t.Errorf("runIDColumn(%q) = %q, want %q", id, got, want)
		}
	}
	if got := runIDColumn(statusEntry{ID: "queue-entry-1"}); got != "queue-entry-1" {
		t.Errorf("a run with no ticket prints %q, want its whole id", got)
	}
}

// The heartbeat lists running jobs first, so a queued request is said to wait
// behind a build rather than a pull request review pass that started earlier.
func TestSlotHoldersListRunningJobsFirst(t *testing.T) {
	t.Cleanup(clearActiveRequests)
	slotRequests.add("pr-pass")
	slotRequests.add("build")
	addActiveRequest("build")
	if got, want := slotHolders(), []string{"build", "pr-pass"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("slotHolders() = %v, want %v", got, want)
	}
}

// inbox -config with a config file that is not a profile prints commands that
// name that file, not a profile its base name might match.
func TestInboxWithAConfigPathPrintsCommandsForThatPath(t *testing.T) {
	newProfileFixture(t, "default") // an isolated profile home
	t.Setenv(consolelink.EnvVar, "")
	dataDir := t.TempDir()
	cfg := filepath.Join(t.TempDir(), "work.yml")
	if err := os.WriteFile(cfg, []byte("data_dir: "+dataDir+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	saveWaitingRequest(t, dataDir, "spec-a", "Add login page", request.StateSpecReview, time.Hour, nil)
	var out, warn bytes.Buffer
	if err := inboxRun([]string{"-config", cfg}, &out, &warn); err != nil {
		t.Fatalf("inboxRun: %v", err)
	}
	if want := "factoryd approve -config " + request.ShellPath(cfg) + " spec-a"; !strings.Contains(out.String(), want) {
		t.Errorf("inbox output lacks %q:\n%s", want, out.String())
	}
}

// logs -f on that request says so and tails no earlier step's log while it
// waits for the ticket's run.
func TestLogsFollowOfARequestWhoseTicketHasNotStartedTailsNothing(t *testing.T) {
	dataDir := slotWaitFixture(t)
	writeLog(t, filepath.Join(request.Dir(dataDir, "queued"), "logs", "plan_tickets.log"), "planned\n", 0)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	var buf lockedBuffer
	if err := runLogs(ctx, &buf, dataDir, "queued", false, true, 40); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.HasPrefix(out, "ticket 1/3 has not started a run yet") || strings.Contains(out, "planned") {
		t.Errorf("logs -f output = %q, want the not-started line and no earlier log", out)
	}
}
