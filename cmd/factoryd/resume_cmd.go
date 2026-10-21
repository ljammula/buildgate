package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
)

// newResumeFlags builds `factoryd resume`'s FlagSet in isolation from
// parsing, so USAGE.md's doc-vs-flag drift test
// (TestUSAGEDocFlagsExistOnSubcommand) can enumerate its real flags without
// executing the command.
func newResumeFlags() (flags *flag.FlagSet, dataDir, from, configPath *string) {
	flags = flag.NewFlagSet("resume", flag.ContinueOnError)
	dataDir = flags.String("data-dir", "data", "directory containing durable request records")
	from = flags.String("from", request.ResumeRound, "How a lost build continues. \"round\" continues in its kept worktree from the last completed round. \"scratch\" discards that worktree and rebuilds the ticket. Any other lost step simply runs again.")
	configPath = flags.String("config", "", "session config path or profile name, for the data dir; empty: the active profile, else the first of ~/.config/factoryd/config.yml and ~/.factory/config.yml that exists")
	finishFlags(flags)
	return
}

// resumeMain implements `factoryd resume [-from round|scratch] <request-id>`:
// the decision for a request waiting in resume_review because the worker
// running its step stopped (request.ResumeRequest, the function POST
// /requests/{id}/resume shares). For a lost build and -from round it first
// checks the preconditions of continuing in the kept worktree (no container
// of the lost run alive, the history intact) and refuses, naming
// `-from scratch` and `factoryd cancel`, when they do not hold. Cancelling
// goes through `factoryd cancel`.
func resumeMain(dp *deps, args []string) error {
	return resumeMainWith(dp, args, requestdriver.ResumeGate{Sandboxes: dp.sandbox.runtime()})
}

// resumeMainWith is resumeMain consulting gate for its resume checks.
func resumeMainWith(dp *deps, args []string, gate requestdriver.ResumeGate) error {
	flags, dataDir, from, configPath := newResumeFlags()
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		flags.Usage()
		return fmt.Errorf("usage: factoryd resume [-from round|scratch] [-config <path>] [-data-dir <path>] <request-id>")
	}
	if *from != request.ResumeRound && *from != request.ResumeScratch {
		return fmt.Errorf("-from must be %q or %q, got %q", request.ResumeRound, request.ResumeScratch, *from)
	}
	if err := resolveDataDirFromSessionConfig(flags, dataDir, *configPath); err != nil {
		return err
	}
	settings, err := loadSettingsForConfig(*configPath)
	if err != nil {
		return err
	}
	id := flags.Arg(0)
	checked := ""
	updated, err := request.ResumeRequest(*dataDir, id, *from, currentOSUser(), time.Now(), func(r *request.Request) ([]string, error) {
		reasons, err := gate.RequestResumeRefusals(context.Background(), *dataDir, settings.SandboxDocker, r)
		if err == nil && len(reasons) == 0 && r.Resume != nil && r.Resume.LostRunID != "" {
			checked = fmt.Sprintf("resume preconditions hold for run %s: no sandbox container alive, worktree HEAD matches its round state", r.Resume.LostRunID)
		}
		return reasons, err
	}, request.WithGateExpiry(settings.ReviewGateExpiry))
	if err != nil {
		notifyIfGateExpired(*dataDir, id, err)
		return err
	}
	// A decision that continues no kept build (a drafting or planning rerun,
	// or pr_review) ends the wait of any worktree kept for this request.
	if updated.State != request.StateBuilding {
		if err := requestdriver.ClearKeptRunsOfRequest(*dataDir, updated.ID); err != nil {
			fmt.Fprintf(os.Stderr, "request %s: could not discard a kept worktree: %v\n", updated.ID, err)
		}
	}
	wakeAfterSave(dp, context.Background(), os.Stderr, *dataDir, updated.ID)
	if checked != "" {
		fmt.Println(checked)
	}
	fmt.Printf("request %s: state=%s (resume -from %s)\n", updated.ID, updated.State, *from)
	return nil
}
