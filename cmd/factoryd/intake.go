package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"buildgate/internal/projectconfig"
)

// readFileIfExists reads path's content, distinguishing "empty file" from
// "no file here yet" -- the second return is false only for the latter, so
// a caller comparing before/after snapshots can tell "this file was
// created by the invocation in between" apart from "this file already
// existed, empty, and got overwritten" (both are freshness, but the
// distinction matters for correctly reading the first case, where a
// direct content-equality check has nothing meaningful to compare against).
func readFileIfExists(path string) (content []byte, existed bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	return b, true
}

// specStatusRE mirrors goal_pilot.py's own SPEC_STATUS_RE
// (`re.compile(r"^STATUS:\s*(\S+)")`) exactly, byte for byte -- see
// parseSpecFirstLineStatus.
var specStatusRE = regexp.MustCompile(`^STATUS:\s*(\S+)`)

// parseSpecFirstLineStatus reads specContent's status the same way
// goal_pilot.py's own read_spec_status() does: match specStatusRE against
// line 1 only, case-sensitively, returning "" if it doesn't match (empty
// content, no leading "STATUS:", wrong case, or a value that isn't the
// very first thing on the line). Deliberately not a substring search
// anywhere in the file -- that mismatch (a fixture doing `grep -q
// "STATUS: FROZEN"` instead of this) is exactly what let a real Opus
// review pass, 2026-09-04, catch a fixture more permissive than the real
// script it stands in for, undetectable by the test it was backing.
func parseSpecFirstLineStatus(specContent []byte) string {
	nl := bytes.IndexByte(specContent, '\n')
	firstLine := specContent
	if nl >= 0 {
		firstLine = specContent[:nl]
	}
	m := specStatusRE.FindSubmatch(firstLine)
	if m == nil {
		return ""
	}
	return string(m[1])
}

// snapshotMarkdownFiles reads every *.md file directly inside dir (not
// subdirectories) into a name -> content map, for a caller to diff
// against the same directory's contents later. A missing dir snapshots as
// empty, not an error -- the caller's own later os.ReadDir call is what
// reports that as a real failure if the directory still doesn't exist by
// then.
// snapshotMarkdownFiles returns an error for any os.ReadDir failure other
// than the directory simply not existing yet -- a *transient* read error
// (found via a real Opus review pass, 2026-09-04, A4) must not silently
// collapse to an empty snapshot: intakeReportSpecDraft's whole freshness
// check exists specifically to catch stale pre-existing ticket files
// being misreported as freshly drafted, and an empty ticketsBefore makes
// every pre-existing ticket compare as "new" -- precisely the failure
// mode this function was added to prevent. A missing directory is the one
// genuinely benign case (a truly fresh pilot dir has no spec/tickets/
// yet), so that alone still returns an empty snapshot with no error.
func snapshotMarkdownFiles(dir string) (map[string][]byte, error) {
	snapshot := map[string][]byte{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return snapshot, nil
		}
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		if content, err := os.ReadFile(filepath.Join(dir, entry.Name())); err == nil {
			snapshot[entry.Name()] = content
		}
	}
	return snapshot, nil
}

