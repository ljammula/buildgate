package sandbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeRunContainersDocker writes a docker stand-in that logs every
// invocation to calls, lists the ids in state for the current label prefix
// only, and (unless stubborn) empties state on "rm -f".
func fakeRunContainersDocker(t *testing.T, ids string, stubborn bool) (docker, calls string) {
	t.Helper()
	dir := t.TempDir()
	docker = filepath.Join(dir, "docker-fake")
	calls = filepath.Join(dir, "calls")
	state := filepath.Join(dir, "state")
	if err := os.WriteFile(state, []byte(ids), 0o600); err != nil {
		t.Fatal(err)
	}
	rm := `: > "` + state + `"`
	if stubborn {
		rm = ":"
	}
	script := `#!/bin/sh
echo "$@" >> "` + calls + `"
case "$1" in
ps) case "$*" in *buildgate.run=*) cat "` + state + `";; esac;;
rm) ` + rm + `;;
esac
exit 0
`
	if err := os.WriteFile(docker, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return docker, calls
}

func readCalls(t *testing.T, calls string) []string {
	t.Helper()
	b, err := os.ReadFile(calls)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimRight(string(b), "\n"), "\n")
}

func TestRemoveRunContainersListsByBothLabelsRemovesAndReverifies(t *testing.T) {
	docker, calls := fakeRunContainersDocker(t, "aaa\nbbb\n", false)
	dataDir := t.TempDir()
	if err := RemoveRunContainers(context.Background(), docker, dataDir, "run-1"); err != nil {
		t.Fatalf("RemoveRunContainers: %v", err)
	}
	got := readCalls(t, calls)
	wantRun := "label=buildgate.run=run-1"
	wantDir := "label=buildgate.data-dir=" + dataDirLabel(dataDir)
	var rm []string
	lists := 0
	for _, c := range got {
		switch {
		case strings.HasPrefix(c, "ps -a -q") && strings.Contains(c, wantRun) && strings.Contains(c, wantDir):
			lists++
		case strings.HasPrefix(c, "rm -f"):
			rm = append(rm, c)
		case strings.HasPrefix(c, "ps -a -q"):
			// legacy-prefix listing
		default:
			t.Errorf("unexpected docker call %q", c)
		}
	}
	if lists != 2 {
		t.Errorf("current-prefix listings = %d, want 2 (before and after removal); calls=%q", lists, got)
	}
	if len(rm) != 1 || rm[0] != "rm -f aaa bbb" {
		t.Errorf("rm calls = %q, want one \"rm -f aaa bbb\"", rm)
	}
}

func TestRemoveRunContainersNothingToRemoveSkipsRm(t *testing.T) {
	docker, calls := fakeRunContainersDocker(t, "", false)
	if err := RemoveRunContainers(context.Background(), docker, t.TempDir(), "run-1"); err != nil {
		t.Fatalf("RemoveRunContainers: %v", err)
	}
	for _, c := range readCalls(t, calls) {
		if strings.HasPrefix(c, "rm") {
			t.Errorf("unexpected rm call %q", c)
		}
	}
}

func TestRemoveRunContainersSurvivorIsCleanupUnconfirmed(t *testing.T) {
	docker, _ := fakeRunContainersDocker(t, "stuck\n", true)
	err := RemoveRunContainers(context.Background(), docker, t.TempDir(), "run-1")
	if !errors.Is(err, ErrCleanupUnconfirmed) {
		t.Fatalf("err = %v, want ErrCleanupUnconfirmed", err)
	}
	if !strings.Contains(err.Error(), "stuck") {
		t.Errorf("err %q does not name the surviving container", err)
	}
}

func TestRemoveRunContainersRefusesEmptyScopeWithoutCallingDocker(t *testing.T) {
	docker, calls := fakeRunContainersDocker(t, "aaa\n", false)
	for _, tc := range []struct{ dataDir, runID string }{{"", "run-1"}, {t.TempDir(), ""}, {"", ""}} {
		if err := RemoveRunContainers(context.Background(), docker, tc.dataDir, tc.runID); err == nil {
			t.Errorf("dataDir=%q runID=%q: want error", tc.dataDir, tc.runID)
		}
	}
	if got := readCalls(t, calls); len(got) != 0 {
		t.Errorf("docker called for an empty scope: %q", got)
	}
}
