package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sync"
	"time"

	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
)

// cancelMain implements `factoryd cancel [-reason "<text>"] [-data-dir
// <path>] <request-id>`: withdraws a non-terminal request via
// internal/request.Cancel -- Request.Cancel's first real caller; POST
// /requests/{id}/cancel (internal/api's own handler) shares it the same
// way approveMain/rejectMain share Approve/Reject, so neither path can
// diverge on what counts as a legal cancel.
// newCancelFlags builds `factoryd cancel`'s FlagSet in isolation from
// parsing, so USAGE.md's doc-vs-flag drift test
// (TestUSAGEDocFlagsExistOnSubcommand) can enumerate its real flags without
// executing the command.
func newCancelFlags() (flags *flag.FlagSet, dataDir, reason, configPath *string) {
	flags = flag.NewFlagSet("cancel", flag.ContinueOnError)
	dataDir = flags.String("data-dir", "data", "directory containing durable request records")
	reason = flags.String("reason", "", "why the request is being cancelled (optional) -- recorded on the request's history. Must precede <request-id> on the command line (flag.Parse stops at the first positional argument)")
	configPath = flags.String("config", "", "session config path (for -data-dir resolution); empty searches the default paths")
	plainFlagUsage(flags)
	return
}

func cancelMain(dp *deps, args []string) error {
	flags, dataDir, reason, configPath := newCancelFlags()
	if err := flags.Parse(args); err != nil {
		return err
	}
	positional := flags.Args()
	if len(positional) != 1 {
		flags.Usage()
		return fmt.Errorf(`usage: factoryd cancel [-reason "<text>"] [-config <path>] [-data-dir <path>] <request-id>`)
	}
	if err := resolveDataDirFromSessionConfig(flags, dataDir, *configPath); err != nil {
		return err
	}
	id := positional[0]

	unlock, err := request.Lock(*dataDir, id)
	if err != nil {
		return fmt.Errorf("cancel request %q: %w", id, err)
	}
	// Released before the wake below, so a slow Temporal never holds the
	// request lock the worker also takes.
	release := sync.OnceFunc(unlock)
	defer release()
	r, err := request.Load(*dataDir, id)
	if err != nil {
		return fmt.Errorf("cancel request %q: %w", id, err)
	}
	if err := r.Cancel(currentOSUser(), *reason, time.Now()); err != nil {
		return fmt.Errorf("cancel request %q: %w", id, err)
	}
	if err := r.Save(*dataDir); err != nil {
		return fmt.Errorf("cancel request %q: %w", id, err)
	}
	release()
	// Cancelling is the decision that ends a lost build's wait: reap every
	// worktree of this request kept for a resume.
	if err := requestdriver.ClearKeptRunsOfRequest(*dataDir, r.ID); err != nil {
		fmt.Fprintf(os.Stderr, "request %s: could not discard a kept worktree: %v\n", r.ID, err)
	}
	wakeAfterSave(dp, context.Background(), os.Stderr, *dataDir, r.ID)
	if _, err := fmt.Fprintf(os.Stdout, "request %s: state=%s\n", r.ID, r.State); err != nil {
		return fmt.Errorf("print cancel result: %w", err)
	}
	return nil
}
