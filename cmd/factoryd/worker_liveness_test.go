package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"buildgate/internal/daemonheartbeat"
	"buildgate/internal/meter"
	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
	"buildgate/internal/sessionconfig"
)

// TestResolveHITLReminderIntervalPrefersConfigPathOverDecoyDefault is the
// regression test for an adversarial-review finding: `serve` now
// documents -config as governing every session-config-derived value on
// that command (a separate, related finding), but
// resolveHITLReminderInterval still always searched the default config
// path regardless -- so a second config.yml sitting there could
// silently set the stale-request reminder cadence for a `serve -config
// X` invocation that named a completely different file.
func TestResolveHITLReminderIntervalPrefersConfigPathOverDecoyDefault(t *testing.T) {
	decoyPath := isolateSessionConfig(t)
	if err := os.MkdirAll(filepath.Dir(decoyPath), 0o750); err != nil {
		t.Fatalf("create decoy config dir: %v", err)
	}
	writeSessionConfig(t, decoyPath, "hitl_reminder_interval: 1m\n")

	namedPath := filepath.Join(t.TempDir(), "named-config.yml")
	writeSessionConfig(t, namedPath, "hitl_reminder_interval: 2m\n")

	if got := resolveHITLReminderInterval(namedPath); got != 2*time.Minute {
		t.Errorf("resolveHITLReminderInterval(namedPath) = %s, want the -config-named config's 2m (got the default-path decoy instead)", got)
	}
	if got := resolveHITLReminderInterval(""); got != time.Minute {
		t.Errorf("resolveHITLReminderInterval(\"\") = %s, want the default-path decoy's 1m", got)
	}
}

// TestStartWorkerHeartbeatWritesAndRefreshesHeartbeat proves
// startWorkerHeartbeat writes a worker heartbeat immediately and that
// `factoryd status`'s own reader sees it as fresh while it is running.
func TestStartWorkerHeartbeatWritesAndRefreshesHeartbeat(t *testing.T) {
	dataDir := t.TempDir()
	stop, err := startWorkerHeartbeat(context.Background(), dataDir, "", "", "", 1)
	if err != nil {
		t.Fatalf("startWorkerHeartbeat: %v", err)
	}
	t.Cleanup(stop)

	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(workerHeartbeatPath(dataDir)); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("startWorkerHeartbeat never wrote its heartbeat file")
		}
		time.Sleep(10 * time.Millisecond)
	}

	if line := workerHeartbeatStatusLine(dataDir, time.Now()); line != "" {
		t.Errorf("workerHeartbeatStatusLine while the heartbeat runs = %q, want \"\" (fresh)", line)
	}
}

// TestWorkerHeartbeatStatusLineReportsStaleness proves the staleness
// message format and that a fresh or absent heartbeat produce no line.
func TestWorkerHeartbeatStatusLineReportsStaleness(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	now := time.Now()

	if line := workerHeartbeatStatusLine(dataDir, now); line != "" {
		t.Errorf("no heartbeat file: line = %q, want \"\"", line)
	}

	fresh := daemonheartbeat.Heartbeat{UpdatedAt: now.Format(time.RFC3339Nano)}
	if err := daemonheartbeat.Write(workerHeartbeatPath(dataDir), fresh); err != nil {
		t.Fatal(err)
	}
	if line := workerHeartbeatStatusLine(dataDir, now); line != "" {
		t.Errorf("fresh heartbeat: line = %q, want \"\"", line)
	}

	stale := daemonheartbeat.Heartbeat{UpdatedAt: now.Add(-time.Hour).Format(time.RFC3339Nano)}
	if err := daemonheartbeat.Write(workerHeartbeatPath(dataDir), stale); err != nil {
		t.Fatal(err)
	}
	line := workerHeartbeatStatusLine(dataDir, now)
	if line == "" {
		t.Fatal("stale heartbeat: line = \"\", want a non-empty staleness message")
	}
	const want = "worker not running (last heartbeat "
	if len(line) < len(want) || line[:len(want)] != want {
		t.Errorf("line = %q, want prefix %q", line, want)
	}
}

