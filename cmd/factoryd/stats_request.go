package main

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"buildgate/internal/sanitize"
	"buildgate/internal/timereport"
)

// statsRequest implements `factoryd stats -request <id>`: where one request's
// time went. It reads the request, its runs' progress feeds and its wait log.
func statsRequest(w io.Writer, dataDir, id string, asJSON bool) error {
	rep, err := timereport.Compute(dataDir, id)
	if err != nil {
		return fmt.Errorf("time report for request %q: %w", id, err)
	}
	if asJSON {
		return encodeJSON(w, rep)
	}
	printRequestTime(w, rep)
	return nil
}

// minutes prints seconds as minutes with one decimal.
func minutes(seconds float64) string { return fmt.Sprintf("%.1f", seconds/60) }

func printRequestTime(w io.Writer, rep *timereport.Report) {
	fmt.Fprintf(w, "request %s (%s)\n", sanitize.Line(rep.RequestID), sanitize.Line(rep.State))
	for _, t := range rep.Tickets {
		fmt.Fprintf(w, "\nticket %d\n", t.Index)
		tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "RUN\tSTATE\tWALL MIN\tLOCK WAIT MIN\tSTAGES (MIN)\tRUN ID")
		for _, r := range t.Runs {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", runLabel(r), sanitize.Line(r.State), minutes(r.Seconds), lockCell(r), stageCell(r), sanitize.Line(r.RunID))
		}
		tw.Flush()
	}
	fmt.Fprintln(w, "\nrepository-busy waits (a step found the repository held by another build):")
	if len(rep.Waits) == 0 {
		fmt.Fprintln(w, "  (none recorded)")
	}
	for _, wt := range rep.Waits {
		fmt.Fprintf(w, "  %s  %s\n", wt.Start, waitEnd(wt))
	}
	c := rep.Critical
	fmt.Fprintln(w, "\ncritical path:")
	if c.Start == "" {
		fmt.Fprintln(w, "  (no run or wait with times)")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintf(tw, "  span\t%s min\t%s to %s\n", minutes(c.SpanSeconds), c.Start, c.End)
	fmt.Fprintf(tw, "  run time\t%s min\tsum over every run\n", minutes(c.RunSeconds))
	fmt.Fprintf(tw, "  lock waits inside runs\t%s min\tpart of the run time\n", minutes(c.LockSeconds))
	fmt.Fprintf(tw, "  repository-busy waits\t%s min\n", minutes(c.BusySeconds))
	fmt.Fprintf(tw, "  idle\t%s min\tspan no run or busy wait covers (includes time waiting on a person)\n", minutes(c.IdleSeconds))
	tw.Flush()
	if c.OngoingWaits > 0 {
		fmt.Fprintf(w, "  %d run(s) in progress are waiting for the lock; not counted.\n", c.OngoingWaits)
	}
	if c.OpenWaits > 0 {
		fmt.Fprintf(w, "  %d wait(s) have no end recorded and count in no total.\n", c.OpenWaits)
	}
}

func runLabel(r timereport.RunTime) string {
	if r.Round > 0 {
		return fmt.Sprintf("%s %d", r.Kind, r.Round)
	}
	return r.Kind
}

func lockCell(r timereport.RunTime) string {
	if len(r.LockWaits) == 0 {
		return "-"
	}
	cell := minutes(r.LockSeconds)
	for _, wt := range r.LockWaits {
		if wt.Open {
			cell += " + open since " + wt.Start
		}
		if wt.Ongoing {
			cell += " + waiting since " + wt.Start
		}
	}
	return cell
}

func stageCell(r timereport.RunTime) string {
	var parts []string
	for _, s := range r.Stages {
		if s.Seconds < 3 {
			continue // under 0.1 minute: in the JSON, not the table
		}
		parts = append(parts, fmt.Sprintf("%s %s", s.Stage, minutes(s.Seconds)))
	}
	if r.Other >= 1 {
		parts = append(parts, "other "+minutes(r.Other))
	}
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, ", ")
}

func waitEnd(wt timereport.Wait) string {
	if wt.Open {
		return "open (no end recorded)"
	}
	return fmt.Sprintf("%s  %s min", wt.End, minutes(wt.Seconds))
}
