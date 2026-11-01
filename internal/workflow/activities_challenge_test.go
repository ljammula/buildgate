package workflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"buildgate/internal/run"
	"buildgate/internal/sandbox"
	"buildgate/internal/testfixture"
	wsisolation "buildgate/internal/workspace"
)

const edgeTest = "// buildgate-challenge: AC-1\npackage p\n\nimport \"testing\"\n\nfunc TestEdge(t *testing.T) {\n\tif Add(0, 0) != 0 {\n\t\tt.Fatal(\"zero\")\n\t}\n}\n"

func holdFixture(t *testing.T) (*reproFixture, ChallengerJobInput) {
	f := newReproFixture(t)
	f.input.ChallengeCriteria, f.input.ChallengeAnswers = 2, []int{1}
	return f, ChallengerJobInput{RunWorkflowInput: f.input, Commit: f.commit}
}

// The hold keeps a copy of each new test file that passes the shape rules,
// pins its hash, reads its header, and records what it refused or did not run,
// each with a note; harness state is ignored.
func TestHoldChallengeFilesAppliesTheShapeRules(t *testing.T) {
	f, in := holdFixture(t)
	writeFile(t, filepath.Join(f.repo, "p", "edge_test.go"), edgeTest)
	writeFile(t, filepath.Join(f.repo, "p", "nohdr_test.go"), "package p\n\nfunc TestNoHeader() {}\n")
	writeFile(t, filepath.Join(f.repo, "p", "bignum_test.go"), "// buildgate-challenge: AC-9\npackage p\n")
	writeFile(t, filepath.Join(f.repo, "p", "helper.go"), "package p\n")
	writeFile(t, filepath.Join(f.repo, ".pi-challenge-session", "x_test.go"), "package p\n")
	files, notes, err := f.acts.holdChallengeFiles(context.Background(), in, f.repo)
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]run.ChallengeFile{}
	for _, file := range files {
		by[file.Path] = file
	}
	if len(files) != 4 {
		t.Fatalf("files = %+v, want the four outside the harness state", files)
	}
	edge := by["p/edge_test.go"]
	if edge.Refused != "" || edge.NotRun != "" || edge.Criterion != "AC-1" || edge.Runner != "go-test-json" || len(edge.TestIDs) != 1 || edge.HostPath == "" || edge.SHA256 != "" && len(edge.SHA256) != 64 {
		t.Errorf("p/edge_test.go = %+v, want held with its header, runner and pinned hash", edge)
	}
	if info, err := os.Stat(edge.HostPath); err != nil || info.Mode().Perm() != 0o444 {
		t.Errorf("host copy: %v, %v; want a read-only file", info, err)
	}
	for _, name := range []string{"p/nohdr_test.go", "p/bignum_test.go"} {
		if by[name].NotRun == "" || by[name].HostPath == "" {
			t.Errorf("%s = %+v, want held and not run for want of a valid header", name, by[name])
		}
	}
	if by["p/helper.go"].Refused == "" || by["p/helper.go"].HostPath != "" {
		t.Errorf("p/helper.go = %+v, want refused and not held: it is no test file", by["p/helper.go"])
	}
	if len(notes) != 3 {
		t.Errorf("notes = %v, want one per refusal and per missing header", notes)
	}
}

func TestHoldChallengeFilesCapsTheCount(t *testing.T) {
	f, in := holdFixture(t)
	for i := 0; i < maxChallengeFiles+2; i++ {
		writeFile(t, filepath.Join(f.repo, "p", "e"+string(rune('a'+i))+"_test.go"), edgeTest)
	}
	files, _, err := f.acts.holdChallengeFiles(context.Background(), in, f.repo)
	if err != nil {
		t.Fatal(err)
	}
	held, refused := 0, 0
	for _, file := range files {
		if file.Refused == "" {
			held++
		} else if strings.Contains(file.Refused, "more than") {
			refused++
		}
	}
	if held != maxChallengeFiles || refused != 2 {
		t.Errorf("held %d, over the cap %d, want %d and 2", held, refused, maxChallengeFiles)
	}
}

