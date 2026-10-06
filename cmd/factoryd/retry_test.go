package main

import (
	"strings"
	"testing"
	"time"

	"buildgate/internal/request"
)

// saveRetryTestRequest saves a request in state under dataDir.
func saveRetryTestRequest(t *testing.T, dataDir, id string, state request.State) {
	t.Helper()
	r := request.New(id, "/repos/app", "app", request.Source{Kind: request.SourceText}, time.Now())
	r.State = state
	if state == request.StateHalted {
		r.Error = "sandbox unreachable"
	}
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
}

func TestRetryRefusesARequestThatIsNotRetryable(t *testing.T) {
	dp := newTestDeps(t)
	t.Parallel()
	dataDir := t.TempDir()
	saveRetryTestRequest(t, dataDir, "req-1", request.StateSpecReview)

	err := retryMain(dp, []string{"-data-dir", dataDir, "req-1"})
	if err == nil || !strings.Contains(err.Error(), string(request.StateSpecReview)) {
		t.Fatalf("retry err = %v, want refusal naming state %q", err, request.StateSpecReview)
	}
	got, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != request.StateSpecReview {
		t.Errorf("state = %q, want untouched %q", got.State, request.StateSpecReview)
	}
}

func TestRetryUnknownID(t *testing.T) {
	dp := newTestDeps(t)
	t.Parallel()
	err := retryMain(dp, []string{"-data-dir", t.TempDir(), "nope"})
	if err == nil || !strings.Contains(err.Error(), `no request "nope"`) {
		t.Fatalf("retry err = %v, want unknown-id error", err)
	}
}

// TestRetryRejectsPathShapedID: the id is joined under <data-dir>/requests,
// so a traversal-shaped one is refused before anything is read.
func TestRetryRejectsPathShapedID(t *testing.T) {
	dp := newTestDeps(t)
	t.Parallel()
	for _, id := range []string{"../evil", "a/b", ".."} {
		err := retryMain(dp, []string{"-data-dir", t.TempDir(), id})
		if err == nil || !strings.Contains(err.Error(), "single path component") {
			t.Errorf("retry %q: err = %v, want the single-path-component rejection", id, err)
		}
	}
}

// TestRetryMainHasNoHarnessOverride pins that retry no longer takes a
// -harness override: a request's harness choice is per role, validated at
// submit against roles.<role>.allowed_harnesses, and a stale choice fails at
// the job that resolves it -- there is nothing left for a retry-time override
// to recover. The retired flag is refused like any other unknown flag.
func TestRetryMainHasNoHarnessOverride(t *testing.T) {
	dp := newTestDeps(t)
	t.Parallel()
	err := retryMain(dp, []string{"-data-dir", t.TempDir(), "-harness", "pi", "req-1"})
	if err == nil || !strings.Contains(err.Error(), "flag provided but not defined: -harness") {
		t.Fatalf("retry -harness err = %v, want the unknown-flag refusal", err)
	}
}

// TestRetryMainReasonFlagReachesRequestHistory covers an adversarial-review
// finding (2026-09-24): `factoryd retry -reason "..." <id>`
// against a request id threads the flag
// through retryRequest/request.Retry into the appended History entry.
func TestRetryMainReasonFlagReachesRequestHistory(t *testing.T) {
	dp := newTestDeps(t)
	t.Parallel()
	dataDir := t.TempDir()
	r := request.New("req-1", "/repos/app", "app", request.Source{Kind: request.SourceText}, time.Now())
	r.State = request.StateHalted
	r.Error = "sandbox unreachable"
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}

	if err := retryMain(dp, []string{"-data-dir", dataDir, "-reason", "relay was flaky, trying again", "req-1"}); err != nil {
		t.Fatalf("retry: %v", err)
	}

	loaded, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	last := loaded.History[len(loaded.History)-1]
	if !strings.Contains(last.Reason, "relay was flaky, trying again") {
		t.Errorf("last History entry Reason = %q, want it to contain the -reason flag's text", last.Reason)
	}
}
