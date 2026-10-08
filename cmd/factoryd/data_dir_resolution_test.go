package main

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"buildgate/internal/sessionconfig"
)

// captureLog collects what the standard logger prints during the test.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	return &buf
}

// profileWithoutDataDir isolates HOME and the config dir and writes a
// default profile that sets no data_dir.
func profileWithoutDataDir(t *testing.T) (configPath string) {
	t.Helper()
	configPath = isolateSessionConfig(t)
	t.Setenv(sessionconfig.ProfileEnv, "")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte("registry_proxy: false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return configPath
}

// A real command, run from two directories against a profile that sets no
// data_dir, reads the same records: the profile's default data dir.
func TestACommandMeansTheSameDataDirFromAnyDirectory(t *testing.T) {
	profileWithoutDataDir(t)
	want := sessionconfig.DefaultDataDir()
	logged := captureLog(t)
	for _, dir := range []string{t.TempDir(), t.TempDir()} {
		t.Chdir(dir)
		logged.Reset()
		if err := statusMain(nil); err != nil {
			t.Fatalf("status from %s: %v", dir, err)
		}
		if !strings.Contains(logged.String(), `data dir: "`+want+`" (source: the default for session config`) {
			t.Errorf("status from %s logged %q, want data dir %s", dir, logged.String(), want)
		}
		if _, err := os.Stat(filepath.Join(dir, "data")); err == nil {
			t.Errorf("status created ./data in %s", dir)
		}
	}
}

// Records in ./data of the working directory that the default no longer
// reaches are pointed out, with the line that keeps using them.
func TestTheDefaultDataDirNamesRecordsLeftInTheWorkingDirectory(t *testing.T) {
	configPath := profileWithoutDataDir(t)
	dir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	if err := os.MkdirAll(filepath.Join(dir, "data", "requests"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	logged := captureLog(t)
	if err := statusMain(nil); err != nil {
		t.Fatal(err)
	}
	local := filepath.Join(dir, "data")
	for _, want := range []string{"note: " + local + " holds factoryd records", "data_dir: " + local, configPath} {
		if !strings.Contains(logged.String(), want) {
			t.Errorf("log %q lacks %q", logged.String(), want)
		}
	}
	// A config that sets data_dir made its choice: nothing to point out.
	if err := os.WriteFile(configPath, []byte("data_dir: "+t.TempDir()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	logged.Reset()
	if err := statusMain(nil); err != nil || strings.Contains(logged.String(), "note:") {
		t.Errorf("with data_dir set: %v, log %q", err, logged.String())
	}
}

// The commands that find a running worker through each profile's data dir
// (restart, stop -all, upgrade) find the one of a profile that sets none.
func TestAProfileWithNoDataDirHasItsWorkerFound(t *testing.T) {
	dp := newTestDeps(t)
	stubStopSeams(dp, t)
	profileWithoutDataDir(t)
	dataDir := sessionconfig.DefaultDataDir()
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	worker := standIn(t)
	writePIDFile(t, dataDir, "quickstart-queue-run.pid", worker.Process.Pid)
	t.Chdir(t.TempDir())
	profiles, err := loadProfiles()
	if err != nil {
		t.Fatal(err)
	}
	dirs, _ := distinctDataDirs(profiles)
	running := upgradeRunning(dp, profiles, dirs, time.Now())
	if len(running) != 1 || running[0].Kind != "worker" || running[0].DataDir != dataDir {
		t.Fatalf("running = %+v, want the worker of %s", running, dataDir)
	}
}

// With no session config at all the flag's relative default stays, and the
// command says that it follows the working directory.
func TestWithNoSessionConfigTheRelativeDefaultIsNamed(t *testing.T) {
	isolateSessionConfig(t)
	t.Chdir(t.TempDir())
	logged := captureLog(t)
	if err := statusMain(nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logged.String(), "warning: there is no session config") || !strings.Contains(logged.String(), "follows the directory") {
		t.Errorf("log %q, want the warning", logged.String())
	}
}
