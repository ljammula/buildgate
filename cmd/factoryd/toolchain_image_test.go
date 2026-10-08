package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"buildgate/internal/hostcontrol"
	"buildgate/internal/toolchain"
)

func TestStageAgentTestsLaysOutTheSuiteAsInTheRepository(t *testing.T) {
	dest := t.TempDir()
	if err := stageAgentTests(dest); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"agent/pi/scripts/build_app.py",
		"agent/pi/scripts/harness_adapters.py",
		"agent/pi/tests/build_app_test.py",
		"agent/pi/tests/fixtures/codex_round1.jsonl",
		"cmd/factoryd/testdata/sanitize_vectors.json",
		"cmd/factoryd/testdata/criterion_key_vectors.json",
	} {
		if _, err := os.Stat(filepath.Join(dest, filepath.FromSlash(want))); err != nil {
			t.Errorf("staged tree lacks %s: %v", want, err)
		}
	}
	// What a worker image cannot run is left out, so that every staged test
	// is one whose failure is the interpreter's.
	tests, err := filepath.Glob(filepath.Join(dest, "agent", "pi", "tests", "*.py"))
	if err != nil || len(tests) == 0 {
		t.Fatalf("staged tests = %v, %v", tests, err)
	}
	for _, path := range tests {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if agentTestNeedsPytest.Match(data) || strings.Contains(string(data), `"internal" / "meter"`) {
			t.Errorf("%s needs pytest or this checkout and was staged", filepath.Base(path))
		}
	}
	// Every repository file a staged test reads through its checkout root is
	// one the tree carries.
	for _, path := range tests {
		data, _ := os.ReadFile(path)
		for _, line := range strings.Split(string(data), "\n") {
			if !strings.Contains(line, `parents[3] / "`) || !strings.Contains(line, "read_text") {
				continue
			}
			rel := strings.NewReplacer(`" / "`, "/", `"`, "").Replace(line[strings.Index(line, `parents[3] / "`)+len(`parents[3] / `) : strings.Index(line, `).read_text`)])
			if _, err := os.Stat(filepath.Join(dest, filepath.FromSlash(rel))); err != nil {
				t.Errorf("%s reads %s, which is not staged", filepath.Base(path), rel)
			}
		}
	}
}

func TestToolchainImageKeyFollowsItsInputs(t *testing.T) {
	goArgs := []string{"GO_MODE=replace", "GO_IMAGE=golang:1.27"}
	key := toolchainImageKey("worker@sha256:aaaa", goArgs)
	if key != toolchainImageKey("worker@sha256:aaaa", []string{"GO_IMAGE=golang:1.27", "GO_MODE=replace"}) {
		t.Error("the key depends on the order of the build arguments")
	}
	for name, other := range map[string]string{
		"another base":    toolchainImageKey("worker@sha256:bbbb", goArgs),
		"another version": toolchainImageKey("worker@sha256:aaaa", []string{"GO_MODE=replace", "GO_IMAGE=golang:1.28"}),
		"python as well":  toolchainImageKey("worker@sha256:aaaa", append([]string{"PYTHON_MODE=replace"}, goArgs...)),
	} {
		if other == key {
			t.Errorf("%s has the same key", name)
		}
	}
}

func TestToolchainImageForDerivesAnImageOnlyWhenTheBaseLacksADeclaredVersion(t *testing.T) {
	t.Setenv(hostcontrol.AutostartEnvVar, "1")
	const base = "worker@sha256:aaaa"
	newDeps := func(t *testing.T) (*deps, *[][]string) {
		dp := newTestDeps(t)
		var built [][]string
		fakeDockerOf(dp).imageToolsFn = func(context.Context, string, string) (toolchain.Installed, string, error) {
			installed, codename := parseImageToolchains(workerImageToolchains)
			return installed, codename, nil
		}
		fakeDockerOf(dp).toolImageFn = func(_ context.Context, _, from string, buildArgs []string) (string, error) {
			built = append(built, append([]string{from}, buildArgs...))
			return "localhost:5050/buildgate-toolchain@sha256:cccc", nil
		}
		return dp, &built
	}
	cases := []struct {
		name      string
		files     map[string]string
		wantImage string
		wantBuild []string
		wantOut   string
	}{
		{"nothing declared", map[string]string{}, base, nil, ""},
		{"the base satisfies it", map[string]string{"go.mod": "module m\n\ngo 1.26\n"}, base, nil, ""},
		{"a newer Go", map[string]string{"go.mod": "module m\n\ngo 1.27\n"}, "localhost:5050/buildgate-toolchain@sha256:cccc",
			[]string{base, "GO_MODE=replace", "GO_IMAGE=golang:1.27"}, "installing Go 1.27"},
		{"another Python", map[string]string{".python-version": "3.12\n"}, "localhost:5050/buildgate-toolchain@sha256:cccc",
			[]string{base, "PYTHON_MODE=replace", "PYTHON_IMAGE=python:3.12-slim-bookworm", "PYTHON_VERSION=3.12"}, "installing Python 3.12"},
		{"Node is reported and kept", map[string]string{".nvmrc": "20\n"}, base, nil, "keeps its own Node"},
		{"a file that does not parse is reported and the base kept", map[string]string{"pyproject.toml": "[project\n"}, base, nil, "were not checked"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dp, built := newDeps(t)
			var out bytes.Buffer
			image, err := toolchainImageFor(dp, context.Background(), &out, toolchainRepo(t, tc.files), base, "docker")
			if err != nil || image != tc.wantImage {
				t.Fatalf("image = %q, %v; want %q", image, err, tc.wantImage)
			}
			var got []string
			if len(*built) == 1 {
				got = (*built)[0]
			}
			if !reflect.DeepEqual(got, tc.wantBuild) || len(*built) > 1 {
				t.Errorf("built %v, want %v", *built, tc.wantBuild)
			}
			if !strings.Contains(out.String(), tc.wantOut) || (tc.wantOut == "" && out.Len() != 0) {
				t.Errorf("output %q, want it to contain %q", out.String(), tc.wantOut)
			}
		})
	}
}