// TestHeartbeatCarriesTheResolvedRoute is route-visibility's own
// regression test (2026-09-25): the heartbeat the worker writes must carry
// roles.execution's own resolved route, not an empty/default value --
// `factoryd status`'s route line is only as trustworthy as what worker
// actually wrote here.
func TestHeartbeatCarriesTheResolvedRoute(t *testing.T) {
	dataDir := t.TempDir()

	settings := sessionconfig.DefaultSettings()
	settings.Routes = map[string]sessionconfig.Route{"codex": {CredentialMode: meter.CredentialModeChatGPTCodex}}
	settings.Models = map[string]sessionconfig.Model{"luna": {ID: "gpt-5.6-luna", Routes: []string{"codex"}}}
	settings.Roles = &sessionconfig.Roles{Execution: &sessionconfig.RoleConfig{Model: "luna"}}
	cfg := requestdriver.WorkerConfig{Settings: settings}
	mode, model := heartbeatRoute(cfg)
	stop, err := startWorkerHeartbeat(context.Background(), dataDir, mode, model, "", 1)
	if err != nil {
		t.Fatalf("startWorkerHeartbeat: %v", err)
	}
	t.Cleanup(stop)

	deadline := time.Now().Add(3 * time.Second)
	var hb daemonheartbeat.Heartbeat
	for {
		var err error
		hb, err = daemonheartbeat.Read(workerHeartbeatPath(dataDir))
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("startWorkerHeartbeat never wrote its heartbeat file")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if hb.RouteCredentialMode != "chatgpt-codex" {
		t.Errorf("heartbeat RouteCredentialMode = %q, want %q", hb.RouteCredentialMode, "chatgpt-codex")
	}
	if hb.RouteWorkerModel != "gpt-5.6-luna" {
		t.Errorf("heartbeat RouteWorkerModel = %q, want %q", hb.RouteWorkerModel, "gpt-5.6-luna")
	}
}

// TestWorkerRouteStatusLine covers the format `factoryd status` prints
// while a route-carrying heartbeat is fresh, that a stale heartbeat (or
// none at all) produces no line, and that untrusted heartbeat content
// (a newline injected into the recorded model id) is flattened to one
// line -- the same sanitize.Line discipline already applied to a
// request's own Reason/Error text.
func TestWorkerRouteStatusLine(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	now := time.Now()

	if line := workerRouteStatusLine(dataDir, now); line != "" {
		t.Errorf("no heartbeat file: line = %q, want \"\"", line)
	}

	fresh := daemonheartbeat.Heartbeat{
		UpdatedAt:           now.Format(time.RFC3339Nano),
		RouteCredentialMode: "chatgpt-codex",
		RouteWorkerModel:    "gpt-5.6-luna",
	}
	if err := daemonheartbeat.Write(workerHeartbeatPath(dataDir), fresh); err != nil {
		t.Fatal(err)
	}
	const want = "worker route: chatgpt-codex · gpt-5.6-luna"
	if line := workerRouteStatusLine(dataDir, now); line != want {
		t.Errorf("fresh heartbeat: line = %q, want %q", line, want)
	}

	modeOnly := daemonheartbeat.Heartbeat{UpdatedAt: now.Format(time.RFC3339Nano), RouteCredentialMode: "static"}
	if err := daemonheartbeat.Write(workerHeartbeatPath(dataDir), modeOnly); err != nil {
		t.Fatal(err)
	}
	if line := workerRouteStatusLine(dataDir, now); line != "worker route: static" {
		t.Errorf("mode-only heartbeat: line = %q, want %q", line, "worker route: static")
	}

	stale := daemonheartbeat.Heartbeat{
		UpdatedAt:           now.Add(-time.Hour).Format(time.RFC3339Nano),
		RouteCredentialMode: "chatgpt-codex",
	}
	if err := daemonheartbeat.Write(workerHeartbeatPath(dataDir), stale); err != nil {
		t.Fatal(err)
	}
	if line := workerRouteStatusLine(dataDir, now); line != "" {
		t.Errorf("stale heartbeat: line = %q, want \"\"", line)
	}

	injected := daemonheartbeat.Heartbeat{
		UpdatedAt:           now.Format(time.RFC3339Nano),
		RouteCredentialMode: "chatgpt-codex",
		RouteWorkerModel:    "gpt-5.6-luna\nFAKE STATUS LINE",
	}
	if err := daemonheartbeat.Write(workerHeartbeatPath(dataDir), injected); err != nil {
		t.Fatal(err)
	}
	if line := workerRouteStatusLine(dataDir, now); strings.Contains(line, "\n") {
		t.Errorf("line with injected newline = %q, want it flattened to one line", line)
	}
}

// TestNotifyWorkerStaleFiresOnceForWaitingRequest proves
// notifyWorkerStale writes its marker on the first call (no second
// desktop notification attempt for the same request), and that a
// request in a human-review state (not worker-dependent) is ignored
// entirely regardless of how long it has waited.
func TestNotifyWorkerStaleFiresOnceForWaitingRequest(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	now := time.Now()
	oldUpdate := now.Add(-30 * time.Minute).Format(time.RFC3339Nano)

	waiting := &request.Request{ID: "req-waiting", State: request.StateBuilding, UpdatedAt: oldUpdate}
	reviewing := &request.Request{ID: "req-reviewing", State: request.StateSpecReview, UpdatedAt: oldUpdate}
	fresh := &request.Request{ID: "req-fresh", State: request.StateBuilding, UpdatedAt: now.Format(time.RFC3339Nano)}

	staleLine := "worker not running (last heartbeat 1h0m0s ago)"
	notifyWorkerStale(dataDir, []*request.Request{waiting, reviewing, fresh}, now, staleLine)

	if _, err := os.Stat(workerStaleNotifyMarkerPath(dataDir, "req-waiting")); err != nil {
		t.Errorf("req-waiting: marker not written: %v", err)
	}
	if _, err := os.Stat(workerStaleNotifyMarkerPath(dataDir, "req-reviewing")); err == nil {
		t.Error("req-reviewing: marker written for a human-review-state request, want none")
	}
	if _, err := os.Stat(workerStaleNotifyMarkerPath(dataDir, "req-fresh")); err == nil {
		t.Error("req-fresh: marker written for a request that hasn't waited long enough, want none")
	}

	// A second call with the marker already present must not touch it
	// again -- proven by removing write access to the marker's directory
	// content indirectly is fragile, so instead assert idempotency the
	// direct way: capture the marker's mtime and confirm a second call
	// doesn't change it.
	info1, err := os.Stat(workerStaleNotifyMarkerPath(dataDir, "req-waiting"))
	if err != nil {
		t.Fatal(err)
	}
	notifyWorkerStale(dataDir, []*request.Request{waiting}, now.Add(time.Minute), staleLine)
	info2, err := os.Stat(workerStaleNotifyMarkerPath(dataDir, "req-waiting"))
	if err != nil {
		t.Fatal(err)
	}
	if !info1.ModTime().Equal(info2.ModTime()) {
		t.Error("notifyWorkerStale re-wrote the marker on a second call for the same request")
	}
}

// TestNotifyWorkerStaleNoOpsWhenHeartbeatFresh proves an empty
// staleLine (worker is alive) skips every request, even one that has
// been in a worker-dependent state for a long time -- this
// notification exists to flag an absent worker, not a merely slow
// build.
func TestNotifyWorkerStaleNoOpsWhenHeartbeatFresh(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	now := time.Now()
	waiting := &request.Request{ID: "req-waiting", State: request.StateBuilding, UpdatedAt: now.Add(-time.Hour).Format(time.RFC3339Nano)}
	notifyWorkerStale(dataDir, []*request.Request{waiting}, now, "")
	if _, err := os.Stat(workerStaleNotifyMarkerPath(dataDir, "req-waiting")); err == nil {
		t.Error("marker written despite an empty (fresh-heartbeat) staleLine")
	}
}

// TestRunWorkerStaleWatcherLoopFiresOnTick is the regression test for
// this notification: `factoryd serve` must run the same stale-request
// check `factoryd status` already runs, on its own ticker, so an operator
// who walked away still gets notified. Drives runWorkerStaleWatcherLoop
// directly against a synthetic tick channel and an injected clock instead of a
// real ticker/time.Now, so the test is instant and deterministic. A
// request left in a worker-dependent state with a stale (heartbeat's
// three-missed-refreshes threshold) worker heartbeat must get its
// notification marker written after exactly one synthetic tick.
func TestRunWorkerStaleWatcherLoopFiresOnTick(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	now := time.Now()

	waiting := &request.Request{ID: "req-waiting", State: request.StateBuilding, UpdatedAt: now.Add(-time.Hour).Format(time.RFC3339Nano)}
	if err := waiting.Save(dataDir); err != nil {
		t.Fatalf("waiting.Save: %v", err)
	}
	// No worker heartbeat file at all -- workerHeartbeatStatusLine
	// (called inside runWorkerStaleCheck) reports "" for that case per
	// its own doc comment, so write a stale one instead, exactly like
	// TestWorkerHeartbeatStatusLineReportsStaleness does.
	stale := daemonheartbeat.Heartbeat{UpdatedAt: now.Add(-time.Hour).Format(time.RFC3339Nano)}
	if err := daemonheartbeat.Write(workerHeartbeatPath(dataDir), stale); err != nil {
		t.Fatal(err)
	}

	tick := make(chan time.Time, 1)
	done := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	loopDone := make(chan struct{})
	go func() {
		runWorkerStaleWatcherLoop(ctx, done, tick, dataDir, func() time.Time { return now })
		close(loopDone)
	}()

	tick <- now
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(workerStaleNotifyMarkerPath(dataDir, "req-waiting")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("runWorkerStaleWatcherLoop never wrote the notification marker after a tick")
		}
		time.Sleep(10 * time.Millisecond)
	}

	close(done)
	select {
	case <-loopDone:
	case <-time.After(3 * time.Second):
		t.Fatal("runWorkerStaleWatcherLoop did not stop after done was closed")
	}
}
