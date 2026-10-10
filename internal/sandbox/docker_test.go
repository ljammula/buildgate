package sandbox

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"buildgate/internal/run"
)

func validSpec() LaunchSpec {
	return LaunchSpec{
		Image:    "factory-worker:test@sha256:deadbeef",
		WorkDir:  "/tmp/workspace",
		InputDir: "/tmp/inputs",
		LogPath:  "/tmp/logs/worker.log",
		Name:     "factory-worker-run1",
		User:     "65532:65532",
		Command:  []string{"/bin/sh", "-c", "echo ok"},
		Memory:   "512m", CPUs: "1", TmpfsSize: "64m", Timeout: time.Minute,
		Network: "none",
		RunID:   "run1",
		DataDir: "/tmp/data",
	}
}

func TestLaunchSpecRejectsUnsafeConfiguration(t *testing.T) {
	checks := []struct {
		name string
		edit func(*LaunchSpec)
	}{
		{"missing image", func(s *LaunchSpec) { s.Image = "" }},
		{"non-digest tag", func(s *LaunchSpec) { s.Image = "factory-worker:test" }},
		{"bare image name", func(s *LaunchSpec) { s.Image = "factory-worker" }},
		{"missing run id", func(s *LaunchSpec) { s.RunID = "" }},
		{"run id with newline", func(s *LaunchSpec) { s.RunID = "run\n1" }},
		{"run id with tab", func(s *LaunchSpec) { s.RunID = "run\t1" }},
		{"missing data dir", func(s *LaunchSpec) { s.DataDir = "" }},
		{"relative data dir", func(s *LaunchSpec) { s.DataDir = "data" }},
		{"relative workspace", func(s *LaunchSpec) { s.WorkDir = "workspace" }},
		{"host network", func(s *LaunchSpec) { s.Network = "host" }},
		{"arbitrary network", func(s *LaunchSpec) { s.Network = "bridge" }},
		{"named root user", func(s *LaunchSpec) { s.User = "root" }},
		{"root uid", func(s *LaunchSpec) { s.User = "0:0" }},
		{"non-numeric gid", func(s *LaunchSpec) { s.User = "1000:abc" }},
		{
			name: "control token",
			edit: func(s *LaunchSpec) { s.Environment = []string{"FACTORYD_API_START_TOKEN=secret"} },
		},
		{
			name: "real Anthropic key",
			edit: func(s *LaunchSpec) { s.Environment = []string{"ANTHROPIC_API_KEY=sk-ant-real-secret"} },
		},
		{
			name: "Git config override",
			edit: func(s *LaunchSpec) { s.Environment = []string{"GIT_CONFIG_COUNT=0"} },
		},
		{"malformed environment", func(s *LaunchSpec) { s.Environment = []string{"PATH"} }},
		// The relay's own egress network satisfies the "factoryd-relay-"
		// prefix but is a real route to the internet (found via review) —
		// a worker spec must never be allowed to name it directly.
		{"relay egress network", func(s *LaunchSpec) { s.Network = relayEgressNetworkName }},
		// The following names round out the forbidden-credential set beyond
		// what was already covered above (found via review): a denylist is
		// the wrong shape for a check whose whole job is catching the case
		// where someone stopped being careful, so IsForbiddenCredentialEnvKey
		// is exercised directly, not just through this table.
		{"GitHub token", func(s *LaunchSpec) { s.Environment = []string{"GITHUB_TOKEN=ghp_secret"} }},
		{"gh CLI token", func(s *LaunchSpec) { s.Environment = []string{"GH_TOKEN=ghp_secret"} }},
		{"GitLab token", func(s *LaunchSpec) { s.Environment = []string{"GITLAB_TOKEN=glpat-secret"} }},
		{"npm token", func(s *LaunchSpec) { s.Environment = []string{"NPM_TOKEN=npm_secret"} }},
		{"Hugging Face token", func(s *LaunchSpec) { s.Environment = []string{"HF_TOKEN=hf_secret"} }},
		{"Anthropic auth token", func(s *LaunchSpec) { s.Environment = []string{"ANTHROPIC_AUTH_TOKEN=secret"} }},
		{"kubeconfig", func(s *LaunchSpec) { s.Environment = []string{"KUBECONFIG=/home/x/.kube/config"} }},
		{"gcloud config", func(s *LaunchSpec) { s.Environment = []string{"CLOUDSDK_CORE_ACCOUNT=x"} }},
		// GIT_CONFIG_GLOBAL/SYSTEM are deliberately absent from
		// IsForbiddenCredentialEnvKey's own shared list (internal/runner's
		// host-worker-env-allow allowlist needs to admit them for this
		// repo's own test-isolation fixtures) -- found via code review:
		// that narrowing silently widened what a SANDBOXED worker's own
		// Environment would accept too, unless Validate rejects these two
		// names on top of the shared list specifically.
		{"git global config redirect", func(s *LaunchSpec) { s.Environment = []string{"GIT_CONFIG_GLOBAL=/workspace/malicious-gitconfig"} }},
		{"git system config redirect", func(s *LaunchSpec) { s.Environment = []string{"GIT_CONFIG_SYSTEM=/workspace/malicious-gitconfig"} }},
		// WorkerUmask is interpolated straight into a shell -c argument
		// (DockerCommand), not passed as a discrete argv element like
		// every other field here -- so it gets its own, narrower
		// character-class validation rather than reusing an existing
		// helper.
		{"umask with shell metacharacter", func(s *LaunchSpec) { s.WorkerUmask = "002; rm -rf /" }},
		{"umask with too many digits", func(s *LaunchSpec) { s.WorkerUmask = "00002" }},
		{"non-octal umask digit", func(s *LaunchSpec) { s.WorkerUmask = "0089" }},
		// ComposeNetwork must be composeServicesNetworkName's own
		// "bg-compose-<runID>" shape -- an unchecked value here would let a
		// caller attach the worker to any network on the host via `docker
		// network connect`, the same class of gap relayEgressNetworkName's
		// own rejection above closes for the primary Network field.
		{"unlisted compose network", func(s *LaunchSpec) { s.ComposeNetwork = "bridge" }},
		// ReferenceOracleDir/ReferenceOracleMountPath validation, added
		// alongside the reference_oracle gate mount itself (GitHub Codex
		// App review, PR #151): the same absolute-path and
		// set-together-or-neither rules InputDir already enforces above,
		// plus the mount path itself must be relative (it's joined onto
		// workerContainerWorkDir, an absolute path already).
		{"relative reference-oracle dir", func(s *LaunchSpec) { s.ReferenceOracleDir = "oracle" }},
		{"reference-oracle dir without mount path", func(s *LaunchSpec) { s.ReferenceOracleDir = "/oracle" }},
		{"reference-oracle mount path without dir", func(s *LaunchSpec) { s.ReferenceOracleMountPath = "verify" }},
		// Containment check added in round 2 of the same review: a
		// source equal to or beneath WorkDir is exactly as tamperable
		// as the Allowed-Files-only convention this field exists to
		// improve on, since the build worker has full read-write
		// access to WorkDir already.
		{"reference-oracle dir equal to workspace", func(s *LaunchSpec) {
			s.ReferenceOracleDir = s.WorkDir
			s.ReferenceOracleMountPath = "verify"
		}},
		{"reference-oracle dir inside workspace", func(s *LaunchSpec) {
			s.ReferenceOracleDir = s.WorkDir + "/verify"
			s.ReferenceOracleMountPath = "verify"
		}},
		{"absolute reference-oracle mount path", func(s *LaunchSpec) {
			s.ReferenceOracleDir = "/oracle"
			s.ReferenceOracleMountPath = "/verify"
		}},
	}
	for _, tc := range checks {
		t.Run(tc.name, func(t *testing.T) {
			s := validSpec()
			tc.edit(&s)
			if err := s.Validate(); err == nil {
				t.Fatal("Validate unexpectedly succeeded")
			}
		})
	}
}

// TestLaunchSpecAllowsNonRootUIDWithRootGID is the regression test for a
// real GitHub Codex App review finding on PR #75 (2026-09-09): that PR
// fixed cmd/factoryd's own validateDefaultSandboxIdentity preflight to stop
// rejecting a non-root UID paired with GID 0 (GID 0 is just the root
// *group* and confers no special privilege by itself), but validateUser --
// the check LaunchSpec.Validate actually calls before every real sandbox
// launch -- carried the identical UID/GID conflation independently, so the
// same legitimate identity still failed here, just later (after the run
// record and isolated worktree already existed). This table's own "root
// gid" case used to assert the opposite (that "1000:0" must be rejected).
func TestLaunchSpecAllowsNonRootUIDWithRootGID(t *testing.T) {
	s := validSpec()
	s.User = "1000:0"
	if err := s.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil -- UID 1000 is not root regardless of GID", err)
	}
}

// TestLaunchSpecAcceptsOnlyTheAnthropicPlaceholderKey guards the relay
// lifecycle's safety property at the Validate layer, independent of the
// orchestrator: only the exact relay placeholder may reach a worker as
// ANTHROPIC_API_KEY, so a caller bypassing the orchestrator still cannot
// smuggle a real credential through Run.
func TestLaunchSpecAcceptsOnlyTheAnthropicPlaceholderKey(t *testing.T) {
	s := validSpec()
	s.Environment = []string{"ANTHROPIC_API_KEY=" + AnthropicAPIKeyPlaceholder}
	if err := s.Validate(); err != nil {
		t.Fatalf("Validate rejected the relay placeholder: %v", err)
	}
}

// TestLaunchSpecAcceptsRunIDsFactorydAlreadyGenerates is the regression for
// the codex finding that Validate's earlier "/ \:" run-id restriction
// rejected run IDs the rest of factoryd already accepts and reports as
// started — ticket-derived IDs and the API starter's caller-supplied
// -run-id routinely contain spaces and colons.
func TestLaunchSpecAcceptsRunIDsFactorydAlreadyGenerates(t *testing.T) {
	for _, runID := range []string{
		"001-full-app: v2 20260829-150625-12965",
		"run 1",
		"a/b:c",
	} {
		s := validSpec()
		s.RunID = runID
		if err := s.Validate(); err != nil {
			t.Errorf("Validate() with RunID %q = %v, want nil", runID, err)
		}
	}
}

// TestLaunchSpecAcceptsAnAllowListedComposeNetwork is the positive
// counterpart to the "unlisted compose network" rejection case above.
func TestLaunchSpecAcceptsAnAllowListedComposeNetwork(t *testing.T) {
	s := validSpec()
	s.ComposeNetwork = "bg-compose-run1"
	if err := s.Validate(); err != nil {
		t.Fatalf("Validate rejected an allow-listed compose network: %v", err)
	}
}

// TestDockerCreateCommandMirrorsDockerCommandExceptTheVerb pins the one
// difference dockerCreateCommand may ever have from DockerCommand: "create"
// in place of "run" as the very first argument, with every other flag
// identical -- the create/connect/start sequence in Run only makes sense if
// the container Run's own foreground `docker start -a` later attaches to
// was created with the exact same flags (network, mounts, resource limits,
// environment) an ordinary `docker run` would have used.
func TestDockerCreateCommandMirrorsDockerCommandExceptTheVerb(t *testing.T) {
	s := validSpec()
	s.ComposeNetwork = "bg-compose-run1"
	runArgs := s.DockerCommand("docker")
	createArgs := s.dockerCreateCommand("docker")
	if len(runArgs) != len(createArgs) {
		t.Fatalf("dockerCreateCommand has %d args, DockerCommand has %d; want the same length", len(createArgs), len(runArgs))
	}
	if runArgs[1] != "run" || createArgs[1] != "create" {
		t.Fatalf("verb mismatch: DockerCommand[1] = %q, dockerCreateCommand[1] = %q", runArgs[1], createArgs[1])
	}
	for i := 2; i < len(runArgs); i++ {
		if runArgs[i] != createArgs[i] {
			t.Fatalf("argument %d differs: DockerCommand = %q, dockerCreateCommand = %q", i, runArgs[i], createArgs[i])
		}
	}
}

func TestDockerCommandUsesRestrictedProfile(t *testing.T) {
	args := validSpec().DockerCommand("docker")
	joined := " " + strings.Join(args, " ") + " "
	for _, want := range []string{" --read-only ", " --user 65532:65532 ", " --cap-drop=ALL ", " --security-opt=no-new-privileges ", " --network none ", " --volume /tmp/workspace:/workspace:rw ", " --volume /tmp/inputs:/inputs:ro "} {
		if !strings.Contains(joined, want) {
			t.Errorf("Docker args missing %q: %v", want, args)
		}
	}
	if strings.Contains(joined, "docker.sock") || strings.Contains(joined, " --privileged ") {
		t.Fatalf("Docker args expose unsafe control: %v", args)
	}
}

// TestDockerCommandMountsReferenceOracleReadOnly is the regression test
// for the reference_oracle gate's read-only overlay mount (GitHub Codex
// App review, PR #151): when ReferenceOracleDir/ReferenceOracleMountPath
// are set, DockerCommand must bind-mount that host directory read-only
// at the given path INSIDE /workspace -- nested over the already-rw
// workspace mount above it, the same technique already used for
// /workspace/.git:ro -- so the reference oracle is never writable by the
// worker, regardless of what -workspace itself allows.
func TestDockerCommandMountsReferenceOracleReadOnly(t *testing.T) {
	s := validSpec()
	s.ReferenceOracleDir = "/tmp/oracle"
	s.ReferenceOracleMountPath = "verify"
	args := s.DockerCommand("docker")
	joined := " " + strings.Join(args, " ") + " "
	if !strings.Contains(joined, " --volume /tmp/oracle:/workspace/verify:ro ") {
		t.Errorf("Docker args missing the read-only reference-oracle overlay mount: %v", args)
	}
}

