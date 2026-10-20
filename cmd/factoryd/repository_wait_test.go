package main

import (
	"strings"
	"testing"
	"time"

	"buildgate/internal/daemonheartbeat"
	"buildgate/internal/request"
)

// TestRepositoryWaitTracker: the earliest time of a wait is kept, a clear drops
// one id, a reset drops all, and every change wakes the heartbeat writer.
func TestRepositoryWaitTracker(t *testing.T) {
	var tr repositoryWaitTracker
	drain := func() {
		select {
		case <-activeRequestChanged:
		default:
		}
	}
	drain()
	first := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)

	tr.markBusy("a", first)
	select {
	case <-activeRequestChanged:
	default:
		t.Error("markBusy did not wake the heartbeat writer")
	}
	tr.markBusy("a", first.Add(time.Minute)) // still waiting: the earliest time stays
	tr.markBusy("b", first.Add(2*time.Minute))
	want := map[string]string{"a": "2026-10-20T10:00:00Z", "b": "2026-10-20T10:02:00Z"}
	got := tr.snapshot()
	if len(got) != 2 || got["a"] != want["a"] || got["b"] != want["b"] {
		t.Errorf("snapshot = %v, want %v", got, want)
	}

	tr.clear("a")
	if got := tr.snapshot(); len(got) != 1 || got["b"] != want["b"] {
		t.Errorf("after clear(a): %v, want only b", got)
	}
	tr.clear("never-waited") // no entry: nothing to drop
	tr.reset()
	if got := tr.snapshot(); got != nil {
		t.Errorf("after reset: %v, want nil", got)
	}
	drain()
}

// TestClearActiveRequestsForgetsRepositoryWaits: the worker's final heartbeat
// lists no wait.
func TestClearActiveRequestsForgetsRepositoryWaits(t *testing.T) {
	repositoryWaits.markBusy("lost-wait", time.Now())
	clearActiveRequests()
	if got := repositoryWaits.snapshot(); got != nil {
		t.Errorf("waits after clear = %v, want none", got)
	}
}

// TestRepositoryWaitReachesStatus: a wait the heartbeat carries shows on a
// building request only, with its exact reason text, and a stale wait on a
// request in another state is ignored.
func TestRepositoryWaitReachesStatus(t *testing.T) {
	now := time.Date(2026, 10, 20, 10, 30, 0, 0, time.UTC)
	requests := []*request.Request{
		{ID: "req-building", Project: "app", State: request.StateBuilding, SubmittedAt: "2026-10-20T09:00:00Z", TicketIndex: 1, TicketCount: 1},
		{ID: "req-reviewing", Project: "app", State: request.StatePRReview, SubmittedAt: "2026-10-20T09:01:00Z", TicketIndex: 1, TicketCount: 1},
	}
	waits := map[string]string{"req-building": "2026-10-20T10:03:00Z", "req-reviewing": "2026-10-20T10:03:00Z"}
	entries := buildRequestStatusEntries(requests, nil, 1, waits, "", now)
	byID := map[string]requestStatusEntry{}
	for _, e := range entries {
		byID[e.ID] = e
	}
	if got := byID["req-building"].RepositoryWaitSince; got != "2026-10-20T10:03:00Z" {
		t.Errorf("building request's RepositoryWaitSince = %q, want the heartbeat's time", got)
	}
	if got := byID["req-reviewing"].RepositoryWaitSince; got != "" {
		t.Errorf("pr_review request's RepositoryWaitSince = %q, want empty", got)
	}
	want := "waiting for the repository (another build holds it), 27m0s"
	if got := requestRepositoryWaitText(byID["req-building"], now); got != want {
		t.Errorf("reason = %q, want %q", got, want)
	}
	if got := requestRepositoryWaitText(requestStatusEntry{RepositoryWaitSince: "garbage"}, now); got != "waiting for the repository (another build holds it)" {
		t.Errorf("unparseable time reason = %q", got)
	}

	out := captureStdout(t, func() {
		if err := printRequestStatusRows(entries, now); err != nil {
			t.Fatal(err)
		}
	})
	var buildingLine string
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "req-building") {
			buildingLine = l
		}
	}
	if !strings.Contains(buildingLine, "building*") || !strings.Contains(buildingLine, want) {
		t.Errorf("building row = %q, want a starred state and %q", buildingLine, want)
	}
}

// TestHeartbeatCarriesRepositoryWaits: the worker heartbeat writes the tracker's
// waits and the reader returns them.
func TestHeartbeatCarriesRepositoryWaits(t *testing.T) {
	repositoryWaits.reset()
	t.Cleanup(repositoryWaits.reset)
	repositoryWaits.markBusy("req-1", time.Date(2026, 10, 20, 10, 3, 0, 0, time.UTC))
	dir := t.TempDir()
	stop, err := startWorkerHeartbeat(t.Context(), dir, "", "", "localhost:7233", "", 1)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	got := daemonheartbeat.WorkerRepositoryWaits(dir, time.Now())
	if len(got) != 1 || got["req-1"] != "2026-10-20T10:03:00Z" {
		t.Errorf("WorkerRepositoryWaits = %v, want req-1 at 10:03", got)
	}
}

// A wait recorded before the request entered its current state (before a halt
// and a retry) is not shown; one from the same second or later is.
func TestCurrentRepositoryWaitDropsAWaitFromBeforeTheState(t *testing.T) {
	r := &request.Request{EnteredAt: "2026-10-20T10:00:00.400Z"}
	for _, c := range []struct{ since, want string }{
		{"2026-10-20T09:30:00Z", ""},
		{"2026-10-20T10:00:00Z", "2026-10-20T10:00:00Z"},
		{"2026-10-20T10:05:00Z", "2026-10-20T10:05:00Z"},
		{"not a time", "not a time"},
	} {
		if got := currentRepositoryWait(r, c.since); got != c.want {
			t.Errorf("currentRepositoryWait(%q) = %q, want %q", c.since, got, c.want)
		}
	}
}
