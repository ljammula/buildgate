package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"buildgate/internal/oraclecanary"
	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
	"buildgate/internal/runner"
)

// Found live 2026-09-21: a drafted oracle that called a function with no result
// as a value never compiled.
const (
	brokenGoOracle = "package mathx\n\nimport \"testing\"\n\nfunc noValue() {}\n\nfunc TestOracleAdd(t *testing.T) {\n\tt.Log(noValue())\n}\n"
	fixedGoOracle  = "package mathx\n\nimport \"testing\"\n\nfunc TestOracleAdd(t *testing.T) {\n\tt.Log(\"ok\")\n}\n"
)

// stubLaunchSeq installs one stub launch per fake, in order; a launch past the
// end fails the test. onLaunch (may be nil) sees the 1-based launch number.
func stubLaunchSeq(t *testing.T, fakes []fakeScript, onLaunch func(n int, l oracleLaunch)) *int {
	t.Helper()
	inner := make([]func(context.Context, requestdriver.WorkerConfig, *request.Request, oracleLaunch) (runner.Result, error), len(fakes))
	for i, fake := range fakes {
		stubLaunch(t, fake, nil)
		inner[i] = launchOracleDraft
	}
	launches := 0
	launchOracleDraft = func(ctx context.Context, cfg requestdriver.WorkerConfig, job *request.Request, l oracleLaunch) (runner.Result, error) {
		launches++
		if launches > len(inner) {
			t.Fatalf("unexpected launch #%d", launches)
		}
		if onLaunch != nil {
			onLaunch(launches, l)
		}
		return inner[launches-1](ctx, cfg, job, l)
	}
	return &launches
}

func goDraftedWith(source string) fakeScript {
	s := goDrafted()
	s.Files = map[string]string{"oracle_001_test.go": source}
	return s
}

func TestAutoRedraftFixesANonCompilingDraftOnce(t *testing.T) {
	f := newDraftFixture(t)
	var feedback string
	launches := stubLaunchSeq(t, []fakeScript{goDraftedWith(brokenGoOracle), goDraftedWith(fixedGoOracle)}, func(n int, l oracleLaunch) {
		if n == 2 {
			b, _ := os.ReadFile(l.FeedbackPath)
			feedback = string(b)
			if l.FeedbackPath == "" || l.FeedbackPath == request.OracleFeedbackPath(f.dataDir, f.id) {
				t.Errorf("the redraft must use its own feedback file, got %q", l.FeedbackPath)
			}
			if entries, _ := os.ReadDir(l.OutDir); len(entries) != 0 {
				t.Errorf("scratch from the first pass was not cleaned before the redraft: %v", entries)
			}
		}
	})
	got, err := f.run(t)
	if err != nil {
		t.Fatal(err)
	}
	if *launches != 2 {
		t.Fatalf("launches = %d, want 2", *launches)
	}
	if got.Status != request.OracleDrafted || !got.AutoRedrafted || len(got.CompileProblems) != 0 {
		t.Fatalf("draft = %+v, want a clean automatic redraft", got)
	}
	if !strings.Contains(feedback, "no value") || !strings.Contains(feedback, "Automatic compile check") {
		t.Errorf("redraft feedback lacks the compiler output:\n%s", feedback)
	}
	if files := f.oracleFiles(t); files["oracle_001_test.go"] != fixedGoOracle {
		t.Errorf("oracle/ holds %q, want the second draft", files["oracle_001_test.go"])
	}
	f.noStagingLeftovers(t)
	logDir := filepath.Join(request.Dir(f.dataDir, f.id), "logs")
	if _, err := os.Stat(filepath.Join(logDir, "oracle_autofeedback.md")); !os.IsNotExist(err) {
		t.Errorf("the redraft feedback file was left behind: %v", err)
	}
}

func TestAutoRedraftKeepsTheFirstDraftWhenTheSecondPassFails(t *testing.T) {
	cases := map[string]fakeScript{
		"script exit":   {Exit: 2},
		"launch error":  {Err: context.DeadlineExceeded},
		"failed status": {Status: "failed"},
	}
	for name, second := range cases {
		t.Run(name, func(t *testing.T) {
			f := newDraftFixture(t)
			launches := stubLaunchSeq(t, []fakeScript{goDraftedWith(brokenGoOracle), second}, nil)
			got, err := f.run(t)
			if err != nil {
				t.Fatal(err)
			}
			if *launches != 2 {
				t.Fatalf("launches = %d, want 2", *launches)
			}
			if got.Status != request.OracleDrafted || got.AutoRedrafted || len(got.CompileProblems) == 0 || !strings.Contains(got.Detail, "did not replace it") {
				t.Fatalf("draft = %+v, want the first draft, still flagged, with the reason", got)
			}
			if files := f.oracleFiles(t); files["oracle_001_test.go"] != brokenGoOracle {
				t.Errorf("the first draft was lost: %v", files)
			}
			f.noStagingLeftovers(t)
		})
	}
}

