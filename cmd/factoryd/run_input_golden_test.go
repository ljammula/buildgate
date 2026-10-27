package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	temporalclient "go.temporal.io/sdk/client"

	"buildgate/internal/run"
	"buildgate/internal/workflow"
)

// updateGoldenEnv, set to 1, rewrites the golden files instead of comparing.
const updateGoldenEnv = "FACTORYD_UPDATE_GOLDEN"

// runInputGoldenConfig is the session config of the golden runs: every key
// that reaches RunWorkflowInput is set to a value that is not its default.
func runInputGoldenConfig(t *testing.T) string {
	t.Helper()
	return "sandbox_docker: " + filepath.Join(testdataDir(t), "fake_docker.sh") + "\n" +
		"sandbox_user: \"1234:5678\"\n" +
		"routes:\n" +
		"  golden-route:\n    credential_mode: static\n    upstream: https://golden.example.invalid\n    credential_env: RUN_INPUT_GOLDEN_KEY\n" +
		"  review-route:\n    credential_mode: static\n    upstream: https://review.example.invalid\n    credential_env: RUN_INPUT_GOLDEN_KEY\n" +
		"models:\n" +
		"  builder:\n    id: builder-model-id\n    api: openai-completions\n    routes: [golden-route]\n    reasoning: true\n" +
		"  reviewer:\n    id: reviewer-model-id\n    api: openai-responses\n    routes: [review-route]\n    reasoning: true\n" +
		"roles:\n" +
		"  execution: { model: builder, thinking: medium, escalation: { model: builder, thinking: low } }\n" +
		"  review: { model: reviewer, thinking: high }\n" +
		"registry_proxy_image: registry-proxy@sha256:" + strings.Repeat("c", 64) + "\n" +
		"compose_services_memory: 3g\n" +
		"compose_services_cpus: \"1.5\"\n" +
		"compose_services_max_services: 3\n" +
		"compose_services_ready_timeout: 2m30s\n" +
		"compose_services_allowed_registries: [registry.example.invalid]\n" +
		"compose_services_require_digest: true\n" +
		"release_protected_paths: \"deploy/,.github/\"\n" +
		"release_max_files_changed: 7\n" +
		"release_max_insertions: 77\n" +
		"release_rollback_plan: revert it\n"
}

