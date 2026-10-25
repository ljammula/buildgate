package request

import (
	"fmt"
	"time"
)

// WaitKind names why a request is waiting rather than running.
type WaitKind string

// The kinds of Wait.
const (
	// WaitNoWorker: a job-state request and no live worker heartbeat.
	WaitNoWorker WaitKind = "no_worker"
	// WaitModelHost: queued behind another job on a single-instance model host.
	WaitModelHost WaitKind = "model_host"
	// WaitComposeSlot: queued behind another run's compose sidecars.
	WaitComposeSlot WaitKind = "compose_slot"
	// WaitRepositoryQueue: queued behind other runs of one repository.
	WaitRepositoryQueue WaitKind = "repository_queue"
	// WaitRepository: another build holds the repository.
	WaitRepository WaitKind = "repository"
	// WaitJobSlot: every job slot of the worker is taken.
	WaitJobSlot WaitKind = "job_slot"
	// WaitStackedTicket: a building ticket cannot start until the earlier
	// ticket's open PR branch has the run the factory selected as its head.
	WaitStackedTicket WaitKind = "stacked_ticket"
)

// StackedWait is why a stacked ticket's build has not started: the earlier
// ticket's open pull-request branch is not at the commit of that ticket's
// current PR-head run, or it could not be read. The worker checks again on
// its next pass.
type StackedWait struct {
	// Since is when the wait began (RFC 3339, UTC).
	Since string `json:"since"`
	// On is the operator sentence: what the branch is at and what to do.
	On string `json:"on"`
}

// Wait is why a request is waiting, as `factoryd status -json`, the API and
// the console carry it. Everything in it is derived from facts the factory
// wrote itself (the worker heartbeat, factory progress stages), never from a
// line the sandboxed worker printed.
type Wait struct {
	Kind WaitKind `json:"kind"`
	// Since is when the wait began (RFC3339), when known.
	Since string `json:"since,omitempty"`
	// On is what the request waits behind: a request id for a job_slot, the
	// holder text of the lock for the others.
	On string `json:"on,omitempty"`
	// Position is the request's place in line (job_slot, from 1); Slots how
	// many jobs the worker runs at once.
	Position int `json:"position,omitempty"`
	Slots    int `json:"slots,omitempty"`
}

// WaitFacts is what WaitFor decides from, gathered by the caller.
type WaitFacts struct {
	// WorkerLive is whether a worker heartbeat is fresh. WorkerUnknown is
	// set instead when the heartbeat could not be read (not that it is
	// missing or stale): no wait that depends on the worker is named then.
	// WorkerLastSeen is the last heartbeat's time, when one was read.
	WorkerLive     bool
	WorkerUnknown  bool
	WorkerLastSeen string
	// WaitingOn, Position and Slots are request.WaitingOn's and
	// QueuePositions' answers for the request, and the heartbeat's slot count.
	WaitingOn string
	Position  int
	Slots     int
	// RepositoryWaitSince is the heartbeat's repository wait for the request.
	RepositoryWaitSince string
	// RunWaitStage, RunWaitSince and RunWaitOn are the wait the request's
	// progress feed reports (progress.RunSummary.WaitingStage, WaitingSince
	// and WaitingReason).
	RunWaitStage, RunWaitSince, RunWaitOn string
}

// WaitFor says why r is waiting, or nil when it is not. In order: no worker
// for a job-state request; an explicit stacked-ticket wait; the wait its
// progress feed names (model host, compose sidecars, repository queue,
// repository lock); the repository wait the heartbeat reports; every job
// slot taken. A wait recorded before r entered its current state belongs to
// an earlier attempt and is dropped.
func WaitFor(r *Request, f WaitFacts) *Wait {
	if r == nil {
		return nil
	}
	if r.State.RunsJob() && !f.WorkerLive && !f.WorkerUnknown {
		return &Wait{Kind: WaitNoWorker, Since: f.WorkerLastSeen}
	}
	if r.State == StateBuilding && r.StackedWait != nil {
		return &Wait{Kind: WaitStackedTicket, Since: r.StackedWait.Since, On: r.StackedWait.On}
	}
	if r.State.RunsJob() {
		if kind, ok := WaitKindForStage(f.RunWaitStage); ok && !r.waitIsStale(f.RunWaitSince) {
			return &Wait{Kind: kind, Since: f.RunWaitSince, On: f.RunWaitOn}
		}
	}
	if (r.State == StateBuilding || r.State == StatePRReview) && f.RepositoryWaitSince != "" && !r.waitIsStale(f.RepositoryWaitSince) {
		return &Wait{Kind: WaitRepository, Since: f.RepositoryWaitSince}
	}
	if r.State.RunsJob() && f.WaitingOn != "" && !f.WorkerUnknown {
		return &Wait{Kind: WaitJobSlot, On: f.WaitingOn, Position: f.Position, Slots: f.Slots}
	}
	return nil
}

// WaitKindForStage maps a progress stage a wait is recorded under to its kind.
func WaitKindForStage(stage string) (WaitKind, bool) {
	switch stage {
	case "model_host_lock":
		return WaitModelHost, true
	case "compose_services_lock":
		return WaitComposeSlot, true
	case "queued":
		return WaitRepositoryQueue, true
	case "repository_lock":
		return WaitRepository, true
	}
	return "", false
}

// waitIsStale reports whether a wait that began at since was recorded before
// r entered its current state: a wait left from before a halt and a retry is
// not this attempt's. An unparseable since is not stale.
func (r *Request) waitIsStale(since string) bool {
	at, err := time.Parse(time.RFC3339, since)
	entered, ok := r.InStateSince()
	return err == nil && ok && at.Before(entered.Truncate(time.Second))
}

// Sentence is the wait as one operator-facing line, with how long it has
// lasted when Since parses.
func (w Wait) Sentence(now time.Time) string {
	var text string
	switch w.Kind {
	case WaitNoWorker:
		text = "no worker is running; this request waits for one"
	case WaitModelHost:
		text = "waiting for the model host" + onSuffix(w.On)
	case WaitComposeSlot:
		text = "waiting for a sidecar slot" + onSuffix(w.On)
	case WaitRepositoryQueue:
		text = "waiting in the repository queue" + onSuffix(w.On)
	case WaitRepository:
		text = "waiting for the repository (another build holds it)"
	case WaitJobSlot:
		if w.Slots <= 1 {
			return "queued behind " + w.On
		}
		text = fmt.Sprintf("waiting for a job slot (all %d in use)", w.Slots)
		if w.Position > 0 {
			text += fmt.Sprintf(", #%d in line", w.Position)
		}
		return text
	case WaitStackedTicket:
		text = w.On
	default:
		text = "waiting"
	}
	if since, err := time.Parse(time.RFC3339, w.Since); err == nil {
		text += ", " + now.Sub(since).Round(time.Second).String()
	}
	return text
}

func onSuffix(on string) string {
	if on == "" {
		return ""
	}
	return ": " + on
}
