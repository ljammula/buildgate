package main

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"buildgate/internal/sandbox"
)

// stackContexts fakes `docker --context <c> ps` per context and the colima
// contexts doctor finds.
func stackContexts(t *testing.T, dp *deps, running map[string][]string, contexts ...dockerContext) {
	t.Helper()
	fakeDockerOf(dp).runningFn = func(_ context.Context, _ string, c string) ([]string, error) {
		names, ok := running[c]
		if !ok {
			return nil, errors.New("no such context")
		}
		return names, nil
	}
	fakeDockerOf(dp).contextFactsFn = func(context.Context, string) dockerContextFacts {
		return dockerContextFacts{Listed: true, Contexts: contexts}
	}
}

func TestStackInPinnedContext(t *testing.T) {
	colima := dockerContext{Name: "colima", Endpoint: "unix:///Users/op/.colima/default/docker.sock"}
	pinned := dockerContext{Name: "colima-buildgate", Endpoint: "unix:///Users/op/.colima/buildgate/docker.sock"}
	desktop := dockerContext{Name: "desktop-linux", Endpoint: "unix:///Users/op/.docker/run/docker.sock"}
	stack := []string{"buildgate-openshell-gateway-1", "buildgate-openshell-meter-1", "factoryd-local-registry"}

	t.Run("no pin, no row", func(t *testing.T) {
		if _, ok := doctorCheckStackInPinnedContext(newTestDeps(t), context.Background(), "docker", ""); ok {
			t.Error("a row without a pin")
		}
	})
	t.Run("only the pinned VM runs them", func(t *testing.T) {
		dp := newTestDeps(t)
		stackContexts(t, dp, map[string][]string{"colima-buildgate": stack, "colima": {"other-app"}}, colima, pinned)
		c, ok := doctorCheckStackInPinnedContext(dp, context.Background(), "docker", "colima-buildgate")
		if !ok || c.Err != nil {
			t.Errorf("row = %+v, %v", c, ok)
		}
	})
	t.Run("both VMs run the stack", func(t *testing.T) {
		dp := newTestDeps(t)
		stackContexts(t, dp, map[string][]string{
			"colima-buildgate": stack,
			"colima":           append([]string{"other-app", "buildgate-temporal-1", "buildgate-postgresql-1"}, stack...),
		}, colima, pinned)
		c, ok := doctorCheckStackInPinnedContext(dp, context.Background(), "docker", "colima-buildgate")
		if !ok || c.Err == nil {
			t.Fatalf("row = %+v, %v", c, ok)
		}
		names := "buildgate-openshell-gateway-1 buildgate-openshell-meter-1 factoryd-local-registry buildgate-temporal-1 buildgate-postgresql-1"
		for _, want := range []string{
			"`docker --context colima update --restart=no " + names + " && docker --context colima stop " + names + "`, then `factoryd doctor -fix`",
			"move Temporal's history first (USAGE.md, \"Move an existing install into buildgate's VM\")",
		} {
			if !strings.Contains(c.Fix, want) {
				t.Errorf("fix lacks %q: %s", want, c.Fix)
			}
		}
		if strings.Contains(c.Fix, "other-app") {
			t.Errorf("fix names another project's container: %s", c.Fix)
		}
	})
	t.Run("Temporal alone in another VM", func(t *testing.T) {
		dp := newTestDeps(t)
		stackContexts(t, dp, map[string][]string{"colima-buildgate": stack, "colima": {"buildgate-temporal-ui-1"}}, colima, pinned)
		c, _ := doctorCheckStackInPinnedContext(dp, context.Background(), "docker", "colima-buildgate")
		if c.Err == nil || !strings.Contains(c.Fix, "move Temporal's history first") {
			t.Errorf("row = %+v", c)
		}
	})
	t.Run("a Docker Desktop context is not read", func(t *testing.T) {
		dp := newTestDeps(t)
		read := map[string]bool{}
		fakeDockerOf(dp).runningFn = func(_ context.Context, _ string, c string) ([]string, error) {
			read[c] = true
			return stack, nil
		}
		fakeDockerOf(dp).contextFactsFn = func(context.Context, string) dockerContextFacts {
			return dockerContextFacts{Listed: true, Contexts: []dockerContext{desktop, pinned}}
		}
		c, ok := doctorCheckStackInPinnedContext(dp, context.Background(), "docker", "colima-buildgate")
		if !ok || c.Err != nil || read["desktop-linux"] {
			t.Errorf("row = %+v, read %v", c, read)
		}
	})
}

// TestDoctorFixStartsNoStackWhileAStaleOneAnswers: -fix would reach the
// stale stack, so it starts nothing and the row says why.
func TestDoctorFixStartsNoStackWhileAStaleOneAnswers(t *testing.T) {
	dp := newTestDeps(t)
	colima := dockerContext{Name: "colima", Endpoint: "unix:///Users/op/.colima/default/docker.sock"}
	stackContexts(t, dp, map[string][]string{"colima-buildgate": nil, "colima": {"buildgate-openshell-gateway-1"}}, colima)
	reissued := false
	fakeSandboxOf(dp).reissueFn = func(context.Context, io.Writer) error {
		reissued = true
		return nil
	}
	started := false
	fakeSandboxOf(dp).startStackFn = func(context.Context, io.Writer, string) error {
		started = true
		return nil
	}
	in := doctorInputs{meterImage: "localhost:5050/factoryd-meter@sha256:aaaa", sandboxDocker: fakeImageDocker(t, true)}
	in.settings.DockerContext = "colima-buildgate"
	checks := doctorOpenShellChecks(dp, context.Background(), in, true, io.Discard)
	if started || reissued {
		t.Errorf("-fix started (%v) or re-issued (%v) while a stale stack answers", started, reissued)
	}
	if len(checks) == 0 || checks[0].Err == nil || !strings.Contains(checks[0].Name, "run only in docker context colima-buildgate") {
		t.Errorf("first row = %+v", checks[0])
	}
}

// TestDoctorFixReissuesOnlyBeforeAStartWithAPin: the bundle is issued again
// only when the start that follows would run with a pinned context and a
// meter image; never otherwise.
func TestDoctorFixReissuesOnlyBeforeAStartWithAPin(t *testing.T) {
	for _, c := range []struct {
		name, meter, pinned string
		want                bool
	}{
		{"pinned with a meter image", "localhost:5050/factoryd-meter@sha256:aaaa", "colima-buildgate", true},
		{"no pin", "localhost:5050/factoryd-meter@sha256:aaaa", "", false},
		{"no meter image", "", "colima-buildgate", false},
		{"no gateway runtime", "localhost:5050/factoryd-meter@sha256:aaaa", "colima-buildgate", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			dp := newTestDeps(t)
			reissued := false
			fakeSandboxOf(dp).reissueFn = func(context.Context, io.Writer) error {
				reissued = true
				return nil
			}
			fakeSandboxOf(dp).startStackFn = func(context.Context, io.Writer, string) error { return nil }
			fakeSandboxOf(dp).gatewayHealthyFn = func(context.Context) error { return errors.New("down") }
			if c.name != "no gateway runtime" {
				fakeSandboxOf(dp).runtimeFn = func() sandbox.Runtime { return &fakeSandboxes{} }
			}
			startSandboxRuntimeForFix(dp, context.Background(), io.Discard, c.meter, c.pinned)
			if reissued != c.want {
				t.Errorf("reissued = %v, want %v", reissued, c.want)
			}
		})
	}
}
