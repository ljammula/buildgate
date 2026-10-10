package requestdriver

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"buildgate/internal/notify"
	"buildgate/internal/request"
)

func notifiedRequest(t *testing.T, dataDir string, state request.State) *request.Request {
	t.Helper()
	t.Setenv(notify.DesktopNotificationsEnvironmentVariable, "0")
	r := &request.Request{ID: "req-1", Project: "checkouts", State: state, TicketCount: 1}
	if err := os.MkdirAll(request.Dir(dataDir, r.ID), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(request.TextPath(dataDir, r.ID), []byte("Add a coupon field\n\nmore text\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	return r
}

func loggedNotifications(t *testing.T, dataDir string) []notify.Notification {
	t.Helper()
	f, err := os.Open(RequestNotificationLogPath(dataDir, "req-1"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []notify.Notification
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var n notify.Notification
		if err := json.Unmarshal(scanner.Bytes(), &n); err != nil {
			t.Fatal(err)
		}
		out = append(out, n)
	}
	return out
}

func TestAReviewNotificationSaysWhatIsAskedOfWhichRequest(t *testing.T) {
	for state, ask := range map[request.State]string{
		request.StateSpecReview:   "Spec ready for your review",
		request.StateOracleReview: "Acceptance tests ready for your review",
		request.StatePlanReview:   "Plan ready for your review",
		request.StateResumeReview: "A step was lost: choose how to continue",
	} {
		dataDir := t.TempDir()
		r := notifiedRequest(t, dataDir, state)
		now := time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC)
		RemindRequest(dataDir, r, now)

		logged := loggedNotifications(t, dataDir)
		if len(logged) != 1 {
			t.Fatalf("%s: %d notifications logged, want 1", state, len(logged))
		}
		n := logged[0]
		if n.Ask != ask || n.Subject != "checkouts: Add a coupon field" || n.RequestID != "req-1" {
			t.Errorf("%s: ask %q, subject %q, request %q", state, n.Ask, n.Subject, n.RequestID)
		}
		if strings.Contains(n.Reason, "req-1") && state != request.StateResumeReview {
			t.Errorf("%s: the sentence under the headline repeats the request id: %q", state, n.Reason)
		}
		if r.LastAsk != ask || r.LastNotifiedAt != "2026-10-10T09:00:00Z" || r.NotifyCount != 1 {
			t.Errorf("%s: request records ask %q at %q, count %d", state, r.LastAsk, r.LastNotifiedAt, r.NotifyCount)
		}
	}
}

func TestAReminderSaysHowLongTheRequestHasWaited(t *testing.T) {
	dataDir := t.TempDir()
	r := notifiedRequest(t, dataDir, request.StateSpecReview)
	start := time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC)
	RemindRequest(dataDir, r, start)
	RemindRequest(dataDir, r, start.Add(2*time.Hour))
	logged := loggedNotifications(t, dataDir)
	if len(logged) != 2 || !strings.HasPrefix(logged[1].Reason, "Waiting for you for 2h0m0s. ") || strings.Contains(logged[0].Reason, "Waiting") {
		t.Errorf("reasons = %q then %q", logged[0].Reason, logged[1].Reason)
	}
}

func TestAHaltAndAQuarantineEachNotifyOnceWithTheReason(t *testing.T) {
	for state, ask := range map[request.State]string{
		request.StateHalted:      "Halted: needs you",
		request.StateQuarantined: "Quarantined: needs you",
	} {
		dataDir := t.TempDir()
		r := notifiedRequest(t, dataDir, state)
		if err := notifyTerminalRequest(dataDir, r, "gate failed: canonical_verify", time.Now()); err != nil {
			t.Fatal(err)
		}
		logged := loggedNotifications(t, dataDir)
		if len(logged) != 1 || logged[0].Ask != ask || logged[0].Reason != "gate failed: canonical_verify" || logged[0].Next != "factoryd retry req-1" {
			t.Errorf("%s: logged %+v", state, logged)
		}
		saved, err := request.Load(dataDir, "req-1")
		if err != nil {
			t.Fatal(err)
		}
		if saved.LastAsk != ask || saved.LastNotifiedAt == "" {
			t.Errorf("%s: the saved request records ask %q at %q", state, saved.LastAsk, saved.LastNotifiedAt)
		}
	}
}

func TestAPullRequestNotifiesOncePerAskAndNeverReviewAfterMerge(t *testing.T) {
	dataDir := t.TempDir()
	r := notifiedRequest(t, dataDir, request.StatePRReview)
	ticket := &request.Ticket{Index: 1, PRURL: "https://github.com/acme/widgets/pull/7", PRState: "draft"}
	now := time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC)
	asks := func() []string {
		var out []string
		for _, n := range loggedNotifications(t, dataDir) {
			out = append(out, n.Ask)
		}
		return out
	}
	poll := func(prState string, ready bool, head string, blockers ...string) {
		ticket.PRState = prState
		ticket.MergeReadiness = &request.MergeReadiness{Ready: ready, HeadSHA: head, Blockers: blockers}
		notifyPullRequestWaiting(dataDir, r, ticket, now)
	}

	poll("draft", false, "aaa", "it is still a draft")
	now = now.Add(pullRequestDraftGrace - time.Second)
	poll("draft", false, "aaa", "it is still a draft")
	if got := asks(); len(got) != 0 {
		t.Fatalf("a draft pull request notified within the grace: %v", got)
	}
	poll("ready", false, "aaa", "its checks are pending or failing")
	poll("ready", false, "aaa", "its checks are pending or failing")
	if got := asks(); len(got) != 1 || got[0] != "Pull request ready for your review" {
		t.Fatalf("after two polls of a pull request ready for review: %v", got)
	}
	poll("ready", true, "aaa")
	poll("ready", true, "aaa")
	poll("approved", true, "aaa")
	if got := asks(); len(got) != 2 || got[1] != "Pull request ready to merge" {
		t.Fatalf("after polls of a pull request ready to merge: %v", got)
	}
	poll("ready", false, "aaa", "1 review thread(s) are open")
	if got := asks(); len(got) != 2 {
		t.Fatalf("a thread opened after \"ready to merge\" notified again: %v", got)
	}
	poll("ready", true, "bbb")
	if got := asks(); len(got) != 3 || got[2] != "Pull request ready to merge" {
		t.Fatalf("a new head ready to merge: %v", got)
	}
	last := loggedNotifications(t, dataDir)[2]
	if last.Next != ticket.PRURL || !strings.Contains(last.Reason, ticket.PRURL) || !strings.Contains(last.Reason, "never merges") {
		t.Errorf("ready-to-merge notification: next %q, reason %q", last.Next, last.Reason)
	}
	if r.LastAsk != "Pull request ready to merge" {
		t.Errorf("request records ask %q", r.LastAsk)
	}
}

func TestNoNotificationLinkCarriesACredential(t *testing.T) {
	dataDir := t.TempDir()
	r := notifiedRequest(t, dataDir, request.StateSpecReview)
	t.Setenv("FACTORYD_CONSOLE_URL", "http://127.0.0.1:1")
	t.Setenv("FACTORYD_API_START_TOKEN", "start-secret")
	t.Setenv("FACTORYD_API_OVERRIDE_TOKEN", "override-secret")
	RemindRequest(dataDir, r, time.Now())
	b, err := os.ReadFile(RequestNotificationLogPath(dataDir, "req-1"))
	if err != nil {
		t.Fatal(err)
	}
	n := loggedNotifications(t, dataDir)[0]
	if n.Link != "http://127.0.0.1:1/requests/req-1" {
		t.Errorf("link = %q, want the request's own page and nothing else", n.Link)
	}
	for _, secret := range []string{"start-secret", "override-secret", "#t=", "#gate=", "token"} {
		if strings.Contains(string(b), secret) {
			t.Errorf("the notification contains %q: %s", secret, b)
		}
	}
}

func TestNotificationTextIsOneLineOfPlainText(t *testing.T) {
	dataDir := t.TempDir()
	r := notifiedRequest(t, dataDir, request.StateHalted)
	if err := os.WriteFile(request.TextPath(dataDir, r.ID), []byte("Add \x1b[31ma coupon\x1b[0m field\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := notifyTerminalRequest(dataDir, r, "verify failed:\n\x1b]0;title\x07 exit 1", time.Now()); err != nil {
		t.Fatal(err)
	}
	n := loggedNotifications(t, dataDir)[0]
	if n.Subject != "checkouts: Add a coupon field" {
		t.Errorf("subject = %q", n.Subject)
	}
	if strings.ContainsAny(n.Reason, "\n\x1b\x07") || !strings.Contains(n.Reason, "verify failed:") || !strings.Contains(n.Reason, "exit 1") {
		t.Errorf("reason = %q, want one line with no control characters", n.Reason)
	}
}

// A pull request the factory never marks ready (failing checks, a thread
// that blocks the flip, a denied decision) must not wait in silence: no run
// of a request sends its own notification.
func TestADraftPullRequestThatStaysNotReadyNotifiesOnceAfterTheGrace(t *testing.T) {
	dataDir := t.TempDir()
	r := notifiedRequest(t, dataDir, request.StatePRReview)
	ticket := &request.Ticket{Index: 1, PRURL: "https://github.com/acme/widgets/pull/7", PRState: "draft"}
	start := time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC)
	poll := func(at time.Time, state, head string) {
		ticket.PRState = state
		ticket.MergeReadiness = &request.MergeReadiness{HeadSHA: head, Blockers: []string{"its checks are pending or failing"}}
		notifyPullRequestWaiting(dataDir, r, ticket, at)
	}
	poll(start, "draft", "aaa")
	poll(start.Add(pullRequestDraftGrace-time.Second), "draft", "aaa")
	if got := loggedNotifications(t, dataDir); len(got) != 0 {
		t.Fatalf("notified within the grace: %+v", got)
	}
	poll(start.Add(pullRequestDraftGrace), "draft", "aaa")
	poll(start.Add(2*pullRequestDraftGrace), "draft", "aaa")
	got := loggedNotifications(t, dataDir)
	if len(got) != 1 || got[0].Ask != "Pull request not ready: needs you" || !strings.Contains(got[0].Reason, "its checks are pending or failing") {
		t.Fatalf("after the grace: %+v", got)
	}
	// A new head starts the grace again; a stacked pull request never notifies.
	poll(start.Add(3*pullRequestDraftGrace), "draft", "bbb")
	poll(start.Add(9*pullRequestDraftGrace), "stacked", "ccc")
	if got := loggedNotifications(t, dataDir); len(got) != 1 {
		t.Fatalf("a new head, then a stacked pull request, notified at once: %+v", got)
	}
}
