package requestdriver

import (
	"strings"
	"testing"
	"time"

	"buildgate/internal/request"
	"buildgate/internal/run"
)

// refDeps is a Deps that only writes refs; any other call panics on the nil
// embedded interface.
type refDeps struct {
	Deps
	refs map[string]string
}

func (d *refDeps) CreateRef(_ string, ref, sha string) error {
	d.refs[ref] = sha
	return nil
}

// TestTagTicketRoundTagsAReplayRunFromItsOnto: a replay run's BaseSHA is the
// replayed head it judges, which is also its result; the ledger entry starts
// from the commit the replay is onto, so the run gets a ref of its own at the
// next free round number.
func TestTagTicketRoundTagsAReplayRunFromItsOnto(t *testing.T) {
	head, onto := strings.Repeat("c", 40), strings.Repeat("a", 40)
	r := &request.Request{ID: "req-1", Workspace: "/repo", Tickets: []request.Ticket{{Index: 2, RoundTags: []request.RoundTag{{N: 1, RunID: "req-1-002"}}}}}
	dp := &refDeps{refs: map[string]string{}}
	replayRun := &run.Run{ID: "req-1-002-restack1", State: run.StateAccepted, BaseSHA: head, ResultSHA: head, DiffBaseSHA: onto, Replay: &run.Replay{OntoSHA: onto, HeadSHA: head}}
	tagTicketRound(dp, r, &r.Tickets[0], replayRun, request.RoundTagRestack, time.Now())
	tags := r.Tickets[0].RoundTags
	if len(tags) != 2 || tags[1].N != 2 || tags[1].Kind != request.RoundTagRestack || tags[1].BaseSHA != onto || dp.refs[request.RoundRef("req-1", 2, 2)] != head {
		t.Fatalf("tags = %+v refs = %v, want the replay run at r2, from %s, keeping %s", tags, dp.refs, onto, head)
	}
}
