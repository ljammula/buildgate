package openshell

import (
	"context"
	"errors"
	"strings"
	"testing"

	"buildgate/internal/sandbox"
)

func TestLazyFailsEveryCallUntilTheStackIsReadyAndAsksAgain(t *testing.T) {
	asked := 0
	lazy := &Lazy{
		Address:   "127.0.0.1:1",
		BundleDir: func() (string, error) { return t.TempDir(), nil },
		Ready: func(context.Context) error {
			asked++
			return errors.New("the gateway is not running: run `factoryd doctor -fix`")
		},
		Containers: &fakeContainers{startedAt: map[string]string{}},
	}
	ctx := context.Background()
	_, createErr := lazy.Create(ctx, sandbox.SandboxRequest{})
	_, waitErr := lazy.Wait(ctx, "n")
	_, statusErr := lazy.Status(ctx, "n")
	_, listErr := lazy.ListByRun(ctx, "d", "r")
	_, namesErr := lazy.Names(ctx)
	for name, err := range map[string]error{
		"Create": createErr, "Wait": waitErr, "Status": statusErr, "ListByRun": listErr, "Names": namesErr,
		"PushCredential": lazy.PushCredential(ctx, sandbox.RouteCredential{}),
	} {
		if err == nil || !strings.Contains(err.Error(), "doctor -fix") {
			t.Errorf("%s = %v, want the reason the stack is not ready", name, err)
		}
	}
	// A sandbox whose gateway cannot be asked is not known to be gone.
	if err := lazy.Delete(ctx, "n"); !errors.Is(err, sandbox.ErrCleanupUnconfirmed) {
		t.Errorf("Delete = %v, want ErrCleanupUnconfirmed", err)
	}
	if asked != 7 {
		t.Errorf("Ready was asked %d times, want once per call (7): a stack started later must be found", asked)
	}
}

func TestLazyReportsAMissingClientBundle(t *testing.T) {
	lazy := &Lazy{Address: "127.0.0.1:1", BundleDir: func() (string, error) { return t.TempDir(), nil }}
	if _, err := lazy.Status(context.Background(), "n"); err == nil {
		t.Error("a bundle directory with no certificate was accepted")
	}
}

func TestNamesListsEverySandbox(t *testing.T) {
	rt, _, _ := newTestRuntime()
	ctx := context.Background()
	if _, err := rt.Create(ctx, testRequest()); err != nil {
		t.Fatal(err)
	}
	names, err := rt.Names(ctx)
	if err != nil || len(names) != 1 || names[0] != testRequest().Name {
		t.Fatalf("Names = %v, %v", names, err)
	}
}
