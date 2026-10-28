package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const proxyLogStart = "2026/10/11 09:00:00 serving registry proxy on :8092\n"

func TestParseRegistryProxyUpstreamFailures(t *testing.T) {
	for _, tc := range []struct {
		name, log string
		want      int
		known     bool
	}{
		{"a proxy that failed nothing", proxyLogStart + `2026/10/11 09:00:01 method="GET" path="/npm/left-pad" status=200 duration=3ms` + "\n", 0, true},
		{"the last total", proxyLogStart + "2026/10/11 09:00:01 upstream_failures=1\n2026/10/11 09:00:02 upstream_failures=2\n", 2, true},
		{"an image older than the count", proxyLogStart, 0, true},
		{"no log", "", 0, false},
		{"a log without the proxy's start", "2026/10/11 09:00:02 upstream_failures=2\n", 0, false},
		{"a total that is not a number", proxyLogStart + "2026/10/11 09:00:02 upstream_failures=many\n", 0, false},
		// The proxy quotes what a worker sends: a path that spells a failure
		// line stays inside its request line, newline and all.
		{"a path that spells a failure line", proxyLogStart + `2026/10/11 09:00:01 method="GET" path="/npm/x\n2026/10/11 09:00:01 upstream_failures=9" status=404 duration=1ms` + "\n", 0, true},
		{"a path that is only the prefix", proxyLogStart + `2026/10/11 09:00:01 method="GET" path="upstream_failures=9" status=404 duration=1ms` + "\n", 0, true},
	} {
		got, known := parseRegistryProxyUpstreamFailures(tc.log)
		if got != tc.want || known != tc.known {
			t.Errorf("%s: = (%d, %v), want (%d, %v)", tc.name, got, known, tc.want, tc.known)
		}
	}
}

// fakeLogsDocker is a docker whose `logs` prints log and exits with code.
func fakeLogsDocker(t *testing.T, log string, code string) string {
	t.Helper()
	dir := t.TempDir()
	logFile := filepath.Join(dir, "log")
	if err := os.WriteFile(logFile, []byte(log), 0o600); err != nil {
		t.Fatal(err)
	}
	docker := filepath.Join(dir, "docker")
	script := "#!/bin/sh\n[ \"$1\" = logs ] && [ \"$2\" = proxy-1 ] || exit 64\ncat '" + logFile + "' >&2\nexit " + code + "\n"
	if err := os.WriteFile(docker, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return docker
}

func TestRegistryProxyUpstreamFailuresReadsTheContainersLog(t *testing.T) {
	log := proxyLogStart + "2026/10/11 09:00:02 upstream_failures=3\n"
	if got, known := registryProxyUpstreamFailures(context.Background(), fakeLogsDocker(t, log, "0"), "proxy-1"); got != 3 || !known {
		t.Errorf("= (%d, %v), want (3, true)", got, known)
	}
	// A log that cannot be read is no count, not a count of zero.
	if got, known := registryProxyUpstreamFailures(context.Background(), fakeLogsDocker(t, log, "1"), "proxy-1"); got != 0 || known {
		t.Errorf("docker logs failed: = (%d, %v), want (0, false)", got, known)
	}
	if _, known := registryProxyUpstreamFailures(context.Background(), "/nonexistent/docker", "proxy-1"); known {
		t.Error("no docker: the count read as known")
	}
	if _, known := registryProxyUpstreamFailures(context.Background(), fakeLogsDocker(t, log, "0"), ""); known {
		t.Error("no container name: the count read as known")
	}
}

func TestRegistryProxyLifecycleUpstreamFailures(t *testing.T) {
	var nilLifecycle *RegistryProxyLifecycle
	if _, known := nilLifecycle.UpstreamFailures(context.Background()); known {
		t.Error("no lifecycle: the count read as known")
	}
	spec := validRegistryProxySpec()
	spec.DataDir = t.TempDir()
	var asked []string
	hooks := RegistryProxyHooks{
		StartRegistryProxy: func(context.Context, string, RegistryProxySpec) (*RegistryProxyHandle, error) {
			return &RegistryProxyHandle{AuditFacts: RegistryProxyLaunchFacts{ContainerName: "proxy-1"}}, nil
		},
		CleanupRegistryProxy: func(context.Context, *RegistryProxyHandle) error { return nil },
		UpstreamFailures: func(_ context.Context, docker, container string) (int, bool) {
			asked = append(asked, docker+" "+container)
			return 4, true
		},
	}
	lifecycle, err := BeginRegistryProxyLifecycle(spec, "docker", spec.RunID, spec.DataDir, hooks)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lifecycle.Cleanup() }()
	if _, known := lifecycle.UpstreamFailures(context.Background()); known || len(asked) != 0 {
		t.Errorf("before the proxy started: known = %v, asked %v", known, asked)
	}
	if err := lifecycle.Ensure(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if got, known := lifecycle.UpstreamFailures(context.Background()); got != 4 || !known {
		t.Errorf("= (%d, %v), want (4, true)", got, known)
	}
	if len(asked) != 1 || asked[0] != "docker proxy-1" {
		t.Errorf("asked %v, want the started container", asked)
	}
	if err := lifecycle.Cleanup(); err != nil {
		t.Fatal(err)
	}
	if _, known := lifecycle.UpstreamFailures(context.Background()); known {
		t.Error("after cleanup: the count read as known")
	}
}

// The proxy logs a line for every request, so a worker decides how long the
// log is and how long a line is: the scan holds one bounded line at a time,
// skips a line longer than that, and still finds the totals around it.
func TestScanRegistryProxyUpstreamFailuresIsBoundedByLineNotByLog(t *testing.T) {
	var log strings.Builder
	log.WriteString("2026/10/11 10:00:00 " + registryProxyListeningLogLine + "\n")
	log.WriteString("2026/10/11 10:00:01 " + registryProxyUpstreamFailuresLogPrefix + "1\n")
	log.WriteString("2026/10/11 10:00:02 GET \"/" + strings.Repeat("a", 3*maxRegistryProxyLogLine) + " " + registryProxyUpstreamFailuresLogPrefix + "99\" status=404\n")
	for i := 0; i < 20000; i++ {
		log.WriteString("2026/10/11 10:00:03 GET \"/simple/pkg/\" status=404 bytes=0\n")
	}
	log.WriteString("2026/10/11 10:00:04 " + registryProxyUpstreamFailuresLogPrefix + "3\n")
	count, known, err := scanRegistryProxyUpstreamFailures(strings.NewReader(log.String()))
	if err != nil || !known || count != 3 {
		t.Errorf("count = %d, known = %v, err = %v; want 3 from a log of %d bytes", count, known, err, log.Len())
	}
}

// parseRegistryProxyUpstreamFailures is scanRegistryProxyUpstreamFailures
// over a log already in hand.
func parseRegistryProxyUpstreamFailures(log string) (count int, known bool) {
	count, known, _ = scanRegistryProxyUpstreamFailures(strings.NewReader(log))
	return count, known
}
