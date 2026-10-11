package run

import (
	"buildgate/internal/meter"
	"buildgate/internal/unresolved"
)

// ModelRouteCause is why a step's model route did not serve it, read from
// the counts the meter's ledger gave its attempt; "" when the route served
// the step to its end, when the attempt called no model, and when the ledger
// predates the record of how requests ended (meter.Calls.Legacy), which
// cannot say.
//
// The route did not serve the step when no request got a success response,
// or when the requests since the last answered one were refused by the
// provider or by the meter. Who refused names the cause: the meter
// (meter_denied), the provider's limit (provider_quota), anything else
// (model_unavailable).
//
// The counts are the meter's, but the requests are the worker's: a worker
// can provoke each of these. So this is only ever a reason to hand a step
// that did not pass to a human; it accepts nothing and rejects nothing.
func ModelRouteCause(calls *meter.Calls) unresolved.Cause {
	if calls == nil || calls.Legacy || calls.Recorded() == 0 {
		return ""
	}
	if calls.Served() && calls.RefusedAtEnd == 0 {
		return ""
	}
	by := calls.RefusedAtEndBy
	if !calls.Served() && by == "" {
		by = refusedBy(*calls)
	}
	switch by {
	case meter.RefusedByMeter:
		return unresolved.MeterDenied
	case meter.RefusedByProviderLimit:
		return unresolved.ProviderQuota
	}
	return unresolved.ModelUnavailable
}

// refusedBy is who refused a step none of whose requests was served and
// none refused at the end (they went unanswered or were rejected): the
// meter when it refused any, else the provider's limit when it answered
// 429, else nobody in particular.
func refusedBy(calls meter.Calls) string {
	switch {
	case calls.Denied() > 0:
		return meter.RefusedByMeter
	case calls.RateLimited > 0:
		return meter.RefusedByProviderLimit
	}
	return ""
}