// TestDockerCommandOmitsReferenceOracleMountWhenUnset confirms the
// converse: leaving both fields empty (the default -- an operator who
// never sets -reference-oracle-dir) produces no such mount at all,
// matching the plain, weaker Allowed-Files-only behavior this repo had
// before ReferenceOracleDir existed.
func TestDockerCommandOmitsReferenceOracleMountWhenUnset(t *testing.T) {
	args := validSpec().DockerCommand("docker")
	joined := " " + strings.Join(args, " ") + " "
	if strings.Contains(joined, "/workspace/verify:ro") || strings.Contains(joined, "oracle") {
		t.Errorf("Docker args unexpectedly mount a reference-oracle overlay with both fields unset: %v", args)
	}
}

// TestReferenceOracleSourceContained is the direct unit test for the
// helper extracted from LaunchSpec.Validate's own containment check
// (found via review, PR #153, round 2): a caller that snapshots
// ReferenceOracleDir before ever building a LaunchSpec -- both
// cmd/factoryd's runGate and RunNamedGateActivity do -- must apply this
// check to the ORIGINAL configured source itself, since Validate only
// ever sees the (always-safe-by-construction) snapshot destination by
// the time a LaunchSpec exists.
//
// Every non-empty case uses a real t.TempDir() tree, not a fake path
// string: the function now canonicalizes (Abs + EvalSymlinks) both
// inputs, which requires them to actually exist, and a fake path like
// "/workspace" would silently fail closed (report "contained") on every
// case regardless of the real answer -- exactly the false-positive this
// test must not paper over.
func TestReferenceOracleSourceContained(t *testing.T) {
	root := t.TempDir()
	workDir := filepath.Join(root, "workspace")
	beneath := filepath.Join(workDir, "verify")
	sibling := filepath.Join(root, "oracle-store")
	for _, dir := range []string{workDir, beneath, sibling} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}

	cases := []struct {
		name, workDir, dir string
		want               bool
	}{
		{"equal to workspace", workDir, workDir, true},
		{"beneath workspace", workDir, beneath, true},
		{"sibling of workspace", workDir, sibling, false},
		{"unrelated absolute path", workDir, os.TempDir(), false},
		{"empty workDir", "", sibling, false},
		{"empty dir", workDir, "", false},
		{"nonexistent dir fails closed as contained", workDir, filepath.Join(root, "does-not-exist"), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ReferenceOracleSourceContained(tc.workDir, tc.dir); got != tc.want {
				t.Errorf("ReferenceOracleSourceContained(%q, %q) = %v, want %v", tc.workDir, tc.dir, got, tc.want)
			}
		})
	}
}

// TestReferenceOracleSourceContainedCanonicalizesRelativePaths is the
// regression test for the first Codex round-3 finding on PR #153: a
// relative -reference-oracle-dir compared against an absolute workDir
// used to make filepath.Rel return a mixed-absolute/relative error,
// which the old implementation read as "not contained" -- the unsafe
// default -- rather than resolving the relative path first.
func TestReferenceOracleSourceContainedCanonicalizesRelativePaths(t *testing.T) {
	root := t.TempDir()
	workDir := filepath.Join(root, "workspace")
	insideRel := filepath.Join(workDir, "oracle")
	for _, dir := range []string{workDir, insideRel} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}

	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(workDir); err != nil {
		t.Fatalf("chdir %s: %v", workDir, err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(oldWD); err != nil {
			t.Fatalf("restore cwd %s: %v", oldWD, err)
		}
	})

	if !ReferenceOracleSourceContained(workDir, "oracle") {
		t.Error("ReferenceOracleSourceContained with a relative dir beneath workDir = false, want true")
	}
}

// TestReferenceOracleSourceContainedResolvesSymlinkedAncestors is the
// regression test for the second Codex round-3 finding on PR #153: an
// absolute -reference-oracle-dir whose real location is inside workDir
// only via a symlinked ancestor used to pass the purely lexical
// comparison, since the symlink's own target was never resolved.
func TestReferenceOracleSourceContainedResolvesSymlinkedAncestors(t *testing.T) {
	root := t.TempDir()
	workDir := filepath.Join(root, "workspace")
	realOracleParent := filepath.Join(workDir, "real-oracle-parent")
	if err := os.MkdirAll(realOracleParent, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", realOracleParent, err)
	}
	oracleDir := filepath.Join(realOracleParent, "oracle")
	if err := os.MkdirAll(oracleDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", oracleDir, err)
	}

	// symlinkAncestor lives OUTSIDE workDir and points at a directory
	// INSIDE it -- an operator-configured -reference-oracle-dir reached
	// through symlinkAncestor looks like a sibling of workDir lexically,
	// but its real, EvalSymlinks-resolved location is beneath workDir.
	symlinkAncestor := filepath.Join(root, "symlinked-ancestor")
	if err := os.Symlink(realOracleParent, symlinkAncestor); err != nil {
		t.Fatalf("symlink %s -> %s: %v", symlinkAncestor, realOracleParent, err)
	}
	dirThroughSymlink := filepath.Join(symlinkAncestor, "oracle")

	if !ReferenceOracleSourceContained(workDir, dirThroughSymlink) {
		t.Error("ReferenceOracleSourceContained through a symlinked ancestor resolving inside workDir = false, want true")
	}
}

// TestReferenceOracleSourceContainedRejectsWorkspaceOwnedSymlinkEvenWhenItResolvesOutside
// is the regression test for the GitHub Codex App's round-5 finding on
// PR #154: a canonical-only check (the fix for the round-3 finding
// above) accepts a configured "-reference-oracle-dir $workDir/link/oracle"
// once "link" is a symlink to some external directory, since the fully
// resolved path is no longer beneath workDir. But "link" itself is a
// small file living INSIDE the writable workspace -- the worker can
// rewrite its target during build (this gate runs post-build), letting
// it choose whatever "external" location becomes the acceptance oracle
// despite the operator's own original configuration. The raw, lexical
// path must be rejected outright regardless of where the symlink
// currently resolves.
func TestReferenceOracleSourceContainedRejectsWorkspaceOwnedSymlinkEvenWhenItResolvesOutside(t *testing.T) {
	root := t.TempDir()
	workDir := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", workDir, err)
	}
	externalTarget := filepath.Join(root, "external-oracle-parent")
	externalOracle := filepath.Join(externalTarget, "oracle")
	if err := os.MkdirAll(externalOracle, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", externalOracle, err)
	}

	// link lives INSIDE workDir (worker-writable) and points at a
	// directory OUTSIDE it -- a fully-resolved-only check would treat
	// dirThroughWorkspaceOwnedLink as safely external.
	link := filepath.Join(workDir, "link")
	if err := os.Symlink(externalTarget, link); err != nil {
		t.Fatalf("symlink %s -> %s: %v", link, externalTarget, err)
	}
	dirThroughWorkspaceOwnedLink := filepath.Join(link, "oracle")

	if !ReferenceOracleSourceContained(workDir, dirThroughWorkspaceOwnedLink) {
		t.Error("ReferenceOracleSourceContained through a workspace-owned symlink resolving outside workDir = false, want true (the symlink itself is worker-writable)")
	}
}

// TestDockerCommandWrapsCommandWithUmaskWhenSet is the regression test for
// the worker-UID-separation mechanism: DockerCommand must wrap the real
// command in a shell prelude
// that sets the container's umask before exec'ing it -- otherwise a
// dedicated worker UID's own newly-created files/directories keep
// whatever the image's baked-in default umask grants, which typically
// denies group-write, defeating the entire point of a shared-group
// permission scheme.
func TestDockerCommandWrapsCommandWithUmaskWhenSet(t *testing.T) {
	s := validSpec()
	s.WorkerUmask = "0002"
	args := s.DockerCommand("docker")
	joined := " " + strings.Join(args, " ") + " "
	const wantScript = `umask 0002; "$@"; ec=$?; chmod -R g+rwX -- '/workspace' 2>/dev/null || true; exit $ec`
	if !strings.Contains(joined, " /bin/sh -c "+wantScript+` -- /bin/sh -c echo ok `) {
		t.Fatalf("Docker args missing umask-wrapped command: %v", args)
	}
}

// TestDockerCommandReclaimsContainerWorkDirRegardlessOfHostWorkDir is the
// regression test for two rounds of the same P1 found via GitHub Codex App
// review of PR #62. Round one: a worker that chmods one of its own created
// paths to something restrictive (0700, say) would otherwise permanently
// strand it from factoryd's own later, differently-UID'd git/rollback code,
// since only the owning UID can always chmod a path back regardless of its
// current mode -- fixed by having the wrapped script run the real command
// (not exec it) and reclaim group-writability once it exits. Round two:
// the first version of that fix reclaimed s.WorkDir -- a HOST path -- but
// the script runs *inside* the container, which only ever sees that host
// path bind-mounted at the fixed container path "/workspace"; s.WorkDir
// itself does not exist inside the container at all, so the reclaim was a
// silent no-op on every real run. This test sets s.WorkDir to something
// that would make that bug obvious (neither "/workspace" nor a value
// shSingleQuote would leave unescaped) and asserts the reclaim always
// targets the fixed container path, never s.WorkDir verbatim.
func TestDockerCommandReclaimsContainerWorkDirRegardlessOfHostWorkDir(t *testing.T) {
	s := validSpec()
	s.WorkerUmask = "0002"
	s.WorkDir = "/host/it's/weird"
	args := s.DockerCommand("docker")
	joined := " " + strings.Join(args, " ") + " "
	const wantScript = `umask 0002; "$@"; ec=$?; chmod -R g+rwX -- '/workspace' 2>/dev/null || true; exit $ec`
	if !strings.Contains(joined, " /bin/sh -c "+wantScript+" ") {
		t.Fatalf("Docker args missing reclaim-chmod wrapped command targeting the container path: %v", args)
	}
	if strings.Contains(joined, "workspace' || true; exit $ec") && strings.Contains(joined, s.WorkDir) {
		// s.WorkDir legitimately appears once, in --volume's HOST:CONTAINER
		// mapping -- but never as the chmod's own argument.
		if strings.Contains(joined, `chmod -R g+rwX -- '`+s.WorkDir) {
			t.Fatalf("Docker args reclaim the HOST path %q instead of the container path: %v", s.WorkDir, args)
		}
	}
	if !strings.Contains(joined, "--volume "+s.WorkDir+":/workspace:rw") {
		t.Fatalf("Docker args missing the expected HOST:CONTAINER volume mapping: %v", args)
	}
}

