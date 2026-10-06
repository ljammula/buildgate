package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/user"
	"time"

	"buildgate/internal/request"
)

// approveMain implements `factoryd approve [-data-dir <path>]
// <request-id>` (USAGE.md §11): advances a request out of spec_review
// (to planning, or to oracle_drafting for a -draft-oracles request),
// oracle_review (to planning) or plan_review (to building). Shares its actual approval
// logic -- internal/request.Approve -- with POST /requests/{id}/approve
// (internal/api's own handler), so neither path can diverge on what
// counts as a legal approval.
// newApproveFlags builds `factoryd approve`'s FlagSet in isolation from
// parsing, so USAGE.md's doc-vs-flag drift test
// (TestUSAGEDocFlagsExistOnSubcommand) can enumerate its real flags without
// executing the command.
func newApproveFlags() (flags *flag.FlagSet, dataDir, configPath *string) {
	flags = flag.NewFlagSet("approve", flag.ContinueOnError)
	dataDir = flags.String("data-dir", "data", "directory containing durable request records")
	configPath = flags.String("config", "", "session config path (for -data-dir resolution); empty searches the default paths")
	plainFlagUsage(flags)
	return
}

func approveMain(dp *deps, args []string) error {
	flags, dataDir, configPath := newApproveFlags()
	if err := flags.Parse(args); err != nil {
		return err
	}
	positional := flags.Args()
	if len(positional) != 1 {
		flags.Usage()
		return fmt.Errorf("usage: factoryd approve [-config <path>] [-data-dir <path>] <request-id>")
	}
	// The session config's data_dir, as submit/retry/status/watch already
	// honour:
	// quickstart's printed `factoryd approve <id>` failed against a
	// non-default data_dir because this hardcoded "data".
	if err := resolveDataDirFromSessionConfig(flags, dataDir, *configPath); err != nil {
		return err
	}

	// nil expectedSHA256: this CLI has no prior fetch of its own to bind
	// the approval to (unlike the console, see Approve's own doc
	// comment), so it keeps the unconditional behavior.
	r, err := request.Approve(*dataDir, positional[0], currentOSUser(), time.Now(), nil)
	if err != nil {
		return fmt.Errorf("approve request %q: %w", positional[0], err)
	}
	wakeAfterSave(dp, context.Background(), os.Stderr, *dataDir, r.ID)
	if _, err := fmt.Fprintf(os.Stdout, "request %s: state=%s\n", r.ID, r.State); err != nil {
		return fmt.Errorf("print approve result: %w", err)
	}
	if r.OracleSkipWarning != "" && r.State == request.StatePlanning {
		if _, err := fmt.Fprintf(os.Stdout, "warning: %s\n", r.OracleSkipWarning); err != nil {
			return fmt.Errorf("print approve warning: %w", err)
		}
	}
	return nil
}

// rejectMain implements `factoryd reject -reason "<text>" [-to plan|spec]
// [-data-dir <path>] <request-id>` (USAGE.md §11): sends a request in
// spec_review, oracle_review or plan_review back to the prior drafting
// state, appending -reason to request.md so the redraft sees it (an
// oracle_review rejection is recorded on the request's Rejections instead,
// never in request.md). Shares internal/request.Reject with POST
// /requests/{id}/reject, the same way approveMain shares
// internal/request.Approve.
//
// -to is meaningful only for a quarantined or halted request: it routes
// to internal/request.SendBack instead of Reject, sending the request
// back to planning (the default) or spec drafting rather than the prior
// drafting state a review-state rejection returns to. Given for a
// request in a review state, it is refused with a clear error rather than
// silently ignored -- Reject's own state has no "target" concept for -to
// to mean anything, and a flag the operator explicitly set should never
// be quietly dropped.
// newRejectFlags builds `factoryd reject`'s FlagSet in isolation from
// parsing, so USAGE.md's doc-vs-flag drift test
// (TestUSAGEDocFlagsExistOnSubcommand) can enumerate its real flags without
// executing the command.
func newRejectFlags() (flags *flag.FlagSet, dataDir, reason, to, configPath *string) {
	flags = flag.NewFlagSet("reject", flag.ContinueOnError)
	dataDir = flags.String("data-dir", "data", "directory containing durable request records")
	reason = flags.String("reason", "", "why the request is being rejected (required) -- appended to request.md so the next drafting pass sees it. Must precede <request-id> on the command line (flag.Parse stops at the first positional argument)")
	to = flags.String("to", "", "for a quarantined or halted request only: send it back to \"plan\" (default) or \"spec\" instead of the prior drafting state -- refused if the request is in a review state instead")
	configPath = flags.String("config", "", "session config path (for -data-dir resolution); empty searches the default paths")
	plainFlagUsage(flags)
	return
}

func rejectMain(dp *deps, args []string) error {
	flags, dataDir, reason, to, configPath := newRejectFlags()
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *reason == "" {
		flags.Usage()
		return fmt.Errorf("-reason is required")
	}
	positional := flags.Args()
	if len(positional) != 1 {
		flags.Usage()
		return fmt.Errorf(`usage: factoryd reject -reason "<text>" [-to plan|spec] [-config <path>] [-data-dir <path>] <request-id>`)
	}
	// See approveMain's matching comment.
	if err := resolveDataDirFromSessionConfig(flags, dataDir, *configPath); err != nil {
		return err
	}

	id := positional[0]
	by := currentOSUser()
	if *to != "" {
		target := request.SendBackTarget(*to)
		if !target.Valid() {
			return fmt.Errorf("-to must be %q or %q, got %q", request.SendBackToPlan, request.SendBackToSpec, *to)
		}
		r, err := request.SendBack(*dataDir, id, by, *reason, target, time.Now())
		if err != nil {
			return fmt.Errorf("send back request %q: %w", id, err)
		}
		wakeAfterSave(dp, context.Background(), os.Stderr, *dataDir, r.ID)
		if _, err := fmt.Fprintf(os.Stdout, "request %s: state=%s\n", r.ID, r.State); err != nil {
			return fmt.Errorf("print reject result: %w", err)
		}
		return nil
	}

	r, err := request.Reject(*dataDir, id, by, *reason, time.Now())
	if err != nil {
		return fmt.Errorf("reject request %q: %w", id, err)
	}
	wakeAfterSave(dp, context.Background(), os.Stderr, *dataDir, r.ID)
	if _, err := fmt.Fprintf(os.Stdout, "request %s: state=%s\n", r.ID, r.State); err != nil {
		return fmt.Errorf("print reject result: %w", err)
	}
	return nil
}

// currentOSUser resolves "by" for a CLI-driven approve/reject: the OS
// user running this process, falling back to $USER when os/user.Current
// fails (as it can in a minimal container with no /etc/passwd entry for
// the running uid).
func currentOSUser() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	if v := os.Getenv("USER"); v != "" {
		return v
	}
	return "unknown"
}
