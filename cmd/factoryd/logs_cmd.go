package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"buildgate/internal/evidence"
	"buildgate/internal/request"
	"buildgate/internal/run"
	"buildgate/internal/sanitize"
)

// logsPollInterval is how often `factoryd logs -f` re-reads the id's log
// files. A package var so tests can shrink it.
var logsPollInterval = 500 * time.Millisecond

// logsTailWindow bounds how much of a file's end is read to find its last
// -n lines, so a multi-megabyte verify log is not slurped whole.
const logsTailWindow = 256 << 10

const logsRelayNote = "note: the relay writes no log file; its usage lands in relay-ledger/usage.jsonl"

// logFile is one log under the data dir.
type logFile struct {
	path     string
	size     int64
	modified time.Time
	// listOnly files (progress.jsonl, notifications.log) appear in -list but
	// are never the "being written now" default: the progress feed changes on
	// every mark, and notification records are not a work log -- either
	// would win over the log the operator wants.
	listOnly bool
}

// logsTarget is what an id resolved to: how to enumerate its logs now and
// whether the thing producing them has finished.
type logsTarget struct {
	files    func() []logFile
	terminal func() bool
	// listFiles, when set, is what -list shows instead of files (a request
	// lists every ticket's run, not only the current one).
	listFiles func() []logFile
}

func newLogsFlags() (flags *flag.FlagSet, dataDir, configPath *string, list, follow *bool, lines *int) {
	flags = flag.NewFlagSet("logs", flag.ContinueOnError)
	dataDir = flags.String("data-dir", "data", "directory containing durable request and run records")
	configPath = flags.String("config", "", "session config path (for -data-dir resolution); empty searches the default paths")
	list = flags.Bool("list", false, "list every log for the id (oldest first) instead of showing the newest")
	follow = flags.Bool("f", false, "follow: print appended bytes and switch to newer log files until the request or run finishes (Ctrl-C to stop)")
	lines = flags.Int("n", 40, "number of trailing lines to show")
	plainFlagUsage(flags)
	return
}

const logsUsage = `usage: factoryd logs [-config <path>] [-data-dir <path>] [-list] [-f] [-n N] <request-id | run-id | queue-run | serve>`

func logsMain(args []string) error {
	flags, dataDir, configPath, list, follow, lines := newLogsFlags()
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		flags.Usage()
		return errors.New(logsUsage)
	}
	if *lines < 0 {
		return errors.New("logs: -n must not be negative")
	}
	if err := resolveDataDirFromSessionConfig(flags, dataDir, *configPath); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	return runLogs(ctx, os.Stdout, *dataDir, flags.Arg(0), *list, *follow, *lines)
}

// runLogs is logsMain after flag parsing, split out for tests.
func runLogs(ctx context.Context, w io.Writer, dataDir, id string, list, follow bool, lines int) error {
	target, err := resolveLogsTarget(dataDir, id)
	if err != nil {
		return err
	}
	if list {
		return printLogList(w, dataDir, target)
	}
	if follow {
		return followLogs(ctx, w, dataDir, target, lines)
	}
	cur, ok := newestLog(target.files())
	if !ok {
		return fmt.Errorf("no logs yet for %s in %s", id, dataDir)
	}
	_, err = printTail(w, dataDir, cur, lines)
	return err
}

func resolveLogsTarget(dataDir, id string) (logsTarget, error) {
	switch id {
	case "queue-run", "serve":
		return processLogsTarget(dataDir, id), nil
	}
	if id == "" || id != filepath.Base(id) || id == "." || id == ".." {
		return logsTarget{}, fmt.Errorf("no request or run %s in %s", id, dataDir)
	}
	if _, err := os.Stat(request.Path(dataDir, id)); err == nil {
		return requestLogsTarget(dataDir, id), nil
	}
	if fi, err := os.Stat(run.Dir(dataDir, id)); err == nil && fi.IsDir() {
		return runLogsTarget(dataDir, id), nil
	}
	return logsTarget{}, fmt.Errorf("no request or run %s in %s", id, dataDir)
}

// processLogsTarget covers the launchd (<name>.{out,err}.log) and
// quickstart (quickstart-<name>.{out,err}.log) process logs.
func processLogsTarget(dataDir, name string) logsTarget {
	logDir := filepath.Join(dataDir, "logs")
	names := []string{name + ".out.log", name + ".err.log", "quickstart-" + name + ".out.log", "quickstart-" + name + ".err.log"}
	return logsTarget{
		files: func() []logFile {
			var out []logFile
			for _, n := range names {
				if lf, ok := statLog(filepath.Join(logDir, n), false); ok {
					out = append(out, lf)
				}
			}
			return out
		},
		terminal: func() bool { return false },
	}
}

