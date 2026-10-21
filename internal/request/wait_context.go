package request

import (
	"errors"
	"os"
	"time"

	"buildgate/internal/daemonheartbeat"
	"buildgate/internal/progress"
)

// WaitContext holds what a list of requests' waits are decided from, read
// once: the worker heartbeat and each request's place among the others.
// `factoryd status`, `factoryd watch` and the API build one and ask it for
// each request, so they agree.
type WaitContext struct {
	// DataDir is where the progress feeds live.
	DataDir string
	// Live, Holders, Slots and RepositoryWaits come from the worker
	// heartbeat (Holders: its SlotRequests).
	Live bool
	// Unknown: the heartbeat exists but could not be read or parsed. A
	// missing or stale heartbeat is Live false, not Unknown.
	Unknown bool
	// LastSeen is the heartbeat's UpdatedAt, when one was read.
	LastSeen        string
	Holders         []string
	Slots           int
	RepositoryWaits map[string]string

	waitingOn map[string]string
	positions map[string]int
}

// NewWaitContext reads dataDir's worker heartbeat as of now and numbers
// requests, which must be in List's order.
func NewWaitContext(dataDir string, requests []*Request, now time.Time) *WaitContext {
	c := &WaitContext{DataDir: dataDir, Slots: 1}
	hb, err := daemonheartbeat.Read(daemonheartbeat.WorkerPath(dataDir))
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		c.Unknown = true
	default:
		c.LastSeen = hb.UpdatedAt
		if !daemonheartbeat.Stale(hb, now, daemonheartbeat.WorkerStaleAfter) {
			c.Live = true
			c.Holders = hb.SlotRequests
			c.Slots = max(hb.JobSlots, 1)
			c.RepositoryWaits = hb.RepositoryWaits
		}
	}
	return c.Number(requests)
}

// Number computes the queue order of requests (in List's order) from the
// context's holders and slots, and returns c.
func (c *WaitContext) Number(requests []*Request) *WaitContext {
	c.waitingOn = WaitingOn(requests, c.Holders, c.Slots)
	c.positions = QueuePositions(requests, c.Holders, c.Slots)
	return c
}

// For is why r is waiting, or nil.
func (c *WaitContext) For(r *Request) *Wait {
	f := WaitFacts{
		WorkerLive:          c.Live,
		WorkerUnknown:       c.Unknown,
		WorkerLastSeen:      c.LastSeen,
		WaitingOn:           c.waitingOn[r.ID],
		Position:            c.positions[r.ID],
		Slots:               c.Slots,
		RepositoryWaitSince: c.RepositoryWaits[r.ID],
	}
	if feed := r.FeedRunID(); feed != "" && r.State.RunsJob() {
		if sum, err := progress.Summary(progress.Path(c.DataDir, feed)); err == nil {
			f.RunWaitStage, f.RunWaitSince, f.RunWaitOn = sum.WaitingStage, sum.WaitingSince, sum.WaitingReason
		}
	}
	return WaitFor(r, f)
}

// FeedRunID is the id of the progress feed r's current job writes: the
// request's own id for a drafting or planning job, the current ticket's run
// for a build; "" when there is none yet.
func (r *Request) FeedRunID() string {
	switch r.State {
	case StateSpecDrafting, StateOracleDrafting, StatePlanning:
		return r.ID
	case StateBuilding:
		if r.TicketIndex >= 1 && r.TicketIndex <= len(r.Tickets) {
			return r.Tickets[r.TicketIndex-1].RunID
		}
	}
	return ""
}
