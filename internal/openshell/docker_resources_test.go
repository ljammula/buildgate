package openshell

import (
	"context"
	"errors"
	"reflect"
	"strings"
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

const workerID = "23f8926ae6759cb5ee972991bbcf11347d43bdb0c013bc9dd20612106f5566e0"

func TestWorkerMemoryReadsAPeakWithoutAStall(t *testing.T) {
	got, err := parseWorkerMemory("peak=7")
	if err != nil || got != (sandbox.WorkerMemory{PeakBytes: 7}) {
		t.Fatalf("parseWorkerMemory = %+v, %v", got, err)
	}
}

func TestWorkerMemoryAsksTheMeterAboutTheWorkerContainer(t *testing.T) {
	docker := &scriptedDocker{answers: map[string]string{
		"ps":      "meterid\n",
		"inspect": "supervisor|" + strings.Repeat("b", 64) + "\n|" + workerID + "\n",
		"exec":    "peak=1350000000 stall_us=420\n",
	}}
	c := containersOver(docker)
	c.MeterProject = "buildgate-openshell"
	got, err := c.WorkerMemory(context.Background(), "bg-0123456789abcdef")
	if want := (sandbox.WorkerMemory{PeakBytes: 1_350_000_000, StallMicros: 420, StallKnown: true}); err != nil || got != want {
		t.Fatalf("WorkerMemory = %+v, %v; want %+v", got, err, want)
	}
	wantCalls := [][]string{
		{"ps", "-a", "-q", "--no-trunc", "--filter", "label=openshell.ai/sandbox-name=bg-0123456789abcdef"},
		{"inspect", "--type", "container", "--format", `{{index .Config.Labels "openshell.ai/isolation-role"}}|{{.Id}}`, "meterid"},
		{"ps", "-q", "--no-trunc", "--filter", "label=com.docker.compose.project=buildgate-openshell", "--filter", "label=com.docker.compose.service=meter"},
		{"exec", "meterid", "/factoryd-meter", "memory-peak", workerID},
	}
	if !reflect.DeepEqual(docker.calls, wantCalls) {
		t.Errorf("docker calls =\n%q\nwant\n%q", docker.calls, wantCalls)
	}
}

func TestWorkerMemoryIsUnknownRatherThanGuessed(t *testing.T) {
	worker := "|" + workerID + "\n"
	cases := map[string]*scriptedDocker{
		"no containers":      {answers: map[string]string{"ps": ""}},
		"supervisor only":    {answers: map[string]string{"ps": "a\n", "inspect": "supervisor|" + workerID + "\n"}},
		"two workers":        {answers: map[string]string{"ps": "a\nb\n", "inspect": worker + worker}},
		"not a container id": {answers: map[string]string{"ps": "a\n", "inspect": "|abc\n"}},
		"cgroup gone":        {answers: map[string]string{"ps": "a\n", "inspect": worker}, errs: map[string]error{"exec": errors.New("no memory.peak")}},
		"unreadable answer":  {answers: map[string]string{"ps": "a\n", "inspect": worker, "exec": "ok\n"}},
		"zero":               {answers: map[string]string{"ps": "a\n", "inspect": worker, "exec": "peak=0\n"}},
		"bad stall":          {answers: map[string]string{"ps": "a\n", "inspect": worker, "exec": "peak=5 stall_us=x\n"}},
		"extra field":        {answers: map[string]string{"ps": "a\n", "inspect": worker, "exec": "peak=5 stall_us=1 x=2\n"}},
	}
	for name, docker := range cases {
		c := containersOver(docker)
		c.MeterProject = "p"
		if got, err := c.WorkerMemory(context.Background(), "bg-a"); err == nil {
			t.Errorf("%s: WorkerMemory = %+v, nil", name, got)
		}
	}
}
