package main

import (
	"buildgate/internal/codereview"
	"flag"
	"regexp"
	"strings"
	"testing"
)

// Once finishFlags has run, no flag's usage string holds a backtick:
// flag.UnquoteUsage would print the first backquoted span as the name of the
// flag's value, and the usage strings here quote commands.
func TestNoFlagUsageContainsBackticks(t *testing.T) {
	t.Parallel()
	for _, c := range commandCatalog() {
		c.flags().VisitAll(func(f *flag.Flag) {
			if strings.Contains(f.Usage, "`") {
				t.Errorf("factoryd %s -%s: Usage still contains a backtick after finishFlags: %q", c.name, f.Name, f.Usage)
			}
		})
	}
}

// flagUsageLimit is how long a flag's usage string may be. The usage string
// is the flag's documentation, printed by -h and `factoryd help -json`.
const flagUsageLimit = 200

// sourceNameInUsage matches what a usage string must not lean on: a Go file,
// a package path of this module, or a pointer at another flag's help.
var sourceNameInUsage = regexp.MustCompile(`\w\.go\b|internal/|own flag|runMain| -- `)

// A usage string is short and says what the flag does, without naming the
// source: an operator and a coding agent both read it in -h.
func TestFlagUsageIsShort(t *testing.T) {
	t.Parallel()
	for _, c := range commandCatalog() {
		c.flags().VisitAll(func(f *flag.Flag) {
			if n := len(f.Usage); n > flagUsageLimit {
				t.Errorf("factoryd %s -%s: the usage string is %d characters, over %d: say what the flag does in one or two sentences", c.name, f.Name, n, flagUsageLimit)
			}
			if m := sourceNameInUsage.FindString(f.Usage); m != "" {
				t.Errorf("factoryd %s -%s: the usage string contains %q: say what the flag does, without naming the source or another flag's help", c.name, f.Name, m)
			}
		})
	}
}

// Both -code-review-policy flags default to the one codereview.DefaultPolicy.
func TestCodeReviewPolicyFlagsShareTheOneDefault(t *testing.T) {
	_, run := newRunFlags()
	_, worker := newWorkerFlags()
	if *run.codeReviewPolicy != codereview.DefaultPolicy || *worker.codeReviewPolicy != codereview.DefaultPolicy {
		t.Fatalf("defaults: run %q, worker %q, want %q", *run.codeReviewPolicy, *worker.codeReviewPolicy, codereview.DefaultPolicy)
	}
}
