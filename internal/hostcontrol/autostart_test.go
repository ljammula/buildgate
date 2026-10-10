package hostcontrol

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"buildgate"
)

// stubTemporalProbe makes the Temporal health probe answer healthy() and
// shortens the start wait, so no test dials a server or waits minutes.
func stubTemporalProbe(dp *fakeDeps, t *testing.T, healthy func() bool) {
	t.Helper()
	oldHealthy, oldDocker := dp.temporalHealthyFn, dp.DockerBinary()
	oldTimeout, oldPoll := TemporalStartTimeout, TemporalPollInterval
	dp.temporalHealthyFn = func(context.Context, string) error {
		if healthy() {
			return nil
		}
		return errors.New("connection refused")
	}
	TemporalStartTimeout, TemporalPollInterval = 200*time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() {
		dp.temporalHealthyFn, dp.dockerBinaryFn = oldHealthy, func() string { return oldDocker }
		TemporalStartTimeout, TemporalPollInterval = oldTimeout, oldPoll
	})
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
}

// fakeAutostartDocker writes a stand-in `docker` that logs every invocation to
// a file and fails `info` when infoFails. Returns the script and log paths.
func fakeAutostartDocker(t *testing.T, infoFails bool) (script, logPath string) {
	t.Helper()
	dir := t.TempDir()
	script = filepath.Join(dir, "docker")
	logPath = filepath.Join(dir, "docker.log")
	body := "#!/bin/sh\necho \"$@\" >> " + logPath + "\n"
	if infoFails {
		body += "[ \"$1\" = info ] && { echo daemon down >&2; exit 1; }\n"
	}
	body += "exit 0\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return script, logPath
}

func TestAutostartEnabled(t *testing.T) {
	t.Setenv(AutostartEnvVar, "0")
	if AutostartEnabled() {
		t.Error("autostartEnabled with FACTORYD_AUTOSTART=0 = true")
	}
	t.Setenv(AutostartEnvVar, "1")
	if !AutostartEnabled() {
		t.Error("autostartEnabled with FACTORYD_AUTOSTART=1 = false")
	}
	os.Unsetenv(AutostartEnvVar)
	if !AutostartEnabled() {
		t.Error("autostartEnabled with the variable unset = false; want on by default")
	}
}

func TestStartTemporalReachableDoesNotTouchDocker(t *testing.T) {
	dp := newFakeDeps(t)
	t.Setenv(AutostartEnvVar, "1")
	stubTemporalProbe(dp, t, func() bool { return true })
	script, logPath := fakeAutostartDocker(t, false)
	dp.dockerBinaryFn = func() string { return script }
	var out bytes.Buffer
	if got := StartTemporal(dp, context.Background(), &out); got == "" {
		t.Fatalf("startTemporal = %q, want the default address", got)
	}
	if _, err := os.Stat(logPath); err == nil {
		t.Error("docker was invoked although Temporal was already reachable")
	}
	if !strings.Contains(out.String(), "Temporal detected at") {
		t.Errorf("output %q lacks the detected line", out.String())
	}
}

func TestStartTemporalAutostartOffRunsDirect(t *testing.T) {
	dp := newFakeDeps(t)
	t.Setenv(AutostartEnvVar, "0")
	stubTemporalProbe(dp, t, func() bool { return false })
	script, logPath := fakeAutostartDocker(t, false)
	dp.dockerBinaryFn = func() string { return script }
	var out bytes.Buffer
	if got := StartTemporal(dp, context.Background(), &out); got != "" {
		t.Fatalf("startTemporal = %q, want empty", got)
	}
	if _, err := os.Stat(logPath); err == nil {
		t.Error("docker was invoked with FACTORYD_AUTOSTART=0")
	}
	for _, want := range []string{"Temporal not available", "FACTORYD_AUTOSTART=0", "builds cannot run without Temporal"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output %q lacks %q", out.String(), want)
		}
	}
}

func TestStartTemporalDockerDownRunsDirect(t *testing.T) {
	dp := newFakeDeps(t)
	t.Setenv(AutostartEnvVar, "1")
	stubTemporalProbe(dp, t, func() bool { return false })
	script, logPath := fakeAutostartDocker(t, true)
	dp.dockerBinaryFn = func() string { return script }
	var out bytes.Buffer
	if got := StartTemporal(dp, context.Background(), &out); got != "" {
		t.Fatalf("startTemporal = %q, want empty", got)
	}
	if !strings.Contains(out.String(), "docker is not usable") {
		t.Errorf("output %q lacks the docker reason", out.String())
	}
	if log, _ := os.ReadFile(logPath); strings.Contains(string(log), "compose") {
		t.Errorf("compose ran although docker info failed:\n%s", log)
	}
}

