package sandbox

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestRunWorkerThroughRuntimeRefusesBeforeAnyGatewayCallWhenGitConfigExposesCredential
// is the gateway launch's refusal: neither a credential push nor a sandbox
// create reaches the runtime, with or without a model route.
func TestRunWorkerThroughRuntimeRefusesBeforeAnyGatewayCallWhenGitConfigExposesCredential(t *testing.T) {
	exposures := []struct {
		name, key, value, want string
	}{
		{"a credential helper", "credential.helper", "!echo attacker-controlled", "credential.helper"},
		{"an HTTP extra header", "http.extraheader", "AUTHORIZATION: bearer ghp_secrettoken123", "extraheader"},
	}
	workers := []struct {
		name   string
		worker func(*testing.T) RuntimeWorker
	}{
		{"no route", func(*testing.T) RuntimeWorker { return RuntimeWorker{} }},
		{"a model route", func(t *testing.T) RuntimeWorker {
			return runtimeWorker(chatGPTRelaySpec(workerTestNow.Add(24*time.Hour)), t.TempDir())
		}},
	}
	for _, exposure := range exposures {
		for _, w := range workers {
			t.Run(exposure.name+" with "+w.name, func(t *testing.T) {
				spec := runtimeSpec(t)
				spec.Name = "factoryd-temporal-worker-1"
				runGit(t, spec.WorkDir, "config", exposure.key, exposure.value)
				rt := &workerRuntime{lines: []string{"ran"}, startedAt: "t"}

				_, err := RunWorkerThroughRuntime(context.Background(), rt, spec, w.worker(t))
				if err == nil || !strings.Contains(strings.ToLower(err.Error()), exposure.want) {
					t.Fatalf("err = %v, want the credential preflight's refusal naming %q", err, exposure.want)
				}
				if strings.Contains(err.Error(), "ghp_secrettoken123") {
					t.Errorf("the refusal echoes the credential: %v", err)
				}
				if len(rt.calls) != 0 {
					t.Errorf("the runtime was called (%v); the refusal comes before any gateway call", rt.calls)
				}
				if recorded, _ := RecordedSandboxes(spec.DataDir, spec.RunID); len(recorded) != 0 {
					t.Errorf("recorded sandboxes = %v, want none", recorded)
				}
			})
		}
	}
}
