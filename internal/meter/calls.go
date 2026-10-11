package meter

import "time"

// Calls counts how one sandbox's model requests ended, read from its ledger.
// Every figure comes from a record the meter wrote, none from anything the
// worker printed.
//
// That does not make every figure proof of an outage. The worker chooses its
// requests: it can drop a connection before the response head (Unanswered),
// send a body the upstream rejects (Rejected, on some routes Failed), or
// spend its way to a refusal (the Denied counts). RateLimited and RetryAt
// come from the pinned upstream's own response and cannot be made up, though
// a burst can provoke a real one. A decision that treats these counts as
// "the provider did not serve this step" must therefore never accept or
// reject work on them: it may only hand the step to a human.
type Calls struct {
	// Answered: a success response arrived and its usage was read.
	Answered int `json:"answered,omitempty"`
	// AnsweredUnread: a success response head arrived and its usage could
	// not be read (a stream cut after the head, a body the meter refused
	// to pass). The worker may have received nothing.
	AnsweredUnread int `json:"answered_unread,omitempty"`
	// RateLimited: the upstream answered 429.
	RateLimited int `json:"rate_limited,omitempty"`
	// Rejected: the upstream answered another 4xx.
	Rejected int `json:"rejected,omitempty"`
	// Failed: the upstream answered anything else that is not a success.
	Failed int `json:"failed,omitempty"`
	// Unanswered: no response head reached the meter: the request is still
	// unsettled, or its reservation expired.
	Unanswered int `json:"unanswered,omitempty"`
	// DeniedCeiling, DeniedBudget and DeniedRate count the requests the
	// meter itself refused: at a run ceiling, at a window budget, at the
	// request rate.
	DeniedCeiling int `json:"denied_ceiling,omitempty"`
	DeniedBudget  int `json:"denied_budget,omitempty"`
	DeniedRate    int `json:"denied_rate,omitempty"`
	// RetryAt is the latest time a refused response asked the client to
	// wait until (its arrival plus its Retry-After), RFC 3339 UTC; "" when
	// none carried one.
	RetryAt string `json:"retry_at,omitempty"`
	// RefusedAtEnd counts the requests refused since the last answered one
	// (by the upstream with 429 or a failure status, or by the meter): the
	// route stopped serving the step and did not start again. Requests that
	// simply got no answer, or that the upstream rejected as malformed, are
	// neither counted nor end the count: a client that cancels its last
	// request on the way out leaves the same trace. RefusedAtEndBy is the
	// kind of the last of them (RefusedBy*).
	RefusedAtEnd   int    `json:"refused_at_end,omitempty"`
	RefusedAtEndBy string `json:"refused_at_end_by,omitempty"`
	// Legacy is true when the ledger holds a complete record from before
	// the meter recorded how a request ended. Its counts are then inferred
	// (SummarizeCalls) and say less: a provider's 429 reads as Failed, an
	// expired reservation as Answered, and the meter's budget and rate
	// refusals are absent. A decision must not rest on a Legacy summary.
	Legacy bool `json:"legacy,omitempty"`
}

// Who refused a request, for RefusedAtEndBy.
const (
	RefusedByProviderLimit = "provider_limit"
	RefusedByProviderError = "provider_error"
	RefusedByMeter         = "meter"
)

// Served reports whether any request got an answer whose usage was read: a
// success head alone (AnsweredUnread) may have delivered nothing.
func (c Calls) Served() bool {
	return c.Answered > 0
}

// Recorded is every request the ledger holds a record of. A request the
// meter refused before it had a ledger entry to write to (a duplicate or
// invalid id, a changed policy, a wrong method, an oversized body) is not
// among them.
func (c Calls) Recorded() int {
	return c.Answered + c.AnsweredUnread + c.RateLimited + c.Rejected + c.Failed + c.Unanswered + c.Denied()
}

// Denied is every request the meter itself refused.
func (c Calls) Denied() int {
	return c.DeniedCeiling + c.DeniedBudget + c.DeniedRate
}

// SummarizeCalls counts records' requests by how each ended.
//
// A complete record written before Settled existed says only that the
// request ended: it counts as Answered when a usage record was written since
// the previous event record (the meter writes the two under one lock), as
// Failed otherwise, and marks the summary Legacy.
func SummarizeCalls(records []LedgerRecord) Calls {
	var calls Calls
	// open counts, per request id, admits not yet matched by a complete: an
	// id may be admitted again once its first request has settled.
	open := map[string]int{}
	usageSinceEvent := false
	for _, rec := range records {
		switch rec.Kind {
		case "":
			usageSinceEvent = true
			continue
		case LedgerKindAdmit:
			open[rec.RequestID]++
		case LedgerKindComplete:
			if open[rec.RequestID] > 0 {
				open[rec.RequestID]--
			}
			calls.countComplete(rec, usageSinceEvent)
		case LedgerKindCeilingExceeded:
			calls.DeniedCeiling++
			calls.refusedAtEnd(RefusedByMeter)
		case LedgerKindBudgetExceeded:
			calls.DeniedBudget++
			calls.refusedAtEnd(RefusedByMeter)
		case LedgerKindRateLimited:
			calls.DeniedRate++
			calls.refusedAtEnd(RefusedByMeter)
		}
		usageSinceEvent = false
	}
	for _, n := range open {
		calls.Unanswered += n
	}
	return calls
}

func (c *Calls) countComplete(rec LedgerRecord, usageBefore bool) {
	c.noteRetryAt(rec)
	switch {
	case rec.Settled == "":
		c.Legacy = true
		if usageBefore {
			c.Answered++
		} else {
			c.Failed++
		}
	case rec.Status == 0:
		c.Unanswered++
	case rec.Status >= 200 && rec.Status < 300 && rec.Settled == SettledExact:
		c.Answered++
		c.RefusedAtEnd, c.RefusedAtEndBy = 0, ""
	case rec.Status >= 200 && rec.Status < 300:
		c.AnsweredUnread++
	case rec.Status == 429:
		c.RateLimited++
		c.refusedAtEnd(RefusedByProviderLimit)
	case rec.Status >= 400 && rec.Status < 500:
		c.Rejected++
	default:
		c.Failed++
		c.refusedAtEnd(RefusedByProviderError)
	}
}

func (c *Calls) refusedAtEnd(by string) {
	c.RefusedAtEnd++
	c.RefusedAtEndBy = by
}

// noteRetryAt keeps the latest "wait until" of the refused responses: the
// record's own time plus its Retry-After.
func (c *Calls) noteRetryAt(rec LedgerRecord) {
	if rec.RetryAfterSeconds <= 0 {
		return
	}
	arrived, err := time.Parse(time.RFC3339Nano, rec.TS)
	if err != nil {
		return
	}
	at := arrived.Add(time.Duration(rec.RetryAfterSeconds) * time.Second).UTC().Format(time.RFC3339)
	if at > c.RetryAt {
		c.RetryAt = at
	}
}
