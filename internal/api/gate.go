package api

import (
	"context"
	"net/http"
	"strings"
	"time"

	"buildgate/internal/request"
)

// GateToken is what a gate-token source reports on each call. The gate token
// is the operator's credential for a console that is not on this machine: it
// opens the reads and the request writes (authorizeRead,
// authorizeRequestWrite), and nothing else. It never opens POST
// /runs/{id}/override, the start-token routes or POST /mcp.
type GateToken struct {
	// On is whether the gate is on: the operator created a gate token for
	// this serve. While it is on, a request that is not local needs the
	// token to read.
	On bool
	// Token is the usable token, "" when the gate is on with none (the
	// token expired, or its file fails inspection). The gate then stays on
	// and no token opens it: a token that goes bad must not reopen the
	// reads it was guarding.
	Token string
}

// WithGateToken turns on the gate token, read from source on every request
// that needs it, so creating, rotating or removing the token needs no
// restart. Without this option the gate is off.
func WithGateToken(source func() GateToken) Option {
	return func(s *Server) {
		s.gateToken = source
	}
}

// gateViaSuffix is appended to the name a request write is recorded under
// when the gate token authorized it. A caller may not send a name that
// already carries it.
const gateViaSuffix = " (gate token)"

// forwardingHeaders are the request headers a reverse proxy adds for the
// backend. A proxy sets them after dropping the caller's own hop-by-hop
// headers, so a caller behind one cannot remove them.
var forwardingHeaders = []string{"Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "X-Real-Ip", "Via"}

// forwarded reports whether a reverse proxy passed r on: it carries a
// forwarding header, or any Tailscale-* header (`tailscale serve` names the
// caller in those).
func forwarded(r *http.Request) bool {
	for _, name := range forwardingHeaders {
		if len(r.Header.Values(name)) > 0 {
			return true
		}
	}
	for name := range r.Header {
		if strings.HasPrefix(name, "Tailscale-") {
			return true
		}
	}
	return false
}

// local reports whether r came from this machine and not through a proxy:
// the server is bound to loopback, so only a process on this machine can
// connect, r names the server's own loopback address, and no proxy passed
// it on. Every no-token relaxation asks this, never the Host header alone: a
// proxy such as `tailscale serve` keeps the caller's Host, so a caller on
// the proxy's network can send "127.0.0.1:<port>" itself.
//
// A forwarder that adds no forwarding header (a TCP-level forward, an ssh
// tunnel, a proxy configured to rewrite Host and add nothing) makes its
// callers local by this test. That is a deployment limit, stated in
// safety-contract.md: such a setup needs -override-token.
func (s *Server) local(r *http.Request) bool {
	return s.loopback && s.hostMatchesLoopback(r) && !forwarded(r)
}

// gate returns the gate token's state for this request. A token equal to
// another credential of this server is not usable: one value must never
// open two classes of route.
func (s *Server) gate() GateToken {
	if s.gateToken == nil {
		return GateToken{}
	}
	g := s.gateToken()
	if !g.On {
		return GateToken{}
	}
	if g.Token != "" {
		others := []string{s.overrideToken, s.startToken, s.readToken}
		if s.mcpToken != nil {
			others = append(others, s.mcpToken())
		}
		for _, other := range others {
			if other == g.Token {
				g.Token = ""
			}
		}
	}
	return g
}

// gateBearer reports whether r carries the usable gate token.
func (s *Server) gateBearer(r *http.Request) bool {
	// No file read for a request with no bearer at all.
	if r.Header.Get("Authorization") == "" {
		return false
	}
	return s.authorize(r, s.gate().Token)
}

// gateRequired reports whether r needs the gate token to read: the gate is
// on and r is not local.
func (s *Server) gateRequired(r *http.Request) bool {
	return !s.local(r) && s.gate().On
}

// consoleGate is /console-config.json's "gate" for r: "off" when r needs no
// gate token, "accepted" when it carries the usable one, else "required".
func (s *Server) consoleGate(r *http.Request) string {
	switch {
	case !s.gateRequired(r):
		return "off"
	case s.gateBearer(r):
		return "accepted"
	default:
		return "required"
	}
}

// badPrincipal is the refusal for a name writePrincipal does not accept.
const badPrincipal = "by is not a name this write can be recorded under: it is too long, or it claims the gate token, which only the server adds"

// writePrincipal is the name a request write by r is recorded under: by
// (requestAPIPrincipal when empty), with gateViaSuffix when only the gate
// token authorized r. ok is false for a by that already carries the suffix,
// which no caller may claim, or one too long to record with it.
func (s *Server) writePrincipal(r *http.Request, by string) (principal string, ok bool) {
	if strings.Contains(by, strings.TrimSpace(gateViaSuffix)) {
		return "", false
	}
	if by == "" {
		by = requestAPIPrincipal
	}
	if s.viaGateOnly(r) {
		by += gateViaSuffix
		if len(by) > request.MaxEditByLen {
			return "", false
		}
	}
	return by, true
}

// viaGateOnly reports whether the gate token is the only thing that
// authorizes r as a request write.
func (s *Server) viaGateOnly(r *http.Request) bool {
	if s.overrideToken == "" && s.loopbackSameOriginWrite(r) && loopbackWriteHasJSONContentType(r) {
		return false
	}
	if s.authorize(r, s.overrideToken) {
		return false
	}
	return s.gateBearer(r)
}

// whileAuthorizedToRead returns r with a context that ends when r stops
// being authorized to read: a stream is authorized when it opens, and the
// gate token that opened it can be rotated out or expire while it is open.
// It asks again at each poll interval. The caller calls stop when the stream
// ends.
func (s *Server) whileAuthorizedToRead(r *http.Request) (watched *http.Request, stop func()) {
	ctx, cancel := context.WithCancel(r.Context())
	watched = r.WithContext(ctx)
	go func() {
		ticker := time.NewTicker(s.pollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if !s.authorizeRead(watched) {
					cancel()
					return
				}
			}
		}
	}()
	return watched, cancel
}
