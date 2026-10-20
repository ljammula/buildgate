package openshell

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"buildgate/internal/sandbox"
)

func TestDockerContainersReadsOOMKilledFromTheWorkerOnly(t *testing.T) {
	ctx := context.Background()
	// The supervisor was OOM-killed; the worker was not.
	supervisorOnly := &scriptedDocker{answers: map[string]string{
		"ps":      "aaa\nbbb\n",
		"inspect": "supervisor|2026-10-04T10:00:00Z|true\n|2026-10-04T10:00:01Z|false\n",
	}}
	if got, err := containersOver(supervisorOnly).WorkerState(ctx, "bg-a"); err != nil || got.OOMKilled {
		t.Errorf("a supervisor's OOM flag read as the worker's: %v, %v", got, err)
	}
	worker := &scriptedDocker{answers: map[string]string{
		"ps":      "aaa\nbbb\n",
		"inspect": "supervisor|2026-10-04T10:00:00Z|false\n|2026-10-04T10:00:01Z|true\n",
	}}
	if got, err := containersOver(worker).WorkerState(ctx, "bg-a"); err != nil || !got.OOMKilled || got.StartedAt != "2026-10-04T10:00:01Z" {
		t.Errorf("the worker's OOM flag: %v, %v", got, err)
	}
	notABool := &scriptedDocker{answers: map[string]string{"ps": "aaa\n", "inspect": "|t|maybe\n"}}
	if got, err := containersOver(notABool).WorkerState(ctx, "bg-a"); err == nil {
		t.Errorf("an unreadable OOM flag: %v, nil", got)
	}
}

func TestDiskFreeRunsStatfsInTheMeterContainer(t *testing.T) {
	docker := &scriptedDocker{answers: map[string]string{"ps": "meterid\n", "exec": "free=3000 total=10000\n"}}
	c := containersOver(docker)
	c.MeterProject = "buildgate-openshell"
	got, err := c.DiskFree(context.Background())
	if err != nil || got.FreeBytes != 3000 || got.TotalBytes != 10000 {
		t.Fatalf("DiskFree = %+v, %v", got, err)
	}
	wantCalls := [][]string{
		{"ps", "-q", "--no-trunc", "--filter", "label=com.docker.compose.project=buildgate-openshell", "--filter", "label=com.docker.compose.service=meter"},
		{"exec", "meterid", "/factoryd-meter", "statfs"},
	}
	if !reflect.DeepEqual(docker.calls, wantCalls) {
		t.Errorf("docker calls =\n%q\nwant\n%q", docker.calls, wantCalls)
	}
}

func TestDiskFreeIsUnknownRatherThanGuessed(t *testing.T) {
	ctx := context.Background()
	cases := map[string]*scriptedDocker{
		"no meter container":     {answers: map[string]string{"ps": ""}},
		"two meter containers":   {answers: map[string]string{"ps": "a\nb\n"}},
		"a meter without statfs": {answers: map[string]string{"ps": "a\n"}, errs: map[string]error{"exec": errors.New("-ledger-root is required")}},
		"an unreadable answer":   {answers: map[string]string{"ps": "a\n", "exec": "ok\n"}},
		"free over total":        {answers: map[string]string{"ps": "a\n", "exec": "free=9 total=3\n"}},
		"docker unreachable":     {errs: map[string]error{"ps": errors.New("cannot connect")}},
	}
	for name, docker := range cases {
		c := containersOver(docker)
		c.MeterProject = "p"
		if got, err := c.DiskFree(ctx); err == nil {
			t.Errorf("%s: DiskFree = %+v, nil", name, got)
		}
	}
	if _, err := containersOver(&scriptedDocker{}).DiskFree(ctx); err == nil {
		t.Error("DiskFree without a meter project succeeded")
	}
}

func TestRuntimeStatusCarriesTheWorkersOOMFlag(t *testing.T) {
	rt, _, containers := newTestRuntime()
	containers.startedAt["bg-orphan"] = "t0"
	containers.oomKilled = map[string]bool{"bg-orphan": true}
	got, err := rt.Status(context.Background(), "bg-orphan")
	if err != nil || !got.Present || !got.OOMKilled || got.StartedAt != "t0" {
		t.Fatalf("Status = %+v, %v", got, err)
	}
}

func TestRuntimeReadsDockersDiskThroughItsContainers(t *testing.T) {
	rt, _, containers := newTestRuntime()
	containers.disk = sandbox.DiskSpace{FreeBytes: 7, TotalBytes: 9}
	if got, err := rt.DockerDiskFree(context.Background()); err != nil || got != containers.disk {
		t.Fatalf("DockerDiskFree = %+v, %v", got, err)
	}
	containers.diskErr = errors.New("no statfs")
	if _, err := rt.DockerDiskFree(context.Background()); err == nil {
		t.Fatal("an unreadable disk was reported as a reading")
	}
	lazy := &Lazy{Containers: containers}
	if _, err := lazy.DockerDiskFree(context.Background()); err == nil {
		t.Fatal("Lazy dropped the unreadable disk")
	}
}