func TestAutoRedraftHappensAtMostOnce(t *testing.T) {
	f := newDraftFixture(t)
	launches := stubLaunchSeq(t, []fakeScript{goDraftedWith(brokenGoOracle), goDraftedWith(brokenGoOracle)}, nil)
	got, err := f.run(t)
	if err != nil {
		t.Fatal(err)
	}
	if *launches != 2 || !got.AutoRedrafted || len(got.CompileProblems) == 0 || !strings.Contains(got.Detail, "compile self-check found problems") {
		t.Fatalf("launches=%d draft=%+v: want exactly one redraft and the second draft flagged", *launches, got)
	}
}

func TestCleanDraftIsNotRedrafted(t *testing.T) {
	f := newDraftFixture(t)
	launches := stubLaunchSeq(t, []fakeScript{goDraftedWith(fixedGoOracle)}, nil)
	got, err := f.run(t)
	if err != nil {
		t.Fatal(err)
	}
	if *launches != 1 || got.AutoRedrafted || len(got.CompileProblems) != 0 {
		t.Fatalf("launches=%d draft=%+v", *launches, got)
	}
}

func TestAutoRedraftKeepsOperatorFeedbackAndAddsCompilerOutput(t *testing.T) {
	f := newDraftFixture(t)
	feedbackPath := request.OracleFeedbackPath(f.dataDir, f.id)
	mustWrite(t, feedbackPath, "## Oracle rejected\n\nuse table tests\n")
	var second string
	stubLaunchSeq(t, []fakeScript{goDraftedWith(brokenGoOracle), goDraftedWith(fixedGoOracle)}, func(n int, l oracleLaunch) {
		if n == 2 {
			b, _ := os.ReadFile(l.FeedbackPath)
			second = string(b)
		}
	})
	in := requestdriver.OracleDraftInput{DataDir: f.dataDir, Request: loadRequest(t, f.dataDir, f.id), Cfg: f.cfg, OracleDir: f.oracleDir, FeedbackPath: feedbackPath}
	if _, err := runOracleDraftJob(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(second, "use table tests") || !strings.Contains(second, "no value") {
		t.Errorf("second-pass feedback = %q, want the operator's note and the compiler output", second)
	}
	if b, _ := os.ReadFile(feedbackPath); string(b) != "## Oracle rejected\n\nuse table tests\n" {
		t.Errorf("the operator's feedback file was modified: %q", b)
	}
}

func twoGoFileManifest(targetA, targetB string) (string, map[string]string) {
	entries := []map[string]any{
		{"criterion": "1. A retried POST /refunds with the same idempotency key returns the original result.", "criterion_index": 1, "oracle_file": "a_test.go", "target_path": targetA, "supersedes": []string{}, "rationale": "r"},
		{"criterion": "2. A non-idempotent POST /refunds still processes normally.", "criterion_index": 2, "oracle_file": "b_test.go", "target_path": targetB, "supersedes": []string{}, "rationale": "r"},
	}
	b, _ := json.Marshal(entries)
	return string(b), map[string]string{
		"a_test.go": "package mathx\n\nimport \"testing\"\n\nfunc TestOracleA(t *testing.T) { t.Log(1) }\n",
		"b_test.go": "package mathx\n\nimport \"testing\"\n\nfunc TestOracleB(t *testing.T) { t.Log(2) }\n",
	}
}

func TestMultiFileOracleGetsAGeneratedRunCommand(t *testing.T) {
	f := newDraftFixture(t)
	manifest, files := twoGoFileManifest("internal/mathx/a_test.go", "internal/other/b_test.go")
	stubLaunch(t, fakeScript{Status: "drafted", Manifest: manifest, Files: files}, nil)
	got, err := f.run(t)
	if err != nil {
		t.Fatal(err)
	}
	want, cmdErr := oraclecanary.GoMultiCommand(".", []oraclecanary.GoOracleFile{{Name: "a_test.go", PkgDir: "internal/mathx"}, {Name: "b_test.go", PkgDir: "internal/other"}})
	if cmdErr != nil {
		t.Fatal(cmdErr)
	}
	oracle := f.oracleFiles(t)
	if oracle["RUN_COMMAND.txt"] != want+"\n" || got.ProposedCommand != want {
		t.Fatalf("RUN_COMMAND.txt = %q, proposed = %q, want %q", oracle["RUN_COMMAND.txt"], got.ProposedCommand, want)
	}
	if got.GeneratedRunCommandSHA256 != oracleSHA256([]byte(want+"\n")) || strings.Contains(strings.Join(got.Files, ","), "RUN_COMMAND") {
		t.Errorf("draft = %+v", got)
	}
	if !strings.Contains(got.Detail, "generated RUN_COMMAND.txt") {
		t.Errorf("detail does not say the command was generated: %s", got.Detail)
	}
	// The generated file passes the same checks a hand-written one does at
	// approval, and (directory-scoped) fits any ticket subset.
	if err := request.ValidateOracleRunCommand(want); err != nil {
		t.Errorf("ValidateOracleRunCommand: %v", err)
	}
	if err := oraclecanary.CheckDir(f.oracleDir); err != nil {
		t.Errorf("CheckDir: %v", err)
	}
}

// TestPythonOracleGetsAGeneratedRunCommand: unlike Go (which needs 2+ files
// before the host writes RUN_COMMAND.txt itself), a single drafted Python
// file is enough, because PythonStdlibCommand's glob-based runner covers one
// file or several identically.
func TestPythonOracleGetsAGeneratedRunCommand(t *testing.T) {
	f := newDraftFixture(t)
	stubLaunch(t, pythonDrafted(), nil)
	got, err := f.run(t)
	if err != nil {
		t.Fatal(err)
	}
	want := oraclecanary.PythonStdlibCommand()
	oracle := f.oracleFiles(t)
	if oracle["RUN_COMMAND.txt"] != want+"\n" || got.ProposedCommand != want {
		t.Fatalf("RUN_COMMAND.txt = %q, proposed = %q, want %q", oracle["RUN_COMMAND.txt"], got.ProposedCommand, want)
	}
	if got.GeneratedRunCommandSHA256 != oracleSHA256([]byte(want+"\n")) {
		t.Errorf("draft = %+v", got)
	}
	if err := request.ValidateOracleRunCommand(want); err != nil {
		t.Errorf("ValidateOracleRunCommand: %v", err)
	}
	if err := oraclecanary.CheckDir(f.oracleDir); err != nil {
		t.Errorf("CheckDir: %v", err)
	}
}

func TestGeneratedRunCommandNeverOverwritesTheOperatorsAndFollowsRedrafts(t *testing.T) {
	manifest, files := twoGoFileManifest("internal/mathx/a_test.go", "internal/mathx/b_test.go")
	drafted := fakeScript{Status: "drafted", Manifest: manifest, Files: files}

	t.Run("operator file present before the first pass", func(t *testing.T) {
		f := newDraftFixture(t)
		mustWrite(t, filepath.Join(f.oracleDir, "RUN_COMMAND.txt"), "cd . && go test .oracle\n")
		stubLaunch(t, drafted, nil)
		got, err := f.run(t)
		if err != nil {
			t.Fatal(err)
		}
		if f.oracleFiles(t)["RUN_COMMAND.txt"] != "cd . && go test .oracle\n" || got.GeneratedRunCommandSHA256 != "" {
			t.Errorf("operator's command was replaced: %q, %+v", f.oracleFiles(t)["RUN_COMMAND.txt"], got)
		}
	})

	t.Run("a redraft replaces its own generated file", func(t *testing.T) {
		f := newDraftFixture(t)
		stubLaunch(t, drafted, nil)
		first, err := f.run(t)
		if err != nil || first.GeneratedRunCommandSHA256 == "" {
			t.Fatalf("first pass: %+v, %v", first, err)
		}
		manifest2, files2 := twoGoFileManifest("internal/mathx/a_test.go", "internal/pkg2/b_test.go")
		stubLaunch(t, fakeScript{Status: "drafted", Manifest: manifest2, Files: files2}, nil)
		second, err := f.run(t)
		if err != nil {
			t.Fatal(err)
		}
		if second.GeneratedRunCommandSHA256 == first.GeneratedRunCommandSHA256 || !strings.Contains(f.oracleFiles(t)["RUN_COMMAND.txt"], "pkg2/") {
			t.Errorf("the generated command was not regenerated for the new draft: %q", f.oracleFiles(t)["RUN_COMMAND.txt"])
		}
	})

	t.Run("a redraft keeps an operator edit", func(t *testing.T) {
		f := newDraftFixture(t)
		stubLaunch(t, drafted, nil)
		if _, err := f.run(t); err != nil {
			t.Fatal(err)
		}
		mustWrite(t, filepath.Join(f.oracleDir, "RUN_COMMAND.txt"), "edited by the operator .oracle\n")
		stubLaunch(t, drafted, nil)
		got, err := f.run(t)
		if err != nil {
			t.Fatal(err)
		}
		if f.oracleFiles(t)["RUN_COMMAND.txt"] != "edited by the operator .oracle\n" || got.GeneratedRunCommandSHA256 != "" {
			t.Errorf("an edited command must be carried across: %q, %+v", f.oracleFiles(t)["RUN_COMMAND.txt"], got)
		}
	})
}

func TestNoGeneratedRunCommandWhenFilesSpanModulesOrHaveNoTarget(t *testing.T) {
	cases := map[string]struct {
		a, b   string
		modDir string
	}{
		"two modules": {"svc1/x/a_test.go", "svc2/x/b_test.go", "svc"},
		"no target":   {"internal/mathx/a_test.go", "", ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newDraftFixture(t)
			if tc.modDir != "" {
				mustWrite(t, filepath.Join(f.workspace, "svc1", "go.mod"), "module example.com/s1\n")
				mustWrite(t, filepath.Join(f.workspace, "svc2", "go.mod"), "module example.com/s2\n")
			}
			var b any = tc.b
			if tc.b == "" {
				b = nil
			}
			manifest, files := twoGoFileManifest(tc.a, "")
			var entries []map[string]any
			_ = json.Unmarshal([]byte(manifest), &entries)
			entries[1]["target_path"] = b
			raw, _ := json.Marshal(entries)
			stubLaunch(t, fakeScript{Status: "drafted", Manifest: string(raw), Files: files}, nil)
			got, err := f.run(t)
			if err != nil || got.Status != request.OracleDrafted {
				t.Fatalf("draft = %+v, %v", got, err)
			}
			if _, ok := f.oracleFiles(t)["RUN_COMMAND.txt"]; ok || got.ProposedCommand != "" {
				t.Errorf("no command may be generated: %+v", got)
			}
			if !strings.Contains(got.Detail, "RUN_COMMAND.txt is yours to write") {
				t.Errorf("detail should still tell the operator to write it: %s", got.Detail)
			}
		})
	}
}