func TestToolchainImageForLeavesTheBaseWhenItCannotCompareAndRefusesWhatCannotPass(t *testing.T) {
	const base = "worker@sha256:aaaa"
	repo := toolchainRepo(t, map[string]string{"go.mod": "module m\n\ngo 1.27\n"})
	var out bytes.Buffer

	t.Run("FACTORYD_AUTOSTART=0 builds nothing", func(t *testing.T) {
		t.Setenv(hostcontrol.AutostartEnvVar, "0")
		dp := newTestDeps(t)
		fakeDockerOf(dp).imageToolsFn = func(context.Context, string, string) (toolchain.Installed, string, error) {
			t.Error("the image was run")
			return nil, "", nil
		}
		if image, err := toolchainImageFor(dp, context.Background(), &out, repo, base, "docker"); err != nil || image != base {
			t.Errorf("image = %q, %v; want the base", image, err)
		}
	})
	t.Setenv(hostcontrol.AutostartEnvVar, "1")
	t.Run("an image that cannot be asked", func(t *testing.T) {
		dp := newTestDeps(t)
		if image, err := toolchainImageFor(dp, context.Background(), &out, repo, base, "docker"); err != nil || image != base {
			t.Errorf("image = %q, %v; want the base", image, err)
		}
	})
	t.Run("an image that reports no Go", func(t *testing.T) {
		dp := newTestDeps(t)
		fakeDockerOf(dp).imageToolsFn = func(context.Context, string, string) (toolchain.Installed, string, error) {
			return toolchain.Installed{}, "bookworm", nil
		}
		if image, err := toolchainImageFor(dp, context.Background(), &out, repo, base, "docker"); err != nil || image != base {
			t.Errorf("image = %q, %v; want the base", image, err)
		}
	})
	t.Run("a failed build refuses the run", func(t *testing.T) {
		dp := newTestDeps(t)
		fakeDockerOf(dp).imageToolsFn = func(context.Context, string, string) (toolchain.Installed, string, error) {
			installed, codename := parseImageToolchains(workerImageToolchains)
			return installed, codename, nil
		}
		fakeDockerOf(dp).toolImageFn = func(context.Context, string, string, []string) (string, error) {
			return "", errors.New("buildgate's build scripts fail their own tests on Python 3.8")
		}
		if image, err := toolchainImageFor(dp, context.Background(), &out, repo, base, "docker"); err == nil || image != "" || !strings.Contains(err.Error(), "sandbox image:") {
			t.Errorf("image = %q, %v; want a refusal", image, err)
		}
	})
}

func TestToolchainBuildFailureQuotesTheScriptsVerdict(t *testing.T) {
	out := "#19 40.59 Ran 589 tests in 40.2s\n#19 40.59 buildgate's build scripts fail their own tests on Python 3.8; this project's interpreter cannot run a build\nERROR: failed to solve: exit code: 1\n"
	if got := toolchainBuildFailure(out); !strings.HasPrefix(got, "buildgate's build scripts fail their own tests on Python 3.8") {
		t.Errorf("failure = %q", got)
	}
	if got := toolchainBuildFailure("pull access denied for golang\n"); got != "pull access denied for golang" {
		t.Errorf("failure = %q, want the last line", got)
	}
}
