package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"buildgate/internal/request"
	"buildgate/internal/run"
	"buildgate/internal/sandbox"
	"buildgate/internal/sessionconfig"
	"buildgate/internal/testfixture"
	"buildgate/internal/workspace"
)

var pruneTestNow = time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

// pruneDataDir is a profile, isolated from the operator's, whose data dir
// holds one request done a month ago with one accepted run: its worktree, a
// scratch directory and a build session, and the records the readers use. It
// returns the data dir and the worktree.
func pruneDataDir(t *testing.T, extraConfig string) (string, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "xdg"))
	t.Setenv(sessionconfig.ProfileEnv, "")
	dataDir := filepath.Join(home, "data")
	writePruneFile(t, filepath.Join(sessionconfig.ConfigDir(), "config.yml"), "data_dir: "+dataDir+"\n"+extraConfig)

	repo := testfixture.NewGitRepo(t)
	head, err := exec.Command("git", "-C", repo, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	month := pruneTestNow.Add(-30 * 24 * time.Hour)
	path, branch, err := workspace.Prepare(repo, filepath.Join(dataDir, "workspaces"), "req-done-001", strings.TrimSpace(string(head)))
	if err != nil {
		t.Fatal(err)
	}
	writePruneFile(t, filepath.Join(path, "big.bin"), strings.Repeat("x", 5000))
	r := request.New("req-done", repo, "app", request.Source{Kind: request.SourceText}, month)
	r.State = request.StateDone
	r.EnteredAt = month.Format(time.RFC3339Nano)
	r.Tickets = []request.Ticket{{Index: 1, RunID: "req-done-001", Branch: branch, PRURL: "https://example.invalid/pr/1"}}
	r.TicketCount = 1
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	pass := true
	rec := &run.Run{ID: "req-done-001", Ticket: "req-done-001", RequestID: "req-done", TicketIndex: 1, Project: "app",
		ProjectPath: repo, WorkspacePath: path, Branch: branch, State: run.StateAccepted,
		CreatedAt: month.Format(time.RFC3339), UpdatedAt: month.Format(time.RFC3339),
		AgentEvidence: &run.AgentEvidence{Rounds: []run.AgentEvidenceRound{{Index: 1, VerifyPassed: &pass}}},
		Attempts:      []run.Attempt{{Kind: "build", StartedAt: month.Format(time.RFC3339), FinishedAt: month.Add(time.Minute).Format(time.RFC3339), LogPath: filepath.Join(run.Dir(dataDir, "req-done-001"), "abc.attempt-1-build_app.log")}}}
	if err := rec.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	runDir := run.Dir(dataDir, "req-done-001")
	writePruneFile(t, filepath.Join(runDir, "abc.attempt-1-build_app.log"), "build output\n")
	writePruneFile(t, filepath.Join(runDir, "progress.jsonl"), `{"stage":"build","at":"`+month.Format(time.RFC3339)+`"}`+"\n")
	writePruneFile(t, filepath.Join(runDir, "prompts", "build-1", "build.md"), "the prompt")
	writePruneFile(t, filepath.Join(runDir, run.BuildSessionDirName, "s.jsonl"), "session")
	writePruneFile(t, filepath.Join(sandbox.RunScratchDir(dataDir, "req-done-001"), "obj"), "cache")
	return dataDir, path
}

func writePruneFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func runPrune(t *testing.T, dp *deps, input string, interactive bool, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := pruneRun(dp, args, strings.NewReader(input), &out, interactive, pruneTestNow)
	return out.String(), err
}

// readerOutputs is what `factoryd stats` (both forms) and `factoryd watch
// -no-follow` print for the data dir's request and run.
func readerOutputs(t *testing.T, dp *deps, dataDir string) []string {
	t.Helper()
	var outs []string
	for _, args := range [][]string{{"-data-dir", dataDir, "-all"}, {"-data-dir", dataDir, "-request", "req-done"}, {"-data-dir", dataDir, "-json", "-all"}} {
		var b bytes.Buffer
		err := statsRun(args, &b, pruneTestNow)
		outs = append(outs, b.String(), errString(err))
	}
	for _, watch := range []func(*os.File) error{
		func(f *os.File) error { return watchRequest(dp, f, dataDir, "req-done", true) },
		func(f *os.File) error { return watchRun(f, dataDir, "req-done-001", true) },
	} {
		f, err := os.CreateTemp(t.TempDir(), "watch")
		if err != nil {
			t.Fatal(err)
		}
		werr := watch(f)
		_, _ = f.Seek(0, 0)
		b, _ := io.ReadAll(f)
		f.Close()
		outs = append(outs, string(b), errString(werr))
	}
	return outs
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func recordBytes(t *testing.T, dataDir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, p := range []string{request.Path(dataDir, "req-done"), filepath.Join(run.Dir(dataDir, "req-done-001"), "run.json"),
		filepath.Join(run.Dir(dataDir, "req-done-001"), "abc.attempt-1-build_app.log"), filepath.Join(run.Dir(dataDir, "req-done-001"), "progress.jsonl"),
		filepath.Join(run.Dir(dataDir, "req-done-001"), "prompts", "build-1", "build.md")} {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("record %s: %v", p, err)
		}
		out[p] = string(b)
	}
	return out
}

