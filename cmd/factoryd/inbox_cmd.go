package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"buildgate/internal/request"
	"buildgate/internal/run"
	"buildgate/internal/sanitize"
)

// inboxStates are the request states that wait on the operator: the four
// human gates, a lost step awaiting a resume decision, and the two states
// only a human can clear.
var inboxStates = map[request.State]bool{
	request.StateSpecReview:   true,
	request.StateOracleReview: true,
	request.StatePlanReview:   true,
	request.StatePRReview:     true,
	request.StateResumeReview: true,
	request.StateHalted:       true,
	request.StateQuarantined:  true,
}

// inboxEntry is one request waiting on the operator, as `factoryd inbox`
// prints it and as `-json` emits it.
type inboxEntry struct {
	Profile    string `json:"profile"`
	DataDir    string `json:"data_dir"`
	ID         string `json:"id"`
	Title      string `json:"title"`
	State      string `json:"state"`
	Since      string `json:"since,omitempty"`
	AgeSeconds int64  `json:"age_seconds"`
	Reason     string `json:"reason,omitempty"`
	Next       string `json:"next,omitempty"`
	Approve    string `json:"approve,omitempty"`
	Reject     string `json:"reject,omitempty"`
	// Baseline is the verify command's result on the base commit of each
	// ticket this request has built (run.BaselineVerify.Summary).
	Baseline   string   `json:"baseline,omitempty"`
	PRURLs     []string `json:"pr_urls,omitempty"`
	ConsoleURL string   `json:"console_url,omitempty"`

	since time.Time
}

func newInboxFlags() (flags *flag.FlagSet, jsonOutput *bool) {
	flags = flag.NewFlagSet("inbox", flag.ContinueOnError)
	jsonOutput = flags.Bool("json", false, "print the entries as a JSON array instead of text")
	plainFlagUsage(flags)
	return flags, jsonOutput
}

// inboxMain implements `factoryd inbox`: everything waiting on the
// operator, across every profile's data dir, oldest first.
func inboxMain(args []string) error {
	return inboxRun(args, os.Stdout, os.Stderr)
}

func inboxRun(args []string, w, warn io.Writer) error {
	flags, jsonOutput := newInboxFlags()
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() > 0 {
		return fmt.Errorf("inbox takes no arguments, got %q", flags.Arg(0))
	}
	entries, err := collectInbox(warn, time.Now())
	if err != nil {
		return err
	}
	if *jsonOutput {
		if entries == nil {
			entries = []inboxEntry{}
		}
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(entries)
	}
	if len(entries) == 0 {
		fmt.Fprintln(w, "Nothing is waiting on you.")
		return nil
	}
	for i, e := range entries {
		if i > 0 {
			fmt.Fprintln(w)
		}
		printInboxEntry(w, e)
	}
	return nil
}

// collectInbox gathers the waiting requests of every distinct data dir among
// the profiles (or of the one resolved data dir when there are no profiles),
// oldest first.
func collectInbox(warn io.Writer, now time.Time) ([]inboxEntry, error) {
	profiles, err := loadProfiles()
	if err != nil {
		return nil, err
	}
	for _, p := range profiles {
		if p.LoadErr != nil {
			fmt.Fprintf(warn, "profile %s: skipped, its config does not load: %s\n", p.Name, sanitize.Line(p.LoadErr.Error()))
		}
	}
	dirs, owners := distinctDataDirs(profiles)
	label := map[string]string{}
	for _, d := range dirs {
		label[d] = owners[d][0]
	}
	if len(profiles) == 0 {
		dataDir := "data"
		if err := resolveDataDirFromSessionConfig(flag.NewFlagSet("inbox", flag.ContinueOnError), &dataDir, ""); err != nil {
			return nil, err
		}
		name, _ := statusProfile("")
		if name == "" {
			name = "default"
		}
		dirs, label = []string{dataDir}, map[string]string{dataDir: name}
	}

	var entries []inboxEntry
	for _, dir := range dirs {
		requests, err := request.List(dir)
		if err != nil {
			return nil, fmt.Errorf("load requests from %q: %w", dir, err)
		}
		base := resolveConsoleBaseURL("", dir)
		for _, r := range requests {
			if inboxStates[r.State] {
				entries = append(entries, buildInboxEntry(r, label[dir], dir, base, now))
			}
		}
	}
	sort.SliceStable(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		if a.since.IsZero() != b.since.IsZero() {
			return !a.since.IsZero()
		}
		return a.since.Before(b.since)
	})
	return entries, nil
}

