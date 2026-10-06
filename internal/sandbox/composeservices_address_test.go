package sandbox

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"buildgate/internal/composeservices"
)

const byAddressComposeYAML = `
services:
  db:
    image: docker.io/library/postgres:16
    ports:
      - "5432:5432"
  cache:
    image: docker.io/library/redis:7
    ports:
      - "6380:6379"
`

// byAddressCompose begins a lifecycle for a worker launched through a
// runtime, whose service addresses come from attemptAddresses[attempt].
func byAddressCompose(t *testing.T, rt Runtime, yaml string, attemptAddresses map[string]map[string]string) (*ComposeServicesLifecycle, error) {
	t.Helper()
	hooks := noopHooks()
	hooks.ServiceAddresses = func(_ context.Context, _, project, network string) (map[string]string, error) {
		if network != composeServicesNetworkName("run-1") {
			t.Errorf("addresses asked on network %q", network)
		}
		addresses, ok := attemptAddresses[project]
		if !ok {
			return nil, errors.New("no such project")
		}
		return addresses, nil
	}
	spec := ComposeServicesSpec{ComposeYAML: []byte(yaml), ParseOptions: composeservices.Options{AllowedImageRegistries: []string{"docker.io/library/"}}, SynthesizeOptions: testSynthesizeOpts}
	return BeginComposeServicesLifecycleFor(rt, spec, "fake-docker", "run-1", t.TempDir(), hooks)
}

func TestComposeServicesByAddressGiveEachAttemptItsOwnAddresses(t *testing.T) {
	l, err := byAddressCompose(t, &workerRuntime{}, byAddressComposeYAML, map[string]map[string]string{
		composeServicesProjectName("run-1", 1): {"db": "172.30.0.2", "cache": "172.30.0.3"},
		composeServicesProjectName("run-1", 2): {"db": "172.30.0.7", "cache": "172.30.0.8"},
	})
	if err != nil {
		t.Fatalf("BeginComposeServicesLifecycleFor: %v", err)
	}
	ctx := context.Background()
	if err := l.EnsureForAttempt(ctx, 1); err != nil {
		t.Fatalf("EnsureForAttempt(1): %v", err)
	}
	first := l.ApplyToWorkerLaunch(LaunchSpec{Network: "none", Command: []string{"python3", "build.py"}})
	if first.Network != "none" || first.ComposeNetwork != "" {
		t.Errorf("Network = %q, ComposeNetwork = %q: a worker launched through a runtime joins no network", first.Network, first.ComposeNetwork)
	}
	wantSidecars := []SidecarEndpoint{{Name: "cache", IP: "172.30.0.3", Ports: []int{6379}}, {Name: "db", IP: "172.30.0.2", Ports: []int{5432}}}
	got := append([]SidecarEndpoint(nil), first.Sidecars...)
	slices.SortFunc(got, func(a, b SidecarEndpoint) int { return strings.Compare(a.Name, b.Name) })
	if !reflect.DeepEqual(got, wantSidecars) {
		t.Errorf("Sidecars = %+v, want %+v", got, wantSidecars)
	}
	for _, want := range []string{
		"BG_COMPOSE_SERVICES=up", "BG_SERVICE_DB=172.30.0.2", "BG_SERVICE_DB_PORT=5432", "BG_SERVICE_CACHE=172.30.0.3", "BG_SERVICE_CACHE_PORT=6379",
		"BG_COMPOSE_FORWARDS=5432=172.30.0.2:5432,6380=172.30.0.3:6379",
	} {
		if !slices.Contains(first.Environment, want) {
			t.Errorf("environment lacks %q:\n%s", want, strings.Join(first.Environment, "\n"))
		}
	}
	if want := []string{WorkerForwarderPath, "--", "python3", "build.py"}; !reflect.DeepEqual(first.Command, want) {
		t.Errorf("Command = %q, want %q", first.Command, want)
	}

	if err := l.TeardownAttempt(ctx, 1); err != nil {
		t.Fatalf("TeardownAttempt(1): %v", err)
	}
	if err := l.EnsureForAttempt(ctx, 2); err != nil {
		t.Fatalf("EnsureForAttempt(2): %v", err)
	}
	second := l.ApplyToWorkerLaunch(LaunchSpec{Network: "none"})
	if !slices.Contains(second.Environment, "BG_SERVICE_DB=172.30.0.7") || !slices.Contains(second.Environment, "BG_COMPOSE_FORWARDS=5432=172.30.0.7:5432,6380=172.30.0.8:6379") {
		t.Errorf("the second attempt kept the first attempt's addresses:\n%s", strings.Join(second.Environment, "\n"))
	}
}

