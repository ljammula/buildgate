package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"buildgate/internal/memory"
	"buildgate/internal/release"
	"buildgate/internal/run"
	"buildgate/internal/sessionconfig"
)

// `factoryd memory` is the operator's side of repository memory. A build
// agent that ended without passing may have noted things worth knowing about
// the repository; those notes become candidate lines only this verb and the
// console's Memory tab show. The operator proposes some of them, and the
// factory opens an ordinary request whose one ticket rewrites the fenced
// section of root AGENTS.md. Nothing here runs by itself: no candidate is
// collected, proposed or dropped unless a person runs the subcommand.

// memoryOperator is who a move made from the command line is recorded as by.
const memoryOperator = "operator"

// exactListFlag collects a repeated flag's values verbatim: a line to remove
// may hold a comma, so it is never split.
type exactListFlag struct{ values []string }

func (f *exactListFlag) String() string {
	if f == nil {
		return ""
	}
	return strings.Join(f.values, "\n")
}

func (f *exactListFlag) Set(s string) error {
	f.values = append(f.values, s)
	return nil
}

type memoryFlags struct {
	set       *flag.FlagSet
	workspace *string
	config    *string
	dataDir   *string
	reason    *string
	jsonOut   *bool
	remove    *exactListFlag
}

// newMemoryFlags builds the one flag set every `factoryd memory` subcommand
// parses, in isolation from parsing, so the doc-vs-flag drift test can
// enumerate it.
func newMemoryFlags() memoryFlags {
	f := memoryFlags{set: flag.NewFlagSet("memory", flag.ContinueOnError), remove: &exactListFlag{}}
	f.workspace = f.set.String("workspace", "", "the repository whose memory to read or change: its root, or any directory inside it (required)")
	f.config = f.set.String("config", "", "session config file or profile name (default: the same search path `factoryd worker -config` uses); its memory.repositories list is the per-repository switch")
	f.dataDir = f.set.String("data-dir", "data", "directory for durable records; must match what `factoryd worker` uses")
	f.jsonOut = f.set.Bool("json", false, "list and show: print JSON instead of text")
	f.reason = f.set.String("reason", "", "drop and off: why, recorded with the change")
	f.set.Var(f.remove, "remove", "propose: an exact line of the section to remove, as `memory list` prints it. Repeatable")
	plainFlagUsage(f.set)
	return f
}

const memoryUsage = `usage: factoryd memory <subcommand> -workspace <repository> [flags] [arguments]

  list                             the switch, the budget, the lines in force and the candidates
  show <id>                        one candidate: its line, the runs that said it, its history
  add "<text>"                     add your own candidate line
  drop [-reason <why>] <id>        drop a candidate
  propose [-remove "<line>"]... [<id>...]
                                   open one request that adds the named candidates
                                   (none named: the most seen) and removes the -remove lines
  on | off [-reason <why>]         resume or stop memory for this repository's project

Flags come before the arguments.`

// memoryCmd is one invocation, resolved: which repository, which data
// directory, which session config.
type memoryCmd struct {
	dp       *deps
	out      io.Writer
	settings sessionconfig.Settings
	dataDir  string
	repoRoot string
	project  string
	jsonOut  bool
	reason   string
	remove   []string
	now      time.Time
}

func (mc *memoryCmd) stamp() string { return mc.now.UTC().Format(time.RFC3339) }

// memoryMain implements `factoryd memory`. A request `propose` submitted
// gets the worker and console a submitted request gets.
func memoryMain(dp *deps, args []string) error {
	flags := newMemoryFlags()
	requestID, err := memoryRun(dp, flags, args, os.Stdout)
	if err != nil {
		return err
	}
	if requestID != "" {
		startDependenciesAfterSubmit(dp, os.Stdout, *flags.config, *flags.dataDir)
	}
	return nil
}

