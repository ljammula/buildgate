package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// A lost run's compose project, container and network are found by the prefixes the
// gateway kept of its labels and removed, container first; another run's,
// and another data dir's, are not touched.
func TestRemoveLostRunResourcesRemovesOnlyThatRunsResources(t *testing.T) {
	dir := t.TempDir()
	logFile := filepath.Join(dir, "docker.log")
	lost := strings.Repeat("ab", 32)
	other := strings.Repeat("cd", 32)
	script := `#!/bin/sh
echo "$*" >> ` + logFile + `
case "$1 $2" in
"compose -p") ;;
"ps -a") case "$*" in *com.docker.compose.project*) printf 'bg-run-lost-a1\t` + lost + `\trun-lost-001-x\nbg-run-kept-a1\t` + lost + `\trun-kept\n' ;; *buildgate.data-dir*) printf 'proxy-lost\t` + lost + `\trun-lost-001-x\nproxy-other-run\t` + lost + `\trun-kept\nproxy-other-dir\t` + other + `\trun-lost-001-x\n' ;; esac ;;
"network ls") case "$*" in *buildgate.data-dir*) printf 'net-lost\t` + lost + `\trun-lost-001-x\n' ;; esac ;;
"volume ls") ;;
esac
exit 0
`
	docker := filepath.Join(dir, "docker")
	if err := os.WriteFile(docker, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	removed, err := RemoveLostRunResources(context.Background(), docker, lost[:63], "run-lost-001")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"compose project bg-run-lost-a1", "proxy-lost", "net-lost"}; !slices.Equal(removed, want) {
		t.Errorf("removed %v, want %v", removed, want)
	}
	log, _ := os.ReadFile(logFile)
	if !strings.Contains(string(log), "rm -f proxy-lost") || !strings.Contains(string(log), "network rm net-lost") ||
		strings.Index(string(log), "rm -f proxy-lost") > strings.Index(string(log), "network rm net-lost") {
		t.Errorf("the container must be removed before the network:\n%s", log)
	}
	if !strings.Contains(string(log), "compose -p bg-run-lost-a1 down -v --remove-orphans") || strings.Contains(string(log), "bg-run-kept-a1 down") {
		t.Errorf("only the lost run's compose project must go down, with its volumes:\n%s", log)
	}
	for _, kept := range []string{"proxy-other-run", "proxy-other-dir"} {
		if strings.Contains(string(log), "rm -f "+kept) {
			t.Errorf("%s belongs to another run or data dir and was removed:\n%s", kept, log)
		}
	}
}

// Prefixes too short to name one run match nothing: no docker call is made.
func TestRemoveLostRunResourcesRefusesAShortPrefix(t *testing.T) {
	for _, c := range []struct{ dataDir, run string }{{"", "run"}, {"abcd", "run"}, {strings.Repeat("ab", 32), ""}} {
		if _, err := RemoveLostRunResources(context.Background(), "/nonexistent/docker", c.dataDir, c.run); err == nil {
			t.Errorf("RemoveLostRunResources(%q, %q) = nil error, want a refusal", c.dataDir, c.run)
		}
	}
}