func TestComposeServicesByAddressFailTheAttemptWithoutAnAddress(t *testing.T) {
	for name, addresses := range map[string]map[string]string{
		"a service is missing":     {"db": "172.30.0.2"},
		"a service has no address": {"db": "172.30.0.2", "cache": ""},
		"an alias, not an address": {"db": "172.30.0.2", "cache": "cache"},
	} {
		l, err := byAddressCompose(t, &workerRuntime{}, byAddressComposeYAML, map[string]map[string]string{composeServicesProjectName("run-1", 1): addresses})
		if err != nil {
			t.Fatalf("%s: BeginComposeServicesLifecycleFor: %v", name, err)
		}
		if err := l.EnsureForAttempt(context.Background(), 1); err == nil || !strings.Contains(err.Error(), "cache") {
			t.Errorf("%s: EnsureForAttempt = %v, want an error naming the service", name, err)
		}
	}
}

func TestComposeServicesByAddressRefuseAPortTheRuntimeBlocks(t *testing.T) {
	yaml := `
services:
  etcd:
    image: docker.io/library/etcd:3
    ports:
      - "2379:2379"
`
	_, err := byAddressCompose(t, &workerRuntime{}, yaml, nil)
	if !errors.Is(err, ErrComposeServicesRejected) || !strings.Contains(err.Error(), "2379") {
		t.Fatalf("err = %v, want the compose file rejected for port 2379", err)
	}
	if l, err := byAddressCompose(t, nil, yaml, nil); err != nil || l.Disabled() {
		t.Fatalf("with no runtime the same file is served by alias: %v", err)
	}
}

func TestComposeServicesWithNoRuntimeKeepTheNetworkAndAliases(t *testing.T) {
	l, err := byAddressCompose(t, nil, byAddressComposeYAML, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.EnsureForAttempt(context.Background(), 1); err != nil {
		t.Fatalf("EnsureForAttempt: %v (the address hook must not be asked)", err)
	}
	s := l.ApplyToWorkerLaunch(LaunchSpec{Network: "none"})
	if s.ComposeNetwork != composeServicesNetworkName("run-1") || len(s.Sidecars) != 0 || !slices.Contains(s.Environment, "BG_SERVICE_DB=db") {
		t.Errorf("ComposeNetwork = %q, Sidecars = %v, environment:\n%s", s.ComposeNetwork, s.Sidecars, strings.Join(s.Environment, "\n"))
	}
}

func TestComposeServicesByAddressRefuseAWorkerEnvValueThatNamesAService(t *testing.T) {
	begin := func(rt Runtime, env map[string]string) error {
		spec := ComposeServicesSpec{ComposeYAML: []byte(byAddressComposeYAML), ParseOptions: composeservices.Options{AllowedImageRegistries: []string{"docker.io/library/"}}, SynthesizeOptions: testSynthesizeOpts, WorkerEnvironment: env}
		_, err := BeginComposeServicesLifecycleFor(rt, spec, "fake-docker", "run-1", t.TempDir(), noopHooks())
		return err
	}
	err := begin(&workerRuntime{}, map[string]string{"APP_MODE": "test", "DATABASE_URL": "postgres://app:s3cret@db:5432/app"})
	if !errors.Is(err, ErrComposeServicesRejected) || !strings.Contains(err.Error(), "DATABASE_URL") || !strings.Contains(err.Error(), `"db"`) {
		t.Fatalf("err = %v, want the entry and the service named", err)
	}
	if strings.Contains(err.Error(), "s3cret") {
		t.Errorf("the refusal prints the value: %v", err)
	}
	for _, served := range []map[string]string{
		{"DATABASE_URL": "postgres://app:s3cret@localhost:5432/app"},
		{"FEATURE": "dbx", "NAME": "mydb", "PATH_PREFIX": "/cachedir"},
		nil,
	} {
		if err := begin(&workerRuntime{}, served); err != nil {
			t.Errorf("worker env %v was refused: %v", served, err)
		}
	}
	if err := begin(nil, map[string]string{"DATABASE_URL": "postgres://app@db:5432/app"}); err != nil {
		t.Errorf("with no runtime an alias resolves and is served: %v", err)
	}
}

func TestNamesHost(t *testing.T) {
	for value, want := range map[string]bool{
		"db": true, "db:5432": true, "postgres://u:p@db:5432/x": true, "kafka,db": true, "http://db/": true,
		"mydb": false, "dbx": false, "db-primary": false, "db.internal": false, "x_db": false, "": false,
	} {
		if got := namesHost(value, "db"); got != want {
			t.Errorf("namesHost(%q, db) = %v, want %v", value, got, want)
		}
	}
}
