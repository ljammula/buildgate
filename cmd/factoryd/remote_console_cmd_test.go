package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"buildgate/internal/consolelink"
)

const fakeTailnetName = "builder.example-tailnet.ts.net"

// fakeTailscale stands in for the tailscale CLI: a node named
// fakeTailnetName whose listeners are web ("/" target per port) and tcp
// (ports that forward plain TCP).
type fakeTailscale struct {
	state string
	web   map[int]string
	tcp   map[int]bool
	calls []string
	fail  string
}

func (f *fakeTailscale) run(_ context.Context, args ...string) ([]byte, error) {
	call := strings.Join(args, " ")
	f.calls = append(f.calls, call)
	if f.fail != "" && strings.HasPrefix(call, f.fail) {
		return []byte("tailscale: refused\n"), errors.New("exit status 1")
	}
	switch {
	case call == "status --json":
		return json.Marshal(map[string]any{"BackendState": f.state, "Self": map[string]string{"DNSName": fakeTailnetName + "."}})
	case call == "serve status --json":
		tcp, web := map[string]any{}, map[string]any{}
		for port := range f.tcp {
			tcp[strconv.Itoa(port)] = map[string]bool{"TCPForward": true}
		}
		for port, target := range f.web {
			tcp[strconv.Itoa(port)] = map[string]bool{"HTTPS": true}
			web[fmt.Sprintf("%s:%d", fakeTailnetName, port)] = map[string]any{"Handlers": map[string]any{"/": map[string]string{"Proxy": target}}}
		}
		return json.Marshal(map[string]any{"TCP": tcp, "Web": web})
	case len(args) == 4 && args[0] == "serve" && args[1] == "--bg" && strings.HasPrefix(args[2], "--https="):
		port, _ := strconv.Atoi(strings.TrimPrefix(args[2], "--https="))
		f.web[port] = args[3]
		return nil, nil
	case len(args) == 3 && args[0] == "serve" && strings.HasPrefix(args[1], "--https=") && args[2] == "off":
		port, _ := strconv.Atoi(strings.TrimPrefix(args[1], "--https="))
		delete(f.web, port)
		return nil, nil
	}
	return nil, fmt.Errorf("fake tailscale: unexpected call %q", call)
}

// remoteConsoleFixture is a session config whose data dir has a serve
// recorded at addr, and a fake tailnet with port 443 already serving
// something else.
func remoteConsoleFixture(t *testing.T, addr string) (dp *deps, ts *fakeTailscale, cfgDir, dataDir string) {
	t.Helper()
	dp = newTestDeps(t)
	cfgDir, dataDir = mcpTestConfig(t)
	ts = &fakeTailscale{state: "Running", web: map[int]string{443: "http://127.0.0.1:3000"}, tcp: map[int]bool{}}
	dp.host.(*fakeHost).tailscaleFn = ts.run
	origListening := consolelink.Listening
	t.Cleanup(func() { consolelink.Listening = origListening })
	consolelink.Listening = func(string) bool { return true }
	remove, err := consolelink.RecordServeAddress(dataDir, addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(remove)
	return dp, ts, cfgDir, dataDir
}

func runRemoteConsole(t *testing.T, dp *deps, args ...string) string {
	t.Helper()
	var out bytes.Buffer
	if err := remoteConsoleMain(dp, &out, args); err != nil {
		t.Fatalf("remote-console %v: %v\n%s", args, err, out.String())
	}
	return out.String()
}

// TestRemoteConsoleOpensAndClosesTheConsoleToAnotherMachine: one command
// adds the listener on a free port, records the host `serve` then accepts,
// and writes a gate token; -off removes all three.
func TestRemoteConsoleOpensAndClosesTheConsoleToAnotherMachine(t *testing.T) {
	dp, ts, cfgDir, _ := remoteConsoleFixture(t, "127.0.0.1:8097")
	path := filepath.Join(cfgDir, "config.remote-console")
	hosts := remoteConsoleHostSource(path)
	gate := gateTokenSource(filepath.Join(cfgDir, "config.gate-token"))
	if got := hosts(); len(got) != 0 {
		t.Fatalf("before the command: allowed hosts = %v, want none", got)
	}

	out := runRemoteConsole(t, dp)
	wantHost := fakeTailnetName + ":8443"
	if got := hosts(); len(got) != 1 || got[0] != wantHost {
		t.Fatalf("allowed hosts = %v, want [%s] (443 serves something else)", got, wantHost)
	}
	if ts.web[8443] != "http://127.0.0.1:8097" || ts.web[443] != "http://127.0.0.1:3000" {
		t.Errorf("listeners = %v, want 8443 forwarding to the serve and 443 untouched", ts.web)
	}
	token := gate()
	if !token.On || token.Token == "" {
		t.Fatalf("gate token = %+v, want one written", token)
	}
	if info, err := os.Lstat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("remote-console file mode = %v (err %v), want 0600", info.Mode().Perm(), err)
	}
	for _, want := range []string{"https://" + wantHost + "/", token.Token, "paste the gate token", "remote-console -off"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "#gate=") {
		t.Errorf("output puts the token in a link:\n%s", out)
	}

	// Again: the same listener, host and token; nothing is added twice.
	calls := len(ts.calls)
	runRemoteConsole(t, dp)
	if got := gate(); got != token {
		t.Errorf("a second run changed the gate token")
	}
	for _, call := range ts.calls[calls:] {
		if strings.HasPrefix(call, "serve --bg") || strings.HasSuffix(call, " off") {
			t.Errorf("a second run changed the listeners: %q", call)
		}
	}

	out = runRemoteConsole(t, dp, "-off")
	if got := hosts(); len(got) != 0 {
		t.Errorf("after -off: allowed hosts = %v, want none", got)
	}
	if got := gate(); got.On {
		t.Errorf("after -off: gate = %+v, want it off", got)
	}
	if _, still := ts.web[8443]; still || ts.web[443] == "" {
		t.Errorf("after -off: listeners = %v, want 8443 gone and 443 untouched", ts.web)
	}
	if !strings.Contains(out, "8443") {
		t.Errorf("-off output does not name what it removed:\n%s", out)
	}
	runRemoteConsole(t, dp, "-off")
}

