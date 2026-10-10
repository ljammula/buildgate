package notify

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"buildgate/internal/consolelink"
	"buildgate/internal/request"
	"buildgate/internal/run"
)

// consoleRunEnvVar mirrors cmd/factoryd/console_link.go's
// consoleLinkEnvVar (FACTORYD_CONSOLE_URL) -- duplicated, not imported,
// because cmd/factoryd is `package main` and nothing under internal/ may
// import it, yet PrepareHalt is a halt-notification construction site
// outside cmd/factoryd (internal/api's overrideRun calls it directly
// too).

// consoleRunLink returns the console's deep link to run runID, mirroring
// cmd/factoryd's consoleRunURL/resolveConsoleBaseURL -- duplicated here
// for the same layering reason as consoleRunEnvVar above. Empty when the
// env var is unset, same as that pair's own "" fallback.
func consoleRunLink(dataDir, runID string) string {
	return consolelink.RunURL(consolelink.BaseURL("", dataDir), runID)
}

// haltNext names the one command an operator should run to move a halted
// run forward: retrying the owning request when runID belongs to one
// (mirrors cmd/factoryd/watch.go's findOwningRequest -- duplicated here
// for the same layering reason as consoleRunEnvVar above), else watching
// the run directly.
func haltNext(dataDir, runID string) string {
	if requests, err := request.List(dataDir); err == nil {
		for _, r := range requests {
			for _, t := range r.Tickets {
				if t.RunID == runID {
					return fmt.Sprintf("factoryd retry %s", r.ID)
				}
			}
		}
	}
	return fmt.Sprintf("factoryd watch %s", runID)
}

// DiscordWebhookURLsEnvironmentVariable names the environment variable
// holding factoryd's optional Discord paging channel(s): a comma-separated
// list of incoming-webhook URLs, so the set can be swapped or extended
// without a code change. Unset or empty disables Discord notification
// entirely. An env var, not a flag, so the webhook URL(s) never appear in
// a process listing (e.g. `ps`).
const DiscordWebhookURLsEnvironmentVariable = "FACTORYD_DISCORD_WEBHOOK_URLS"

// DiscordWebhookURLs parses DiscordWebhookURLsEnvironmentVariable into its
// individual webhook URLs, trimming whitespace and dropping empty entries.
func DiscordWebhookURLs() []string {
	var urls []string
	for _, u := range strings.Split(os.Getenv(DiscordWebhookURLsEnvironmentVariable), ",") {
		if u = strings.TrimSpace(u); u != "" {
			urls = append(urls, u)
		}
	}
	return urls
}

// SlackWebhookURLsEnvironmentVariable names the environment variable
// holding factoryd's optional Slack paging channel(s): a comma-separated
// list of incoming-webhook URLs, mirroring
// DiscordWebhookURLsEnvironmentVariable. Unset or empty disables Slack
// notification entirely. An env var, not a flag, so the webhook URL(s)
// never appear in a process listing (e.g. `ps`).
const SlackWebhookURLsEnvironmentVariable = "FACTORYD_SLACK_WEBHOOK_URLS"

// SlackWebhookURLs parses SlackWebhookURLsEnvironmentVariable into its
// individual webhook URLs, trimming whitespace and dropping empty entries.
func SlackWebhookURLs() []string {
	var urls []string
	for _, u := range strings.Split(os.Getenv(SlackWebhookURLsEnvironmentVariable), ",") {
		if u = strings.TrimSpace(u); u != "" {
			urls = append(urls, u)
		}
	}
	return urls
}

