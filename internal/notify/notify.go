// Package notify provides the factory's minimal out-of-band notification
// boundary. LogNotifier is an honest stand-in for a future Slack or email
// integration: it appends notifications to a durable local JSON-lines file,
// but does not itself page or message a human.
package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"buildgate/internal/run"
)

// Notifier accepts a notification for out-of-band delivery. Implementations
// must honor context cancellation and return after accepting the notification;
// they must never wait for a human response. A future implementation that does
// network I/O must be dispatched asynchronously from run completion.
type Notifier interface {
	Notify(ctx context.Context, n Notification) error
}

// Notification is the payload emitted when a run is quarantined. It is the
// same shape as run.NotificationRecord — the durable record a run stores is
// exactly what was sent, not a hand-copied approximation of it.
type Notification = run.NotificationRecord

// subject is the one-line "what changed" header every channel (desktop,
// Discord, Slack) leads with. A request-scoped notification sets
// RequestID instead of RunID/Ticket (see run.NotificationRecord), so it
// names the request -- a run-only header rendered those as
// "run  () -> awaiting_pr", hiding which request needed the operator.
func subject(n Notification) string {
	if n.RequestID != "" {
		return fmt.Sprintf("request %s -> %s", n.RequestID, n.State)
	}
	return fmt.Sprintf("run %s (%s) -> %s", n.RunID, n.Ticket, n.State)
}

// LogNotifier durably appends notifications as JSON lines at Path.
type LogNotifier struct {
	Path string
}

// Notify appends n to the notification log and syncs it to disk.
func (l LogNotifier) Notify(ctx context.Context, n Notification) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	// 0o600: this log records run/ticket/reason evidence for other local
	// users on the machine not to need read access to.
	f, err := os.OpenFile(l.Path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open notification log: %w", err)
	}
	defer f.Close()

	if err := json.NewEncoder(f).Encode(n); err != nil {
		return fmt.Errorf("append notification: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync notification log: %w", err)
	}
	return nil
}