// The commit step commits the held files as a child of the result commit with
// the pinned hash re-read from it.
func TestCommitChallengeCommitsPinnedFilesAsAChildOfTheResult(t *testing.T) {
	f, in := holdFixture(t)
	writeFile(t, filepath.Join(f.repo, "p", "edge_test.go"), edgeTest)
	files, _, err := f.acts.holdChallengeFiles(context.Background(), in, f.repo)
	if err != nil || len(files) != 1 {
		t.Fatalf("hold: %+v, %v", files, err)
	}
	if err := os.Remove(filepath.Join(f.repo, "p", "edge_test.go")); err != nil {
		t.Fatal(err)
	}
	out, err := f.acts.CommitChallengeActivity(context.Background(), CommitChallengeInput{RunWorkflowInput: f.input, ResultSHA: f.commit, Files: files})
	if err != nil {
		t.Fatal(err)
	}
	if out.SHA == "" || out.SHA == f.commit {
		t.Fatalf("new head = %q, want a child of %s", out.SHA, f.commit)
	}
	files[0].SHA256 = strings.Repeat("0", 64)
	if _, err := f.acts.CommitChallengeActivity(context.Background(), CommitChallengeInput{RunWorkflowInput: f.input, ResultSHA: out.SHA, Files: files}); err == nil {
		t.Error("a file whose pinned hash does not match its copy was committed")
	}
}

// The undo step moves the worktree and its branch back to the result commit,
// and refuses a commit it cannot resolve.
func TestUndoChallengeCommitMovesTheBranchBackToTheResult(t *testing.T) {
	f, in := holdFixture(t)
	writeFile(t, filepath.Join(f.repo, "p", "edge_test.go"), edgeTest)
	files, _, err := f.acts.holdChallengeFiles(context.Background(), in, f.repo)
	if err != nil || len(files) != 1 {
		t.Fatalf("hold: %+v, %v", files, err)
	}
	if err := os.Remove(filepath.Join(f.repo, "p", "edge_test.go")); err != nil {
		t.Fatal(err)
	}
	out, err := f.acts.CommitChallengeActivity(context.Background(), CommitChallengeInput{RunWorkflowInput: f.input, ResultSHA: f.commit, Files: files})
	if err != nil {
		t.Fatal(err)
	}
	// Refused: a challenge commit that is not HEAD, and a parent that is not the result.
	for name, in := range map[string]UndoChallengeCommitInput{
		"HEAD is not the challenge commit": {RunWorkflowInput: f.input, ResultSHA: f.commit, CommitSHA: f.commit},
		"parent is not the result commit":  {RunWorkflowInput: f.input, ResultSHA: out.SHA, CommitSHA: out.SHA},
		"unknown result":                   {RunWorkflowInput: f.input, ResultSHA: "nonesuch", CommitSHA: out.SHA},
	} {
		if err := f.acts.UndoChallengeCommitActivity(context.Background(), in); err == nil {
			t.Errorf("%s: the undo was not refused", name)
		}
		if h, _ := exec.Command("git", "-C", f.repo, "rev-parse", "HEAD").Output(); strings.TrimSpace(string(h)) != out.SHA {
			t.Fatalf("%s: a refused undo moved HEAD to %s", name, h)
		}
	}
	if err := f.acts.UndoChallengeCommitActivity(context.Background(), UndoChallengeCommitInput{RunWorkflowInput: f.input, ResultSHA: f.commit, CommitSHA: out.SHA}); err != nil {
		t.Fatal(err)
	}
	head, err := exec.Command("git", "-C", f.repo, "rev-parse", "HEAD").Output()
	if err != nil || strings.TrimSpace(string(head)) != f.commit {
		t.Fatalf("HEAD = %q, %v; want %s", head, err, f.commit)
	}
	if _, err := os.Stat(filepath.Join(f.repo, "p", "edge_test.go")); !os.IsNotExist(err) {
		t.Errorf("the committed file is still in the worktree: %v", err)
	}
}