func TestSelfCheckReportsSpecExampleContradictionsWithoutBlocking(t *testing.T) {
	criteria := []string{"1. `ALL` and `done ` are accepted state filters (case-insensitive, whitespace-trimmed)."}
	src := "package mathx\n\nimport \"testing\"\n\nfunc TestOracleState(t *testing.T) {\n\tinvalidStates := []string{\"bogus\", \"ALL\", \"done \"}\n\tt.Log(invalidStates)\n}\n"
	out := t.TempDir()
	mustWrite(t, filepath.Join(out, "oracle_001_test.go"), src)
	m, _ := json.Marshal([]map[string]any{{"criterion": criteria[0], "criterion_index": 1, "oracle_file": "oracle_001_test.go", "target_path": "internal/mathx/oracle_001_test.go", "supersedes": []string{}, "rationale": "r"}})
	mustWrite(t, filepath.Join(out, "MANIFEST.json"), string(m))
	mustWrite(t, filepath.Join(out, "evidence.json"), `{"schema_version":1,"status":"drafted"}`)
	f := newDraftFixture(t)
	got := installDraftedOracle(installInput{
		OutDir: out, EvidencePath: filepath.Join(out, "evidence.json"), OracleDir: f.oracleDir,
		Workspace: f.workspace, Criteria: criteria,
	})
	if got.Status != request.OracleDrafted || len(got.SpecWarnings) == 0 || len(got.CompileProblems) != 0 {
		t.Fatalf("draft = %+v, want drafted with spec warnings and no compile problems", got)
	}
	if !strings.Contains(got.Detail, "spec-example check") || !strings.Contains(got.SpecWarnings[0], `"ALL"`) {
		t.Errorf("detail/warnings do not name the literal: %s / %v", got.Detail, got.SpecWarnings)
	}
	if files := f.oracleFiles(t); files["oracle_001_test.go"] != src {
		t.Errorf("a warning must not stop the draft from being installed: %v", files)
	}
}
