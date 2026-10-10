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
	// funnel ports are open to the internet; otherPaths ports serve "/api"
	// beside "/"; otherName ports belong to another name of the node.
	funnel, otherPaths, otherName map[int]bool
	calls                         []string
	fail                          string
	// onAdd runs when a listener is added, before it is.
	onAdd func()
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
		funnel := map[string]bool{}
		for port, target := range f.web {
			tcp[strconv.Itoa(port)] = map[string]bool{"HTTPS": true}
			handlers := map[string]any{"/": map[string]string{"Proxy": target}}
			if f.otherPaths[port] {
				handlers["/api"] = map[string]string{"Proxy": "http://127.0.0.1:3001"}
			}
			name := fakeTailnetName
			if f.otherName[port] {
				name = "svc.example-tailnet.ts.net"
			}
			web[fmt.Sprintf("%s:%d", name, port)] = map[string]any{"Handlers": handlers}
			if f.funnel[port] {
				funnel[fmt.Sprintf("%s:%d", name, port)] = true
			}
		}
		return json.Marshal(map[string]any{"TCP": tcp, "Web": web, "AllowFunnel": funnel})
	case len(args) == 4 && args[0] == "serve" && args[1] == "--bg" && strings.HasPrefix(args[2], "--https="):
		port, _ := strconv.Atoi(strings.TrimPrefix(args[2], "--https="))
		if f.onAdd != nil {
			f.onAdd()
		}
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
	ts = &fakeTailscale{state: "Running", web: map[int]string{443: "http://127.0.0.1:3000"}, tcp: map[int]bool{}, funnel: map[int]bool{}, otherPaths: map[int]bool{}, otherName: map[int]bool{}}
	dp.host.(*fakeHost).tailscaleFn = ts.run
	origListening := consolelink.Listening
	t.Cleanup(func() { consolelink.Listening = origListening })
	consolelink.Listening = func(string) bool { return true }
	if addr != "" {
		remove, err := consolelink.RecordServeAddress(dataDir, addr)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(remove)
	}
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
// writes a gate token, adds the listener on a free port and records the host
// `serve` then accepts, in that order; -off removes all three.
func TestRemoteConsoleOpensAndClosesTheConsoleToAnotherMachine(t *testing.T) {
	dp, ts, cfgDir, _ := remoteConsoleFixture(t, "127.0.0.1:8097")
	path := filepath.Join(cfgDir, "config.remote-console")
	hosts := remoteConsoleHostSource(path)
	gate := gateTokenSource(filepath.Join(cfgDir, "config.gate-token"))
	if got := hosts(); len(got) != 0 {
		t.Fatalf("before the command: allowed hosts = %v, want none", got)
	}
	// When the listener is added the gate token exists and the host is not
	// yet accepted: nothing forwards to a console with no gate.
	ts.onAdd = func() {
		if !gate().On || gate().Token == "" || len(hosts()) != 0 {
			t.Errorf("at the moment the listener is added: gate %+v, hosts %v; want the token written and no host yet", gate(), hosts())
		}
	}

	out := runRemoteConsole(t, dp)
	ts.onAdd = nil
	wantHost := fakeTailnetName + ":8443"
	if got := hosts(); len(got) != 1 || got[0] != wantHost {
		t.Fatalf("allowed hosts = %v, want [%s] (443 serves something else)", got, wantHost)
	}
	if ts.web[8443] != "http://127.0.0.1:8097" || ts.web[443] != "http://127.0.0.1:3000" {
		t.Errorf("listeners = %v, want 8443 forwarding to the serve and 443 untouched", ts.web)
	}
	token := gate()
	if info, err := os.Lstat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("remote-console file mode = %v (err %v), want 0600", info.Mode().Perm(), err)
	}
	for _, want := range []string{"https://" + wantHost + "/", token.Token, "paste the gate token", "remote-console -off", "tailnet"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "#gate=") {
		t.Errorf("output puts the token in a link:\n%s", out)
	}

	// Again: the same listener, host and token; nothing is added twice.
	calls := len(ts.calls)
	runRemoteConsole(t, dp, "-ttl", "30m")
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
		t.Errorf("after -off: gate = %+v, want the token this command wrote removed", got)
	}
	if _, still := ts.web[8443]; still || ts.web[443] == "" {
		t.Errorf("after -off: listeners = %v, want 8443 gone and 443 untouched", ts.web)
	}
	if !strings.Contains(out, "8443") {
		t.Errorf("-off output does not name what it removed:\n%s", out)
	}
	runRemoteConsole(t, dp, "-off")
}

