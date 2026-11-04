package request

import (
	"fmt"
	"slices"
	"time"

	"buildgate/internal/unresolved"
)

// RouteHolder is the request whose provider-quota refusal holds route at now,
// nil when none does: a request waiting in resume_review with cause
// provider_quota whose Resume.Routes name route, unless the provider's time
// (the meter's RFC 3339 RetryAt) has passed, or the refusal came at or before
// resumedAt (RFC 3339: the asking request's own resume after such a refusal,
// which says the limit has reset; "" for none). A request parked or refused a
// resume since is not held by the refusal any more. The hold is derived from
// that record alone, so it ends by itself when the holder leaves resume_review
// (a resume, a cancel, a rebuild): nothing is stored that could outlive it. A
// RetryAt in any other form (the time of day a worker printed) ends nothing.
// When the requests cannot be listed nothing is held, and the error says why.
func RouteHolder(dataDir, route, resumedAt string, now time.Time) (*Request, error) {
	if route == "" {
		return nil, nil
	}
	all, err := List(dataDir)
	if err != nil {
		return nil, fmt.Errorf("route hold on %s: list requests: %w", route, err)
	}
	resumed, resumedErr := time.Parse(time.RFC3339Nano, resumedAt)
	for _, r := range all {
		if !holdsRoute(r, route) {
			continue
		}
		if at, err := time.Parse(time.RFC3339, r.Resume.RetryAt); err == nil && !now.Before(at) {
			continue
		}
		if refused, err := time.Parse(time.RFC3339Nano, r.Resume.At); resumedErr == nil && err == nil && !refused.After(resumed) {
			continue
		}
		return r, nil
	}
	return nil, nil
}

// holdsRoute reports whether r waits in resume_review on a provider-quota
// refusal of route, and on nothing since (a park or a refused resume).
func holdsRoute(r *Request, route string) bool {
	return r.State == StateResumeReview && r.Resume != nil && r.Resume.Cause == unresolved.ProviderQuota &&
		!r.Resume.Parked() && len(r.Resume.Refused) == 0 && slices.Contains(r.Resume.Routes, route)
}
