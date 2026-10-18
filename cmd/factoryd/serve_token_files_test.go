package main

import (
	"bytes"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"buildgate/internal/sessionconfig"
)

// serveTokenFilesHome isolates HOME and XDG_CONFIG_HOME with one session
// config, and returns its directory and the data dir it names.
func serveTokenFilesHome(t *testing.T) (cfgDir, dataDir string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "xdg"))
	t.Setenv(sessionconfig.ProfileEnv, "")
	cfgDir = filepath.Join(home, "xdg", "factoryd")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	dataDir = filepath.Join(home, "data")
	if err := os.WriteFile(filepath.Join(cfgDir, "config.yml"), []byte("data_dir: "+dataDir+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(overrideTokenEnvironmentVariable, "")
	t.Setenv(startTokenEnvironmentVariable, "")
	t.Setenv(readTokenEnvironmentVariable, "")
	return cfgDir, dataDir
}

// serveMainUntilBind runs serveMain against an address already in use, so it
// stops after resolving its tokens, and returns its log and error.
func serveMainUntilBind(t *testing.T, dataDir string) (string, error) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a test listener: %v", err)
	}
	t.Cleanup(func() { listener.Close() })
	var logBuf bytes.Buffer
	previousOutput := log.Writer()
	log.SetOutput(&logBuf)
	t.Cleanup(func() { log.SetOutput(previousOutput) })
	t.Cleanup(func() { tier2SettingsOverride = nil })
	err = serveMain(newTestDeps(t), []string{"-data-dir", dataDir, "-addr", listener.Addr().String()})
	return logBuf.String(), err
}

func TestServeReadsTheOverrideAndReadTokenFiles(t *testing.T) {
	cfgDir, dataDir := serveTokenFilesHome(t)
	for _, name := range []string{serveOverrideTokenFileName, serveReadTokenFileName} {
		if err := os.WriteFile(filepath.Join(cfgDir, name), []byte("token-from-"+name+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	logs, err := serveMainUntilBind(t, dataDir)
	if err == nil || strings.Contains(err.Error(), "refusing to start") {
		t.Fatalf("serveMain = %v, want it to stop only at the bind", err)
	}
	if !strings.Contains(logs, "(override endpoint enabled)") {
		t.Fatalf("serve did not take the override token from its file:\n%s", logs)
	}
	if strings.Contains(logs, "token-from-") {
		t.Fatalf("serve log leaked a token value:\n%s", logs)
	}
}

func TestServeTokensFromFilesFillsEveryToken(t *testing.T) {
	cfgDir, _ := serveTokenFilesHome(t)
	for _, name := range []string{serveOverrideTokenFileName, serveReadTokenFileName, serveConsoleTokenFileName} {
		if err := os.WriteFile(filepath.Join(cfgDir, name), []byte("from-"+name+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	configPath := filepath.Join(cfgDir, "config.yml")
	sv := &serveRun{sf: serveFlags{configPath: &configPath}}
	if err := sv.tokensFromFiles(); err != nil {
		t.Fatal(err)
	}
	if sv.overrideToken != "from-"+serveOverrideTokenFileName || sv.readToken != "from-"+serveReadTokenFileName || sv.consoleToken != "from-"+serveConsoleTokenFileName {
		t.Fatalf("override %q, read %q, console %q: want each from its own file", sv.overrideToken, sv.readToken, sv.consoleToken)
	}
}

func TestServeTokenFilesDoNotReplaceTheEnvironment(t *testing.T) {
	cfgDir, _ := serveTokenFilesHome(t)
	if err := os.WriteFile(filepath.Join(cfgDir, serveReadTokenFileName), []byte("from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(cfgDir, "config.yml")
	sv := &serveRun{sf: serveFlags{configPath: &configPath}, readToken: "from-env"}
	if err := sv.tokensFromFiles(); err != nil {
		t.Fatal(err)
	}
	if sv.readToken != "from-env" || sv.overrideToken != "" {
		t.Fatalf("override %q, read %q: want the environment's read token and no override token", sv.overrideToken, sv.readToken)
	}
}

func TestServeRefusesToStartOnATokenFileItCannotUse(t *testing.T) {
	cfgDir, dataDir := serveTokenFilesHome(t)
	path := filepath.Join(cfgDir, serveOverrideTokenFileName)
	if err := os.WriteFile(path, []byte("loose\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := serveMainUntilBind(t, dataDir)
	if err == nil || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "install-service -force -require-tokens") {
		t.Fatalf("serveMain = %v, want a refusal naming %s and how to write a new one", err, path)
	}
}
