package main

import (
	"context"
	"errors"
	"testing"

	"buildgate/internal/sandbox"
)

func TestTestDepsHaveNoSandboxRuntime(t *testing.T) {
	if rt := newTestDeps(t).sandbox.runtime(); rt != nil {
		t.Fatalf("runtime() = %v, want nil: a test reaches no gateway by default", rt)
	}
}

func TestAnUnsetFakeSandboxRuntimeMethodFails(t *testing.T) {
	rt := &fakeSandboxes{}
	ctx := context.Background()
	_, createErr := rt.Create(ctx, sandbox.SandboxRequest{})
	_, waitErr := rt.Wait(ctx, "n")
	_, statusErr := rt.Status(ctx, "n")
	_, listErr := rt.ListByRun(ctx, "d", "r")
	for name, err := range map[string]error{
		"Create": createErr, "Wait": waitErr, "Status": statusErr, "ListByRun": listErr,
		"Delete": rt.Delete(ctx, "n"), "PushCredential": rt.PushCredential(ctx, sandbox.RouteCredential{}),
	} {
		if !errors.Is(err, errFakeSandboxesUnset) {
			t.Errorf("%s = %v, want errFakeSandboxesUnset", name, err)
		}
	}
}