// diff_scope sets a committed challenge file aside by its pinned hash, never
// by its path: the same path with other content is still a changed file.
func TestSetAsideChallengeFilesIsByHashNeverByPath(t *testing.T) {
	f, in := holdFixture(t)
	writeFile(t, filepath.Join(f.repo, "p", "edge_test.go"), edgeTest)
	files, _, err := f.acts.holdChallengeFiles(context.Background(), in, f.repo)
	if err != nil || len(files) != 1 {
		t.Fatalf("hold: %+v, %v", files, err)
	}
	head := trustedCommit(t, f.repo)
	changed := []string{"p/add.go", "p/edge_test.go"}
	if got := setAsideChallengeFiles(f.repo, head, changed, files); len(got) != 1 || got[0] != "p/add.go" {
		t.Errorf("set aside = %v, want only p/add.go left", got)
	}
	files[0].SHA256 = strings.Repeat("1", 64)
	if got := setAsideChallengeFiles(f.repo, head, changed, files); len(got) != 2 {
		t.Errorf("set aside with another hash = %v, want both files still judged", got)
	}
}

func TestHoldChallengeFilesRefusesAFileOverTheSizeCap(t *testing.T) {
	f, in := holdFixture(t)
	writeFile(t, filepath.Join(f.repo, "p", "big_test.go"), "// buildgate-challenge: AC-1\n"+strings.Repeat("//\n", maxChallengeBytes))
	files, _, err := f.acts.holdChallengeFiles(context.Background(), in, f.repo)
	if err != nil || len(files) != 1 || files[0].Refused == "" || files[0].HostPath != "" {
		t.Errorf("files = %+v, %v; want the oversize file refused and not held", files, err)
	}
}

