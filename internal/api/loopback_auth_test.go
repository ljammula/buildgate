package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"buildgate/internal/release"
	"buildgate/internal/request"
	"buildgate/internal/run"
)

// loopbackRequestFor builds a request against a /requests route the same
// way requestActionFor does, but with an explicit Host header -- the
// loopback bind's own Host check looks at r.Host, which httptest.NewRequest
// otherwise defaults to "example.com" for a bare relative path, never a
// value this package's tests would otherwise exercise.
func loopbackRequestFor(t *testing.T, method, path, host, body string) *http.Request {
	t.Helper()
	req := requestActionFor(t, method, path, "", body)
	req.Host = host
	return req
}

// TestLoopbackRejectsRebindingHost covers the loopback bind's
// DNS-rebinding defense: a Server told its own -addr via WithListenAddr
// refuses every route -- including a plain read -- whose Host header
// doesn't name this server's own loopback address, even with no Origin
// header at all (what a bare GET navigation, not just a rebound
// fetch(), would send).
func TestLoopbackRejectsRebindingHost(t *testing.T) {
	dataDir := t.TempDir()
	seedApprovableRequest(t, dataDir, "req-1", request.StateSpecReview, false)
	server := NewServer(dataDir, WithListenAddr("127.0.0.1:8090"), WithOverrideToken("test-token"))

	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, loopbackRequestFor(t, http.MethodGet, "/requests/req-1", "attacker.example:8090", ""))

	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusForbidden, recorder.Body.String())
	}
}

// TestLoopbackRejectsCrossOriginWrite covers the loopback relaxation's
// CSRF defense: even with a Host header naming this server's own
// loopback address, a write route
// with no configured override token refuses a request whose Origin names
// a different origin -- regardless of what Sec-Fetch-Site claims (an
// adversarial review, 2026-09-24, found: a mismatched Origin is no longer
// overridable by a forgeable Sec-Fetch-Site value the way it was before
// loopbackSameOriginWrite's own doc comment rewrite -- only a non-browser
// caller can set Sec-Fetch-Site at all, so trusting it over a mismatched
// Origin gained nothing a real browser would ever send).
func TestLoopbackRejectsCrossOriginWrite(t *testing.T) {
	dataDir := t.TempDir()
	seedApprovableRequest(t, dataDir, "req-1", request.StateSpecReview, false)
	server := NewServer(dataDir, WithListenAddr("127.0.0.1:8090"))

	recorder := httptest.NewRecorder()
	req := loopbackRequestFor(t, http.MethodPost, "/requests/req-1/approve", "127.0.0.1:8090", "")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://evil.example")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	server.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusForbidden, recorder.Body.String())
	}
	reloaded, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.State != request.StateSpecReview {
		t.Errorf("State = %q, want unchanged %q", reloaded.State, request.StateSpecReview)
	}
}

// TestLoopbackAllowsSameOriginWriteWithoutToken covers the loopback
// relaxation's positive case: bound to loopback, no override token
// configured, a request whose Host
// names this server's own loopback address, whose Origin matches it (or
// is absent with Sec-Fetch-Site: same-origin), and whose Content-Type is
// application/json succeeds without any bearer token at all -- exactly
// what the embedded console's own fetch() calls send (an adversarial
// review, 2026-09-24, narrowed this relaxation to the console
// specifically; see TestLoopbackRejectsWriteWithNoOriginSignal and
// TestLoopbackRejectsNonJSONContentType for what it now excludes).
func TestLoopbackAllowsSameOriginWriteWithoutToken(t *testing.T) {
	dataDir := t.TempDir()
	server := NewServer(dataDir, WithListenAddr("127.0.0.1:8090"))

	cases := []struct {
		name   string
		id     string
		modify func(r *http.Request)
	}{
		{"same-origin Origin", "req-same-origin", func(r *http.Request) { r.Header.Set("Origin", "http://127.0.0.1:8090") }},
		{"no Origin, Sec-Fetch-Site same-origin", "req-sec-fetch-site", func(r *http.Request) {
			r.Header.Set("Sec-Fetch-Site", "same-origin")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			seedApprovableRequest(t, dataDir, tc.id, request.StateSpecReview, false)
			recorder := httptest.NewRecorder()
			req := loopbackRequestFor(t, http.MethodPost, "/requests/"+tc.id+"/approve", "127.0.0.1:8090", "")
			req.Header.Set("Content-Type", "application/json; charset=utf-8")
			tc.modify(req)
			server.ServeHTTP(recorder, req)

			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
			}
		})
	}
}

