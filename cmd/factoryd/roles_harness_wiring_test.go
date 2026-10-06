package main

import (
	"path/filepath"
	"testing"

	"buildgate/internal/run"
)

// TestIntegrationExecutionRoleHarnessReachesBuildAppArgvAndAttempt proves the
// execution role's harness reaches build_app.py's own argv (always, as
// --harness) and the recorded attempt (Attempt.Harness), end to end through a
// real `factoryd` subprocess, and that -execution-harness (the per-request
// choice a queue entry forwards) wins within roles.execution.allowed_harnesses.
// The review role runs pi in the same config, so the two roles differ.
func TestIntegrationExecutionRoleHarnessReachesBuildAppArgvAndAttempt(t *testing.T) {
	fakeDockerPath, err := filepath.Abs("testdata/fake_docker.sh")
	if err != nil {
		t.Fatalf("resolve fake docker path: %v", err)
	}
	config := "sandbox_docker: " + fakeDockerPath + "\n" +
		"routes:\n  litellm:\n    credential_mode: static\n    upstream: https://litellm.example.invalid\n    credential_env: INTEGRATION_HARNESS_TEST_KEY\n" +
		"models:\n  luna:\n    id: gpt-9000\n    api: openai-completions\n    routes: [litellm]\n" +
		"  other:\n    id: gpt-9001\n    api: openai-completions\n    routes: [litellm]\n" +
		"roles:\n  execution:\n    model: luna\n    harness: pifork\n    allowed_harnesses: [pifork, pi]\n" +
		"  review:\n    model: other\n    harness: pi\n"

	for _, tc := range []struct {
		name  string
		extra []string
		want  string
	}{
		{"role default", nil, "pifork"},
		{"per-run choice", []string{"-execution-harness", "pi"}, "pi"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws := newFixtureRepo(t)
			path := isolateSessionConfig(t)
			t.Setenv("INTEGRATION_HARNESS_TEST_KEY", "sk-test")
			writeSessionConfig(t, path, config)

			r := runFactorydWithSpecAndFlags(t, ws, "commit", "true", "# Ticket: fixture\n", "5m0s", nil, tc.extra)
			var build *run.Attempt
			for i := range r.Attempts {
				if r.Attempts[i].Kind == "build" {
					build = &r.Attempts[i]
					break
				}
			}
			if build == nil {
				t.Fatal("no build attempt recorded")
			}
			if !hasArgPair(build.Command, "--harness", tc.want) {
				t.Errorf("build attempt argv = %v, want --harness %s", build.Command, tc.want)
			}
			if build.Harness != tc.want {
				t.Errorf("build attempt Harness = %q, want %q", build.Harness, tc.want)
			}
		})
	}
}