// TestRemoteConsoleNeverTakesAPortThatServesSomethingElse covers the choice
// of port: the listener already forwarding to this serve is reused, a wanted
// port that serves something else is refused, and plain-TCP ports are taken.
func TestRemoteConsoleNeverTakesAPortThatServesSomethingElse(t *testing.T) {
	const backend = "127.0.0.1:8097"
	cases := []struct {
		name      string
		listeners map[int]string
		wanted    int
		want      int
		wantErr   string
	}{
		{"nothing listens", map[int]string{}, 0, 443, ""},
		{"443 taken", map[int]string{443: "http://127.0.0.1:3000"}, 0, 8443, ""},
		{"443 is plain TCP or other paths", map[int]string{443: ""}, 0, 8443, ""},
		{"already forwarding on 8444", map[int]string{443: "http://127.0.0.1:3000", 8444: "http://" + backend}, 0, 8444, ""},
		{"wanted and free", map[int]string{443: "http://127.0.0.1:3000"}, 9443, 9443, ""},
		{"wanted and ours", map[int]string{9443: "http://" + backend}, 9443, 9443, ""},
		{"wanted and taken", map[int]string{443: "http://127.0.0.1:3000"}, 443, 0, "already serves http://127.0.0.1:3000"},
		{"every default taken", map[int]string{443: "x", 8443: "x", 8444: "x", 8445: "x", 8446: "x", 8447: "x", 8448: "x", 8449: "x"}, 0, 0, "-https-port"},
	}
	for _, tc := range cases {
		got, err := chooseRemotePort(tc.listeners, backend, tc.wanted)
		if tc.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("%s: err = %v, want one naming %q", tc.name, err, tc.wantErr)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("%s: port = %d (err %v), want %d", tc.name, got, err, tc.want)
		}
	}
	if got := remoteHostFor(fakeTailnetName, 443); got != fakeTailnetName {
		t.Errorf("host for port 443 = %q, want the bare name (a browser sends no port)", got)
	}

	dp, ts, cfgDir, _ := remoteConsoleFixture(t, backend)
	var out bytes.Buffer
	if err := remoteConsoleMain(dp, &out, []string{"-https-port", "443"}); err == nil {
		t.Fatal("-https-port 443 while 443 serves something else: want an error")
	}
	if ts.web[443] != "http://127.0.0.1:3000" || len(remoteConsoleHostSource(filepath.Join(cfgDir, "config.remote-console"))()) != 0 {
		t.Errorf("a refused port changed something: listeners %v", ts.web)
	}
	if gateTokenSource(filepath.Join(cfgDir, "config.gate-token"))().On {
		t.Error("a refused port left a gate token behind")
	}
}