// intakeMain automates the first mile of the idea -> spec -> contract ->
// tickets -> factoryd handoff pi-harness-hardening's own goal_pilot.py
// already implements, but that nothing in this repo ever invoked (found
// via the 2026-09-03 Opus factory-pipeline review, item C5): factoryd's
// own entry precondition was "a ticket file already exists on disk," with
// no automated path to producing one.
//
// It drives goal_pilot.py with -non-interactive and, always, --checkpoint
// review (overriding goal_pilot.py's own DEFAULT_CHECKPOINT="skip" --
// see below), which deterministically stops at one of two points:
//
//   - A fresh pilot dir (spec.md not yet FROZEN): goal_pilot.py's own
//     step 3, the spec-freeze checkpoint, documented there as "hard stop,
//     never skippable" -- refusing to auto-approve it rather than
//     guessing a human's judgment call. That is success for intakeMain's
//     own job, not failure: goal_pilot.py's step 2 (draft spec.md +
//     spec/tickets/*.md via /spec-plan, inject the
//     Verify-Command:/Allowed-Files:/Required-Changed-Files: ticketspec
//     keys, write an ARCHITECTURE.md structural stub) already completed
//     and persisted to disk before that checkpoint ever runs.
//     spec/contract.md does NOT exist yet at this point -- it's
//     goal_pilot.py's own step 4, after spec approval -- so a run
//     against these tickets still needs either -skip-project-check or
//     the spec approved and goal_pilot.py continuing through
//     contract-plan before factoryd's mandatory project-bootstrap
//     preflight can pass.
//   - A pilot dir whose spec.md is already FROZEN on disk (the exact
//     signal goal_pilot.py's own main() checks -- `if status != "FROZEN"`
//     -- to decide whether to re-run steps 2-3 at all): a second
//     `factoryd intake` invocation against the same -pilot-dir, made
//     only after a human edited spec.md's STATUS line by hand, skips
//     straight to step 4 (/contract-plan, drafting spec/contract.md and
//     spec/acceptance/) and then halts at step 5, the acceptance-suite
//     checkpoint -- "the highest-stakes artifact in the pipeline," per
//     /contract-plan's own words, and every bit as much a hard stop as
//     step 3.
//
// Either halt is intakeMain's own success signal; intakeMain never
// approves either checkpoint itself, or the third, always-on checkpoint
// after step 5 (post-ticket-001, reached only once ticket_runner.py has
// actually built ticket 001 -- step 6, which this always stays clear of
// by forcing --checkpoint review so step 5 halts non-interactively
// instead of goal_pilot.py's own default silently proceeding into that
// build). Both recognized halts are goal_pilot.py's own designed-in
// behavior, driven only by a human's own prior edit to spec.md on disk
// (never inferred or auto-approved by this function) -- re-invoking
// intake after that edit is not automating past a checkpoint, it's
// picking the pipeline back up from where the human explicitly left it.

// withIntakeLock runs fn while holding an advisory, cross-process
// exclusive lock scoped to one -pilot-dir, so two concurrent `factoryd
// intake` invocations against the same pilot dir can't interleave two
// goal_pilot.py subprocesses writing the same spec/ tree (found via a
// real Opus review pass, 2026-09-04, A2 -- see intakeMain's own doc
// comment on the failure mode this prevents). Uses acquireExclusiveLock
// (worker_config.go), the same fail-fast `syscall.Flock` helper
// acquireWorkerLock is built on: a second concurrent invocation
// against the same pilot dir should see that immediately, not sit
// silently behind the first one until it releases the lock, which the
// blocking wait this used previously (matching
// internal/release.withKillSwitchLock) would have done instead.
func withIntakeLock(pilotDir string, fn func() error) error {
	// Lock file lives beside -pilot-dir, not inside it (found via a real
	// `factoryd intake` run against a brand-new -pilot-dir, 2026-09-05):
	// spec-plan.md's own step-0 scaffold check treats a -pilot-dir that
	// "exists, is non-empty, and has no Makefile" as a hard stop -- and a
	// lock file created inside a truly fresh, otherwise-empty -pilot-dir
	// tripped exactly that check on intakeMain's very first invocation,
	// before goal_pilot.py ever got a chance to scaffold anything. Same
	// dotted-sibling convention goal_pilot.py itself already uses for its
	// raw-spec-input scratch file (`pilot_dir.parent /
	// ".<pilot_dir.name>.goal-pilot-raw-spec-input.txt"`), so this needs
	// no new directory and matches an existing precedent instead of
	// inventing another lock-file location scheme.
	//
	// pilotDir must be canonicalized first (Codex review of PR #56): a
	// relative -pilot-dir of exactly "." has filepath.Dir/Base both also
	// return ".", so the naive version of this derived a lock path back
	// inside pilotDir itself -- recreating the exact scaffold-check
	// failure this fix exists to close. Resolving to an absolute,
	// symlink-resolved path first also keeps two invocations that name
	// the same pilot dir through a symlink and its target serialized on
	// the same lock file, same as resolveExistingAncestor's own doc
	// comment already established for this file's containment checks.
	absPilotDir, err := filepath.Abs(pilotDir)
	if err != nil {
		return fmt.Errorf("resolve absolute pilot dir %s: %w", pilotDir, err)
	}
	resolvedPilotDir, err := resolveExistingAncestor(absPilotDir)
	if err != nil {
		return fmt.Errorf("resolve pilot dir %s: %w", absPilotDir, err)
	}
	parent := filepath.Dir(resolvedPilotDir)
	if err := os.MkdirAll(parent, 0o750); err != nil {
		return fmt.Errorf("create pilot dir parent %s: %w", parent, err)
	}
	lockPath := filepath.Join(parent, "."+filepath.Base(resolvedPilotDir)+".intake.lock")
	contendedMsg := fmt.Sprintf("another factoryd intake is already running against pilot dir %s: stop it before starting a second one", resolvedPilotDir)
	unlock, err := acquireExclusiveLock(lockPath, contendedMsg)
	if err != nil {
		return err
	}
	defer unlock()
	return fn()
}

