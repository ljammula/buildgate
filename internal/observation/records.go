package observation

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"buildgate/internal/request"
	"buildgate/internal/run"
	"buildgate/internal/sanitize"
)

const (
	// maxSentenceBytes bounds the factory's sentence about one check.
	maxSentenceBytes = 300
	// maxNameBytes bounds a cleaned name taken from a record: a check, a
	// file, a thread id, an anchor.
	maxNameBytes = 200
	// maxListed bounds a list of names taken from a record.
	maxListed = 50
)

// CheckNote is a check a quarantined run failed and what the factory says
// about it.
type CheckNote struct {
	Check string `json:"check"`
	// Sentence is the factory's own sentence about the failure, as the
	// handoff to a later build attempt carries it; absent when it has none.
	Sentence string `json:"sentence,omitempty"`
}

// computeID is the first 16 hex characters of the SHA-256 over the
// observation's kind, its run ids (the observation's own run, then the run
// that fixed it; the request id when it has none), its round indexes and
// its check name, each joined with a NUL byte, the ids and indexes
// separated by commas.
func computeID(o Observation) string {
	runs := []string{}
	for _, id := range []string{o.RunID, o.AcceptedRunID} {
		if id != "" {
			runs = append(runs, id)
		}
	}
	if len(runs) == 0 && o.RequestID != "" {
		runs = append(runs, o.RequestID)
	}
	rounds := o.Rounds
	if len(rounds) == 0 {
		rounds = o.idRounds
	}
	check := o.Check
	if check == "" {
		check = o.idCheck
	}
	sum := sha256.Sum256([]byte(strings.Join([]string{o.Kind, strings.Join(runs, ","), joinCommas(rounds), check}, "\x00")))
	return hex.EncodeToString(sum[:])[:16]
}

func joinCommas(ns []int) string {
	parts := make([]string, 0, len(ns))
	for _, n := range ns {
		parts = append(parts, fmt.Sprint(n))
	}
	return strings.Join(parts, ",")
}

// withIDs sets every observation's ID.
func withIDs(list []Observation) []Observation {
	for i := range list {
		list[i].ID = computeID(list[i])
	}
	return list
}

// name cleans one name taken from a record to a single line and a bound.
func name(s string) string {
	return capBytes(sanitize.Line(s), maxNameBytes)
}

func capBytes(s string, n int) string {
	if len(s) > n {
		s = strings.ToValidUTF8(s[:n], "")
	}
	return s
}

func names(in []string) []string {
	var out []string
	for _, s := range in {
		if c := name(s); c != "" && len(out) < maxListed {
			out = append(out, c)
		}
	}
	return out
}

// requestObservations lists what the requests record: pull-request review
// rounds whose commit was pushed, and the operator's edits and send-backs at
// the spec and plan gates. Only identifiers, names and times are taken from
// a record: never a comment, an edit or a reason.
func requestObservations(requests []*request.Request) []Observation {
	var out []Observation
	for _, r := range requests {
		if r == nil {
			continue
		}
		for i := range r.Tickets {
			out = append(out, reviewRounds(r, &r.Tickets[i])...)
		}
		out = append(out, operatorActions(r)...)
	}
	return withIDs(out)
}

func reviewRounds(r *request.Request, t *request.Ticket) []Observation {
	var out []Observation
	for _, rnd := range t.Rounds {
		if rnd.Kind != "" || rnd.Outcome != request.RoundAccepted || !rnd.Pushed {
			continue
		}
		out = append(out, Observation{
			Kind: KindReviewCommentAccepted, Source: SourceRequest, RunID: name(rnd.RunID), RequestID: name(r.ID),
			TicketIndex: t.Index, At: rnd.At, Rounds: []int{rnd.Index}, ThreadIDs: names(rnd.ThreadIDs),
			What: fmt.Sprintf("Review round %d of ticket %d answered %d review thread(s) and its commit was pushed.", rnd.Index, t.Index, len(rnd.ThreadIDs)),
		})
	}
	return out
}

func gateStage(s request.State) bool {
	return s == request.StateSpecReview || s == request.StatePlanReview
}

func operatorActions(r *request.Request) []Observation {
	var out []Observation
	for i, e := range r.Edits {
		if !gateStage(e.FromState) {
			continue
		}
		out = append(out, Observation{
			Kind: KindOperatorEdit, Source: SourceRequest, RequestID: name(r.ID), At: e.At, Stage: string(e.FromState),
			Anchors: names([]string{e.Path}), idRounds: []int{i + 1}, idCheck: "edit",
			What: fmt.Sprintf("The operator edited %s at %s.", name(e.Path), e.FromState),
		})
	}
	for i, rej := range r.Rejections {
		stage := rej.Stage()
		if !gateStage(stage) {
			continue
		}
		out = append(out, Observation{
			Kind: KindOperatorEdit, Source: SourceRequest, RequestID: name(r.ID), At: rej.At, Stage: string(stage),
			Anchors: rejectionAnchors(rej), idRounds: []int{i + 1}, idCheck: "rejection",
			What: fmt.Sprintf("The operator sent the draft back at %s.", stage),
		})
	}
	return out
}

