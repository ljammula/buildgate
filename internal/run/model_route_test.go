package run

import (
	"buildgate/internal/meter"
	"buildgate/internal/unresolved"
	"testing"
)

func TestModelRouteCauseReadsOnlyTheMetersCounts(t *testing.T) {
	for _, tc := range []struct {
		name  string
		calls *meter.Calls
		want  unresolved.Cause
	}{
		{"an attempt that called no model", nil, ""},
		{"an empty ledger", &meter.Calls{}, ""},
		{"every request answered", &meter.Calls{Answered: 40}, ""},
		{"answered, with a rejected and an unanswered request on the way", &meter.Calls{Answered: 40, Rejected: 1, Unanswered: 1}, ""},
		{"a provider limit hit and then answered again", &meter.Calls{Answered: 40, RateLimited: 2}, ""},
		{"the provider's limit from some point on", &meter.Calls{Answered: 12, RateLimited: 3, RefusedAtEnd: 3, RefusedAtEndBy: meter.RefusedByProviderLimit}, unresolved.ProviderQuota},
		{"the meter's budget from some point on", &meter.Calls{Answered: 12, DeniedBudget: 5, RefusedAtEnd: 5, RefusedAtEndBy: meter.RefusedByMeter}, unresolved.MeterDenied},
		{"upstream errors from some point on", &meter.Calls{Answered: 12, Failed: 2, RefusedAtEnd: 2, RefusedAtEndBy: meter.RefusedByProviderError}, unresolved.ModelUnavailable},
		{"nothing ever answered: every request refused with 429", &meter.Calls{RateLimited: 4, RefusedAtEnd: 4, RefusedAtEndBy: meter.RefusedByProviderLimit}, unresolved.ProviderQuota},
		{"nothing ever answered: no response at all", &meter.Calls{Unanswered: 3}, unresolved.ModelUnavailable},
		{"nothing ever answered: only stream heads that were cut", &meter.Calls{AnsweredUnread: 2}, unresolved.ModelUnavailable},
		{"nothing ever answered: the meter refused, then silence", &meter.Calls{DeniedCeiling: 1, Unanswered: 1, RefusedAtEnd: 1, RefusedAtEndBy: meter.RefusedByMeter}, unresolved.MeterDenied},
		{"a ledger that cannot say how requests ended", &meter.Calls{Failed: 3, Legacy: true}, ""},
	} {
		if got := ModelRouteCause(tc.calls); got != tc.want {
			t.Errorf("%s: cause = %q, want %q", tc.name, got, tc.want)
		}
	}
}
