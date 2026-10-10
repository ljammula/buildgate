package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"buildgate/internal/notify"
	"buildgate/internal/run"
)

// A run that is a request's ticket halts: the halt is on the run's record
// and in its notifications.log, and nothing is sent to the operator for it.
// The request's own notification, with the request's page as its link, is
// the one they get. A run started on its own still sends its own.
func TestARequestsOwnRunDoesNotSendASecondNotification(t *testing.T) {
	var hits atomic.Int32
	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer webhook.Close()
	t.Setenv("FACTORYD_DISCORD_WEBHOOK_URLS", webhook.URL)

	dataDir := t.TempDir()
	owned := &run.Run{ID: "run-owned", Ticket: "req-1-001", RequestID: "req-1", State: run.StateHalted}
	if err := save(owned, dataDir); err != nil {
		t.Fatal(err)
	}
	notify.WaitForPendingDispatches(2 * time.Second)
	if got := hits.Load(); got != 0 {
		t.Errorf("a request's own run sent %d notification(s), want none", got)
	}
	logged, err := os.ReadFile(filepath.Join(run.Dir(dataDir, owned.ID), "notifications.log"))
	if err != nil || !strings.Contains(string(logged), `"state":"halted"`) || len(owned.Notifications) != 1 {
		t.Errorf("the halt is not on the run's own record: log %q (%v), %d on the run", logged, err, len(owned.Notifications))
	}

	alone := &run.Run{ID: "run-alone", Ticket: "fixture-ticket", State: run.StateHalted}
	if err := save(alone, dataDir); err != nil {
		t.Fatal(err)
	}
	notify.WaitForPendingDispatches(2 * time.Second)
	if got := hits.Load(); got != 1 {
		t.Errorf("a run started on its own sent %d notification(s), want 1", got)
	}
}

func TestDoctorSaysWhenANotificationClickWouldDoNothing(t *testing.T) {
	if c := doctorCheckNotificationClick(true); c.Err != nil {
		t.Errorf("with terminal-notifier: %v", c.Err)
	}
	c := doctorCheckNotificationClick(false)
	if c.Err == nil || !c.Advisory || !strings.Contains(c.Fix, "brew install terminal-notifier") {
		t.Errorf("without terminal-notifier: %+v, want an advisory naming the install", c)
	}
}
