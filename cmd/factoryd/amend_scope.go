package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"buildgate/internal/request"
)

// amendScopeMain implements `factoryd amend-scope -reason "<text>"
// [-data-dir <path>] <request-id> <file>...`: an operator widens ONE
// quarantined ticket's approved Allowed-Files: scope, re-pinning that
// ticket spec's approval hash, so `factoryd retry` can rebuild the ticket.
// Shares internal/request.AmendScope with nothing else yet (v1: CLI only,
// see AGENTS.md's out-of-scope note) -- the one recovery path for a build
// diff_scope quarantined over a file that is a legitimate part of the
// ticket but that the plan never listed.
// newAmendScopeFlags builds `factoryd amend-scope`'s FlagSet in isolation
// from parsing, so USAGE.md's doc-vs-flag drift test
// (TestUSAGEDocFlagsExistOnSubcommand) can enumerate its real flags without
// executing the command.
func newAmendScopeFlags() (flags *flag.FlagSet, dataDir, reason, configPath *string) {
	flags = flag.NewFlagSet("amend-scope", flag.ContinueOnError)
	dataDir = flags.String("data-dir", "data", "directory holding the queue the entry was submitted to; must match `factoryd submit`'s. Must precede <id> on the command line (flag.Parse stops at the first positional argument)")
	reason = flags.String("reason", "", "why this ticket's scope is being widened (required) -- recorded on the request's history. Must precede <request-id> on the command line (flag.Parse stops at the first positional argument)")
	configPath = flags.String("config", "", "session config path (for -data-dir resolution); empty searches the default paths")
	plainFlagUsage(flags)
	return
}

func amendScopeMain(dp *deps, args []string) error {
	flags, dataDir, reason, configPath := newAmendScopeFlags()
	if err := flags.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*reason) == "" {
		flags.Usage()
		return fmt.Errorf("-reason is required")
	}
	positional := flags.Args()
	if len(positional) < 2 {
		flags.Usage()
		return fmt.Errorf(`usage: factoryd amend-scope -reason "<text>" [-config <path>] [-data-dir <path>] <request-id> <file>...`)
	}
	if err := resolveDataDirFromSessionConfig(flags, dataDir, *configPath); err != nil {
		return err
	}

	id := positional[0]
	files := positional[1:]
	r, err := request.AmendScope(*dataDir, id, currentOSUser(), *reason, files, time.Now())
	if err != nil {
		return fmt.Errorf("amend scope for request %q: %w", id, err)
	}
	wakeAfterSave(dp, context.Background(), os.Stderr, *dataDir, r.ID)
	if _, err := fmt.Fprintf(os.Stdout, "request %s: ticket %d Allowed-Files widened with %s\n", r.ID, r.TicketIndex, strings.Join(files, ", ")); err != nil {
		return fmt.Errorf("print amend-scope result: %w", err)
	}
	return nil
}