// newIntakeFlags builds `factoryd intake`'s FlagSet in isolation from
// parsing, so USAGE.md's doc-vs-flag drift test
// (TestUSAGEDocFlagsExistOnSubcommand) can enumerate its real flags without
// executing the command.
func newIntakeFlags() (flags *flag.FlagSet, specInput, pilotDir, goalPilotInterpreter, goalPilotScript *string, timeout *time.Duration, checkpoint, verifyCommand *string) {
	flags = flag.NewFlagSet("intake", flag.ContinueOnError)
	specInput = flags.String("spec-input", "", "path to a rough-input file, or literal rough-input text (required) -- passed straight through to goal_pilot.py's own --spec-input")
	pilotDir = flags.String("pilot-dir", "", "pilot directory to scaffold spec/tickets into; a later `factoryd <run> -workspace` should be this same directory once the spec is approved (required)")
	goalPilotInterpreter = flags.String("goal-pilot-interpreter", "python3", "interpreter used to invoke -goal-pilot-script")
	goalPilotScript = flags.String("goal-pilot-script", "", "path to goal_pilot.py (default: this version's embedded harness copy)")
	timeout = flags.Duration("timeout", 50*time.Minute, "timeout for the drafting step; goal_pilot.py's own /spec-plan step is bounded at 45m internally, this adds process-overhead margin")
	checkpoint = flags.String("checkpoint", "review", "passed through to goal_pilot.py's own --checkpoint (review|skip). Defaults to review, NOT goal_pilot.py's own DEFAULT_CHECKPOINT=skip: skip mode does not halt at goal_pilot.py's step 5 in --non-interactive mode at all, so a resumed invocation (spec.md already FROZEN) would proceed unattended straight into ticket_runner.py building ticket 001 -- exactly the kind of checkpoint-skipping this command exists to never do on its own. Pass -checkpoint skip only if you want that build to happen; intakeMain's own success detection below does not recognize its later halt")
	verifyCommand = flags.String("verify-command", "", "passed through to goal_pilot.py's own --verify-command, overriding its hardcoded \"make verify && make verify-full\" default when it injects a drafted ticket's Verify-Command: key. That default assumes the target repo's own Makefile defines both targets -- true only for a repo this pipeline scaffolds from scratch. An existing repo being onboarded (factoryd onboard / -preflight-profile brownfield) has no reason to define verify-full at all, so pass its real verify command here (typically the same one that ends up in its .factory.yml) or every drafted ticket after 001 declares a Verify-Command that can never pass, permanently quarantining an otherwise-correct run")
	plainFlagUsage(flags)
	return
}