// TestRemoteConsoleLeavesWhatItDidNotSetUp: a gate token that was there
// before the command stays after -off, and a listener port someone has since
// pointed elsewhere is neither removed nor reused.
func TestRemoteConsoleLeavesWhatItDidNotSetUp(t *testing.T) {
	dp, ts, cfgDir, _ := remoteConsoleFixture(t, "127.0.0.1:8097")
	gate := gateTokenSource(filepath.Join(cfgDir, "config.gate-token"))
	runGateToken(t, dp)
	mine := gate()
	runRemoteConsole(t, dp)
	if gate() != mine {
		t.Error("remote-console replaced a usable gate token")
	}
	// The operator points 8443 at another app by hand.
	ts.web[8443] = "http://127.0.0.1:5000"
	var out bytes.Buffer
	err := remoteConsoleMain(dp, &out, []string{"-off"})
	if err == nil || !strings.Contains(err.Error(), "left alone") {
		t.Errorf("-off with the port re-pointed by hand: err = %v, want it to say the listener is left alone", err)
	}
	if ts.web[8443] != "http://127.0.0.1:5000" {
		t.Errorf("-off removed a listener it did not set up: %v", ts.web)
	}
	if gate() != mine {
		t.Errorf("-off removed a gate token it did not write: %+v", gate())
	}
	if len(remoteConsoleHostSource(filepath.Join(cfgDir, "config.remote-console"))()) != 0 {
		t.Error("-off left the host accepted")
	}

	// On again: 8443 is someone else's now, so the next free port is taken,
	// and moving on from it later does not touch 8443 either.
	runRemoteConsole(t, dp)
	if ts.web[8444] != "http://127.0.0.1:8097" || ts.web[8443] != "http://127.0.0.1:5000" {
		t.Errorf("listeners = %v, want the console on 8444 and 8443 untouched", ts.web)
	}
}

// TestRemoteConsoleKeepsItsAddressWhenServeMoves: the listener this command
// recorded is re-pointed when the profile's serve comes back on another
// port, so the other machine's address does not change.
func TestRemoteConsoleKeepsItsAddressWhenServeMoves(t *testing.T) {
	dp, ts, cfgDir, dataDir := remoteConsoleFixture(t, "127.0.0.1:8097")
	path := filepath.Join(cfgDir, "config.remote-console")
	runRemoteConsole(t, dp)
	remove, err := consolelink.RecordServeAddress(dataDir, "127.0.0.1:8123")
	if err != nil {
		t.Fatal(err)
	}
	defer remove()
	out := runRemoteConsole(t, dp)
	if ts.web[8443] != "http://127.0.0.1:8123" || len(ts.web) != 2 {
		t.Errorf("listeners = %v, want 8443 re-pointed at the serve's new address and no second listener", ts.web)
	}
	if rc, ok := readRemoteConsole(path); !ok || rc.Host != fakeTailnetName+":8443" || rc.Backend != "127.0.0.1:8123" || !rc.WroteToken {
		t.Errorf("file = %+v (ok %v), want the same host with the new backend", rc, ok)
	}
	if !strings.Contains(out, "https://"+fakeTailnetName+":8443/") {
		t.Errorf("the address changed:\n%s", out)
	}

	// Moving to a port of the operator's choice removes the old listener.
	runRemoteConsole(t, dp, "-https-port", "9443")
	if _, old := ts.web[8443]; old || ts.web[9443] != "http://127.0.0.1:8123" {
		t.Errorf("listeners = %v, want the listener moved from 8443 to 9443", ts.web)
	}
}

