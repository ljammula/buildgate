package request

import (
	"os"
	"strings"
	"testing"
	"time"

	"buildgate/internal/daemonheartbeat"
	"buildgate/internal/progress"
)

func waitReq(id string, state State, entered string) *Request {
	return &Request{ID: id, State: state, EnteredAt: entered}
}

func TestWaitForJobSlot(t *testing.T) {
	r := waitReq("b", StateBuilding, "")
	w := WaitFor(r, WaitFacts{WorkerLive: true, WaitingOn: "a", Position: 2, Slots: 3})
	if w == nil || w.Kind != WaitJobSlot || w.On != "a" || w.Position != 2 || w.Slots != 3 {
		t.Fatalf("wait = %+v, want job_slot on a, #2 of 3", w)
	}
	now := time.Now()
	if got, want := w.Sentence(now), "waiting for a job slot (all 3 in use), #2 in line"; got != want {
		t.Errorf("sentence = %q, want %q", got, want)
	}
	one := WaitFor(r, WaitFacts{WorkerLive: true, WaitingOn: "a", Slots: 1})
	if got, want := one.Sentence(now), "queued behind a"; got != want {
		t.Errorf("one slot sentence = %q, want %q", got, want)
	}
	if WaitFor(r, WaitFacts{WorkerLive: true}) != nil {
		t.Error("a request nothing holds back has a wait")
	}
}

func TestWaitForRepositoryHeldByAnotherBuild(t *testing.T) {
	r := waitReq("b", StateBuilding, "2026-10-20T10:00:00Z")
	w := WaitFor(r, WaitFacts{WorkerLive: true, RepositoryWaitSince: "2026-10-20T10:03:00Z"})
	if w == nil || w.Kind != WaitRepository || w.Since != "2026-10-20T10:03:00Z" {
		t.Fatalf("wait = %+v, want repository since the heartbeat's time", w)
	}
	now, _ := time.Parse(time.RFC3339, "2026-10-20T10:30:00Z")
	if got, want := w.Sentence(now), "waiting for the repository (another build holds it), 27m0s"; got != want {
		t.Errorf("sentence = %q, want %q", got, want)
	}
	if got := (Wait{Kind: WaitRepository, Since: "garbage"}).Sentence(now); got != "waiting for the repository (another build holds it)" {
		t.Errorf("unparseable time sentence = %q", got)
	}
	if pr := WaitFor(waitReq("p", StatePRReview, ""), WaitFacts{WorkerLive: true, RepositoryWaitSince: "2026-10-20T10:03:00Z"}); pr == nil || pr.Kind != WaitRepository {
		t.Errorf("pr_review pass waiting for the repository: %+v", pr)
	}
	if done := WaitFor(waitReq("d", StateDone, ""), WaitFacts{WorkerLive: true, RepositoryWaitSince: "2026-10-20T10:03:00Z"}); done != nil {
		t.Errorf("a finished request has a wait: %+v", done)
	}
}

func TestWaitForRepositoryLockInItsSlot(t *testing.T) {
	r := waitReq("b", StateBuilding, "")
	w := WaitFor(r, WaitFacts{WorkerLive: true, RunWaitStage: "repository_lock", RunWaitSince: "2026-10-20T10:03:00Z", RunWaitOn: "another build holds the repository"})
	if w == nil || w.Kind != WaitRepository || w.On != "another build holds the repository" {
		t.Fatalf("wait = %+v, want repository from the run's feed", w)
	}
}

func TestWaitForRepositoryQueue(t *testing.T) {
	w := WaitFor(waitReq("b", StateBuilding, ""), WaitFacts{WorkerLive: true, RunWaitStage: "queued", RunWaitSince: "t", RunWaitOn: "behind 2 run(s) on acme/app"})
	if w == nil || w.Kind != WaitRepositoryQueue {
		t.Fatalf("wait = %+v, want repository_queue", w)
	}
	if got := w.Sentence(time.Now()); !strings.Contains(got, "repository queue") || !strings.Contains(got, "behind 2 run(s) on acme/app") {
		t.Errorf("sentence = %q", got)
	}
}