func buildInboxEntry(r *request.Request, profile, dataDir, consoleBase string, now time.Time) inboxEntry {
	e := inboxEntry{
		Profile:    profile,
		DataDir:    dataDir,
		ID:         r.ID,
		Title:      sanitize.Line(request.Title(dataDir, r.ID)),
		State:      string(r.State),
		ConsoleURL: consoleRequestURL(consoleBase, r.ID),
	}
	for _, ts := range []string{r.WaitingSince, r.EnteredAt, r.UpdatedAt} {
		if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
			e.since = t
			e.Since = t.UTC().Format(time.RFC3339)
			e.AgeSeconds = int64(now.Sub(t).Seconds())
			break
		}
	}
	e.Baseline = inboxBaseline(dataDir, r)
	switch r.State {
	case request.StateSpecReview, request.StateOracleReview, request.StatePlanReview:
		e.Approve = fmt.Sprintf("factoryd approve -config %s %s", profile, r.ID)
		e.Reject = fmt.Sprintf("factoryd reject -config %s -reason \"...\" %s", profile, r.ID)
	case request.StatePRReview:
		for _, t := range r.Tickets {
			if t.PRURL != "" {
				e.PRURLs = append(e.PRURLs, t.PRURL)
			}
		}
	case request.StateHalted, request.StateQuarantined, request.StateResumeReview:
		e.Reason = sanitize.Line(r.Error)
		e.Next = sanitize.Line(r.NextAction())
	}
	return e
}

// inboxBaseline is the baseline verify result of each ticket of r that has
// a run, in ticket order; a request with several tickets names each. ""
// before any ticket was built.
func inboxBaseline(dataDir string, r *request.Request) string {
	var parts []string
	for i, t := range r.Tickets {
		if t.RunID == "" {
			continue
		}
		built, err := run.Load(dataDir, t.RunID)
		if err != nil {
			continue
		}
		summary := sanitize.Line(built.BaselineVerify.Summary())
		if summary == "" {
			continue
		}
		if len(r.Tickets) > 1 {
			summary = fmt.Sprintf("ticket %d %s", i+1, summary)
		}
		parts = append(parts, summary)
	}
	return strings.Join(parts, "; ")
}

func printInboxEntry(w io.Writer, e inboxEntry) {
	title := e.Title
	if title == "" {
		title = "-"
	}
	fmt.Fprintf(w, "%s  %s  %s  %s  %s\n", inboxAge(time.Duration(e.AgeSeconds)*time.Second), e.Profile, e.State, e.ID, title)
	switch {
	case e.Approve != "":
		fmt.Fprintf(w, "  %s\n  %s\n", e.Approve, e.Reject)
	case len(e.PRURLs) > 0:
		fmt.Fprintf(w, "  review and merge: %s\n", strings.Join(e.PRURLs, " "))
	case e.State == string(request.StatePRReview):
		fmt.Fprintln(w, "  review and merge the PR on GitHub")
	default:
		fmt.Fprintf(w, "  reason: %s\n  next: %s\n", e.Reason, e.Next)
	}
	if e.Baseline != "" {
		fmt.Fprintf(w, "  baseline verify: %s\n", e.Baseline)
	}
	if e.ConsoleURL != "" {
		fmt.Fprintf(w, "  console: %s\n", e.ConsoleURL)
	}
}

// inboxAge renders how long a request has waited: "<1m", "42m", "3h05m",
// "2d4h".
func inboxAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "<1m"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd%dh", int(d.Hours())/24, int(d.Hours())%24)
	}
}