func TestPruneLeavesStatsAndWatchOutputAndEveryRecordUnchanged(t *testing.T) {
	dataDir, worktree := pruneDataDir(t, "")
	dp := newTestDeps(t)
	before, records := readerOutputs(t, dp, dataDir), recordBytes(t, dataDir)
	if !strings.Contains(before[0], "app") || !strings.Contains(before[2], "ticket 1") || !strings.Contains(before[6], "req-done") || !strings.Contains(before[8], "req-done-001") {
		t.Fatalf("the readers print nothing of the fixture, so comparing them proves nothing: %q", before)
	}
	out, err := runPrune(t, dp, "", false, "-yes", "-images=false")
	if err != nil {
		t.Fatalf("prune: %v\n%s", err, out)
	}
	if _, err := os.Lstat(worktree); !os.IsNotExist(err) {
		t.Fatalf("the worktree is still there: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Freed ") || !strings.Contains(out, "worktree") {
		t.Errorf("output does not list the worktree and the bytes freed:\n%s", out)
	}
	if after := readerOutputs(t, dp, dataDir); !reflect.DeepEqual(before, after) {
		t.Errorf("stats/watch output changed by prune:\nbefore %q\nafter  %q", before, after)
	}
	if after := recordBytes(t, dataDir); !reflect.DeepEqual(records, after) {
		t.Errorf("a record changed")
	}
}

func TestPruneListsOnlyWithoutATerminalAndAsksOnOne(t *testing.T) {
	dataDir, worktree := pruneDataDir(t, "")
	dp := newTestDeps(t)
	out, err := runPrune(t, dp, "", false, "-images=false")
	if err != nil || !strings.Contains(out, "Listed only") || !strings.Contains(out, displayPath(worktree)) {
		t.Fatalf("no terminal: %v\n%s", err, out)
	}
	out, err = runPrune(t, dp, "n\n", true, "-images=false")
	if err != nil || !strings.Contains(out, "Remove these? [y/N]") || !strings.Contains(out, "Nothing removed") {
		t.Fatalf("answered no: %v\n%s", err, out)
	}
	if _, err := os.Lstat(worktree); err != nil {
		t.Fatalf("the worktree went without a yes: %v", err)
	}
	out, err = runPrune(t, dp, "y\n", true, "-images=false")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(worktree); !os.IsNotExist(err) {
		t.Fatalf("answered yes, the worktree stays:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "runs", "req-done-001", "run.json")); err != nil {
		t.Fatal(err)
	}
}

func TestPruneRefusesWithNoSessionConfig(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv(sessionconfig.ProfileEnv, "")
	if _, err := runPrune(t, newTestDeps(t), "", false, "-images=false"); err == nil || !strings.Contains(err.Error(), "no session config") {
		t.Fatalf("err = %v, want a refusal naming the missing session config", err)
	}
}

func pruneImage(hex, kind string, created time.Time, size int64, tags ...string) dockerImage {
	return dockerImage{ID: "sha256:" + strings.Repeat(hex, 64), Kind: kind, RepoTags: tags, Created: created, UniqueSize: size}
}

func TestPruneRemovesOnlyUnpinnedUnusedOldImagesOfAProfilesRepositories(t *testing.T) {
	pinned := "localhost:5050/buildgate-worker@sha256:" + strings.Repeat("a", 64)
	pruneDataDir(t, "sandbox_image: "+pinned+"\n")
	old := pruneTestNow.Add(-30 * 24 * time.Hour)
	inUse := pruneImage("c", "meter", old, 5, "localhost:5050/buildgate-worker:busy")
	inUse.Containers = 1
	images := []dockerImage{
		pruneImage("a", "worker", old, 10, "localhost:5050/buildgate-worker:local"),
		pruneImage("b", "worker", old, 3e6, "localhost:5050/buildgate-worker:older"),
		pruneImage("1", "worker", old, 2e9, "localhost:5050/buildgate-worker:r1", "localhost:5050/buildgate-worker:r2"),
		inUse,
		pruneImage("d", "install", old, 5, "localhost:5050/buildgate-worker:install"),
		pruneImage("e", "toolchain", pruneTestNow.Add(-time.Hour), 5, "localhost:5050/buildgate-worker:new"),
		// Labelled, old and unpinned, but in no repository a profile names:
		// a derived image inherits the label.
		pruneImage("f", "worker", old, 3e6, "example.com/other-app:1"),
		pruneImage("2", "relay", old, 3e6),
	}
	dp := newTestDeps(t)
	fd := fakeDockerOf(dp)
	fd.buildgateImagesFn = func(context.Context, string) ([]dockerImage, error) { return images, nil }
	var removed [][]string
	fd.removeImageFn = func(_ context.Context, _ string, refs []string) error {
		removed = append(removed, refs)
		return nil
	}
	total := int64(100e9)
	fd.imagesBytesFn = func(context.Context, string) (int64, error) {
		t := total
		total -= 2003e6
		return t, nil
	}
	out, err := runPrune(t, dp, "", false, "-yes")
	if err != nil {
		t.Fatalf("prune: %v\n%s", err, out)
	}
	want := [][]string{{"localhost:5050/buildgate-worker:r1", "localhost:5050/buildgate-worker:r2"}, {"sha256:" + strings.Repeat("b", 64)}}
	if !reflect.DeepEqual(removed, want) {
		t.Errorf("removed %v, want %v", removed, want)
	}
	if !strings.Contains(out, "Freed 2.0 GB in Docker") || !strings.Contains(out, "build cache: left alone") {
		t.Errorf("output:\n%s", out)
	}
}

func TestPruneKeepsAnImagePinnedByADigestOrBySideConfigOutsideTheProfileDir(t *testing.T) {
	pruneDataDir(t, "sandbox_image: localhost:5050/buildgate-worker:local\n")
	side := t.TempDir()
	digest := strings.Repeat("9", 64)
	writePruneFile(t, filepath.Join(side, "side.yml"), "data_dir: "+filepath.Join(side, "data")+"\nsandbox_image: localhost:5050/buildgate-worker@sha256:"+digest+"\n")
	if err := os.MkdirAll(filepath.Join(side, "data"), 0o750); err != nil {
		t.Fatal(err)
	}
	old := pruneTestNow.Add(-30 * 24 * time.Hour)
	// Its id is not the pinned digest; the pinned one is the digest of its
	// repo digest, as `docker image inspect` reports a pushed image.
	byRepoDigest := pruneImage("7", "worker", old, 1, "localhost:5050/buildgate-worker:side")
	byRepoDigest.RepoDigests = []string{"localhost:5050/buildgate-worker@sha256:" + digest}
	free := pruneImage("8", "worker", old, 2, "localhost:5050/buildgate-worker:free")
	dp := newTestDeps(t)
	fd := fakeDockerOf(dp)
	fd.buildgateImagesFn = func(context.Context, string) ([]dockerImage, error) {
		return []dockerImage{byRepoDigest, free}, nil
	}
	short := func(img dockerImage) string { return img.short() }
	// Without the side config nothing names the image: it is listed.
	out, err := runPrune(t, dp, "", false)
	if err != nil || !strings.Contains(out, short(byRepoDigest)) {
		t.Fatalf("control: %v\n%s", err, out)
	}
	out, err = runPrune(t, dp, "", false, "-config", filepath.Join(side, "side.yml"))
	if err != nil {
		t.Fatalf("prune -config: %v\n%s", err, out)
	}
	if strings.Contains(out, short(byRepoDigest)) || !strings.Contains(out, short(free)) {
		t.Errorf("the side config's pin must keep its image and only it:\n%s", out)
	}
}

func TestPruneLeavesAnImageThatIsInUseWhenRereadBeforeRemoval(t *testing.T) {
	pruneDataDir(t, "sandbox_image: localhost:5050/buildgate-worker:local\n")
	old := pruneTestNow.Add(-30 * 24 * time.Hour)
	img := pruneImage("5", "worker", old, 7, "localhost:5050/buildgate-worker:old")
	dp := newTestDeps(t)
	fd := fakeDockerOf(dp)
	calls := 0
	fd.buildgateImagesFn = func(context.Context, string) ([]dockerImage, error) {
		calls++
		listed := img
		if calls > 1 {
			listed.Containers = 1 // a container started after the listing
		}
		return []dockerImage{listed}, nil
	}
	fd.removeImageFn = func(_ context.Context, _ string, refs []string) error {
		t.Errorf("removed %v", refs)
		return nil
	}
	fd.imagesBytesFn = func(context.Context, string) (int64, error) { return 1, nil }
	out, err := runPrune(t, dp, "", false, "-yes")
	if err != nil || !strings.Contains(out, "left") {
		t.Fatalf("prune: %v\n%s", err, out)
	}
}

func TestPruneRefusesAnAgeUnderADay(t *testing.T) {
	pruneDataDir(t, "")
	for _, age := range []string{"1h", "23h", "0d"} {
		if _, err := runPrune(t, newTestDeps(t), "", false, "-images=false", "-older-than", age); err == nil || !strings.Contains(err.Error(), "-older-than") {
			t.Errorf("-older-than %s: err = %v, want a refusal", age, err)
		}
	}
}

func TestPruneSkipsACorruptRequestAndListsIt(t *testing.T) {
	dataDir, _ := pruneDataDir(t, "")
	b, err := os.ReadFile(request.Path(dataDir, "req-done"))
	if err != nil {
		t.Fatal(err)
	}
	writePruneFile(t, request.Path(dataDir, "req-copy"), string(b))
	out, err := runPrune(t, newTestDeps(t), "", false, "-images=false")
	if err != nil || !strings.Contains(out, "skipped: request req-copy") {
		t.Fatalf("prune: %v\n%s", err, out)
	}
}

func TestParseDockerImageListings(t *testing.T) {
	inspect := []byte(`[{"Id":"sha256:aa","RepoTags":["x:local"],"RepoDigests":["x@sha256:aa"],"Created":"2026-10-01T10:00:00.5Z","Config":{"Labels":{"buildgate.image":"worker"}}},
		{"Id":"sha256:bb","RepoTags":[],"Created":"2026-10-01T10:00:00Z","Config":{"Labels":{"software-factory.image":"relay"}}},
		{"Id":"sha256:cc","Created":"2026-10-01T10:00:00Z","Config":{"Labels":{"other":"x"}}}]`)
	images, err := parseImageInspect(inspect)
	if err != nil || len(images) != 2 || images[0].Kind != "worker" || images[1].Kind != "relay" || images[0].Created.IsZero() {
		t.Fatalf("images = %+v, %v", images, err)
	}
	df := []byte(`{"Images":[{"ID":"sha256:aa","Containers":"2","UniqueSize":"2.488GB"}]}`)
	images, err = withDiskUsage(images, df)
	if err != nil || images[0].Containers != 2 || images[0].UniqueSize != 2488000000 {
		t.Fatalf("with df = %+v, %v", images, err)
	}
	if images[1].Containers != 1 {
		t.Errorf("an image df does not list must count as in use, got %+v", images[1])
	}
	total, err := parseImagesTotal([]byte("{\"Type\":\"Images\",\"Size\":\"324.2GB\"}\n{\"Type\":\"Containers\",\"Size\":\"1GB\"}\n"))
	if err != nil || total != 324200000000 {
		t.Errorf("total = %d, %v", total, err)
	}
}

func TestWorkerAutoPruneRunsAtMostOnceADay(t *testing.T) {
	dataDir, worktree := pruneDataDir(t, "")
	dp := newTestDeps(t)
	if !autoPruneOnce(dp, dataDir, 14*24*time.Hour, pruneTestNow) {
		t.Fatal("first prune did not run")
	}
	if _, err := os.Lstat(worktree); !os.IsNotExist(err) {
		t.Fatal("the worker's prune left the worktree")
	}
	if autoPruneOnce(dp, dataDir, 14*24*time.Hour, pruneTestNow.Add(time.Hour)) {
		t.Error("a second prune within a day ran")
	}
	if !autoPruneOnce(dp, dataDir, 14*24*time.Hour, pruneTestNow.Add(25*time.Hour)) {
		t.Error("a prune a day later did not run")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	autoPruneLoop(ctx, dp, dataDir, 0, nil) // off: returns at once
}

func TestImagePinsMatchTheIDAndTheDigestPartOfEveryRepoDigest(t *testing.T) {
	img := dockerImage{ID: "sha256:" + strings.Repeat("1", 64), RepoTags: []string{"r/app:t"},
		RepoDigests: []string{"r/app@sha256:" + strings.Repeat("2", 64)}}
	for _, ref := range []string{"sha256:" + strings.Repeat("1", 64), "sha256:" + strings.Repeat("2", 64),
		"other/name@sha256:" + strings.Repeat("1", 64), "other/name@sha256:" + strings.Repeat("2", 64), "r/app:t"} {
		if !img.pinnedBy(map[string]bool{ref: true}) {
			t.Errorf("pin %s does not name the image", ref)
		}
	}
	if img.pinnedBy(map[string]bool{"sha256:" + strings.Repeat("3", 64): true, "r/app:other": true}) {
		t.Error("an unrelated pin names the image")
	}
}
