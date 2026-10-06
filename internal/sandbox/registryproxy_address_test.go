package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// byAddressLifecycle is a started registry proxy lifecycle for a worker that
// reaches the proxy by address, over a fake starter and address hook.
func byAddressLifecycle(t *testing.T, rt Runtime, address func(context.Context, string, string, string) (string, error)) (*RegistryProxyLifecycle, RegistryProxySpec, error) {
	t.Helper()
	spec := validRegistryProxySpec()
	spec.DataDir = t.TempDir()
	handle := &RegistryProxyHandle{
		WorkerBaseURL: "http://registry-proxy:8092",
		AuditFacts: RegistryProxyLaunchFacts{
			NetworkName: "factoryd-registryproxy-abc", ContainerName: "factoryd-registryproxy-container-abc",
			WorkerBaseURL: "http://registry-proxy:8092",
		},
	}
	hooks := RegistryProxyHooks{
		StartRegistryProxy: func(context.Context, string, RegistryProxySpec) (*RegistryProxyHandle, error) {
			return handle, nil
		},
		CleanupRegistryProxy: func(context.Context, *RegistryProxyHandle) error { return nil },
		Address:              address,
	}
	lifecycle, err := BeginRegistryProxyLifecycleFor(rt, spec, "docker", spec.RunID, spec.DataDir, hooks)
	if err != nil {
		t.Fatalf("BeginRegistryProxyLifecycleFor: %v", err)
	}
	t.Cleanup(func() { _ = lifecycle.Cleanup() })
	return lifecycle, spec, lifecycle.Ensure(context.Background(), "")
}

func TestRegistryProxyByAddressGivesTheWorkerTheProxysAddress(t *testing.T) {
	var asked [][2]string
	lifecycle, spec, err := byAddressLifecycle(t, &workerRuntime{}, func(_ context.Context, _, container, network string) (string, error) {
		asked = append(asked, [2]string{container, network})
		return "172.28.0.2", nil
	})
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if want := [][2]string{{"factoryd-registryproxy-container-abc", "factoryd-registryproxy-abc"}}; !reflect.DeepEqual(asked, want) {
		t.Errorf("address asked for %v, want %v", asked, want)
	}
	worker := LaunchSpec{Name: "factoryd-worker-1", Network: "none", Environment: []string{"npm_config_registry=http://evil", "PATH=/usr/bin"}, DataDir: spec.DataDir, RunID: spec.RunID, User: fmt.Sprintf("65532:%d", os.Getgid())}
	worker, err = lifecycle.PrepareWorker(worker)
	if err != nil {
		t.Fatalf("PrepareWorker: %v", err)
	}
	if worker.Network != "none" {
		t.Errorf("Network = %q: a worker launched through a runtime joins no network", worker.Network)
	}
	if want := []SidecarEndpoint{{Name: "registry-proxy", IP: "172.28.0.2", Ports: []int{8092}}}; !reflect.DeepEqual(worker.Sidecars, want) {
		t.Errorf("Sidecars = %+v, want %+v", worker.Sidecars, want)
	}
	env := strings.Join(worker.Environment, "\n")
	if strings.Contains(env, "registry-proxy") || strings.Contains(env, "evil") {
		t.Errorf("environment names the alias or keeps the caller's registry:\n%s", env)
	}
	for _, route := range spec.Routes {
		if route.Prefix == npmRoutePrefix && !slices.Contains(worker.Environment, "npm_config_registry=http://172.28.0.2:8092"+npmRoutePrefix) {
			t.Errorf("environment lacks the npm registry at the proxy's address:\n%s", env)
		}
		if route.Prefix == pypiRoutePrefix && !slices.Contains(worker.Environment, "PIP_TRUSTED_HOST=172.28.0.2") {
			t.Errorf("environment lacks pip's trust in the proxy's address:\n%s", env)
		}
		if route.Prefix == goProxyRoutePrefix && !slices.Contains(worker.Environment, "GOPROXY=http://172.28.0.2:8092"+strings.TrimRight(goProxyRoutePrefix, "/")) {
			t.Errorf("environment lacks GOPROXY at the proxy's address:\n%s", env)
		}
	}
}

func TestRegistryProxyByAddressFailsWithoutAnAddress(t *testing.T) {
	for name, address := range map[string]func(context.Context, string, string, string) (string, error){
		"docker cannot say": func(context.Context, string, string, string) (string, error) {
			return "", errors.New("no such container")
		},
		"not an address":   func(context.Context, string, string, string) (string, error) { return "registry-proxy", nil },
		"an empty address": func(context.Context, string, string, string) (string, error) { return "", nil },
	} {
		if _, _, err := byAddressLifecycle(t, &workerRuntime{}, address); err == nil {
			t.Errorf("%s: Ensure succeeded", name)
		}
	}
}

func TestRegistryProxyWithNoRuntimeKeepsTheSharedNetwork(t *testing.T) {
	lifecycle, spec, err := byAddressLifecycle(t, nil, func(context.Context, string, string, string) (string, error) {
		t.Error("the address was asked for a docker run worker")
		return "", nil
	})
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	worker, err := lifecycle.PrepareWorker(LaunchSpec{Name: "factoryd-worker-1", Network: "none", DataDir: spec.DataDir, RunID: spec.RunID, User: fmt.Sprintf("65532:%d", os.Getgid())})
	if err != nil {
		t.Fatalf("PrepareWorker: %v", err)
	}
	if worker.Network != "factoryd-registryproxy-abc" || len(worker.Sidecars) != 0 {
		t.Errorf("Network = %q, Sidecars = %v; want the proxy's network and no sidecar", worker.Network, worker.Sidecars)
	}
}
