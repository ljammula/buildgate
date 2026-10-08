package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"buildgate/internal/sessionconfig"
	"buildgate/internal/toolchain"
)

const workerImageToolchains = "go version go1.26.8 linux/arm64\nPython 3.13.15\nnode v22.23.2\ncodename=bookworm\n"

// toolchainRepo is a directory with the given files.
func toolchainRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// toolchainDocker is a `docker` whose `run` prints out.
func toolchainDocker(t *testing.T, out string) string {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "docker")
	script := "#!/bin/sh\ncat <<'OUT'\n" + out + "OUT\n"
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return binary
}

func TestParseImageToolchainsReadsEachToolAndTheCodename(t *testing.T) {
	installed, codename := parseImageToolchains(workerImageToolchains)
	got := map[string]string{}
	for tool, v := range installed {
		got[tool] = v.String()
	}
	want := map[string]string{toolchain.Go: "1.26.8", toolchain.Python: "3.13.15", toolchain.Node: "22.23.2"}
	if !reflect.DeepEqual(got, want) || codename != "bookworm" {
		t.Errorf("parsed %v, %q; want %v, bookworm", got, codename, want)
	}
	// An image with no Node prints "node " and nothing to read a version from.
	if installed, _ := parseImageToolchains("Python 3.13.15\nnode \ncodename=bookworm\n"); len(installed) != 1 {
		t.Errorf("an image with only Python parsed as %v", installed)
	}
}

func TestPlanProjectImageInstallsWhatTheBaseImageLacks(t *testing.T) {
	installed, codename := parseImageToolchains(workerImageToolchains)
	plan := func(files map[string]string) (projectImagePlan, error) {
		reqs, err := toolchain.Detect(toolchainRepo(t, files))
		if err != nil {
			t.Fatal(err)
		}
		return planProjectImage(toolchain.Check(reqs, installed), codename)
	}
	cases := []struct {
		name  string
		files map[string]string
		want  []string
	}{
		{"nothing declared", map[string]string{}, nil},
		{"the base image satisfies both", map[string]string{"go.mod": "module m\n\ngo 1.25\n", "pyproject.toml": "[project]\nrequires-python = \">=3.11\"\n"}, nil},
		{"a newer Go", map[string]string{"go.mod": "module m\n\ngo 1.27\n"}, []string{"GO_MODE=replace", "GO_IMAGE=golang:1.27"}},
		{"the toolchain line's Go", map[string]string{"go.mod": "module m\n\ngo 1.27.0\n\ntoolchain go1.27.2\n"}, []string{"GO_MODE=replace", "GO_IMAGE=golang:1.27.2"}},
		{"another Python, on the base image's distribution", map[string]string{".python-version": "3.12\n"},
			[]string{"PYTHON_MODE=replace", "PYTHON_IMAGE=python:3.12-slim-bookworm", "PYTHON_VERSION=3.12"}},
		{"a pinned patch keeps major.minor as the installed name", map[string]string{".python-version": "3.11.9\n"},
			[]string{"PYTHON_MODE=replace", "PYTHON_IMAGE=python:3.11.9-slim-bookworm", "PYTHON_VERSION=3.11"}},
		{"Node is never replaced", map[string]string{".nvmrc": "20\n"}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := plan(tc.files)
			if err != nil || !reflect.DeepEqual(got.BuildArgs, tc.want) {
				t.Errorf("plan = %v, %v; want %v", got.BuildArgs, err, tc.want)
			}
		})
	}
	node, _ := plan(map[string]string{".nvmrc": "20\n"})
	if len(node.Notes) != 1 || !strings.Contains(node.Notes[0], "keeps its own Node") {
		t.Errorf("a Node mismatch notes = %q, want the reason it is kept", node.Notes)
	}
	if _, err := plan(map[string]string{"pyproject.toml": "[project]\nrequires-python = \"<3.12\"\n"}); err == nil || !strings.Contains(err.Error(), "names no version to install") {
		t.Errorf("an upper bound alone planned %v, want a refusal", err)
	}
}

