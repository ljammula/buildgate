package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	temporalworkflow "go.temporal.io/sdk/workflow"

	"buildgate/internal/codereview"
	"buildgate/internal/hostcontrol"
	"buildgate/internal/openshell"
	"buildgate/internal/run"
	"buildgate/internal/sessionconfig"
)

// The live proof of SC-022's reproduction run: for each recorded case under
// testdata/live_reproduction/cases, a fresh clone of the case's repository
// under the data root (the only host directory the VM shares read-write) is
// checked out at the result commit, the review's recorded finding is written
// as the review's evidence with the case's spec citation and reproduction,
// and the production code does the rest: holdReproductions (the shape rules
// and the hold), reproduceReviewFindings (head1, head2, base through
// RunReviewReproductionActivity, launched through the real OpenShell gateway
// with the case's recorded image, registry proxy and test patterns) and
// reproduction.Decide. Nothing of the tree runs on the host.
//
// It runs only with REPRO_LIVE=1, after `factoryd doctor -fix` has started
// the stack, one case at a time; REPRO_LIVE_CASE names one case. A case
// whose want_verdict is set fails when the verdict differs.

// liveReproCase is one recorded case.
type liveReproCase struct {
	Name             string          `json:"name"`
	Source           string          `json:"source"`
	RecordedRun      string          `json:"recorded_run"`
	BaseSHA          string          `json:"base_sha"`
	ResultSHA        string          `json:"result_sha"`
	Spec             string          `json:"spec"`
	SpecLine         int             `json:"spec_line"`
	SpecText         string          `json:"spec_text"`
	ReproductionPath string          `json:"reproduction_path"`
	Reproduction     string          `json:"reproduction"`
	Finding          map[string]any  `json:"finding"`
	Input            json.RawMessage `json:"input"`
	PlanExpected     string          `json:"plan_expected"`
	WantVerdict      string          `json:"want_verdict"`
}

const liveReproDir = "testdata/live_reproduction"

func TestLiveReviewReproduction(t *testing.T) {
	if os.Getenv("REPRO_LIVE") != "1" {
		t.Skip("set REPRO_LIVE=1 to run the reviewer's reproductions through the real gateway")
	}
	paths, err := filepath.Glob(filepath.Join(liveReproDir, "cases", "*.json"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no cases: %v", err)
	}
	sort.Strings(paths)
	only := os.Getenv("REPRO_LIVE_CASE")
	for _, p := range paths {
		c := loadLiveReproCase(t, p)
		if only != "" && !strings.Contains(","+only+",", ","+c.Name+",") {
			continue
		}
		t.Run(c.Name, func(t *testing.T) { runLiveReproCase(t, c) })
	}
}

func loadLiveReproCase(t *testing.T, p string) liveReproCase {
	t.Helper()
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var c liveReproCase
	if err := json.Unmarshal(data, &c); err != nil {
		t.Fatalf("%s: %v", p, err)
	}
	return c
}

func runLiveReproCase(t *testing.T, c liveReproCase) {
	acts, input := liveReproSetup(t, c)
	held, err := acts.holdReproductions(context.Background(), input)
	if err != nil {
		t.Fatalf("hold: %v", err)
	}
	for _, h := range held {
		t.Logf("held: path=%s runner=%s ids=%v spec_line=%d holds_literal=%v refused=%q", h.Path, h.Runner, h.TestIDs, h.SpecLine, h.HoldsLiteral, h.Refused)
	}
	if len(held) != 1 || held[0].Refused != "" {
		t.Fatalf("held = %+v; want one accepted reproduction", held)
	}
	findings := liveReproduce(t, acts, input, held, c.ResultSHA)
	if len(findings) != 1 {
		t.Fatalf("findings = %+v", findings)
	}
	rep := findings[0].Reproduction
	for _, r := range rep.Runs {
		t.Logf("pass %-5s commit=%s outcome=%s tests=%v exit=%d log=%s", r.Pass, r.Commit[:12], r.Outcome, r.Tests, r.ExitCode, r.LogPath)
	}
	t.Logf("VERDICT %s: %s (%s); disposition=%s; plan expected %s", c.Name, rep.Verdict, rep.Reason, findings[0].Disposition, c.PlanExpected)
	writeLiveReproResult(t, acts.LogDir, findings)
	if c.WantVerdict != "" && rep.Verdict != c.WantVerdict {
		t.Errorf("verdict = %s, want %s", rep.Verdict, c.WantVerdict)
	}
}

// liveReproSetup clones the case's repository at the result commit, writes
// the review's evidence and the approved spec, and builds Activities over
// the real gateway and the run's input from the case's recorded input.
func liveReproSetup(t *testing.T, c liveReproCase) (*Activities, RunWorkflowInput) {
	t.Helper()
	root := filepath.Join(sessionconfig.DataRoot(), "p2-proof", fmt.Sprintf("%s-%d", c.Name, time.Now().Unix()))
	repo := filepath.Join(root, "repo")
	t.Logf("case root: %s", root)
	home, _ := os.UserHomeDir()
	source := strings.Replace(c.Source, "~", home, 1)
	liveGit(t, "", "clone", "-q", "--no-hardlinks", "--no-checkout", source, repo)
	liveGit(t, repo, "checkout", "-q", "--detach", c.ResultSHA)
	content, err := os.ReadFile(filepath.Join(liveReproDir, c.Reproduction))
	if err != nil {
		t.Fatal(err)
	}
	c.Finding["spec_line"] = map[string]any{"number": c.SpecLine, "text": c.SpecText}
	c.Finding["reproduction"] = map[string]any{"path": c.ReproductionPath, "content": string(content)}
	evidence, err := json.Marshal(map[string]any{"schema_version": 1, "review_policy": "advisory", "available": true, "findings": []any{c.Finding}})
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(repo, codereview.EvidenceFile), string(evidence))
	spec, err := os.ReadFile(filepath.Join(liveReproDir, c.Spec))
	if err != nil {
		t.Fatal(err)
	}
	specPath := filepath.Join(root, "approved-spec.md")
	writeFile(t, specPath, string(spec))

	runID := "p2-proof-" + filepath.Base(root)
	dataDir := filepath.Join(root, "data")
	acts := &Activities{
		DataDir: dataDir, LogDir: run.Dir(dataDir, runID), CheckpointDir: filepath.Join(root, "checkpoints"),
		Sandboxes: liveGatewayRuntime(t), MeterLedgerRoot: filepath.Join(root, "ledgers"), SandboxDocker: "docker",
	}
	for _, dir := range []string{acts.LogDir, acts.CheckpointDir, acts.MeterLedgerRoot} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	var input RunWorkflowInput
	if err := json.Unmarshal(c.Input, &input); err != nil {
		t.Fatalf("recorded input: %v", err)
	}
	input.WorkspacePath, input.BaseSHA, input.InstructionBaseSHA, input.ProjectConfigCommitSHA = repo, c.BaseSHA, c.BaseSHA, c.BaseSHA
	input.RunID, input.DataDir, input.LogDir, input.CheckpointDir = runID, acts.DataDir, acts.LogDir, acts.CheckpointDir
	input.CodeReviewPolicy, input.ReviewReproduction = "advisory", true
	input.ReviewSpecPath, input.ReviewSpecSHA256 = specPath, sha256Of(spec)
	return acts, input
}

