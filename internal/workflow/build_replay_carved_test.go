package workflow

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.temporal.io/sdk/temporal"

	"buildgate/internal/release"
	"buildgate/internal/run"
	"buildgate/internal/workspace"
)

// carvedFixture is a real carve: source, the ticket's accepted run, changed a
// file of pr_order group 1 (db/) and one of group 2 (src/) on top of U; the
// replay run's worktree is at the commit carved from it for group 1, whose
// replay record is the one the host writes for a carve.
func newCarvedFixture(t *testing.T) *replayActivityFixture {
	t.Helper()
	f := newReplayActivityFixture(t)
	groups := [][]string{{"db/"}, {"src/"}}
	f.git("checkout", "-q", "-b", "mixed", f.upstream)
	writeFile(t, filepath.Join(f.dir, "db", "a.sql"), "create\n")
	writeFile(t, filepath.Join(f.dir, "src", "b.go"), "package src\n")
	mixed := f.commit("README.md", "mixed\n", "mixed")
	source := f.saveSource(&run.Run{ResultSHA: mixed, StackBaseSHA: f.upstream,
		ChangedFiles: []string{"README.md", "db/a.sql", "src/b.go"},
		PROrder:      &run.PROrder{Groups: groups}})
	f.saveDecision(source, release.Decision{Allowed: false})
	carved, err := workspace.CarveCommit(context.Background(), f.dir, f.upstream, mixed, []string{"db/a.sql"}, "carved")
	if err != nil {
		t.Fatal(err)
	}
	f.git("checkout", "-q", "-b", "carved", carved)
	f.input.OnBranch, f.input.DiffBaseSHA = "carved", f.upstream
	f.replay = &run.Replay{RequestID: "req-1", TicketIndex: 2, OfRunID: "source", SourceRunIDs: []string{"source"},
		UpstreamSHA: f.upstream, SourceHeadSHA: mixed, OntoSHA: f.upstream, HeadSHA: carved,
		Commits: []run.ReplayedCommit{{Original: mixed, Replayed: carved}},
		Carve:   &run.Carve{Groups: groups, Group: 1}}
	return f
}

// A carved replay of a source run the repository's pr_order denied passes the
// host's checks, which derive the carved tree again.
func TestApplyReplayAcceptsACarvedCommit(t *testing.T) {
	f := newCarvedFixture(t)
	if _, err := f.apply(); err != nil {
		t.Fatalf("a real carve was refused: %v", err)
	}
	if f.creates != 0 {
		t.Errorf("sandboxes created = %d, want none", f.creates)
	}
}

// Every way a carve can disagree with its source is a non-retryable refusal.
func TestApplyReplayRefusesACarvedCommitThatDoesNotFit(t *testing.T) {
	for name, tc := range map[string]struct {
		arrange func(f *replayActivityFixture)
		want    string
	}{
		"a source that was allowed": {func(f *replayActivityFixture) {
			source, err := run.Load(f.dataDir(), "source")
			if err != nil {
				t.Fatal(err)
			}
			f.saveDecision(source, release.Decision{Allowed: true})
		}, "not a denial for its pr_order groups"},
		"a source that changed one group": {func(f *replayActivityFixture) {
			source, _ := run.Load(f.dataDir(), "source")
			source.ChangedFiles = []string{"db/a.sql"}
			if err := source.Save(f.dataDir()); err != nil {
				t.Fatal(err)
			}
		}, "no more than one pr_order group"},
		"a source of another request": {func(f *replayActivityFixture) {
			source, _ := run.Load(f.dataDir(), "source")
			source.RequestID = "req-other"
			if err := source.Save(f.dataDir()); err != nil {
				t.Fatal(err)
			}
		}, "not of this run's request"},
		"a carved tree that holds another file": {func(f *replayActivityFixture) {
			f.git("checkout", "-q", "-b", "wide", f.upstream)
			writeFile(t, filepath.Join(f.dir, "db", "a.sql"), "create\n")
			writeFile(t, filepath.Join(f.dir, "src", "b.go"), "package src\n")
			wide := f.commit("README.md", "wide\n", "wide")
			f.replay.HeadSHA, f.replay.Commits[0].Replayed = wide, wide
			f.input.OnBranch = "wide"
		}, "holds tree"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newCarvedFixture(t)
			tc.arrange(f)
			_, err := f.apply()
			var appErr *temporal.ApplicationError
			if !errors.As(err, &appErr) || appErr.Type() != ReplayFailureType || !appErr.NonRetryable() {
				t.Fatalf("error = %v, want a non-retryable %s", err, ReplayFailureType)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

// A check that cannot read what it checks is no finding about the replay: the
// step fails retryably, never as a refusal that would use up a request's move
// to planning.
func TestApplyReplayHostReadFailureIsNotARefusal(t *testing.T) {
	f := newCarvedFixture(t)
	if err := os.RemoveAll(filepath.Join(f.dataDir(), "runs", "source")); err != nil {
		t.Fatal(err)
	}
	_, err := f.apply()
	if err == nil {
		t.Fatal("a source run that cannot be read passed")
	}
	var appErr *temporal.ApplicationError
	if errors.As(err, &appErr) && appErr.Type() == ReplayFailureType {
		t.Fatalf("error = %v, want a retryable failure, not %s", err, ReplayFailureType)
	}
	if !strings.Contains(err.Error(), "could not be checked") {
		t.Errorf("error = %v, want it to say the check could not be made", err)
	}
}
