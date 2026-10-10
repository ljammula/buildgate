package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"text/tabwriter"
	"time"

	"buildgate/internal/release"
	"buildgate/internal/run"
	"buildgate/internal/sanitize"
	"buildgate/internal/stats"
)

// statsFlags are `factoryd stats`'s flags, built apart from parsing so
// TestUSAGEDocFlagsExistOnSubcommand can enumerate them.
type statsFlags struct {
	set        *flag.FlagSet
	dataDir    *string
	project    *string
	since      *string
	bucket     *int
	all        *bool
	jsonOutput *bool
	configPath *string
}

func newStatsFlags() statsFlags {
	flags := flag.NewFlagSet("stats", flag.ContinueOnError)
	f := statsFlags{set: flags}
	f.dataDir = flags.String("data-dir", "data", "directory containing durable run records")
	f.project = flags.String("project", "", "show this one project's numbers and their trend; empty prints one row per project")
	f.since = flags.String("since", "", "count tickets whose first run began on/after this: a number of days such as 30d, or a date such as 2026-09-01; empty counts every ticket")
	f.bucket = flags.Int("bucket", stats.DefaultBucketDays, "days in one row of the trend table")
	f.all = flags.Bool("all", false, "also count the live-smoke tickets, which are built to prove the pipeline and some to fail")
	f.jsonOutput = flags.Bool("json", false, "print machine-readable JSON instead of tables")
	f.configPath = flags.String("config", "", "session config path (for -data-dir resolution); empty searches the default paths")
	plainFlagUsage(flags)
	return f
}

// statsMain implements `factoryd stats`: whether the factory is getting
// better, as numbers per repository and over time. It reads run records and
// changes nothing.
func statsMain(args []string) error {
	return statsRun(args, os.Stdout, time.Now())
}

func statsRun(args []string, w io.Writer, now time.Time) error {
	f := newStatsFlags()
	if err := f.set.Parse(args); err != nil {
		return err
	}
	if f.set.NArg() > 0 {
		return fmt.Errorf("stats takes no arguments, got %q", f.set.Arg(0))
	}
	since, err := stats.ParseSince(*f.since, now)
	if err != nil {
		return fmt.Errorf("-%w", err)
	}
	if *f.bucket < 1 || *f.bucket > 365 {
		return fmt.Errorf("-bucket %d: want a number of days from 1 to 365", *f.bucket)
	}
	if err := resolveDataDirFromSessionConfig(f.set, f.dataDir, *f.configPath); err != nil {
		return err
	}
	runs, err := run.LoadAll(*f.dataDir)
	if err != nil {
		return fmt.Errorf("list runs from %q: %w", *f.dataDir, err)
	}
	opts := stats.Options{Since: since, BucketDays: *f.bucket, Now: now}
	if !*f.all {
		opts.ExcludeTicketPrefixes = []string{stats.SmokePrefix}
	}
	if *f.project != "" {
		opts.Project = *f.project
		report := stats.Compute(projectRuns(runs, *f.project), opts)
		if *f.jsonOutput {
			return encodeJSON(w, report)
		}
		printProjectStats(w, report)
		return nil
	}
	overview := stats.ComputeOverview(runs, opts, release.ProjectOf)
	if *f.jsonOutput {
		return encodeJSON(w, overview)
	}
	printStatsOverview(w, overview)
	return nil
}

func encodeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func projectRuns(runs []*run.Run, project string) []*run.Run {
	var out []*run.Run
	for _, r := range runs {
		if release.ProjectOf(r) == project {
			out = append(out, r)
		}
	}
	return out
}

// share prints part of whole as "3/8 (38%)", "-" when there is no whole.
func share(part, whole int) string {
	if whole == 0 {
		return "-"
	}
	return fmt.Sprintf("%d/%d (%d%%)", part, whole, int(math.Round(100*float64(part)/float64(whole))))
}

func spreadText(s stats.Spread) string {
	if s.Series == 0 {
		return "-"
	}
	return fmt.Sprintf("%g", s.Median)
}

func topCount(counts []stats.Count) string {
	if len(counts) == 0 {
		return "-"
	}
	return fmt.Sprintf("%s (%d)", counts[0].Name, counts[0].Runs)
}