// TestLoopbackRejectsWriteWithNoOriginSignal locks in a narrowing added
// by an adversarial review (2026-09-24): a request with neither an
// Origin header nor a Sec-Fetch-Site header -- what a bare `curl` or a
// classic HTML <form> POST looks like -- no longer qualifies for the
// no-token relaxation, even on loopback with a matching Host and a JSON
// Content-Type. Before this, an absent Origin was treated the same as
// a real same-origin browser request; a local operator who wants this
// without a browser still has `factoryd approve`/`cancel`/etc., which
// touch the data directory
// directly and need no HTTP auth of their own.
func TestLoopbackRejectsWriteWithNoOriginSignal(t *testing.T) {
	dataDir := t.TempDir()
	seedApprovableRequest(t, dataDir, "req-1", request.StateSpecReview, false)
	server := NewServer(dataDir, WithListenAddr("127.0.0.1:8090"))

	recorder := httptest.NewRecorder()
	req := loopbackRequestFor(t, http.MethodPost, "/requests/req-1/approve", "127.0.0.1:8090", "")
	req.Header.Set("Content-Type", "application/json")
	server.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusForbidden, recorder.Body.String())
	}
	reloaded, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.State != request.StateSpecReview {
		t.Errorf("State = %q, want unchanged %q", reloaded.State, request.StateSpecReview)
	}
}

// TestLoopbackRejectsNonJSONContentType locks in a narrowing added by
// an adversarial review (2026-09-24): even a same-origin request
// (matching Origin) on loopback is refused the no-token relaxation when
// its Content-Type isn't application/json -- text/plain is one of the
// three Content-Type values a classic HTML <form> can send without any
// Origin/Sec-Fetch-Site header
// at all in some browser configurations, and blocking it here closes that
// gap independently of the Origin check.
func TestLoopbackRejectsNonJSONContentType(t *testing.T) {
	dataDir := t.TempDir()
	seedApprovableRequest(t, dataDir, "req-1", request.StateSpecReview, false)
	server := NewServer(dataDir, WithListenAddr("127.0.0.1:8090"))

	recorder := httptest.NewRecorder()
	req := loopbackRequestFor(t, http.MethodPost, "/requests/req-1/approve", "127.0.0.1:8090", "")
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("Origin", "http://127.0.0.1:8090")
	server.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusForbidden, recorder.Body.String())
	}
	reloaded, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.State != request.StateSpecReview {
		t.Errorf("State = %q, want unchanged %q", reloaded.State, request.StateSpecReview)
	}
}

// TestNonLoopbackWriteStillRequiresToken covers the plan's own "with a
// wider bind ... behaviour is unchanged" scope decision: a Server bound to
// a non-loopback address (or never told its bind at all) never relaxes
// the override-token requirement, no matter what Host/Origin a caller
// sends.
func TestNonLoopbackWriteStillRequiresToken(t *testing.T) {
	dataDir := t.TempDir()
	seedApprovableRequest(t, dataDir, "req-1", request.StateSpecReview, false)

	for _, addr := range []string{"0.0.0.0:8090", ":8090", "10.0.0.5:8090"} {
		t.Run(addr, func(t *testing.T) {
			server := NewServer(dataDir, WithListenAddr(addr))
			recorder := httptest.NewRecorder()
			req := requestActionFor(t, http.MethodPost, "/requests/req-1/approve", "", "")
			// No Host override here: with no loopback opt-in the Host
			// check itself must not fire either, so a request reaches the
			// override-token gate and is refused there.
			server.ServeHTTP(recorder, req)

			if recorder.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusForbidden, recorder.Body.String())
			}
		})
	}
}