// TestRemoteConsoleMovesItsListenerAndReportsWhatItCannotDo: a second run
// with another port moves the listener, and each failure says what is and is
// not in place.
func TestRemoteConsoleMovesItsListenerAndReportsWhatItCannotDo(t *testing.T) {
	dp, ts, cfgDir, _ := remoteConsoleFixture(t, "127.0.0.1:8097")
	path := filepath.Join(cfgDir, "config.remote-console")
	runRemoteConsole(t, dp)
	runRemoteConsole(t, dp, "-https-port", "9443")
	if _, old := ts.web[8443]; old || ts.web[9443] != "http://127.0.0.1:8097" {
		t.Errorf("listeners = %v, want the listener moved from 8443 to 9443", ts.web)
	}
	if rc, ok := readRemoteConsole(path); !ok || rc.Host != fakeTailnetName+":9443" || rc.Listener != 9443 || rc.Backend != "127.0.0.1:8097" {
		t.Errorf("file = %+v (ok %v), want the new port", rc, ok)
	}

	var out bytes.Buffer
	ts.state = "NeedsLogin"
	if err := remoteConsoleMain(dp, &out, nil); err == nil || !strings.Contains(err.Error(), "tailscale up") {
		t.Errorf("Tailscale not connected: err = %v, want one naming `tailscale up`", err)
	}
	ts.state = "Running"
	ts.fail = "status"
	if err := remoteConsoleMain(dp, &out, nil); err == nil || !strings.Contains(err.Error(), "another reverse proxy") {
		t.Errorf("no tailscale: err = %v, want one pointing at the manual steps", err)
	}
	ts.fail = "serve --https=9443 off"
	err := remoteConsoleMain(dp, &out, []string{"-off"})
	if err == nil || !strings.Contains(err.Error(), "tailscale serve --https=9443 off") {
		t.Errorf("-off with a listener that will not go: err = %v, want the command to run", err)
	}
	if len(remoteConsoleHostSource(path)()) != 0 || gateTokenSource(filepath.Join(cfgDir, "config.gate-token"))().On {
		t.Error("-off left the host or the gate token in place when the listener would not go")
	}
	for _, args := range [][]string{{"extra"}, {"-ttl", "1s"}, {"-https-port", "70000"}} {
		if err := remoteConsoleMain(dp, &out, args); err == nil {
			t.Errorf("remote-console %v: want an error", args)
		}
	}
}

// TestRemoteConsoleFileThatCannotBeTrustedAllowsNoHost: `serve` allows a
// host only on the word of a file with the token files' checks and exactly
// the three lines, each well formed.
func TestRemoteConsoleFileThatCannotBeTrustedAllowsNoHost(t *testing.T) {
	valid := "host " + fakeTailnetName + ":8443\nlistener 8443\nbackend 127.0.0.1:8097\n"
	cases := map[string]func(t *testing.T, path string){
		"readable by others": func(t *testing.T, path string) { writeFileMode(t, path, valid, 0o644) },
		"a symlink": func(t *testing.T, path string) {
			target := filepath.Join(t.TempDir(), "elsewhere")
			writeFileMode(t, target, valid, 0o600)
			if err := os.Symlink(target, path); err != nil {
				t.Fatal(err)
			}
		},
		"no host line": func(t *testing.T, path string) {
			writeFileMode(t, path, "listener 8443\nbackend 127.0.0.1:8097\n", 0o600)
		},
		"a URL as host": func(t *testing.T, path string) {
			writeFileMode(t, path, strings.Replace(valid, "host ", "host https://", 1), 0o600)
		},
		"a wildcard host": func(t *testing.T, path string) {
			writeFileMode(t, path, strings.Replace(valid, fakeTailnetName, "*.ts.net", 1), 0o600)
		},
		"an unknown line": func(t *testing.T, path string) { writeFileMode(t, path, valid+"host2 other.example\n", 0o600) },
		"two hosts a line": func(t *testing.T, path string) {
			writeFileMode(t, path, strings.Replace(valid, ":8443\n", ":8443 other.example\n", 1), 0o600)
		},
		"empty": func(t *testing.T, path string) { writeFileMode(t, path, "", 0o600) },
	}
	for name, write := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.remote-console")
			write(t, path)
			if got := remoteConsoleHostSource(path)(); len(got) != 0 {
				t.Errorf("allowed hosts = %v, want none", got)
			}
		})
	}
	path := filepath.Join(t.TempDir(), "config.remote-console")
	writeFileMode(t, path, valid, 0o600)
	if got := remoteConsoleHostSource(path)(); len(got) != 1 || got[0] != fakeTailnetName+":8443" {
		t.Errorf("a well-formed file: allowed hosts = %v", got)
	}
}

// TestDoctorWarnsOfARemoteConsoleWhoseListenerPointsElsewhere: a serve that
// came back on another port leaves the listener forwarding to the old one.
func TestDoctorWarnsOfARemoteConsoleWhoseListenerPointsElsewhere(t *testing.T) {
	dp, _, cfgDir, dataDir := remoteConsoleFixture(t, "127.0.0.1:8097")
	configPath := filepath.Join(cfgDir, "config.yml")
	if checks := doctorRemoteConsoleChecks(configPath, dataDir); len(checks) != 0 {
		t.Fatalf("remote console off: %+v, want no row", checks)
	}
	runRemoteConsole(t, dp)
	if checks := doctorRemoteConsoleChecks(configPath, dataDir); len(checks) != 0 {
		t.Fatalf("listener matches the serve: %+v, want no row", checks)
	}
	remove, err := consolelink.RecordServeAddress(dataDir, "127.0.0.1:8123")
	if err != nil {
		t.Fatal(err)
	}
	defer remove()
	checks := doctorRemoteConsoleChecks(configPath, dataDir)
	if len(checks) != 1 || !checks[0].Advisory || !strings.Contains(checks[0].Err.Error(), "127.0.0.1:8097") || !strings.Contains(checks[0].Err.Error(), "127.0.0.1:8123") || !strings.Contains(checks[0].Fix, "factoryd remote-console") {
		t.Errorf("checks = %+v, want one warning naming both addresses and the command", checks)
	}
}