// A challenge round's build gets each held file mounted read-only at its
// path, from a staged copy whose hash is checked first; the empty mountpoint
// is removed afterwards. Any other build gets nothing.
func TestChallengeRoundBuildMountsTheHeldFilesReadOnly(t *testing.T) {
	f, in := holdFixture(t)
	writeFile(t, filepath.Join(f.repo, "p", "edge_test.go"), edgeTest)
	files, _, err := f.acts.holdChallengeFiles(context.Background(), in, f.repo)
	if err != nil || len(files) != 1 {
		t.Fatalf("hold: %+v, %v", files, err)
	}
	if err := os.Remove(filepath.Join(f.repo, "p", "edge_test.go")); err != nil {
		t.Fatal(err)
	}
	input := f.input
	input.ChallengeHeld = files
	if ctx, cleanup, err := f.acts.withChallengeMounts(context.Background(), input); err != nil || len(workspaceMasksFrom(ctx)) != 0 {
		cleanup()
		t.Fatalf("a build that is not the round: masks %v, %v; want none", workspaceMasksFrom(ctx), err)
	}
	input.ChallengeRound = true
	ctx, cleanup, err := f.acts.withChallengeMounts(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	masks := workspaceMasksFrom(ctx)
	if len(masks) != 1 || masks[0].Target != "p/edge_test.go" {
		t.Fatalf("masks = %+v, want the held file at its path", masks)
	}
	if info, err := os.Stat(masks[0].Source); err != nil || info.Mode().Perm()&0o222 != 0 {
		t.Errorf("staged copy: %v, %v; want a read-only file", info, err)
	}
	if _, err := os.Stat(filepath.Join(f.repo, "p", "edge_test.go")); err != nil {
		t.Errorf("no mountpoint was made: %v", err)
	}
	cleanup()
	if _, err := os.Stat(filepath.Join(f.repo, "p", "edge_test.go")); !os.IsNotExist(err) {
		t.Errorf("the mountpoint was left in the worktree: %v", err)
	}
	input.ChallengeHeld[0].SHA256 = strings.Repeat("0", 64)
	if _, cleanup, err := f.acts.withChallengeMounts(context.Background(), input); err == nil {
		cleanup()
		t.Error("a held file whose copy no longer matches its hash was mounted")
	}
}

// Ignored output (a cache the challenger's own test run wrote) is never listed,
// and files refused for another reason do not use up the cap a test needs.
func TestHoldChallengeFilesIgnoresIgnoredOutputAndRefusalsDoNotFillTheCap(t *testing.T) {
	f, in := holdFixture(t)
	writeFile(t, filepath.Join(f.repo, ".gitignore"), ".cache/\n")
	for i := 0; i < maxChallengeFiles+3; i++ {
		writeFile(t, filepath.Join(f.repo, ".cache", "c"+string(rune('a'+i))), "x")
		writeFile(t, filepath.Join(f.repo, "a"+string(rune('a'+i))+".txt"), "x")
	}
	writeFile(t, filepath.Join(f.repo, "p", "edge_test.go"), edgeTest)
	files, notes, err := f.acts.holdChallengeFiles(context.Background(), in, f.repo)
	if err != nil {
		t.Fatal(err)
	}
	var heldEdge bool
	for _, file := range files {
		if strings.HasPrefix(file.Path, ".cache/") {
			t.Errorf("ignored file %s was listed", file.Path)
		}
		if file.Path == "p/edge_test.go" && file.Refused == "" {
			heldEdge = true
		}
	}
	if !heldEdge {
		t.Errorf("the test file was not held behind refused files: %+v", files)
	}
	if len(notes) > maxChallengeNotes+1 {
		t.Errorf("%d notes, want at most %d and one summary", len(notes), maxChallengeNotes)
	}
}

// A held file in a directory the worktree lacks is mounted: the directory is
// made for the build and removed with the mountpoint.
func TestChallengeRoundBuildMountsAFileInANewDirectory(t *testing.T) {
	f, in := holdFixture(t)
	writeFile(t, filepath.Join(f.repo, "p", "edge", "deep", "edge_test.go"), edgeTest)
	files, _, err := f.acts.holdChallengeFiles(context.Background(), in, f.repo)
	if err != nil || len(files) != 1 || files[0].Refused != "" {
		t.Fatalf("hold: %+v, %v", files, err)
	}
	if err := os.RemoveAll(filepath.Join(f.repo, "p", "edge")); err != nil {
		t.Fatal(err)
	}
	input := f.input
	input.ChallengeHeld, input.ChallengeRound = files, true
	_, cleanup, err := f.acts.withChallengeMounts(context.Background(), input)
	if err != nil {
		t.Fatalf("a held file in a new directory was not mounted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.repo, "p", "edge", "deep", "edge_test.go")); err != nil {
		t.Errorf("no mountpoint: %v", err)
	}
	cleanup()
	if _, err := os.Stat(filepath.Join(f.repo, "p", "edge")); !os.IsNotExist(err) {
		t.Errorf("the made directory was left: %v", err)
	}
}

// A mount's empty stub is excluded from git while the round builds, so
// `git add -A` never stages it, and the exclude file is put back after.
func TestChallengeRoundMountStubIsExcludedFromGit(t *testing.T) {
	f, in := holdFixture(t)
	writeFile(t, filepath.Join(f.repo, "p", "edge", "edge_test.go"), edgeTest)
	files, _, err := f.acts.holdChallengeFiles(context.Background(), in, f.repo)
	if err != nil || len(files) != 1 || files[0].Refused != "" {
		t.Fatalf("hold: %+v, %v", files, err)
	}
	if err := os.RemoveAll(filepath.Join(f.repo, "p", "edge")); err != nil {
		t.Fatal(err)
	}
	input := f.input
	input.ChallengeHeld, input.ChallengeRound = files, true
	_, cleanup, err := f.acts.withChallengeMounts(context.Background(), input)
	if err != nil {
		t.Fatalf("mount: %v", err)
	}
	add := exec.Command("git", "-C", f.repo, "add", "-A")
	if out, err := add.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v\n%s", err, out)
	}
	staged, err := exec.Command("git", "-C", f.repo, "diff", "--cached", "--name-only").Output()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(staged), "edge_test.go") {
		t.Errorf("the mount stub was staged: %q", staged)
	}
	cleanup()
	exclude, _ := exec.Command("git", "-C", f.repo, "rev-parse", "--path-format=absolute", "--git-path", "info/exclude").Output()
	if body, _ := os.ReadFile(strings.TrimSpace(string(exclude))); strings.Contains(string(body), "edge_test.go") {
		t.Errorf("the exclude file kept the held path: %q", body)
	}
}

