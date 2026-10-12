package request

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"buildgate/internal/run"
)

// The note's parts. InspectNoteFor puts together those that hold in a
// request's state.
const (
	inspectNoteCopy = "Only the \"Your own copy\" line writes: it makes a folder of your own from the commit the gates checked. " +
		"Try or change the work there, or with `gh pr checkout` when there is a pull request, never in the factory's worktree: " +
		"a commit there is not released (the push is pinned to the checked commit, and a branch that moved is refused), " +
		"a retry, resume or reconcile may reuse or remove the folder, and a review does not start on a worktree that is not clean."
	inspectNoteHost = "Running the build's tests on this machine runs code a model wrote, outside the sandbox."
	inspectNotePR   = "To send a change back, comment on the pull request from a reviewer the factory trusts (not the account it pushes as): " +
		"the comment starts a corrective round."
	inspectNoteQuarantined = "To send a change back: `factoryd retry` rebuilds the ticket, `factoryd amend-scope` widens its files."
	inspectNoteSendBack    = "`factoryd reject -to spec` redrafts the spec."
	inspectNoteHalted      = "To continue, follow the next step printed for the request (`factoryd retry` when the cause is fixed)."
	inspectNoteResume      = "To continue: `factoryd resume`, or `factoryd resume -from scratch` to rebuild the ticket."
)

// InspectDraftNote is the note printed with the commands of a draft gate.
const InspectDraftNote = "Nothing is built yet: these are the files the gate approves. Change them in " +
	"the console's editor, or reject with a reason to have them redrafted."

// InspectRunNote is the note printed with one run's commands, when no
// request state says how a change goes back.
const InspectRunNote = inspectNoteCopy + " " + inspectNoteHost

// InspectNoteFor is the note for built work in state, with the pull-request
// clause only when a ticket has a pull request, and the quarantined clause's
// send-back only while it is allowed (no ticket accepted yet).
func InspectNoteFor(state State, hasPR, canSendBack bool) string {
	parts := []string{inspectNoteCopy, inspectNoteHost}
	if hasPR {
		parts = append(parts, inspectNotePR)
	}
	switch state {
	case StateQuarantined:
		if canSendBack {
			parts = append(parts, inspectNoteQuarantined+" "+inspectNoteSendBack)
		} else {
			parts = append(parts, inspectNoteQuarantined)
		}
	case StateHalted:
		parts = append(parts, inspectNoteHalted)
	case StateResumeReview:
		parts = append(parts, inspectNoteResume)
	}
	return strings.Join(parts, " ")
}

// InspectCommand is one command an operator can copy, with what it shows.
type InspectCommand struct {
	Label   string `json:"label"`
	Command string `json:"command"`
}

// InspectTicket is the commands for one ticket's run: its worktree, the
// commits the gates checked, and the commands that read them.
type InspectTicket struct {
	Ticket   int              `json:"ticket"`
	RunID    string           `json:"run_id"`
	Worktree string           `json:"worktree,omitempty"`
	Base     string           `json:"base,omitempty"`
	Result   string           `json:"result,omitempty"`
	Commands []InspectCommand `json:"commands"`
}

// Inspection is what an operator can run to look at a request's work before
// a decision: the files of a draft gate, or each built ticket's commits.
// Paths under the home directory are written from "~" so a line pastes into
// any shell on the machine that runs the factory and shows no user name.
type Inspection struct {
	Commands []InspectCommand `json:"commands,omitempty"`
	Tickets  []InspectTicket  `json:"tickets,omitempty"`
	Note     string           `json:"note,omitempty"`
}

// Empty reports whether there is nothing to show.
func (i Inspection) Empty() bool {
	return len(i.Commands) == 0 && len(i.Tickets) == 0
}