func intakeMain(args []string) error {
	flags, specInput, pilotDir, goalPilotInterpreter, goalPilotScript, timeout, checkpoint, verifyCommand := newIntakeFlags()
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *specInput == "" || *pilotDir == "" {
		flags.Usage()
		return fmt.Errorf("-spec-input and -pilot-dir are required")
	}
	// Falls back to -pilot-dir's own already-recorded .factory.yml
	// verify_command when -verify-command was left at its default (found
	// via adversarial review, 2026-09-11): the documented common case is
	// -pilot-dir and a later `factoryd <run> -workspace` naming the same
	// directory (this flag's own help text above says so), so an operator
	// who already ran `factoryd onboard`/`init -write-factory-yml` against
	// this exact directory has a real verify command recorded there and
	// forgetting the separate -verify-command flag on `intake` would
	// otherwise still fall through to goal_pilot.py's unsatisfiable
	// hardcoded default -- the same failure class -verify-command exists
	// to prevent, reappearing because this one caller wasn't config-aware
	// the way run_ticket.go/submit.go already are (applyProjectConfigDefaults).
	// Never overrides an explicitly-passed -verify-command.
	explicit := map[string]bool{}
	flags.Visit(func(f *flag.Flag) { explicit[f.Name] = true })
	if !explicit["verify-command"] {
		if cfg, found, err := projectconfig.Load(*pilotDir); err != nil {
			return fmt.Errorf("load %s from -pilot-dir: %w", projectconfig.FileName, err)
		} else if found && cfg.VerifyCommand != "" {
			*verifyCommand = cfg.VerifyCommand
			fmt.Printf("intake: -verify-command not given; using %q from %s/%s\n", *verifyCommand, *pilotDir, projectconfig.FileName)
		}
	}
	resolvedGoalPilotScript, err := resolveHarnessScript(*goalPilotScript, "goal_pilot.py")
	if err != nil {
		return err
	}
	*goalPilotScript = resolvedGoalPilotScript

	// Whole invocation runs under one advisory lock on -pilot-dir (found
	// via a real Opus review pass, 2026-09-04, A2): without it, two
	// concurrent `factoryd intake` invocations against the same pilot dir
	// interleave two goal_pilot.py subprocesses writing the same spec/
	// tree -- both take their content snapshots below before either
	// writes, so both see changes and both report success, each
	// attributing the other's output to itself. This repo's own T-07/
	// SC-011 shape (see safety-contract.md); withIntakeLock fails fast on
	// contention (acquireExclusiveLock, worker_config.go) rather than waiting,
	// so a second invocation is refused immediately instead of silently
	// queuing behind the first.
	return withIntakeLock(*pilotDir, func() error {
		return intakeRun(*specInput, *pilotDir, *goalPilotInterpreter, *goalPilotScript, *timeout, *checkpoint, *verifyCommand)
	})
}