// memoryRun parses and runs one subcommand, writing to out. It returns the
// id of the request it submitted, "" for every subcommand but propose.
func memoryRun(dp *deps, flags memoryFlags, args []string, out io.Writer) (string, error) {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return "", errors.New(memoryUsage)
	}
	sub := args[0]
	if err := flags.set.Parse(args[1:]); err != nil {
		return "", err
	}
	mc, err := resolveMemoryCmd(dp, flags, out)
	if err != nil {
		return "", err
	}
	rest := flags.set.Args()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	switch sub {
	case "list":
		return "", mc.list(ctx)
	case "show":
		return "", mc.show(rest)
	case "add":
		return "", mc.add(ctx, rest)
	case "drop":
		return "", mc.drop(rest)
	case "propose":
		return mc.propose(ctx, rest)
	case "on":
		return "", mc.on()
	case "off":
		return "", mc.off()
	}
	return "", fmt.Errorf("unknown memory subcommand %q\n%s", sub, memoryUsage)
}

// resolveMemoryCmd resolves the session config, the data directory and the
// repository the flags name. The project is the base name of the git root,
// as a request submitted for the repository records it.
func resolveMemoryCmd(dp *deps, flags memoryFlags, out io.Writer) (*memoryCmd, error) {
	settings, err := loadSettingsForConfig(*flags.config)
	if err != nil {
		return nil, err
	}
	if err := resolveDataDirFromSessionConfig(flags.set, flags.dataDir, *flags.config); err != nil {
		return nil, err
	}
	if *flags.workspace == "" {
		return nil, fmt.Errorf("-workspace is required\n%s", memoryUsage)
	}
	workspaceAbs, err := filepath.Abs(*flags.workspace)
	if err != nil {
		return nil, fmt.Errorf("resolve -workspace: %w", err)
	}
	if info, err := os.Stat(workspaceAbs); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("-workspace %q is not a directory", *flags.workspace)
	}
	dataAbs, err := filepath.Abs(*flags.dataDir)
	if err != nil {
		return nil, fmt.Errorf("resolve -data-dir: %w", err)
	}
	*flags.dataDir = dataAbs
	repoRoot := release.RepositoryRoot(workspaceAbs)
	project := filepath.Base(repoRoot)
	if err := memory.ValidProject(project); err != nil {
		return nil, fmt.Errorf("-workspace %q: %w", *flags.workspace, err)
	}
	return &memoryCmd{
		dp: dp, out: out, settings: settings, dataDir: dataAbs, repoRoot: repoRoot, project: project,
		jsonOut: *flags.jsonOut, reason: *flags.reason, remove: flags.remove.values, now: time.Now(),
	}, nil
}

// list prints the switch, the budget, the lines in force and the candidates.
// With memory on it first collects what finished runs' build agents noted and
// brings the store in line with the section at HEAD; with memory off, or the
// section unreadable, it reads what is there and writes nothing.
func (mc *memoryCmd) list(ctx context.Context) error {
	ro, err := readMemory(ctx, mc.dp, mc.settings, mc.dataDir, mc.repoRoot, mc.project)
	if err != nil {
		return err
	}
	collected := ""
	if ro.gateErr == nil && ro.sectionErr == nil {
		store, _, err := memoryGate(mc.dp, mc.settings, mc.dataDir, mc.repoRoot, mc.project)
		if err != nil {
			return err
		}
		added, refused, state, err := refreshMemoryStore(store, mc.dataDir, mc.project, ro.section.Lines, mc.stamp())
		if err != nil {
			return err
		}
		ro.state = state
		if added+refused > 0 {
			collected = fmt.Sprintf("collected from build agents' notes: %d new candidate(s), %d note(s) refused by the text rule\n", added, refused)
		}
	}
	view := ro.view(mc.project)
	if mc.jsonOut {
		return writeIndentedJSON(mc.out, view)
	}
	state := "on"
	if !view.On {
		state = "off -- " + view.OffReason
	}
	fmt.Fprintf(mc.out, "repository memory for %s (%s): %s\n", mc.project, mc.repoRoot, state)
	fmt.Fprintf(mc.out, "budget: %d of %d lines, %d of %d characters\n", view.UsedLines, view.BudgetLines, view.UsedChars, view.BudgetChars)
	fmt.Fprint(mc.out, collected)
	fmt.Fprintln(mc.out, "\nin force (the fenced section of AGENTS.md at HEAD):")
	switch {
	case view.SectionError != "":
		fmt.Fprintf(mc.out, "  cannot be read: %s\n", view.SectionError)
	case len(view.InForce) == 0:
		fmt.Fprintln(mc.out, "  (none)")
	}
	for _, line := range view.InForce {
		fmt.Fprintf(mc.out, "  %s\n", line)
	}
	fmt.Fprintln(mc.out, "\ncandidates:")
	if len(view.Candidates) == 0 {
		fmt.Fprintln(mc.out, "  (none)")
		return nil
	}
	tw := tabwriter.NewWriter(mc.out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "  ID\tSEEN\tSOURCE\tSTATE\tLINE")
	for _, c := range view.Candidates {
		fmt.Fprintf(tw, "  %s\t%d\t%s\t%s\t%s\n", c.ID, c.Seen, c.Source, c.State, c.Line)
	}
	return tw.Flush()
}

func writeIndentedJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// findLesson returns the index of the lesson whose id is, or uniquely starts
// with, id.
func findLesson(st *memory.StoreState, id string) (int, error) {
	found := -1
	for i, l := range st.Lessons {
		switch {
		case id == "" || !strings.HasPrefix(l.ID, id):
		case l.ID == id:
			return i, nil
		case found >= 0:
			return -1, fmt.Errorf("candidate id %q matches more than one candidate; give more of it", id)
		default:
			found = i
		}
	}
	if found < 0 {
		return -1, fmt.Errorf("no candidate has id %q (see `factoryd memory list`)", id)
	}
	return found, nil
}

// memoryRunSeen is one run that said a candidate's line, for `memory show`.
type memoryRunSeen struct {
	ID    string `json:"id"`
	Ended string `json:"ended,omitempty"`
}

// show prints one candidate. It reads the store and the run records only.
func (mc *memoryCmd) show(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: factoryd memory show -workspace <repository> <id>")
	}
	store, err := memory.OpenReadOnly(mc.dataDir, mc.project)
	if err != nil {
		return err
	}
	st, err := store.Load()
	if err != nil {
		return err
	}
	i, err := findLesson(&st, args[0])
	if err != nil {
		return err
	}
	l := st.Lessons[i]
	runs := make([]memoryRunSeen, 0, len(l.Runs))
	for _, id := range l.Runs {
		seen := memoryRunSeen{ID: id}
		if r, err := run.Load(mc.dataDir, id); err == nil && len(r.Attempts) > 0 {
			seen.Ended = r.Attempts[len(r.Attempts)-1].FinishedAt
		}
		runs = append(runs, seen)
	}
	if mc.jsonOut {
		return writeIndentedJSON(mc.out, struct {
			memory.Lesson
			RunsSeen []memoryRunSeen `json:"runs_seen"`
		}{l, runs})
	}
	fmt.Fprintf(mc.out, "%s\n\nid: %s\nstate: %s\nsource: %s\nseen in: %d run(s)\nfirst seen: %s\nlast seen: %s\n", l.Line, l.ID, l.State, l.Source, l.Seen, l.FirstSeenAt, l.LastSeenAt)
	if l.RequestID != "" {
		fmt.Fprintf(mc.out, "request: %s\n", l.RequestID)
	}
	fmt.Fprintln(mc.out, "\nruns that said it (newest last):")
	if len(runs) == 0 {
		fmt.Fprintln(mc.out, "  (none)")
	}
	for _, r := range runs {
		fmt.Fprintf(mc.out, "  %s  %s\n", r.ID, r.Ended)
	}
	fmt.Fprintln(mc.out, "\nhistory:")
	if len(l.History) == 0 {
		fmt.Fprintln(mc.out, "  (none)")
	}
	for _, h := range l.History {
		fmt.Fprintf(mc.out, "  %s  %s -> %s  by %s  %s\n", h.At, h.From, h.To, h.By, h.Reason)
	}
	return nil
}