// Inspect returns the commands for r in its current state: the drafted
// files at spec, oracle and plan review; each built ticket's latest work at
// pr_review, quarantined, halted and resume_review; nothing otherwise.
func Inspect(dataDir string, r *Request) Inspection {
	if abs, err := filepath.Abs(dataDir); err == nil {
		dataDir = abs
	}
	switch r.State {
	case StateSpecReview, StateOracleReview, StatePlanReview:
		return Inspection{Commands: draftCommands(dataDir, r), Note: InspectDraftNote}
	case StatePRReview, StateQuarantined, StateHalted, StateResumeReview:
		var out Inspection
		hasPR := false
		for _, t := range r.Tickets {
			hasPR = hasPR || t.PRURL != ""
			if ticket, ok := inspectTicket(dataDir, t, r.State == StatePRReview); ok {
				out.Tickets = append(out.Tickets, ticket)
			}
		}
		if len(out.Tickets) > 0 {
			out.Note = InspectNoteFor(r.State, hasPR, !r.AnyTicketAccepted())
		}
		return out
	default:
		return Inspection{}
	}
}

// inspectTicket is the commands for t's latest work: the last of its runs
// (the build, then each corrective round's) that recorded a result, read
// against the ticket's own base, so a round's commands show the whole change,
// not the round's slice of it. onPR counts only the rounds the pull request
// holds (accepted and pushed): a later round that recorded a result but was
// not pushed is not what a reviewer of the pull request sees.
func inspectTicket(dataDir string, t Ticket, onPR bool) (InspectTicket, bool) {
	ids := []string{t.RunID}
	for _, round := range t.Rounds {
		if onPR && (round.Outcome != RoundAccepted || !round.Pushed) {
			continue
		}
		ids = append(ids, round.RunID)
	}
	var first, latest *run.Run
	for _, id := range ids {
		if id == "" || !run.ValidID(id) {
			continue
		}
		loaded, err := run.Load(dataDir, id)
		if err != nil {
			continue
		}
		if first == nil {
			first = loaded
		}
		if latest == nil || loaded.ResultSHA != "" {
			latest = loaded
		}
	}
	if latest == nil {
		return InspectTicket{}, false
	}
	base := ticketBase(first)
	ticket := inspectRunFrom(dataDir, latest, base)
	ticket.Ticket = t.Index
	return ticket, len(ticket.Commands) > 0
}

func ticketBase(r *run.Run) string {
	if r.DiffBaseSHA != "" {
		return r.DiffBaseSHA
	}
	return r.BaseSHA
}

func draftCommands(dataDir string, r *Request) []InspectCommand {
	dir := Dir(dataDir, r.ID)
	cmds := []InspectCommand{{Label: "The spec", Command: "less " + ShellPath(SpecPath(dataDir, r.ID))}}
	switch r.State {
	case StateOracleReview:
		cmds = append(cmds, InspectCommand{Label: "The drafted oracle files", Command: "ls -R " + ShellPath(filepath.Join(dir, RequestOracleDirName))})
	case StatePlanReview:
		tickets := filepath.Join(dir, "tickets")
		cmds = append(cmds, InspectCommand{Label: "The drafted tickets", Command: "ls " + ShellPath(tickets)})
		if _, err := os.Stat(filepath.Join(tickets, "001.spec.md")); err == nil {
			cmds = append(cmds, InspectCommand{Label: "The first ticket", Command: "less " + ShellPath(filepath.Join(tickets, "001.spec.md"))})
		}
	}
	return cmds
}

// InspectRun returns the commands for one run against its own base: git
// reads of the commits the gates checked, the files it was judged against,
// and its logs.
func InspectRun(dataDir string, r *run.Run) InspectTicket {
	if abs, err := filepath.Abs(dataDir); err == nil {
		dataDir = abs
	}
	return inspectRunFrom(dataDir, r, ticketBase(r))
}

