package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"buildgate/internal/release"
	"buildgate/internal/run"
)

// factoryDirCapturingDocker is the package's fake docker behind a wrapper
// that, for every `run`, saves the launch's argv and a copy of the lint.sh in
// the directory bound at /workspace/.factory, read at launch time: the
// snapshot is gone once the launch returns. The fake runs the command on the
// host and mounts nothing, so what a launch would have seen is asserted from
// the bind's source.
func factoryDirCapturingDocker(t *testing.T, capture string) string {
	t.Helper()
	fake, err := filepath.Abs("testdata/fake_docker.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
if [ "$1" = run ]; then
	n=$(mktemp "` + capture + `/launch.XXXXXX")
	printf '%s\n' "$@" >"$n"
	for a in "$@"; do
		case "$a" in
		*:/workspace/.factory:ro)
			src="${a%:/workspace/.factory:ro}"
			printf '%s\n' "$src" >"$n.source"
			cp "$src/lint.sh" "$n.lint.sh"
			;;
		esac
	done
fi
exec "` + fake + `" "$@"
`
	path := filepath.Join(t.TempDir(), "docker-capturing")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

// A repository whose lint gate runs a script under .factory/, and a build
// that edits that script: every sandbox of the run is launched with
// /workspace/.factory bound read-only to a snapshot holding the committed
// script, the run records the commit and the snapshot's hash, and the
// result, which changes a protected path, is refused at release.
func TestIntegrationFactoryDirIsMountedFromTheCommitTheConfigWasReadFrom(t *testing.T) {
	ws := newFixtureRepo(t)
	const committedLint = "#!/bin/sh\nexit 0\n"
	if err := os.Mkdir(filepath.Join(ws, ".factory"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".factory", "lint.sh"), []byte(committedLint), 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := runGit(t, ws, "add", ".factory"); err != nil {
		t.Fatalf("git add: %v: %s", err, out)
	}
	commitFactoryYML(t, ws, "lint_command: sh .factory/lint.sh\n")
	trusted, err := runGit(t, ws, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	trusted = strings.TrimSpace(trusted)

	capture, dataDir := t.TempDir(), t.TempDir()
	env := append(isolatedSessionConfigEnv(t, "sandbox_docker: "+factoryDirCapturingDocker(t, capture)+"\n"), "FAKE_APPEND_FILE=.factory/lint.sh")
	r := runFactorydWithSpecFlagsAndDataDir(t, ws, "commit", "true", "# fixture spec\n", "60s", env, nil, dataDir)

	assertFactoryDirBoundInEveryWorkerLaunch(t, capture, ws, committedLint)
	assertFactoryDirEvidence(t, r, trusted)
	assertFactoryDirChangeRefusedAtRelease(t, r, dataDir, ws)
}

// assertFactoryDirBoundInEveryWorkerLaunch is (a): every worker launch, the
// lint gate's among them, binds a snapshot holding the committed bytes.
func assertFactoryDirBoundInEveryWorkerLaunch(t *testing.T, capture, ws, committedLint string) {
	t.Helper()
	launches, err := filepath.Glob(filepath.Join(capture, "launch.??????"))
	if err != nil || len(launches) == 0 {
		t.Fatalf("captured launches = %v (%v)", launches, err)
	}
	lintLaunches, workers := 0, 0
	for _, launch := range launches {
		argv, _ := os.ReadFile(launch)
		if !strings.Contains(string(argv), ":/workspace:") {
			// Not a worker: the mount-visibility probe has no workspace.
			continue
		}
		workers++
		got, err := os.ReadFile(launch + ".lint.sh")
		if err != nil {
			t.Errorf("a launch carries no /workspace/.factory bind: %v\nargv:\n%s", err, argv)
			continue
		}
		if string(got) != committedLint {
			t.Errorf("the bound .factory/lint.sh = %q, want the committed %q\nargv:\n%s", got, committedLint, argv)
		}
		source, _ := os.ReadFile(launch + ".source")
		if src := strings.TrimSpace(string(source)); strings.HasPrefix(src, ws) || strings.Contains(src, "/workspaces/") || !strings.Contains(src, "factory-dir-") {
			t.Errorf("bind source %q, want a per-launch snapshot outside the workspace", src)
		} else if _, err := os.Lstat(src); err == nil {
			t.Errorf("the snapshot %s is still there after the run", src)
		}
		if strings.Contains(string(argv), "sh .factory/lint.sh") {
			lintLaunches++
		}
	}
	if lintLaunches != 1 || workers < 4 {
		t.Errorf("captured %d worker launches, %d of them the lint gate's; want the baseline verify, the build, the verify and one lint gate", workers, lintLaunches)
	}
}

// assertFactoryDirEvidence is (c): the run record names the commit, and each
// attempt the snapshot.
func assertFactoryDirEvidence(t *testing.T, r *run.Run, trusted string) {
	t.Helper()
	if r.ProjectConfigCommitSHA != trusted {
		t.Errorf("project_config_commit_sha = %q, want %q", r.ProjectConfigCommitSHA, trusted)
	}
	if len(r.Attempts) < 3 {
		t.Fatalf("attempts = %+v", r.Attempts)
	}
	for _, a := range r.Attempts {
		if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(a.FactoryDirSHA256) || a.FactoryDirSHA256 != r.Attempts[0].FactoryDirSHA256 || a.FactoryDirCommit != trusted {
			t.Errorf("%s attempt: factory_dir_sha256 = %q factory_dir_commit = %q, want one hash and %s", a.Kind, a.FactoryDirSHA256, a.FactoryDirCommit, trusted)
		}
	}

}

// assertFactoryDirChangeRefusedAtRelease is (b): the fake docker runs the
// gate on the host, where the edited script still exits 0, so every gate
// passes and the run is accepted; release then refuses the result for
// changing the protected .factory/lint.sh.
func assertFactoryDirChangeRefusedAtRelease(t *testing.T, r *run.Run, dataDir, ws string) {
	t.Helper()
	if r.State != run.StateAccepted {
		t.Fatalf("state = %q, want %q (gates: %+v)", r.State, run.StateAccepted, r.GateResults)
	}
	if passed, found := gatePassed(r, "lint"); !found || !passed {
		t.Errorf("lint gate found=%v passed=%v, want passed", found, passed)
	}
	decision := readReleaseDecision(t, dataDir, release.ProjectFromWorkspace(ws), r.ID)
	named := false
	for _, reason := range decision.Reasons {
		named = named || strings.Contains(reason, ".factory/lint.sh")
	}
	if decision.Allowed || !named {
		t.Errorf("release decision allowed=%v reasons=%v, want refused naming .factory/lint.sh", decision.Allowed, decision.Reasons)
	}
}
