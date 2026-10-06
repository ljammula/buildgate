package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"buildgate/internal/oraclecanary"
	"buildgate/internal/run"
)

// canaryDockerWrapper wraps testdata/fake_docker.sh so the reference-oracle
// mount's HOST directory is visible to the gate command as $ORACLE_HOST (the
// fake docker only translates a whole argv element, never the inside of a
// `sh -c` string). Whichever snapshot is mounted at /workspace/verify is what
// $ORACLE_HOST names, so the same command sees the real oracle on the real run
// and the canary on the canary run.
func canaryDockerWrapper(t *testing.T) string {
	t.Helper()
	script := fmt.Sprintf("#!/bin/sh\nfor a in \"$@\"; do\n  case \"$a\" in\n    *:/workspace/verify:ro) ORACLE_HOST=\"${a%%%%:*}\"; export ORACLE_HOST;;\n  esac\ndone\nexec %q \"$@\"\n", fakeSandboxDockerBinary(t))
	path := filepath.Join(t.TempDir(), "docker-canary-wrapper")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

// canaryDockerEnv points a factoryd subprocess at canaryDockerWrapper.
func canaryDockerEnv(t *testing.T) []string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not on PATH: the oracle command really runs `go test`")
	}
	env := isolatedSessionConfigEnv(t, "sandbox_docker: "+canaryDockerWrapper(t)+"\nsandbox_image: "+fakeSandboxImage+"\n")
	// The isolated HOME would give `go test` a cold build cache.
	if out, err := exec.Command("go", "env", "GOCACHE").Output(); err == nil {
		env = append(env, "GOCACHE="+strings.TrimSpace(string(out)))
	}
	return env
}

// executingOracleCommand REALLY runs the mounted oracle file with go test in a
// scratch module, so the canary's own t.Fatal executes on the canary run.
func executingOracleCommand(file string) string {
	return `d=$(mktemp -d) && cd "$d" && printf 'module m\n\ngo 1.21\n' > go.mod && cp "$ORACLE_HOST/` + file + `" . && go test -count=1 -run TestOracle .`
}

// inspectingOracleCommand only READS the oracle file: it "fails" on the canary
// file's content without any test executing, so it must not be trusted.
func inspectingOracleCommand(file string) string {
	return `! grep -q 't.Fatal' "$ORACLE_HOST/` + file + `"`
}

// trustworthyOracleCommand executes the mounted oracle, so the runtime canary
// trusts it. Needs canaryDockerEnv.
var trustworthyOracleCommand = executingOracleCommand("x_oracle_test.go")

// writeTrustworthyOracle writes an oracle directory the canary can be built for
// and trustworthyOracleCommand executes.
func writeTrustworthyOracle(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	body := "package verify\n\nimport \"testing\"\n\nfunc TestOracleX(t *testing.T) {}\n"
	if err := os.WriteFile(filepath.Join(dir, "x_oracle_test.go"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// runOracleCanaryGate runs a real bare run with an oracle directory and a
// -reference-oracle-command, with the fake docker above.
func runOracleCanaryGate(t *testing.T, files map[string]string, command string) *run.Run {
	t.Helper()
	ws := newFixtureRepo(t)
	oracleDir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(oracleDir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	env := canaryDockerEnv(t)
	return runFactorydWithSpecAndFlags(t, ws, "commit", "true", "", "120s", env, []string{
		"-reference-oracle-dir", oracleDir,
		"-reference-oracle-mount-path", "verify",
		"-reference-oracle-command", command,
	})
}

func gatePassed(r *run.Run, check string) (passed, found bool) {
	for _, g := range r.GateResults {
		if g.Check == check {
			return g.Passed, true
		}
	}
	return false, false
}

func attemptCount(r *run.Run, kind string) int {
	n := 0
	for _, a := range r.Attempts {
		if a.Kind == kind {
			n++
		}
	}
	return n
}

const canaryGoOracle = "package p\n\nimport \"testing\"\n\nfunc TestOracleLevel(t *testing.T) {}\n"

func TestIntegrationDirectReferenceOracleCanaryTrustsCommandThatExecutesTheOracle(t *testing.T) {
	r := runOracleCanaryGate(t, map[string]string{"p_oracle_test.go": canaryGoOracle}, executingOracleCommand("p_oracle_test.go"))
	if r.State != run.StateAccepted {
		t.Fatalf("state = %q, want accepted", r.State)
	}
	if r.OracleCanary == nil || r.OracleCanary.Verdict != string(oraclecanary.VerdictTrustworthy) {
		t.Fatalf("OracleCanary = %+v", r.OracleCanary)
	}
	if attemptCount(r, oraclecanary.AttemptKind) != 1 {
		t.Errorf("canary attempts = %d, want 1", attemptCount(r, oraclecanary.AttemptKind))
	}
}

func TestIntegrationDirectReferenceOracleCanaryQuarantinesAVacuousCommand(t *testing.T) {
	// `true` never looks at the oracle: passes on the canary too.
	r := runOracleCanaryGate(t, map[string]string{"p_oracle_test.go": canaryGoOracle}, "true")
	if r.State != run.StateQuarantined {
		t.Fatalf("state = %q, want quarantined", r.State)
	}
	if passed, found := gatePassed(r, "reference_oracle"); !found || passed {
		t.Errorf("reference_oracle gate found=%v passed=%v, want a failed gate", found, passed)
	}
	if r.OracleCanary == nil || r.OracleCanary.Verdict != string(oraclecanary.VerdictFalsePass) {
		t.Fatalf("OracleCanary = %+v", r.OracleCanary)
	}
	for _, a := range r.Attempts {
		if a.Kind != "reference_oracle" {
			continue
		}
		log, err := os.ReadFile(a.LogPath)
		if err != nil || !strings.Contains(string(log), "[oracle canary] verdict=FALSE_PASS") {
			t.Errorf("gate log lacks the canary verdict (err=%v):\n%s", err, log)
		}
	}
}

func TestIntegrationDirectReferenceOracleCanaryQuarantinesAnUnsupportedOracle(t *testing.T) {
	r := runOracleCanaryGate(t, map[string]string{"notes.rb": "puts 1\n"}, "true")
	if r.State != run.StateQuarantined {
		t.Fatalf("state = %q, want quarantined", r.State)
	}
	if r.OracleCanary == nil || r.OracleCanary.Verdict != oraclecanary.UnsupportedVerdict {
		t.Fatalf("OracleCanary = %+v", r.OracleCanary)
	}
	if attemptCount(r, oraclecanary.AttemptKind) != 0 {
		t.Error("a canary ran without a snapshot")
	}
}

// A command that reads the oracle instead of running it used to be judged
// trustworthy (the canary file made its check fail). It must now be blocked.
func TestIntegrationDirectReferenceOracleCanaryBlocksACommandThatInspectsInsteadOfExecutes(t *testing.T) {
	r := runOracleCanaryGate(t, map[string]string{"p_oracle_test.go": canaryGoOracle}, inspectingOracleCommand("p_oracle_test.go"))
	if r.State != run.StateQuarantined {
		t.Fatalf("state = %q, want quarantined", r.State)
	}
	if r.OracleCanary == nil || r.OracleCanary.Verdict != string(oraclecanary.VerdictCanaryNotExecuted) {
		t.Fatalf("OracleCanary = %+v", r.OracleCanary)
	}
}
