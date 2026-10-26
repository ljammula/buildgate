package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"buildgate/internal/release"
	"buildgate/internal/run"
)

// replaySpec is the ticket of the replay integration tests: content.txt is
// the ticket's own file.
const replaySpec = "# Ticket: fixture\n\n## Out of scope\n\nAllowed-Files: content.txt\n"

// replayEnv is the session config of the replay integration tests: the fake
// docker, and a release policy that allows a run whose gates pass and that
// ran in the sandbox.
func replayEnv(t *testing.T) []string {
	t.Helper()
	return isolatedSessionConfigEnv(t, "sandbox_docker: "+filepath.Join(testdataDir(t), "fake_docker.sh")+"\n"+
		"release_max_files_changed: 10\nrelease_max_insertions: 100\nrelease_rollback_plan: revert the commit\n")
}

// gitOut runs git in dir and returns its trimmed output, failing the test on
// an error.
func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// commitOnMain commits path with content on ws's checked-out main and
// returns the new commit.
func commitOnMain(t *testing.T, ws, path, content string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(ws, path), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	gitOut(t, ws, "add", "-A")
	gitOut(t, ws, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "-m", "main moves: "+path)
	return gitOut(t, ws, "rev-parse", "HEAD")
}

// patchID is `git patch-id --stable` of commit in dir.
func patchID(t *testing.T, dir, commit string) string {
	t.Helper()
	show := exec.Command("git", "-C", dir, "show", commit)
	ids := exec.Command("git", "-C", dir, "patch-id", "--stable")
	pipe, err := show.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	ids.Stdin = pipe
	if err := show.Start(); err != nil {
		t.Fatal(err)
	}
	out, err := ids.Output()
	if err != nil {
		t.Fatalf("git patch-id: %v", err)
	}
	_ = show.Wait()
	return strings.Fields(string(out))[0]
}

