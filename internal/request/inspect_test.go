package request

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"buildgate/internal/run"
)

func gitRepoWithTwoCommits(t *testing.T) (dir, base, result string) {
	t.Helper()
	dir = t.TempDir()
	git := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "a.txt")
	git("commit", "-q", "-m", "base")
	base = git("rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("commit", "-q", "-am", "result")
	return dir, base, git("rev-parse", "HEAD")
}

func labels(cmds []InspectCommand) []string {
	var out []string
	for _, c := range cmds {
		out = append(out, c.Label)
	}
	return out
}

func command(t *testing.T, cmds []InspectCommand, label string) string {
	t.Helper()
	for _, c := range cmds {
		if c.Label == label {
			return c.Command
		}
	}
	t.Fatalf("no %q command in %v", label, labels(cmds))
	return ""
}

func TestInspectDraftGatesOpenTheirFiles(t *testing.T) {
	dataDir := t.TempDir()
	r := &Request{ID: "req-1", State: StateSpecReview}
	got := Inspect(dataDir, r)
	if len(got.Tickets) != 0 || got.Note != InspectDraftNote {
		t.Fatalf("spec review: %+v", got)
	}
	if c := command(t, got.Commands, "The spec"); c != "less "+ShellPath(SpecPath(dataDir, "req-1")) {
		t.Errorf("spec command = %q", c)
	}

	r.State = StateOracleReview
	if c := command(t, Inspect(dataDir, r).Commands, "The drafted oracle files"); !strings.HasSuffix(c, "/requests/req-1/oracle") {
		t.Errorf("oracle command = %q", c)
	}

	r.State = StatePlanReview
	tickets := filepath.Join(Dir(dataDir, "req-1"), "tickets")
	if err := os.MkdirAll(tickets, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := labels(Inspect(dataDir, r).Commands); strings.Join(got, "|") != "The spec|The drafted tickets" {
		t.Errorf("plan review without a ticket file: %v", got)
	}
	if err := os.WriteFile(filepath.Join(tickets, "001.spec.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if c := command(t, Inspect(dataDir, r).Commands, "The first ticket"); !strings.HasSuffix(c, "/tickets/001.spec.md") {
		t.Errorf("first ticket command = %q", c)
	}
}

func TestInspectBuiltTicketReadsTheCheckedCommitsFromTheRepository(t *testing.T) {
	dataDir := t.TempDir()
	repo, base, result := gitRepoWithTwoCommits(t)
	worktree := t.TempDir()
	built := &run.Run{ID: "run-1", Ticket: "t", RepositoryRoot: repo, WorkspacePath: worktree, BaseSHA: base, ResultSHA: result, State: run.StateQuarantined}
	if err := built.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(run.Dir(dataDir, "run-1"), "CONFORMITY_EVIDENCE.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := &Request{ID: "req-1", State: StateQuarantined, Tickets: []Ticket{{Index: 1, RunID: "run-1"}, {Index: 2}}}

	got := Inspect(dataDir, r)
	if len(got.Tickets) != 1 || got.Note != InspectNoteFor(StateQuarantined, false, true) {
		t.Fatalf("inspection: %+v", got)
	}
	ticket := got.Tickets[0]
	if ticket.Ticket != 1 || ticket.Base != base || ticket.Result != result || ticket.Worktree != HomePath(worktree) {
		t.Errorf("ticket = %+v", ticket)
	}
	span := base + ".." + result
	if c := command(t, ticket.Commands, "The files changed"); c != "git -C "+ShellPath(repo)+" diff --stat "+span+" --" {
		t.Errorf("diff --stat = %q", c)
	}
	out, err := exec.Command("sh", "-c", command(t, ticket.Commands, "The commits")).CombinedOutput()
	if err != nil || !strings.Contains(string(out), "result") || strings.Contains(string(out), "base") {
		t.Errorf("the commits command printed %q (%v), want only the result commit", out, err)
	}
	if c := command(t, ticket.Commands, "Your own copy, to try or change"); !strings.HasSuffix(c, "/review/run-1 "+result) {
		t.Errorf("own copy = %q", c)
	}
	command(t, ticket.Commands, "The review's verdict on each criterion")
	for _, absent := range []string{"The ticket it was judged against", "The build's report"} {
		for _, l := range labels(ticket.Commands) {
			if l == absent {
				t.Errorf("%q is listed although its file does not exist", absent)
			}
		}
	}
}

func TestInspectDiffBaseWinsAndAGoneWorktreeIsNotNamed(t *testing.T) {
	dataDir := t.TempDir()
	repo, base, result := gitRepoWithTwoCommits(t)
	built := &run.Run{ID: "run-1", Ticket: "t", RepositoryRoot: repo, WorkspacePath: filepath.Join(dataDir, "gone"), BaseSHA: "aaaa", DiffBaseSHA: base, ResultSHA: result}
	ticket := InspectRun(dataDir, built)
	if ticket.Worktree != "" || ticket.Base != base {
		t.Errorf("ticket = %+v, want no worktree and the diff base", ticket)
	}
}

func TestInspectWithNoResultReadsTheWorktreeHeadAndOffersNoCopy(t *testing.T) {
	dataDir := t.TempDir()
	repo, base, _ := gitRepoWithTwoCommits(t)
	ticket := InspectRun(dataDir, &run.Run{ID: "run-1", RepositoryRoot: "/elsewhere", WorkspacePath: repo, BaseSHA: base})
	if c := command(t, ticket.Commands, "The worktree's change now (not checked)"); c != "git -C "+ShellPath(repo)+" diff "+base+"..HEAD --" {
		t.Errorf("worktree change = %q", c)
	}
	for _, l := range labels(ticket.Commands) {
		if strings.HasPrefix(l, "Your own copy") {
			t.Errorf("an own-copy line at HEAD of a worktree the factory may reuse")
		}
	}
	none := InspectRun(dataDir, &run.Run{ID: "run-2", RepositoryRoot: repo, WorkspacePath: filepath.Join(dataDir, "gone")})
	if got := labels(none.Commands); strings.Join(got, "|") != "The newest log|Every log and saved prompt" {
		t.Errorf("a run with no base or result: %v", got)
	}
}

func TestInspectOtherStatesShowNothing(t *testing.T) {
	for _, s := range []State{StateSubmitted, StateBuilding, StateDone, StateCancelled} {
		if got := Inspect(t.TempDir(), &Request{ID: "r", State: s}); !got.Empty() {
			t.Errorf("%s: %+v", s, got)
		}
	}
}

func TestShellPathWritesHomeAsTildeAndQuotesTheRest(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip(err)
	}
	for in, want := range map[string]string{
		filepath.Join(home, "code", "app"):     "~/code/app",
		filepath.Join(home, "my repo", "x.md"): "~/'my repo/x.md'",
		"/srv/it's":                            `'/srv/it'\''s'`,
		"/srv/plain-path_1.2":                  "/srv/plain-path_1.2",
		home:                                   "~",
	} {
		if got := ShellPath(in); got != want {
			t.Errorf("ShellPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestInspectShowsATicketsLatestRoundAgainstItsOwnBase(t *testing.T) {
	dataDir := t.TempDir()
	repo, base, result := gitRepoWithTwoCommits(t)
	first := &run.Run{ID: "run-1", Ticket: "t", RepositoryRoot: repo, WorkspacePath: repo, BaseSHA: base, ResultSHA: base}
	round := &run.Run{ID: "run-1-round1", Ticket: "t", RepositoryRoot: repo, WorkspacePath: repo, BaseSHA: base, ResultSHA: result}
	failed := &run.Run{ID: "run-1-round2", Ticket: "t", RepositoryRoot: repo, WorkspacePath: repo, BaseSHA: result}
	// Recorded a result but was quarantined and never pushed: not on the pull request.
	unpushed := &run.Run{ID: "run-1-round3", Ticket: "t", RepositoryRoot: repo, WorkspacePath: repo, BaseSHA: result, ResultSHA: base}
	for _, r := range []*run.Run{first, round, failed, unpushed} {
		if err := r.Save(dataDir); err != nil {
			t.Fatal(err)
		}
	}
	r := &Request{ID: "req-1", State: StatePRReview, Tickets: []Ticket{{
		Index: 1, RunID: "run-1", PRURL: "https://example.test/pull/1",
		Rounds: []Round{
			{Index: 1, RunID: "run-1-round1", Outcome: RoundAccepted, Pushed: true},
			{Index: 2, RunID: "run-1-round2", Outcome: RoundAccepted, Pushed: true},
			{Index: 3, RunID: "run-1-round3", Outcome: RoundQuarantined},
		},
	}}}
	got := Inspect(dataDir, r)
	if len(got.Tickets) != 1 {
		t.Fatalf("tickets: %+v", got.Tickets)
	}
	ticket := got.Tickets[0]
	if ticket.RunID != "run-1-round1" || ticket.Base != base || ticket.Result != result {
		t.Errorf("ticket = %+v, want the last pushed round with a result, against the first run's base", ticket)
	}
	r.State = StateQuarantined
	if got := Inspect(dataDir, r).Tickets[0].RunID; got != "run-1-round3" {
		t.Errorf("quarantined: run = %s, want the last round with a result, pushed or not", got)
	}
	if !strings.Contains(got.Note, "comment on the pull request") {
		t.Errorf("a request with a pull request lacks the comment clause: %q", got.Note)
	}
}

func TestInspectNoteSaysHowAChangeGoesBackInEachState(t *testing.T) {
	for state, want := range map[State]string{
		StateQuarantined:  "`factoryd retry` rebuilds the ticket",
		StateHalted:       "follow the next step printed",
		StateResumeReview: "`factoryd resume`",
	} {
		note := InspectNoteFor(state, false, true)
		if !strings.Contains(note, want) || strings.Contains(note, "comment on the pull request") || strings.Contains(note, "These commands only read") {
			t.Errorf("%s: %q", state, note)
		}
	}
	if strings.Contains(InspectNoteFor(StateQuarantined, false, false), "reject -to spec") {
		t.Error("the quarantined note offers a send-back that is refused once a ticket is accepted")
	}
	if !strings.Contains(InspectNoteFor(StatePRReview, true, true), "not the account it pushes as") {
		t.Error("the pull-request clause does not say a comment from the factory's own account starts nothing")
	}
}

func TestInspectPutsNoRecordedValueIntoACommandUnchecked(t *testing.T) {
	dataDir := t.TempDir()
	repo, base, result := gitRepoWithTwoCommits(t)
	for _, bad := range []struct{ base, result string }{
		{"--output=/tmp/x", result},
		{base, "$(touch /tmp/pwned)"},
		{base + ";id", result},
		{"", result},
	} {
		got := InspectRun(dataDir, &run.Run{ID: "run-1", RepositoryRoot: repo, BaseSHA: bad.base, ResultSHA: bad.result})
		for _, c := range got.Commands {
			if strings.HasPrefix(c.Command, "git ") {
				t.Errorf("base %q result %q: a git line was built: %q", bad.base, bad.result, c.Command)
			}
		}
	}
	got := InspectRun(dataDir, &run.Run{ID: "run 1;id", RepositoryRoot: repo, BaseSHA: base, ResultSHA: result})
	if c := command(t, got.Commands, "The newest log"); !strings.HasSuffix(c, " 'run 1;id'") {
		t.Errorf("logs line = %q, want the run id quoted", c)
	}
	if c := command(t, got.Commands, "Your own copy, to try or change"); !strings.Contains(c, "~/'review/run 1;id' ") {
		t.Errorf("own copy = %q, want the run id quoted", c)
	}
}

func TestShellQuoteHandlesShellCharacters(t *testing.T) {
	for in, want := range map[string]string{
		"a$b":   `'a$b'`,
		"a`b`":  "'a`b`'",
		"a\nb":  "'a\nb'",
		"-x":    `'-x'`,
		"~root": `'~root'`,
		"=ls":   `'=ls'`,
		"":      `''`,
	} {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}
