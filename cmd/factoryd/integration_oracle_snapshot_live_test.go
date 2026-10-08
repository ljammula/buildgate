package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"buildgate/internal/evidence"
	"buildgate/internal/run"
	"buildgate/internal/testfixture"
)

// TestLiveDockerBuildPhaseMountsTheOracleSnapshot is the build-phase-specific
// live-Docker proof for the reference oracle: the build container mounts
// a run-owned SNAPSHOT of the oracle directory taken before the container
// started, not the live source directory, and cannot write to it. Every
// other in-loop test uses the fake
// docker and so never mounts anything; TestRunLiveDockerCannotWriteReferenceOracleMount
// proves read-only-ness at the sandbox layer but not which directory the build
// phase hands it.
//
// While the real build container is running, the host edits the source oracle
// directory. The probe script (testdata/live_oracle_snapshot_build_app.sh) then
// reports what the container sees: it must still be the ORIGINAL content, the
// recorded build hash must be the original's hash, and a write into the mount
// must be refused. The run's final state is not asserted: the post-build gate
// takes its own snapshot at gate time and may legitimately react to the edit.
func TestLiveDockerBuildPhaseMountsTheOracleSnapshot(t *testing.T) {
	if os.Getenv("DOCKER_SANDBOX_LIVE") != "1" {
		t.Skip("set DOCKER_SANDBOX_LIVE=1 to run the real Docker acceptance test")
	}
	if os.Geteuid() == 0 {
		t.Skip("needs a non-root user: with workspace isolation disabled a root run resolves identity 0:0, which the sandbox launch spec rejects")
	}
	liveRoot := os.Getenv("DOCKER_SANDBOX_LIVE_ROOT")
	if liveRoot == "" {
		liveRoot = os.TempDir()
	}
	repoRoot, err := os.MkdirTemp(liveRoot, "factoryd-oracle-snapshot-live-repo-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(repoRoot) })
	ws := testfixture.NewGitRepoAt(t, repoRoot)
	dataDir, err := os.MkdirTemp(liveRoot, "factoryd-oracle-snapshot-live-data-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dataDir) })
	if err := os.Chmod(dataDir, 0o777); err != nil {
		t.Fatal(err)
	}
	// Outside the workspace (ReferenceOracleSourceContained) and visible to the
	// engine's VM like the other live tests' directories.
	oracleDir, err := os.MkdirTemp(liveRoot, "factoryd-oracle-snapshot-live-oracle-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(oracleDir) })
	if err := os.Chmod(oracleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	const original = "package verify\n\nimport \"testing\"\n\nfunc TestOracleX(t *testing.T) {}\n"
	const edited = "package verify\n\nimport \"testing\"\n\nfunc TestOracleX(t *testing.T) { t.Fatal(\"edited on the host mid-build\") }\n"
	oracleFile := filepath.Join(oracleDir, "x_oracle_test.go")
	if err := os.WriteFile(oracleFile, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	wantHash, err := evidence.SHA256Tree(oracleDir)
	if err != nil {
		t.Fatal(err)
	}

	script, err := filepath.Abs("testdata/live_oracle_snapshot_build_app.sh")
	if err != nil {
		t.Fatal(err)
	}
	hostDone := make(chan probeAnswer, 1)
	go func() { hostDone <- answerOracleProbe(dataDir, oracleFile, edited) }()

	// No image is configured for this run's throwaway session config, and a
	// run without one is refused, so name one the way
	// TestWorkerUIDSeparationLiveDocker does (and, like it, with the real
	// HOME, or the docker CLI loses colima's context).
	t.Setenv("HOME", realHomeForLiveDocker)
	workerImage := os.Getenv("DOCKER_SANDBOX_IMAGE")
	if workerImage == "" {
		workerImage = testfixture.ResolveImageDigest(t, "docker", "python:3.13-slim-bookworm")
	}
	r := runFactorydWithSpecFlagsAndDataDir(t, ws, "commit", "true", "", "6m", xdgOnlySandboxDockerOverride(t, "docker"), []string{
		"-sandbox-image", workerImage,
		"-build-app-script", script,
		"-reference-oracle-dir", oracleDir,
		"-reference-oracle-mount-path", ".oracle",
		"-reference-oracle-command", "true",
		"-reference-oracle-in-loop-retry",
	}, dataDir)
	var answer probeAnswer
	select {
	case answer = <-hostDone:
		if answer.err != nil {
			t.Fatalf("host side of the probe (state=%s): %v", r.State, answer.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("the build container never signalled readiness, so the mid-build edit never happened (state=%s)", r.State)
	}

	if got, _ := os.ReadFile(oracleFile); string(got) != edited {
		t.Fatalf("non-vacuity: the source oracle was not edited mid-build (got %q)", got)
	}
	if answer.seen != original {
		t.Errorf("the build container saw %q, want the ORIGINAL snapshot content %q: it is mounting the live source directory", answer.seen, original)
	}
	if strings.TrimSpace(answer.write) != "blocked" {
		t.Errorf("a write into the oracle mount was not refused (probe reported %q)", answer.write)
	}
	var build *run.Attempt
	for i := range r.Attempts {
		if r.Attempts[i].Kind == "build" {
			build = &r.Attempts[i]
			break
		}
	}
	if build == nil {
		t.Fatalf("no build attempt recorded (state=%s): %+v", r.State, r.Attempts)
	}
	if build.ReferenceOracleSHA256 != wantHash {
		t.Errorf("build Attempt.ReferenceOracleSHA256 = %q, want %q (the pre-edit tree hash)", build.ReferenceOracleSHA256, wantHash)
	}
	if editedHash, err := evidence.SHA256Tree(oracleDir); err == nil && build.ReferenceOracleSHA256 == editedHash {
		t.Errorf("the recorded build hash equals the EDITED source tree's hash: the live directory was hashed")
	}
}

// probeAnswer is what the build-phase probe reported: the oracle content it
// saw after the host's edit, and whether its write into the mount was refused.
type probeAnswer struct {
	seen, write string
	err         error
}

// answerOracleProbe is the host side of live_oracle_snapshot_build_app.sh. The
// build runs in the run's own worktree under dataDir, which is removed when the
// run ends, so it finds the probe there, edits the SOURCE oracle, signals the
// probe on, reads its two answers and only then acks, which lets the probe exit.
func answerOracleProbe(dataDir, oracleFile, edited string) probeAnswer {
	wait := func(pattern string) (string, error) {
		for deadline := time.Now().Add(4 * time.Minute); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
			if found, _ := filepath.Glob(pattern); len(found) > 0 {
				return found[0], nil
			}
		}
		return "", fmt.Errorf("no %s: %w", pattern, os.ErrDeadlineExceeded)
	}
	ready, err := wait(filepath.Join(dataDir, "workspaces", "*", ".live-ready"))
	if err != nil {
		return probeAnswer{err: err}
	}
	worktree := filepath.Dir(ready)
	if err := os.WriteFile(oracleFile, []byte(edited), 0o644); err != nil {
		return probeAnswer{err: err}
	}
	if err := os.WriteFile(filepath.Join(worktree, ".live-go"), nil, 0o666); err != nil {
		return probeAnswer{err: err}
	}
	if _, err := wait(filepath.Join(worktree, ".live-write")); err != nil {
		return probeAnswer{err: err}
	}
	seen, err := os.ReadFile(filepath.Join(worktree, ".live-seen"))
	if err != nil {
		return probeAnswer{err: err}
	}
	write, err := os.ReadFile(filepath.Join(worktree, ".live-write"))
	if err != nil {
		return probeAnswer{err: err}
	}
	return probeAnswer{seen: string(seen), write: string(write), err: os.WriteFile(filepath.Join(worktree, ".live-ack"), nil, 0o666)}
}
