package main

import (
	"context"
	"errors"
	"testing"

	"buildgate/internal/sandbox"
)

// A model job's launch is refused below the disk floor before anything it
// would start (a registry proxy, compose services) is begun.
func TestAModelJobLaunchIsRefusedBelowTheDiskFloorBeforeAnySidecar(t *testing.T) {
	ctx := context.Background()
	if err := refuseLowDisk(ctx, nil); err != nil {
		t.Errorf("no runtime: %v", err)
	}
	read := func(free uint64) sandbox.Runtime {
		return &fakeSandboxes{diskFreeFn: func(context.Context) (sandbox.DiskSpace, error) {
			return sandbox.DiskSpace{FreeBytes: free, TotalBytes: 1 << 40}, nil
		}}
	}
	if err := refuseLowDisk(ctx, read(sandbox.LaunchDiskFloor-1)); !errors.Is(err, sandbox.ErrSandboxDiskLow) {
		t.Errorf("below the floor: %v, want ErrSandboxDiskLow", err)
	}
	if err := refuseLowDisk(ctx, read(sandbox.LaunchDiskFloor)); err != nil {
		t.Errorf("at the floor: %v", err)
	}
	if err := refuseLowDisk(ctx, &fakeSandboxes{}); err != nil {
		t.Errorf("an unreadable disk: %v", err)
	}
	registrySpec := &sandbox.RegistryProxySpec{}
	started := false
	hooks := sandbox.RegistryProxyHooks{StartRegistryProxy: func(context.Context, string, sandbox.RegistryProxySpec) (*sandbox.RegistryProxyHandle, error) {
		started = true
		return nil, errors.New("must not start")
	}}
	_, err := runSandboxWithRetriesVia(read(0), t.TempDir(), ctx, t.TempDir(), "", "", "", func(int) string { return t.TempDir() + "/x.log" }, 1,
		"img@sha256:aa", "docker", "", 0, "run-1", t.TempDir(), "", "", "", nil, nil, registrySpec, hooks, nil, sandbox.ComposeServicesHooks{}, "", "", nil, nil, "sh")
	if !errors.Is(err, sandbox.ErrSandboxDiskLow) || started {
		t.Errorf("err = %v, proxy started = %v; want the refusal before any sidecar", err, started)
	}
}
