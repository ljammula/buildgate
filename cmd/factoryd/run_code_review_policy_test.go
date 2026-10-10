package main

import (
	"fmt"
	"os"
	"testing"
	"time"
)

// codeReviewPolicyOfRun runs a single-ticket build under a session config
// that sets code_review_policy: required, with the golden flag set's own
// -code-review-policy removed and extraFlags added, and returns the policy
// the workflow was started with.
func codeReviewPolicyOfRun(t *testing.T, extraFlags ...string) string {
	t.Helper()
	runID := fmt.Sprintf("review-policy-run-%d-%d", os.Getpid(), time.Now().UnixNano())
	address := sharedTemporalAddress(t)
	workspace := newFixtureRepo(t)
	dataDir, inputs := t.TempDir(), t.TempDir()
	var flags []string
	all := runInputGoldenFlags(t, workspace, dataDir, inputs, runID, address)
	for i := 0; i < len(all); i++ {
		if all[i] == "-code-review-policy" {
			i++
			continue
		}
		flags = append(flags, all[i])
	}
	cmd := factorydCommand(t, append(flags, extraFlags...)...)
	cmd.Env = append(os.Environ(), "FAKE_BUILD_APP_MODE=commit", "RUN_INPUT_GOLDEN_KEY=sk-test", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	cmd.Env = append(cmd.Env, isolatedSessionConfigEnv(t, runInputGoldenConfig(t)+"code_review_policy: required\n")...)
	out, _ := cmd.CombinedOutput()
	t.Logf("factoryd output:\n%s", out)
	policy, _ := startedRunWorkflowInput(t, address, runID)["code_review_policy"].(string)
	return policy
}

// A single-ticket run with no -code-review-policy takes the session config's
// code_review_policy, as a request's ticket build does through the worker.
func TestSingleTicketRunTakesCodeReviewPolicyFromSessionConfig(t *testing.T) {
	if got := codeReviewPolicyOfRun(t); got != "required" {
		t.Fatalf("code_review_policy = %q, want the session config's \"required\"", got)
	}
}

// The flag still wins over the session config.
func TestSingleTicketRunCodeReviewPolicyFlagOverridesSessionConfig(t *testing.T) {
	if got := codeReviewPolicyOfRun(t, "-code-review-policy", "off"); got != "off" {
		t.Fatalf("code_review_policy = %q, want the flag's \"off\"", got)
	}
}
