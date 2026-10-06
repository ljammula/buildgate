package notify

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"buildgate/internal/run"
)

func TestSlackNotifierPostsExpectedContent(t *testing.T) {
	var gotBody slackPayload
	var gotContentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotContentType = r.Header.Get("Content-Type")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := Notification{RunID: "run-1", Ticket: "ticket-1", Reason: "policy gate did not pass: canonical_verify", State: run.StateQuarantined}
	if err := (SlackNotifier{WebhookURL: srv.URL}).Notify(context.Background(), n); err != nil {
		t.Fatalf("Notify: %v", err)
	}

	if gotContentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotContentType)
	}
	for _, want := range []string{n.RunID, n.Ticket, string(n.State), n.Reason} {
		if !strings.Contains(gotBody.Text, want) {
			t.Errorf("text = %q, want it to contain %q", gotBody.Text, want)
		}
	}
}

// TestSlackNotifierEscapesChannelMention is the regression test for the
// mention-injection concern this notifier exists to close: unlike
// Discord, a plain Slack incoming webhook has no allowed_mentions-style
// field, so n.Reason and n.Ticket (CLI-supplied, potentially operator-
// or generator-authored) must have any leading `<!`/`<@` sequence
// neutralized before being interpolated, or a crafted value could page
// an entire channel.
func TestSlackNotifierEscapesChannelMention(t *testing.T) {
	var gotBody slackPayload
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := Notification{
		RunID:  "run-1",
		Ticket: "<!channel> urgent <@U0123456789>",
		Reason: "<!here> please look <!everyone>",
		State:  run.StateHalted,
	}
	if err := (SlackNotifier{WebhookURL: srv.URL}).Notify(context.Background(), n); err != nil {
		t.Fatalf("Notify: %v", err)
	}

	for _, mention := range []string{"<!channel>", "<!here>", "<!everyone>", "<@U0123456789>"} {
		if strings.Contains(gotBody.Text, mention) {
			t.Errorf("text = %q, must not contain live mention syntax %q", gotBody.Text, mention)
		}
	}
	if !strings.Contains(gotBody.Text, "&lt;!channel&gt;") && !strings.Contains(gotBody.Text, "&lt;!channel>") {
		t.Errorf("text = %q, want the neutralized channel mention still visible", gotBody.Text)
	}
}

// TestSlackNotifierAppendsHyperlinkedNext proves that when both Next and
// Link are set, Slack's own <url|text> syntax renders a hyperlinked
// "next" line rather than two separate plain-text lines.
func TestSlackNotifierAppendsHyperlinkedNext(t *testing.T) {
	var gotBody slackPayload
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := Notification{
		RunID:  "run-1",
		Reason: "run halted",
		Next:   "factoryd retry req-1",
		Link:   "https://console.example/requests/req-1",
	}
	if err := (SlackNotifier{WebhookURL: srv.URL}).Notify(context.Background(), n); err != nil {
		t.Fatalf("Notify: %v", err)
	}

	want := "next: <https://console.example/requests/req-1|factoryd retry req-1>"
	if !strings.Contains(gotBody.Text, want) {
		t.Errorf("text = %q, want it to contain %q", gotBody.Text, want)
	}
}

// TestSlackNotifierOmitsNextLineWhenAbsent proves an older-shaped
// notification (no Next/Link) renders exactly as before.
func TestSlackNotifierOmitsNextLineWhenAbsent(t *testing.T) {
	var gotBody slackPayload
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := Notification{RunID: "run-1", Reason: "run halted"}
	if err := (SlackNotifier{WebhookURL: srv.URL}).Notify(context.Background(), n); err != nil {
		t.Fatalf("Notify: %v", err)
	}

	if strings.Contains(gotBody.Text, "next:") {
		t.Errorf("text = %q, want no next: line when Next/Link are unset", gotBody.Text)
	}
}

func TestSlackNotifierNonSuccessStatusErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	err := (SlackNotifier{WebhookURL: srv.URL}).Notify(context.Background(), Notification{})
	if err == nil {
		t.Fatal("expected an error for a non-2xx webhook response")
	}
}

func TestSlackNotifierRespectsContextDeadline(t *testing.T) {
	// blockCh must be closed (unblocking the handler) before srv.Close()
	// runs, or Close blocks forever waiting for that in-flight handler —
	// deferred in this order so it runs first (defers are LIFO).
	blockCh := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-blockCh
	}))
	defer srv.Close()
	defer close(blockCh)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	err := (SlackNotifier{WebhookURL: srv.URL}).Notify(ctx, Notification{})
	if err == nil {
		t.Fatal("expected an error when the context deadline is exceeded before the webhook responds")
	}
}

func TestSlackNotifierUnreachableURLErrors(t *testing.T) {
	// A closed listener's port is guaranteed to refuse the connection
	// immediately (unlike port 0, which some platforms treat specially
	// and can hang indefinitely instead of failing fast).
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := (SlackNotifier{WebhookURL: "http://" + addr + "/not-a-real-endpoint"}).Notify(ctx, Notification{}); err == nil {
		t.Fatal("expected an error for an unreachable webhook URL")
	}
}

// TestSlackNotifierRedactsWebhookURLOnConnectionFailure mirrors the
// Discord regression test: a webhook POST that fails during
// DNS/TLS/connect/timeout returns a *url.Error whose string embeds the
// full request URL — which, for a real Slack webhook, contains its
// authentication token — and logging that error verbatim would write
// the live credential into factoryd's logs.
func TestSlackNotifierRedactsWebhookURLOnConnectionFailure(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()

	const secretToken = "super-secret-webhook-token"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = (SlackNotifier{WebhookURL: "http://" + addr + "/services/T000/B000/" + secretToken}).Notify(ctx, Notification{})
	if err == nil {
		t.Fatal("expected an error for an unreachable webhook URL")
	}
	if strings.Contains(err.Error(), secretToken) {
		t.Errorf("error = %q, want it to never contain the webhook token", err.Error())
	}
}

func TestSlackWebhookURLsParsesCommaSeparatedList(t *testing.T) {
	t.Setenv(SlackWebhookURLsEnvironmentVariable, " https://hooks.slack.com/a , https://hooks.slack.com/b ,,")
	got := SlackWebhookURLs()
	want := []string{"https://hooks.slack.com/a", "https://hooks.slack.com/b"}
	if len(got) != len(want) {
		t.Fatalf("SlackWebhookURLs() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("SlackWebhookURLs()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestSlackWebhookURLsEmptyWhenUnset(t *testing.T) {
	t.Setenv(SlackWebhookURLsEnvironmentVariable, "")
	if got := SlackWebhookURLs(); got != nil {
		t.Errorf("SlackWebhookURLs() = %v, want nil for an unset env var", got)
	}
}

// TestSlackNotifierNamesRequestForRequestScopedNotification proves a
// request-scoped notification's text names the request rather than an
// empty run header.
func TestSlackNotifierNamesRequestForRequestScopedNotification(t *testing.T) {
	var gotBody slackPayload
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := Notification{RequestID: "req-1", Reason: "waiting for you", State: run.State("spec_review")}
	if err := (SlackNotifier{WebhookURL: srv.URL}).Notify(context.Background(), n); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if !strings.HasPrefix(gotBody.Text, "factoryd: request req-1 -> spec_review\n") {
		t.Errorf("text = %q, want it to lead with the request header", gotBody.Text)
	}
}
