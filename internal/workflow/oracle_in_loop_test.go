package workflow

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.temporal.io/sdk/testsuite"

	"buildgate/internal/evidence"
	"buildgate/internal/policy"
	"buildgate/internal/runner"
)

// Tests for the Temporal/-repository path's in-loop reference oracle
// (Phase 0.5 parity): RunWorkflowInput.ReferenceOracleInLoopRetry makes
// RunBuildActivity mount the snapshotted oracle read-only into the BUILD
// container and forward the command to build_app.py, mirroring cmd/factoryd's build-phase block.

func TestBuildActivityArgsForwardsReferenceOracleCommand(t *testing.T) {
	got := buildActivityArgs("/path/build_app.py", "/ws", "/ws/spec.md", "required", 3, 45, "deadbeef", "make verify", "", "cd verify && go test ./...", "", "", "")
	found := false
	for i, a := range got {
		if a == "--reference-oracle-command" {
			if i+1 >= len(got) || got[i+1] != "cd verify && go test ./..." {
				t.Fatalf("--reference-oracle-command not followed by its value: %v", got)
			}
			found = true
		}
	}
	if !found {
		t.Fatalf("buildActivityArgs did not include --reference-oracle-command: %v", got)
	}
}

func TestBuildActivityArgsOmitsReferenceOracleCommandWhenEmpty(t *testing.T) {
	got := buildActivityArgs("/path/build_app.py", "/ws", "/ws/spec.md", "required", 3, 45, "deadbeef", "make verify", "", "", "", "", "")
	for _, a := range got {
		if a == "--reference-oracle-command" {
			t.Fatalf("buildActivityArgs included --reference-oracle-command for an empty command: %v", got)
		}
	}
}

// oracleFixture writes a small oracle directory and returns it plus its
// independently computed content hash.
func oracleFixture(t *testing.T) (dir, hash string) {
	t.Helper()
	dir = t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "oracle_test.go"), []byte("package verify\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	hash, err := evidence.SHA256Tree(dir)
	if err != nil {
		t.Fatal(err)
	}
	return dir, hash
}

// runBuildActivityWithFakeRunner executes RunBuildActivity with a fake
// runner that captures the argv build_app.py would have received.
func runBuildActivityWithFakeRunner(t *testing.T, input RunWorkflowInput) (args []string, err error) {
	t.Helper()
	activities := &Activities{
		LogDir: t.TempDir(),
		runWithRetries: func(_ context.Context, _ string, _ func(int) string, _ int, onAttempt func(int, runner.Result, error), _ string, a ...string) (runner.Result, error) {
			args = a
			res := runner.Result{Command: append([]string{"python3"}, a...), ExitCode: 0}
			onAttempt(1, res, nil)
			return res, nil
		},
	}
	wrapper := func(ctx context.Context, in RunWorkflowInput) (BuildActivityResult, error) {
		return activities.RunBuildActivity(ctx, in)
	}
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(wrapper)
	_, err = env.ExecuteActivity(wrapper, input)
	return args, err
}

func TestRunBuildActivityForwardsOracleCommandOnlyWithInLoopOptIn(t *testing.T) {
	oracleDir, _ := oracleFixture(t)
	base := fixtureInput()
	base.WorkspacePath = t.TempDir()
	base.ReferenceOracleDir = oracleDir
	base.ReferenceOracleMountPath = "verify"
	setGateCommand(&base, policy.ReferenceOracleGateID, "go test ./verify/...")

	// Opt-in: the command reaches build_app.py.
	withOptIn := base
	withOptIn.ReferenceOracleInLoopRetry = true
	args, err := runBuildActivityWithFakeRunner(t, withOptIn)
	if err != nil {
		t.Fatalf("RunBuildActivity with in-loop opt-in: %v", err)
	}
	if !containsArgPair(args, "--reference-oracle-command", "go test ./verify/...") {
		t.Errorf("args = %v, want --reference-oracle-command forwarded when ReferenceOracleInLoopRetry is set", args)
	}

	// The pairing every existing user of the post-build gate has (trio set,
	// NO opt-in) must be byte-for-byte unchanged: nothing forwarded.
	args, err = runBuildActivityWithFakeRunner(t, base)
	if err != nil {
		t.Fatalf("RunBuildActivity without in-loop opt-in: %v", err)
	}
	for _, a := range args {
		if a == "--reference-oracle-command" {
			t.Fatalf("args = %v: --reference-oracle-command was forwarded without ReferenceOracleInLoopRetry -- silently exposing gate-only oracle configuration to the build", args)
		}
	}
}