func requestLogsTarget(dataDir, id string) logsTarget {
	load := func() *request.Request {
		r, err := request.Load(dataDir, id)
		if err != nil {
			return nil
		}
		return r
	}
	gather := func(runIDs func(*request.Request) []string) []logFile {
		out := walkLogs(request.Dir(dataDir, id))
		if r := load(); r != nil {
			for _, rid := range runIDs(r) {
				out = append(out, walkLogs(run.Dir(dataDir, rid))...)
			}
		}
		return out
	}
	return logsTarget{
		files: func() []logFile { return gather(currentTicketRunIDs) },
		listFiles: func() []logFile {
			return gather(func(r *request.Request) []string { return allRequestRunIDs(dataDir, r) })
		},
		terminal: func() bool {
			r := load()
			if r == nil {
				return false
			}
			switch r.State {
			case request.StateDone, request.StateQuarantined, request.StateHalted, request.StateResumeReview, request.StateCancelled:
				return true
			}
			return false
		},
	}
}

// currentTicketRunIDs returns the run behind the request's current ticket
// (TicketIndex is 1-based); with no current ticket (a finished request) it
// falls back to the last ticket that has a run.
func currentTicketRunIDs(r *request.Request) []string {
	if i := r.TicketIndex; i >= 1 && i <= len(r.Tickets) && r.Tickets[i-1].RunID != "" {
		return []string{r.Tickets[i-1].RunID}
	}
	for i := len(r.Tickets) - 1; i >= 0; i-- {
		if r.Tickets[i].RunID != "" {
			return []string{r.Tickets[i].RunID}
		}
	}
	return nil
}

// allRequestRunIDs returns every run the request has had: each ticket's
// current run plus the earlier runs of a ticket that was rebuilt (after a
// retry, or after a worker restart), which the request record no longer
// links. Run ids start with the request id, so the runs dir names them.
func allRequestRunIDs(dataDir string, r *request.Request) []string {
	seen := map[string]bool{}
	var ids []string
	add := func(id string) {
		if id != "" && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	for _, t := range r.Tickets {
		add(t.RunID)
	}
	if entries, err := os.ReadDir(filepath.Join(dataDir, "runs")); err == nil {
		for _, e := range entries {
			if e.IsDir() && strings.HasPrefix(e.Name(), r.ID+"-") {
				add(e.Name())
			}
		}
	}
	return ids
}

func runLogsTarget(dataDir, id string) logsTarget {
	return logsTarget{
		files: func() []logFile { return walkLogs(run.Dir(dataDir, id)) },
		terminal: func() bool {
			r, err := run.Load(dataDir, id)
			if err != nil {
				return false
			}
			switch r.State {
			case run.StateAccepted, run.StateHalted, run.StateQuarantined:
				return true
			}
			return false
		},
	}
}

func statLog(path string, listOnly bool) (logFile, bool) {
	fi, err := os.Stat(path)
	if err != nil || !fi.Mode().IsRegular() {
		return logFile{}, false
	}
	return logFile{path: path, size: fi.Size(), modified: fi.ModTime(), listOnly: listOnly}, true
}

// walkLogs returns every *.log under root; progress.jsonl and notifications.log are list-only.
func walkLogs(root string) []logFile {
	var out []logFile
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		name := d.Name()
		// The build agent's notes sit in the run directory; only the
		// handoff reads them (SC-018). Skipped by name, not left out by
		// their suffix alone.
		if name == evidence.AgentNotesFileName {
			return nil
		}
		switch {
		case name == "progress.jsonl" || name == "notifications.log":
			if lf, ok := statLog(p, true); ok {
				out = append(out, lf)
			}
		case strings.HasSuffix(name, ".log"):
			if lf, ok := statLog(p, false); ok {
				out = append(out, lf)
			}
		}
		return nil
	})
	return out
}

// newestLog picks the most recently modified log; ties go to the later path
// so the choice is stable.
func newestLog(files []logFile) (logFile, bool) {
	var best logFile
	found := false
	for _, f := range files {
		if f.listOnly {
			continue
		}
		if !found || f.modified.After(best.modified) || (f.modified.Equal(best.modified) && f.path > best.path) {
			best, found = f, true
		}
	}
	return best, found
}

func printLogList(w io.Writer, dataDir string, t logsTarget) error {
	files := t.files()
	if t.listFiles != nil {
		files = t.listFiles()
	}
	sort.Slice(files, func(i, j int) bool {
		if !files[i].modified.Equal(files[j].modified) {
			return files[i].modified.Before(files[j].modified)
		}
		return files[i].path < files[j].path
	})
	now := time.Now()
	for _, f := range files {
		fmt.Fprintf(w, "%s  %s  %s (%s ago)\n", relToDataDir(dataDir, f.path), humanBytes(f.size), f.modified.Format("2006-01-02 15:04:05"), humanAge(now.Sub(f.modified)))
	}
	if len(files) == 0 {
		fmt.Fprintln(w, "no logs yet")
	}
	fmt.Fprintln(w, logsRelayNote)
	return nil
}

