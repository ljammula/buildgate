package main

import (
	"flag"
	"strings"
	"testing"
)

// TestNoFlagUsageContainsBackticks is the regression test for the
// flag.UnquoteUsage misrendering fixed by plainFlagUsage (flag_usage.go):
// once plainFlagUsage has run, no flag's Usage string may still contain a
// backtick, since flag.UnquoteUsage treats the first backquoted span in a
// Usage string as that flag's argument placeholder name (see its own doc
// comment) -- almost every backtick in this package's flag descriptions
// quotes a shell command, not an intended placeholder.
//
// Reuses usage_doc_flags_test.go's own docTrackedCommands registry, which
// already builds every documented subcommand's real FlagSet via its
// new<Cmd>Flags constructor without running the command -- exactly the
// enumeration this test needs, and the same one TestUSAGEDocFlagsExistOnSubcommand
// relies on staying complete. doctorMain's three separately registered
// flags (-yes, -list-models, -notify-test) are not reachable this way; its
// own plainFlagUsage call covers them.
func TestNoFlagUsageContainsBackticks(t *testing.T) {
	t.Parallel()
	for _, cmd := range sortedKeys(docTrackedCommands) {
		fs := docTrackedCommands[cmd]()
		fs.VisitAll(func(f *flag.Flag) {
			if strings.Contains(f.Usage, "`") {
				t.Errorf("factoryd %s -%s: Usage still contains a backtick after plainFlagUsage: %q", cmd, f.Name, f.Usage)
			}
		})
	}
}
