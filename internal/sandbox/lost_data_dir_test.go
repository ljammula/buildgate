package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// fakeLostRunDocker is a docker that lists the given lines for every listing
// whose arguments contain the key, logs each call, and succeeds.
func fakeLostRunDocker(t *testing.T, listings map[string]string) (docker, logFile string) {
	t.Helper()
	dir := t.TempDir()
	logFile = filepath.Join(dir, "docker.log")
	script := "#!/bin/sh\necho \"$*\" >> " + logFile + "\ncase \"$*\" in\n"
	for key, lines := range listings {
		script += "*'" + key + "'*) printf '" + lines + "' ;;\n"
	}
	script += "esac\nexit 0\n"
	docker = filepath.Join(dir, "docker")
	if err := os.WriteFile(docker, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return docker, logFile
}

// A lost run's compose project, containers, networks and volumes are those
// whose labels, in the gateway's form, equal the run's: a longer run id the
// gateway cut to the run's label is the run's, while a sibling round whose
// id merely begins with the run's, and another data dir's, are not.
func TestRemoveLostRunResourcesRemovesOnlyThatRunsResources(t *testing.T) {
	dataDir := strings.Repeat("ab", 32)
	longRun := strings.Repeat("r", 60) + "-001-20261018-205220-59191"
	lost := LostRun{Run: GatewayLabelValue(longRun), DataDir: GatewayLabelValue(dataDir)}
	short := LostRun{Run: "req-001", DataDir: lost.DataDir}
	other := strings.Repeat("cd", 32)
	docker, logFile := fakeLostRunDocker(t, map[string]string{
		"--filter label=com.docker.compose.project": "bg-lost-a1\\t" + dataDir + "\\t" + longRun + "\\nbg-round-a1\\t" + dataDir + "\\treq-001-corrective1\\n",
		"ps -a --filter label=buildgate.data-dir --format": "proxy-lost\\t" + dataDir + "\\t" + longRun + "\\nproxy-round\\t" + dataDir + "\\treq-001-corrective1\\nproxy-other-dir\\t" + other + "\\t" + longRun + "\\n",
		"network ls --filter label=buildgate.data-dir":     "net-lost\\t" + dataDir + "\\t" + longRun + "\\n",
		"volume ls --filter label=buildgate.data-dir":      "vol-lost\\t" + dataDir + "\\t" + longRun + "\\nvol-round\\t" + dataDir + "\\treq-001-corrective1\\n",
	})
	containers, err := RemoveLostRunContainers(context.Background(), docker, lost)
	if err != nil {
		t.Fatal(err)
	}
	rest, err := RemoveLostRunNetworksAndVolumes(context.Background(), docker, lost)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := append(containers, rest...), []string{"compose project bg-lost-a1", "proxy-lost", "net-lost", "vol-lost"}; !slices.Equal(got, want) {
		t.Errorf("removed %v, want %v", got, want)
	}
	if removed, _ := RemoveLostRunContainers(context.Background(), docker, short); slices.Contains(removed, "proxy-round") || slices.Contains(removed, "compose project bg-round-a1") {
		t.Errorf("run req-001 must not own its corrective round's resources: removed %v", removed)
	}
	log, _ := os.ReadFile(logFile)
	for _, kept := range []string{"rm -f proxy-other-dir", "bg-round-a1 down", "rm -f vol-round"} {
		if strings.Contains(string(log), kept) {
			t.Errorf("%q touches another run or data dir:\n%s", kept, log)
		}
	}
	if !strings.Contains(string(log), "compose -p bg-lost-a1 down -v --remove-orphans") {
		t.Errorf("the lost run's compose project must go down with its volumes:\n%s", log)
	}
}

// Labels too short to name one run match nothing: no docker call is made.
func TestRemoveLostRunResourcesRefusesLabelsThatNameNoRun(t *testing.T) {
	for _, lost := range []LostRun{{Run: "run", DataDir: ""}, {Run: "run", DataDir: "abcd"}, {Run: "", DataDir: strings.Repeat("ab", 31) + "a"}} {
		if _, err := RemoveLostRunContainers(context.Background(), "/nonexistent/docker", lost); err == nil {
			t.Errorf("RemoveLostRunContainers(%+v) = nil error, want a refusal", lost)
		}
		if _, err := RemoveLostRunNetworksAndVolumes(context.Background(), "/nonexistent/docker", lost); err == nil {
			t.Errorf("RemoveLostRunNetworksAndVolumes(%+v) = nil error, want a refusal", lost)
		}
	}
}
