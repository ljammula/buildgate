package notify

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"buildgate/internal/run"
)

func TestLogNotifierWritesValidJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notifications.log")
	want := Notification{
		RunID:  "run-1",
		Ticket: "ticket-1",
		Reason: "policy gate did not pass",
		State:  run.StateQuarantined,
		SentAt: "2026-08-25T12:00:00Z",
	}

	if err := (LogNotifier{Path: path}).Notify(context.Background(), want); err != nil {
		t.Fatalf("Notify: %v", err)
	}

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read notification log: %v", err)
	}
	var got Notification
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("notification log is not valid JSON: %v", err)
	}
	if got != want {
		t.Errorf("notification = %+v, want %+v", got, want)
	}
}

func TestLogNotifierAppends(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notifications.log")
	n := LogNotifier{Path: path}
	first := Notification{RunID: "run-1", State: run.StateQuarantined}
	second := Notification{RunID: "run-2", State: run.StateQuarantined}

	if err := n.Notify(context.Background(), first); err != nil {
		t.Fatalf("first Notify: %v", err)
	}
	if err := n.Notify(context.Background(), second); err != nil {
		t.Fatalf("second Notify: %v", err)
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open notification log: %v", err)
	}
	defer f.Close()

	var got []Notification
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var notification Notification
		if err := json.Unmarshal(scanner.Bytes(), &notification); err != nil {
			t.Fatalf("unmarshal notification line: %v", err)
		}
		got = append(got, notification)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan notification log: %v", err)
	}
	if len(got) != 2 || got[0] != first || got[1] != second {
		t.Errorf("notifications = %+v, want [%+v %+v]", got, first, second)
	}
}

// TestSubjectNamesRequestForRequestScopedNotification proves the header
// every channel leads with names the request when RequestID is set
// (RunID/Ticket empty, per run.NotificationRecord's contract) instead of
// rendering the run-only "run  () -> state" shape, and is unchanged for
// a run's own notification.
func TestSubjectNamesRequestForRequestScopedNotification(t *testing.T) {
	for _, tc := range []struct {
		name string
		n    Notification
		want string
	}{
		{"run", Notification{RunID: "run-1", Ticket: "ticket-1", State: run.StateHalted}, "run run-1 (ticket-1) -> halted"},
		{"request", Notification{RequestID: "req-1", State: run.State("awaiting_pr")}, "request req-1 -> awaiting_pr"},
	} {
		if got := subject(tc.n); got != tc.want {
			t.Errorf("%s: subject = %q, want %q", tc.name, got, tc.want)
		}
	}
}
