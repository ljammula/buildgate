package release

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"buildgate/internal/run"
)

func TestEvaluateDecisionFailsClosedWhenKillSwitchEngaged(t *testing.T) {
	dir := t.TempDir()
	if err := Engage(dir, "app", "operator", "incident", func() string { return "2026-08-27T00:00:00Z" }); err != nil {
		t.Fatal(err)
	}
	candidate := cleanMergeCandidate()
	decision, err := EvaluateDecision(dir, "app", candidate, MergePolicy{RollbackPlan: "rollback-v1", MaxFilesChanged: 10, MaxInsertions: 100}, "now")
	if err != nil {
		t.Fatal(err)
	}
	if decision.Allowed || len(decision.Reasons) == 0 {
		t.Fatalf("decision = %+v, want denied with reasons", decision)
	}
}

func TestEvaluateDecisionSavesAuditableAllowedDecision(t *testing.T) {
	dir := t.TempDir()
	decision, err := EvaluateDecision(dir, "app", cleanMergeCandidate(), MergePolicy{RollbackPlan: "rollback-v1", MaxFilesChanged: 10, MaxInsertions: 100}, "now")
	if err != nil {
		t.Fatal(err)
	}
	if !decision.Allowed {
		t.Fatalf("decision = %+v, want allowed", decision)
	}
	if err := SaveDecision(dir, decision); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "projects", "app", "release-decisions", "run-1.json")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("decision mode = %o, want 600", info.Mode().Perm())
	}
}

// TestEvaluateDecisionRecordsTestsRequiredOptOut is the tests_added
// gate's own requirement that a ticket's opt-out is recorded on the
// release decision itself, not only on the run record -- see
// run.Run.TestsRequiredOptOut's own doc comment for why.
func TestEvaluateDecisionRecordsTestsRequiredOptOut(t *testing.T) {
	dir := t.TempDir()
	candidate := cleanMergeCandidate()
	candidate.TestsRequiredOptOut = "this ticket only updates documentation"
	decision, err := EvaluateDecision(dir, "app", candidate, MergePolicy{RollbackPlan: "rollback-v1", MaxFilesChanged: 10, MaxInsertions: 100}, "now")
	if err != nil {
		t.Fatal(err)
	}
	if decision.TestsRequiredOptOut != "this ticket only updates documentation" {
		t.Errorf("TestsRequiredOptOut = %q, want it copied from the candidate run", decision.TestsRequiredOptOut)
	}
}

