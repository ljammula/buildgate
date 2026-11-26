package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
	"buildgate/internal/run"
	"buildgate/internal/sanitize"
)

func newInspectFlags() (flags *flag.FlagSet, dataDir, configPath *string, jsonOutput, commands *bool) {
	flags = flag.NewFlagSet("inspect", flag.ContinueOnError)
	dataDir = flags.String("data-dir", "data", "directory containing durable request and run records")
	configPath = flags.String("config", "", "session config path or profile name, for the data dir; empty: the active profile")
	jsonOutput = flags.Bool("json", false, "print the commands as JSON instead of text")
	commands = flags.Bool("commands", false, "also print the commands to copy: the git reads of the change, a worktree of your own, the files it was judged against and its logs")
	finishFlags(flags)
	return
}

// inspectMain implements `factoryd inspect <request-id | run-id>`: what a
// request's built work is before a decision, read from each run's record
// (the change's size and files, the gate results, the review's findings),
// and with -commands the commands that show the rest (request.Inspect, or
// one run's: request.InspectRun).
func inspectMain(args []string) error {
	return inspectRun(args, os.Stdout)
}

func inspectRun(args []string, w io.Writer) error {
	flags, dataDir, configPath, jsonOutput, commands := newInspectFlags()
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return fmt.Errorf("usage: factoryd inspect [-config <profile>] [-commands] [-json] <request-id | run-id>")
	}
	if err := resolveDataDirFromSessionConfig(flags, dataDir, *configPath); err != nil {
		return err
	}
	id, err := resolveIDArg(flags, *dataDir, *configPath, flags.Arg(0), idRequestOrRun)
	if err != nil {
		return err
	}
	inspection, err := inspectionFor(*dataDir, id)
	if err != nil {
		return err
	}
	if *jsonOutput {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(inspection)
	}
	if inspection.Empty() {
		fmt.Fprintf(w, "Nothing to inspect for %s in its current state: work is shown at a review gate and once it is built.\n", sanitize.Line(id))
		return nil
	}
	printInspection(w, *dataDir, inspection, *commands)
	if !*commands && len(inspection.Tickets) > 0 {
		fmt.Fprintf(w, "\nfactoryd inspect -commands %s prints the commands that show the diff, the ticket, the report and the logs.\n", sanitize.Line(id))
	}
	return nil
}

// inspectionFor resolves id as a request first, then as a run.
func inspectionFor(dataDir, id string) (request.Inspection, error) {
	if r, err := request.Load(dataDir, id); err == nil {
		return request.Inspect(dataDir, r), nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return request.Inspection{}, fmt.Errorf("load request %s: %w", id, err)
	}
	loaded, err := run.Load(dataDir, id)
	if err != nil {
		return request.Inspection{}, fmt.Errorf("load run %s: %w", id, err)
	}
	ticket := request.InspectRun(dataDir, loaded)
	return request.Inspection{Tickets: []request.InspectTicket{ticket}, Note: request.InspectRunNote}, nil
}

// printInspection prints a draft gate's commands (its files are all there is
// to look at), then each ticket's run: what its record holds, and with
// commands the commands and the note about using them.
func printInspection(w io.Writer, dataDir string, i request.Inspection, commands bool) {
	printInspectCommands(w, "", i.Commands)
	for n, t := range i.Tickets {
		if n > 0 || len(i.Commands) > 0 {
			fmt.Fprintln(w)
		}
		label := "run " + t.RunID
		if t.Ticket > 0 {
			label = fmt.Sprintf("ticket %03d  run %s", t.Ticket, t.RunID)
		}
		fmt.Fprintln(w, label)
		if loaded, err := run.Load(dataDir, t.RunID); err == nil {
			printRunFacts(w, loaded)
		}
		if t.Worktree != "" {
			fmt.Fprintf(w, "  worktree   %s\n", t.Worktree)
		}
		if commands {
			printInspectCommands(w, "  ", t.Commands)
		}
	}
	if i.Note != "" && (commands || len(i.Tickets) == 0) {
		fmt.Fprintf(w, "\n%s\n", i.Note)
	}
}