func TestRunBuildActivityRejectsIncompleteInLoopOracleConfig(t *testing.T) {
	oracleDir, _ := oracleFixture(t)
	for name, mutate := range map[string]func(*RunWorkflowInput){
		"no dir":        func(in *RunWorkflowInput) { in.ReferenceOracleDir = "" },
		"no mount path": func(in *RunWorkflowInput) { in.ReferenceOracleMountPath = "" },
		"no command":    func(in *RunWorkflowInput) { setGateCommand(in, policy.ReferenceOracleGateID, "") },
	} {
		t.Run(name, func(t *testing.T) {
			in := fixtureInput()
			in.WorkspacePath = t.TempDir()
			in.ReferenceOracleInLoopRetry = true
			in.ReferenceOracleDir = oracleDir
			in.ReferenceOracleMountPath = "verify"
			setGateCommand(&in, policy.ReferenceOracleGateID, "true")
			mutate(&in)
			_, err := runBuildActivityWithFakeRunner(t, in)
			if err == nil || !strings.Contains(err.Error(), "ReferenceOracleInLoopRetry requires") {
				t.Fatalf("error = %v, want the incomplete-configuration refusal", err)
			}
		})
	}
}

func TestRunBuildActivityRejectsOracleDirInsideWorkspace(t *testing.T) {
	workspace := t.TempDir()
	inside := filepath.Join(workspace, "verify-src")
	if err := os.MkdirAll(inside, 0o750); err != nil {
		t.Fatal(err)
	}
	in := fixtureInput()
	in.WorkspacePath = workspace
	in.ReferenceOracleInLoopRetry = true
	in.ReferenceOracleDir = inside
	in.ReferenceOracleMountPath = "verify"
	setGateCommand(&in, policy.ReferenceOracleGateID, "true")
	_, err := runBuildActivityWithFakeRunner(t, in)
	if err == nil || !strings.Contains(err.Error(), "must not be inside the workspace") {
		t.Fatalf("error = %v, want the workspace-containment refusal", err)
	}
}

