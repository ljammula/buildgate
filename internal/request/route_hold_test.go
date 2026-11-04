package request

import (
	"testing"
	"time"

	"buildgate/internal/unresolved"
)

// saveQuotaHolder saves a request waiting in resume_review on the provider's
// limit for routes, with retryAt as the provider's time.
func saveQuotaHolder(t *testing.T, dataDir, id string, retryAt string, routes ...string) *Request {
	t.Helper()
	r := New(id, "/repos/app", "app", Source{Kind: SourceText}, time.Now())
	r.State = StateBuilding
	if err := r.EnterResumeReview(StateBuilding, LostStep{Cause: unresolved.ProviderQuota, RetryAt: retryAt, Routes: routes}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	return r
}

// holderOf is RouteHolder, failing the test on an error.
func holderOf(t *testing.T, dataDir, route, resumedAt string, now time.Time) *Request {
	t.Helper()
	r, err := RouteHolder(dataDir, route, resumedAt, now)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// TestRouteHolderIsTheRequestWaitingOnTheProvidersLimit proves a route is held
// by a request waiting in resume_review on the provider's limit for it, until
// the meter's time passes, and that the hold ends when the request leaves
// resume_review; a time a worker printed ends nothing, and another route or
// another cause holds nothing.
func TestRouteHolderIsTheRequestWaitingOnTheProvidersLimit(t *testing.T) {
	dataDir := t.TempDir()
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	saveQuotaHolder(t, dataDir, "req-text", "3:11 AM by the worker container's clock", "codex")
	if got := holderOf(t, dataDir, "codex", "", now); got == nil || got.ID != "req-text" {
		t.Fatalf("RouteHolder = %v, want req-text: a printed time ends nothing", got)
	}
	if got := holderOf(t, dataDir, "local", "", now); got != nil {
		t.Fatalf("RouteHolder(local) = %s, want none: the refusal named codex", got.ID)
	}

	held := saveQuotaHolder(t, dataDir, "req-meter", now.Add(time.Hour).Format(time.RFC3339), "anthropic")
	if got := holderOf(t, dataDir, "anthropic", "", now); got == nil || got.ID != held.ID {
		t.Fatalf("RouteHolder before the meter's time = %v, want %s", got, held.ID)
	}
	if got := holderOf(t, dataDir, "anthropic", "", now.Add(2*time.Hour)); got != nil {
		t.Fatalf("RouteHolder after the meter's time = %s, want none", got.ID)
	}

	r, err := Load(dataDir, "req-text")
	if err != nil {
		t.Fatal(err)
	}
	if err := advance(r, []State{StateResumeReview}, StateCancelled, "", "", now); err != nil {
		t.Fatal(err)
	}
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	if got := holderOf(t, dataDir, "codex", "", now); got != nil {
		t.Fatalf("RouteHolder after the holder was cancelled = %s, want none", got.ID)
	}
}

// TestRouteHolderSkipsRefusalsAResumeAnswered proves a launch of a request the
// operator resumed after a quota refusal is not held by a refusal at or before
// that resume (the resume says the limit has reset), while a later refusal
// still holds it; and that a holder parked or refused a resume since holds
// nothing.
func TestRouteHolderSkipsRefusalsAResumeAnswered(t *testing.T) {
	dataDir := t.TempDir()
	now := time.Now()
	saveQuotaHolder(t, dataDir, "req-c", "", "codex")
	resumed := time.Now().UTC().Add(time.Second).Format(time.RFC3339Nano)
	if got := holderOf(t, dataDir, "codex", resumed, now); got != nil {
		t.Fatalf("holder for a launch resumed after the refusal = %s, want none", got.ID)
	}
	before := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano)
	if got := holderOf(t, dataDir, "codex", before, now); got == nil {
		t.Fatal("holder for a resume before the refusal = none, want req-c")
	}

	r, err := Load(dataDir, "req-c")
	if err != nil {
		t.Fatal(err)
	}
	r.Resume.Refused = []string{"a container of the lost run is still alive"}
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	if got := holderOf(t, dataDir, "codex", "", now); got != nil {
		t.Fatalf("holder after a refused resume = %s, want none", got.ID)
	}
}