// liveGatewayRuntime connects to this machine's gateway as the worker does.
func liveGatewayRuntime(t *testing.T) *openshell.Runtime {
	t.Helper()
	stack, err := hostcontrol.OpenShellStackPath()
	if err != nil {
		t.Fatal(err)
	}
	client, err := openshell.Connect(hostcontrol.OpenShellGatewayAddr, hostcontrol.OpenShellMTLSDir(stack))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	readiness, err := openshell.ConnectReadiness(hostcontrol.OpenShellGatewayAddr, hostcontrol.OpenShellMTLSDir(stack))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = readiness.Close() })
	return &openshell.Runtime{Client: client, Containers: &openshell.DockerContainers{MeterProject: hostcontrol.OpenShellProject},
		Volumes: &openshell.DockerVolumes{}, Readiness: readiness, Warn: t.Logf}
}

// liveReproduce runs the workflow's reproduction phase
// (reproduceReviewFindings) in a test workflow environment whose
// reproduction Activity is the real one.
func liveReproduce(t *testing.T, acts *Activities, input RunWorkflowInput, held []run.HeldReproduction, resultSHA string) []run.FindingReproduction {
	t.Helper()
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.SetTestTimeout(90 * time.Minute)
	env.RegisterActivityWithOptions(acts.RunReviewReproductionActivity, activity.RegisterOptions{Name: RunReviewReproductionActivityName})
	env.RegisterWorkflowWithOptions(liveReproductionWorkflow, temporalworkflow.RegisterOptions{Name: "liveReproductionWorkflow"})
	env.ExecuteWorkflow("liveReproductionWorkflow", input, held, resultSHA)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("reproduction phase: %v", err)
	}
	var findings []run.FindingReproduction
	if err := env.GetWorkflowResult(&findings); err != nil {
		t.Fatal(err)
	}
	return findings
}

// liveReproductionWorkflow calls the phase with RunWorkflow's build/verify
// Activity options and explicit Activity IDs.
func liveReproductionWorkflow(ctx temporalworkflow.Context, input RunWorkflowInput, held []run.HeldReproduction, resultSHA string) ([]run.FindingReproduction, error) {
	buildVerifyCtx := temporalworkflow.WithActivityOptions(ctx, temporalworkflow.ActivityOptions{
		StartToCloseTimeout: time.Hour, HeartbeatTimeout: stepHeartbeatTimeout,
		RetryPolicy: &temporal.RetryPolicy{MaximumAttempts: 1},
	})
	ids := &activityIDs{explicit: true, seen: map[string]int{}}
	result := &RunWorkflowResult{ResultSHA: resultSHA, BaseSHA: input.BaseSHA}
	return reproduceReviewFindings(ctx, ids, buildVerifyCtx, input, &RunProgress{}, result, held, input.WorkspacePath, "")
}

func liveGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	if dir != "" {
		args = append([]string{"-C", dir}, args...)
	}
	if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

// writeLiveReproResult keeps the phase's record beside the run's logs.
func writeLiveReproResult(t *testing.T, logDir string, findings []run.FindingReproduction) {
	t.Helper()
	data, err := json.MarshalIndent(findings, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(logDir, "review-reproduction.json"), string(data)+"\n")
	t.Logf("record: %s", filepath.Join(logDir, "review-reproduction.json"))
}
