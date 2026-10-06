package main

import (
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
	gitDir := ws
	hostDone := make(chan error, 1)
	go func() {
		deadline := time.Now().Add(4 * time.Minute)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(filepath.Join(gitDir, ".live-ready")); err == nil {
				if err := os.WriteFile(oracleFile, []byte(edited), 0o644); err != nil {
					hostDone <- err
					return
				}
				hostDone <- os.WriteFile(filepath.Join(gitDir, ".live-go"), nil, 0o666)
				return
			}
			time.Sleep(200 * time.Millisecond)
		}
		hostDone <- os.ErrDeadlineExceeded
	}()

	r := runFactorydWithSpecFlagsAndDataDir(t, ws, "commit", "true", "", "6m", xdgOnlySandboxDockerOverride(t, "docker"), []string{
		"-sandbox-image", "",
		"-build-app-script", script,
		"-reference-oracle-dir", oracleDir,
		"-reference-oracle-mount-path", ".oracle",
		"-reference-oracle-command", "true",
		"-reference-oracle-in-loop-retry",
	}, dataDir)
	select {
	case err := <-hostDone:
		if err != nil {
			t.Fatalf("host-side edit of the source oracle: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the build container never signalled readiness, so the mid-build edit never happened")
	}

	if got, _ := os.ReadFile(oracleFile); string(got) != edited {
		t.Fatalf("non-vacuity: the source oracle was not edited mid-build (got %q)", got)
	}
	seen, err := os.ReadFile(filepath.Join(gitDir, ".live-seen"))
	if err != nil {
		t.Fatalf("the probe reported nothing (state=%s): %v", r.State, err)
	}
	if string(seen) != original {
		t.Errorf("the build container saw %q, want the ORIGINAL snapshot content %q: it is mounting the live source directory", seen, original)
	}
	if got, _ := os.ReadFile(filepath.Join(gitDir, ".live-write")); strings.TrimSpace(string(got)) != "blocked" {
		t.Errorf("a write into the oracle mount was not refused (probe reported %q)", got)
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
