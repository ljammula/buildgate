package notify

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"buildgate/internal/run"
)

func TestDiscordNotifierPostsExpectedContent(t *testing.T) {
	var gotBody discordPayload
	var gotContentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotContentType = r.Header.Get("Content-Type")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	n := Notification{RunID: "run-1", Ticket: "ticket-1", Reason: "policy gate did not pass: canonical_verify", State: run.StateQuarantined}
	if err := (DiscordNotifier{WebhookURL: srv.URL}).Notify(context.Background(), n); err != nil {
		t.Fatalf("Notify: %v", err)
	}

	if gotContentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotContentType)
	}
	for _, want := range []string{n.RunID, n.Ticket, string(n.State), n.Reason} {
		if !strings.Contains(gotBody.Content, want) {
			t.Errorf("content = %q, want it to contain %q", gotBody.Content, want)
		}
	}
	if gotBody.AllowedMentions.Parse == nil || len(gotBody.AllowedMentions.Parse) != 0 {
		t.Errorf("allowed_mentions.parse = %v, want an explicit empty list so no mention type can be generated", gotBody.AllowedMentions.Parse)
	}
}

// TestDiscordNotifierDisablesMentionParsing is the regression test for a
// real finding from review: without an explicit allowed_mentions, Discord
// parses @everyone/@here/user/role mention syntax out of the message
// content — and n.Ticket, a CLI-supplied value not otherwise restricted
// from containing that syntax, is interpolated into Content unescaped.
// The raw JSON, not just the decoded struct, is checked here so a future
// change can't silently drop the field via an omitempty/zero-value quirk.
func TestDiscordNotifierDisablesMentionParsing(t *testing.T) {
	var rawBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		rawBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	n := Notification{Ticket: "@everyone urgent <@&123456789012345678>"}
	if err := (DiscordNotifier{WebhookURL: srv.URL}).Notify(context.Background(), n); err != nil {
		t.Fatalf("Notify: %v", err)
	}

	if !strings.Contains(string(rawBody), `"allowed_mentions":{"parse":[]}`) {
		t.Errorf("request body = %s, want an explicit allowed_mentions.parse: [] disabling all mention types", rawBody)
	}
}

// TestDiscordNotifierAppendsNextAndLink proves a notification carrying
// Next/Link gets a trailing "next: ..." line and the link as a bare URL
// (Discord auto-embeds a plain URL as a clickable link).
func TestDiscordNotifierAppendsNextAndLink(t *testing.T) {
	var gotBody discordPayload
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	n := Notification{
		RunID:  "run-1",
		Reason: "run halted",
		Next:   "factoryd retry req-1",
		Link:   "https://console.example/requests/req-1",
	}
	if err := (DiscordNotifier{WebhookURL: srv.URL}).Notify(context.Background(), n); err != nil {
		t.Fatalf("Notify: %v", err)
	}

	if !strings.Contains(gotBody.Content, "next: factoryd retry req-1") {
		t.Errorf("content = %q, want a next: line", gotBody.Content)
	}
	if !strings.Contains(gotBody.Content, n.Link) {
		t.Errorf("content = %q, want the link included", gotBody.Content)
	}
}

// TestDiscordNotifierOmitsNextAndLinkWhenAbsent proves an older-shaped
// notification (no Next/Link) renders exactly as before -- no stray
// "next:" line.
func TestDiscordNotifierOmitsNextAndLinkWhenAbsent(t *testing.T) {
	var gotBody discordPayload
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	n := Notification{RunID: "run-1", Reason: "run halted"}
	if err := (DiscordNotifier{WebhookURL: srv.URL}).Notify(context.Background(), n); err != nil {
		t.Fatalf("Notify: %v", err)
	}

	if strings.Contains(gotBody.Content, "next:") {
		t.Errorf("content = %q, want no next: line when Next/Link are unset", gotBody.Content)
	}
}

func TestDiscordNotifierNonSuccessStatusErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	err := (DiscordNotifier{WebhookURL: srv.URL}).Notify(context.Background(), Notification{})
	if err == nil {
		t.Fatal("expected an error for a non-2xx webhook response")
	}
}

func TestDiscordNotifierRespectsContextDeadline(t *testing.T) {
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

	err := (DiscordNotifier{WebhookURL: srv.URL}).Notify(ctx, Notification{})
	if err == nil {
		t.Fatal("expected an error when the context deadline is exceeded before the webhook responds")
	}
}

func TestDiscordNotifierUnreachableURLErrors(t *testing.T) {
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
	if err := (DiscordNotifier{WebhookURL: "http://" + addr + "/not-a-real-endpoint"}).Notify(ctx, Notification{}); err == nil {
		t.Fatal("expected an error for an unreachable webhook URL")
	}
}

// TestDiscordNotifierRedactsWebhookURLOnConnectionFailure is the
// regression test for a real finding from review: a webhook POST that
// fails during DNS/TLS/connect/timeout returns a *url.Error whose string
// embeds the full request URL — which, for a real Discord webhook,
// contains its authentication token — and logging that error verbatim
// would write the live credential into factoryd's logs.
func TestDiscordNotifierRedactsWebhookURLOnConnectionFailure(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()

	const secretToken = "super-secret-webhook-token"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = (DiscordNotifier{WebhookURL: "http://" + addr + "/api/webhooks/1/" + secretToken}).Notify(ctx, Notification{})
	if err == nil {
		t.Fatal("expected an error for an unreachable webhook URL")
	}
	if strings.Contains(err.Error(), secretToken) {
		t.Errorf("error = %q, want it to never contain the webhook token", err.Error())
	}
}

// TestDiscordNotifierRedactsWebhookURLOnMalformedURL covers the other
// error path that can embed the URL: http.NewRequestWithContext itself
// returns a *url.Error for a URL it can't parse.
func TestDiscordNotifierRedactsWebhookURLOnMalformedURL(t *testing.T) {
	const secretToken = "super-secret-webhook-token"
	err := (DiscordNotifier{WebhookURL: "http://\x7f/" + secretToken}).Notify(context.Background(), Notification{})
	if err == nil {
		t.Fatal("expected an error for a malformed webhook URL")
	}
	if strings.Contains(err.Error(), secretToken) {
		t.Errorf("error = %q, want it to never contain the webhook token", err.Error())
	}
}

// TestDiscordNotifierNamesRequestForRequestScopedNotification proves a
// request-scoped notification's content names the request rather than an
// empty run header.
func TestDiscordNotifierNamesRequestForRequestScopedNotification(t *testing.T) {
	var gotBody discordPayload
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	n := Notification{RequestID: "req-1", Reason: "waiting for you", State: run.State("spec_review")}
	if err := (DiscordNotifier{WebhookURL: srv.URL}).Notify(context.Background(), n); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if !strings.HasPrefix(gotBody.Content, "factoryd: request req-1 -> spec_review\n") {
		t.Errorf("content = %q, want it to lead with the request header", gotBody.Content)
	}
}
