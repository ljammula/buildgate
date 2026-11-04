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
		{"every request rejected as malformed: the client's doing", &meter.Calls{Rejected: 3}, ""},
		{"a ledger that cannot say how requests ended", &meter.Calls{Failed: 3, Legacy: true}, ""},
	} {
		if got := ModelRouteCause(tc.calls); got != tc.want {
			t.Errorf("%s: cause = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestQuotaRefusedRoutesAreTheLaunchesTheProviderRefused proves only the
// routes of launches the provider refused with its own limit are named, once
// each, as the walk records show such a launch (rate limited, refused at the
// end by the provider, no time).
func TestQuotaRefusedRoutesAreTheLaunchesTheProviderRefused(t *testing.T) {
	refused := &meter.Calls{RateLimited: 1, RefusedAtEnd: 1, RefusedAtEndBy: meter.RefusedByProviderLimit}
	r := &Run{Attempts: []Attempt{
		{RelayRoute: "codex", RelayCalls: &meter.Calls{Answered: 9}},
		{RelayRoute: "codex", RelayCalls: refused},
		{RelayRoute: "terra", RelayCalls: refused},
		{RelayRoute: "codex", RelayCalls: refused},
		{RelayRoute: "", RelayCalls: refused},
		{RelayRoute: "meter", RelayCalls: &meter.Calls{Answered: 1, DeniedBudget: 1, RefusedAtEnd: 1, RefusedAtEndBy: meter.RefusedByMeter}},
	}}
	got := QuotaRefusedRoutes(r)
	if len(got) != 2 || got[0] != "codex" || got[1] != "terra" {
		t.Fatalf("QuotaRefusedRoutes = %v, want [codex terra]", got)
	}
}
