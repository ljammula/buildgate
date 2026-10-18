package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
)

// An MCP client reports a request to its operator, who follows and approves
// it in the console: every request a request tool returns carries
// console_url, the console page of that request. It is built from the
// address the MCP call came in on, and only when that address is one this
// server accepts (hostAllowed: its own loopback address, or an -allowed-host
// name such as `factoryd remote-console`'s), so it names a console the
// caller's side can reach and a caller cannot plant another host in it; on
// a wider -addr, where ServeHTTP checks no Host, any other Host gets no
// link. It never carries a token: the console on another machine asks for
// the gate token as always.

type mcpConsoleBaseKey struct{}

// mcpRequestTools are the tools whose result is a request or a list of
// them.
var mcpRequestTools = map[string]bool{"list_requests": true, "get_request": true, "submit_request": true}

// withMCPConsoleBase returns r with the console base of its address in its
// context, for the tool calls the SDK runs under it; r unchanged when its
// Host is not one this server accepts.
func (s *Server) withMCPConsoleBase(r *http.Request) *http.Request {
	if !s.hostAllowed(r) {
		return r
	}
	return r.WithContext(context.WithValue(r.Context(), mcpConsoleBaseKey{}, mcpConsoleBase(r)))
}

// mcpConsoleBase is scheme://host of the console r reached this server
// through: https over TLS; the scheme a proxy names in X-Forwarded-Proto;
// else https when a proxy passed r on (`tailscale serve`, a reverse proxy in
// front of a loopback serve, is where TLS ends); else plain http.
func mcpConsoleBase(r *http.Request) string {
	scheme := "http"
	proto := strings.ToLower(strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Proto"), ",")[0]))
	switch {
	case r.TLS != nil:
		scheme = "https"
	case proto == "http" || proto == "https":
		scheme = proto
	case forwarded(r):
		scheme = "https"
	}
	return (&url.URL{Scheme: scheme, Host: r.Host}).String()
}

// mcpWithConsoleURLs adds console_url to every request in a request tool's
// result: the object get_request and submit_request return, or each row of
// list_requests'. Text that is not one of those is returned unchanged.
func mcpWithConsoleURLs(ctx context.Context, tool string, text string) string {
	base, _ := ctx.Value(mcpConsoleBaseKey{}).(string)
	if base == "" || !mcpRequestTools[tool] {
		return text
	}
	trimmed := bytes.TrimSpace([]byte(text))
	if len(trimmed) > 0 && trimmed[0] == '[' {
		var rows []json.RawMessage
		if err := json.Unmarshal(trimmed, &rows); err != nil {
			return text
		}
		for i, row := range rows {
			rows[i] = withConsoleURL(base, row)
		}
		out, err := json.Marshal(rows)
		if err != nil {
			return text
		}
		return string(out)
	}
	return string(withConsoleURL(base, trimmed))
}

// withConsoleURL adds console_url to object when it has a string id.
func withConsoleURL(base string, object json.RawMessage) json.RawMessage {
	object = bytes.TrimSpace(object)
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(object, &fields); err != nil || len(object) == 0 || object[0] != '{' {
		return object
	}
	if _, present := fields["console_url"]; present {
		return object
	}
	var id string
	if err := json.Unmarshal(fields["id"], &id); err != nil || id == "" {
		return object
	}
	link, err := json.Marshal(base + "/requests/" + url.PathEscape(id))
	if err != nil {
		return object
	}
	var out bytes.Buffer
	out.WriteString(`{"console_url":`)
	out.Write(link)
	if rest := bytes.TrimSpace(object[1:]); len(rest) > 0 && rest[0] != '}' {
		out.WriteByte(',')
	}
	out.Write(bytes.TrimSpace(object[1:]))
	return out.Bytes()
}