// A spec with no numbered criteria under the exact heading makes every AC-n
// header invalid, and the pull request is told why, once.
func TestHoldChallengeFilesNamesAMissingCriteriaHeading(t *testing.T) {
	f, in := holdFixture(t)
	in.ChallengeCriteria = 0
	writeFile(t, filepath.Join(f.repo, "p", "a_test.go"), edgeTest)
	writeFile(t, filepath.Join(f.repo, "p", "b_test.go"), edgeTest)
	files, notes, err := f.acts.holdChallengeFiles(context.Background(), in, f.repo)
	if err != nil || len(files) != 2 {
		t.Fatalf("hold: %+v, %v", files, err)
	}
	named := 0
	for _, n := range notes {
		if strings.Contains(n, "Acceptance criteria") {
			named++
		}
	}
	if named != 1 {
		t.Errorf("notes = %v, want exactly one naming the missing heading", notes)
	}
}

// An answer-<n> header is valid for an answered question's own number, not
// for the count of answered questions.
func TestHoldChallengeFilesChecksAnswerNumbersAgainstTheAnsweredSet(t *testing.T) {
	f, in := holdFixture(t)
	in.ChallengeAnswers = []int{3}
	writeFile(t, filepath.Join(f.repo, "p", "q3_test.go"), strings.Replace(edgeTest, "AC-1", "answer-3", 1))
	writeFile(t, filepath.Join(f.repo, "p", "q1_test.go"), strings.Replace(edgeTest, "AC-1", "answer-1", 1))
	files, _, err := f.acts.holdChallengeFiles(context.Background(), in, f.repo)
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		switch file.Path {
		case "p/q3_test.go":
			if file.NotRun != "" || file.Criterion != "answer-3" {
				t.Errorf("q3 = %+v, want held to run", file)
			}
		case "p/q1_test.go":
			if file.NotRun == "" {
				t.Errorf("q1 = %+v, want not run: question 1 was not answered", file)
			}
		}
	}
}

// A file citing a criterion of a later ticket is held and not run, with a note.
func TestHoldChallengeFilesDoesNotRunACriterionOfALaterTicket(t *testing.T) {
	f, in := holdFixture(t)
	in.ChallengeCriteria, in.ChallengeCovered = 3, []int{1, 2}
	writeFile(t, filepath.Join(f.repo, "p", "ok_test.go"), strings.Replace(edgeTest, "AC-1", "AC-2", 1))
	writeFile(t, filepath.Join(f.repo, "p", "later_test.go"), strings.Replace(edgeTest, "AC-1", "AC-3", 1))
	files, notes, err := f.acts.holdChallengeFiles(context.Background(), in, f.repo)
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		switch file.Path {
		case "p/ok_test.go":
			if file.NotRun != "" {
				t.Errorf("covered criterion = %+v, want held to run", file)
			}
		case "p/later_test.go":
			if file.NotRun != "cites a criterion outside the ticket's and earlier tickets' coverage" {
				t.Errorf("later criterion = %+v, want not run", file)
			}
		}
	}
	if !strings.Contains(strings.Join(notes, "\n"), "outside the ticket's and earlier tickets' coverage") {
		t.Errorf("notes = %v, want one naming the later ticket", notes)
	}
}

