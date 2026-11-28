package main

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
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
	stack := []string{"buildgate-openshell-gateway-1", "buildgate-openshell-meter-1", "factoryd-local-registry"}

	t.Run("no pin, no row", func(t *testing.T) {
		if _, ok := doctorCheckStackInPinnedContext(newTestDeps(t), context.Background(), "docker", ""); ok {
			t.Error("a row without a pin")
		}
	})
	t.Run("the stack runs in the pinned VM", func(t *testing.T) {
		dp := newTestDeps(t)
		stackContexts(t, dp, map[string][]string{"colima-buildgate": stack}, colima, pinned)
		c, ok := doctorCheckStackInPinnedContext(dp, context.Background(), "docker", "colima-buildgate")
		if !ok || c.Err != nil {
			t.Errorf("row = %+v, %v", c, ok)
		}
	})
	t.Run("a stale stack in the default VM", func(t *testing.T) {
		dp := newTestDeps(t)
		stackContexts(t, dp, map[string][]string{
			"colima-buildgate": {"buildgate-openshell-meter-1"},
			"colima":           append([]string{"other-app", "buildgate-temporal-1", "buildgate-postgresql-1"}, stack...),
		}, colima, pinned, dockerContext{Name: "desktop-linux", Endpoint: "unix:///x"})
		c, ok := doctorCheckStackInPinnedContext(dp, context.Background(), "docker", "colima-buildgate")
		if !ok || c.Err == nil {
			t.Fatalf("row = %+v, %v", c, ok)
		}
		names := "buildgate-openshell-gateway-1 factoryd-local-registry"
		for _, want := range []string{
			"`docker --context colima update --restart=no " + names + " && docker --context colima stop " + names + "`, then `factoryd doctor -fix`",
			"buildgate-temporal-1 and buildgate-postgresql-1 also run there",
			"Move an existing install into buildgate's VM",
		} {
			if !strings.Contains(c.Fix, want) {
				t.Errorf("fix lacks %q: %s", want, c.Fix)
			}
		}
		if strings.Contains(c.Fix, "other-app") || !strings.Contains(c.Err.Error(), "docker context colima and not in colima-buildgate") {
			t.Errorf("row = %+v", c)
		}
	})
	t.Run("missing everywhere is left to the other rows", func(t *testing.T) {
		dp := newTestDeps(t)
		stackContexts(t, dp, map[string][]string{"colima-buildgate": nil, "colima": {"other-app"}}, colima, pinned)
		if c, ok := doctorCheckStackInPinnedContext(dp, context.Background(), "docker", "colima-buildgate"); ok {
			t.Errorf("row = %+v", c)
		}
	})
}

// TestDoctorFixStartsNoStackWhileAStaleOneAnswers: -fix would reach the
// stale stack, so it starts nothing and the row says why.
func TestDoctorFixStartsNoStackWhileAStaleOneAnswers(t *testing.T) {
	dp := newTestDeps(t)
	colima := dockerContext{Name: "colima", Endpoint: "unix:///Users/op/.colima/default/docker.sock"}
	stackContexts(t, dp, map[string][]string{"colima-buildgate": nil, "colima": {"buildgate-openshell-gateway-1"}}, colima)
	started := false
	fakeSandboxOf(dp).startStackFn = func(context.Context, io.Writer, string) error {
		started = true
		return nil
	}
	in := doctorInputs{meterImage: "localhost:5050/factoryd-meter@sha256:aaaa", sandboxDocker: fakeImageDocker(t, true)}
	in.settings.DockerContext = "colima-buildgate"
	checks := doctorOpenShellChecks(dp, context.Background(), in, true, io.Discard)
	if started {
		t.Error("-fix started the stack while a stale one answers")
	}
	if len(checks) == 0 || checks[0].Err == nil || !strings.Contains(checks[0].Name, "run in docker context colima-buildgate") {
		t.Errorf("first row = %+v", checks[0])
	}
}
