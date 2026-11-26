package main

import (
	"flag"
	"fmt"
	"io"
	"strings"
)

// doctorRegisterVerboseFlag registers doctor's -v: every row, passing ones
// included, in the order the checks ran.
func doctorRegisterVerboseFlag(flags *flag.FlagSet) *bool {
	return flags.Bool("v", false, "List every check, the passing ones included. Without it only failures and warnings are listed, then a count.")
}

// needingDocker marks checks as rows that need the Docker daemon: when the
// daemon does not answer, doctor's report lists them as one line under the
// docker row (doctorReport) rather than as one failure each.
func needingDocker(checks ...doctorCheck) []doctorCheck {
	for i := range checks {
		checks[i].NeedsDocker = true
	}
	return checks
}

// doctorReport prints doctor's report and returns how many checks failed.
// With verbose it is runDoctorChecks' full list. Without, it lists only the
// failures and warnings, in the order they ran, then the count of the rest;
// when the Docker daemon does not answer, the failing rows that need it
// are one line naming them, and only the docker row counts as failed.
func doctorReport(checks []doctorCheck, w io.Writer, verbose bool) (failed int) {
	if verbose {
		return runDoctorChecks(checks, w)
	}
	dockerDown := false
	for _, c := range checks {
		if c.Name == doctorDockerReachableName && c.Err != nil && !c.Advisory {
			dockerDown = true
		}
	}
	var waiting []string
	if dockerDown {
		for _, c := range checks {
			if c.NeedsDocker && c.Err != nil {
				waiting = append(waiting, doctorShortName(c.Name))
			}
		}
	}
	var warned, passed int
	for _, c := range checks {
		switch {
		case dockerDown && c.NeedsDocker && c.Err != nil:
			continue
		case c.Err == nil:
			passed++
			if c.Use != "" {
				printDoctorRow(w, c)
			}
			continue
		}
		f, wr := printDoctorRow(w, c)
		failed += f
		warned += wr
		if c.Name == doctorDockerReachableName && len(waiting) > 0 {
			fmt.Fprintf(w, "skip  %d checks need Docker: %s\n", len(waiting), strings.Join(waiting, ", "))
		}
	}
	extra := []string{}
	if warned > 0 {
		extra = append(extra, fmt.Sprintf("%d warning(s)", warned))
	}
	if len(waiting) > 0 {
		extra = append(extra, fmt.Sprintf("%d need Docker", len(waiting)))
	}
	summary := fmt.Sprintf("%d/%d checks passed", passed, len(checks))
	if len(extra) > 0 {
		summary += " (" + strings.Join(extra, ", ") + ")"
	}
	fmt.Fprintf(w, "\n%s; `factoryd doctor -v` lists every check\n", summary)
	return failed
}

// doctorShortName is a row's name up to its first detail (a parenthesised
// image reference, a ": limits" clause), for the one line that lists rows.
func doctorShortName(name string) string {
	if i := strings.IndexAny(name, "(:"); i > 0 {
		return strings.TrimSpace(name[:i])
	}
	return name
}