// add records the operator's own candidate, under the text rule every
// candidate passes. A dropped candidate with the same line is a candidate
// again.
func (mc *memoryCmd) add(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return errors.New(`usage: factoryd memory add -workspace <repository> "<text>"`)
	}
	store, _, err := memoryGate(mc.dp, mc.settings, mc.dataDir, mc.repoRoot, mc.project)
	if err != nil {
		return err
	}
	lesson, err := memory.NewLesson(memory.NormaliseNote(args[0]), memory.SourceOperator, mc.stamp())
	if err != nil {
		return fmt.Errorf("%w\na line is one plain sentence of at most 120 characters: letters, digits, spaces and . , : ; ( ) ' \" / _ = + - only, with commands quoted in backticks; no address, home path, secret or markup", err)
	}
	_, section, err := memorySectionAtHead(ctx, mc.dp, mc.repoRoot)
	if err != nil {
		return err
	}
	for _, l := range section.Lines {
		if l == lesson.Line {
			return fmt.Errorf("the line is already in force in AGENTS.md at HEAD: %s", lesson.Line)
		}
	}
	said := ""
	err = store.Update(func(st *memory.StoreState) error {
		for i := range st.Lessons {
			have := &st.Lessons[i]
			if have.Line != lesson.Line {
				continue
			}
			if have.State != memory.StateDropped {
				said = fmt.Sprintf("already a %s line: %s  %s", have.State, have.ID, have.Line)
				return nil
			}
			said = fmt.Sprintf("candidate again: %s  %s", have.ID, have.Line)
			return have.Move(memory.StateCandidate, mc.stamp(), memoryOperator, "added again")
		}
		if len(st.Lessons) >= memory.MaxLessons {
			return fmt.Errorf("the store holds %d lines, its limit: drop some first", memory.MaxLessons)
		}
		st.Lessons = append(st.Lessons, lesson)
		said = fmt.Sprintf("added candidate %s  %s", lesson.ID, lesson.Line)
		return nil
	})
	if err != nil {
		return err
	}
	fmt.Fprintln(mc.out, said)
	return nil
}

// drop moves a candidate or proposed line to dropped.
func (mc *memoryCmd) drop(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: factoryd memory drop -workspace <repository> [-reason <why>] <id>")
	}
	store, _, err := memoryGate(mc.dp, mc.settings, mc.dataDir, mc.repoRoot, mc.project)
	if err != nil {
		return err
	}
	said := ""
	err = store.Update(func(st *memory.StoreState) error {
		i, err := findLesson(st, args[0])
		if err != nil {
			return err
		}
		l := &st.Lessons[i]
		if err := l.Move(memory.StateDropped, mc.stamp(), memoryOperator, mc.reason); err != nil {
			return fmt.Errorf("candidate %s is %s: %w", l.ID, l.State, err)
		}
		said = fmt.Sprintf("dropped %s  %s", l.ID, l.Line)
		if l.RequestID != "" {
			said += fmt.Sprintf("\nrequest %s still proposes it: cancel that request to stop the change", l.RequestID)
		}
		return nil
	})
	if err != nil {
		return err
	}
	fmt.Fprintln(mc.out, said)
	return nil
}

// off writes the project's stop marker. The session-config switch and the
// kill switch are not this command's: it refuses while either has memory off.
func (mc *memoryCmd) off() error {
	store, _, err := memoryGate(mc.dp, mc.settings, mc.dataDir, mc.repoRoot, mc.project)
	if err != nil {
		return err
	}
	if err := store.SetOff(memoryOperator, mc.reason); err != nil {
		return err
	}
	fmt.Fprintf(mc.out, "memory is off for %s: `factoryd memory on` resumes it\n", mc.project)
	return nil
}

// on removes the stop marker. It cannot switch on a repository the session
// config does not list, and says so.
func (mc *memoryCmd) on() error {
	engaged, err := release.IsEngaged(mc.dataDir, mc.project)
	if err != nil {
		return fmt.Errorf("memory: read the kill switch for project %q: %w", mc.project, err)
	}
	_, listed := mc.settings.MemoryFor(mc.repoRoot)
	if err := memory.Gate(listed, false, engaged); err != nil {
		if !listed && !engaged {
			return fmt.Errorf("%w\nadd this repository's root (%s) under memory.repositories: that switch is yours to edit, and `memory on` only removes the stop marker `memory off` wrote", err, mc.repoRoot)
		}
		return err
	}
	store, err := memory.Open(mc.dataDir, mc.project)
	if err != nil {
		return err
	}
	if err := store.ClearOff(); err != nil {
		return err
	}
	fmt.Fprintf(mc.out, "memory is on for %s\n", mc.project)
	return nil
}