func TestWaitForStackedTicketTakesPrecedenceOverProgress(t *testing.T) {
	r := waitReq("b", StateBuilding, "")
	r.StackedWait = &StackedWait{Since: "2026-10-20T10:00:00Z", On: "ticket 2/2 waits: ticket 1's PR branch is at old; the next pass will check again"}
	w := WaitFor(r, WaitFacts{WorkerLive: true, RunWaitStage: "queued", RunWaitOn: "behind another run"})
	if w == nil || w.Kind != WaitStackedTicket || w.On != r.StackedWait.On || w.Since != r.StackedWait.Since {
		t.Fatalf("wait = %+v, want stacked_ticket with its explicit reason", w)
	}
	if got := w.Sentence(time.Now()); !strings.Contains(got, "next pass will check again") {
		t.Errorf("sentence = %q, want stacked-ticket action", got)
	}
}

func TestWaitForModelHost(t *testing.T) {
	w := WaitFor(waitReq("p", StatePlanning, ""), WaitFacts{WorkerLive: true, RunWaitStage: "model_host_lock", RunWaitOn: "queued behind other"})
	if w == nil || w.Kind != WaitModelHost || w.On != "queued behind other" {
		t.Fatalf("wait = %+v, want model_host", w)
	}
	if got, want := w.Sentence(time.Now()), "waiting for the model host: queued behind other"; got != want {
		t.Errorf("sentence = %q, want %q", got, want)
	}
}

func TestWaitForComposeSlot(t *testing.T) {
	w := WaitFor(waitReq("b", StateBuilding, ""), WaitFacts{WorkerLive: true, RunWaitStage: "compose_services_lock", RunWaitOn: "queued behind run-x"})
	if w == nil || w.Kind != WaitComposeSlot {
		t.Fatalf("wait = %+v, want compose_slot", w)
	}
	if got, want := w.Sentence(time.Now()), "waiting for a sidecar slot: queued behind run-x"; got != want {
		t.Errorf("sentence = %q, want %q", got, want)
	}
}

func TestWaitForNoWorker(t *testing.T) {
	for _, state := range []State{StateSubmitted, StateSpecDrafting, StatePlanning, StateBuilding} {
		w := WaitFor(waitReq("x", state, "2026-10-20T10:00:00Z"), WaitFacts{})
		if w == nil || w.Kind != WaitNoWorker || w.Since != "" {
			t.Errorf("%s without a worker: %+v, want no_worker with no start time", state, w)
		}
	}
	// Since is when the worker was last seen, when a heartbeat was read.
	if w := WaitFor(waitReq("x", StateBuilding, "2026-10-20T10:00:00Z"), WaitFacts{WorkerLastSeen: "2026-10-20T09:59:00Z"}); w.Since != "2026-10-20T09:59:00Z" {
		t.Errorf("Since = %q, want the last heartbeat", w.Since)
	}
	// A heartbeat that could not be read says nothing about the worker.
	if w := WaitFor(waitReq("x", StateBuilding, ""), WaitFacts{WorkerUnknown: true, WaitingOn: "a", Slots: 1}); w != nil {
		t.Errorf("unreadable heartbeat: %+v, want no wait", w)
	}
	// A human gate waits on a person, not a worker.
	if w := WaitFor(waitReq("x", StateSpecReview, ""), WaitFacts{}); w != nil {
		t.Errorf("spec_review without a worker: %+v, want none", w)
	}
	if got := (Wait{Kind: WaitNoWorker}).Sentence(time.Now()); !strings.Contains(got, "no worker") {
		t.Errorf("sentence = %q", got)
	}
}

func TestWaitForPrecedence(t *testing.T) {
	r := waitReq("b", StateBuilding, "")
	all := WaitFacts{
		WorkerLive: true, WaitingOn: "a", Position: 1, Slots: 2,
		RepositoryWaitSince: "2026-10-20T10:03:00Z",
		RunWaitStage:        "model_host_lock", RunWaitOn: "queued behind other",
	}
	if w := WaitFor(r, all); w.Kind != WaitModelHost {
		t.Errorf("feed + heartbeat + slots: %s, want model_host", w.Kind)
	}
	all.RunWaitStage = ""
	if w := WaitFor(r, all); w.Kind != WaitRepository {
		t.Errorf("heartbeat + slots: %s, want repository", w.Kind)
	}
	all.RepositoryWaitSince = ""
	if w := WaitFor(r, all); w.Kind != WaitJobSlot {
		t.Errorf("slots only: %s, want job_slot", w.Kind)
	}
	all.WorkerLive = false
	all.RunWaitStage = "model_host_lock"
	if w := WaitFor(r, all); w.Kind != WaitNoWorker {
		t.Errorf("no worker: %s, want no_worker over everything", w.Kind)
	}
}

