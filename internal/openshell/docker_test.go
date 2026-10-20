package openshell

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// scriptedDocker answers each docker call from a script keyed by its first
// argument and records the argv.
type scriptedDocker struct {
	answers map[string]string
	errs    map[string]error
	calls   [][]string
}

func (s *scriptedDocker) run(_ context.Context, args ...string) ([]byte, error) {
	s.calls = append(s.calls, args)
	return []byte(s.answers[args[0]]), s.errs[args[0]]
}

func containersOver(s *scriptedDocker) *DockerContainers { return &DockerContainers{run: s.run} }

func TestWorkerStateSkipsTheSupervisor(t *testing.T) {
	docker := &scriptedDocker{answers: map[string]string{
		"ps":      "aaa\nbbb\n",
		"inspect": "supervisor|2026-10-04T10:00:00.1Z|true\n|2026-10-04T10:00:01.2Z|false\n",
	}}
	got, err := containersOver(docker).WorkerState(context.Background(), "bg-0123456789abcdef")
	if err != nil || got != (WorkerState{StartedAt: "2026-10-04T10:00:01.2Z"}) {
		t.Fatalf("WorkerState = %v, %v", got, err)
	}
	wantCalls := [][]string{
		{"ps", "-a", "-q", "--no-trunc", "--filter", "label=openshell.ai/sandbox-name=bg-0123456789abcdef"},
		{"inspect", "--type", "container", "--format", `{{index .Config.Labels "openshell.ai/isolation-role"}}|{{.State.StartedAt}}|{{.State.OOMKilled}}`, "aaa", "bbb"},
	}
	if !reflect.DeepEqual(docker.calls, wantCalls) {
		t.Errorf("docker calls =\n%q\nwant\n%q", docker.calls, wantCalls)
	}
}

func TestWorkerStateWithNoWorker(t *testing.T) {
	for name, answers := range map[string]map[string]string{
		"no container":    {"ps": ""},
		"supervisor only": {"ps": "aaa\n", "inspect": "supervisor|2026-10-04T10:00:00Z|false\n"},
	} {
		got, err := containersOver(&scriptedDocker{answers: answers}).WorkerState(context.Background(), "bg-a")
		if err != nil || got != (WorkerState{}) {
			t.Errorf("%s: WorkerState = %v, %v; want empty", name, got, err)
		}
	}
}

func TestWorkerStateFails(t *testing.T) {
	ctx := context.Background()
	two := &scriptedDocker{answers: map[string]string{"ps": "a\nb\n", "inspect": "|t1|false\n|t2|false\n"}}
	if got, err := containersOver(two).WorkerState(ctx, "bg-a"); err == nil || !strings.Contains(err.Error(), "2 worker containers") {
		t.Errorf("two workers: %v, %v", got, err)
	}
	garbled := &scriptedDocker{answers: map[string]string{"ps": "a\n", "inspect": "no separator|x\n"}}
	if got, err := containersOver(garbled).WorkerState(ctx, "bg-a"); err == nil {
		t.Errorf("an unreadable inspect line: %v, nil", got)
	}
	down := &scriptedDocker{errs: map[string]error{"ps": errors.New("cannot connect")}}
	if got, err := containersOver(down).WorkerState(ctx, "bg-a"); err == nil {
		t.Errorf("Docker unreachable: %v, nil", got)
	}
}

func TestPresentAndRemove(t *testing.T) {
	ctx := context.Background()
	docker := &scriptedDocker{answers: map[string]string{"ps": "aaa\nbbb\n"}}
	c := containersOver(docker)
	if present, err := c.Present(ctx, "bg-a"); err != nil || !present {
		t.Fatalf("Present = %v, %v", present, err)
	}
	if err := c.Remove(ctx, "bg-a"); err != nil {
		t.Fatal(err)
	}
	if last := docker.calls[len(docker.calls)-1]; !reflect.DeepEqual(last, []string{"rm", "-f", "aaa", "bbb"}) {
		t.Errorf("last call = %q", last)
	}

	none := &scriptedDocker{answers: map[string]string{"ps": "\n"}}
	c = containersOver(none)
	if present, err := c.Present(ctx, "bg-a"); err != nil || present {
		t.Fatalf("Present with no container = %v, %v", present, err)
	}
	if err := c.Remove(ctx, "bg-a"); err != nil || len(none.calls) != 2 {
		t.Fatalf("Remove with no container: %v after %d calls; want no rm", err, len(none.calls))
	}
}

