package main

import (
	"net"
	"net/url"
)

// consoleBaseIsOwnLoopbackServe reports whether base (a resolved console
// base URL, e.g. from resolveConsoleBaseURL/consolelink.BaseURL) actually
// names the loopback serve address serveAddr's own start token belongs
// to -- host 127.0.0.1/localhost/::1, port matching serveAddr's own port
// exactly.
//
// Both resolveConsoleBaseURL/consolelink.BaseURL and `factoryd console`'s
// own base resolution honor FACTORYD_CONSOLE_URL, an operator override
// meant for "the console is reachable at a different address than the
// default guess" (a reverse proxy, a different port, a machine reached
// over Tailscale). None of that is wrong on its own, but a start token
// is a PERMANENT credential for every start-class route on THIS
// machine's own serve (POST /runs, daemon lifecycle, release/stats) --
// appending it to whatever FACTORYD_CONSOLE_URL happens to name, with no
// check that it's even the same server the token was minted for, would
// hand that permanent credential to any origin an operator (or anything
// that can set that env var in this process's environment) points it at
// (an adversarial review, 2026-09-24). Only ever attach the token
// when base resolves to exactly the loopback address the token is
// actually for; otherwise the caller must print the plain link instead.
func consoleBaseIsOwnLoopbackServe(base, serveAddr string) bool {
	if base == "" || serveAddr == "" {
		return false
	}
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		return false
	}
	host, port, err := net.SplitHostPort(u.Host)
	if err != nil {
		return false
	}
	_, wantPort, err := net.SplitHostPort(serveAddr)
	if err != nil {
		return false
	}
	if port != wantPort {
		return false
	}
	switch host {
	case "127.0.0.1", "localhost", "::1":
		return true
	default:
		return false
	}
}