// PrepareHalt is a run's halt alert: found via the 2026-09-05 Opus
// review, S2 -- cmd/factoryd/main.go assigns r.State = run.StateHalted at
// dozens of call sites (internal/workflow/workflow.go a handful more),
// but notify.Notifier had exactly four construction sites before this,
// all inside the accept-or-quarantine junction: under an unattended
// policy a halt is the dominant stop mode, and it was entirely silent.
//
// Shared by cmd/factoryd's own save() helper (which every one of those
// call sites already funnels through) and internal/api's overrideRun
// (found via a real GitHub Codex App review, round 3, of the PR that
// first added this: POST /runs/{id}/override moving a run to
// run.StateHalted called run.Run.Save directly, bypassing save()'s hook
// entirely, so an HTTP-initiated halt alerted nobody while an
// identical CLI- or workflow-initiated one did) -- internal/api cannot
// import cmd/factoryd (an import-cycle-shaped layering violation even
// setting cycles aside, since cmd/factoryd is a `package main`), so this
// lives in the one package both already depend on for exactly this kind
// of durable notification.
//
// Deliberately generic, not call-site-specific: the reason string points
// at this run's own durable record and logs rather than repeating
// whatever error string the caller already returned, since neither
// caller here has that context in a uniform shape. Guarded against
// renotifying on a second save of an already-halted run (e.g. a later
// reconciliation pass only flipping HaltConfirmed) by checking whether
// the most recent notification already covers this halt.
//
// Split from Discord dispatch (DispatchDiscord, below) deliberately: this
// only does local, fast work (building the notification, appending to
// r.Notifications, writing the durable per-run log) so a caller can
// persist r first and dispatch the network-bound alert only afterward
// (found via a real GitHub Codex App review round on the same original
// PR: an earlier version dispatched Discord before r was durably saved,
// reopening the exact crash window the accept-or-quarantine junctions
// elsewhere in this codebase deliberately avoid).
func PrepareHalt(r *run.Run, dataDir string) (Notification, bool) {
	if r.State != run.StateHalted {
		return Notification{}, false
	}
	if n := len(r.Notifications); n > 0 && r.Notifications[n-1].State == run.StateHalted {
		return Notification{}, false
	}

	reason := fmt.Sprintf("run halted — see %s and its build/verify logs for detail", run.Dir(dataDir, r.ID))
	// r.HaltError is set only when the caller had the actual causing error
	// in hand (see its own doc comment) -- included here, not just left in
	// the durable run record, because the exact failure mode this exists
	// for (a runner.Run infrastructure error) is also the one where the
	// build/verify logs this reason otherwise points to are empty, making
	// "see the logs" a dead end without it.
	switch {
	case r.Triage != "":
		// Factory-authored triage (see run.Run.Triage) is the one-sentence
		// "why" an operator wants first; the run dir stays for the receipts
		// and the exact causing error, when known, is still appended --
		// the triage category alone ("Docker mount/visibility failure")
		// would drop the path or image name an operator needs.
		reason = fmt.Sprintf("%s — see %s", r.Triage, run.Dir(dataDir, r.ID))
		if r.HaltError != "" {
			reason = fmt.Sprintf("%s (%s)", reason, r.HaltError)
		}
	case r.HaltError != "":
		reason = fmt.Sprintf("%s (%s)", reason, r.HaltError)
	}

	delivered := true
	n := Notification{
		RunID:     r.ID,
		Ticket:    r.Ticket,
		Reason:    reason,
		State:     run.StateHalted,
		SentAt:    time.Now().Format(time.RFC3339),
		Delivered: &delivered,
		RunDir:    run.AbsDir(dataDir, r.ID),
		Next:      haltNext(dataDir, r.ID),
		Link:      consoleRunLink(dataDir, r.ID),
	}
	// This runs before the caller's own durable save, which is what
	// actually creates run.Dir(dataDir, r.ID) in the ordinary case (every
	// run's very first save, at StateReady, creates it long before any
	// halt can occur) -- nothing here should depend on that ordering
	// holding at every one of this hook's call sites forever.
	if err := os.MkdirAll(run.Dir(dataDir, r.ID), 0o750); err != nil {
		return Notification{}, false
	}
	notifyCtx, cancelNotify := context.WithTimeout(context.Background(), 2*time.Second)
	notifyErr := (LogNotifier{Path: filepath.Join(run.Dir(dataDir, r.ID), "notifications.log")}).Notify(notifyCtx, n)
	cancelNotify()
	if notifyErr != nil {
		delivered = false
		n.DeliveryError = notifyErr.Error()
	}
	r.Notifications = append(r.Notifications, n)
	return n, true
}

// DispatchDiscord best-effort pages every configured Discord webhook with
// n. Always called only after the notification it carries is already
// durably persisted, never before -- see PrepareHalt's own doc comment.
func DispatchDiscord(n Notification) {
	for _, webhookURL := range DiscordWebhookURLs() {
		discordCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = (DiscordNotifier{WebhookURL: webhookURL}).Notify(discordCtx, n)
		cancel()
	}
}

// DispatchSlack best-effort pages every configured Slack webhook with n.
// Mirrors DispatchDiscord's fan-out shape; see PrepareHalt's own doc
// comment for why this must only ever be called after n is already
// durably persisted.
func DispatchSlack(n Notification) {
	for _, webhookURL := range SlackWebhookURLs() {
		slackCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = (SlackNotifier{WebhookURL: webhookURL}).Notify(slackCtx, n)
		cancel()
	}
}

