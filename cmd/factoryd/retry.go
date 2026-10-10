package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"buildgate/internal/release"
	"buildgate/internal/request"
)

// retryMain implements `factoryd retry [-data-dir data] <id>`: put a
// quarantined, halted or resume_review request back in motion (see
// retryRequestFrom). Any other request state, or an id no request has, is
// refused with the reason.
// newRetryFlags builds `factoryd retry`'s FlagSet in isolation from
// parsing, so USAGE.md's doc-vs-flag drift test
// (TestUSAGEDocFlagsExistOnSubcommand) can enumerate its real flags without
// executing the command.
func newRetryFlags() (flags *flag.FlagSet, dataDir, reason, from, configPath *string) {
	flags = flag.NewFlagSet("retry", flag.ContinueOnError)
	dataDir = flags.String("data-dir", "data", "directory holding the request; must match `factoryd submit`'s. Must precede <id> on the command line (flag.Parse stops at the first positional argument)")
	// reason (an adversarial review, 2026-09-24) mirrors the
	// console's own required retry-reason field (POST /requests/{id}/retry's
	// body -- see retryRequestHandler) and reject's own -reason flag
	// (newRejectFlags): recorded on the request's appended History entry
	// via request.Retry, not dropped.
	reason = flags.String("reason", "", "why this request is being retried, recorded on its history")
	from = flags.String("from", retryFromAttempt, "where a rebuilt ticket starts: \"attempt\" continues from the quarantined attempt's commit when a build may be told what it failed on and it committed something (otherwise from the base commit); \"scratch\" always starts from the base commit. No effect on a retry that rebuilds no ticket")
	configPath = flags.String("config", "", "session config path (for -data-dir resolution); empty searches the default paths")
	plainFlagUsage(flags)
	return
}

// `factoryd retry -from`'s two values.
const (
	retryFromAttempt = "attempt"
	retryFromScratch = "scratch"
)

func retryMain(dp *deps, args []string) error {
	flags, dataDir, reason, from, configPath := newRetryFlags()
	if err := flags.Parse(args); err != nil {
		return err
	}
	if err := resolveDataDirFromSessionConfig(flags, dataDir, *configPath); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return fmt.Errorf("usage: factoryd retry [-config <path>] [-data-dir <dir>] [-reason ...] [-from attempt|scratch] <id>")
	}
	if *from != retryFromAttempt && *from != retryFromScratch {
		return fmt.Errorf("-from must be %q or %q, got %q", retryFromAttempt, retryFromScratch, *from)
	}
	id := flags.Arg(0)
	// The id is joined beneath <data-dir>/requests; a path-shaped one could
	// read an entry outside it. Same rule every other id in this data
	// directory follows.
	if err := release.SinglePathComponent("request id", id); err != nil {
		return err
	}
	req, err := request.Load(*dataDir, id)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("no request %q under %s", id, *dataDir)
		}
		return err
	}
	handled, err := retryRequestFrom(dp, *dataDir, req, *reason, *from == retryFromScratch, time.Now())
	if !handled {
		return fmt.Errorf("request %q is %s; only a quarantined, halted or resume_review request can be retried", id, req.State)
	}
	return err
}

// retryRequestFrom handles `factoryd retry <request-id>` for a request:
// if r is quarantined or halted, it delegates to the shared
// request.Retry -- the same function POST /requests/{id}/retry calls, so neither path can
// diverge on what counts as a legal retry (see request.Retry's own doc
// comment for exactly which shapes it recognizes) -- and reports
// handled=true. Otherwise reports handled=false so retryMain refuses the
// request with its state. fromScratch is `-from scratch`'s choice: a rebuilt
// ticket starts from the base commit whatever the quarantined attempt left.
func retryRequestFrom(dp *deps, dataDir string, r *request.Request, reason string, fromScratch bool, now time.Time) (handled bool, err error) {
	// resume_review is handled too, so request.Retry refuses it with the hint
	// to use `factoryd resume`, rather than a refusal that names nothing.
	if r.State != request.StateQuarantined && r.State != request.StateHalted && r.State != request.StateResumeReview {
		return false, nil
	}
	retry := request.Retry
	if fromScratch {
		retry = request.RetryFromScratch
	}
	updated, err := retry(dataDir, r.ID, currentOSUser(), reason, now, func(a0 string, a1 string) request.PROpenOutcome { return retryPullRequestOpener(dp, a0, a1) })
	if err != nil {
		return true, err
	}
	wakeAfterSave(dp, context.Background(), os.Stderr, dataDir, updated.ID)
	fmt.Println(updated.ID)
	return true, nil
}