// stubDocker writes a docker stand-in that appends every argv line to
// argvLog, and -- when mutate is non-empty -- runs it on `docker run`,
// simulating an external process editing the LIVE oracle directory while
// the build container is running.
func stubDocker(t *testing.T, argvLog, mutate string) string {
	t.Helper()
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" >> " + argvLog + "\n"
	if mutate != "" {
		script += "if [ \"$1\" = run ]; then " + mutate + "; fi\n"
	}
	script += "exit 0\n"
	path := filepath.Join(t.TempDir(), "docker-fake")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func executeBuildActivity(t *testing.T, activities *Activities, input RunWorkflowInput) BuildActivityResult {
	t.Helper()
	wrapper := func(ctx context.Context, in RunWorkflowInput) (BuildActivityResult, error) {
		return activities.RunBuildActivity(ctx, in)
	}
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(wrapper)
	raw, err := env.ExecuteActivity(wrapper, input)
	if err != nil {
		t.Fatalf("execute build Activity: %v", err)
	}
	var result BuildActivityResult
	if err := raw.Get(&result); err != nil {
		t.Fatalf("decode build Activity result: %v", err)
	}
	return result
}

func sandboxedBuildInput(t *testing.T, docker string, dataDir string) RunWorkflowInput {
	t.Helper()
	in := fixtureInput()
	in.WorkspacePath = t.TempDir()
	// A real spec file: the sandbox launch stages it into the container's
	// /inputs mount, unlike the fake-runner tests above that never read it.
	in.SpecPath = filepath.Join(t.TempDir(), "spec.md")
	if err := os.WriteFile(in.SpecPath, []byte("# spec\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A real (empty) build script: the launch stages it too. The stub
	// docker never executes it.
	in.BuildAppScript = filepath.Join(t.TempDir(), "build_app.py")
	if err := os.WriteFile(in.BuildAppScript, []byte("# stub\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	in.BuildAppInterpreter = "python3"
	in.SandboxImage = "factory-worker:test@sha256:deadbeef"
	in.SandboxDocker = docker
	in.RunID = "run-id"
	in.DataDir = dataDir
	return in
}

// TestRunBuildActivityMountsSnapshottedOracleReadOnlyAndRecordsHash is the
// end-to-end proof, through the real runSandboxWithRetries against a stub
// docker, of the three claims the in-loop mechanism depends on: the mount
// is read-only and points at a SNAPSHOT (never the live operator
// directory), the recorded hash describes what was mounted even if the
// live directory is edited while the container runs (the TOCTOU gap), and
// the snapshot does not outlive the Activity.
func TestRunBuildActivityMountsSnapshottedOracleReadOnlyAndRecordsHash(t *testing.T) {
	oracleDir, wantHash := oracleFixture(t)
	argvLog := filepath.Join(t.TempDir(), "argv.log")
	docker := stubDocker(t, argvLog, "echo tampered >> "+filepath.Join(oracleDir, "oracle_test.go"))
	activities := &Activities{LogDir: t.TempDir(), SandboxDocker: docker, DataDir: t.TempDir()}
	in := sandboxedBuildInput(t, docker, activities.DataDir)
	in.ReferenceOracleInLoopRetry = true
	in.ReferenceOracleDir = oracleDir
	in.ReferenceOracleMountPath = "verify"
	setGateCommand(&in, policy.ReferenceOracleGateID, "go test ./verify/...")

	result := executeBuildActivity(t, activities, in)

	// Non-vacuity: prove the stub actually edited the LIVE directory during
	// the run, or the hash assertion below would pass for the wrong reason.
	live, err := os.ReadFile(filepath.Join(oracleDir, "oracle_test.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(live), "tampered") {
		t.Fatalf("stub docker never mutated the live oracle directory (content %q) -- this test is not exercising the TOCTOU window", live)
	}
	raw, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatal(err)
	}
	argv := string(raw)
	snapshotDir, _ := filepath.Abs(filepath.Join(activities.LogDir, "reference-oracle-build-snapshot"))
	if !strings.Contains(argv, snapshotDir+":/workspace/verify:ro") {
		t.Errorf("docker argv does not mount the snapshot read-only at /workspace/verify (want %s:/workspace/verify:ro):\n%s", snapshotDir, argv)
	}
	if strings.Contains(argv, oracleDir+":") {
		t.Errorf("docker argv mounts the LIVE oracle directory %s instead of a snapshot:\n%s", oracleDir, argv)
	}
	if !strings.Contains(argv, "--reference-oracle-command") || !strings.Contains(argv, "go test ./verify/...") {
		t.Errorf("docker argv does not carry --reference-oracle-command to build_app.py:\n%s", argv)
	}
	if len(result.Attempts) == 0 {
		t.Fatal("no build attempts recorded")
	}
	for _, a := range result.Attempts {
		if a.ReferenceOracleSHA256 != wantHash {
			t.Errorf("Attempt.ReferenceOracleSHA256 = %q, want the hash of the content as it was BEFORE the live directory was edited mid-run (%q)", a.ReferenceOracleSHA256, wantHash)
		}
	}
	if _, statErr := os.Stat(snapshotDir); !os.IsNotExist(statErr) {
		t.Errorf("build snapshot %s survived the Activity: stat err = %v", snapshotDir, statErr)
	}
}

// TestRunBuildActivityMountsNothingWithoutInLoopOptIn is the converse, and
// the backward-compatibility guarantee: an operator who configured the
// oracle trio solely for the post-build gate sees no mount, no hash, and no
// forwarded command in the build.
func TestRunBuildActivityMountsNothingWithoutInLoopOptIn(t *testing.T) {
	oracleDir, _ := oracleFixture(t)
	argvLog := filepath.Join(t.TempDir(), "argv.log")
	docker := stubDocker(t, argvLog, "")
	activities := &Activities{LogDir: t.TempDir(), SandboxDocker: docker, DataDir: t.TempDir()}
	in := sandboxedBuildInput(t, docker, activities.DataDir)
	in.ReferenceOracleDir = oracleDir
	in.ReferenceOracleMountPath = "verify"
	setGateCommand(&in, policy.ReferenceOracleGateID, "go test ./verify/...")

	result := executeBuildActivity(t, activities, in)

	raw, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatal(err)
	}
	argv := string(raw)
	for _, forbidden := range []string{"/workspace/verify", "--reference-oracle-command", oracleDir} {
		if strings.Contains(argv, forbidden) {
			t.Errorf("docker argv contains %q without ReferenceOracleInLoopRetry:\n%s", forbidden, argv)
		}
	}
	for _, a := range result.Attempts {
		if a.ReferenceOracleSHA256 != "" {
			t.Errorf("Attempt.ReferenceOracleSHA256 = %q, want empty without the opt-in", a.ReferenceOracleSHA256)
		}
	}
}

func containsArgPair(args []string, flag, value string) bool {
	for i, a := range args {
		if a == flag && i+1 < len(args) && args[i+1] == value {
			return true
		}
	}
	return false
}

// TestRunBuildActivityFailsClosedWhenOracleDirIsUnreachableOnTheWorker pins
// the deliberate fail-closed choice (raised in review): an oracle directory
// that does not exist on THIS Worker's filesystem -- a Worker on another
// host, a container without the data dir mounted -- must fail the build
// Activity before build_app.py ever launches, never silently run the build
// without the in-loop check the operator explicitly approved and opted
// into. The same reachability constraint -WorkspacePath itself already has.
func TestRunBuildActivityFailsClosedWhenOracleDirIsUnreachableOnTheWorker(t *testing.T) {
	in := fixtureInput()
	in.WorkspacePath = t.TempDir()
	in.ReferenceOracleInLoopRetry = true
	in.ReferenceOracleDir = filepath.Join(t.TempDir(), "not-on-this-worker")
	in.ReferenceOracleMountPath = "verify"
	setGateCommand(&in, policy.ReferenceOracleGateID, "true")

	launched := false
	activities := &Activities{
		LogDir: t.TempDir(),
		runWithRetries: func(_ context.Context, _ string, _ func(int) string, _ int, _ func(int, runner.Result, error), _ string, _ ...string) (runner.Result, error) {
			launched = true
			return runner.Result{}, nil
		},
	}
	wrapper := func(ctx context.Context, in RunWorkflowInput) (BuildActivityResult, error) {
		return activities.RunBuildActivity(ctx, in)
	}
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(wrapper)
	_, err := env.ExecuteActivity(wrapper, in)
	if err == nil || !strings.Contains(err.Error(), "is not accessible on this host") {
		t.Fatalf("error = %v, want a fail-closed error naming the unreachable oracle directory", err)
	}
	if launched {
		t.Fatal("build_app.py was launched despite the oracle directory being unreachable")
	}
}