// The most changed files and review findings a run's facts list.
const (
	inspectFilesShown    = 20
	inspectFindingsShown = 20
)

// printRunFacts prints what r's record says of its work: state and pull
// request or reason, the change's size and files, the baseline, each gate's
// result, the review's findings and the challenger's verdict. Every text of
// the record is printed as one sanitized line.
func printRunFacts(w io.Writer, r *run.Run) {
	state := string(r.State)
	if r.PullRequestURL != "" {
		state += " · " + sanitize.Line(r.PullRequestURL)
	} else if reason := sanitize.Line(requestdriver.StatusReason(r)); reason != "" {
		state += " · " + reason
	}
	if state != "" {
		fmt.Fprintf(w, "  state      %s\n", state)
	}
	printRunChange(w, r)
	if summary := sanitize.Line(r.BaselineVerify.Summary()); summary != "" {
		fmt.Fprintf(w, "  baseline   %s\n", summary)
	}
	printRunGates(w, r.GateResults)
	printRunReview(w, r.CodeReview)
	if r.Challenge != nil && r.Challenge.Verdict != "" {
		fmt.Fprintf(w, "  challenge  %s (%d held test files)\n", sanitize.Line(r.Challenge.Verdict), len(r.Challenge.Files))
	}
}

// printRunChange prints the recorded diff stat and the recorded file list.
// The two are separate records and can differ in count, which the line says.
func printRunChange(w io.Writer, r *run.Run) {
	if r.DiffStat == nil {
		return
	}
	listed := ""
	if len(r.ChangedFiles) != r.DiffStat.FilesChanged {
		listed = fmt.Sprintf(" (the record lists %d)", len(r.ChangedFiles))
	}
	fmt.Fprintf(w, "  change     %d files, +%d -%d%s\n", r.DiffStat.FilesChanged, r.DiffStat.Insertions, r.DiffStat.Deletions, listed)
	for n, f := range r.ChangedFiles {
		if n == inspectFilesShown {
			fmt.Fprintf(w, "               ... and %d more\n", len(r.ChangedFiles)-n)
			break
		}
		fmt.Fprintf(w, "               %s\n", sanitize.Line(f))
	}
}

func printRunGates(w io.Writer, gates []run.GateResult) {
	if len(gates) == 0 {
		return
	}
	failed := 0
	for _, g := range gates {
		if !g.Passed {
			failed++
		}
	}
	fmt.Fprintf(w, "  gates      %d passed, %d failed\n", len(gates)-failed, failed)
	for _, g := range gates {
		result := "passed"
		if !g.Passed {
			result = fmt.Sprintf("FAILED (exit %d)", g.ExitCode)
		}
		fmt.Fprintf(w, "               %-22s %s\n", sanitize.Line(g.Check), result)
	}
}

func printRunReview(w io.Writer, review *run.CodeReviewResult) {
	if review == nil {
		return
	}
	if !review.Available {
		fmt.Fprintf(w, "  review     no verdict (policy %s)\n", sanitize.Line(review.Policy))
		return
	}
	fmt.Fprintf(w, "  review     %d findings (policy %s)\n", len(review.Findings), sanitize.Line(review.Policy))
	for n, f := range review.Findings {
		if n == inspectFindingsShown {
			fmt.Fprintf(w, "               ... and %d more\n", len(review.Findings)-n)
			break
		}
		where := sanitize.Line(f.File)
		if where != "" && f.Line > 0 {
			where += fmt.Sprintf(":%d", f.Line)
		}
		fmt.Fprintf(w, "               %s  %s  %s\n", sanitize.Line(f.Severity), where, sanitize.Line(f.Summary))
	}
}

func printInspectCommands(w io.Writer, indent string, cmds []request.InspectCommand) {
	for _, c := range cmds {
		fmt.Fprintf(w, "%s%s:\n%s  %s\n", indent, c.Label, indent, c.Command)
	}
}
