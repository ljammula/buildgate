package main

import (
	"path/filepath"
	"reflect"
	"testing"

	"buildgate/internal/run"
)

func TestStampRouteSkipsByAttemptRole(t *testing.T) {
	exec := []run.RouteSkip{{Route: "a", Reason: "credential unavailable"}}
	rev := []run.RouteSkip{{Route: "b", Reason: "upstream scheme"}}
	tests := []struct {
		name string
		opts temporalSliceOptions
		kind string
		want []run.RouteSkip
	}{
		{"build takes execution skips", temporalSliceOptions{ExecutionRouteSkips: exec, ReviewRouteSkips: rev}, "build", exec},
		{"verify runs no model", temporalSliceOptions{ExecutionRouteSkips: exec, ReviewRouteSkips: rev}, "verify", nil},
		{"conformity takes review skips", temporalSliceOptions{ExecutionRouteSkips: exec, ReviewRouteSkips: rev}, "spec_conformity", rev},
		{"code review takes review skips", temporalSliceOptions{ExecutionRouteSkips: exec, ReviewRouteSkips: rev}, "code_review", rev},
		{"combined review takes review skips", temporalSliceOptions{ExecutionRouteSkips: exec, ReviewRouteSkips: rev}, "review", rev},
		{"gate attempt untouched", temporalSliceOptions{ExecutionRouteSkips: exec, ReviewRouteSkips: rev}, "full_suite_verify", nil},
		{"empty execution list leaves nil", temporalSliceOptions{ReviewRouteSkips: rev}, "build", nil},
		{"empty review list leaves nil", temporalSliceOptions{ExecutionRouteSkips: exec}, "spec_conformity", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			attempts := []run.Attempt{{Kind: tc.kind}}
			stampRouteSkips(attempts, tc.opts)
			if !reflect.DeepEqual(attempts[0].RelayRouteSkipped, tc.want) {
				t.Errorf("RelayRouteSkipped = %+v, want %+v", attempts[0].RelayRouteSkipped, tc.want)
			}
		})
	}
}

// TestIntegrationRouteFallbackRecordsSkipOnBuildAttempt: the first routes:
// candidate has no credential, the second works; the skip lands on the
// build attempt in run.json.
func TestIntegrationRouteFallbackRecordsSkipOnBuildAttempt(t *testing.T) {
	ws := newFixtureRepo(t)
	fakeDockerPath, err := filepath.Abs("testdata/fake_docker.sh")
	if err != nil {
		t.Fatalf("resolve fake docker path: %v", err)
	}
	path := isolateSessionConfig(t)
	t.Setenv("ROUTE_SKIP_TEST_GOOD_KEY", "sk-test")
	writeSessionConfig(t, path, "sandbox_docker: "+fakeDockerPath+"\n"+
		"routes:\n"+
		"  broken:\n    credential_mode: static\n    upstream: https://broken.example.invalid\n    credential_env: ROUTE_SKIP_TEST_UNSET_KEY\n"+
		"  good:\n    credential_mode: static\n    upstream: https://good.example.invalid\n    credential_env: ROUTE_SKIP_TEST_GOOD_KEY\n"+
		"models:\n  luna:\n    id: gpt-9000\n    api: openai-completions\n    routes: [broken, good]\n"+
		"roles:\n  execution:\n    model: luna\n")
	r := runFactorydWithSpecAndFlags(t, ws, "commit", "true", "# Ticket: fixture\n", "5m0s", nil, nil)
	for _, a := range r.Attempts {
		if a.Kind != "build" {
			continue
		}
		if len(a.RelayRouteSkipped) != 1 || a.RelayRouteSkipped[0].Route != "broken" {
			t.Errorf("RelayRouteSkipped = %+v, want one skip naming broken", a.RelayRouteSkipped)
		}
		return
	}
	t.Fatal("no build attempt recorded")
}
