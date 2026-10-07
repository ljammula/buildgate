package hostcontrol

import "testing"

func TestFailedChecksInKeepsFailuresAndTheirFixes(t *testing.T) {
	output := "ok    docker daemon reachable\n" +
		"warn  Docker VM shares all of $HOME\n" +
		"      fix: narrow the mounts\n" +
		"FAIL  roles.execution: no configured route is usable\n" +
		"      fix: route codex: token expires soon; run `codex login`\n" +
		"ok    sandbox tmpfs size\n" +
		"\n20/24 checks passed\n"
	want := "FAIL  roles.execution: no configured route is usable\n" +
		"      fix: route codex: token expires soon; run `codex login`\n"
	if got := failedChecksIn(output); got != want {
		t.Errorf("failedChecksIn = %q, want %q", got, want)
	}
	if got := failedChecksIn("ok    everything\n"); got != "" {
		t.Errorf("failedChecksIn with no failure = %q, want empty", got)
	}
}
