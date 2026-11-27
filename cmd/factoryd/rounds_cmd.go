package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"buildgate/internal/request"
	"buildgate/internal/sanitize"
)

func newRoundsFlags() (flags *flag.FlagSet, dataDir, configPath *string) {
	flags = flag.NewFlagSet("rounds", flag.ContinueOnError)
	dataDir = flags.String("data-dir", "data", "internal: the data dir to use in place of the session config's data_dir")
	configPath = flags.String("config", "", "session config path or profile name, for the data dir; empty: the active profile")
	finishFlags(flags)
	return
}

// roundsMain implements `factoryd rounds <request-id>`: the round ledger of a
// request, one line per build round that ended in a commit. It reads the
// request record and changes nothing; `factoryd retry -from <round>` rebuilds
// a ticket from one of them.
func roundsMain(args []string) error {
	return roundsRun(args, os.Stdout)
}

func roundsRun(args []string, w io.Writer) error {
	flags, dataDir, configPath := newRoundsFlags()
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return fmt.Errorf("usage: factoryd rounds [-config <profile>] <request-id>")
	}
	if err := resolveDataDirFromSessionConfig(flags, dataDir, *configPath); err != nil {
		return err
	}
	id, err := resolveIDArg(flags, *dataDir, *configPath, flags.Arg(0), idRequest)
	if err != nil {
		return err
	}
	r, err := request.Load(*dataDir, id)
	if err != nil {
		return err
	}
	printRounds(w, r)
	return nil
}

// printRounds writes r's ledger as a table: ticket, round, kind, commit,
// outcome, failed checks, ref, and whether the ticket's branch was pushed.
func printRounds(w io.Writer, r *request.Request) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	rows := 0
	fmt.Fprintln(tw, "TICKET\tROUND\tKIND\tSHA\tOUTCOME\tFAILED CHECKS\tREF\tPUSHED")
	for i := range r.Tickets {
		ticket := &r.Tickets[i]
		for _, tag := range ticket.RoundTags {
			failed := "-"
			if len(tag.FailedChecks) > 0 {
				failed = strings.Join(tag.FailedChecks, ",")
			}
			pushed := "no"
			if ticket.Pushed() {
				pushed = "yes"
			}
			sha := tag.SHA
			if len(sha) > 12 {
				sha = sha[:12]
			}
			fmt.Fprintf(tw, "%d\t%d\t%s\t%s\t%s\t%s\t%s\t%s\n", ticket.Index, tag.N, sanitize.Line(tag.Kind), sha, sanitize.Line(tag.Outcome), sanitize.Line(failed), sanitize.Line(tag.Ref), pushed)
			rows++
		}
	}
	if rows == 0 {
		fmt.Fprintf(w, "request %s has no recorded rounds: a round is recorded when a build ends in a commit of its own.\n", sanitize.Line(r.ID))
		return
	}
	_ = tw.Flush()
}