// intakeRun is intakeMain's actual work, run under withIntakeLock.
func intakeRun(specInput, pilotDir, goalPilotInterpreter, goalPilotScript string, timeout time.Duration, checkpoint, verifyCommand string) error {
	specPath := filepath.Join(pilotDir, "spec", "spec.md")
	contractPath := filepath.Join(pilotDir, "spec", "contract.md")
	ticketsDir := filepath.Join(pilotDir, "spec", "tickets")
	// Content snapshots, taken before launch, not a launch-time
	// wall-clock comparison (found via a second round of Codex review of
	// PR #45, on this exact fix's own first attempt): a filesystem whose
	// mtime resolution is coarser than time.Now() can truncate a
	// genuinely fresh write's timestamp down into the same quantum as (or
	// earlier than) the launch instant, wrongly rejecting a fast,
	// perfectly legitimate successful intake as stale. Comparing actual
	// content sidesteps timestamp resolution entirely. Also snapshots
	// every ticket file, not just spec.md (found in the same review
	// round): goal_pilot.py's step 2 always rewrites spec.md on success,
	// but a *partial* failure (this repo's own FAKE_GOAL_PILOT_MODE=
	// no_tickets fixture reproduces it: spec.md gets rewritten, no ticket
	// does) would otherwise still report every pre-existing ticket file
	// as freshly drafted -- checking spec.md's own freshness alone isn't
	// enough to vouch for the ticket list.
	specBefore, specExistedBefore := readFileIfExists(specPath)
	ticketsBefore, err := snapshotMarkdownFiles(ticketsDir)
	if err != nil {
		return fmt.Errorf("snapshot existing tickets before running goal_pilot.py: %w", err)
	}
	_, contractExistedBefore := readFileIfExists(contractPath)

	// Printed before invocation, not treated as a hard refusal (found via
	// a real Opus review pass, 2026-09-04, A1): goal_pilot.py's own
	// main() branches on this exact same signal --
	// read_spec_status(pilot_dir) != "FROZEN" -- to decide whether to
	// re-run step 2 (/spec-plan) at all, and step 2's whole job is
	// rewriting spec.md. A pilot dir genuinely mid-review (never yet
	// frozen) hitting this branch is completely normal, so this can't be
	// a hard error -- but a human who believes they already froze the
	// spec (edited STATUS somewhere that isn't line 1, or wrote
	// "Status:"/"STATUS:FROZEN" without matching goal_pilot.py's own
	// case-sensitive, line-1-anchored `^STATUS:\s*(\S+)`) would otherwise
	// have their review silently discarded and re-drafted with no
	// indication why. This turns that into a visible warning in real
	// time, not a silent surprise discovered only after the fact.
	if specExistedBefore && parseSpecFirstLineStatus(specBefore) != "FROZEN" {
		// Backed up unconditionally before goal_pilot.py can overwrite it
		// (suggested by the same Opus review pass, 2026-09-04, as the
		// cheapest mitigation regardless of provenance: this doesn't need
		// to distinguish "genuinely still draft" from "a human edited
		// this and it's about to be lost" -- it just makes the
		// destructive case recoverable and the warning below actionable
		// instead of just alarming). Best-effort: a backup failure is
		// worth surfacing but must not block a legitimate first-freeze
		// invocation from proceeding.
		backupPath := specPath + ".pre-intake"
		if err := os.WriteFile(backupPath, specBefore, 0o644); err != nil {
			fmt.Printf("intake: warning: could not back up %s to %s before overwriting: %v\n", specPath, backupPath, err)
		}
		fmt.Printf(
			"intake: %s exists but its first line does not read exactly \"STATUS: FROZEN\" "+
				"(goal_pilot.py's own read_spec_status parses only line 1, case-sensitively) -- "+
				"about to re-run /spec-plan, which will overwrite it. Its current content was saved "+
				"to %s first. If you already reviewed and intended to freeze it, stop now and check "+
				"line 1.\n",
			specPath, backupPath,
		)
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	argv := []string{
		goalPilotScript,
		"--spec-input", specInput,
		"--pilot-dir", pilotDir,
		"--non-interactive",
		"--checkpoint", checkpoint,
	}
	if verifyCommand != "" {
		argv = append(argv, "--verify-command", verifyCommand)
	}
	cmd := exec.CommandContext(ctx, goalPilotInterpreter, argv...)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	runErr := cmd.Run()
	fmt.Print(output.String())
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("goal_pilot.py did not complete the drafting step within %s", timeout)
	}
	// runErr's exit code is intentionally not treated as failure by
	// itself: a non-interactive run's own designed exit path is refusing
	// a checkpoint after the step before it already succeeded, which
	// exits non-zero the same way an unrelated crash would. Distinguished
	// instead by goal_pilot.py's own step3_freeze_checkpoint /
	// step5_checkpoint printing one of these exact, stable phrases to
	// stderr when either does -- found via a local codex review pass (for
	// the first of the two): the file-freshness checks below prove
	// content changed, not that goal_pilot.py actually reached the
	// checkpoint its own design puts *after* the drafting step before it
	// finishes in full; a crash partway through that step could otherwise
	// leave freshly changed files on disk that this function would
	// wrongly present as a completed draft. Checking for one of these
	// phrases confirms the corresponding drafting step ran to completion
	// (the checkpoint after it never executes otherwise), not merely that
	// some output changed.
	// Anchored to goal_pilot.py's own full, stable line (the
	// "--non-interactive: ... Halting." wrapper both checkpoints share),
	// not just the distinguishing fragment in the middle of it (found via
	// a real Opus review pass, 2026-09-04, A3): step4_compile's own
	// success path is idempotent and backed by os.Stat alone, not a
	// content diff (see intakeReportContractDraft's doc comment), so its
	// correctness rests entirely on this match being unambiguous. The
	// full line is far less likely to appear by coincidence in unrelated
	// output (e.g. -spec-input's own literal text, echoed back by
	// goal_pilot.py at the top of every invocation) than the bare
	// fragment was.
	const nonInteractiveSpecFreezeRefusal = "--non-interactive: refusing to auto-approve the spec freeze. Halting."
	const nonInteractiveAcceptanceSuiteRefusal = "--non-interactive: refusing to auto-confirm the acceptance-suite checkpoint. Halting."
	// This third checkpoint is always on regardless of -checkpoint, but
	// only reachable at all when -checkpoint skip let step 5 proceed
	// without halting -- see intakeMain's own doc comment and the
	// -checkpoint flag help. Recognized explicitly (found via a real
	// Opus review pass, 2026-09-04, A5), not folded into the generic
	// "reached neither checkpoint" error below: unlike that case, this
	// one did complete real, consequential work -- ticket_runner.py
	// actually built ticket 001 unattended -- and deserves its own
	// distinct, honest message rather than being reported the same way
	// as a genuine drafting failure that changed nothing.
	const nonInteractivePostTicket001Refusal = "--non-interactive: refusing to auto-confirm the post-ticket-001 checkpoint. Halting."
	reachedSpecFreeze := strings.Contains(output.String(), nonInteractiveSpecFreezeRefusal)
	reachedAcceptanceSuite := strings.Contains(output.String(), nonInteractiveAcceptanceSuiteRefusal)
	if strings.Contains(output.String(), nonInteractivePostTicket001Refusal) {
		return fmt.Errorf("goal_pilot.py used -checkpoint %s to proceed past the acceptance-suite checkpoint and actually built ticket 001 unattended via ticket_runner.py, then halted at the always-on post-ticket-001 checkpoint -- this is real, consequential output, not a drafting failure; intakeMain does not interpret it further, review it directly above and in %s/logs", checkpoint, pilotDir)
	}
	if !reachedSpecFreeze && !reachedAcceptanceSuite {
		if runErr != nil {
			return fmt.Errorf("goal_pilot.py did not reach either the spec-freeze checkpoint (no %q) or the acceptance-suite checkpoint (no %q) in its output -- treating this as a real failure rather than presenting whatever partial output exists as a completed draft; see its own output above: %w", nonInteractiveSpecFreezeRefusal, nonInteractiveAcceptanceSuiteRefusal, runErr)
		}
		return fmt.Errorf("goal_pilot.py did not reach either the spec-freeze checkpoint (no %q) or the acceptance-suite checkpoint (no %q) in its output -- treating this as a real failure rather than presenting whatever partial output exists as a completed draft; see its own output above", nonInteractiveSpecFreezeRefusal, nonInteractiveAcceptanceSuiteRefusal)
	}

	if reachedSpecFreeze {
		return intakeReportSpecDraft(specPath, specBefore, specExistedBefore, ticketsDir, ticketsBefore, pilotDir)
	}
	return intakeReportContractDraft(contractPath, contractExistedBefore, pilotDir)
}