func TestProjectImageArgsPrintsOneBuildArgumentPerLine(t *testing.T) {
	dp := newTestDeps(t)
	binary := toolchainDocker(t, workerImageToolchains)
	fakeDockerOf(dp).dockerBinaryFn = func() string { return binary }
	repo := toolchainRepo(t, map[string]string{"go.mod": "module m\n\ngo 1.27\n", ".python-version": "3.12\n"})
	var stdout, stderr bytes.Buffer
	if err := projectImageArgsMain(dp, []string{"-project-dir", repo, "-base-image", "worker@sha256:aaaa"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	want := "GO_MODE=replace\nGO_IMAGE=golang:1.27\nPYTHON_MODE=replace\nPYTHON_IMAGE=python:3.12-slim-bookworm\nPYTHON_VERSION=3.12\n"
	if stdout.String() != want {
		t.Errorf("stdout = %q, want %q", stdout.String(), want)
	}
	if !strings.Contains(stderr.String(), "installing Go 1.27") || !strings.Contains(stderr.String(), "installing Python 3.12") {
		t.Errorf("stderr does not say what is installed: %q", stderr.String())
	}
	stdout.Reset()
	if err := projectImageArgsMain(dp, []string{"-project-dir", toolchainRepo(t, nil), "-base-image", "worker@sha256:aaaa"}, &stdout, &stderr); err != nil || stdout.Len() != 0 {
		t.Errorf("a project that declares nothing printed %q, %v", stdout.String(), err)
	}
}

func TestDoctorProjectToolchainRows(t *testing.T) {
	repo := toolchainRepo(t, map[string]string{"go.mod": "module m\n\ngo 1.27\n", "pyproject.toml": "[project]\nrequires-python = \">=3.11\"\n", ".nvmrc": "20\n"})
	in := doctorInputs{targetRepo: repo, sandboxImage: "worker@sha256:aaaa", sandboxDocker: toolchainDocker(t, workerImageToolchains), imageSourceRoot: "/src/buildgate"}
	checks := doctorCheckProjectToolchains(context.Background(), in)
	if len(checks) != 3 {
		t.Fatalf("checks = %+v, want one per declaration", checks)
	}
	goRow, pythonRow, nodeRow := checks[0], checks[1], checks[2]
	if goRow.Err == nil || goRow.Advisory || !strings.Contains(goRow.Err.Error(), "go 1.26.8") ||
		!strings.Contains(goRow.Fix, "make -C /src/buildgate project-sandbox-image PROJECT_DIR="+repo+" BASE_IMAGE=worker@sha256:aaaa") {
		t.Errorf("Go row = %+v, want a failure naming the image's Go and the build command", goRow)
	}
	if pythonRow.Err != nil || !strings.Contains(pythonRow.Detail, "python 3.13.15") {
		t.Errorf("Python row = %+v, want ok", pythonRow)
	}
	if nodeRow.Err == nil || !nodeRow.Advisory {
		t.Errorf("Node row = %+v, want a warning", nodeRow)
	}
	if got := doctorCheckProjectToolchains(context.Background(), doctorInputs{targetRepo: toolchainRepo(t, nil), sandboxImage: "worker@sha256:aaaa", sandboxDocker: in.sandboxDocker}); got != nil {
		t.Errorf("a repo that declares nothing has rows: %+v", got)
	}
}

func TestSubmitWarnsOfAToolchainTheSandboxImageLacks(t *testing.T) {
	dp := newTestDeps(t)
	settings := sessionconfig.Settings{SandboxImage: "worker@sha256:aaaa", SandboxDocker: "docker"}
	repo := toolchainRepo(t, map[string]string{"go.mod": "module m\n\ngo 1.27\n"})
	var out bytes.Buffer
	// The image cannot be asked: nothing is printed, and nothing refuses.
	if warnProjectToolchains(dp, context.Background(), &out, repo, settings); out.Len() != 0 {
		t.Fatalf("an image that cannot be run printed %q", out.String())
	}
	fakeDockerOf(dp).imageToolsFn = func(context.Context, string, string) (toolchain.Installed, string, error) {
		installed, codename := parseImageToolchains(workerImageToolchains)
		return installed, codename, nil
	}
	warnProjectToolchains(dp, context.Background(), &out, repo, settings)
	for _, want := range []string{`warning: go.mod declares "go 1.27" and the sandbox image has go 1.26.8`, "factoryd doctor -target-repo " + repo} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("warning %q lacks %q", out.String(), want)
		}
	}
	out.Reset()
	if warnProjectToolchains(dp, context.Background(), &out, toolchainRepo(t, map[string]string{"go.mod": "module m\n\ngo 1.26\n"}), settings); out.Len() != 0 {
		t.Errorf("a satisfied declaration printed %q", out.String())
	}
}
