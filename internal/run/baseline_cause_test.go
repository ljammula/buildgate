package run

import (
	"strings"
	"testing"
)

func TestBaselineVerifyCauseHasItsOwnWords(t *testing.T) {
	for _, tc := range []struct {
		b       BaselineVerify
		summary string
		advice  string
	}{
		{BaselineVerify{Cause: BaselineCauseMissingModule, CauseDetail: "pytest"},
			`failed: exit 1; the sandbox image lacks the Python module "pytest"`, "setup: command"},
		{BaselineVerify{Cause: BaselineCauseMissingProgram, CauseDetail: "mvn"},
			`failed: exit 1; the sandbox image lacks the program "mvn"`, "sandbox image the project runs in"},
		{BaselineVerify{Cause: BaselineCauseDependencyUnavailable, CauseDetail: "org.apache.maven.surefire:surefire-junit4:jar:2.22.2"},
			`failed: exit 1; cannot get the dependency "org.apache.maven.surefire:surefire-junit4:jar:2.22.2" (no network or cache for it)`, "setup: command"},
		{BaselineVerify{Cause: BaselineCauseMissingPath, CauseDetail: "tests/X.csproj"},
			`failed: exit 1; the command cannot find "tests/X.csproj"`, "verify_command in .factory.yml"},
		{BaselineVerify{Cause: BaselineCauseMissingPath},
			`failed: exit 1; the command cannot find a file it needs`, "Allowed-Files"},
		{BaselineVerify{FirstError: "boom"}, `failed: exit 1; first error in log: "boom"`, "Fix the verify command or the sandbox image"},
	} {
		b := tc.b
		b.ExitCode = 1
		if got := b.Summary(); got != tc.summary {
			t.Errorf("Summary = %q, want %q", got, tc.summary)
		}
		advice := b.HaltAdvice()
		if !strings.HasPrefix(advice, "No model call was made:") || !strings.Contains(advice, tc.advice) {
			t.Errorf("HaltAdvice = %q, want it to hold %q", advice, tc.advice)
		}
		if got := len("halted before the build: baseline verify ") + len(b.Summary()); got > 200 {
			t.Errorf("triage sentence for %q is %d bytes", b.Cause, got)
		}
	}
}
