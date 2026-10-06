package main

// TestWorkerUIDSeparationLiveDocker is the real-engine acceptance test
// for Phase 6 (worker UID separation, CLAIMS.md's matching deferred
// note): a sandboxed worker container running under a *dedicated* UID
// (sandbox.DefaultWorkerUID), sharing only a GID with the factoryd host
// process, must (1) actually be able to write into a worktree factoryd
// itself created and exclusively owned beforehand (internal/workspace.
// EnableWorkerGroupWrite's whole reason to exist), (2) leave files
// attributable to that dedicated UID, not factoryd's own -- closing the
// gap where worker-authored content was indistinguishable from
// factoryd's own -- and (3) leave the workspace in a state factoryd's own trusted
// host-side git operations (runner.GitCommitAll, runner.GitRevParseHEAD)
// can still act on afterward, including committing a *new directory* the
// worker itself created (the specific risk this mechanism's own docs flag:
// a worker-created directory that came back without group-write would
// make factoryd's later `git worktree remove --force` unable to unlink
// entries inside it -- see LaunchSpec.WorkerUmask's doc comment). This is
// the live proof CLAIMS.md's deferred note asked for: "verify these
// [GitCommitAll/GitRevParseHEAD] still work against a chown'd worktree."
//
// This deliberately drives internal/workspace and internal/sandbox and
// internal/runner directly, rather than through the full factoryd binary
// (unlike sandbox_default_live_test.go): the mechanism under test is the
// EnableWorkerGroupWrite/WorkerUmask/DisableWorkerGroupWrite chain itself,
// and driving it directly keeps this test's own assertions about file
// ownership and permissions unambiguous, without a full CLI/JSON round
// trip in between.
//
// hostBindMountPreservesContainerUID probes a real, significant caveat
// found while writing this test on this repo's own colima-backed Docker
// (found via review, 2026-09-07): a container's declared --user is
// genuinely honored *inside* the container (a worker process really runs
// as, and can only `id` as, the dedicated UID -- verified directly with
// `docker run --user 65532:... alpine id`) but colima's shared-folder bind
// mount for a host path under $HOME normalizes the *host-visible* owner of
// a file the container creates back to the mounting host user, not the
// container's own declared UID -- confirmed by writing a file as UID 65532
// through the same bind-mount mechanism this test itself uses and
// stat(2)-ing it from the host afterward. On a native Linux Docker host
// (a bind mount over the same real filesystem, no VM/virtiofs/9p
// boundary in between -- the expected production deployment target for a
// daemon that itself needs the Docker socket), no such remapping happens
// and host-visible ownership genuinely reflects the container's UID; this
// is a property of colima's (and likely Docker Desktop's) shared-mount
// implementation specifically. This matters beyond test hygiene: it means
// the gap where worker-authored content is indistinguishable from
// factoryd's own is NOT actually closed by this mechanism on such a host,
// even though the in-container privilege separation (a distinct process
// identity that cannot itself become factoryd's UID) still is. Probed
// once per test run rather than hardcoded on GOOS, since Linux CI itself
// may run inside a container runtime with its own remapping (e.g. a
// userns-remapped daemon) -- this asks the actual Docker backend in use,
// not the host OS.
import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"buildgate/internal/runner"
	"buildgate/internal/sandbox"
	"buildgate/internal/testfixture"
	wsisolation "buildgate/internal/workspace"
)