// runInputGoldenFlags is the full flag set of the golden runs: the run flags
// that reach RunWorkflowInput, each with a value that is not its default.
// -compose-services stays at its default (true), which is the value the JSON
// shows. -recheck-reproductions names a held copy under a quarantined run
// the flags write into dataDir. Not set, so absent from the golden: -prior-run,
// -on-branch, -diff-base, -resume-worktree-of and -replay (each needs an
// earlier run; TestReplayRunWorkflowInputMatchesGolden covers -replay, which
// also refuses -earlier-attempt),
// -baseline-check (it takes no ticket; TestBaselineCheckReachesTheWorkflow
// covers it), and the
// spec's Required-Content and the config's skills and test patterns.
// inputs is the directory the flag files (spec, criteria, oracle) are written to.
func runInputGoldenFlags(t *testing.T, workspace, dataDir, inputs, runID, address string) []string {
	t.Helper()
	write := func(name, content string) string {
		path := filepath.Join(inputs, name)
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		return path
	}
	oracleDir := filepath.Join(inputs, "oracle")
	if err := os.MkdirAll(oracleDir, 0o755); err != nil {
		t.Fatalf("create oracle dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(oracleDir, "check.sh"), []byte("exit 0\n"), 0o644); err != nil {
		t.Fatalf("write oracle check: %v", err)
	}
	// A quarantined run's held reproduction, for -recheck-reproductions.
	held := filepath.Join(dataDir, "runs", "golden-quarantined", "review-repro", "0", "held_test.go")
	if err := os.MkdirAll(filepath.Dir(held), 0o755); err != nil {
		t.Fatalf("create the held reproduction's directory: %v", err)
	}
	if err := os.WriteFile(held, []byte("package p\n"), 0o444); err != nil {
		t.Fatalf("write the held reproduction: %v", err)
	}
	if err := (&run.Run{ID: "golden-quarantined", RequestID: "req-golden"}).Save(dataDir); err != nil {
		t.Fatalf("save the quarantined run: %v", err)
	}
	manifest := fmt.Sprintf(`{"request_id":"req-golden","reproductions":[{"run_id":"golden-quarantined","finding":0,"path":"held_test.go","sha256":"%s","host_path":%q,"spec_line":3,"test_ids":["TestHeld"],"runner":"go-test-json"}]}`, strings.Repeat("a", 64), held)
	head, err := runGit(t, workspace, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("read the workspace's head: %v", err)
	}
	return []string{
		"-ticket", "001-golden",
		"-instruction-base", strings.TrimSpace(head),
		"-run-id", runID,
		"-workspace", workspace,
		"-spec", write("spec.md", wellFormedRequestTicketspec),
		"-request-ticket",
		"-data-dir", dataDir,
		"-temporal-address", address,
		"-build-app-interpreter", "/bin/sh",
		"-build-app-script", filepath.Join(testdataDir(t), "fake_build_app.sh"),
		"-build-app-max-attempts", "3",
		"-execution-model", "builder",
		"-execution-thinking", "low",
		"-code-review-policy", "advisory",
		"-earlier-attempt", write("earlier-attempt.md", "# What the earlier attempt left (run golden-0)\n"),
		"-max-rounds", "7",
		"-timeout-minutes", "11",
		"-verify-command", "true",
		"-fast-check-command", "echo fast",
		"-verify-max-attempts", "4",
		"-full-suite-command", "echo full",
		"-lint-command", "echo lint",
		"-security-command", "echo security",
		"-unit-test-command", "echo unit",
		"-integration-test-command", "echo integration",
		"-reference-oracle-command", "sh verify/check.sh",
		"-reference-oracle-dir", oracleDir,
		"-reference-oracle-mount-path", "verify",
		"-reference-oracle-in-loop-retry",
		"-no-commit-oracles",
		"-bug",
		"-sandbox-image", fakeSandboxImage,
		"-registry-proxy",
		"-review-reproduction",
		"-review-spec", write("approved-spec.md", "# Approved spec\n\n- `Parse` returns a map.\n"),
		"-recheck-reproductions", write("recheck.json", manifest),
	}
}

// startedRunWorkflowInput reads workflowID's RunWorkflowInput back from the
// test Temporal server: the first history event carries exactly what the
// client sent.
func startedRunWorkflowInput(t *testing.T, address, workflowID string) map[string]any {
	t.Helper()
	c, err := temporalclient.Dial(temporalclient.Options{HostPort: address})
	if err != nil {
		t.Fatalf("dial test Temporal: %v", err)
	}
	defer c.Close()
	deadline := time.Now().Add(10 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		iter := c.GetWorkflowHistory(ctx, workflowID, "", false, enumspb.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)
		var payload []byte
		if iter.HasNext() {
			if event, err := iter.Next(); err == nil {
				if attrs := event.GetWorkflowExecutionStartedEventAttributes(); attrs != nil && len(attrs.GetInput().GetPayloads()) == 1 {
					payload = attrs.GetInput().GetPayloads()[0].GetData()
				}
			}
		}
		cancel()
		if payload != nil {
			var input map[string]any
			if err := json.Unmarshal(payload, &input); err != nil {
				t.Fatalf("decode RunWorkflowInput of %s: %v", workflowID, err)
			}
			return input
		}
		if time.Now().After(deadline) {
			t.Fatalf("workflow %s never started on the test Temporal server", workflowID)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// compareRunInputGolden replaces every run-specific string in input with its
// placeholder and compares the result, field by field as indented JSON, with
// testdata/<name>.
func compareRunInputGolden(t *testing.T, name string, input map[string]any, placeholders [][2]string) {
	t.Helper()
	raw, err := json.MarshalIndent(input, "", "  ")
	if err != nil {
		t.Fatalf("encode RunWorkflowInput: %v", err)
	}
	for _, p := range placeholders {
		if p[0] == "" {
			t.Fatalf("placeholder %s has no value to replace", p[1])
		}
		raw = bytes.ReplaceAll(raw, []byte(p[0]), []byte(p[1]))
	}
	raw = append(raw, '\n')
	goldenPath := filepath.Join("testdata", name)
	if os.Getenv(updateGoldenEnv) == "1" {
		if err := os.WriteFile(goldenPath, raw, 0o644); err != nil {
			t.Fatalf("write %s: %v", goldenPath, err)
		}
		return
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden (run with %s=1 to create it): %v", updateGoldenEnv, err)
	}
	if bytes.Equal(raw, want) {
		return
	}
	gotLines, wantLines := strings.Split(string(raw), "\n"), strings.Split(string(want), "\n")
	var diff []string
	for i := 0; i < len(gotLines) || i < len(wantLines); i++ {
		var g, w string
		if i < len(gotLines) {
			g = gotLines[i]
		}
		if i < len(wantLines) {
			w = wantLines[i]
		}
		if g != w {
			diff = append(diff, fmt.Sprintf("line %d:\n  got:  %s\n  want: %s", i+1, g, w))
		}
	}
	t.Fatalf("RunWorkflowInput differs from %s (%d line(s)):\n%s", goldenPath, len(diff), strings.Join(diff, "\n"))
}

// runInputGoldenRun runs the built factoryd with the full flag set (plus
// extraFlags) to the end of its run and returns what the comparison needs.
// The run's own outcome is not asserted: the input is what is under test.
func runInputGoldenRun(t *testing.T, runID string, extraFlags ...string) (address string, placeholders [][2]string) {
	t.Helper()
	return runInputGoldenRunWith(t, runID, func(_, _ string, flags []string) ([]string, [][2]string) {
		return append(flags, extraFlags...), nil
	})
}

// runInputGoldenRunWith is runInputGoldenRun with the flag set given to
// prepare, which may change the workspace and the flags and returns
// placeholders of its own.
func runInputGoldenRunWith(t *testing.T, runID string, prepare func(workspace, inputs string, flags []string) ([]string, [][2]string)) (address string, placeholders [][2]string) {
	t.Helper()
	address = sharedTemporalAddress(t)
	workspace := newFixtureRepo(t)
	commitFactoryYML(t, workspace, goldenFactoryYML)
	dataDir, inputs := t.TempDir(), t.TempDir()
	flags, own := prepare(workspace, inputs, runInputGoldenFlags(t, workspace, dataDir, inputs, runID, address))
	cmd := factorydCommand(t, flags...)
	cmd.Env = append(os.Environ(), "FAKE_BUILD_APP_MODE=commit", "RUN_INPUT_GOLDEN_KEY=sk-test", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	cmd.Env = append(cmd.Env, isolatedSessionConfigEnv(t, runInputGoldenConfig(t))...)
	out, _ := cmd.CombinedOutput()
	t.Logf("factoryd output:\n%s", out)
	// The run record keeps the recheck it was given from its start, for the
	// build that follows it even when its gates fail before the recheck. A
	// replay run is given none.
	if slices.Contains(flags, "-recheck-reproductions") {
		rec, err := run.Load(dataDir, runID)
		if err != nil || len(rec.ReviewReproductionRechecksGiven) != 1 || rec.ReviewReproductionRechecksGiven[0].RunID != "golden-quarantined" {
			t.Errorf("run record's rechecks given = %+v (%v), want the manifest's one entry", rec, err)
		}
	}

	resolve := func(path string) string {
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			t.Fatalf("resolve %s: %v", path, err)
		}
		return resolved
	}
	baseSHA, err := runGit(t, workspace, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("read the workspace's base commit: %v", err)
	}
	// Both spellings of each temp path: macOS temp dirs are symlinked.
	return address, append(own, [][2]string{
		{resolve(dataDir), "<DATA_DIR>"},
		{dataDir, "<DATA_DIR>"},
		{resolve(workspace), "<WORKSPACE>"},
		{workspace, "<WORKSPACE>"},
		{filepath.Base(workspace), "<PROJECT>"},
		{resolve(inputs), "<INPUTS>"},
		{inputs, "<INPUTS>"},
		{testdataDir(t), "<TESTDATA>"},
		{strings.TrimSpace(baseSHA), "<BASE_SHA>"},
		{runID, "<RUN_ID>"},
	}...)
}

// testdataDir is this package's testdata directory, absolute.
func testdataDir(t *testing.T) string {
	t.Helper()
	path, err := filepath.Abs("testdata")
	if err != nil {
		t.Fatalf("resolve testdata: %v", err)
	}
	return path
}

// TestRunWorkflowInputFromFullFlagSetMatchesGolden runs factoryd with every
// run flag and session-config key that reaches RunWorkflowInput set to a
// non-default value, and compares the input the Temporal server received with
// a golden captured at tag m6. A flag that stops reaching the workflow, or
// reaches a different field, changes the JSON.
func TestRunWorkflowInputFromFullFlagSetMatchesGolden(t *testing.T) {
	runID := fmt.Sprintf("golden-run-%d-%d", os.Getpid(), time.Now().UnixNano())
	address, placeholders := runInputGoldenRun(t, runID)
	compareRunInputGolden(t, "run_workflow_input.golden.json", startedRunWorkflowInput(t, address, runID), placeholders)
}

// TestRepositoryOwnerRunWorkflowInputFromFullFlagSetMatchesGolden is the same
// comparison for a -repository run, whose RunWorkflow is a child of the
// repository owner workflow and carries the owner fields as well.
func TestRepositoryOwnerRunWorkflowInputFromFullFlagSetMatchesGolden(t *testing.T) {
	runID := fmt.Sprintf("golden-owner-run-%d-%d", os.Getpid(), time.Now().UnixNano())
	repository := "golden-repository-" + runID
	ownerID := workflow.RepositoryOwnerWorkflowID(repository)
	terminateRepositoryOwnerAtCleanup(t, sharedTemporalAddress(t), ownerID)
	address, placeholders := runInputGoldenRun(t, runID, "-repository", repository)
	placeholders = append([][2]string{{ownerID, "<OWNER_ID>"}, {repository, "<REPOSITORY>"}}, placeholders...)
	childID := workflow.RepositoryOwnerRunWorkflowID(ownerID, runID)
	compareRunInputGolden(t, "run_workflow_input_repository_owner.golden.json", startedRunWorkflowInput(t, address, childID), placeholders)
}

// TestReplayRunWorkflowInputMatchesGolden is the same comparison for a replay
// run: the full flag set without -earlier-attempt and -run-id (both refused
// with -replay), plus -on-branch, -diff-base and -replay for a commit
// replayed onto the workspace's head.
func TestReplayRunWorkflowInputMatchesGolden(t *testing.T) {
	var dataDir string
	address, placeholders := runInputGoldenRunWith(t, "unused", func(workspace, inputs string, flags []string) ([]string, [][2]string) {
		onto, head := replayedBranch(t, workspace, "buildgate/req-golden/t2-restack1")
		for _, refused := range []string{"-earlier-attempt", "-run-id", "-review-spec", "-recheck-reproductions"} {
			if i := slices.Index(flags, refused); i >= 0 {
				flags = slices.Delete(flags, i, i+2)
			}
		}
		// A replay run's review is advice only: it takes no reproduction input.
		if i := slices.Index(flags, "-review-reproduction"); i >= 0 {
			flags = slices.Delete(flags, i, i+1)
		}
		dataDir = flags[slices.Index(flags, "-data-dir")+1]
		flags = append(flags, "-on-branch", "buildgate/req-golden/t2-restack1", "-diff-base", onto, "-replay", writeReplayFile(t, inputs, onto, head))
		return flags, [][2]string{{head, "<REPLAY_HEAD>"}}
	})
	entries, err := os.ReadDir(filepath.Join(dataDir, "runs"))
	var runs []string
	for _, e := range entries {
		if e.Name() != "golden-quarantined" { // the full flag set's held reproduction
			runs = append(runs, e.Name())
		}
	}
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs of the replay golden run: %v, %v; want one", runs, err)
	}
	runID := runs[0]
	placeholders = append([][2]string{{runID, "<RUN_ID>"}}, placeholders...)
	compareRunInputGolden(t, "run_workflow_input_replay.golden.json", startedRunWorkflowInput(t, address, runID), placeholders)
}

// replayedBranch creates branch in workspace with one commit on its HEAD, as
// the host's replay leaves it, and returns the HEAD (onto) and the branch's
// commit (head). The workspace stays on its branch.
func replayedBranch(t *testing.T, workspace, branch string) (onto, head string) {
	t.Helper()
	out, err := runGit(t, workspace, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	onto = strings.TrimSpace(string(out))
	scratch := filepath.Join(t.TempDir(), "replay")
	if out, err := runGit(t, workspace, "worktree", "add", "-q", "-b", branch, scratch, onto); err != nil {
		t.Fatalf("git worktree add: %v: %s", err, out)
	}
	if err := os.WriteFile(filepath.Join(scratch, "content.txt"), []byte("replayed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "-A"}, {"-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "-m", "replayed commit"}} {
		if out, err := runGit(t, scratch, args...); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	out, err = runGit(t, scratch, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	head = strings.TrimSpace(string(out))
	if out, err := runGit(t, workspace, "worktree", "remove", "--force", scratch); err != nil {
		t.Fatalf("git worktree remove: %v: %s", err, out)
	}
	return onto, head
}

// goldenFactoryYML is the committed .factory.yml of the golden runs: setup and
// autofix have no flag, so this file is the only way they reach the input.
const goldenFactoryYML = "setup:\n  - npm ci\n  - make generate\nautofix:\n  - gofmt -w .\n"

// commitFactoryYML commits content as workspace's .factory.yml.
func commitFactoryYML(t *testing.T, workspace, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(workspace, ".factory.yml"), []byte(content), 0o644); err != nil {
		t.Fatalf("write .factory.yml: %v", err)
	}
	for _, args := range [][]string{{"add", ".factory.yml"}, {"commit", "-q", "-m", "add .factory.yml"}} {
		if out, err := runGit(t, workspace, args...); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
}
