package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/user"
	"time"

	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
)

// approveMain implements `factoryd approve [-data-dir <path>]
// <request-id>` (USAGE.md §11): advances a request out of spec_review
// (to planning) or plan_review (to building). Shares its actual approval
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
	configPath = flags.String("config", "", "session config path or profile name, for the data dir; empty: the active profile, else the first of ~/.config/factoryd/config.yml and ~/.factory/config.yml that exists")
	finishFlags(flags)
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

	opts, err := gateExpiryOptions(*configPath)
	if err != nil {
		return fmt.Errorf("approve request %q: %w", positional[0], err)
	}
	// nil expectedSHA256: this CLI has no prior fetch of its own to bind
	// the approval to (unlike the console, see Approve's own doc
	// comment), so it keeps the unconditional behavior.
	r, err := request.Approve(*dataDir, positional[0], currentOSUser(), time.Now(), nil, opts...)
	if err != nil {
		notifyIfGateExpired(*dataDir, positional[0], err)
		return fmt.Errorf("approve request %q: %w", positional[0], err)
	}
	wakeAfterSave(dp, context.Background(), os.Stderr, *dataDir, r.ID)
	if _, err := fmt.Fprintf(os.Stdout, "request %s: state=%s\n", r.ID, r.State); err != nil {
		return fmt.Errorf("print approve result: %w", err)
	}
	return nil
}

// rejectMain implements `factoryd reject -reason "<text>" [-to plan|spec]
// [-data-dir <path>] <request-id>` (USAGE.md §11): sends a request in
// spec_review or plan_review back to the prior drafting
// state, recording -reason on the request's Rejections so the redraft sees it
// (never in request.md). Shares internal/request.Reject with POST
// /requests/{id}/reject, the same way approveMain shares
// internal/request.Approve.
//
// -comments-file takes the place of -reason for a review of the spec or the
// plan: a JSON list of comments on passages (request.RejectionAnchor). With
// only comments, the redraft changes just the commented sections.
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
func newRejectFlags() (flags *flag.FlagSet, dataDir, reason, to, configPath, commentsFile *string) {
	flags = flag.NewFlagSet("reject", flag.ContinueOnError)
	dataDir = flags.String("data-dir", "data", "directory containing durable request records")
	reason = flags.String("reason", "", "why the request is being rejected; the next redraft is given it")
	to = flags.String("to", "", "Send a quarantined or halted request back to \"plan\" or \"spec\" to be redrafted. Refused in a review state and once any ticket is accepted. Empty rejects the document at the current review gate.")
	configPath = flags.String("config", "", "session config path or profile name, for the data dir; empty: the active profile, else the first of ~/.config/factoryd/config.yml and ~/.factory/config.yml that exists")
	commentsFile = flags.String("comments-file", "", "JSON list of comments on passages of the spec or tickets; only the commented sections are redrafted. A -reason given too asks for a whole redraft. - reads standard input.")
	finishFlags(flags)
	return
}

func rejectMain(dp *deps, args []string) error {
	flags, dataDir, reason, to, configPath, commentsFile := newRejectFlags()
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *reason == "" && *commentsFile == "" {
		flags.Usage()
		return fmt.Errorf("-reason or -comments-file is required")
	}
	if *commentsFile != "" && *to != "" {
		return fmt.Errorf("-comments-file comments on a review's document, and -to sends a quarantined or halted request back: give one")
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

	opts, err := gateExpiryOptions(*configPath)
	if err != nil {
		return fmt.Errorf("reject request %q: %w", id, err)
	}
	var anchors []request.RejectionAnchor
	if *commentsFile != "" {
		var err error
		if anchors, err = readCommentsFile(*commentsFile, os.Stdin); err != nil {
			return err
		}
	}
	r, err := request.RejectAnchored(*dataDir, id, by, *reason, anchors, time.Now(), opts...)
	if err != nil {
		notifyIfGateExpired(*dataDir, id, err)
		return fmt.Errorf("reject request %q: %w", id, err)
	}
	wakeAfterSave(dp, context.Background(), os.Stderr, *dataDir, r.ID)
	if _, err := fmt.Fprintf(os.Stdout, "request %s: state=%s\n", r.ID, r.State); err != nil {
		return fmt.Errorf("print reject result: %w", err)
	}
	return nil
}

// readCommentsFile reads `reject -comments-file`: a JSON array of
// {path, section, item, quote, note}, from the named file or, for "-", stdin.
// Unknown fields are refused so a misspelt "quote" cannot silently drop the
// binding of a comment to its passage.
func readCommentsFile(name string, stdin io.Reader) ([]request.RejectionAnchor, error) {
	var data []byte
	var err error
	if name == "-" {
		data, err = io.ReadAll(stdin)
	} else {
		data, err = os.ReadFile(name)
	}
	if err != nil {
		return nil, fmt.Errorf("read -comments-file: %w", err)
	}
	var anchors []request.RejectionAnchor
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&anchors); err != nil {
		return nil, fmt.Errorf("-comments-file must be a JSON list of {path, section, item, quote, note}: %w", err)
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return nil, fmt.Errorf("-comments-file must hold one JSON list and nothing after it")
	}
	if len(anchors) == 0 {
		return nil, fmt.Errorf("-comments-file holds no comments")
	}
	return anchors, nil
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

// gateExpiryOptions makes a decision refuse a review gate past the session
// config's review_gate_expiry, atomically under the request's lock
// (request.WithGateExpiry). A config that does not load refuses the decision:
// a gate's expiry is not guessed.
func gateExpiryOptions(configPath string) ([]request.DecisionOption, error) {
	settings, err := loadSettingsForConfig(configPath)
	if err != nil {
		return nil, fmt.Errorf("the session config cannot be read, so review_gate_expiry is unknown and the decision is refused: %w", err)
	}
	return []request.DecisionOption{request.WithGateExpiry(settings.ReviewGateExpiry)}, nil
}

// notifyIfGateExpired tells the operator when a refused decision halted its
// request because the review gate had expired.
func notifyIfGateExpired(dataDir, id string, err error) {
	if !errors.Is(err, request.ErrGateExpired) {
		return
	}
	if nerr := requestdriver.NotifyGateExpired(dataDir, id, time.Now()); nerr != nil {
		fmt.Fprintf(os.Stderr, "request %s: could not send the expiry notification: %v\n", id, nerr)
	}
}
