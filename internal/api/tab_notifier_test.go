package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"buildgate/internal/consolelink"
)

// flushRecorder is a ResponseRecorder the event stream can flush.
type flushRecorder struct{ *httptest.ResponseRecorder }

func (flushRecorder) Flush() {}

// streamRequestEventsBriefly opens GET path as a console on this machine
// would, with headers changed by edit, holds it open for a few polls and
// closes it.
func streamRequestEventsBriefly(t *testing.T, server *Server, path string, edit func(*http.Request)) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, path, nil).WithContext(ctx)
	req.Host = "127.0.0.1:8090"
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	if edit != nil {
		edit(req)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		server.ServeHTTP(flushRecorder{httptest.NewRecorder()}, req)
	}()
	time.Sleep(60 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the stream did not end when its request was cancelled")
	}
}

// The host's banner stands down only for a console tab on this machine that
// says it raises the notifications itself: a stream that does not say so, a
// page of another origin, and a caller through a proxy leave no record.
func TestOnlyALocalSameOriginNotifierStreamRecordsANotifyingTab(t *testing.T) {
	cases := []struct {
		name    string
		path    string
		edit    func(*http.Request)
		present bool
	}{
		{name: "a console tab on this machine that notifies", path: "/requests/events?notifier=1", present: true},
		{name: "a console tab that does not notify", path: "/requests/events"},
		{name: "a page of another origin", path: "/requests/events?notifier=1", edit: func(r *http.Request) {
			r.Header.Set("Sec-Fetch-Site", "cross-site")
			r.Header.Set("Origin", "https://elsewhere.example")
		}},
		{name: "a caller with neither Origin nor Sec-Fetch-Site", path: "/requests/events?notifier=1", edit: func(r *http.Request) {
			r.Header.Del("Sec-Fetch-Site")
		}},
		{name: "a console through a proxy", path: "/requests/events?notifier=1", edit: func(r *http.Request) {
			r.Host = "console.example"
			r.Header.Set("X-Forwarded-For", "100.64.0.1")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dataDir := t.TempDir()
			server := NewServer(dataDir, WithListenAddr("127.0.0.1:8090"), WithAllowedHosts([]string{"console.example"}), WithPollInterval(10*time.Millisecond))
			streamRequestEventsBriefly(t, server, tc.path, tc.edit)
			if got := consolelink.TabNotifierPresent(dataDir); got != tc.present {
				t.Errorf("a notifying tab is recorded = %v, want %v", got, tc.present)
			}
		})
	}
}