// DispatchDesktop best-effort shows a local desktop notification for n.
// Mirrors DispatchDiscord's shape for symmetry, though there is only
// ever one "webhook" here: the local machine itself. DesktopNotifier.Notify
// already never errors, so there is nothing here to discard but the
// (always-nil) return value.
//
// A request's notification (one with an Ask) is not shown while a console
// tab on this machine raises dataDir's request notifications itself
// (consolelink.TabNotifierPresent): the operator gets one notification, the
// tab's, whose click brings that tab to the front. A run's own notification
// is always shown: no tab raises it.
func DispatchDesktop(dataDir string, n Notification) {
	if n.Ask != "" && consolelink.TabNotifierPresent(dataDir) {
		return
	}
	desktopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	_ = (DesktopNotifier{}).Notify(desktopCtx, n)
	cancel()
}

// pendingDispatches tracks DispatchExternal calls that have started but
// not yet finished. Found via Codex review round 2 on PR #88: a CLI
// process can exit (log.Fatalf, or a normal return out of main) before a
// bare `go notify.DispatchExternal(n)` goroutine ever reaches the
// network, silently dropping the alert. WaitForPendingDispatches gives a
// process a bounded flush point instead.
//
// A mutex-guarded count plus a "reached zero" channel, not a
// sync.WaitGroup: WaitForPendingDispatches must be able to give up on a
// timeout, and a WaitGroup.Wait cannot be abandoned -- the goroutine
// parked in it wakes later and touches the WaitGroup unsynchronized with
// the next Add, which the race detector reports (CI on main, 2026-09-10:
// TestWaitForPendingDispatchesReturnsOnTimeout's leaked waiter racing
// TestDispatchExternalRunsChannelsConcurrently's DispatchExternal).
// Selecting on a channel leaves nothing behind when the timeout wins.
var pendingDispatches = pendingCounter{zero: closedChan()}

type pendingCounter struct {
	mu    sync.Mutex
	count int
	// zero is closed when count last reached zero; replaced with a fresh
	// open channel when count leaves zero again.
	zero chan struct{}
}

func closedChan() chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}

func (p *pendingCounter) add() {
	p.mu.Lock()
	if p.count == 0 {
		p.zero = make(chan struct{})
	}
	p.count++
	p.mu.Unlock()
}

func (p *pendingCounter) done() {
	p.mu.Lock()
	p.count--
	if p.count == 0 {
		close(p.zero)
	}
	p.mu.Unlock()
}

// zeroReached returns a channel that is closed once every dispatch
// counted so far has finished -- already closed when none is pending.
func (p *pendingCounter) zeroReached() <-chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.zero
}

// DispatchExternal best-effort pages every configured out-of-band
// channel with n: Discord, Slack, and a local desktop notification, all
// three concurrently. The single call site every notification-
// dispatching caller should use in place of calling the individual
// Dispatch* functions directly. Runs the fan-out on its own goroutine and
// returns immediately, per Notifier's documented contract that network-
// I/O dispatch must be asynchronous from run completion; call
// WaitForPendingDispatches before process exit to give it a bounded
// chance to actually finish first.
//
// The three channels run concurrently, not one after another (found via
// Codex review round 3 on PR #88): each has its own 5s timeout, so
// running them serially made the worst case for the whole dispatch ~15s
// -- longer than WaitForPendingDispatches' own 5s exit-flush timeout, so
// a slow Discord webhook alone could starve Slack and desktop of their
// chance to fire before the flush gave up, defeating them as fallback
// channels entirely. Concurrent, the worst case is ~5s: the slowest
// single channel, not their sum.
func DispatchExternal(dataDir string, n Notification) {
	pendingDispatches.add()
	go func() {
		defer pendingDispatches.done()
		var channels sync.WaitGroup
		channels.Add(3)
		go func() { defer channels.Done(); DispatchDiscord(n) }()
		go func() { defer channels.Done(); DispatchSlack(n) }()
		go func() { defer channels.Done(); DispatchDesktop(dataDir, n) }()
		channels.Wait()
	}()
}

// WaitForPendingDispatches blocks until every DispatchExternal call made
// so far has finished, or until timeout elapses, whichever comes first --
// a genuinely hung webhook must still let the process exit eventually.
// Call exactly once, at a process's true top-level exit boundary, after
// every code path that might call DispatchExternal has already run (see
// cmd/factoryd/main.go's own call site).
func WaitForPendingDispatches(timeout time.Duration) {
	select {
	case <-pendingDispatches.zeroReached():
	case <-time.After(timeout):
	}
}