// TestSaveDecisionPublishesAtomicallyNotByTruncation is the regression for a
// real Codex finding on this PR: SaveDecision used to write through
// os.WriteFile directly, which truncates the destination file before
// writing its new contents -- so a GET /runs/{id}/release racing a save
// could observe a partial document and LoadDecision would fail to unmarshal
// it, matching the same class of bug kill_switch.go was fixed for in
// PR #46. A single-threaded save-then-reload can't distinguish
// truncate-then-write from temp-file-plus-rename -- both look identical
// once the write completes -- so this drives a real writer/reader race:
// one goroutine repeatedly overwrites the same decision with large content
// (to widen the truncate-then-write vulnerable window) while another
// concurrently reloads it, asserting every read is either the previous
// complete decision or the new one, never a truncation-induced unmarshal
// error. This is expected to fail intermittently under -race on the
// pre-fix os.WriteFile implementation and pass deterministically once
// SaveDecision publishes through rename, since POSIX rename is atomic with
// respect to a concurrent open/read.
func TestSaveDecisionPublishesAtomicallyNotByTruncation(t *testing.T) {
	dir := t.TempDir()
	longReason := make([]string, 2000)
	for i := range longReason {
		longReason[i] = "a padding reason to make each write larger and the truncation window wider"
	}
	first := Decision{RunID: "run-1", Project: "app", Allowed: true, Reasons: longReason, Evaluated: "2026-09-03T10:00:00Z"}
	if err := SaveDecision(dir, first); err != nil {
		t.Fatal(err)
	}

	const iterations = 200
	done := make(chan error, 1)
	go func() {
		for i := 0; i < iterations; i++ {
			d := first
			d.Evaluated = "2026-09-03T11:00:00Z"
			if i%2 == 1 {
				d.Allowed = false
			}
			if err := SaveDecision(dir, d); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()

	for i := 0; i < iterations; i++ {
		loaded, err := LoadDecision(dir, "app", "run-1")
		if err != nil {
			t.Fatalf("LoadDecision raced a save and observed a non-atomic write: %v", err)
		}
		if loaded == nil {
			t.Fatal("LoadDecision returned nil for a run that was saved before this loop started")
		}
	}
	if err := <-done; err != nil {
		t.Fatalf("SaveDecision failed: %v", err)
	}

	path := filepath.Join(dir, "projects", "app", "release-decisions", "run-1.json")
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "run-1.json" {
			t.Fatalf("stray file left behind after concurrent saves: %q", e.Name())
		}
	}
}

// TestLoadDecisionRoundTripsAndRejectsEscapingIdentifiers proves the read
// counterpart of SaveDecision returns exactly what was recorded, reports a
// run with no decision as (nil, nil) rather than fabricating an allowed one,
// and refuses a project or run id that is not a single path component — the
// values reaching it are derived from a run record and an HTTP path.
func TestLoadDecisionRoundTripsAndRejectsEscapingIdentifiers(t *testing.T) {
	dir := t.TempDir()
	saved := Decision{RunID: "run-1", Project: "app", Allowed: false, Reasons: []string{"kill switch is engaged"}, Evaluated: "2026-09-03T10:00:00Z"}
	if err := SaveDecision(dir, saved); err != nil {
		t.Fatal(err)
	}

	loaded, err := LoadDecision(dir, "app", "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if loaded == nil || loaded.Allowed || loaded.Evaluated != saved.Evaluated || len(loaded.Reasons) != 1 {
		t.Fatalf("decision = %+v, want the saved denied decision", loaded)
	}

	missing, err := LoadDecision(dir, "app", "run-never-decided")
	if err != nil {
		t.Fatalf("unrecorded decision = error %v, want (nil, nil)", err)
	}
	if missing != nil {
		t.Fatalf("unrecorded decision = %+v, want nil", missing)
	}

	for _, bad := range [][2]string{{"..", "run-1"}, {"app", "../../etc/passwd"}, {"", "run-1"}, {"a/b", "run-1"}} {
		if _, err := LoadDecision(dir, bad[0], bad[1]); err == nil {
			t.Errorf("LoadDecision(%q, %q) succeeded, want rejection", bad[0], bad[1])
		}
	}
}

func cleanMergeCandidate() run.Run {
	return run.Run{ID: "run-1", State: run.StateAccepted, BaseSHA: "base", ResultSHA: "result", ChangedFiles: []string{"lib/app.go"}, DiffStat: &run.DiffStat{FilesChanged: 1, Insertions: 2}, GateResults: []run.GateResult{{Check: "verify", Passed: true}}, DependencyLockfilesTouched: []string{}, Attempts: []run.Attempt{{Kind: "build", ImageDigest: "sha256:deadbeef"}}}
}

// TestRecordDecisionAndInvalidateDecisionSerializeAndDoNotLoseInvalidation
// is the regression test for a real GitHub Codex App review finding on
// this PR: RecordDecision (a fresh evaluation, possibly Allowed: true)
// and InvalidateDecision (cross-run attribution recorded after the fact)
// used to share no lock at all, so whichever one saved last silently won
// regardless of which one was actually more current. Run with -race:
// many goroutines call RecordDecision concurrently with one goroutine
// calling InvalidateDecision partway through; the final state must always
// be denied and Invalidated, never a race-won Allowed: true.
func TestRecordDecisionAndInvalidateDecisionSerializeAndDoNotLoseInvalidation(t *testing.T) {
	dir := t.TempDir()
	candidate := cleanMergeCandidate()
	cfg := MergePolicy{RollbackPlan: "rollback-v1", MaxFilesChanged: 10, MaxInsertions: 100}

	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := RecordDecision(dir, "app", candidate, cfg); err != nil {
				t.Errorf("RecordDecision: %v", err)
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := InvalidateDecision(dir, "app", candidate.ID, "regression attributed by a later run", "now"); err != nil {
			t.Errorf("InvalidateDecision: %v", err)
		}
	}()
	wg.Wait()

	// One more RecordDecision after every goroutine above has finished --
	// the invalidation must have already landed by now (WaitGroup ordering
	// guarantees that), so this must see it and stay denied.
	final, err := RecordDecision(dir, "app", candidate, cfg)
	if err != nil {
		t.Fatalf("final RecordDecision: %v", err)
	}
	if final == nil {
		t.Fatal("final RecordDecision returned nil decision, want one")
	}
	if final.Allowed {
		t.Error("final RecordDecision.Allowed = true, want false")
	}
	if !final.Invalidated {
		t.Error("final RecordDecision.Invalidated = false, want true")
	}
	decision, err := LoadDecision(dir, "app", candidate.ID)
	if err != nil {
		t.Fatalf("LoadDecision: %v", err)
	}
	if decision == nil {
		t.Fatal("decision = nil, want one to exist")
	}
	if decision.Allowed {
		t.Error("decision.Allowed = true, want false -- the invalidation must never be lost to a racing RecordDecision")
	}
	if !decision.Invalidated {
		t.Error("decision.Invalidated = false, want true")
	}
}

func TestProjectOfPrefersRecordedProjectOverDerivation(t *testing.T) {
	r := &run.Run{ProjectPath: "/home/u/code/payments", Project: "payments"}
	if got := ProjectOf(r); got != "payments" {
		t.Errorf("ProjectOf() = %q, want %q (recorded field, never re-derived)", got, "payments")
	}
}

func TestProjectOfFallsBackToDerivationForLegacyRuns(t *testing.T) {
	// Project left unset, as a record predating the field would have it;
	// the path does not exist and is not a repository, so the derivation
	// falls through to the path's own basename.
	r := &run.Run{ProjectPath: "/nonexistent/myapp"}
	if got := ProjectOf(r); got != "myapp" {
		t.Errorf("ProjectOf() = %q, want %q (fallback derivation)", got, "myapp")
	}
}

// TestProjectFromWorkspaceIsTheRepositoryBasename pins the single
// derivation every entry point shares: the containing repository's own
// basename, whether the path is the repository root (`factoryd submit
// ~/code/payments`) or the empty <repo>/workspace placeholder the
// product-spec intake layout keeps inside it, and never the parent
// directory's ("code") or the placeholder's ("workspace") -- the two
// collisions the Codex review of PR #96 traced back to having more than
// one derivation.
func TestProjectFromWorkspaceIsTheRepositoryBasename(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "code", "payments")
	placeholder := filepath.Join(repo, "workspace")
	if err := os.MkdirAll(placeholder, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q"}, {"commit", "-q", "--allow-empty", "-m", "init"}} {
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	for _, path := range []string{repo, placeholder} {
		if got := ProjectFromWorkspace(path); got != "payments" {
			t.Errorf("ProjectFromWorkspace(%q) = %q, want %q", path, got, "payments")
		}
	}
	plain := filepath.Join(t.TempDir(), "notes-demo")
	if err := os.MkdirAll(plain, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := ProjectFromWorkspace(plain); got != "notes-demo" {
		t.Errorf("ProjectFromWorkspace(non-repo dir) = %q, want %q", got, "notes-demo")
	}
}

// TestRejectProjectCollisionRefusesADifferentRepositoryWithTheSameBasename
// pins the Codex findings on PR #97: /clients/acme/api and /clients/beta/api
// derive the same id, so the second must be refused with both paths
// named; the same repository again is not a collision; and the claim is
// atomic across concurrent first runs -- exactly one of N racing
// repositories wins the id.
func TestRejectProjectCollisionRefusesADifferentRepositoryWithTheSameBasename(t *testing.T) {
	dataDir := t.TempDir()
	if err := RejectProjectCollision(dataDir, "api", "/clients/acme/api"); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if err := RejectProjectCollision(dataDir, "api", "/clients/acme/api"); err != nil {
		t.Fatalf("same repository again: %v", err)
	}
	err := RejectProjectCollision(dataDir, "api", "/clients/beta/api")
	if err == nil {
		t.Fatal("a different repository with the same basename was accepted")
	}
	for _, want := range []string{"/clients/acme/api", "/clients/beta/api", "distinct directory name"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	if err := RejectProjectCollision(dataDir, "other", "/clients/beta/other"); err != nil {
		t.Fatalf("unrelated project: %v", err)
	}
	if err := RejectProjectCollision(dataDir, "../evil", "/x/evil"); err == nil {
		t.Fatal("traversal-shaped project id was accepted")
	}

	// N racing first claims for one id: exactly one wins.
	raceDir := t.TempDir()
	const n = 16
	var wg sync.WaitGroup
	wins := make(chan int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if RejectProjectCollision(raceDir, "svc", fmt.Sprintf("/team%d/svc", i)) == nil {
				wins <- i
			}
		}(i)
	}
	wg.Wait()
	close(wins)
	if got := len(wins); got != 1 {
		t.Fatalf("%d racing claims succeeded, want exactly 1", got)
	}
}

// TestRecordReleaseDecisionPersistsFailureMarkerOnCorruptKillSwitch is the
// regression test for the residual observability gap left after PR #154
// made recordReleaseDecision's one real side effect (nil decision => no PR
// opens) fail-closed: a corrupted/unreadable kill_switch.json made
// RecordDecision fail with no trace on disk at all, so an accepted run's
// decision file was simply absent -- indistinguishable in the console from
// a run that was never evaluated. RecordDecision must now leave a
// DecisionFailure marker alongside where the real decision would have
// gone, and LoadDecision must report it via a distinct
// *DecisionRecordingFailedError rather than folding it into the ordinary
// (nil, nil) "no decision" case.
func TestRecordReleaseDecisionPersistsFailureMarkerOnCorruptKillSwitch(t *testing.T) {
	dir := t.TempDir()
	project := "app"
	projectDir := filepath.Join(dir, "projects", project)
	if err := os.MkdirAll(projectDir, 0o750); err != nil {
		t.Fatal(err)
	}
	killSwitchPath := filepath.Join(projectDir, "kill_switch.json")
	if err := os.WriteFile(killSwitchPath, []byte("{not valid json"), 0o600); err != nil {
		t.Fatal(err)
	}

	candidate := cleanMergeCandidate()
	cfg := MergePolicy{RollbackPlan: "rollback-v1", MaxFilesChanged: 10, MaxInsertions: 100}
	decision, err := RecordDecision(dir, project, candidate, cfg)
	if err == nil {
		t.Fatal("RecordDecision succeeded against a corrupted kill_switch.json, want an error")
	}
	if decision != nil {
		t.Fatalf("RecordDecision returned a decision (%+v) alongside an error, want nil", decision)
	}

	realDecisionPath := filepath.Join(projectDir, "release-decisions", candidate.ID+".json")
	if _, statErr := os.Stat(realDecisionPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("real decision file exists after a failed RecordDecision: %v", statErr)
	}

	markerPath := filepath.Join(projectDir, "release-decisions", candidate.ID+".error.json")
	markerBytes, statErr := os.ReadFile(markerPath)
	if statErr != nil {
		t.Fatalf("failure marker not written: %v", statErr)
	}
	var failure DecisionFailure
	if err := json.Unmarshal(markerBytes, &failure); err != nil {
		t.Fatalf("unmarshal failure marker: %v", err)
	}
	if failure.Error == "" {
		t.Error("failure.Error is empty, want the RecordDecision error text")
	}
	if failure.At == "" {
		t.Error("failure.At is empty, want a timestamp")
	}
	if failure.KillSwitchReadable {
		t.Error("failure.KillSwitchReadable = true, want false against a corrupted kill_switch.json")
	}

	// LoadDecision must report this distinctly from "no decision".
	loaded, loadErr := LoadDecision(dir, project, candidate.ID)
	if loaded != nil {
		t.Fatalf("LoadDecision returned a decision (%+v), want nil", loaded)
	}
	var recordingFailedErr *DecisionRecordingFailedError
	if !errors.As(loadErr, &recordingFailedErr) {
		t.Fatalf("LoadDecision error = %v, want a *DecisionRecordingFailedError", loadErr)
	}
	if recordingFailedErr.Failure.Error == "" {
		t.Error("DecisionRecordingFailedError.Failure.Error is empty")
	}

	// Fixing the kill switch and retrying (`factoryd retry`) must succeed
	// and clear the marker -- a leftover marker would otherwise keep
	// confusing a console/API reader after the operator has already fixed
	// the problem.
	if err := os.Remove(killSwitchPath); err != nil {
		t.Fatal(err)
	}
	if _, err := RecordDecision(dir, project, candidate, cfg); err != nil {
		t.Fatalf("RecordDecision after fixing kill_switch.json: %v", err)
	}
	if _, statErr := os.Stat(markerPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("stale failure marker still exists after a successful retry: %v", statErr)
	}
	final, err := LoadDecision(dir, project, candidate.ID)
	if err != nil {
		t.Fatalf("LoadDecision after successful retry: %v", err)
	}
	if final == nil {
		t.Fatal("LoadDecision after successful retry = nil, want the recorded decision")
	}
}