// rejectionAnchors names the places a rejection's notes were tied to: the
// file and heading, and the item number, never the note.
func rejectionAnchors(rej request.Rejection) []string {
	var raw []string
	for _, a := range rej.Anchors {
		label := a.Path
		if a.Section != "" {
			label += " " + a.Section
		}
		if a.Item > 0 {
			label += fmt.Sprintf(" item %d", a.Item)
		}
		raw = append(raw, label)
	}
	return names(raw)
}

// fixedChecks lists, for each accepted run that followed a quarantined run
// of the same ticket, the checks the earlier run failed. A run follows
// another when it was given its record (run.Run.EarlierAttemptOf), or, for a
// corrective round of a request that recorded none, when it is the accepted
// round's run built on the branch of the request's earlier quarantined run.
// The observation's source is the request when the request's rounds name
// the accepted run, and the run record otherwise.
func fixedChecks(finished []*run.Run, requests []*request.Request, sentences func(*run.Run) map[string]string) []Observation {
	byID := map[string]*run.Run{}
	for _, r := range finished {
		byID[r.ID] = r
	}
	rounds := acceptedRoundRuns(requests)
	var out []Observation
	for _, accepted := range finished {
		if accepted.State != run.StateAccepted {
			continue
		}
		earlier := earlierQuarantined(accepted, byID, finished, rounds[accepted.ID])
		if earlier == nil {
			continue
		}
		source := SourceRun
		if rounds[accepted.ID] {
			source = SourceRequest
		}
		if o, ok := checkFixed(earlier, accepted, source, sentences); ok {
			out = append(out, o)
		}
	}
	return withIDs(out)
}

// acceptedRoundRuns is the set of run ids of the accepted pre-pull-request
// corrective rounds the requests recorded.
func acceptedRoundRuns(requests []*request.Request) map[string]bool {
	out := map[string]bool{}
	for _, r := range requests {
		if r == nil {
			continue
		}
		for _, t := range r.Tickets {
			for _, rnd := range t.Rounds {
				if rnd.Outcome == request.RoundAccepted && (rnd.Kind == request.CorrectiveRoundKind || rnd.Kind == request.ConformityRoundKind) {
					out[rnd.RunID] = true
				}
			}
		}
	}
	return out
}

// earlierQuarantined is the quarantined run of the same ticket that
// accepted follows, or nil.
func earlierQuarantined(accepted *run.Run, byID map[string]*run.Run, finished []*run.Run, isRound bool) *run.Run {
	var q *run.Run
	switch {
	case accepted.EarlierAttemptOf != "":
		q = byID[accepted.EarlierAttemptOf]
	case isRound && accepted.RequestID != "" && accepted.Branch != "":
		q = latestOnBranch(accepted, finished)
	}
	if q == nil || q.ID == accepted.ID || q.State != run.StateQuarantined {
		return nil
	}
	if q.Ticket != "" && accepted.Ticket != "" && q.Ticket != accepted.Ticket {
		return nil
	}
	return q
}

// latestOnBranch is the request's latest quarantined run on accepted's
// branch that was last updated before accepted was.
func latestOnBranch(accepted *run.Run, finished []*run.Run) *run.Run {
	var best *run.Run
	for _, r := range finished {
		if r.State != run.StateQuarantined || r.RequestID != accepted.RequestID || r.Branch != accepted.Branch || !moment(r.UpdatedAt).Before(moment(accepted.UpdatedAt)) {
			continue
		}
		if best == nil || moment(r.UpdatedAt).After(moment(best.UpdatedAt)) {
			best = r
		}
	}
	return best
}

func checkFixed(earlier, accepted *run.Run, source string, sentences func(*run.Run) map[string]string) (Observation, bool) {
	var said map[string]string
	if sentences != nil {
		said = sentences(earlier)
	}
	var notes []CheckNote
	var failed []string
	seen := map[string]bool{}
	for _, g := range earlier.GateResults {
		if g.Passed || g.Check == "" || seen[g.Check] {
			continue
		}
		seen[g.Check] = true
		notes = append(notes, CheckNote{Check: name(g.Check), Sentence: capBytes(sanitize.Line(said[g.Check]), maxSentenceBytes)})
		failed = append(failed, name(g.Check))
	}
	if len(notes) == 0 {
		return Observation{}, false
	}
	o := Observation{
		Kind: KindCheckFixed, Source: source, RunID: earlier.ID, AcceptedRunID: accepted.ID, Ticket: accepted.Ticket,
		At: accepted.UpdatedAt, Checks: notes, ChangedFiles: names(fixingFiles(accepted)), idCheck: strings.Join(failed, ","),
		What: fmt.Sprintf("Run %s was quarantined on %s; run %s of the same ticket was accepted.", earlier.ID, strings.Join(failed, ", "), accepted.ID),
	}
	if len(failed) > maxBlockersNamed {
		o.What = fmt.Sprintf("Run %s was quarantined on %s and %d more; run %s of the same ticket was accepted.", earlier.ID, strings.Join(failed[:maxBlockersNamed], ", "), len(failed)-maxBlockersNamed, accepted.ID)
	}
	return o, true
}

// fixingFiles are the files the accepted run's last round changed: the
// files it changed to get past what the earlier run failed on. Nil when the
// record holds no round.
func fixingFiles(accepted *run.Run) []string {
	if accepted.AgentEvidence == nil || len(accepted.AgentEvidence.Rounds) == 0 {
		return nil
	}
	rounds := accepted.AgentEvidence.Rounds
	return rounds[len(rounds)-1].ChangedFiles
}
