package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"buildgate/internal/progress"
	"buildgate/internal/request"
)

// TestStatusShowsEachWaitKind: every kind of wait reaches `factoryd status`
// as a starred state and its one sentence, and `status -json` as a typed
// wait.
func TestStatusShowsEachWaitKind(t *testing.T) {
	now := time.Date(2026, 10, 20, 10, 30, 0, 0, time.UTC)
	dataDir := t.TempDir()
	building := func(id, runID, at string) *request.Request {
		return &request.Request{ID: id, Project: "app", State: request.StateBuilding, SubmittedAt: at, EnteredAt: "2026-10-20T09:00:00Z", TicketIndex: 1, TicketCount: 1, Tickets: []request.Ticket{{Index: 1, RunID: runID}}}
	}
	requests := []*request.Request{
		{ID: "r-running", Project: "app", State: request.StateBuilding, SubmittedAt: "2026-10-20T08:00:00Z", EnteredAt: "2026-10-20T09:00:00Z", TicketIndex: 1, TicketCount: 1, Tickets: []request.Ticket{{Index: 1, RunID: "r-running-run"}}},
		building("r-slot", "", "2026-10-20T08:01:00Z"),
		building("r-repo", "", "2026-10-20T08:02:00Z"),
		building("r-queue", "r-queue-run", "2026-10-20T08:03:00Z"),
		{ID: "r-host", Project: "app", State: request.StatePlanning, SubmittedAt: "2026-10-20T08:04:00Z", EnteredAt: "2026-10-20T09:00:00Z"},
		building("r-compose", "r-compose-run", "2026-10-20T08:05:00Z"),
		building("r-lock", "r-lock-run", "2026-10-20T08:06:00Z"),
	}
	mark := func(id, stage, event, detail string) {
		t.Helper()
		if err := progress.Append(progress.Path(dataDir, id), progress.Event{Ts: "2026-10-20T10:20:00.000Z", Source: "factory", Stage: stage, Event: event, Detail: detail}); err != nil {
			t.Fatal(err)
		}
	}
	mark("r-queue-run", "queued", "note", "behind 2 run(s) on acme/app")
	mark("r-host", "model_host_lock", "waiting", "queued behind r-other")
	mark("r-compose-run", "compose_services_lock", "waiting", "queued behind run-x")
	mark("r-lock-run", "repository_lock", "waiting", "another build holds the repository")
	waits := &request.WaitContext{DataDir: dataDir, Live: true, Holders: []string{"r-running"}, Slots: 1, RepositoryWaits: map[string]string{"r-repo": "2026-10-20T10:03:00Z"}}
	waits.Number(requests)

	entries := buildRequestStatusEntries(requests, waits, "", now)
	byID := map[string]requestStatusEntry{}
	for _, e := range entries {
		byID[e.ID] = e
	}
	out := captureStdout(t, func() {
		if err := printRequestStatusRows(entries, now); err != nil {
			t.Fatal(err)
		}
	})
	rows := map[string]string{}
	for _, l := range strings.Split(out, "\n") {
		if f := strings.Fields(l); len(f) > 0 {
			rows[f[0]] = l
		}
	}

	for _, c := range []struct {
		id   string
		kind request.WaitKind
		text string
	}{
		{"r-slot", request.WaitJobSlot, "queued behind r-running"},
		{"r-repo", request.WaitRepository, "waiting for the repository (another build holds it), 27m0s"},
		{"r-queue", request.WaitRepositoryQueue, "waiting in the repository queue: behind 2 run(s) on acme/app, 10m0s"},
		{"r-host", request.WaitModelHost, "waiting for the model host: queued behind r-other, 10m0s"},
		{"r-compose", request.WaitComposeSlot, "waiting for a sidecar slot: queued behind run-x, 10m0s"},
		{"r-lock", request.WaitRepository, "waiting for the repository (another build holds it), 10m0s"},
	} {
		if w := byID[c.id].Wait; w == nil || w.Kind != c.kind {
			t.Errorf("%s: wait = %+v, want kind %s", c.id, w, c.kind)
		}
		if !strings.Contains(rows[c.id], c.text) || !strings.Contains(rows[c.id], "*") {
			t.Errorf("%s row = %q, want a starred state and %q", c.id, rows[c.id], c.text)
		}
	}
	if byID["r-running"].Wait != nil || strings.Contains(rows["r-running"], "*") {
		t.Errorf("the running request is shown waiting: %+v / %q", byID["r-running"].Wait, rows["r-running"])
	}

	// No worker heartbeat: every job-state request says so.
	idle := &request.WaitContext{DataDir: dataDir, Slots: 1}
	idle.Number(requests)
	noWorker := captureStdout(t, func() {
		if err := printRequestStatusRows(buildRequestStatusEntries(requests, idle, "", now), now); err != nil {
			t.Fatal(err)
		}
	})
	if got := strings.Count(noWorker, "no worker is running"); got != len(requests) {
		t.Errorf("rows naming no worker = %d, want %d:\n%s", got, len(requests), noWorker)
	}

	b, err := json.Marshal(byID["r-host"])
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"wait":{"kind":"model_host"`, `"on":"queued behind r-other"`} {
		if !strings.Contains(string(b), key) {
			t.Errorf("-json entry %s lacks %s", b, key)
		}
	}
	for _, gone := range []string{"waiting_on", "queue_position", "job_slots", "repository_wait_since"} {
		if strings.Contains(string(b), gone) {
			t.Errorf("-json entry still carries %s", gone)
		}
	}
}