func TestShSingleQuote(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/workspace", "'/workspace'"},
		{"", "''"},
		{"it's", `'it'"'"'s'`},
		{"''", `''"'"''"'"''`},
	}
	for _, c := range cases {
		if got := shSingleQuote(c.in); got != c.want {
			t.Errorf("shSingleQuote(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestDockerCommandLeavesCommandUnwrappedWithoutUmask covers the default,
// pre-existing behavior (an operator-set -sandbox-user, or the
// -allow-unsandboxed host path's own equivalent, both leave WorkerUmask
// unset): DockerCommand must not silently change the exact argv Docker
// execs for any configuration that never opted into worker UID
// separation.
func TestDockerCommandLeavesCommandUnwrappedWithoutUmask(t *testing.T) {
	s := validSpec()
	args := s.DockerCommand("docker")
	joined := " " + strings.Join(args, " ") + " "
	if strings.Contains(joined, "umask") {
		t.Fatalf("Docker args unexpectedly wrapped the command with a umask prelude: %v", args)
	}
	if !strings.HasSuffix(joined, " /bin/sh -c echo ok ") {
		t.Fatalf("Docker args missing the plain, unwrapped command: %v", args)
	}
}

func TestValidateWorkerUIDRejectsRootAndHostUID(t *testing.T) {
	if err := ValidateWorkerUID(0, 1000); err == nil {
		t.Error("ValidateWorkerUID(0, 1000) unexpectedly succeeded")
	}
	if err := ValidateWorkerUID(-1, 1000); err == nil {
		t.Error("ValidateWorkerUID(-1, 1000) unexpectedly succeeded")
	}
	if err := ValidateWorkerUID(1000, 1000); err == nil {
		t.Error("ValidateWorkerUID(1000, 1000) (same as host UID) unexpectedly succeeded")
	}
	if err := ValidateWorkerUID(DefaultWorkerUID, 1000); err != nil {
		t.Errorf("ValidateWorkerUID(DefaultWorkerUID, 1000) = %v, want nil", err)
	}
}

// TestValidateWorkerUIDRejectsOutOfRangeUID guards a real gap found via
// GitHub Codex App review of PR #62 (P2): on a 64-bit host, a configured
// worker UID above 2^31-1 passed this startup validation only for the
// resolved --user value to be rejected later by validateUser, which parses
// UID components with strconv.ParseUint(..., 31) -- after the daemon had
// already started, or a run had already created its durable record and
// isolated worktree, on configuration this function had declared valid.
func TestValidateWorkerUIDRejectsOutOfRangeUID(t *testing.T) {
	if err := ValidateWorkerUID(maxSandboxUID, 1000); err != nil {
		t.Errorf("ValidateWorkerUID(maxSandboxUID, 1000) = %v, want nil", err)
	}
	if err := ValidateWorkerUID(maxSandboxUID+1, 1000); err == nil {
		t.Error("ValidateWorkerUID(maxSandboxUID+1, 1000) unexpectedly succeeded")
	}
	if err := ValidateWorkerUID(1<<32, 1000); err == nil {
		t.Error("ValidateWorkerUID(1<<32, 1000) unexpectedly succeeded")
	}
}

// TestResolveDefaultWorkerIdentity guards the shared helper cmd/factoryd's
// own runSandboxWithRetries and internal/workflow.Activities' equivalent
// both now call, instead of each duplicating this exact resolution (found
// via code review).
func TestResolveDefaultWorkerIdentity(t *testing.T) {
	if user, umask := ResolveDefaultWorkerIdentity("", 65532, 1000); user != "65532:1000" || umask != DefaultWorkerUmask {
		t.Errorf(`ResolveDefaultWorkerIdentity("", 65532, 1000) = (%q, %q), want ("65532:1000", %q)`, user, umask, DefaultWorkerUmask)
	}
	if user, umask := ResolveDefaultWorkerIdentity("1000:1000", 65532, 1000); user != "1000:1000" || umask != "" {
		t.Errorf(`ResolveDefaultWorkerIdentity("1000:1000", 65532, 1000) = (%q, %q), want ("1000:1000", "") -- an explicit user must pass through unchanged with no umask forced`, user, umask)
	}
}

func TestRunMountsLinkedWorktreeGitMetadataReadOnly(t *testing.T) {
	repo := t.TempDir()
	for _, args := range [][]string{{"init"}, {"config", "user.email", "test@example.com"}, {"config", "user.name", "Test"}} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", repo, "add", ".").CombinedOutput(); err != nil {
		t.Fatalf("git add: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "-C", repo, "commit", "-m", "fixture").CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v: %s", err, out)
	}
	worktree := filepath.Join(t.TempDir(), "worktree")
	if out, err := exec.Command("git", "-C", repo, "worktree", "add", "-b", "fixture-worktree", worktree).CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %v: %s", err, out)
	}

	argsPath := filepath.Join(t.TempDir(), "args")
	docker := filepath.Join(t.TempDir(), "docker-fake")
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$@\" > %q\nexit 0\n", argsPath)
	if err := os.WriteFile(docker, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	spec := validSpec()
	spec.WorkDir = worktree
	spec.InputDir = ""
	spec.LogPath = filepath.Join(t.TempDir(), "worker.log")
	spec.DataDir = t.TempDir()
	if _, err := Run(context.Background(), docker, spec); err != nil {
		t.Fatalf("Run: %v", err)
	}
	commonDir, target, err := gitCommonDir(worktree)
	if err != nil {
		t.Fatal(err)
	}
	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	want := commonDir + ":" + target + ":ro"
	if !strings.Contains(string(args), want) {
		t.Fatalf("docker args missing read-only Git common-dir mount %q: %s", want, args)
	}
}

func TestStageFileDoesNotExposeParentDirectory(t *testing.T) {
	parent := t.TempDir()
	sourceDir := filepath.Join(parent, "source")
	if err := os.Mkdir(sourceDir, 0o700); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(sourceDir, "build.py")
	if err := os.WriteFile(source, []byte("print('ok')"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir, staged, cleanup, err := StageFile(source, parent)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if dir == sourceDir || filepath.Dir(staged) != dir {
		t.Fatalf("staged file escaped private directory: dir=%q staged=%q", dir, staged)
	}
	if _, err := os.Stat(filepath.Join(dir, "source")); !os.IsNotExist(err) {
		t.Fatalf("staged directory unexpectedly contains source parent: %v", err)
	}
}

// TestStageFilePermitsNonRootContainerUserToRead is the regression for the
// staged-input-permissions gap found via review: the container
// runs as a non-root numeric UID (Validate rejects anything else) that is
// never the same UID staging this file on the host, so the staged
// directory/file must be traversable/readable by *any* UID, not just the
// host user that created them, or every sandboxed attempt fails before its
// command even starts.
func TestStageFilePermitsNonRootContainerUserToRead(t *testing.T) {
	parent := t.TempDir()
	source := filepath.Join(parent, "source", "build.py")
	if err := os.MkdirAll(filepath.Dir(source), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("print('ok')"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir, staged, cleanup, err := StageFile(source, parent)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := dirInfo.Mode().Perm(); perm&0o755 != 0o755 {
		t.Errorf("staged directory mode = %o, want at least 0755 (world-traversable)", perm)
	}
	fileInfo, err := os.Stat(staged)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fileInfo.Mode().Perm(); perm&0o644 != 0o644 {
		t.Errorf("staged file mode = %o, want at least 0644 (world-readable)", perm)
	}
}

// TestScriptsSHA256StableForIdenticalScriptsDifferentWhenSiblingChanges is
// the regression for M4-K1's harness-provenance requirement: two staged
// directories with byte-identical script+sibling content must hash
// identically (so the same harness build always shows the same digest),
// and changing a sibling module's own content -- not just the script
// itself -- must change the digest (the digest is evidence of everything
// that ran, not only the one file StageFile staged).
func TestScriptsSHA256StableForIdenticalScriptsDifferentWhenSiblingChanges(t *testing.T) {
	newStagedDir := func(t *testing.T, scriptContent, siblingContent string) string {
		t.Helper()
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "build_app.py"), []byte(scriptContent), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "draft_spec.py"), []byte(siblingContent), 0o644); err != nil {
			t.Fatal(err)
		}
		return dir
	}

	dirA := newStagedDir(t, "print('build')", "print('draft')")
	dirB := newStagedDir(t, "print('build')", "print('draft')")
	dirC := newStagedDir(t, "print('build')", "print('draft changed')")

	hashA, err := ScriptsSHA256(dirA)
	if err != nil {
		t.Fatalf("ScriptsSHA256(dirA): %v", err)
	}
	hashB, err := ScriptsSHA256(dirB)
	if err != nil {
		t.Fatalf("ScriptsSHA256(dirB): %v", err)
	}
	hashC, err := ScriptsSHA256(dirC)
	if err != nil {
		t.Fatalf("ScriptsSHA256(dirC): %v", err)
	}

	if hashA == "" {
		t.Fatal("ScriptsSHA256 = \"\", want a non-empty digest")
	}
	if hashA != hashB {
		t.Errorf("ScriptsSHA256(dirA) = %q, ScriptsSHA256(dirB) = %q, want equal for identical staged content", hashA, hashB)
	}
	if hashA == hashC {
		t.Errorf("ScriptsSHA256(dirA) == ScriptsSHA256(dirC) = %q, want different digests when a sibling's content differs", hashA)
	}
}

// TestRunRecordsImageDigest is the regression for the "record the
// resolved digest per run" gap found via review: Result must carry the
// digest already embedded in the (now digest-pinned-only) launch image.
func TestRunRecordsImageDigest(t *testing.T) {
	workspace := t.TempDir()
	docker := filepath.Join(t.TempDir(), "docker-fake")
	if err := os.WriteFile(docker, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	spec := validSpec()
	spec.WorkDir = workspace
	spec.InputDir = ""
	spec.LogPath = filepath.Join(t.TempDir(), "worker.log")
	spec.DataDir = t.TempDir()
	result, err := Run(context.Background(), docker, spec)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.ImageDigest != "sha256:deadbeef" {
		t.Errorf("ImageDigest = %q, want %q", result.ImageDigest, "sha256:deadbeef")
	}
	owner, err := os.ReadFile(ownerPIDPath(spec.DataDir, spec.RunID))
	if err != nil {
		t.Fatalf("read owner PID marker: %v", err)
	}
	if string(owner) != strconv.Itoa(os.Getpid()) {
		t.Errorf("owner PID marker = %q, want this process's own PID %d", owner, os.Getpid())
	}
}

// TestRunRemovesItsScratchDir: a container's scratch directory (Go build
// and module caches) is removed once it exits, so no later container of
// the run -- canonical verify in particular -- can reuse what it wrote.
func TestRunRemovesItsScratchDir(t *testing.T) {
	docker := filepath.Join(t.TempDir(), "docker-fake")
	if err := os.WriteFile(docker, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	spec := validSpec()
	spec.WorkDir = t.TempDir()
	spec.InputDir = ""
	spec.LogPath = filepath.Join(t.TempDir(), "worker.log")
	spec.DataDir = t.TempDir()
	scratch, err := EnsureScratchDir(spec.DataDir, spec.RunID, spec.Name, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scratch, "planted-cache-entry"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	spec.ScratchDir = scratch
	if _, err := Run(context.Background(), docker, spec); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := os.Stat(scratch); !os.IsNotExist(err) {
		t.Fatalf("scratch dir %s still exists after its container exited: %v", scratch, err)
	}
}

// TestRunPassesHostEnvironmentToDockerSubprocess is the regression for a
// "Docker client configuration" test gap found via review:
// dockerClientEnv() (cmd.Env = os.Environ()) is what lets the trusted
// host-side `docker` CLI itself pick up DOCKER_HOST/DOCKER_CONTEXT/TLS
// settings for a non-default Docker endpoint on Linux; this proves that
// environment genuinely reaches the docker subprocess Run launches, not
// just that dockerClientEnv() returns os.Environ() in isolation.
func TestRunPassesHostEnvironmentToDockerSubprocess(t *testing.T) {
	dumpPath := filepath.Join(t.TempDir(), "env-dump")
	docker := filepath.Join(t.TempDir(), "docker-fake")
	script := fmt.Sprintf("#!/bin/sh\nenv > %s\nexit 0\n", dumpPath)
	if err := os.WriteFile(docker, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SANDBOX_CLIENT_ENV_MARKER", "marker-value-12345")

	spec := validSpec()
	spec.WorkDir = t.TempDir()
	spec.InputDir = ""
	spec.LogPath = filepath.Join(t.TempDir(), "worker.log")
	spec.DataDir = t.TempDir()
	if _, err := Run(context.Background(), docker, spec); err != nil {
		t.Fatalf("Run: %v", err)
	}

	dump, err := os.ReadFile(dumpPath)
	if err != nil {
		t.Fatalf("read env dump: %v", err)
	}
	if !strings.Contains(string(dump), "SANDBOX_CLIENT_ENV_MARKER=marker-value-12345") {
		t.Fatalf("docker subprocess env missing host marker variable; dump = %q", dump)
	}
}

// TestRunWithComposeNetworkUsesCreateConnectStartSequence pins Run's own
// branch for ComposeNetwork != "": the ordinary single `docker run` never
// happens (the fake docker below records "run" as an argv error if it
// ever sees it), and instead a `docker create` (with --network still
// present for the primary Network), a `docker network connect
// <ComposeNetwork> <name>`, and a `docker start -a <name>` are issued, in
// that order.
func TestRunWithComposeNetworkUsesCreateConnectStartSequence(t *testing.T) {
	t.Setenv(SkipMountVisibilityCheckEnv, "1")
	callsPath := filepath.Join(t.TempDir(), "calls")
	docker := filepath.Join(t.TempDir(), "docker-fake")
	// Each invocation appends its own subcommand plus a "|"-joined argv
	// snapshot as one line, so assertions below can check both the call
	// order and each call's own arguments without a multi-line parse.
	script := "#!/bin/sh\n" +
		"sub=\"$1\"\n" +
		"line=\"$sub\"\n" +
		"shift\n" +
		"for a in \"$@\"; do line=\"$line|$a\"; done\n" +
		"printf '%s\\n' \"$line\" >> \"" + callsPath + "\"\n" +
		"exit 0\n"
	if err := os.WriteFile(docker, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	spec := validSpec()
	spec.WorkDir = t.TempDir()
	spec.InputDir = ""
	spec.LogPath = filepath.Join(t.TempDir(), "worker.log")
	spec.DataDir = t.TempDir()
	spec.Network = "factoryd-relay-run1"
	spec.ComposeNetwork = "bg-compose-run1"

	if _, err := Run(context.Background(), docker, spec); err != nil {
		t.Fatalf("Run: %v", err)
	}

	calls, err := os.ReadFile(callsPath)
	if err != nil {
		t.Fatalf("read fake docker calls: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(calls)), "\n")
	if len(lines) != 3 {
		t.Fatalf("docker calls = %v, want exactly 3 (create, network connect, start)", lines)
	}
	if !strings.HasPrefix(lines[0], "create|") {
		t.Errorf("first call = %q, want it to start with the create verb", lines[0])
	}
	if !strings.Contains(lines[0], "|--network|"+spec.Network+"|") {
		t.Errorf("create call = %q, want it to still carry the primary --network %q", lines[0], spec.Network)
	}
	if lines[1] != "network|connect|"+spec.ComposeNetwork+"|"+spec.Name {
		t.Errorf("second call = %q, want %q", lines[1], "network|connect|"+spec.ComposeNetwork+"|"+spec.Name)
	}
	if lines[2] != "start|-a|"+spec.Name {
		t.Errorf("third call = %q, want %q", lines[2], "start|-a|"+spec.Name)
	}
}

func TestRunWithComposeNetworkReplacesPrivateNoneNetwork(t *testing.T) {
	t.Setenv(SkipMountVisibilityCheckEnv, "1")
	callsPath := filepath.Join(t.TempDir(), "calls")
	docker := filepath.Join(t.TempDir(), "docker-fake")
	script := "#!/bin/sh\n" +
		"sub=\"$1\"\n" +
		"line=\"$sub\"\n" +
		"shift\n" +
		"for a in \"$@\"; do line=\"$line|$a\"; done\n" +
		"printf '%s\\n' \"$line\" >> \"" + callsPath + "\"\n" +
		"exit 0\n"
	if err := os.WriteFile(docker, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	spec := validSpec()
	spec.WorkDir = t.TempDir()
	spec.InputDir = ""
	spec.LogPath = filepath.Join(t.TempDir(), "worker.log")
	spec.DataDir = t.TempDir()
	spec.ComposeNetwork = "bg-compose-run1"

	if _, err := Run(context.Background(), docker, spec); err != nil {
		t.Fatalf("Run: %v", err)
	}

	calls, err := os.ReadFile(callsPath)
	if err != nil {
		t.Fatalf("read fake docker calls: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(calls)), "\n")
	if len(lines) != 1 {
		t.Fatalf("docker calls = %v, want exactly one direct run", lines)
	}
	if !strings.Contains(lines[0], "run|") {
		t.Fatalf("docker call = %q, want docker run", lines[0])
	}
	if !strings.Contains(lines[0], "|--network|"+spec.ComposeNetwork+"|") {
		t.Fatalf("docker call = %q, want the compose network as the primary network", lines[0])
	}
	if strings.Contains(lines[0], "network|connect") {
		t.Fatalf("docker call = %q, must not attach a second network to private none", lines[0])
	}
}

// TestRunWithoutComposeNetworkIsUnaffected is the negative counterpart:
// ComposeNetwork left empty (the overwhelming common case) must launch
// with the ordinary single `docker run`, completely unchanged from before
// ComposeNetwork existed.
func TestRunWithoutComposeNetworkIsUnaffected(t *testing.T) {
	t.Setenv(SkipMountVisibilityCheckEnv, "1")
	callsPath := filepath.Join(t.TempDir(), "calls")
	docker := filepath.Join(t.TempDir(), "docker-fake")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$1\" >> \"" + callsPath + "\"\n" +
		"exit 0\n"
	if err := os.WriteFile(docker, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	spec := validSpec()
	spec.WorkDir = t.TempDir()
	spec.InputDir = ""
	spec.LogPath = filepath.Join(t.TempDir(), "worker.log")
	spec.DataDir = t.TempDir()

	if _, err := Run(context.Background(), docker, spec); err != nil {
		t.Fatalf("Run: %v", err)
	}
	calls, err := os.ReadFile(callsPath)
	if err != nil {
		t.Fatalf("read fake docker calls: %v", err)
	}
	if got := strings.TrimSpace(string(calls)); got != "run" {
		t.Fatalf("docker verbs invoked = %q, want exactly one \"run\"", got)
	}
}

// TestRunWithComposeNetworkCleansUpOnConnectFailure: a failed `docker
// network connect` must not leave the just-created container behind for
// the caller (or a later ReconcileOrphans pass) to discover independently
// -- Run itself removes it before returning.
func TestRunWithComposeNetworkCleansUpOnConnectFailure(t *testing.T) {
	t.Setenv(SkipMountVisibilityCheckEnv, "1")
	callsPath := filepath.Join(t.TempDir(), "calls")
	docker := filepath.Join(t.TempDir(), "docker-fake")
	script := "#!/bin/sh\n" +
		"sub=\"$1\"\n" +
		"printf '%s\\n' \"$sub\" >> \"" + callsPath + "\"\n" +
		"if [ \"$sub\" = \"network\" ]; then exit 1; fi\n" +
		"exit 0\n"
	if err := os.WriteFile(docker, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	spec := validSpec()
	spec.WorkDir = t.TempDir()
	spec.InputDir = ""
	spec.LogPath = filepath.Join(t.TempDir(), "worker.log")
	spec.DataDir = t.TempDir()
	spec.Network = "factoryd-relay-run1"
	spec.ComposeNetwork = "bg-compose-run1"

	if _, err := Run(context.Background(), docker, spec); err == nil {
		t.Fatal("Run with a failing network connect = nil error, want one")
	}
	calls, err := os.ReadFile(callsPath)
	if err != nil {
		t.Fatalf("read fake docker calls: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(calls)), "\n")
	if len(lines) != 3 || lines[0] != "create" || lines[1] != "network" || lines[2] != "rm" {
		t.Fatalf("docker verbs invoked = %v, want [create network rm] (the failed connect's own cleanup)", lines)
	}
}

func TestRunTreatsDockerLauncherStatusAsInfrastructureFailure(t *testing.T) {
	workspace := t.TempDir()
	docker := filepath.Join(t.TempDir(), "docker-fake")
	if err := os.WriteFile(docker, []byte("#!/bin/sh\nexit 125\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	spec := validSpec()
	spec.WorkDir = workspace
	spec.InputDir = ""
	spec.LogPath = filepath.Join(t.TempDir(), "worker.log")
	spec.DataDir = t.TempDir()
	_, err := Run(context.Background(), docker, spec)
	if err == nil {
		t.Fatal("Run unexpectedly treated Docker launcher failure as worker exit")
	}
}

// TestRunEnforcesOutputSizeLimit is the regression for the "bound durable
// ... log growth" gap found via review: a worker that
// never stops printing must be reaped and reported, not allowed to grow its
// log file without end.
func TestRunEnforcesOutputSizeLimit(t *testing.T) {
	previous := maxLogBytes
	maxLogBytes = 64
	t.Cleanup(func() { maxLogBytes = previous })

	workspace := t.TempDir()
	docker := filepath.Join(t.TempDir(), "docker-fake")
	// A fake "docker run" that just keeps printing; the real binary is never
	// invoked (Setpgid/Cancel target this fake process's own group). Run's
	// own pre-launch mount-visibility probe (verifyWorkDirMountVisibility)
	// also shells out through this same fake binary before the real
	// launch below, so it must recognize and answer that invocation
	// (matched by mountVisibilityProbePath appearing in its args) rather
	// than loop forever itself.
	// Teardown (reapContainer, on the output-limit error below) shells out
	// through this same fake binary for `docker rm -f` and `docker exec
	// ... chmod` -- without exiting immediately for those too, both fall
	// into the infinite loop below meant only for the initial `docker run`
	// launch, and ensureRemoved's own 10-second timeout (docker.go) is
	// what actually ends the test, adding 10s of pure wait for no
	// additional coverage (found live, 2026-09-16: this test alone
	// accounted for 10s of internal/sandbox's ~17s non-live suite time).
	script := "#!/bin/sh\ncase \"$1\" in rm|exec) exit 0 ;; esac\ncase \"$*\" in *sandbox-mount-probe*) exit 0 ;; esac\nwhile true; do echo 'line of sandbox output well past the limit'; done\n"
	if err := os.WriteFile(docker, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	spec := validSpec()
	spec.WorkDir = workspace
	spec.InputDir = ""
	spec.LogPath = filepath.Join(t.TempDir(), "worker.log")
	spec.DataDir = t.TempDir()

	_, err := Run(context.Background(), docker, spec)
	if err == nil || !strings.Contains(err.Error(), "exceeded the log size limit") {
		t.Fatalf("Run() error = %v, want an output-limit error", err)
	}
}

// TestRunRelaysFactoryProgressLinesToProgressPath is the regression test
// for the progress feed's sandbox relay (internal/progress,
// progress-contract.md): a FACTORY_PROGRESS line on the worker's stdout
// must be both written to the log file unchanged AND appended, parsed,
// to LaunchSpec.ProgressPath -- while a non-matching line is only ever
// written to the log, never to the progress file.
func TestRunRelaysFactoryProgressLinesToProgressPath(t *testing.T) {
	workspace := t.TempDir()
	docker := filepath.Join(t.TempDir(), "docker-fake")
	script := "#!/bin/sh\n" +
		"case \"$1\" in rm|exec) exit 0 ;; esac\n" +
		"case \"$*\" in *sandbox-mount-probe*) exit 0 ;; esac\n" +
		"echo 'plain worker output'\n" +
		"echo 'FACTORY_PROGRESS {\"stage\":\"round\",\"event\":\"start\",\"round\":1,\"max_rounds\":3}'\n"
	if err := os.WriteFile(docker, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	spec := validSpec()
	spec.WorkDir = workspace
	spec.InputDir = ""
	spec.LogPath = filepath.Join(t.TempDir(), "worker.log")
	spec.DataDir = t.TempDir()
	spec.ProgressPath = filepath.Join(t.TempDir(), "progress.jsonl")

	if _, err := Run(context.Background(), docker, spec); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	logBytes, err := os.ReadFile(spec.LogPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if !strings.Contains(string(logBytes), "plain worker output") || !strings.Contains(string(logBytes), "FACTORY_PROGRESS") {
		t.Fatalf("log file missing raw worker lines: %s", logBytes)
	}

	progressBytes, err := os.ReadFile(spec.ProgressPath)
	if err != nil {
		t.Fatalf("read progress file: %v", err)
	}
	if strings.Contains(string(progressBytes), "plain worker output") {
		t.Fatalf("progress file must only contain parsed FACTORY_PROGRESS lines: %s", progressBytes)
	}
	if !strings.Contains(string(progressBytes), `"source":"worker"`) || !strings.Contains(string(progressBytes), `"stage":"round"`) {
		t.Fatalf("progress file missing relayed event: %s", progressBytes)
	}
}

// TestRunRefusesToLaunchBelowFreeSpaceFloor is the regression test for a
// real Opus review finding, 2026-09-04: the worker's rw /workspace mount
// had no size limit at all -- a worker that simply writes without end can
// fill the host filesystem, including whatever volume holds -data-dir and
// every run's durable evidence. Proves checkMinFreeBytes actually gates
// Run: with the floor set absurdly high (above any real filesystem's free
// space), launch is refused before Docker is ever invoked -- the fake
// "docker" binary here would fail loudly (nonexistent flags) if reached at
// all, so success here would mean the guard let a launch through it
// shouldn't have.
func TestRunRefusesToLaunchBelowFreeSpaceFloor(t *testing.T) {
	previous := minFreeBytesForLaunch
	minFreeBytesForLaunch = 1 << 62 // far more than any real filesystem has free
	t.Cleanup(func() { minFreeBytesForLaunch = previous })

	workspace := t.TempDir()
	docker := filepath.Join(t.TempDir(), "docker-fake")
	if err := os.WriteFile(docker, []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	spec := validSpec()
	spec.WorkDir = workspace
	spec.InputDir = ""
	spec.LogPath = filepath.Join(t.TempDir(), "worker.log")
	spec.DataDir = t.TempDir()

	_, err := Run(context.Background(), docker, spec)
	if err == nil || !strings.Contains(err.Error(), "bytes free") {
		t.Fatalf("Run() error = %v, want a free-space error", err)
	}
}

// TestRunChecksFreeSpaceOnDataDirSeparatelyFromWorkDir is the regression
// test for a real self-review finding, 2026-09-04: -workspace and
// -data-dir are never required to share a filesystem, but the free-space
// guard originally checked only WorkDir -- silently defeating its own
// stated purpose of protecting the volume holding durable run evidence
// whenever the two differ. Proves both that a distinct DataDir is
// actually consulted (the floor set high enough to trip it, with WorkDir
// itself left plenty of room) and that a DataDir which doesn't exist yet
// fails open rather than blocking an otherwise-legitimate launch.
// TestRunChecksFreeSpaceOnDataDirSeparatelyFromWorkDir is the regression
// test for a real self-review finding, 2026-09-04: -workspace and
// -data-dir are never required to share a filesystem, but the free-space
// guard originally checked only WorkDir -- silently defeating its own
// stated purpose of protecting the volume holding durable run evidence
// whenever the two differ.
//
// Injects statfsFreeBytes rather than putting both directories on the
// real filesystem (found via a real Codex review comment on this same
// change's own PR): WorkDir's check always runs first, so with both
// paths on one real filesystem it short-circuits before DataDir's check
// is ever reached -- deleting the DataDir branch entirely would still
// have passed the original version of this test. The fake here makes
// WorkDir report plenty of free space and DataDir report almost none, so
// only a genuine, independent DataDir check can produce the failure
// asserted below.
func TestRunChecksFreeSpaceOnDataDirSeparatelyFromWorkDir(t *testing.T) {
	previous := statfsFreeBytes
	t.Cleanup(func() { statfsFreeBytes = previous })

	workspace := t.TempDir()
	dataDir := t.TempDir()
	statfsFreeBytes = func(path string) (uint64, bool) {
		switch path {
		case workspace:
			return minFreeBytesForLaunch * 2, true // WorkDir: plenty of room
		case dataDir:
			return minFreeBytesForLaunch / 2, true // DataDir: below the floor
		default:
			return previous(path)
		}
	}

	// This fake docker's "exit 1" stands in for the real worker launch
	// this test expects to reach (proving DataDir's free-space check
	// failed open) -- it must answer Run's own pre-launch mount-visibility
	// probe (verifyWorkDirMountVisibility) with success first, or the
	// probe itself would report a mount-visibility failure before the
	// real launch this test is actually about ever runs.
	docker := filepath.Join(t.TempDir(), "docker-fake")
	if err := os.WriteFile(docker, []byte("#!/bin/sh\ncase \"$*\" in *sandbox-mount-probe*) exit 0 ;; esac\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	spec := validSpec()
	spec.WorkDir = workspace
	spec.InputDir = ""
	spec.LogPath = filepath.Join(t.TempDir(), "worker.log")
	spec.DataDir = dataDir

	_, err := Run(context.Background(), docker, spec)
	if err == nil || !strings.Contains(err.Error(), "data directory:") {
		t.Fatalf("Run() error = %v, want a free-space error prefixed \"data directory:\" (WorkDir alone has plenty of room; only an independent DataDir check can produce this)", err)
	}

	// Now prove the DataDir branch fails OPEN, not closed, when the
	// lookup itself fails (e.g. DataDir doesn't exist on disk yet --
	// Validate only requires the string to be a non-empty absolute path,
	// never that the directory already exists).
	statfsFreeBytes = func(path string) (uint64, bool) {
		if path == workspace {
			return minFreeBytesForLaunch * 2, true
		}
		return 0, false // DataDir: lookup fails
	}
	result, err := Run(context.Background(), docker, spec)
	if err != nil {
		t.Fatalf("Run() with a failed DataDir free-space lookup = %v, want the check to fail open", err)
	}
	if result.ExitCode != 1 {
		t.Fatalf("Run() exit code = %d, want 1 (the fake docker's own exit, proving launch was actually attempted)", result.ExitCode)
	}
}

// TestVerifyWorkDirMountVisibilityFailsClosedWhenMarkerInvisible is the
// regression test for the real, reproduced colima trap: a probe container
// that cannot see the marker this same process just wrote to the host side
// of its own bind mount (simulated here by a fake "docker" that always
// answers `test -f` with exit 1, standing in for a real Docker backend
// silently not sharing the mounted path) must fail closed with a specific,
// actionable diagnostic -- not a generic Docker error a real operator would
// have to rediscover the cause of themselves.
func TestVerifyWorkDirMountVisibilityFailsClosedWhenMarkerInvisible(t *testing.T) {
	docker := filepath.Join(t.TempDir(), "docker-fake")
	if err := os.WriteFile(docker, []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	spec := validSpec()
	spec.WorkDir = t.TempDir()

	err := verifyWorkDirMountVisibility(context.Background(), docker, spec)
	if err == nil {
		t.Fatal("verifyWorkDirMountVisibility() = nil, want a mount-visibility error")
	}
	for _, want := range []string{spec.WorkDir, "does not appear to share", "colima", SkipMountVisibilityCheckEnv} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing expected substring %q", err.Error(), want)
		}
	}
}

// TestVerifyWorkDirMountVisibilityPassesWhenMarkerVisible proves the normal
// case: a probe container that DOES answer `test -f` with exit 0 (standing
// in for a real Docker backend that shares the mount correctly) passes
// cleanly.
func TestVerifyWorkDirMountVisibilityPassesWhenMarkerVisible(t *testing.T) {
	docker := filepath.Join(t.TempDir(), "docker-fake")
	if err := os.WriteFile(docker, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	spec := validSpec()
	spec.WorkDir = t.TempDir()

	if err := verifyWorkDirMountVisibility(context.Background(), docker, spec); err != nil {
		t.Fatalf("verifyWorkDirMountVisibility() = %v, want nil", err)
	}
}

// TestVerifyWorkDirMountVisibilityFailsOpenWhenInconclusive proves the
// check does not permanently block a legitimate worker image that has no
// shell at all (LaunchSpec.WorkerUmask documents the same class of image):
// a probe failure that is NOT `test`'s own exit-1 "file not found" (here,
// exit 127, the conventional "command not found" a shell-less image's
// --entrypoint /bin/sh would produce) says nothing conclusive about mount
// visibility and must fail open, not closed.
func TestVerifyWorkDirMountVisibilityFailsOpenWhenInconclusive(t *testing.T) {
	docker := filepath.Join(t.TempDir(), "docker-fake")
	if err := os.WriteFile(docker, []byte("#!/bin/sh\nexit 127\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	spec := validSpec()
	spec.WorkDir = t.TempDir()

	if err := verifyWorkDirMountVisibility(context.Background(), docker, spec); err != nil {
		t.Fatalf("verifyWorkDirMountVisibility() = %v, want nil (inconclusive must fail open)", err)
	}
}

// TestVerifyWorkDirMountVisibilitySkippedViaEnv proves
// SkipMountVisibilityCheckEnv is a real, honored escape valve: with it set,
// the probe never even shells out (a fake "docker" that would otherwise
// fail closed is never invoked at all).
func TestVerifyWorkDirMountVisibilitySkippedViaEnv(t *testing.T) {
	docker := filepath.Join(t.TempDir(), "docker-fake")
	if err := os.WriteFile(docker, []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(SkipMountVisibilityCheckEnv, "1")
	spec := validSpec()
	spec.WorkDir = t.TempDir()

	if err := verifyWorkDirMountVisibility(context.Background(), docker, spec); err != nil {
		t.Fatalf("verifyWorkDirMountVisibility() = %v, want nil when skipped via env", err)
	}
}

// TestVerifyWorkDirMountVisibilityCachedPerWorkDir proves the probe runs at
// most once per resolved WorkDir per process: internal/runner.RunWithRetries
// calls sandbox.Run fresh on every retry attempt against the same WorkDir,
// and re-paying a probe container's cost on every retry would be wasted
// overhead once the first attempt already confirmed the mount is visible.
func TestVerifyWorkDirMountVisibilityCachedPerWorkDir(t *testing.T) {
	countPath := filepath.Join(t.TempDir(), "invocations")
	docker := filepath.Join(t.TempDir(), "docker-fake")
	script := fmt.Sprintf("#!/bin/sh\necho x >> %q\nexit 0\n", countPath)
	if err := os.WriteFile(docker, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	spec := validSpec()
	spec.WorkDir = t.TempDir()
	t.Cleanup(func() { mountVisibilityVerified.Delete(spec.WorkDir) })

	if err := verifyWorkDirMountVisibility(context.Background(), docker, spec); err != nil {
		t.Fatalf("first call: verifyWorkDirMountVisibility() = %v, want nil", err)
	}
	if err := verifyWorkDirMountVisibility(context.Background(), docker, spec); err != nil {
		t.Fatalf("second call: verifyWorkDirMountVisibility() = %v, want nil", err)
	}

	invocations, err := os.ReadFile(countPath)
	if err != nil {
		t.Fatalf("read invocation count: %v", err)
	}
	if got := strings.Count(string(invocations), "x"); got != 1 {
		t.Fatalf("docker invoked %d times across two calls for the same WorkDir, want 1 (second call should have used the cache)", got)
	}
}

// resolveDigestForLiveTest pulls tag and returns its digest-pinned reference
// (name@sha256:...), since Validate now requires every sandbox image to be
// digest-pinned and this live test's default image is a plain tag.
func resolveDigestForLiveTest(t *testing.T, tag string) string {
	t.Helper()
	if err := exec.Command("docker", "pull", tag).Run(); err != nil {
		t.Fatalf("docker pull %s: %v", tag, err)
	}
	out, err := exec.Command("docker", "inspect", "--format", "{{index .RepoDigests 0}}", tag).Output()
	if err != nil {
		t.Fatalf("docker inspect %s: %v", tag, err)
	}
	digestRef := strings.TrimSpace(string(out))
	if digestRef == "" || !strings.Contains(digestRef, "@sha256:") {
		t.Fatalf("docker inspect %s did not return a digest-pinned reference: %q", tag, digestRef)
	}
	return digestRef
}

// TestReconcileOrphansRemovesOnlyTerminalRuns is the regression for the
// orphaned-container gap found via review: a container whose
// run record is already terminal (including a StateHalted record only once
// HaltConfirmed is true — an unconfirmed halt is not proof the underlying
// execution actually stopped) must be removed; one whose run is still
// active, one whose halt is unconfirmed, and one with no matching run
// record at all must all be left alone (too ambiguous for the conservative
// protocol).
func TestReconcileOrphansRemovesOnlyTerminalRuns(t *testing.T) {
	dataDir := t.TempDir()
	for _, r := range []run.Run{
		{ID: "run-terminal", State: run.StateHalted, HaltConfirmed: true},
		{ID: "run-active", State: run.StateSliceRunning},
		{ID: "run-halted-unconfirmed", State: run.StateHalted, HaltConfirmed: false},
	} {
		r := r
		if err := r.Save(dataDir); err != nil {
			t.Fatalf("save run %s: %v", r.ID, err)
		}
	}

	docker := filepath.Join(t.TempDir(), "docker-fake")
	script := `#!/bin/sh
case "$1" in
ps)
  printf 'container-terminal\trun-terminal\n'
  printf 'container-active\trun-active\n'
  printf 'container-halted-unconfirmed\trun-halted-unconfirmed\n'
  printf 'container-unknown\trun-unknown\n'
  ;;
rm)
  exit 0
  ;;
esac
`
	if err := os.WriteFile(docker, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	removed, err := ReconcileOrphans(context.Background(), docker, dataDir)
	if err != nil {
		t.Fatalf("ReconcileOrphans: %v", err)
	}
	if len(removed) != 1 || removed[0] != "container-terminal" {
		t.Fatalf("removed = %v, want only [container-terminal]", removed)
	}
}

// TestReconcileOrphansExcludesRelayContainers is the regression for a
// codex finding on PR #42: a relay container carries this same
// buildgate.data-dir label RouteSpec.DataDir attaches (see its own
// doc comment), so it matches this function's own listing filter too.
// Without an explicit exclusion, this worker-only reconciler would process
// a stale relay container as if it were an ordinary worker container --
// removing only the container (never its paired network) via a plain
// ensureRemoved, then unconditionally quarantining the run via
// quarantineAbandonedRun, which has no equivalent of
// ReconcileRelayOrphans' own WorkerContainerPresentForRun guard. That could
// mark a run HaltConfirmed=true while its real worker container is still
// running. Proven here with a stale non-terminal run whose only listed
// container is named like a relay container: it must be left entirely
// alone by this function.
func TestReconcileOrphansExcludesRelayContainers(t *testing.T) {
	previousStale := ownerStaleAfter
	ownerStaleAfter = 0
	t.Cleanup(func() { ownerStaleAfter = previousStale })
	previousInterval := ownerHeartbeatInterval
	ownerHeartbeatInterval = time.Millisecond
	t.Cleanup(func() { ownerHeartbeatInterval = previousInterval })

	dataDir := t.TempDir()
	r := run.Run{ID: "run-relay-only-listed", State: run.StateSliceRunning}
	if err := r.Save(dataDir); err != nil {
		t.Fatalf("save run: %v", err)
	}
	if err := writeOwnerHeartbeat(dataDir, r.ID); err != nil {
		t.Fatalf("writeOwnerHeartbeat: %v", err)
	}

	docker := filepath.Join(t.TempDir(), "docker-fake")
	script := `#!/bin/sh
case "$1" in
ps)
  printf 'factoryd-relay-container-excluded\trun-relay-only-listed\n'
  ;;
rm)
  exit 0
  ;;
esac
`
	if err := os.WriteFile(docker, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	removed, err := ReconcileOrphans(context.Background(), docker, dataDir)
	if err != nil {
		t.Fatalf("ReconcileOrphans: %v", err)
	}
	if len(removed) != 0 {
		t.Fatalf("removed = %v, want none -- a relay container must be left to ReconcileRelayOrphans, not swept up here", removed)
	}
	record, err := run.Load(dataDir, r.ID)
	if err != nil {
		t.Fatalf("run.Load after reconciliation: %v", err)
	}
	if record.State == run.StateQuarantined || record.HaltConfirmed {
		t.Errorf("run state = %q, HaltConfirmed = %v, want untouched -- ReconcileOrphans must not quarantine a run based on a relay container alone", record.State, record.HaltConfirmed)
	}
}

// TestReconcileOrphansCancellationInterruptsRemoval is the regression for
// a real P2 finding from a sixth round of GitHub Codex review of PR #37:
// ensureRemoved used to always build its own context.Background()-based
// 10-second timeout regardless of ReconcileOrphans' own ctx parameter, so
// a caller's cancellation (daemonMain's own signalCtx on SIGTERM) could not
// interrupt an in-flight `docker rm` — only the staleness-debounce wait
// (the prior round's fix) actually honored it. Proven with a fake `docker
// rm` that sleeps far longer than the test's own cancellation delay:
// ReconcileOrphans must return promptly once ctx is canceled, not wait out
// the full sleep.
func TestReconcileOrphansCancellationInterruptsRemoval(t *testing.T) {
	dataDir := t.TempDir()
	r := run.Run{ID: "run-terminal", State: run.StateHalted, HaltConfirmed: true}
	if err := r.Save(dataDir); err != nil {
		t.Fatalf("save run: %v", err)
	}

	docker := filepath.Join(t.TempDir(), "docker-fake")
	script := "#!/bin/sh\ncase \"$1\" in\nps) printf 'container-terminal\\trun-terminal\\n' ;;\nrm) sleep 5 ;;\nesac\n"
	if err := os.WriteFile(docker, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	started := time.Now()
	_, err := ReconcileOrphans(ctx, docker, dataDir)
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("ReconcileOrphans with a canceled ctx during removal: want error, got nil")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("ReconcileOrphans took %v to return after ctx was canceled at ~200ms — cancellation did not interrupt the in-flight removal (fake `docker rm` sleeps 5s)", elapsed)
	}
}

// TestReconcileOrphansQuarantinesAbandonedNonTerminalRun is the regression
// for a codex finding: a non-terminal run record alone left its container
// (and the stuck run itself) orphaned forever whenever the owning attempt
// was no longer actually being supervised, contradicting the intended
// contract that an unknown prior container must be stopped,
// recorded, and quarantined. Once the owner heartbeat writeOwnerHeartbeat
// records goes stale, both the container and the run record must be
// reclaimed — proven here via a genuinely stale marker (old mtime), not
// merely a dead PID, since staleness (not process existence) is the actual
// signal ownerHeartbeatStale checks (see its own doc comment for why: a
// long-lived host process can outlive one specific attempt it once owned).
func TestReconcileOrphansQuarantinesAbandonedNonTerminalRun(t *testing.T) {
	previousStale := ownerStaleAfter
	ownerStaleAfter = 0 // any marker at all is immediately "stale" for this test
	t.Cleanup(func() { ownerStaleAfter = previousStale })
	// Also shrunk (found via a round-4 codex review fix adding a
	// staleness-debounce wait of ownerHeartbeatInterval before reaping —
	// see ReconcileOrphans' own comment): without this, the debounce wait
	// still uses the real 15s default, making this test needlessly slow.
	previousInterval := ownerHeartbeatInterval
	ownerHeartbeatInterval = time.Millisecond
	t.Cleanup(func() { ownerHeartbeatInterval = previousInterval })

	dataDir := t.TempDir()
	r := run.Run{ID: "run-abandoned", State: run.StateSliceRunning}
	if err := r.Save(dataDir); err != nil {
		t.Fatalf("save run: %v", err)
	}
	if err := writeOwnerHeartbeat(dataDir, r.ID); err != nil {
		t.Fatalf("writeOwnerHeartbeat: %v", err)
	}

	docker := filepath.Join(t.TempDir(), "docker-fake")
	script := "#!/bin/sh\ncase \"$1\" in\nps) printf 'container-abandoned\\trun-abandoned\\n' ;;\nrm) exit 0 ;;\nesac\n"
	if err := os.WriteFile(docker, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	removed, err := ReconcileOrphans(context.Background(), docker, dataDir)
	if err != nil {
		t.Fatalf("ReconcileOrphans: %v", err)
	}
	if len(removed) != 1 || removed[0] != "container-abandoned" {
		t.Fatalf("removed = %v, want [container-abandoned]", removed)
	}
	record, err := run.Load(dataDir, r.ID)
	if err != nil {
		t.Fatalf("run.Load after reconciliation: %v", err)
	}
	if record.State != run.StateQuarantined || !record.HaltConfirmed {
		t.Errorf("run state = %q, HaltConfirmed = %v, want quarantined and confirmed", record.State, record.HaltConfirmed)
	}
}

// TestReconcileOrphansLeavesNonTerminalRunWithLiveOwner proves the flip
// side: a non-terminal run whose owner heartbeat is fresh (well inside
// ownerStaleAfter) must not be touched — it may be a legitimate,
// still-running sibling invocation against the same data directory, or
// (per ownerHeartbeatStale's own doc comment) still-active work inside a
// long-lived daemon process.
func TestReconcileOrphansLeavesNonTerminalRunWithLiveOwner(t *testing.T) {
	dataDir := t.TempDir()
	r := run.Run{ID: "run-live-owner", State: run.StateSliceRunning}
	if err := r.Save(dataDir); err != nil {
		t.Fatalf("save run: %v", err)
	}
	if err := writeOwnerHeartbeat(dataDir, r.ID); err != nil {
		t.Fatalf("writeOwnerHeartbeat: %v", err)
	}

	docker := filepath.Join(t.TempDir(), "docker-fake")
	script := "#!/bin/sh\ncase \"$1\" in\nps) printf 'container-live-owner\\trun-live-owner\\n' ;;\nrm) exit 0 ;;\nesac\n"
	if err := os.WriteFile(docker, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	removed, err := ReconcileOrphans(context.Background(), docker, dataDir)
	if err != nil {
		t.Fatalf("ReconcileOrphans: %v", err)
	}
	if len(removed) != 0 {
		t.Fatalf("removed = %v, want none (owner process is still alive)", removed)
	}
	record, err := run.Load(dataDir, r.ID)
	if err != nil {
		t.Fatalf("run.Load after reconciliation: %v", err)
	}
	if record.State != run.StateSliceRunning {
		t.Errorf("run state = %q, want unchanged %q", record.State, run.StateSliceRunning)
	}
}

// TestReconcileOrphansDebouncesStalenessBeforeReaping is the regression for
// a real P2 finding from a fourth round of GitHub Codex review of PR #37:
// a process/host suspend (SIGSTOP, laptop sleep, cgroup freeze) longer than
// ownerStaleAfter can make a genuinely live attempt's heartbeat marker
// look stale for the brief window between resume and the owning heartbeat
// goroutine's next scheduled refresh. Simulates exactly that: the marker
// starts already stale (as if the process had been suspended past
// ownerStaleAfter), and the just-resumed heartbeat refreshes it during
// ReconcileOrphans' own debounce wait (staleOwnerRefreshedDuringDebounce)
// — proving the container survives instead of being reaped out from
// under a real, still-running attempt.
func TestReconcileOrphansDebouncesStalenessBeforeReaping(t *testing.T) {
	dataDir := t.TempDir()
	r := run.Run{ID: "run-resumed-owner", State: run.StateSliceRunning}
	if err := r.Save(dataDir); err != nil {
		t.Fatalf("save run: %v", err)
	}
	debounces := staleOwnerRefreshedDuringDebounce(t, dataDir, r.ID)

	docker := filepath.Join(t.TempDir(), "docker-fake")
	script := "#!/bin/sh\ncase \"$1\" in\nps) printf 'container-resumed-owner\\trun-resumed-owner\\n' ;;\nrm) exit 0 ;;\nesac\n"
	if err := os.WriteFile(docker, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	removed, err := ReconcileOrphans(context.Background(), docker, dataDir)
	if err != nil {
		t.Fatalf("ReconcileOrphans: %v", err)
	}
	if len(removed) != 0 {
		t.Fatalf("removed = %v, want none: the owner refreshed its heartbeat during the debounce wait, so this container must survive", removed)
	}
	if *debounces != 1 {
		t.Errorf("debounce waits = %d, want 1: the stale marker must be rechecked after one wait", *debounces)
	}
	record, err := run.Load(dataDir, r.ID)
	if err != nil {
		t.Fatalf("run.Load after reconciliation: %v", err)
	}
	if record.State != run.StateSliceRunning {
		t.Errorf("run state = %q, want unchanged %q — a resumed owner's run must not be quarantined out from under it", record.State, run.StateSliceRunning)
	}
}

// staleOwnerRefreshedDuringDebounce sets up a suspended-then-resumed owner
// for runID without any wall-clock wait: its heartbeat marker is written and
// back-dated past ownerStaleAfter, and the debounce wait (ownerDebounceAfter)
// is replaced by one that refreshes the marker, as the owner's heartbeat
// goroutine would on resume, and then fires at once. It fails the test if the
// marker does not read stale before the wait or fresh after the refresh, and
// returns the number of debounce waits started.
func staleOwnerRefreshedDuringDebounce(t *testing.T, dataDir, runID string) *int {
	t.Helper()
	// Strictly positive, so that a refreshed marker reads as fresh again,
	// and far larger than any scheduling delay.
	previousStale := ownerStaleAfter
	ownerStaleAfter = time.Hour
	t.Cleanup(func() { ownerStaleAfter = previousStale })

	if err := writeOwnerHeartbeat(dataDir, runID); err != nil {
		t.Fatalf("writeOwnerHeartbeat: %v", err)
	}
	suspendedSince := time.Now().Add(-2 * ownerStaleAfter)
	if err := os.Chtimes(ownerPIDPath(dataDir, runID), suspendedSince, suspendedSince); err != nil {
		t.Fatalf("back-date owner heartbeat: %v", err)
	}
	if !ownerHeartbeatStale(dataDir, runID) {
		t.Fatal("back-dated owner heartbeat does not read stale")
	}

	debounces := new(int)
	previousDebounceAfter := ownerDebounceAfter
	ownerDebounceAfter = func(time.Duration) <-chan time.Time {
		*debounces++
		if err := writeOwnerHeartbeat(dataDir, runID); err != nil {
			t.Errorf("refresh owner heartbeat during the debounce wait: %v", err)
		}
		if ownerHeartbeatStale(dataDir, runID) {
			t.Error("refreshed owner heartbeat still reads stale")
		}
		ch := make(chan time.Time, 1)
		ch <- time.Now()
		return ch
	}
	t.Cleanup(func() { ownerDebounceAfter = previousDebounceAfter })
	return debounces
}

// TestReconcileOrphansDebouncesBatchOnce is the regression for a real P2
// finding from a ninth round of GitHub Codex review of PR #37: an earlier
// version of the staleness debounce waited a full ownerHeartbeatInterval
// separately for every stale non-terminal candidate in one scan, so N
// abandoned runs at startup delayed the caller (daemonMain, before it
// even dials Temporal) by N * ownerHeartbeatInterval — a modest orphan
// backlog could make a restart unavailable for minutes. Three stale
// candidates here must all be reconciled within roughly one debounce
// interval, not three.
func TestReconcileOrphansDebouncesBatchOnce(t *testing.T) {
	previousStale := ownerStaleAfter
	ownerStaleAfter = 0 // every marker starts already stale
	t.Cleanup(func() { ownerStaleAfter = previousStale })
	previousInterval := ownerHeartbeatInterval
	const interval = 300 * time.Millisecond
	ownerHeartbeatInterval = interval
	t.Cleanup(func() { ownerHeartbeatInterval = previousInterval })

	// Replace the debounce wait's real timer with an immediately-firing
	// channel and count how many times it's constructed, instead of
	// asserting on wall-clock elapsed time (found flaky under `make
	// verify`'s concurrent -race load this session, always passing
	// standalone/on rerun): what this test actually needs to prove is that
	// the batch is debounced once, not once per candidate — not how long a
	// real wait takes on a possibly-loaded machine.
	var debounceCalls int
	previousDebounceAfter := ownerDebounceAfter
	ownerDebounceAfter = func(d time.Duration) <-chan time.Time {
		debounceCalls++
		ch := make(chan time.Time, 1)
		ch <- time.Now()
		return ch
	}
	t.Cleanup(func() { ownerDebounceAfter = previousDebounceAfter })

	dataDir := t.TempDir()
	ids := []string{"run-batch-a", "run-batch-b", "run-batch-c"}
	for _, id := range ids {
		r := run.Run{ID: id, State: run.StateSliceRunning}
		if err := r.Save(dataDir); err != nil {
			t.Fatalf("save run %s: %v", id, err)
		}
		if err := writeOwnerHeartbeat(dataDir, id); err != nil {
			t.Fatalf("writeOwnerHeartbeat %s: %v", id, err)
		}
	}

	docker := filepath.Join(t.TempDir(), "docker-fake")
	script := "#!/bin/sh\ncase \"$1\" in\n" +
		"ps)\n" +
		"  printf 'container-batch-a\\trun-batch-a\\n'\n" +
		"  printf 'container-batch-b\\trun-batch-b\\n'\n" +
		"  printf 'container-batch-c\\trun-batch-c\\n'\n" +
		"  ;;\n" +
		"rm) exit 0 ;;\n" +
		"esac\n"
	if err := os.WriteFile(docker, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	removed, err := ReconcileOrphans(context.Background(), docker, dataDir)
	if err != nil {
		t.Fatalf("ReconcileOrphans: %v", err)
	}
	if len(removed) != 3 {
		t.Fatalf("removed = %v, want all 3 abandoned containers", removed)
	}
	// The old per-candidate behavior would have constructed this wait once
	// per stale candidate (3, here); the batched debounce constructs it
	// exactly once for the whole batch.
	if debounceCalls != 1 {
		t.Fatalf("debounce wait constructed %d times for 3 stale candidates, want exactly 1 shared across the whole batch, not one per candidate", debounceCalls)
	}
}

// TestOwnerHeartbeatRefreshesAndStops proves startOwnerHeartbeat's own
// contract directly: the marker's mtime keeps advancing while it runs, and
// stops advancing once stop() returns — the mechanism ReconcileOrphans'
// staleness check relies on.
func TestOwnerHeartbeatRefreshesAndStops(t *testing.T) {
	previousInterval := ownerHeartbeatInterval
	ownerHeartbeatInterval = 5 * time.Millisecond
	t.Cleanup(func() { ownerHeartbeatInterval = previousInterval })

	dataDir := t.TempDir()
	const runID = "run-heartbeat"
	if err := writeOwnerHeartbeat(dataDir, runID); err != nil {
		t.Fatalf("writeOwnerHeartbeat: %v", err)
	}
	firstStat, err := os.Stat(ownerPIDPath(dataDir, runID))
	if err != nil {
		t.Fatal(err)
	}

	stop := startOwnerHeartbeat(dataDir, runID)
	deadline := time.Now().Add(2 * time.Second)
	for {
		stat, err := os.Stat(ownerPIDPath(dataDir, runID))
		if err != nil {
			t.Fatal(err)
		}
		if stat.ModTime().After(firstStat.ModTime()) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("owner heartbeat marker never refreshed")
		}
		time.Sleep(time.Millisecond)
	}
	stop()

	afterStop, err := os.Stat(ownerPIDPath(dataDir, runID))
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond) // several heartbeat intervals
	stillAfterStop, err := os.Stat(ownerPIDPath(dataDir, runID))
	if err != nil {
		t.Fatal(err)
	}
	if !stillAfterStop.ModTime().Equal(afterStop.ModTime()) {
		t.Error("owner heartbeat kept refreshing after stop() returned")
	}
}

func TestRunLiveDocker(t *testing.T) {
	if os.Getenv("DOCKER_SANDBOX_LIVE") != "1" {
		t.Skip("set DOCKER_SANDBOX_LIVE=1 to run the real Docker acceptance test")
	}
	liveRoot := os.Getenv("DOCKER_SANDBOX_LIVE_ROOT")
	if liveRoot == "" {
		liveRoot = os.TempDir()
	}
	repoDir, err := os.MkdirTemp(liveRoot, "factoryd-sandbox-live-repo-")
	if err != nil {
		t.Fatalf("create repository: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(repoDir) })
	for _, args := range [][]string{{"init"}, {"config", "user.email", "test@example.com"}, {"config", "user.name", "Test"}} {
		if out, err := exec.Command("git", append([]string{"-C", repoDir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(repoDir, "README.md"), []byte("fixture\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", repoDir, "add", ".").CombinedOutput(); err != nil {
		t.Fatalf("git add: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "-C", repoDir, "commit", "-m", "fixture").CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v: %s", err, out)
	}
	workDir, err := os.MkdirTemp(liveRoot, "factoryd-sandbox-live-worktree-parent-")
	if err != nil {
		t.Fatalf("create worktree parent: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(workDir) })
	workDir = filepath.Join(workDir, "worktree")
	if out, err := exec.Command("git", "-C", repoDir, "worktree", "add", "-b", "sandbox-live", workDir).CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %v: %s", err, out)
	}
	if err := os.Chmod(workDir, 0o777); err != nil {
		t.Fatalf("chmod workspace: %v", err)
	}
	spec := validSpec()
	spec.Image = os.Getenv("DOCKER_SANDBOX_IMAGE")
	if spec.Image == "" {
		spec.Image = resolveDigestForLiveTest(t, "golang:1.26-bookworm")
	}
	spec.WorkDir = workDir
	spec.User = fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
	spec.InputDir = ""
	logDir, err := os.MkdirTemp(liveRoot, "factoryd-sandbox-log-")
	if err != nil {
		t.Fatalf("create log dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(logDir) })
	spec.LogPath = filepath.Join(logDir, "worker.log")
	spec.DataDir = t.TempDir()
	spec.Command = []string{"/bin/sh", "-c", "set -e; test \"$(id -u)\" != 0 || exit 12; test ! -e /var/run/docker.sock || exit 13; test \"$(git rev-parse --is-inside-work-tree)\" = true; echo worker-write >> README.md; if git update-ref refs/heads/worker-must-not-write HEAD 2>/dev/null; then exit 14; fi; echo sandbox-ok"}
	result, err := Run(context.Background(), "docker", spec)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.ExitCode != 0 {
		contents, _ := os.ReadFile(spec.LogPath)
		t.Fatalf("exit code = %d, want 0; command=%v; log=%s", result.ExitCode, result.Command, contents)
	}
	if out, err := exec.Command("git", "-C", repoDir, "show-ref", "--verify", "--quiet", "refs/heads/worker-must-not-write").CombinedOutput(); err == nil {
		t.Fatalf("sandbox worker wrote a ref through the read-only Git common-dir mount: %s", out)
	}
}

// TestRunLiveDockerCannotRewriteWorktreeGitPointer is the regression test
// for a real, demonstrated escape a real Opus review pass found and
// reproduced end to end, 2026-09-04: a linked worktree's own .git is
// not a directory, it's a regular file containing a "gitdir: <host path>"
// pointer. DockerCommand used to mount only the git-common-dir the
// pointer resolves to read-only (proven safe by TestRunLiveDocker above)
// -- the pointer FILE itself was left inside the rw /workspace bind
// mount, fully writable. A worker that rewrote it to point at a second,
// worker-authored fake repo (also inside /workspace, so equally
// writable) got the HOST's own later git calls against this same
// workspace (GitRevParseHEAD, GitIsClean, GitCommitAll's `git add -A`,
// ...) to read that fake repo's tracked config instead of the real one
// -- and git resolves filter.<name>.clean, diff.external,
// core.fsmonitor, etc. straight out of whatever a repo's own config
// declares, running them as whichever user invokes git. That user is the
// factoryd host process itself, with its own full environment (API keys,
// SSH agent, Docker access) -- host code execution from a worker that
// never left its container in any conventional sense.
//
// Proves the fix (DockerCommand now mounts the pointer file read-only
// too, not just the common dir it resolves to): the worker's rewrite
// attempt is denied by Docker at the container boundary, and the host's
// .git pointer is provably byte-identical before and after the run.
func TestRunLiveDockerCannotRewriteWorktreeGitPointer(t *testing.T) {
	if os.Getenv("DOCKER_SANDBOX_LIVE") != "1" {
		t.Skip("set DOCKER_SANDBOX_LIVE=1 to run the real Docker acceptance test")
	}
	liveRoot := os.Getenv("DOCKER_SANDBOX_LIVE_ROOT")
	if liveRoot == "" {
		liveRoot = os.TempDir()
	}
	repoDir, err := os.MkdirTemp(liveRoot, "factoryd-sandbox-live-repo2-")
	if err != nil {
		t.Fatalf("create repository: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(repoDir) })
	for _, args := range [][]string{{"init"}, {"config", "user.email", "test@example.com"}, {"config", "user.name", "Test"}} {
		if out, err := exec.Command("git", append([]string{"-C", repoDir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(repoDir, "README.md"), []byte("fixture\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", repoDir, "add", ".").CombinedOutput(); err != nil {
		t.Fatalf("git add: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "-C", repoDir, "commit", "-m", "fixture").CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v: %s", err, out)
	}
	workParent, err := os.MkdirTemp(liveRoot, "factoryd-sandbox-live-worktree2-parent-")
	if err != nil {
		t.Fatalf("create worktree parent: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(workParent) })
	workDir := filepath.Join(workParent, "worktree")
	if out, err := exec.Command("git", "-C", repoDir, "worktree", "add", "-b", "sandbox-live-2", workDir).CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %v: %s", err, out)
	}
	if err := os.Chmod(workDir, 0o777); err != nil {
		t.Fatalf("chmod workspace: %v", err)
	}

	gitPointerPath := filepath.Join(workDir, ".git")
	before, err := os.ReadFile(gitPointerPath)
	if err != nil {
		t.Fatalf("read .git pointer before run: %v", err)
	}
	if !strings.HasPrefix(string(before), "gitdir: ") {
		t.Fatalf(".git pointer = %q, want the real worktree gitdir: line (test setup assumption violated)", before)
	}

	spec := validSpec()
	spec.Image = os.Getenv("DOCKER_SANDBOX_IMAGE")
	if spec.Image == "" {
		spec.Image = resolveDigestForLiveTest(t, "golang:1.26-bookworm")
	}
	spec.WorkDir = workDir
	spec.User = fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
	spec.InputDir = ""
	logDir, err := os.MkdirTemp(liveRoot, "factoryd-sandbox-log2-")
	if err != nil {
		t.Fatalf("create log dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(logDir) })
	spec.LogPath = filepath.Join(logDir, "worker.log")
	spec.DataDir = t.TempDir()
	// The actual attack: build a second, fully worker-controlled fake
	// repo inside the writable /workspace mount, then try to redirect
	// .git at it. A real exploit would also declare a malicious
	// filter.<name>.clean in the fake repo's config and a matching
	// .gitattributes entry so the host's later `git add -A` runs it --
	// this test only needs to prove the redirection itself is refused,
	// since without it nothing downstream is reachable at all.
	spec.Command = []string{"/bin/sh", "-c", `set -e
mkdir -p /workspace/fake.git/worktrees/w
git init -q --bare /workspace/fake.git 2>/dev/null || true
if echo "gitdir: /workspace/fake.git/worktrees/w" > /workspace/.git 2>/tmp/write-err; then
  echo REWRITE-SUCCEEDED
else
  echo "REWRITE-BLOCKED: $(cat /tmp/write-err)"
fi`}
	result, err := Run(context.Background(), "docker", spec)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	logContents, _ := os.ReadFile(spec.LogPath)
	if strings.Contains(string(logContents), "REWRITE-SUCCEEDED") {
		t.Fatalf("sandbox worker rewrote the worktree's .git pointer file -- regression; log=%s", logContents)
	}
	if !strings.Contains(string(logContents), "REWRITE-BLOCKED") {
		t.Fatalf("worker's rewrite attempt neither succeeded nor was observed as blocked -- test itself may be broken; exit=%d log=%s", result.ExitCode, logContents)
	}

	after, err := os.ReadFile(gitPointerPath)
	if err != nil {
		t.Fatalf("read .git pointer after run: %v", err)
	}
	if string(after) != string(before) {
		t.Fatalf(".git pointer changed during the run: before=%q after=%q -- the read-only mount did not hold", before, after)
	}
}

// TestRunLiveDockerCannotWriteReferenceOracleMount is the live-Docker
// counterpart to TestDockerCommandMountsReferenceOracleReadOnly: that
// test only asserts the generated `docker` command string contains the
// `:ro` overlay mount flag for ReferenceOracleDir/ReferenceOracleMountPath
// (internal/sandbox/docker_test.go); nothing before this test actually
// spun up a real container and tried to write through it, so a Docker
// engine/storage-driver combination that silently ignored `:ro` on a
// nested overlay mount (the same class of engine-specific quirk
// sandbox-image's own local-registry digest workaround exists for, see
// .local-registry's own doc comment in the Makefile) would have gone
// undetected. Reproduces the same "attempt the write inside the
// container, assert REWRITE-BLOCKED via the log, then assert the
// host-side file is still byte-identical" shape
// TestRunLiveDockerCannotRewriteWorktreeGitPointer above uses for the
// .git pointer -- this proves the mechanism a future generated
// reference oracle would depend on actually holds, before building
// anything on top of it.
//
// referenceOracleAttacks is the one place TestRunLiveDockerCannotWriteReferenceOracleMount
// names its three attacks -- its own shell script and both marker-check
// loops derive from this list, not three independently hardcoded copies,
// so renaming or adding an attack can't silently desync script vs. check.
var referenceOracleAttacks = []string{"OVERWRITE", "CREATE", "DELETE"}

func TestRunLiveDockerCannotWriteReferenceOracleMount(t *testing.T) {
	if os.Getenv("DOCKER_SANDBOX_LIVE") != "1" {
		t.Skip("set DOCKER_SANDBOX_LIVE=1 to run the real Docker acceptance test")
	}
	liveRoot := os.Getenv("DOCKER_SANDBOX_LIVE_ROOT")
	if liveRoot == "" {
		liveRoot = os.TempDir()
	}
	workDir, err := os.MkdirTemp(liveRoot, "factoryd-sandbox-live-oracle-workspace-")
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(workDir) })
	if err := os.Chmod(workDir, 0o777); err != nil {
		t.Fatalf("chmod workspace: %v", err)
	}

	// oracleDir is deliberately a sibling of workDir, not a subdirectory
	// of it -- ReferenceOracleSourceContained (LaunchSpec.Validate) halts
	// the run outright if the two overlap, the same contract
	// -reference-oracle-dir's own flag help documents on the real CLI.
	oracleDir, err := os.MkdirTemp(liveRoot, "factoryd-sandbox-live-oracle-source-")
	if err != nil {
		t.Fatalf("create oracle source dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(oracleDir) })
	oracleFile := filepath.Join(oracleDir, "expected.txt")
	const oracleContents = "pristine reference output\n"
	if err := os.WriteFile(oracleFile, []byte(oracleContents), 0o666); err != nil {
		t.Fatalf("write oracle fixture: %v", err)
	}

	spec := validSpec()
	spec.Image = os.Getenv("DOCKER_SANDBOX_IMAGE")
	if spec.Image == "" {
		spec.Image = resolveDigestForLiveTest(t, "golang:1.26-bookworm")
	}
	spec.WorkDir = workDir
	spec.User = fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
	spec.InputDir = ""
	spec.ReferenceOracleDir = oracleDir
	spec.ReferenceOracleMountPath = "verify"
	logDir, err := os.MkdirTemp(liveRoot, "factoryd-sandbox-log-oracle-")
	if err != nil {
		t.Fatalf("create log dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(logDir) })
	spec.LogPath = filepath.Join(logDir, "worker.log")
	spec.DataDir = t.TempDir()
	// Three separate attacks in one run, each independently reported:
	// overwrite the existing oracle file, create a new file inside the
	// mount, and delete the existing file. A worker that could pull off
	// any one of these could tamper with a future reference-oracle
	// check's expected output (or its own fixtures) before that check
	// ever ran against it.
	spec.Command = []string{"/bin/sh", "-c", `set -e
if echo tampered > /workspace/verify/expected.txt 2>/tmp/write-err; then
  echo OVERWRITE-SUCCEEDED
else
  echo "OVERWRITE-BLOCKED: $(cat /tmp/write-err)"
fi
if echo new > /workspace/verify/worker-authored.txt 2>/tmp/create-err; then
  echo CREATE-SUCCEEDED
else
  echo "CREATE-BLOCKED: $(cat /tmp/create-err)"
fi
if rm -f /workspace/verify/expected.txt 2>/tmp/delete-err; then
  echo DELETE-SUCCEEDED
else
  echo "DELETE-BLOCKED: $(cat /tmp/delete-err)"
fi`}
	result, err := Run(context.Background(), "docker", spec)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	logContents, _ := os.ReadFile(spec.LogPath)
	log := string(logContents)
	// Derived from one list, not two independently hardcoded marker sets
	// (found via review): a future rename/addition to the shell script's
	// own markers above only needs to change referenceOracleAttacks, and
	// both the "worker must not succeed" and "attempt must be observed"
	// checks below stay in sync automatically instead of one silently
	// checking for a marker the script can never emit.
	for _, attack := range referenceOracleAttacks {
		if strings.Contains(log, attack+"-SUCCEEDED") {
			t.Errorf("sandbox worker %s-SUCCEEDED against the read-only reference-oracle mount -- log=%s", attack, log)
		}
		if !strings.Contains(log, attack+"-BLOCKED") {
			t.Errorf("worker's %s attempt neither succeeded nor was observed as blocked -- test itself may be broken; exit=%d log=%s", attack, result.ExitCode, log)
		}
	}

	afterContents, err := os.ReadFile(oracleFile)
	if err != nil {
		t.Fatalf("read oracle fixture after run (expected it to still exist): %v", err)
	}
	if string(afterContents) != oracleContents {
		t.Fatalf("oracle fixture changed during the run: before=%q after=%q -- the read-only mount did not hold", oracleContents, afterContents)
	}
	if _, err := os.Stat(filepath.Join(oracleDir, "worker-authored.txt")); err == nil {
		t.Fatal("worker-authored.txt exists in the oracle source dir after the run -- the worker wrote into it despite the read-only mount")
	}
}

// TestRunLiveDockerWithHardenedResourceLimits is the live-Docker acceptance
// test for the container-hardening increments: --cgroupns=private and the
// --ulimit core=0/nofile/nproc ceilings DockerCommand now always sets. The
// stated risk for this change is a too-tight ulimit silently
// breaking the Go toolchain a sandboxed build/verify/full-suite step
// depends on -- `go build` links against a large number of open files
// under a warm build cache and `go test` execs a freshly built test binary
// as a child process, so both are run for real here rather than asserting
// only on DockerCommand's argument list.
func TestRunLiveDockerWithHardenedResourceLimits(t *testing.T) {
	if os.Getenv("DOCKER_SANDBOX_LIVE") != "1" {
		t.Skip("set DOCKER_SANDBOX_LIVE=1 to run the real Docker acceptance test")
	}
	liveRoot := os.Getenv("DOCKER_SANDBOX_LIVE_ROOT")
	if liveRoot == "" {
		liveRoot = os.TempDir()
	}
	repoDir, err := os.MkdirTemp(liveRoot, "factoryd-sandbox-live-repo3-")
	if err != nil {
		t.Fatalf("create repository: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(repoDir) })
	for _, args := range [][]string{{"init"}, {"config", "user.email", "test@example.com"}, {"config", "user.name", "Test"}} {
		if out, err := exec.Command("git", append([]string{"-C", repoDir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	const mainGo = `package main

import "fmt"

func Add(a, b int) int { return a + b }

func main() { fmt.Println(Add(2, 3)) }
`
	const mainTestGo = `package main

import "testing"

func TestAdd(t *testing.T) {
	if got := Add(2, 3); got != 5 {
		t.Fatalf("Add(2, 3) = %d, want 5", got)
	}
}
`
	if err := os.WriteFile(filepath.Join(repoDir, "go.mod"), []byte("module hardened-limits-fixture\n\ngo 1.22\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "main.go"), []byte(mainGo), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "main_test.go"), []byte(mainTestGo), 0o666); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", repoDir, "add", ".").CombinedOutput(); err != nil {
		t.Fatalf("git add: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "-C", repoDir, "commit", "-m", "fixture").CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v: %s", err, out)
	}
	workDir, err := os.MkdirTemp(liveRoot, "factoryd-sandbox-live-worktree-parent3-")
	if err != nil {
		t.Fatalf("create worktree parent: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(workDir) })
	workDir = filepath.Join(workDir, "worktree")
	if out, err := exec.Command("git", "-C", repoDir, "worktree", "add", "-b", "sandbox-live-limits", workDir).CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %v: %s", err, out)
	}
	if err := os.Chmod(workDir, 0o777); err != nil {
		t.Fatalf("chmod workspace: %v", err)
	}
	spec := validSpec()
	spec.Image = os.Getenv("DOCKER_SANDBOX_IMAGE")
	if spec.Image == "" {
		spec.Image = resolveDigestForLiveTest(t, "golang:1.26-bookworm")
	}
	spec.WorkDir = workDir
	spec.User = fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
	spec.InputDir = ""
	spec.TmpfsSize = "512m" // GOCACHE/GOTMPDIR under /home/worker need real room for a `go test` link+exec, not the 64m validSpec() default
	logDir, err := os.MkdirTemp(liveRoot, "factoryd-sandbox-log3-")
	if err != nil {
		t.Fatalf("create log dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(logDir) })
	spec.LogPath = filepath.Join(logDir, "worker.log")
	spec.DataDir = t.TempDir()
	spec.Timeout = 3 * time.Minute
	// DockerCommand's fixed PATH (/usr/local/bin:/usr/bin:/bin) doesn't
	// include the golang image's own /usr/local/go/bin -- appended here
	// rather than via spec.Environment, keeping this test scoped to
	// proving the ulimit/cgroupns ceilings alone don't break the
	// toolchain, not to how PATH itself is assembled.
	spec.Command = []string{"/bin/sh", "-c", "set -e; export HOME=/home/worker GOCACHE=/home/worker/.cache/go-build GOTMPDIR=/home/worker PATH=/usr/local/go/bin:$PATH; go build ./... && go test ./..."}
	result, err := Run(context.Background(), "docker", spec)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.ExitCode != 0 {
		contents, _ := os.ReadFile(spec.LogPath)
		t.Fatalf("go build/test failed under the hardened resource limits: exit code = %d; log=%s", result.ExitCode, contents)
	}
}

// TestRunLiveDockerReclaimsWorkDirAfterForcedTeardown is the regression
// test for a real P1 found via GitHub Codex App review of PR #62:
// DockerCommand's own post-command shell epilogue (see LaunchSpec.WorkerUmask's
// doc comment) only reclaims group-writability under workerContainerWorkDir
// when the wrapped command exits on its own -- a forced teardown (this
// run's own -timeout-minutes, its output-size limit, or a factoryd crash)
// kills it first, before the epilogue ever runs. A worker that creates a
// nonempty directory, chmods it restrictive (0700), and never exits on its
// own would otherwise permanently strand that directory from factoryd's
// own later git/rollback code once a forced teardown hits. Proves
// reclaimContainerWorkDir's docker-exec-based fix against a REAL forced
// teardown, not a fake one: this run's own real spec.Timeout fires (the
// command sleeps well past it), Run returns a timeout error rather than
// the wrapped command's own exit code, and the created directory is
// group-writable -- and its entries unlinkable by factoryd's own UID --
// on the host afterward regardless.
func TestRunLiveDockerReclaimsWorkDirAfterForcedTeardown(t *testing.T) {
	if os.Getenv("DOCKER_SANDBOX_LIVE") != "1" {
		t.Skip("set DOCKER_SANDBOX_LIVE=1 to run the real Docker acceptance test")
	}
	liveRoot := os.Getenv("DOCKER_SANDBOX_LIVE_ROOT")
	if liveRoot == "" {
		liveRoot = os.TempDir()
	}
	workerUID := DefaultWorkerUID
	if workerUID == os.Getuid() {
		// Same guard as TestRunLiveDockerWithHardenedResourceLimits's own
		// worker-UID-separation sibling in cmd/factoryd: this test's whole
		// premise -- proving factoryd's own UID can reclaim a directory a
		// DIFFERENT UID made restrictive -- is moot if they happen to
		// collide in whatever environment runs it.
		t.Skipf("DefaultWorkerUID (%d) equals this test process's own UID; cannot exercise UID separation here", workerUID)
	}
	workDir, err := os.MkdirTemp(liveRoot, "factoryd-sandbox-live-reclaim-")
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(workDir) })
	if err := os.Chmod(workDir, 0o777); err != nil {
		t.Fatalf("chmod workspace: %v", err)
	}
	logDir, err := os.MkdirTemp(liveRoot, "factoryd-sandbox-live-reclaim-log-")
	if err != nil {
		t.Fatalf("create log dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(logDir) })

	spec := LaunchSpec{
		Image:       os.Getenv("DOCKER_SANDBOX_IMAGE"),
		WorkDir:     workDir,
		LogPath:     filepath.Join(logDir, "worker.log"),
		Name:        "factoryd-worker-reclaim-live-test",
		User:        fmt.Sprintf("%d:%d", workerUID, os.Getgid()),
		WorkerUmask: "0002",
		Command: []string{"/bin/sh", "-c",
			"set -e; mkdir restricted-dir; echo nested > restricted-dir/nested.txt; chmod 0700 restricted-dir; sleep 300",
		},
		Memory: "512m", CPUs: "1", TmpfsSize: "64m",
		Timeout: 15 * time.Second, // well short of the 300s sleep above -- forces Run's own timeout-cancellation path
		Network: "none",
		RunID:   "reclaim-live",
		DataDir: t.TempDir(),
	}
	if spec.Image == "" {
		spec.Image = resolveDigestForLiveTest(t, "golang:1.26-bookworm")
	}

	if _, err := Run(context.Background(), "docker", spec); err == nil {
		t.Fatal("Run() succeeded, want a timeout error (the command sleeps well past spec.Timeout)")
	}

	restrictedDir := filepath.Join(workDir, "restricted-dir")
	info, statErr := os.Stat(restrictedDir)
	if statErr != nil {
		t.Fatalf("stat %s: %v", restrictedDir, statErr)
	}
	if info.Mode().Perm()&0o020 == 0 {
		t.Errorf("restricted-dir mode = %o, want group-write reclaimed despite the worker's own 0700 chmod and this run's forced (timeout) teardown", info.Mode().Perm())
	}
	// factoryd's own later cleanup, a different UID relying on group
	// membership rather than ownership, must now be able to unlink an
	// entry inside the reclaimed directory -- the concrete operation the
	// unreclaimed bug this test guards against would have denied.
	if err := os.Remove(filepath.Join(restrictedDir, "nested.txt")); err != nil {
		t.Errorf("could not unlink inside the reclaimed directory: %v", err)
	}
}

// TestRunLiveDockerPassesMountVisibilityForNormalWorkDir is the live-Docker
// acceptance test proving verifyWorkDirMountVisibility passes cleanly, end
// to end through the real Run entry point, for the ordinary case: a
// WorkDir this session's own Docker backend genuinely does share into its
// containers (the same DOCKER_SANDBOX_LIVE_ROOT convention every other
// live test in this file already relies on for exactly this reason -- see
// its own doc comment). A regression here would mean the probe itself
// blocks completely legitimate launches, not just the invisible-mount case
// it exists to catch.
func TestRunLiveDockerPassesMountVisibilityForNormalWorkDir(t *testing.T) {
	if os.Getenv("DOCKER_SANDBOX_LIVE") != "1" {
		t.Skip("set DOCKER_SANDBOX_LIVE=1 to run the real Docker acceptance test")
	}
	liveRoot := os.Getenv("DOCKER_SANDBOX_LIVE_ROOT")
	if liveRoot == "" {
		liveRoot = os.TempDir()
	}
	workDir, err := os.MkdirTemp(liveRoot, "factoryd-sandbox-live-mount-probe-ok-")
	if err != nil {
		t.Fatalf("create workdir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(workDir) })
	if err := os.Chmod(workDir, 0o777); err != nil {
		t.Fatalf("chmod workspace: %v", err)
	}

	spec := validSpec()
	spec.Image = os.Getenv("DOCKER_SANDBOX_IMAGE")
	if spec.Image == "" {
		spec.Image = resolveDigestForLiveTest(t, "golang:1.26-bookworm")
	}
	spec.WorkDir = workDir
	spec.User = fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
	spec.InputDir = ""
	logDir, err := os.MkdirTemp(liveRoot, "factoryd-sandbox-log-mount-probe-ok-")
	if err != nil {
		t.Fatalf("create log dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(logDir) })
	spec.LogPath = filepath.Join(logDir, "worker.log")
	spec.DataDir = t.TempDir()
	spec.Command = []string{"/bin/sh", "-c", "echo mount-visibility-ok"}

	result, err := Run(context.Background(), "docker", spec)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.ExitCode != 0 {
		contents, _ := os.ReadFile(spec.LogPath)
		t.Fatalf("exit code = %d, want 0; log=%s", result.ExitCode, contents)
	}
}

// TestMountVisibilityProbeCatchesRealInvisibleMountLiveDocker is the
// live-Docker regression test for the actual bug this session found: on a
// Docker backend that does not share a given host path into its
// containers (colima's default only shares $HOME, not /tmp or
// /private/tmp), a container bind-mounting that path sees an EMPTY
// directory tree rather than an error -- `docker run` accepts the
// --volume flag without complaint. This reproduces that exact shape
// without depending on the test host's own Docker backend actually having
// the colima gap: it bind-mounts a genuinely different, empty directory
// (dirB) at the same container path a real probe would use, then checks
// for a marker that only exists in a second directory (dirA) -- exactly
// what happens when a backend silently substitutes different (or no)
// content for the host path it claims to share. Confirms `test -f`
// against that mismatched mount fails with exit 1, real-engine proof of
// the exact signal verifyWorkDirMountVisibility's own exit-code-1 branch
// (unit-tested in TestVerifyWorkDirMountVisibilityFailsClosedWhenMarkerInvisible)
// treats as a confirmed, fail-closed mount-visibility failure.
func TestMountVisibilityProbeCatchesRealInvisibleMountLiveDocker(t *testing.T) {
	if os.Getenv("DOCKER_SANDBOX_LIVE") != "1" {
		t.Skip("set DOCKER_SANDBOX_LIVE=1 to run the real Docker acceptance test")
	}
	liveRoot := os.Getenv("DOCKER_SANDBOX_LIVE_ROOT")
	if liveRoot == "" {
		liveRoot = os.TempDir()
	}
	dirA, err := os.MkdirTemp(liveRoot, "factoryd-sandbox-live-mount-probe-a-")
	if err != nil {
		t.Fatalf("create dirA: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dirA) })
	dirB, err := os.MkdirTemp(liveRoot, "factoryd-sandbox-live-mount-probe-b-")
	if err != nil {
		t.Fatalf("create dirB: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dirB) })

	marker := "marker-only-in-dirA"
	if err := os.WriteFile(filepath.Join(dirA, marker), []byte("present\n"), 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	// dirB deliberately stays empty -- standing in for the content a
	// container actually sees when its backend does not really share the
	// host path it was told to.

	image := os.Getenv("DOCKER_SANDBOX_IMAGE")
	if image == "" {
		image = resolveDigestForLiveTest(t, "golang:1.26-bookworm")
	}

	run := func(mountSource string) int {
		cmd := exec.Command("docker", "run", "--rm", "--network", "none",
			"--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()),
			"--volume", mountSource+":"+mountVisibilityProbePath+":ro",
			"--entrypoint", "/bin/sh", image,
			"-c", "test -f "+shSingleQuote(mountVisibilityProbePath+"/"+marker))
		if out, err := cmd.CombinedOutput(); err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				return exitErr.ExitCode()
			}
			t.Fatalf("docker run: %v: %s", err, out)
		}
		return 0
	}

	if code := run(dirA); code != 0 {
		t.Fatalf("probe against dirA (has the marker) exit code = %d, want 0", code)
	}
	if code := run(dirB); code != 1 {
		t.Fatalf("probe against dirB (empty -- simulating an invisible mount) exit code = %d, want 1 (the exact signal verifyWorkDirMountVisibility treats as a confirmed mount-visibility failure)", code)
	}
}

// TestDockerCommandPassesUnrecordedEnvironmentByNameOnly: an operator
// value (compose_services_worker_env, possibly holding a sidecar password)
// must not appear in the argv Result.Command records in run.json and in
// Temporal activity results, and its flag must precede every factory
// --env so a duplicate could never replace a factory value.
func TestDockerCommandPassesUnrecordedEnvironmentByNameOnly(t *testing.T) {
	s := validSpec()
	s.UnrecordedEnvironment = []string{"PSQL_URL=postgres://user:secret@database:5432/db"}
	args := s.DockerCommand("docker")
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "secret") {
		t.Fatalf("docker argv carries the unrecorded value: %v", args)
	}
	named := strings.Index(joined, "--env PSQL_URL ")
	path := strings.Index(joined, "--env PATH=")
	if named < 0 || path < 0 || named > path {
		t.Fatalf("docker argv must pass --env PSQL_URL before the factory's --env flags: %v", args)
	}
	if err := s.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestValidateRejectsUnrecordedEnvironmentOverridingTheFactory(t *testing.T) {
	for _, entry := range []string{"HOME=/tmp", "PATH=/evil", "GIT_CONFIG_VALUE_0=x", "GITHUB_TOKEN=x", "NOEQUALS"} {
		s := validSpec()
		s.Environment = append(s.Environment, "HOME=/home/worker")
		s.UnrecordedEnvironment = []string{entry}
		if err := s.Validate(); err == nil {
			t.Errorf("Validate accepted UnrecordedEnvironment %q", entry)
		}
	}
}
