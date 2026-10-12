package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"buildgate/internal/request"
	"buildgate/internal/run"
	"buildgate/internal/sanitize"
)

func newInspectFlags() (flags *flag.FlagSet, dataDir, configPath *string, jsonOutput *bool) {
	flags = flag.NewFlagSet("inspect", flag.ContinueOnError)
	dataDir = flags.String("data-dir", "data", "directory containing durable request and run records")
	configPath = flags.String("config", "", "session config path or profile name, for the data dir; empty: the active profile")
	jsonOutput = flags.Bool("json", false, "print the commands as JSON instead of text")
	finishFlags(flags)
	return
}

// inspectMain implements `factoryd inspect <request-id | run-id>`: the
// commands that show a request's work before a decision (request.Inspect),
// or one run's (request.InspectRun).
func inspectMain(args []string) error {
	return inspectRun(args, os.Stdout)
}

func inspectRun(args []string, w io.Writer) error {
	flags, dataDir, configPath, jsonOutput := newInspectFlags()
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return fmt.Errorf("usage: factoryd inspect [-config <profile>] [-json] <request-id | run-id>")
	}
	if err := resolveDataDirFromSessionConfig(flags, dataDir, *configPath); err != nil {
		return err
	}
	id := flags.Arg(0)
	if !run.ValidID(id) {
		return fmt.Errorf("%q is not a request or run id", id)
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
		fmt.Fprintf(w, "Nothing to inspect for %s in its current state: commands are shown at a review gate and for built work.\n", sanitize.Line(id))
		return nil
	}
	printInspection(w, inspection)
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
		return request.Inspection{}, fmt.Errorf("no request or run %q in %s", id, dataDir)
	}
	ticket := request.InspectRun(dataDir, loaded)
	return request.Inspection{Tickets: []request.InspectTicket{ticket}, Note: request.InspectRunNote}, nil
}

func printInspection(w io.Writer, i request.Inspection) {
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
		if t.Worktree != "" {
			fmt.Fprintf(w, "  worktree  %s\n", t.Worktree)
		}
		printInspectCommands(w, "  ", t.Commands)
	}
	if i.Note != "" {
		fmt.Fprintf(w, "\n%s\n", i.Note)
	}
}

func printInspectCommands(w io.Writer, indent string, cmds []request.InspectCommand) {
	for _, c := range cmds {
		fmt.Fprintf(w, "%s%s:\n%s  %s\n", indent, c.Label, indent, c.Command)
	}
}
