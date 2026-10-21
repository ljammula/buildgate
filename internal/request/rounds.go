package request

import (
	"fmt"
	"strings"
)

// RoundRefPrefix is the git namespace of the refs that keep each build round's
// commit reachable in the target repository's local .git. The refs are written
// by the host and are never pushed (safety-contract.md, Forge row).
const RoundRefPrefix = "refs/buildgate/rounds/"

// RoundTag kinds, the Kind of a RoundTag: the first build of a ticket, then an
// automatic corrective or conformity round, then a PR-review round.
const (
	RoundTagBuild      = "build"
	RoundTagCorrective = CorrectiveRoundKind
	RoundTagConformity = ConformityRoundKind
	RoundTagPRReview   = "pr_review"
)

// RoundTag is one entry of a ticket's round ledger: the commit a build round
// ended in, the ref that keeps it, and how the round's gates judged it.
type RoundTag struct {
	// N numbers the ticket's rounds from 1 in the order they were recorded.
	N int `json:"n"`
	// RunID is the round's run; Kind is one of the RoundTag constants.
	RunID string `json:"run_id"`
	Kind  string `json:"kind"`
	// SHA is the round's result commit; BaseSHA and DiffBaseSHA the commit it
	// started from and the base its diff is judged against.
	SHA         string `json:"sha"`
	BaseSHA     string `json:"base_sha,omitempty"`
	DiffBaseSHA string `json:"diff_base_sha,omitempty"`
	// Ref is RoundRefPrefix<request>/t<ticket>/r<N>.
	Ref string `json:"ref"`
	// Outcome is accepted, quarantined or halted; FailedChecks names the gates
	// that did not pass.
	Outcome      string   `json:"outcome"`
	FailedChecks []string `json:"failed_checks,omitempty"`
	At           string   `json:"at"`
}

// RoundRef is the ref of round n of ticket ticketIndex of request id.
func RoundRef(id string, ticketIndex, n int) string {
	return fmt.Sprintf("%s%s/t%d/r%d", RoundRefPrefix, id, ticketIndex, n)
}

// RoundTagFor completes tag for ticket ticketIndex's next ledger entry: its N
// and Ref. ok is false for a round that left nothing to name: no result
// commit, one equal to its starting commit, or a run already in the ledger.
// It changes nothing: the caller writes the ref, then AppendRoundTag.
func (r *Request) RoundTagFor(ticketIndex int, tag RoundTag) (RoundTag, bool) {
	if tag.SHA == "" || tag.SHA == tag.BaseSHA {
		return tag, false
	}
	for i := range r.Tickets {
		if r.Tickets[i].Index != ticketIndex {
			continue
		}
		for _, have := range r.Tickets[i].RoundTags {
			if have.RunID == tag.RunID {
				return tag, false
			}
		}
		tag.N = len(r.Tickets[i].RoundTags) + 1
		tag.Ref = RoundRef(r.ID, ticketIndex, tag.N)
		return tag, true
	}
	return tag, false
}

// AppendRoundTag records tag, as completed by RoundTagFor, on ticket ticketIndex.
func (r *Request) AppendRoundTag(ticketIndex int, tag RoundTag) {
	for i := range r.Tickets {
		if r.Tickets[i].Index == ticketIndex {
			r.Tickets[i].RoundTags = append(r.Tickets[i].RoundTags, tag)
			return
		}
	}
}

// RoundTagN is ticket's ledger entry for round n.
func (t *Ticket) RoundTagN(n int) (RoundTag, bool) {
	for _, tag := range t.RoundTags {
		if tag.N == n {
			return tag, true
		}
	}
	return RoundTag{}, false
}

// Pushed reports whether any of the ticket's commits has reached a remote: it
// has a pull request, or a round pushed its commit to the PR branch.
func (t *Ticket) Pushed() bool {
	if t.PRURL != "" {
		return true
	}
	for _, round := range t.Rounds {
		if round.Pushed {
			return true
		}
	}
	return false
}

// retryFromRoundCheck refuses a retry from round n unless the ticket it
// rebuilds has that round in its ledger and nothing of it has been pushed: a
// pushed branch could only be reset by a force-push, which factoryd never does.
func (r *Request) retryFromRoundCheck(n int) error {
	if r.TicketCount == 0 || r.TicketIndex < 1 || r.TicketIndex > len(r.Tickets) {
		return fmt.Errorf("request %s: no ticket is being built, so there is no round %d to retry from %w", r.ID, n, ErrIllegalTransition)
	}
	ticket := &r.Tickets[r.TicketIndex-1]
	if r.HaltKind == HaltAcceptedNoPR || r.HaltKind == HaltOracleMaterialize {
		return fmt.Errorf("request %s: a request halted as %q rebuilds nothing, so it cannot retry from round %d %w", r.ID, r.HaltKind, n, ErrIllegalTransition)
	}
	if ticket.Pushed() {
		return fmt.Errorf("request %s: ticket %d's branch has been pushed (pull request %s); retrying from round %d would need a force-push: close the pull request first %w", r.ID, ticket.Index, ticket.PRURL, n, ErrIllegalTransition)
	}
	if _, ok := ticket.RoundTagN(n); !ok {
		return fmt.Errorf("request %s: ticket %d has no round %d (rounds: %s; see `factoryd rounds %s`) %w", r.ID, ticket.Index, n, roundNumbers(ticket), r.ID, ErrIllegalTransition)
	}
	return nil
}

func roundNumbers(t *Ticket) string {
	if len(t.RoundTags) == 0 {
		return "none"
	}
	var ns []string
	for _, tag := range t.RoundTags {
		ns = append(ns, fmt.Sprint(tag.N))
	}
	return strings.Join(ns, ", ")
}