// intakeReportSpecDraft is intakeMain's success path for a fresh pilot dir
// (spec.md not yet FROZEN going in): goal_pilot.py's step 2 drafted
// spec.md + spec/tickets/*.md before halting at the spec-freeze
// checkpoint. Split out of intakeMain so intakeReportContractDraft (the
// resumed-pilot-dir success path) reads as its own, equally simple case
// rather than a branch buried in one long function.
func intakeReportSpecDraft(specPath string, specBefore []byte, specExistedBefore bool, ticketsDir string, ticketsBefore map[string][]byte, pilotDir string) error {
	specAfter, statErr := os.ReadFile(specPath)
	if statErr != nil {
		return fmt.Errorf("goal_pilot.py did not produce %s (see its own output above): %w", specPath, statErr)
	}
	if specExistedBefore && bytes.Equal(specBefore, specAfter) {
		return fmt.Errorf("goal_pilot.py did not draft a new spec at %s during this invocation -- found an existing, unchanged file from an earlier run instead; see its own output above", specPath)
	}

	entries, err := os.ReadDir(ticketsDir)
	if err != nil {
		return fmt.Errorf("goal_pilot.py did not produce a spec/tickets directory at %s (see its own output above): %w", ticketsDir, err)
	}
	var tickets []string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		content, err := os.ReadFile(filepath.Join(ticketsDir, entry.Name()))
		if err != nil {
			continue // transient read error -- skip rather than fail the whole invocation over one file
		}
		if before, existed := ticketsBefore[entry.Name()]; existed && bytes.Equal(before, content) {
			continue // unchanged from before this invocation -- stale, not freshly drafted
		}
		tickets = append(tickets, entry.Name())
	}
	sort.Strings(tickets)
	if len(tickets) == 0 {
		return fmt.Errorf("goal_pilot.py ran but drafted no new ticket files under %s during this invocation -- see its own output above", ticketsDir)
	}
	// The pilot dir itself is never the right -workspace value (found via
	// Codex review of PR #45): projectBootstrapArtifactPaths and
	// resolvePiTicketPath both search under filepath.Dir(-workspace)/spec,
	// not -workspace/spec directly, matching /spec-plan's own scaffold
	// (`mkdir -p "$pilot_dir/spec/tickets" ... "$pilot_dir/workspace"` --
	// see pi-harness-hardening/prompts/spec-plan.md's step 0). The correct
	// -workspace is the "workspace" subdirectory /spec-plan already
	// scaffolds beneath pilot_dir, so filepath.Dir(-workspace) lands back
	// on pilot_dir itself, where spec/ and ARCHITECTURE.md actually are.
	workspacePath := filepath.Join(pilotDir, "workspace")
	fmt.Printf("\nintake: drafted %d ticket(s) under %s:\n", len(tickets), ticketsDir)
	for _, ticket := range tickets {
		fmt.Printf("  %s\n", ticket)
	}
	fmt.Printf(
		"\nNext: review %s -- the spec-freeze checkpoint is never auto-approved by design. "+
			"spec/contract.md does not exist yet (goal_pilot.py's own step 4, after approval), "+
			"so factoryd's mandatory project-bootstrap preflight will not pass against -workspace %s "+
			"until the spec is approved and this same `factoryd intake` command is re-run against "+
			"the same -pilot-dir to continue through contract-plan. To approve it: the FIRST LINE "+
			"of spec.md must read exactly \"STATUS: FROZEN\" (case-sensitive; anything else on line "+
			"1 -- including a heading or blank line above it -- and goal_pilot.py re-drafts the "+
			"spec instead of continuing).\n",
		specPath, workspacePath,
	)
	return nil
}