func relToDataDir(dataDir, p string) string {
	if rel, err := filepath.Rel(dataDir, p); err == nil {
		return rel
	}
	return p
}

func humanBytes(n int64) string {
	switch {
	case n < 1<<10:
		return fmt.Sprintf("%d B", n)
	case n < 1<<20:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	}
}

func humanAge(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// cleanLogText treats log bytes as untrusted: escape sequences, control
// characters, bidi/format runes and obvious credentials go; \r is dropped
// too (a bare \r rewrites the terminal line), leaving \n and \t.
func cleanLogText(s string) string {
	return strings.ReplaceAll(sanitize.Text(s), "\r", "")
}

// printTail prints f's header and last n lines and returns the byte offset
// it read up to, so a follower can continue from there.
func printTail(w io.Writer, dataDir string, f logFile, n int) (int64, error) {
	fmt.Fprintf(w, "==> %s (%s, modified %s ago)\n", relToDataDir(dataDir, f.path), humanBytes(f.size), humanAge(time.Since(f.modified)))
	fh, err := os.Open(f.path)
	if err != nil {
		return 0, fmt.Errorf("open log: %w", err)
	}
	defer fh.Close()
	fi, err := fh.Stat()
	if err != nil {
		return 0, fmt.Errorf("stat log: %w", err)
	}
	size := fi.Size()
	start := int64(0)
	if size > logsTailWindow {
		start = size - logsTailWindow
	}
	buf := make([]byte, size-start)
	if _, err := fh.ReadAt(buf, start); err != nil && !errors.Is(err, io.EOF) {
		return 0, fmt.Errorf("read log: %w", err)
	}
	text := string(buf)
	if start > 0 {
		// Drop the partial first line of a mid-file window.
		if i := strings.IndexByte(text, '\n'); i >= 0 {
			text = text[i+1:]
		}
	}
	text = strings.TrimSuffix(text, "\n")
	var all []string
	if text != "" {
		all = strings.Split(text, "\n")
	}
	if n < len(all) {
		all = all[len(all)-n:]
	}
	for _, l := range all {
		fmt.Fprintln(w, cleanLogText(l))
	}
	return size, nil
}

// followLogs tails the id's newest log, switching to a newer one when it
// appears, until the request/run is terminal or ctx is cancelled. Terminal
// is sampled before each read, so the final appended bytes are drained.
func followLogs(ctx context.Context, w io.Writer, dataDir string, t logsTarget, lines int) error {
	var (
		cur    logFile
		offset int64
		carry  string
		have   bool
	)
	flush := func() {
		if carry != "" {
			fmt.Fprintln(w, cleanLogText(carry))
			carry = ""
		}
	}
	for {
		done := t.terminal()
		if next, ok := newestLog(t.files()); ok {
			if !have || next.path != cur.path {
				flush()
				off, err := printTail(w, dataDir, next, lines)
				if err != nil {
					return err
				}
				cur, offset, have = next, off, true
			} else if next.size < offset {
				offset = 0 // truncated in place
			}
			var err error
			if offset, carry, err = copyAppended(w, cur.path, offset, carry); err != nil {
				return err
			}
		}
		if done {
			flush()
			return nil
		}
		select {
		case <-ctx.Done():
			flush()
			return nil
		case <-time.After(logsPollInterval):
		}
	}
}

// copyAppended prints complete lines appended to path after offset,
// keeping a trailing partial line in carry until its newline arrives.
func copyAppended(w io.Writer, path string, offset int64, carry string) (int64, string, error) {
	fh, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return offset, carry, nil
		}
		return offset, carry, fmt.Errorf("open log: %w", err)
	}
	defer fh.Close()
	if _, err := fh.Seek(offset, io.SeekStart); err != nil {
		return offset, carry, fmt.Errorf("seek log: %w", err)
	}
	b, err := io.ReadAll(fh)
	if err != nil {
		return offset, carry, fmt.Errorf("read log: %w", err)
	}
	if len(b) == 0 {
		return offset, carry, nil
	}
	text := carry + string(b)
	offset += int64(len(b))
	i := strings.LastIndexByte(text, '\n')
	if i < 0 {
		return offset, text, nil
	}
	fmt.Fprintln(w, cleanLogText(text[:i]))
	return offset, text[i+1:], nil
}