func TestStartTemporalStartsEmbeddedComposeStack(t *testing.T) {
	dp := newFakeDeps(t)
	t.Setenv(AutostartEnvVar, "1")
	script, logPath := fakeAutostartDocker(t, false)
	stubTemporalProbe(dp, t, func() bool {
		log, _ := os.ReadFile(logPath)
		return strings.Contains(string(log), "up -d")
	})
	dp.dockerBinaryFn = func() string { return script }
	var out bytes.Buffer
	if got := StartTemporal(dp, context.Background(), &out); got == "" {
		t.Fatalf("startTemporal returned direct; output:\n%s", out.String())
	}
	composePath := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "factoryd", "temporal", "docker-compose.yml")
	written, err := os.ReadFile(composePath)
	if err != nil {
		t.Fatalf("compose file not written: %v", err)
	}
	if !bytes.Equal(written, buildgate.TemporalCompose) || len(written) == 0 {
		t.Error("written compose file differs from the embedded docker-compose.temporal.yml")
	}
	log, _ := os.ReadFile(logPath)
	if want := "compose -f " + composePath + " up -d"; !strings.Contains(string(log), want) {
		t.Errorf("docker log %q lacks %q", log, want)
	}
	for _, want := range []string{"starting Temporal (first start pulls images)", "Temporal started at"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output %q lacks %q", out.String(), want)
		}
	}
}

func TestStartTemporalNeverHealthyRunsDirect(t *testing.T) {
	dp := newFakeDeps(t)
	t.Setenv(AutostartEnvVar, "1")
	stubTemporalProbe(dp, t, func() bool { return false })
	script, _ := fakeAutostartDocker(t, false)
	dp.dockerBinaryFn = func() string { return script }
	var out bytes.Buffer
	if got := StartTemporal(dp, context.Background(), &out); got != "" {
		t.Fatalf("startTemporal = %q, want empty", got)
	}
	if !strings.Contains(out.String(), "not healthy") {
		t.Errorf("output %q lacks the timeout reason", out.String())
	}
}

func TestResolveTemporalAddress(t *testing.T) {
	dp := newFakeDeps(t)
	t.Setenv(AutostartEnvVar, "1")
	old := dp.ensureTemporalFn
	t.Cleanup(func() { dp.ensureTemporalFn = old })
	var calls int
	dp.ensureTemporalFn = func(context.Context, io.Writer) string { calls++; return "localhost:7233" }

	if addr, err := ResolveTemporalAddress(dp, context.Background(), "10.0.0.5:7233", io.Discard); err != nil || addr != "10.0.0.5:7233" || calls != 0 {
		t.Errorf("explicit: got (%q, %v) after %d checks, want the address forwarded with no check", addr, err, calls)
	}
	if addr, err := ResolveTemporalAddress(dp, context.Background(), "none", io.Discard); err != nil || addr != "none" || calls != 0 {
		t.Errorf("none is just a host name: got (%q, %v) after %d checks, want it forwarded unchanged", addr, err, calls)
	}
	if addr, err := ResolveTemporalAddress(dp, context.Background(), "", io.Discard); err != nil || addr != "localhost:7233" || calls != 1 {
		t.Errorf("auto: got (%q, %v) after %d checks, want the ensured address", addr, err, calls)
	}
	dp.ensureTemporalFn = func(context.Context, io.Writer) string { return "" }
	if addr, err := ResolveTemporalAddress(dp, context.Background(), "", io.Discard); err == nil || addr != "" || !strings.Contains(err.Error(), "builds run only on Temporal") {
		t.Errorf("auto without Temporal: got (%q, %v), want an error saying builds run only on Temporal", addr, err)
	}
	// FACTORYD_AUTOSTART=0: nothing is selected automatically and Temporal
	// is not even probed.
	t.Setenv(AutostartEnvVar, "0")
	calls = 0
	dp.ensureTemporalFn = func(context.Context, io.Writer) string { calls++; return "localhost:7233" }
	_, err := ResolveTemporalAddress(dp, context.Background(), "", io.Discard)
	if want := "no Temporal address: pass -temporal-address (FACTORYD_AUTOSTART=0 starts nothing)"; err == nil || err.Error() != want || calls != 0 {
		t.Errorf("autostart off: got error %v after %d checks, want %q with no check", err, calls, want)
	}
}

func TestWorkerSpawnArgs(t *testing.T) {
	got := strings.Join(WorkerSpawnArgs("c.yml", "d", "temporal.example:7233"), " ")
	if want := "worker -config c.yml -data-dir d -temporal-address temporal.example:7233"; got != want {
		t.Errorf("worker argv = %q, want %q", got, want)
	}
}

func TestShellCredentialEnvKeepsWhatTheShellHas(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-test")
	t.Setenv("GITHUB_COPILOT_TOKEN", "")
	got := QuickstartChildEnv(ShellCredentialEnv())
	var anthropic, copilot []string
	for _, kv := range got {
		switch {
		case strings.HasPrefix(kv, "ANTHROPIC_API_KEY="):
			anthropic = append(anthropic, kv)
		case strings.HasPrefix(kv, "GITHUB_COPILOT_TOKEN="):
			copilot = append(copilot, kv)
		}
	}
	if len(anthropic) != 1 || anthropic[0] != "ANTHROPIC_API_KEY=sk-test" {
		t.Errorf("child ANTHROPIC_API_KEY entries = %v, want exactly the shell's value", anthropic)
	}
	if len(copilot) != 1 || copilot[0] != "GITHUB_COPILOT_TOKEN=" {
		t.Errorf("child GITHUB_COPILOT_TOKEN entries = %v, want exactly the shell's (empty) value", copilot)
	}
}