// The challenger is given the spec only when its approved prefix still has
// the hash spec_review approved: the snapshot's own hash is not enough.
func TestChallengerJobComparesTheSpecWithTheApprovedHash(t *testing.T) {
	f, _ := holdFixture(t)
	spec := "# Spec\n"
	body := spec + "\nAnswered.\n"
	path := filepath.Join(t.TempDir(), "spec.snapshot.md")
	writeFile(t, path, body)
	whole := sha256.Sum256([]byte(body))
	approved := sha256.Sum256([]byte(spec))
	for name, tc := range map[string]struct {
		sha   string
		bytes int
		want  string
	}{
		"approved hash differs":  {strings.Repeat("0", 64), len(spec), "no longer matches"},
		"no approved hash":       {"", 0, "no approved hash"},
		"matching approved hash": {hex.EncodeToString(approved[:]), len(spec), ""},
	} {
		in := f.input
		in.ChallengeSpecPath, in.ChallengeSpecSHA256 = path, hex.EncodeToString(whole[:])
		in.ChallengeApprovedSHA256, in.ChallengeApprovedBytes = tc.sha, tc.bytes
		in.RoutePolicy, in.ChallengerRelayPolicy = &sandbox.RoutePolicy{}, &sandbox.RoutePolicy{}
		res := f.acts.runChallengerJob(context.Background(), ChallengerJobInput{
			RunWorkflowInput: in, Commit: f.commit,
		})
		if tc.want == "" {
			if strings.Contains(res.Cause, "approved") {
				t.Errorf("%s: cause %q, want past the hash check", name, res.Cause)
			}
			continue
		}
		if !strings.Contains(res.Cause, tc.want) {
			t.Errorf("%s: cause %q, want it to contain %q", name, res.Cause, tc.want)
		}
	}
}

// A job that exited non-zero (a provider that refused its last turn, a turn
// that ran out of time) still has the files it left held, with a note; a job
// that left none, or only files the shape rules refused, gives no verdict.
func TestChallengeFilesAreHeldWhateverTheJobsExitStatus(t *testing.T) {
	f, in := holdFixture(t)
	writeFile(t, filepath.Join(f.repo, "p", "edge_test.go"), edgeTest)
	held, _, err := f.acts.holdChallengeFiles(context.Background(), in, f.repo)
	if err != nil || len(held) != 1 || held[0].Refused != "" {
		t.Fatalf("held %+v, %v; want one held file", held, err)
	}
	refused := []run.ChallengeFile{{Path: "notes.txt", Refused: "not a test path"}}
	for name, tc := range map[string]struct {
		exit        int
		files       []run.ChallengeFile
		cause, note string
	}{
		"clean exit, held":      {0, held, "", ""},
		"clean exit, none":      {0, nil, "handed over no test file", ""},
		"clean exit, refused":   {0, refused, "handed over no test file", ""},
		"refused turn, held":    {42, held, "", "exit status 42; the test files it left were held"},
		"refused turn, none":    {42, nil, "exit status 42 and handed over no test", ""},
		"refused turn, refused": {42, refused, "exit status 42 and handed over no test", ""},
		"failed setup, none":    {95, nil, "exit status 95", ""},
	} {
		cause, note := challengeHoldOutcome(tc.exit, tc.files)
		if (tc.cause == "") != (cause == "") || !strings.Contains(cause, tc.cause) {
			t.Errorf("%s: cause %q, want %q", name, cause, tc.cause)
		}
		if (tc.note == "") != (note == "") || !strings.Contains(note, tc.note) {
			t.Errorf("%s: note %q, want %q", name, note, tc.note)
		}
	}
}