// TestConsoleConfigWritesEnabled checks that GET /console-config.json's
// own writes_enabled field reports exactly
// what authorizeRequestWrite would decide for this same request -- true
// only for a same-origin request on a loopback bind with no override
// token configured. It says nothing about overrideRun's own, stricter
// authorizeOverride -- see TestLoopbackOverrideRunStillRequiresToken.
func TestConsoleConfigWritesEnabled(t *testing.T) {
	dataDir := t.TempDir()

	t.Run("loopback no token same-origin", func(t *testing.T) {
		server := NewServer(dataDir, WithListenAddr("127.0.0.1:8090"))
		recorder := httptest.NewRecorder()
		req := loopbackRequestFor(t, http.MethodGet, "/console-config.json", "127.0.0.1:8090", "")
		// An adversarial review (2026-09-24) narrowed
		// loopbackSameOriginWrite to require either a matching Origin or
		// (absent Origin) Sec-Fetch-Site: same-origin -- exactly what the
		// console's own same-origin fetch() sends, unlike a bare request
		// with neither header at all.
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		server.ServeHTTP(recorder, req)
		var got consoleConfigView
		if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if !got.WritesEnabled {
			t.Errorf("WritesEnabled = false, want true")
		}
	})

	t.Run("token configured", func(t *testing.T) {
		server := NewServer(dataDir, WithListenAddr("127.0.0.1:8090"), WithOverrideToken("test-token"))
		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, loopbackRequestFor(t, http.MethodGet, "/console-config.json", "127.0.0.1:8090", ""))
		var got consoleConfigView
		if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got.WritesEnabled {
			t.Errorf("WritesEnabled = true, want false when a token is configured")
		}
	})

	t.Run("non-loopback bind", func(t *testing.T) {
		server := NewServer(dataDir, WithListenAddr("0.0.0.0:8090"))
		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, requestActionFor(t, http.MethodGet, "/console-config.json", "", ""))
		var got consoleConfigView
		if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got.WritesEnabled {
			t.Errorf("WritesEnabled = true, want false off loopback")
		}
	})
}

// TestConsoleConfigTemporalUIURL checks that temporal_ui_url is omitted
// when unset and present when WithTemporalUIURL is configured.
func TestConsoleConfigTemporalUIURL(t *testing.T) {
	dataDir := t.TempDir()

	recorder := httptest.NewRecorder()
	NewServer(dataDir).ServeHTTP(recorder, requestActionFor(t, http.MethodGet, "/console-config.json", "", ""))
	if strings.Contains(recorder.Body.String(), "temporal_ui_url") {
		t.Errorf("body %q names temporal_ui_url when unset", recorder.Body.String())
	}

	recorder = httptest.NewRecorder()
	NewServer(dataDir, WithTemporalUIURL("http://localhost:8233")).ServeHTTP(recorder, requestActionFor(t, http.MethodGet, "/console-config.json", "", ""))
	var got consoleConfigView
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.TemporalUIURL != "http://localhost:8233" {
		t.Errorf("TemporalUIURL = %q, want %q", got.TemporalUIURL, "http://localhost:8233")
	}
}

// TestConsoleConfigReleasePolicyWarning is the regression test for
// surfacing a release-policy denial in the console: GET
// /console-config.json must surface a
// non-empty release_policy_warning exactly when this Server's own
// configured release.MergePolicy can never allow a release decision, so
// a request board can explain why every PR is silently withheld instead
// of leaving the operator to discover it after a first accepted run
// produces nothing.
func TestConsoleConfigReleasePolicyWarning(t *testing.T) {
	dataDir := t.TempDir()

	t.Run("deny-all policy warns", func(t *testing.T) {
		server := NewServer(dataDir, WithReleasePolicy(release.MergePolicy{}))
		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, requestActionFor(t, http.MethodGet, "/console-config.json", "", ""))
		var got consoleConfigView
		if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got.ReleasePolicyWarning == "" {
			t.Error("ReleasePolicyWarning = \"\", want a non-empty reason for a deny-all policy")
		}
		if !strings.Contains(got.ReleasePolicyWarning, "release_max_files_changed") {
			t.Errorf("ReleasePolicyWarning = %q, want it to name the session-config fix", got.ReleasePolicyWarning)
		}
	})

	t.Run("usable policy is silent", func(t *testing.T) {
		server := NewServer(dataDir, WithReleasePolicy(release.MergePolicy{MaxFilesChanged: 25, MaxInsertions: 1000, RollbackPlan: "git revert the merge commit on main"}))
		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, requestActionFor(t, http.MethodGet, "/console-config.json", "", ""))
		if strings.Contains(recorder.Body.String(), "release_policy_warning") {
			t.Errorf("body %q names release_policy_warning for a usable policy", recorder.Body.String())
		}
	})
}

