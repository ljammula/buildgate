package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"buildgate/internal/api"
)

func runGateToken(t *testing.T, dp *deps, args ...string) string {
	t.Helper()
	var out bytes.Buffer
	if err := gateTokenMain(dp, &out, args); err != nil {
		t.Fatalf("gateTokenMain %v: %v", args, err)
	}
	return out.String()
}

// TestGateTokenMainCreatesAndRotates: each form of the command is checked
// through the source `serve` reads the file with.
func TestGateTokenMainCreatesAndRotates(t *testing.T) {
	dp := newTestDeps(t)
	cfgDir, _ := mcpTestConfig(t)
	path := filepath.Join(cfgDir, "config.gate-token")
	if got := gateTokenPathFor(filepath.Join(cfgDir, "config.yml")); got != path {
		t.Fatalf("token path = %s, want %s", got, path)
	}
	source := gateTokenSource(path)
	if got := source(); got != (api.GateToken{}) {
		t.Fatalf("before the command: %+v, want the gate off", got)
	}

	out := runGateToken(t, dp)
	first := source()
	if !first.On || first.Token == "" {
		t.Fatalf("after `factoryd gate-token`: %+v, want the gate on with a token", first)
	}
	if info, err := os.Lstat(path); err != nil || info.Mode().Perm() != 0o600 || !info.Mode().IsRegular() {
		t.Fatalf("token file = %v (err %v), want a regular file of mode 0600", info.Mode(), err)
	}
	for _, want := range []string{first.Token, "#gate=" + first.Token, "Expires:", "cannot override"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if file := readGateFile(path, time.Now()); file.expires.Sub(time.Now()).Round(time.Hour) != gateDefaultTTL {
		t.Errorf("expires in %s, want the default %s", time.Until(file.expires), gateDefaultTTL)
	}
	if runGateToken(t, dp); source() != first {
		t.Error("a second `factoryd gate-token` changed the token")
	}

	out = runGateToken(t, dp, "-rotate", "-ttl", "30m")
	second := source()
	if second.Token == "" || second.Token == first.Token || strings.Contains(out, first.Token) {
		t.Errorf("-rotate: token = %q (was %q), output:\n%s", second.Token, first.Token, out)
	}
	if file := readGateFile(path, time.Now()); time.Until(file.expires) > 30*time.Minute || time.Until(file.expires) < 29*time.Minute {
		t.Errorf("-ttl 30m: expires in %s", time.Until(file.expires))
	}

	// A rotation that cannot write the new token leaves the old one working.
	dp.forge.(*fakeForge).insideGitWorkTreeFn = func(string) bool { return true }
	var discard bytes.Buffer
	if err := gateTokenMain(dp, &discard, []string{"-rotate"}); err == nil {
		t.Error("-rotate with the token directory refused: want an error")
	}
	if got := source(); got != second {
		t.Errorf("after a failed -rotate: %+v, want the working token kept", got)
	}
	dp.forge.(*fakeForge).insideGitWorkTreeFn = func(string) bool { return false }

}

// TestGateTokenMainDisablesAndRemoves: -disable closes the remote console and
// keeps the gate on, -remove turns the gate off, and the forms that make no
// sense together are refused.
func TestGateTokenMainDisablesAndRemoves(t *testing.T) {
	dp := newTestDeps(t)
	cfgDir, _ := mcpTestConfig(t)
	source := gateTokenSource(filepath.Join(cfgDir, "config.gate-token"))
	var discard bytes.Buffer
	runGateToken(t, dp)

	out := runGateToken(t, dp, "-disable")
	if got := source(); got != (api.GateToken{On: true}) {
		t.Errorf("after -disable: %+v, want the gate on with no token", got)
	}
	if !strings.Contains(out, "neither read nor write") {
		t.Errorf("-disable output does not say what it closes:\n%s", out)
	}
	if err := gateTokenMain(dp, &discard, nil); err == nil || !strings.Contains(err.Error(), "-rotate") {
		t.Errorf("the plain command on a disabled gate = %v, want a refusal naming -rotate", err)
	}
	if runGateToken(t, dp, "-rotate"); source().Token == "" {
		t.Error("-rotate after -disable wrote no token")
	}

	out = runGateToken(t, dp, "-remove")
	if got := source(); got != (api.GateToken{}) {
		t.Errorf("after -remove: %+v, want the gate off", got)
	}
	if !strings.Contains(out, "reads with no token again") {
		t.Errorf("-remove output does not say reads are open again:\n%s", out)
	}
	runGateToken(t, dp, "-remove")

	for _, args := range [][]string{{"-rotate", "-disable"}, {"-disable", "-remove"}, {"-ttl", "10s"}, {"-ttl", "1000h"}, {"extra"}} {
		if err := gateTokenMain(dp, &discard, args); err == nil {
			t.Errorf("gate-token %v: want an error", args)
		}
	}
}

// TestGateFileThatCannotBeUsedLeavesTheGateOnWithNoToken: a file that goes
// bad must not turn the gate off, which would reopen the reads it guards.
func TestGateFileThatCannotBeUsedLeavesTheGateOnWithNoToken(t *testing.T) {
	dp := newTestDeps(t)
	now := time.Now()
	valid := "tok-abc\n" + gateExpiresPrefix + now.Add(time.Hour).UTC().Format(time.RFC3339) + "\n"
	cases := map[string]func(t *testing.T, path string){
		"readable by others": func(t *testing.T, path string) { writeFileMode(t, path, valid, 0o644) },
		"a symlink": func(t *testing.T, path string) {
			target := filepath.Join(t.TempDir(), "elsewhere")
			writeFileMode(t, target, valid, 0o600)
			if err := os.Symlink(target, path); err != nil {
				t.Fatal(err)
			}
		},
		"a directory": func(t *testing.T, path string) {
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
		},
		"expired": func(t *testing.T, path string) {
			writeFileMode(t, path, "tok-abc\n"+gateExpiresPrefix+now.Add(-time.Minute).UTC().Format(time.RFC3339)+"\n", 0o600)
		},
		"no expiry line": func(t *testing.T, path string) { writeFileMode(t, path, "tok-abc\n", 0o600) },
		"an expiry that does not parse": func(t *testing.T, path string) {
			writeFileMode(t, path, "tok-abc\nexpires tomorrow\n", 0o600)
		},
		"two tokens on a line": func(t *testing.T, path string) {
			writeFileMode(t, path, "tok abc\n"+gateExpiresPrefix+now.Add(time.Hour).UTC().Format(time.RFC3339)+"\n", 0o600)
		},
		"disabled": func(t *testing.T, path string) { writeFileMode(t, path, gateDisabledLine+"\n", 0o600) },
	}
	for name, write := range cases {
		t.Run(name, func(t *testing.T) {
			cfgDir, _ := mcpTestConfig(t)
			path := filepath.Join(cfgDir, "config.gate-token")
			write(t, path)
			if got := gateTokenSource(path)(); got != (api.GateToken{On: true}) {
				t.Errorf("source = %+v, want the gate on with no token", got)
			}
			checks := doctorGateTokenChecks(filepath.Join(cfgDir, "config.yml"), now)
			if len(checks) != 1 || checks[0].Err == nil || !strings.Contains(checks[0].Err.Error(), "neither read nor write") || checks[0].Fix == "" {
				t.Errorf("doctor = %+v, want one row saying the remote console is closed and how to fix it", checks)
			}
			// The plain command replaces only an expired token; it never
			// writes over a file it cannot trust, or a disabled gate.
			var out bytes.Buffer
			err := gateTokenMain(dp, &out, nil)
			if name == "expired" {
				if err != nil || gateTokenSource(path)().Token == "" {
					t.Errorf("the plain command on an expired token: err %v, source %+v; want a new token", err, gateTokenSource(path)())
				}
				return
			}
			if err == nil {
				t.Errorf("the plain command wrote over the file:\n%s", out.String())
			}
		})
	}
	// A usable file and an absent one have no doctor row.
	cfgDir, _ := mcpTestConfig(t)
	if checks := doctorGateTokenChecks(filepath.Join(cfgDir, "config.yml"), now); len(checks) != 0 {
		t.Errorf("no gate file: doctor = %+v, want no row", checks)
	}
	runGateToken(t, dp)
	if checks := doctorGateTokenChecks(filepath.Join(cfgDir, "config.yml"), now); len(checks) != 0 {
		t.Errorf("a usable gate file: doctor = %+v, want no row", checks)
	}
	if got := gateTokenSource(filepath.Join(cfgDir, "config.gate-token"))(); !got.On || got.Token == "" {
		t.Errorf("a usable gate file: source = %+v", got)
	}
}

// TestGateTokenIsPerProfile: profiles share one directory, and a gate turned
// on for one profile's serve stays off for another's.
func TestGateTokenIsPerProfile(t *testing.T) {
	dp := newTestDeps(t)
	cfgDir, _ := mcpTestConfig(t)
	other := filepath.Join(cfgDir, "office.yml")
	if err := os.WriteFile(other, []byte("data_dir: "+filepath.Join(t.TempDir(), "data")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGateToken(t, dp, "-config", other)
	if got := gateTokenSource(gateTokenPathFor(other))(); got.Token == "" {
		t.Error("the profile `factoryd gate-token -config` named has no token")
	}
	if got := gateTokenSource(gateTokenPathFor(filepath.Join(cfgDir, "config.yml")))(); got.On {
		t.Errorf("the default profile's gate was turned on too: %+v", got)
	}
	if got := mcpTokenSource(mcpTokenPathFor(other))(); got != "" {
		t.Errorf("creating a gate token turned the MCP endpoint on (token %q)", got)
	}
}

func writeFileMode(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}