func TestWaitForDropsAWaitFromBeforeTheCurrentState(t *testing.T) {
	r := waitReq("b", StateBuilding, "2026-10-20T10:00:00.400Z")
	for _, c := range []struct {
		since string
		want  bool
	}{
		{"2026-10-20T09:30:00Z", false},
		{"2026-10-20T10:00:00Z", true},
		{"2026-10-20T10:05:00Z", true},
		{"not a time", true},
	} {
		heartbeat := WaitFor(r, WaitFacts{WorkerLive: true, RepositoryWaitSince: c.since}) != nil
		feed := WaitFor(r, WaitFacts{WorkerLive: true, RunWaitStage: "model_host_lock", RunWaitSince: c.since}) != nil
		if heartbeat != c.want || feed != c.want {
			t.Errorf("wait since %q: heartbeat shown %v, feed shown %v, want %v", c.since, heartbeat, feed, c.want)
		}
	}
}

// TestWaitForIgnoresWorkerLines: a line the sandboxed worker printed never
// names a wait. A worker-sourced model_host_lock or repository_lock line in a
// request's feed, even one that parses as a progress event, is not read.
func TestWaitForIgnoresWorkerLines(t *testing.T) {
	dataDir := t.TempDir()
	r := &Request{ID: "req-1", State: StatePlanning, EnteredAt: "2026-10-20T10:00:00Z"}
	for _, stage := range []string{"model_host_lock", "compose_services_lock", "repository_lock", "queued"} {
		for _, event := range []string{"waiting", "note"} {
			e := progress.Event{Source: "worker", Stage: stage, Event: event, Detail: "queued behind forged"}
			if err := progress.Append(progress.Path(dataDir, r.ID), e); err != nil {
				t.Fatal(err)
			}
		}
	}
	c := &WaitContext{DataDir: dataDir, Live: true, Slots: 1}
	c.Number([]*Request{r})
	if w := c.For(r); w != nil {
		t.Errorf("wait = %+v from worker lines, want none", w)
	}
	// The same line from the factory is a wait.
	if err := progress.Mark(progress.Path(dataDir, r.ID), "model_host_lock", "waiting", "", "queued behind real"); err != nil {
		t.Fatal(err)
	}
	if w := c.For(r); w == nil || w.Kind != WaitModelHost || w.On != "queued behind real" {
		t.Errorf("wait = %+v from a factory line, want model_host", w)
	}
}

// TestWaitContextTellsAMissingHeartbeatFromAnUnreadableOne: only a missing or
// stale heartbeat means no worker; one that cannot be parsed is unknown.
func TestWaitContextTellsAMissingHeartbeatFromAnUnreadableOne(t *testing.T) {
	r := &Request{ID: "r", State: StateBuilding}
	dir := t.TempDir()
	if w := NewWaitContext(dir, []*Request{r}, time.Now()).For(r); w == nil || w.Kind != WaitNoWorker {
		t.Errorf("missing heartbeat: %+v, want no_worker", w)
	}
	if err := os.WriteFile(daemonheartbeat.WorkerPath(dir), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if w := NewWaitContext(dir, []*Request{r}, time.Now()).For(r); w != nil {
		t.Errorf("unparseable heartbeat: %+v, want no wait", w)
	}
	old := time.Now().Add(-time.Hour).Format(time.RFC3339Nano)
	if err := daemonheartbeat.Write(daemonheartbeat.WorkerPath(dir), daemonheartbeat.Heartbeat{UpdatedAt: old}); err != nil {
		t.Fatal(err)
	}
	if w := NewWaitContext(dir, []*Request{r}, time.Now()).For(r); w == nil || w.Kind != WaitNoWorker || w.Since != old {
		t.Errorf("stale heartbeat: %+v, want no_worker since its last beat", w)
	}
}

func TestWaitForBranchLockNamesTheBranch(t *testing.T) {
	w := WaitFor(waitReq("b", StateBuilding, ""), WaitFacts{WorkerLive: true, RunWaitStage: "branch_lock", RunWaitSince: "2026-10-20T10:03:00Z", RunWaitOn: "branch pr-a"})
	if w == nil || w.Kind != WaitRepository {
		t.Fatalf("wait = %+v, want repository kind", w)
	}
	if got := w.Sentence(time.Now()); !strings.Contains(got, "branch pr-a") || strings.Contains(got, "another build holds it") {
		t.Errorf("sentence = %q, want it to name branch pr-a", got)
	}
}
