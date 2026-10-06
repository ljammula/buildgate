package workflow

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"go.temporal.io/sdk/testsuite"

	"buildgate/internal/oraclecanary"
	"buildgate/internal/run"
	"buildgate/internal/runner"
)

const canaryTestOracleSrc = "package p\n\nimport \"testing\"\n\nfunc TestOracleLevel(t *testing.T) {}\n"

// fakeDockerScript is a fake docker: the real-oracle run exits realExit; the
// canary run (its snapshot is mounted) prints canaryOutput -- or, when empty,
// extracts this invocation's nonce from the mounted canary source and prints the
// marker the way an executing test would -- and exits canaryExit. Every run
// first sleeps sleepSecs.
func fakeDockerScript(argLog string, realExit, canaryExit int, canaryOutput, sleepSecs string) string {
	printCanary := fmt.Sprintf("echo %q", canaryOutput)
	if canaryOutput == "" {
		printCanary = `cat "$d"/*_test.go | sed -n 's/.*t.Fatal("oracle canary \([0-9a-f]*\)" + "\([0-9a-f]*\): this test must fail").*/oracle canary \1\2: this test must fail/p'`
	}
	return fmt.Sprintf("#!/bin/sh\necho \"$@\" >> %q\nif [ \"$1\" = run ]; then\n  sleep %s\n  case \"$*\" in\n    *reference-oracle-canary-snapshot*)\n      for a in \"$@\"; do case \"$a\" in *reference-oracle-canary-snapshot:*) d=\"${a%%%%:*}\";; esac; done\n      %s; exit %d;;\n    *reference-oracle-snapshot*) exit %d;;\n  esac\nfi\nexit 0\n", argLog, sleepSecs, printCanary, canaryExit, realExit)
}

// canaryGate runs the reference_oracle gate Activity against a fake docker
// whose real-oracle run exits realExit and whose canary run (recognised by the
// canary snapshot being mounted) exits canaryExit. It returns the result, the
// docker argument log, and the Activities (for its LogDir).
func canaryGate(t *testing.T, files map[string]string, realExit, canaryExit int) (VerifyActivityResult, string, *Activities) {
	t.Helper()
	return canaryGateWithOutput(t, files, realExit, canaryExit, "")
}