func TestANameOutsideTheGatewayAlphabetNeverReachesDocker(t *testing.T) {
	docker := &scriptedDocker{}
	c := containersOver(docker)
	for _, name := range []string{"", "a b", "x=y", "UPPER", "a,label=other", "-lead", strings.Repeat("a", 64)} {
		if _, err := c.Present(context.Background(), name); err == nil {
			t.Errorf("Present(%q) was accepted", name)
		}
		if err := c.Remove(context.Background(), name); err == nil {
			t.Errorf("Remove(%q) was accepted", name)
		}
	}
	if len(docker.calls) != 0 {
		t.Errorf("docker was called: %q", docker.calls)
	}
}

func volumesOver(s *scriptedDocker) *DockerVolumes { return &DockerVolumes{run: s.run} }

func TestVolumeCreateInspectsThenCreatesWithSortedLabels(t *testing.T) {
	docker := &scriptedDocker{errs: map[string]error{"volume": errors.New("docker volume: exit status 1: Error response from daemon: get bg-scratch-bg-a: No such volume: bg-scratch-bg-a")}}
	calls := 0
	v := &DockerVolumes{run: func(ctx context.Context, args ...string) ([]byte, error) {
		calls++
		if calls == 2 {
			docker.errs = nil
		}
		return docker.run(ctx, args...)
	}}
	if err := v.Create(context.Background(), "bg-scratch-bg-a", map[string]string{"buildgate.run": "r1", "buildgate.data-dir": "d1"}); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"volume", "inspect", "--format", "{{.Name}}", "bg-scratch-bg-a"},
		{"volume", "create", "--label", "buildgate.data-dir=d1", "--label", "buildgate.run=r1", "bg-scratch-bg-a"},
	}
	if !reflect.DeepEqual(docker.calls, want) {
		t.Errorf("calls =\n%q\nwant\n%q", docker.calls, want)
	}
}

func TestVolumeCreateRefusesAnExistingVolume(t *testing.T) {
	docker := &scriptedDocker{}
	err := volumesOver(docker).Create(context.Background(), "bg-scratch-bg-a", nil)
	if err == nil || !strings.Contains(err.Error(), "bg-scratch-bg-a already exists") {
		t.Fatalf("err = %v", err)
	}
	if len(docker.calls) != 1 {
		t.Errorf("calls = %q, want the inspect only", docker.calls)
	}
}

func TestVolumeCreateReturnsAnotherInspectFailure(t *testing.T) {
	docker := &scriptedDocker{errs: map[string]error{"volume": errors.New("cannot connect to the daemon")}}
	if err := volumesOver(docker).Create(context.Background(), "bg-scratch-bg-a", nil); err == nil || len(docker.calls) != 1 {
		t.Errorf("err = %v, calls = %q", err, docker.calls)
	}
}

func TestVolumeRemoveTreatsAMissingVolumeAsGone(t *testing.T) {
	ctx := context.Background()
	missing := &scriptedDocker{errs: map[string]error{"volume": errors.New("docker volume: exit 1: Error: No such volume: bg-scratch-bg-a")}}
	if err := volumesOver(missing).Remove(ctx, "bg-scratch-bg-a"); err != nil {
		t.Errorf("a missing volume: %v", err)
	}
	if want := [][]string{{"volume", "rm", "bg-scratch-bg-a"}}; !reflect.DeepEqual(missing.calls, want) {
		t.Errorf("calls = %q, want %q", missing.calls, want)
	}
	busy := &scriptedDocker{errs: map[string]error{"volume": errors.New("volume is in use")}}
	if err := volumesOver(busy).Remove(ctx, "bg-scratch-bg-a"); err == nil {
		t.Error("a volume in use was reported removed")
	}
}

func TestVolumeNamesOutsideThePatternNeverReachTheCLI(t *testing.T) {
	docker := &scriptedDocker{}
	v := volumesOver(docker)
	for _, name := range []string{"", "other", "bg-scratch-", "bg-scratch-A", "bg-scratch-a b", "bg-scratch-a;rm"} {
		if v.Create(context.Background(), name, nil) == nil || v.Remove(context.Background(), name) == nil {
			t.Errorf("%q was accepted", name)
		}
	}
	if len(docker.calls) != 0 {
		t.Errorf("calls = %q, want none", docker.calls)
	}
}
