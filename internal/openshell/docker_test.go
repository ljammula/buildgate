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

func TestWorkerStartedAtSkipsTheSupervisor(t *testing.T) {
	docker := &scriptedDocker{answers: map[string]string{
		"ps":      "aaa\nbbb\n",
		"inspect": "supervisor|2026-10-04T10:00:00.1Z\n|2026-10-04T10:00:01.2Z\n",
	}}
	got, err := containersOver(docker).WorkerStartedAt(context.Background(), "bg-0123456789abcdef")
	if err != nil || got != "2026-10-04T10:00:01.2Z" {
		t.Fatalf("WorkerStartedAt = %q, %v", got, err)
	}
	wantCalls := [][]string{
		{"ps", "-a", "-q", "--no-trunc", "--filter", "label=openshell.ai/sandbox-name=bg-0123456789abcdef"},
		{"inspect", "--type", "container", "--format", `{{index .Config.Labels "openshell.ai/isolation-role"}}|{{.State.StartedAt}}`, "aaa", "bbb"},
	}
	if !reflect.DeepEqual(docker.calls, wantCalls) {
		t.Errorf("docker calls =\n%q\nwant\n%q", docker.calls, wantCalls)
	}
}

func TestWorkerStartedAtWithNoWorker(t *testing.T) {
	for name, answers := range map[string]map[string]string{
		"no container":    {"ps": ""},
		"supervisor only": {"ps": "aaa\n", "inspect": "supervisor|2026-10-04T10:00:00Z\n"},
	} {
		got, err := containersOver(&scriptedDocker{answers: answers}).WorkerStartedAt(context.Background(), "bg-a")
		if err != nil || got != "" {
			t.Errorf("%s: WorkerStartedAt = %q, %v; want empty", name, got, err)
		}
	}
}

func TestWorkerStartedAtFails(t *testing.T) {
	ctx := context.Background()
	two := &scriptedDocker{answers: map[string]string{"ps": "a\nb\n", "inspect": "|t1\n|t2\n"}}
	if got, err := containersOver(two).WorkerStartedAt(ctx, "bg-a"); err == nil || !strings.Contains(err.Error(), "2 worker containers") {
		t.Errorf("two workers: %q, %v", got, err)
	}
	garbled := &scriptedDocker{answers: map[string]string{"ps": "a\n", "inspect": "no separator\n"}}
	if got, err := containersOver(garbled).WorkerStartedAt(ctx, "bg-a"); err == nil {
		t.Errorf("an unreadable inspect line: %q, nil", got)
	}
	down := &scriptedDocker{errs: map[string]error{"ps": errors.New("cannot connect")}}
	if got, err := containersOver(down).WorkerStartedAt(ctx, "bg-a"); err == nil {
		t.Errorf("Docker unreachable: %q, nil", got)
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