// replayOnto cherry-picks source's commits (source.BaseSHA..source.ResultSHA)
// onto onto in a scratch worktree, on the new local branch branch, as the
// host's replay does, removes the worktree and writes the replay file. It
// returns the replayed head and the file's path.
func replayOnto(t *testing.T, ws string, source *run.Run, onto, branch string) (head, file string) {
	t.Helper()
	scratch := filepath.Join(t.TempDir(), "replay")
	gitOut(t, ws, "worktree", "add", "-q", "-b", branch, scratch, onto)
	originals := strings.Fields(gitOut(t, ws, "rev-list", "--reverse", source.BaseSHA+".."+source.ResultSHA))
	record := run.Replay{OfRunID: source.ID, SourceRunIDs: []string{source.ID}, UpstreamSHA: source.BaseSHA, SourceHeadSHA: source.ResultSHA, OntoSHA: onto}
	for _, original := range originals {
		gitOut(t, scratch, "-c", "user.name=t", "-c", "user.email=t@example.com", "cherry-pick", "--allow-empty", original)
		replayed := gitOut(t, scratch, "rev-parse", "HEAD")
		record.Commits = append(record.Commits, run.ReplayedCommit{Original: original, Replayed: replayed, OriginalPatchID: patchID(t, ws, original), ReplayedPatchID: patchID(t, ws, replayed)})
	}
	head = gitOut(t, scratch, "rev-parse", "HEAD")
	record.HeadSHA = head
	gitOut(t, ws, "worktree", "remove", "--force", scratch)
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	file = filepath.Join(t.TempDir(), "replay.json")
	if err := os.WriteFile(file, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return head, file
}

// buildLogs are the build script's logs in a run directory.
func buildLogs(t *testing.T, dir string) []string {
	t.Helper()
	logs, err := filepath.Glob(filepath.Join(dir, "*build_app.log"))
	if err != nil {
		t.Fatal(err)
	}
	return logs
}

// acceptedSource builds the ticket once, normally, and returns its accepted
// run.
func acceptedSource(t *testing.T, ws, dataDir, verify string, env []string, extra ...string) *run.Run {
	t.Helper()
	source := runFactorydWithSpecFlagsAndDataDir(t, ws, "commit", verify, replaySpec, "30s", env, extra, dataDir)
	if len(buildLogs(t, run.Dir(dataDir, source.ID))) == 0 {
		t.Fatal("the source run left no build log: buildLogs looks for the wrong name")
	}
	if source.State != run.StateAccepted || !source.Sandboxed() {
		t.Fatalf("source run: state %q, sandboxed %v (%q), want an accepted sandboxed run", source.State, source.Sandboxed(), source.HaltError)
	}
	return source
}

// replayRunFlags are the flags of a replay run of the commits on branch,
// replayed onto onto, described by file.
func replayRunFlags(branch, onto, file string, extra ...string) []string {
	return append([]string{"-on-branch", branch, "-diff-base", onto, "-replay", file}, extra...)
}

// TestIntegrationReplayRunJudgesTheReplayedCommitWithoutABuild: a replay run
// of an accepted ticket's commits onto a moved main is accepted on the
// replayed commit with no build (a build would fail here), measures its diff
// from the new main, carries the source's sandboxed build attempt, is allowed
// by the release policy and keeps its branch.
func TestIntegrationReplayRunJudgesTheReplayedCommitWithoutABuild(t *testing.T) {
	ws := newFixtureRepo(t)
	dataDir := t.TempDir()
	env := replayEnv(t)
	source := acceptedSource(t, ws, dataDir, "true", env)
	onto := commitOnMain(t, ws, "other.txt", "main moved\n")
	const branch = "buildgate/req-1/t1-restack1"
	head, file := replayOnto(t, ws, source, onto, branch)

	r := runFactorydWithSpecFlagsAndDataDir(t, ws, "fail", "true", replaySpec, "30s", env, replayRunFlags(branch, onto, file), dataDir)
	if r.State != run.StateAccepted {
		t.Fatalf("replay run state = %q (%q, gates %+v), want accepted", r.State, r.HaltError, r.GateResults)
	}
	if r.ResultSHA != head || r.DiffBaseSHA != onto || r.Replay == nil || r.Replay.HeadSHA != head {
		t.Errorf("result %s, diff base %s, replay %+v; want result %s from %s", r.ResultSHA, r.DiffBaseSHA, r.Replay, head, onto)
	}
	if !slices.Equal(r.ChangedFiles, []string{"content.txt"}) {
		t.Errorf("ChangedFiles = %v, want only the ticket's own content.txt", r.ChangedFiles)
	}
	var builds []run.Attempt
	for _, a := range r.Attempts {
		if a.Kind == "build" {
			builds = append(builds, a)
		}
	}
	if len(builds) != 1 || builds[0].ReplayOf != source.ID || builds[0].ImageDigest == "" || builds[0].LogPath != "" {
		t.Errorf("build attempts = %+v, want one carried from %s with its image digest and no log", builds, source.ID)
	}
	if logs := buildLogs(t, run.Dir(dataDir, r.ID)); len(logs) != 0 {
		t.Errorf("a replay run has build logs: %v", logs)
	}
	decision, err := release.LoadDecision(dataDir, r.Project, r.ID)
	if err != nil || !decision.Allowed {
		t.Errorf("release decision = %+v, %v; want allowed", decision, err)
	}
	if gitOut(t, ws, "rev-parse", "refs/heads/"+branch) != head {
		t.Error("the replay branch moved or was deleted")
	}
}

// TestIntegrationReplayRunQuarantinesWhenTheNewBaseBreaksVerify: the replayed
// commit is judged on the new base: a verify that fails there quarantines the
// run, and nothing is built to fix it.
func TestIntegrationReplayRunQuarantinesWhenTheNewBaseBreaksVerify(t *testing.T) {
	ws := newFixtureRepo(t)
	dataDir := t.TempDir()
	const verify = "test ! -e breaks-verify.txt"
	env := replayEnv(t)
	source := acceptedSource(t, ws, dataDir, verify, env)
	onto := commitOnMain(t, ws, "breaks-verify.txt", "x\n")
	const branch = "buildgate/req-1/t1-restack1"
	_, file := replayOnto(t, ws, source, onto, branch)

	r := runFactorydWithSpecFlagsAndDataDir(t, ws, "fail", verify, replaySpec, "30s", env, replayRunFlags(branch, onto, file, "-verify-max-attempts", "1"), dataDir)
	if r.State != run.StateQuarantined {
		t.Fatalf("replay run state = %q (%q), want quarantined by verify on the new base", r.State, r.HaltError)
	}
	if logs := buildLogs(t, run.Dir(dataDir, r.ID)); len(logs) != 0 {
		t.Errorf("a replay run has build logs: %v", logs)
	}
}

// TestIntegrationReplayRunHaltsWhenTheBranchMovedAfterTheReplay: a branch
// that is not at the replayed head halts the run before anything runs.
func TestIntegrationReplayRunHaltsWhenTheBranchMovedAfterTheReplay(t *testing.T) {
	ws := newFixtureRepo(t)
	dataDir := t.TempDir()
	env := replayEnv(t)
	source := acceptedSource(t, ws, dataDir, "true", env)
	onto := commitOnMain(t, ws, "other.txt", "main moved\n")
	const branch = "buildgate/req-1/t1-restack1"
	head, file := replayOnto(t, ws, source, onto, branch)
	gitOut(t, ws, "branch", "-f", branch, onto)

	r := runFactorydWithSpecFlagsAndDataDir(t, ws, "fail", "true", replaySpec, "30s", env, replayRunFlags(branch, onto, file), dataDir)
	if r.State != run.StateHalted || r.HaltReasonCode != run.HaltReasonReplayRefused || !strings.Contains(r.HaltError, head) {
		t.Fatalf("replay run: state %q, code %q, error %q; want halted %s naming %s", r.State, r.HaltReasonCode, r.HaltError, run.HaltReasonReplayRefused, head)
	}
	if len(r.Attempts) != 0 {
		t.Errorf("attempts = %+v, want nothing launched", r.Attempts)
	}
}

// TestIntegrationReplayRunRunsTheFullSuiteDespiteCadence: a replay run runs
// the full suite whatever the cadence says: its commits sit on a base no
// earlier full suite saw.
func TestIntegrationReplayRunRunsTheFullSuiteDespiteCadence(t *testing.T) {
	ws := newFixtureRepo(t)
	dataDir := t.TempDir()
	cadence := []string{"-full-suite-command", "true", "-full-suite-cadence", "5"}
	env := replayEnv(t)
	source := acceptedSource(t, ws, dataDir, "true", env, cadence...)
	onto := commitOnMain(t, ws, "other.txt", "main moved\n")
	const branch = "buildgate/req-1/t1-restack1"
	_, file := replayOnto(t, ws, source, onto, branch)

	r := runFactorydWithSpecFlagsAndDataDir(t, ws, "fail", "true", replaySpec, "30s", env, replayRunFlags(branch, onto, file, cadence...), dataDir)
	ran := slices.ContainsFunc(r.GateResults, func(g run.GateResult) bool { return g.Check == "full_suite_verify" && g.Passed })
	if r.State != run.StateAccepted || !r.FullSuiteScheduled || !ran {
		t.Fatalf("replay run: state %q, full suite scheduled %v, gates %+v; want accepted with the full suite run", r.State, r.FullSuiteScheduled, r.GateResults)
	}
}

// TestIntegrationReplayRunRequiredFileOnlyInTheEarlierTicketDoesNotPass: the
// diff is measured from the new base, so a required file that only the
// earlier work (now part of the base) changed does not satisfy the ticket.
func TestIntegrationReplayRunRequiredFileOnlyInTheEarlierTicketDoesNotPass(t *testing.T) {
	ws := newFixtureRepo(t)
	dataDir := t.TempDir()
	env := replayEnv(t)
	source := acceptedSource(t, ws, dataDir, "true", env)
	onto := commitOnMain(t, ws, "earlier.txt", "the earlier ticket's file\n")
	const branch = "buildgate/req-1/t1-restack1"
	_, file := replayOnto(t, ws, source, onto, branch)
	spec := "# Ticket: fixture\n\n## Out of scope\n\nAllowed-Files: content.txt, earlier.txt\nRequired-Changed-Files: earlier.txt\n"

	r := runFactorydWithSpecFlagsAndDataDir(t, ws, "fail", "true", spec, "30s", env, replayRunFlags(branch, onto, file), dataDir)
	failed := slices.ContainsFunc(r.GateResults, func(g run.GateResult) bool { return g.Check == "required_files_changed" && !g.Passed })
	if r.State != run.StateQuarantined || !failed {
		t.Fatalf("replay run: state %q, gates %+v; want quarantined by required_files_changed", r.State, r.GateResults)
	}
}