// TestRemoteConsoleNeverTakesAPortThatServesSomethingElse covers the choice
// of port, from the listeners as the tailscale CLI reports them.
func TestRemoteConsoleNeverTakesAPortThatServesSomethingElse(t *testing.T) {
	const backend = "127.0.0.1:8097"
	none := remoteConsole{}
	cases := []struct {
		name      string
		listeners map[int]string
		wanted    int
		previous  remoteConsole
		want      int
		wantErr   string
	}{
		{"nothing listens", map[int]string{}, 0, none, 443, ""},
		{"443 taken", map[int]string{443: "127.0.0.1:3000"}, 0, none, 8443, ""},
		{"443 serves something else", map[int]string{443: ""}, 0, none, 8443, ""},
		{"already forwarding on 8444", map[int]string{443: "127.0.0.1:3000", 8444: backend}, 0, none, 8444, ""},
		{"wanted and free", map[int]string{443: "127.0.0.1:3000"}, 9443, none, 9443, ""},
		{"wanted and ours", map[int]string{9443: backend}, 9443, none, 9443, ""},
		{"wanted and taken", map[int]string{443: "127.0.0.1:3000"}, 443, none, 0, "already serves http://127.0.0.1:3000"},
		{"wanted is a Funnel port forwarding here", map[int]string{8443: listenerFunnel}, 8443, none, 0, "open to the internet"},
		{"a Funnel port is never reused", map[int]string{443: listenerFunnel}, 0, none, 8443, ""},
		{"the recorded port, serve moved", map[int]string{8444: "127.0.0.1:7000"}, 0, remoteConsole{Listener: 8444, Backend: "127.0.0.1:7000"}, 8444, ""},
		{"the recorded port, re-pointed by hand", map[int]string{8443: "127.0.0.1:5000"}, 0, remoteConsole{Listener: 8443, Backend: "127.0.0.1:7000"}, 443, ""},
		{"every default taken", map[int]string{443: "", 8443: "", 8444: "", 8445: "", 8446: "", 8447: "", 8448: "", 8449: ""}, 0, none, 0, "-https-port"},
	}
	for _, tc := range cases {
		got, err := chooseRemotePort(tc.listeners, backend, tc.wanted, tc.previous)
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

	// What the CLI's JSON becomes: only a port whose one handler is "/" of
	// this name, forwarding to a loopback address, is a console listener.
	dp, ts, cfgDir, _ := remoteConsoleFixture(t, backend)
	ts.web = map[int]string{443: "http://127.0.0.1:3000", 8443: "http://localhost:8097/", 8444: "http://" + backend, 8445: "http://" + backend, 8446: "http://" + backend, 8447: "https://example.com"}
	ts.otherPaths[8444], ts.otherName[8445], ts.funnel[8446], ts.tcp[8448] = true, true, true, true
	listeners, err := tailscaleListeners(context.Background(), dp, fakeTailnetName)
	if err != nil {
		t.Fatal(err)
	}
	want := map[int]string{443: "127.0.0.1:3000", 8443: backend, 8444: "", 8445: "", 8446: listenerFunnel, 8447: "", 8448: ""}
	if fmt.Sprint(listeners) != fmt.Sprint(want) {
		t.Errorf("listeners = %v, want %v", listeners, want)
	}

	ts.web = map[int]string{443: "http://127.0.0.1:3000"}
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

// TestRemoteConsoleSaysWhatIsAndIsNotInPlaceWhenAStepFails: each failure
// leaves nothing half open and names what the operator has to do.
func TestRemoteConsoleSaysWhatIsAndIsNotInPlaceWhenAStepFails(t *testing.T) {
	dp, ts, cfgDir, _ := remoteConsoleFixture(t, "127.0.0.1:8097")
	path := filepath.Join(cfgDir, "config.remote-console")
	hosts := remoteConsoleHostSource(path)
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
	// The listener cannot be added: no host is recorded.
	ts.fail = "serve --bg"
	if err := remoteConsoleMain(dp, &out, nil); err == nil || !strings.Contains(err.Error(), "nothing else was changed") || len(hosts()) != 0 {
		t.Errorf("listener refused: err = %v, hosts %v; want an error and no host recorded", err, hosts())
	}
	// A disabled gate is not replaced, and nothing is opened behind it.
	ts.fail = ""
	runGateToken(t, dp, "-disable")
	if err := remoteConsoleMain(dp, &out, nil); err == nil || !strings.Contains(err.Error(), "-rotate") {
		t.Errorf("disabled gate: err = %v, want one naming -rotate", err)
	}
	if len(hosts()) != 0 || len(ts.web) != 1 {
		t.Errorf("disabled gate: hosts %v, listeners %v; want nothing opened", hosts(), ts.web)
	}
	runGateToken(t, dp, "-rotate")

	runRemoteConsole(t, dp)
	ts.fail = "serve --https=8443 off"
	err := remoteConsoleMain(dp, &out, []string{"-off"})
	if err == nil || !strings.Contains(err.Error(), "tailscale serve --https=8443 off") {
		t.Errorf("-off with a listener that will not go: err = %v, want the command to run", err)
	}
	if len(hosts()) != 0 {
		t.Error("-off left the host accepted when the listener would not go")
	}
	for _, args := range [][]string{{"extra"}, {"-ttl", "1s"}, {"-https-port", "70000"}} {
		if err := remoteConsoleMain(dp, &out, args); err == nil {
			t.Errorf("remote-console %v: want an error", args)
		}
	}
}

// TestRemoteConsoleStartsNoServeWhenAutostartIsOff: with FACTORYD_AUTOSTART=0
// and no serve for the data dir, the command says how to start one and
// opens nothing.
func TestRemoteConsoleStartsNoServeWhenAutostartIsOff(t *testing.T) {
	dp, ts, cfgDir, _ := remoteConsoleFixture(t, "")
	t.Setenv("FACTORYD_AUTOSTART", "0")
	var out bytes.Buffer
	err := remoteConsoleMain(dp, &out, nil)
	if err == nil || !strings.Contains(err.Error(), "factoryd console") {
		t.Fatalf("err = %v, want one naming `factoryd console`", err)
	}
	if len(ts.web) != 1 || gateTokenSource(filepath.Join(cfgDir, "config.gate-token"))().On {
		t.Errorf("something was opened with no serve: listeners %v", ts.web)
	}
}

// TestRemoteConsoleFileThatCannotBeTrustedAllowsNoHost: `serve` allows a
// host only on the word of a file with the token files' checks and exactly
// its four lines, each well formed.
func TestRemoteConsoleFileThatCannotBeTrustedAllowsNoHost(t *testing.T) {
	valid := remoteConsole{Host: fakeTailnetName + ":8443", Listener: 8443, Backend: "127.0.0.1:8097", WroteToken: true}.content()
	swap := func(old, new string) func(t *testing.T, path string) {
		return func(t *testing.T, path string) {
			changed := strings.Replace(valid, old, new, 1)
			if changed == valid {
				t.Fatalf("%q is not in the valid file", old)
			}
			writeFileMode(t, path, changed, 0o600)
		}
	}
	cases := map[string]func(t *testing.T, path string){
		"readable by others": func(t *testing.T, path string) { writeFileMode(t, path, valid, 0o644) },
		"a symlink": func(t *testing.T, path string) {
			target := filepath.Join(t.TempDir(), "elsewhere")
			writeFileMode(t, target, valid, 0o600)
			if err := os.Symlink(target, path); err != nil {
				t.Fatal(err)
			}
		},
		"no host line":          swap("host "+fakeTailnetName+":8443\n", ""),
		"a URL as host":         swap("host ", "host https://"),
		"a wildcard host":       swap(fakeTailnetName, "*.ts.net"),
		"an upper-case host":    swap(fakeTailnetName, "Builder.example-tailnet.ts.net"),
		"a host with no port":   swap(":8443\n", ":\n"),
		"the loopback address":  swap(fakeTailnetName+":8443", "127.0.0.1:8097"),
		"localhost":             swap(fakeTailnetName, "localhost"),
		"an unknown line":       swap("gate-token written\n", "gate-token written\nhost2 other.example\n"),
		"the host line twice":   swap("listener ", "host other.example\nlistener "),
		"two hosts on a line":   swap(":8443\n", ":8443 other.example\n"),
		"a backend elsewhere":   swap("backend 127.0.0.1:8097", "backend 10.0.0.5:8097"),
		"no gate-token line":    swap("gate-token written\n", ""),
		"a gate-token of maybe": swap("gate-token written", "gate-token maybe"),
		"empty":                 func(t *testing.T, path string) { writeFileMode(t, path, "", 0o600) },
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
