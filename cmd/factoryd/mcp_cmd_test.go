package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"buildgate/internal/consolelink"
)

// mcpTestConfig writes a session config under a temp HOME and returns its
// directory and data dir.
func mcpTestConfig(t *testing.T) (cfgDir, dataDir string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "xdg"))
	cfgDir = filepath.Join(home, "xdg", "factoryd")
	dataDir = filepath.Join(home, "data")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "config.yml"), []byte("data_dir: "+dataDir+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return cfgDir, dataDir
}

func runMCP(t *testing.T, dp *deps, args ...string) string {
	t.Helper()
	var out bytes.Buffer
	if err := remoteMain(dp, &out, append([]string{"mcp"}, args...)); err != nil {
		t.Fatalf("remote mcp %v: %v", args, err)
	}
	return out.String()
}

// TestMCPMainCreatesRotatesAndRemovesTheToken: the token file is what turns
// the endpoint on, so each form of the command is checked through the
// source `serve` reads it with.
func TestMCPMainCreatesRotatesAndRemovesTheToken(t *testing.T) {
	dp := newTestDeps(t)
	cfgDir, dataDir := mcpTestConfig(t)
	tokenPath := filepath.Join(cfgDir, "config.mcp-token")
	if got := mcpTokenPathFor(filepath.Join(cfgDir, "config.yml")); got != tokenPath {
		t.Fatalf("token path = %s, want %s", got, tokenPath)
	}
	source := mcpTokenSource(tokenPath)
	if got := source(); got != "" {
		t.Fatalf("before `factoryd remote mcp`: token source = %q, want the endpoint off", got)
	}

	out := runMCP(t, dp)
	first := source()
	if first == "" {
		t.Fatalf("after `factoryd remote mcp`: no usable token at %s", tokenPath)
	}
	info, err := os.Stat(tokenPath)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("token file mode = %v (err %v), want 0600", info.Mode().Perm(), err)
	}
	// No serve recorded for this data dir: the token is not printed beside
	// the default address, which another data dir's serve may hold.
	if !strings.Contains(out, "No `factoryd serve` is recorded") || strings.Contains(out, first) || strings.Contains(out, consolelink.DefaultServeAddr) {
		t.Errorf("with no serve recorded, output should name neither the token nor an address:\n%s", out)
	}

	if runMCP(t, dp); source() != first {
		t.Errorf("a second `factoryd remote mcp` changed the token")
	}

	fakeHostOf(dp).tcpListeningFn = func(string) bool { return true }
	remove, err := consolelink.RecordServeAddress(dataDir, "127.0.0.1:8097")
	if err != nil {
		t.Fatal(err)
	}
	defer remove()
	out = runMCP(t, dp, "-rotate")
	second := source()
	if second == "" || second == first {
		t.Errorf("-rotate: token = %q, want a new one (was %q)", second, first)
	}
	if !strings.Contains(out, "http://127.0.0.1:8097/mcp") || !strings.Contains(out, second) || strings.Contains(out, first) || !strings.Contains(out, "cannot approve") {
		t.Errorf("-rotate output should name the recorded serve and only the new token:\n%s", out)
	}

	// A rotation that cannot write the new token leaves the old one working.
	dp.forge.(*fakeForge).insideGitWorkTreeFn = func(string) bool { return true }
	var discard bytes.Buffer
	if err := remoteMain(dp, &discard, []string{"mcp", "-rotate"}); err == nil {
		t.Error("-rotate with the token directory unwritable: want an error")
	}
	if got := source(); got != second {
		t.Errorf("after a failed -rotate: token = %q, want the working one kept", got)
	}
	dp.forge.(*fakeForge).insideGitWorkTreeFn = func(string) bool { return false }

	runMCP(t, dp, "-remove")
	if got := source(); got != "" {
		t.Errorf("after -remove: token source = %q, want the endpoint off", got)
	}
	runMCP(t, dp, "-remove")
}

// TestMCPTokenIsPerProfile: profiles share one directory, and the endpoint
// turned on for one profile's serve stays off for another's.
func TestMCPTokenIsPerProfile(t *testing.T) {
	dp := newTestDeps(t)
	cfgDir, _ := mcpTestConfig(t)
	other := filepath.Join(cfgDir, "office.yml")
	if err := os.WriteFile(other, []byte("data_dir: "+filepath.Join(t.TempDir(), "data")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runMCP(t, dp, "-config", other)
	if got := mcpTokenSource(mcpTokenPathFor(other))(); got == "" {
		t.Error("the profile `factoryd remote mcp -config` named has no token")
	}
	if got := mcpTokenSource(mcpTokenPathFor(filepath.Join(cfgDir, "config.yml")))(); got != "" {
		t.Errorf("the default profile's endpoint was turned on too (token %q)", got)
	}
}

func TestMCPMainRefusesBadInvocations(t *testing.T) {
	dp := newTestDeps(t)
	cfgDir, _ := mcpTestConfig(t)
	var out bytes.Buffer
	missing := filepath.Join(cfgDir, "typo", "config.yml")
	if err := remoteMain(dp, &out, []string{"mcp", "-config", missing}); err == nil {
		t.Error("-config naming no file: want an error")
	}
	if _, err := os.Stat(filepath.Dir(missing)); err == nil {
		t.Error("-config naming no file created its directory")
	}
	if err := remoteMain(dp, &out, []string{"mcp", "-rotate", "-remove"}); err == nil {
		t.Error("-rotate -remove: want an error")
	}
	if err := remoteMain(dp, &out, []string{"mcp", "extra"}); err == nil {
		t.Error("a positional argument: want an error")
	}
}

// TestMCPTokenSourceRefusesAnExposedFile: a token file readable beyond its
// owner leaves the endpoint off.
func TestMCPTokenSourceRefusesAnExposedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.mcp-token")
	if err := os.WriteFile(path, []byte("secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := mcpTokenSource(path)(); got != "" {
		t.Errorf("token source = %q for a 0644 file, want the endpoint off", got)
	}
}

// TestRemoteMCPRefusesATokenOnlyFlag: -disable belongs to `remote token`.
func TestRemoteMCPRefusesATokenOnlyFlag(t *testing.T) {
	dp := newTestDeps(t)
	mcpTestConfig(t)
	var out bytes.Buffer
	if err := remoteMain(dp, &out, []string{"mcp", "-disable"}); err == nil || !strings.Contains(err.Error(), "takes no -disable") {
		t.Errorf("remote mcp -disable: err = %v, want it refused", err)
	}
}