// canaryGateWithOutput is canaryGate with the text the canary run prints; ""
// means what a real canary test prints: the marker for the nonce this invocation
// embedded in the mounted canary source.
func canaryGateWithOutput(t *testing.T, files map[string]string, realExit, canaryExit int, canaryOutput string) (VerifyActivityResult, string, *Activities) {
	t.Helper()
	oracleDir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(oracleDir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	argLog := filepath.Join(t.TempDir(), "docker-args.log")
	docker := filepath.Join(t.TempDir(), "docker-fake")
	script := fakeDockerScript(argLog, realExit, canaryExit, canaryOutput, "0")
	if err := os.WriteFile(docker, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	activities := &Activities{LogDir: t.TempDir(), SandboxDocker: docker, DataDir: t.TempDir()}
	input := NamedGateActivityInput{
		RunWorkflowInput: RunWorkflowInput{
			Ticket:                   "fixture-ticket",
			WorkspacePath:            t.TempDir(),
			SandboxImage:             "factory-worker:test@sha256:deadbeef",
			SandboxDocker:            docker,
			RunID:                    "run-id",
			DataDir:                  activities.DataDir,
			ReferenceOracleDir:       oracleDir,
			ReferenceOracleMountPath: ".oracle",
		},
		Check: "reference_oracle", OracleCanary: true,
		Command: "go test -run TestOracleLevel",
	}
	wrapper := func(ctx context.Context, in NamedGateActivityInput) (VerifyActivityResult, error) {
		return activities.RunNamedGateActivity(ctx, in)
	}
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(wrapper)
	raw, err := env.ExecuteActivity(wrapper, input)
	if err != nil {
		t.Fatalf("execute reference_oracle gate Activity: %v", err)
	}
	var result VerifyActivityResult
	if err := raw.Get(&result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	logged, _ := os.ReadFile(argLog)
	return result, string(logged), activities
}

func canaryAttempts(r VerifyActivityResult) []run.Attempt {
	var out []run.Attempt
	for _, a := range r.Attempts {
		if a.Kind == oraclecanary.AttemptKind {
			out = append(out, a)
		}
	}
	return out
}

func gateLog(t *testing.T, r VerifyActivityResult) string {
	t.Helper()
	b, err := os.ReadFile(r.Result.LogPath)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestReferenceOracleGateRunsCanaryAndTrustsOnlyAFailingCanary(t *testing.T) {
	files := map[string]string{"p_oracle_test.go": canaryTestOracleSrc, "MANIFEST.json": "{}"}
	r, dockerArgs, acts := canaryGate(t, files, 0, 1)
	if r.Result.ExitCode != 0 {
		t.Fatalf("trustworthy oracle command failed the gate: exit=%d", r.Result.ExitCode)
	}
	if r.OracleCanary == nil || r.OracleCanary.Verdict != string(oraclecanary.VerdictTrustworthy) || r.OracleCanary.Ecosystem != "go" {
		t.Fatalf("OracleCanary = %+v", r.OracleCanary)
	}
	if got := canaryAttempts(r); len(got) != 1 || got[0].ExitCode != 1 {
		t.Fatalf("canary attempts = %+v", got)
	}
	if !strings.Contains(gateLog(t, r), "[oracle canary] verdict=TRUSTWORTHY") {
		t.Errorf("gate log lacks the canary note:\n%s", gateLog(t, r))
	}
	// Same mount path for both runs, read-only, and the scratch snapshot is gone.
	var mounts []string
	for _, line := range strings.Split(dockerArgs, "\n") {
		if strings.HasPrefix(line, "run ") && strings.Contains(line, "-snapshot") {
			for _, f := range strings.Fields(line) {
				if strings.Contains(f, "-snapshot:") {
					mounts = append(mounts, f[strings.Index(f, "-snapshot:"):])
				}
			}
		}
	}
	if len(mounts) != 2 || !strings.HasSuffix(mounts[0], ":/workspace/.oracle:ro") || !strings.HasSuffix(mounts[1], ":/workspace/.oracle:ro") {
		t.Errorf("real and canary runs must mount read-only at the same path; mounts = %v", mounts)
	}
	if _, err := os.Stat(filepath.Join(acts.LogDir, "reference-oracle-canary-snapshot")); !os.IsNotExist(err) {
		t.Errorf("canary snapshot survived the Activity: %v", err)
	}
}

// A command that runs no oracle test passes on the canary too: FALSE_PASS.
func TestReferenceOracleGateFailsClosedWhenTheCommandNeverRunsTheOracle(t *testing.T) {
	files := map[string]string{"p_oracle_test.go": canaryTestOracleSrc}
	r, _, _ := canaryGate(t, files, 0, 0)
	if r.Result.ExitCode == 0 {
		t.Fatal("a command that passes on the canary was accepted")
	}
	if r.OracleCanary == nil || r.OracleCanary.Verdict != string(oraclecanary.VerdictFalsePass) {
		t.Fatalf("OracleCanary = %+v", r.OracleCanary)
	}
	if !strings.Contains(gateLog(t, r), "FALSE_PASS") {
		t.Errorf("gate log lacks the reason:\n%s", gateLog(t, r))
	}
}

// A canary that died of an environment error must not vouch for the command.
func TestReferenceOracleGateFailsClosedOnInconclusiveCanary(t *testing.T) {
	files := map[string]string{"p_oracle_test.go": canaryTestOracleSrc}
	r, _, _ := canaryGate(t, files, 0, 137)
	if r.Result.ExitCode == 0 || r.OracleCanary == nil || r.OracleCanary.Verdict != string(oraclecanary.VerdictCanaryInconclusive) {
		t.Fatalf("exit=%d canary=%+v", r.Result.ExitCode, r.OracleCanary)
	}
}

// An oracle the canary cannot be built for is not silently passed.
func TestReferenceOracleGateFailsClosedWhenNoCanaryCanBeBuilt(t *testing.T) {
	files := map[string]string{"notes.rb": "puts 1\n"}
	r, dockerArgs, _ := canaryGate(t, files, 0, 1)
	if r.Result.ExitCode == 0 {
		t.Fatal("an oracle with no possible canary was accepted")
	}
	if r.OracleCanary == nil || r.OracleCanary.Verdict != oraclecanary.UnsupportedVerdict {
		t.Fatalf("OracleCanary = %+v", r.OracleCanary)
	}
	if len(canaryAttempts(r)) != 0 || strings.Contains(dockerArgs, "canary-snapshot") {
		t.Errorf("a canary run happened without a snapshot: %+v", canaryAttempts(r))
	}
}

// The canary is a second run only after a real pass: a failing oracle command
// stays an ordinary failure with no canary evidence.
func TestReferenceOracleGateSkipsCanaryWhenTheOracleFails(t *testing.T) {
	files := map[string]string{"p_oracle_test.go": canaryTestOracleSrc}
	r, dockerArgs, _ := canaryGate(t, files, 1, 1)
	if r.Result.ExitCode == 0 || r.OracleCanary != nil || len(canaryAttempts(r)) != 0 || strings.Contains(dockerArgs, "canary-snapshot") {
		t.Fatalf("exit=%d canary=%+v", r.Result.ExitCode, r.OracleCanary)
	}
}

// A command that merely INSPECTS the oracle fails on the canary file without the
// canary test ever running: no marker in the output, so it is not trusted.
func TestReferenceOracleGateFailsClosedWhenTheCanaryFailedWithoutRunning(t *testing.T) {
	files := map[string]string{"p_oracle_test.go": canaryTestOracleSrc}
	r, _, _ := canaryGateWithOutput(t, files, 0, 1, "grep: pattern found")
	if r.Result.ExitCode == 0 {
		t.Fatal("a command that inspects the oracle instead of running it was accepted")
	}
	if r.OracleCanary == nil || r.OracleCanary.Verdict != string(oraclecanary.VerdictCanaryNotExecuted) {
		t.Fatalf("OracleCanary = %+v", r.OracleCanary)
	}
	if !strings.Contains(gateLog(t, r), "inspect the oracle rather than execute it") {
		t.Errorf("gate log lacks the reason:\n%s", gateLog(t, r))
	}
}

// Each run gets its OWN half of the Activity's budget. With one shared absolute
// deadline, a real run taking 0.6 of the budget left the canary 0.4 and timed it
// out, halting the run for a good command (false block).
func TestReferenceOracleCanaryGetsItsOwnTimeoutWindow(t *testing.T) {
	oracleDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(oracleDir, "p_oracle_test.go"), []byte(canaryTestOracleSrc), 0o600); err != nil {
		t.Fatal(err)
	}
	const budget = 3 * time.Second      // whole Activity deadline; each run gets half = 1.5s
	const step = 900 * time.Millisecond // 0.6 of half
	calls := 0
	a := &Activities{LogDir: t.TempDir(), DataDir: t.TempDir()}
	nonceRe := regexp.MustCompile(`t\.Fatal\("oracle canary ([0-9a-f]+)" \+ "([0-9a-f]+): this test must fail"\)`)
	a.runWithRetriesChecked = func(ctx context.Context, _ string, logPath func(int) string, _ int, before func(int) error, after func(int, runner.Result, error) error, _ string, args ...string) (runner.Result, error) {
		calls++
		canary := calls == 2
		if err := before(1); err != nil {
			return runner.Result{}, err
		}
		select {
		case <-time.After(step):
		case <-ctx.Done():
			return runner.Result{}, ctx.Err()
		}
		log := logPath(1)
		if err := os.MkdirAll(filepath.Dir(log), 0o750); err != nil {
			return runner.Result{}, err
		}
		body, exit := "ok\n", 0
		if canary {
			// What the executing canary test prints: the marker for the nonce
			// embedded in the canary snapshot this Activity just built.
			src, err := os.ReadFile(filepath.Join(a.LogDir, "reference-oracle-canary-snapshot", "p_oracle_test.go"))
			if err != nil {
				return runner.Result{}, err
			}
			m := nonceRe.FindStringSubmatch(string(src))
			if m == nil {
				return runner.Result{}, fmt.Errorf("no nonce in canary source:\n%s", src)
			}
			body, exit = "oracle canary "+m[1]+m[2]+": this test must fail\n", 1
		}
		if err := os.WriteFile(log, []byte(body), 0o600); err != nil {
			return runner.Result{}, err
		}
		res := runner.Result{Command: []string{"sh", "-c", args[len(args)-1]}, StartedAt: time.Now(), FinishedAt: time.Now(), ExitCode: exit, LogPath: log}
		if err := after(1, res, nil); err != nil {
			return runner.Result{}, err
		}
		return res, nil
	}
	input := NamedGateActivityInput{
		RunWorkflowInput: RunWorkflowInput{Ticket: "t", WorkspacePath: t.TempDir(), RunID: "run-id", DataDir: a.DataDir, LogDir: a.LogDir, ReferenceOracleDir: oracleDir, ReferenceOracleMountPath: ".oracle"},
		Check:            "reference_oracle", OracleCanary: true,
		Command: "go test -run TestOracleLevel",
	}
	wrapper := func(ctx context.Context, in NamedGateActivityInput) (VerifyActivityResult, error) {
		ctx, cancel := context.WithTimeout(ctx, budget)
		defer cancel()
		return a.RunNamedGateActivity(ctx, in)
	}
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(wrapper)
	raw, err := env.ExecuteActivity(wrapper, input)
	if err != nil {
		t.Fatalf("canary timed out or failed although each run used 0.6 of its own half: %v", err)
	}
	var res VerifyActivityResult
	if err := raw.Get(&res); err != nil {
		t.Fatal(err)
	}
	if res.Result.ExitCode != 0 || res.OracleCanary == nil || res.OracleCanary.Verdict != string(oraclecanary.VerdictTrustworthy) {
		t.Fatalf("exit=%d canary=%+v", res.Result.ExitCode, res.OracleCanary)
	}
}