// intakeReportContractDraft is intakeMain's success path for a resumed
// pilot dir (spec.md already FROZEN going in, by a human's own prior
// edit): goal_pilot.py skipped straight to step 4 (/contract-plan,
// drafting spec/contract.md and spec/acceptance/) and halted at step 5,
// the acceptance-suite checkpoint. Unlike intakeReportSpecDraft's ticket
// freshness check, contract.md is allowed to be byte-identical to before:
// step 4 is itself idempotent (is_compile_complete's own marker file
// skips re-running /contract-plan once it has already succeeded), so a
// repeat `factoryd intake` invocation against an already-compiled pilot
// dir legitimately reaches this same halt with nothing new to show for
// it -- that is still success, not a stale/partial draft, precisely
// because reaching this halt at all already proves (by goal_pilot.py's
// own step4_compile) that spec/contract.md exists, on this invocation or
// an earlier one.
func intakeReportContractDraft(contractPath string, contractExistedBefore bool, pilotDir string) error {
	if _, err := os.Stat(contractPath); err != nil {
		return fmt.Errorf("goal_pilot.py reached the acceptance-suite checkpoint but %s does not exist (see its own output above): %w", contractPath, err)
	}
	freshness := "freshly drafted"
	if contractExistedBefore {
		freshness = "already compiled by an earlier invocation"
	}
	workspacePath := filepath.Join(pilotDir, "workspace")
	fmt.Printf("\nintake: %s is %s.\n", contractPath, freshness)
	fmt.Printf(
		"\nNext: review %s -- goal_pilot.py's own /contract-plan calls this "+
			"\"the highest-stakes artifact in the pipeline,\" and the acceptance-suite "+
			"checkpoint it just halted at is every bit as much a hard stop as the spec "+
			"freeze was. Once reviewed, `factoryd check-project` and `factoryd <run>` "+
			"against -workspace %s can both proceed -- factoryd's mandatory "+
			"project-bootstrap preflight requires exactly this file.\n",
		contractPath, workspacePath,
	)
	return nil
}