func inspectRunFrom(dataDir string, r *run.Run, base string) InspectTicket {
	t := InspectTicket{RunID: r.ID}
	if r.WorkspacePath != "" {
		if info, err := os.Stat(r.WorkspacePath); err == nil && info.IsDir() {
			t.Worktree = HomePath(r.WorkspacePath)
		}
	}
	t.Commands = append(t.Commands, gitCommands(r, base, &t)...)
	runDir := run.Dir(dataDir, r.ID)
	for _, f := range []struct{ label, name string }{
		{"The ticket it was judged against", "spec.snapshot.md"},
		{"The review's verdict on each criterion", "CONFORMITY_EVIDENCE.json"},
		{"The build's report", "BUILD_REPORT.md"},
	} {
		if _, err := os.Stat(filepath.Join(runDir, f.name)); err == nil {
			t.Commands = append(t.Commands, InspectCommand{Label: f.label, Command: "less " + ShellPath(filepath.Join(runDir, f.name))})
		}
	}
	data, id := ShellPath(dataDir), shellQuote(r.ID)
	t.Commands = append(t.Commands,
		InspectCommand{Label: "The newest log", Command: fmt.Sprintf("factoryd logs -data-dir %s %s", data, id)},
		InspectCommand{Label: "Every log and saved prompt", Command: fmt.Sprintf("factoryd logs -data-dir %s -list %s", data, id)},
	)
	return t
}

// commitName is what a recorded commit must look like before it is put in a
// command: a hex object name, so a record cannot pass a git option or shell
// text through it.
var commitName = regexp.MustCompile(`^[0-9a-f]{7,64}$`)

// gitCommands are the reads of base..result, and a worktree of the
// operator's own at the result. With no result commit they read the
// factory's worktree HEAD, labelled as not checked. None without a valid
// base, or with neither a result nor a worktree.
func gitCommands(r *run.Run, base string, t *InspectTicket) []InspectCommand {
	if !commitName.MatchString(base) {
		return nil
	}
	repo, result, checked := r.RepositoryRoot, r.ResultSHA, true
	if repo == "" && t.Worktree != "" {
		repo = r.WorkspacePath
	}
	if result == "" && t.Worktree != "" {
		repo, result, checked = r.WorkspacePath, "HEAD", false
	}
	if repo == "" || (result != "HEAD" && !commitName.MatchString(result)) {
		return nil
	}
	t.Base, t.Result = base, result
	git := "git -C " + ShellPath(repo)
	span := base + ".." + result
	if !checked {
		return []InspectCommand{
			{Label: "The worktree's commits now (no result was recorded: not what the gates checked)", Command: git + " log --oneline " + span + " --"},
			{Label: "The worktree's change now (not checked)", Command: git + " diff " + span + " --"},
		}
	}
	return []InspectCommand{
		{Label: "The commits", Command: git + " log --oneline " + span + " --"},
		{Label: "The files changed", Command: git + " diff --stat " + span + " --"},
		{Label: "The whole change", Command: git + " diff " + span + " --"},
		{
			Label:   "Your own copy, to try or change",
			Command: git + " worktree add " + ShellPath(filepath.Join(homeDir(), "review", r.ID)) + " " + result,
		},
	}
}

// HomePath returns path with the home directory written as "~", unchanged
// when it is not under the home directory.
func HomePath(path string) string {
	home := homeDir()
	if home == "" || !filepath.IsAbs(path) {
		return path
	}
	rel, err := filepath.Rel(home, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return path
	}
	if rel == "." {
		return "~"
	}
	return filepath.Join("~", rel)
}

// ShellPath is HomePath quoted for a POSIX shell: a leading "~/" stays bare
// so the shell expands it, and the rest is single-quoted when it holds
// anything but plain path characters.
func ShellPath(path string) string {
	p := HomePath(path)
	if p == "~" {
		return p
	}
	if rest, ok := strings.CutPrefix(p, "~/"); ok {
		return "~/" + shellQuote(rest)
	}
	return shellQuote(p)
}

// shellQuote leaves a word of plain characters as it is and single-quotes
// anything else. A word starting with "-", "~" or "=" is quoted too: "~" and
// (in zsh) "=" expand at the start of a word, and "-" reads as an option.
func shellQuote(s string) string {
	if s != "" && !strings.ContainsRune("-~=", rune(s[0])) && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789._-/+=@:,~") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func homeDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return home
}