func printStatsOverview(w io.Writer, o stats.Overview) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "PROJECT\tTICKETS\tONE-SHOT\tACCEPTED\tMEDIAN ROUNDS\tTOP QUARANTINE CHECK")
	row := func(name string, r stats.Report) {
		m := r.Overall
		fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\t%s\n", sanitize.Line(name), m.Tickets,
			share(m.OneShot, m.Tickets), share(m.Accepted, m.Tickets), spreadText(m.RoundsToGreen), topCount(m.QuarantinedBy))
	}
	for _, r := range o.Projects {
		row(r.Project, r)
	}
	row("overall", o.Overall)
	tw.Flush()
	printStatsNotes(w, o.Overall)
}

// printStatsNotes says what the numbers leave out.
func printStatsNotes(w io.Writer, r stats.Report) {
	fmt.Fprintln(w)
	if r.ExcludedRuns > 0 {
		fmt.Fprintf(w, "%d live-smoke run(s) not counted; -all counts them.\n", r.ExcludedRuns)
	}
	if r.Unfinished > 0 {
		fmt.Fprintf(w, "%d run(s) still in progress not counted.\n", r.Unfinished)
	}
	if r.Since != "" {
		fmt.Fprintf(w, "Tickets whose first run began on or after %s.\n", r.Since[:10])
	}
}

func printProjectStats(w io.Writer, r stats.Report) {
	m := r.Overall
	fmt.Fprintf(w, "project %s: %d ticket(s) over %d finished run(s)\n\n", sanitize.Line(r.Project), m.Tickets, r.Runs)
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintf(tw, "one-shot\t%s\n", share(m.OneShot, m.Tickets))
	fmt.Fprintf(tw, "accepted\t%s\n", share(m.Accepted, m.Tickets))
	if m.RoundsToGreen.Series == 0 {
		fmt.Fprintf(tw, "rounds to green\t-\n")
	} else {
		fmt.Fprintf(tw, "rounds to green\tmedian %g, p90 %g over %d accepted ticket(s)\n", m.RoundsToGreen.Median, m.RoundsToGreen.P90, m.RoundsToGreen.Series)
	}
	fmt.Fprintf(tw, "same failure twice\t%s of failed-round pairs that recorded a signature\n", share(m.SameFailurePairs, m.ComparablePairs))
	fmt.Fprintf(tw, "round changed no file\t%s of failed-round pairs\n", share(m.NoChangePairs, m.FailedRoundPairs))
	fmt.Fprintf(tw, "corrective builds\t%d ran, %d accepted\n", m.CorrectiveBuilds.Ran, m.CorrectiveBuilds.Accepted)
	fmt.Fprintf(tw, "relay spend\t%d tokens, $%.2f; $%.2f per accepted ticket\n", m.Spend.Tokens, float64(m.Spend.CostMicroUSD)/1e6, float64(m.Spend.PerAcceptedTicketMicroUSD)/1e6)
	tw.Flush()
	printBucketTable(w, r)
	printCounts(w, "quarantined by", m.QuarantinedBy)
	printCounts(w, "halted by", m.HaltedBy)
	printStatsNotes(w, r)
}

func printBucketTable(w io.Writer, r stats.Report) {
	if len(r.Buckets) == 0 {
		return
	}
	head := "WEEK OF"
	if r.BucketDays != stats.DefaultBucketDays {
		head = fmt.Sprintf("%d DAYS FROM", r.BucketDays)
	}
	fmt.Fprintln(w)
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintf(tw, "%s\tTICKETS\tONE-SHOT %%\tACCEPTED %%\tMEDIAN ROUNDS\tSAME-FAILURE %%\n", head)
	for _, b := range r.Buckets {
		m := b.Metrics
		fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\t%s\n", b.Start[:10], m.Tickets,
			share(m.OneShot, m.Tickets), share(m.Accepted, m.Tickets), spreadText(m.RoundsToGreen), share(m.SameFailurePairs, m.ComparablePairs))
	}
	tw.Flush()
}

func printCounts(w io.Writer, title string, counts []stats.Count) {
	fmt.Fprintf(w, "\n%s:\n", title)
	if len(counts) == 0 {
		fmt.Fprintln(w, "  (none)")
		return
	}
	width := 0
	for _, c := range counts {
		width = max(width, len(c.Name))
	}
	for _, c := range counts {
		fmt.Fprintf(w, "  %-*s  %d\n", width, c.Name, c.Runs)
	}
}
