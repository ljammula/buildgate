package api

import "time"

// Options only the tests use.

// WithPollInterval changes how often SSE streams check the durable record.
func WithPollInterval(interval time.Duration) Option {
	return func(s *Server) {
		if interval > 0 {
			s.pollInterval = interval
		}
	}
}

// withStatsClock fixes the clock GET /stats reads, for a test that compares
// its response with a committed file: the route's buckets are counted back
// from the current day, so on the real clock that response changes daily.
func withStatsClock(now func() time.Time) Option {
	return func(s *Server) { s.statsNow = now }
}

// WithReviewGateExpiry sets a fixed review_gate_expiry: a decision on a gate
// that has waited this long is refused under the request's lock, which halts
// the request (gate_expired). 0, the default, is off.
func WithReviewGateExpiry(d time.Duration) Option {
	return WithReviewGateExpirySource(func() (time.Duration, error) { return d, nil })
}