// TestLoopbackAllowedHostAcceptsReadButNotWriteRelaxation locks in an
// allowed-host distinction added by an adversarial review (2026-09-24):
// `factoryd serve -allowed-host` lets a request through this server's
// own Host check (DNS-rebinding defense)
// under a Host value that isn't its own true loopback address -- an
// operator reaching it through `ssh -L 9000:localhost:8090 ...`, say --
// but that acceptance is for reads only. A write under the same allowed
// Host, with no override token, must still be refused: only the server's
// own true loopback address ever qualifies for the no-token relaxation.
func TestLoopbackAllowedHostAcceptsReadButNotWriteRelaxation(t *testing.T) {
	dataDir := t.TempDir()
	seedApprovableRequest(t, dataDir, "req-1", request.StateSpecReview, false)
	server := NewServer(dataDir, WithListenAddr("127.0.0.1:8090"), WithAllowedHosts([]string{"localhost:9000"}))

	t.Run("read succeeds under the allowed host", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, loopbackRequestFor(t, http.MethodGet, "/requests/req-1", "localhost:9000", ""))
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
		}
	})

	t.Run("write under the allowed host still requires a token", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		req := loopbackRequestFor(t, http.MethodPost, "/requests/req-1/approve", "localhost:9000", "")
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", "http://localhost:9000")
		server.ServeHTTP(recorder, req)
		if recorder.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want %d (an allowed host must never qualify for the no-token write relaxation): %s", recorder.Code, http.StatusForbidden, recorder.Body.String())
		}
		reloaded, err := request.Load(dataDir, "req-1")
		if err != nil {
			t.Fatal(err)
		}
		if reloaded.State != request.StateSpecReview {
			t.Errorf("State = %q, want unchanged %q", reloaded.State, request.StateSpecReview)
		}
	})
}

// TestLoopbackRejectsHostNotInAllowedList covers the negative case of the
// allowed-host distinction above: a Host naming neither this server's
// own true loopback address nor a
// configured -allowed-host value is still refused.
func TestLoopbackRejectsHostNotInAllowedList(t *testing.T) {
	dataDir := t.TempDir()
	seedApprovableRequest(t, dataDir, "req-1", request.StateSpecReview, false)
	server := NewServer(dataDir, WithListenAddr("127.0.0.1:8090"), WithAllowedHosts([]string{"localhost:9000"}))

	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, loopbackRequestFor(t, http.MethodGet, "/requests/req-1", "attacker.example:8090", ""))
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusForbidden, recorder.Body.String())
	}
}

// TestLoopbackOverrideRunStillRequiresToken locks in a stricter check
// added by an adversarial review (2026-09-24): POST /runs/{id}/override
// moves a quarantined run straight to accepted or halted and records a
// release Decision -- unlike the request-pipeline write routes the
// loopback relaxation covers (see
// TestLoopbackAllowsSameOriginWriteWithoutToken), a same-origin, no-token
// loopback request must still be refused here. Before this was split
// into authorizeOverride (token only) vs authorizeRequestWrite (the
// loopback relaxation allowed), authorizeOverride's own relaxation meant any
// same-origin process on the operator's machine -- not just the console
// -- could accept/halt a quarantined run with no token at all, even
// though WithOverrideToken exists precisely to gate this operation.
func TestLoopbackOverrideRunStillRequiresToken(t *testing.T) {
	dataDir := t.TempDir()
	seedRun(t, dataDir, run.Run{ID: "run-1", Ticket: "ticket-1", State: run.StateQuarantined, CreatedAt: "2026-08-26T10:00:00Z"})

	// No WithOverrideToken configured -- exactly the condition under which
	// the loopback relaxation lets every other write route through with
	// no token. A same-origin request naming this server's own loopback
	// Host, with no Origin header at all (loopbackSameOriginWrite's own
	// most permissive case).
	server := NewServer(dataDir, WithListenAddr("127.0.0.1:8090"))
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/runs/run-1/override", strings.NewReader(`{"by":"operator","reason":"reviewed manually","state":"accepted"}`))
	req.Host = "127.0.0.1:8090"
	server.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d (override must always require a bearer token, even on a same-origin loopback request with no token configured): %s", recorder.Code, http.StatusForbidden, recorder.Body.String())
	}
	reloaded, err := run.Load(dataDir, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.State != run.StateQuarantined {
		t.Errorf("State = %q, want unchanged %q (override must not have applied)", reloaded.State, run.StateQuarantined)
	}
}

// TestNewServerWithoutListenAddrUnaffected is a regression guard: every
// test in this package that predates the loopback bind constructs a
// Server with no WithListenAddr at all, so the Host check and
// loopback-write relaxation must both stay
// completely inert for one -- confirmed here against the exact "example.com"
// Host httptest.NewRequest defaults a bare relative-path request to.
func TestNewServerWithoutListenAddrUnaffected(t *testing.T) {
	dataDir := t.TempDir()
	seedApprovableRequest(t, dataDir, "req-1", request.StateSpecReview, false)
	server := NewServer(dataDir, WithOverrideToken("test-token"))

	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, requestActionFor(t, http.MethodPost, "/requests/req-1/approve", "test-token", ""))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
}