// hostBindMountPreservesContainerUID reports whether this Docker backend's
// bind mounts expose a container-created file's real declared UID to the
// host's own stat(2), by actually writing one and checking. See this
// test's own doc comment above for why this can legitimately differ by
// environment (colima found not to, here) and why it's probed rather than
// assumed from GOOS.
func hostBindMountPreservesContainerUID(t *testing.T, dockerBinary, liveRoot string) bool {
	t.Helper()
	probeDir, err := os.MkdirTemp(liveRoot, "factoryd-worker-uid-probe-")
	if err != nil {
		t.Fatalf("create UID-preservation probe dir: %v", err)
	}
	defer os.RemoveAll(probeDir)
	probeUID := sandbox.DefaultWorkerUID
	cmd := exec.Command(dockerBinary, "run", "--rm",
		"--volume", probeDir+":/probe",
		"--user", fmt.Sprintf("%d:%d", probeUID, os.Getgid()),
		"alpine", "sh", "-c", "touch /probe/marker")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("UID-preservation probe container: %v: %s", err, out)
	}
	info, err := os.Stat(filepath.Join(probeDir, "marker"))
	if err != nil {
		t.Fatalf("stat UID-preservation probe marker: %v", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	return int(stat.Uid) == probeUID
}

func TestWorkerUIDSeparationLiveDocker(t *testing.T) {
	if os.Getenv("DOCKER_SANDBOX_LIVE") != "1" {
		t.Skip("set DOCKER_SANDBOX_LIVE=1 to run the real Docker acceptance test")
	}
	// This test's own process shells out to the real "docker" binary
	// directly (testfixture.ResolveImageDigest below, and its own later
	// docker run calls) -- see
	// TestRunSandboxWithRetriesReachesModelOnlyThroughRelayLiveDocker's
	// comment on the same line for why TestMain's package-wide HOME
	// override otherwise makes the real docker CLI lose colima's context
	// (found live 2026-09-14: "pull python:3.13-slim-bookworm ... failed
	// to connect to the docker API at unix:///var/run/docker.sock").
	t.Setenv("HOME", realHomeForLiveDocker)
	dockerBinary := "docker"
	liveRoot := os.Getenv("DOCKER_SANDBOX_LIVE_ROOT")
	if liveRoot == "" {
		liveRoot = os.TempDir()
	}
	workerImage := os.Getenv("DOCKER_SANDBOX_IMAGE")
	if workerImage == "" {
		workerImage = testfixture.ResolveImageDigest(t, dockerBinary, "python:3.13-slim-bookworm")
	}

	repoDir, err := os.MkdirTemp(liveRoot, "factoryd-worker-uid-repo-")
	if err != nil {
		t.Fatalf("create repository: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(repoDir) })
	for _, args := range [][]string{{"init"}, {"config", "user.email", "test@example.com"}, {"config", "user.name", "Test"}} {
		if out, err := exec.Command("git", append([]string{"-C", repoDir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(repoDir, "README.md"), []byte("fixture\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", repoDir, "add", ".").CombinedOutput(); err != nil {
		t.Fatalf("git add: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "-C", repoDir, "commit", "-m", "fixture").CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v: %s", err, out)
	}
	baseSHA, err := runner.GitRevParseHEAD(repoDir)
	if err != nil {
		t.Fatalf("resolve base SHA: %v", err)
	}

	parentDir, err := os.MkdirTemp(liveRoot, "factoryd-worker-uid-worktree-parent-")
	if err != nil {
		t.Fatalf("create worktree parent: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(parentDir) })

	worktreePath, branch, err := wsisolation.Prepare(repoDir, parentDir, "worker-uid-live", baseSHA)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	removed := false
	t.Cleanup(func() {
		if !removed {
			_ = wsisolation.Remove(repoDir, worktreePath, branch)
		}
	})

	// This is the setup step under test: grants factoryd's own primary
	// group write access across the fresh worktree, since factoryd (a
	// non-root process) can never chown these paths to the dedicated
	// worker UID outright -- see EnableWorkerGroupWrite's own doc comment.
	if err := wsisolation.EnableWorkerGroupWrite(worktreePath, os.Getgid()); err != nil {
		t.Fatalf("EnableWorkerGroupWrite: %v", err)
	}

	logDir, err := os.MkdirTemp(liveRoot, "factoryd-worker-uid-log-")
	if err != nil {
		t.Fatalf("create log dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(logDir) })

	workerUID := sandbox.DefaultWorkerUID
	if workerUID == os.Getuid() {
		// Guards this test's own environment assumption (found via
		// review): sandbox.ValidateWorkerUID would refuse this
		// configuration in production for exactly this reason, and this
		// test's entire premise -- proving the worker's files are
		// attributable to a UID *other* than factoryd's own -- is moot
		// if they happen to collide in whatever environment runs it.
		t.Skipf("sandbox.DefaultWorkerUID (%d) equals this test process's own UID; cannot exercise UID separation here", workerUID)
	}

	spec := sandbox.LaunchSpec{
		Image:       workerImage,
		WorkDir:     worktreePath,
		LogPath:     filepath.Join(logDir, "worker.log"),
		Name:        "factoryd-worker-uid-live-test",
		User:        fmt.Sprintf("%d:%d", workerUID, os.Getgid()),
		WorkerUmask: "0002",
		Command: []string{"/bin/sh", "-c",
			// Writes a new top-level file AND a new subdirectory
			// containing a file -- the subdirectory specifically
			// exercises the teardown risk this mechanism's own docs
			// name: a directory the worker creates must itself come
			// back group-writable (via WorkerUmask), or factoryd's
			// later git/removal operations can't unlink entries inside
			// it despite being able to write the worktree's other,
			// pre-existing paths.
			"set -e; echo worker-write > worker-created.txt; mkdir worker-created-dir; echo nested > worker-created-dir/nested.txt",
		},
		Memory: "512m", CPUs: "1", TmpfsSize: "64m",
		Timeout: 60 * time.Second,
		Network: "none",
		RunID:   "worker-uid-live",
		DataDir: t.TempDir(),
	}
	result, err := sandbox.Run(context.Background(), dockerBinary, spec)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.ExitCode != 0 {
		contents, _ := os.ReadFile(spec.LogPath)
		t.Fatalf("exit code = %d, want 0; log=%s", result.ExitCode, contents)
	}

	// (1): the worker's own writes must exist regardless of this host's
	// bind-mount UID behavior -- this is the part every environment must
	// prove: a worker running under a UID that only shares a *group* with
	// factoryd, against a worktree EnableWorkerGroupWrite prepared, can
	// actually write new files and new directories into it.
	for _, rel := range []string{"worker-created.txt", "worker-created-dir/nested.txt"} {
		if _, err := os.Stat(filepath.Join(worktreePath, rel)); err != nil {
			t.Fatalf("stat %s: %v (worker could not write under its dedicated UID)", rel, err)
		}
	}

	// (2): host-visible attribution to the dedicated UID -- the actual
	// fix for the attribution gap -- only asserted when this Docker
	// backend's own bind mount is confirmed to preserve it (see
	// hostBindMountPreservesContainerUID's doc comment: colima's
	// shared-folder mount here does not, though a
	// native Linux Docker host's bind mount does). Skipping the assertion
	// on a backend that doesn't preserve it is not the same as skipping
	// the whole test -- every other property this test proves (the
	// worker can actually write, and factoryd's own git operations still
	// work afterward) holds regardless of this specific host quirk.
	if hostBindMountPreservesContainerUID(t, dockerBinary, liveRoot) {
		for _, rel := range []string{"worker-created.txt", "worker-created-dir/nested.txt"} {
			path := filepath.Join(worktreePath, rel)
			info, err := os.Stat(path)
			if err != nil {
				t.Fatalf("stat %s: %v", rel, err)
			}
			stat, ok := info.Sys().(*syscall.Stat_t)
			if !ok {
				t.Skip("this platform's os.FileInfo.Sys() is not *syscall.Stat_t")
			}
			if int(stat.Uid) != workerUID {
				t.Errorf("%s: owned by UID %d, want the dedicated worker UID %d (worker UID separation did not take effect)", rel, stat.Uid, workerUID)
			}
			if int(stat.Uid) == os.Getuid() {
				t.Errorf("%s: owned by factoryd's own UID %d -- indistinguishable from factoryd's own content, exactly the attribution gap this mechanism exists to close", rel, os.Getuid())
			}
		}
	} else {
		t.Logf("this Docker backend's bind mount does not preserve a container's declared UID in host-visible file ownership (see hostBindMountPreservesContainerUID's doc comment) -- skipping the host-attribution assertion; every other property below is still checked")
	}

	// (3): factoryd's own trusted host-side git operations, run as
	// factoryd's own UID (never the worker's), must still work against a
	// worktree partly owned by a different UID -- including committing a
	// worker-created directory tree.
	if err := runner.GitCommitAll(worktreePath, "worker-uid-live test commit"); err != nil {
		t.Fatalf("host-side GitCommitAll against a worker-UID-separated worktree failed: %v", err)
	}
	resultSHA, err := runner.GitRevParseHEAD(worktreePath)
	if err != nil {
		t.Fatalf("host-side GitRevParseHEAD against a worker-UID-separated worktree failed: %v", err)
	}
	if resultSHA == baseSHA {
		t.Fatalf("GitRevParseHEAD returned the base SHA %q; the safety-net commit above did not actually land", baseSHA)
	}

	// Teardown: revoking the group-write grant on factoryd-owned paths
	// must not error (best-effort though it is), and the subsequent
	// worktree removal -- which must delete the worker-owned directory
	// the worker itself created -- must still succeed. This is the
	// concrete proof that WorkerUmask's group-write-on-create closes the
	// teardown-permission gap a plain EnableWorkerGroupWrite-only scheme
	// would leave open (see LaunchSpec.WorkerUmask's own doc comment).
	if err := wsisolation.DisableWorkerGroupWrite(worktreePath); err != nil {
		t.Fatalf("DisableWorkerGroupWrite: %v", err)
	}
	if err := wsisolation.Remove(repoDir, worktreePath, branch); err != nil {
		t.Fatalf("Remove (worktree cleanup after worker UID separation): %v", err)
	}
	removed = true
}