// Two challenge rounds with held files and one isolated run on one
// repository at once: each round's exclude block is added and removed under
// the git metadata lock, so info/exclude ends as it began plus the lines its
// other writers own, no held stub lands in any commit, and the isolated run's
// own file is committed. (SC-011.)
func TestConcurrentChallengeRoundsLeaveInfoExcludeAsTheyFoundIt(t *testing.T) {
	repo := testfixture.NewGitRepo(t)
	git := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	excludePath := git(repo, "rev-parse", "--path-format=absolute", "--git-path", "info/exclude")
	if err := os.MkdirAll(filepath.Dir(excludePath), 0o755); err != nil {
		t.Fatal(err)
	}
	const original = "# operator\n/local-only\n"
	if err := os.WriteFile(excludePath, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	base := git(repo, "rev-parse", "HEAD")
	wts := map[string]string{}
	for _, name := range []string{"r1", "r2", "iso"} {
		wts[name] = filepath.Join(t.TempDir(), name)
		git(repo, "worktree", "add", "-q", "-b", name, wts[name])
	}
	var wg sync.WaitGroup
	errs := make(chan error, 3)
	for _, name := range []string{"r1", "r2"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := heldRound(wts[name], name, 15); err != nil {
				errs <- err
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 15; i++ {
			if err := wsisolation.ExcludeHarnessArtifacts(wts["iso"]); err != nil {
				errs <- err
				return
			}
			if err := os.WriteFile(filepath.Join(wts["iso"], "iso.txt"), []byte(fmt.Sprint(i)), 0o644); err != nil {
				errs <- err
				return
			}
			git(wts["iso"], "add", "-A")
			git(wts["iso"], "commit", "-q", "--allow-empty", "-m", "iso")
		}
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	body, err := os.ReadFile(excludePath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(body), original) || strings.Contains(string(body), "held") || strings.Contains(string(body), "factoryd-run") || !strings.Contains(string(body), ".pi-build-session/") {
		t.Errorf("info/exclude = %q; want the original lines then only the harness entries", body)
	}
	for _, name := range []string{"r1", "r2", "iso"} {
		files := git(wts[name], "log", "--name-only", "--format=", base+".."+name)
		if strings.Contains(files, "held") {
			t.Errorf("%s committed a held stub: %q", name, files)
		}
		if !strings.Contains(files, map[string]string{"r1": "r1.txt", "r2": "r2.txt", "iso": "iso.txt"}[name]) {
			t.Errorf("%s did not commit its own file: %q", name, files)
		}
	}
}

// heldRound runs rounds challenge rounds in wt: mount stubs for two held
// paths, excluded from git, a commit of the round's own file, then the
// exclusion and the stubs removed.
func heldRound(wt, name string, rounds int) error {
	masks := []sandbox.WorkspaceMask{{Target: "p/held_test.go"}, {Target: "p/" + name + "_held[1].go"}}
	for i := 0; i < rounds; i++ {
		if err := os.MkdirAll(filepath.Join(wt, "p"), 0o755); err != nil {
			return err
		}
		for _, m := range masks {
			if err := os.WriteFile(filepath.Join(wt, m.Target), nil, 0o644); err != nil {
				return err
			}
		}
		unexclude, err := excludeMountsFromGit(context.Background(), wt, masks)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(wt, name+".txt"), []byte(fmt.Sprint("work", i)), 0o644); err != nil {
			return err
		}
		for _, args := range [][]string{{"add", "-A"}, {"commit", "-q", "-m", name}} {
			if out, err := exec.Command("git", append([]string{"-C", wt, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...).CombinedOutput(); err != nil {
				return fmt.Errorf("%s git %v: %v\n%s", name, args, err, out)
			}
		}
		unexclude()
		for _, m := range masks {
			_ = os.Remove(filepath.Join(wt, m.Target))
		}
	}
	return nil
}
