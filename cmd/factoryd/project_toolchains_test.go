package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"buildgate/internal/hostcontrol"
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
	if err := writeExecutable(binary, []byte(script), 0o755); err != nil {
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
	// What derived images print, as captured from them.
	for out, want := range map[string]map[string]string{
		"java openjdk version \"21.0.12.1\" 2026-08-18 LTS\nApache Maven 3.9.16 (2bdd9fddda4b155ebf8000e807eb73fd829a51d5)\n": {toolchain.Java: "21.0.12", toolchain.Maven: "3.9.16"},
		"ruby 4.0.9 (2026-10-23 revision 3e93b23afc) +PRISM [aarch64-linux]\nbundler 4.0.20\n":                                {toolchain.Ruby: "4.0.9", toolchain.Bundler: "4.0.20"},
		"ruby 3.3.13 (2026-10-23 revision af48f08f21) [aarch64-linux]\nbundler Bundler version 2.5.22\n":                      {toolchain.Ruby: "3.3.13", toolchain.Bundler: "2.5.22"},
	} {
		installed, _ := parseImageToolchains(out)
		got := map[string]string{}
		for tool, v := range installed {
			got[tool] = v.String()
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("parsed %v from %q, want %v", got, out, want)
		}
	}
}

func TestDoctorProjectToolchainRowsForAPomTheImageHasNoJDKFor(t *testing.T) {
	repo := toolchainRepo(t, map[string]string{"pom.xml": "<project/>"})
	in := doctorInputs{targetRepo: repo, sandboxImage: "worker@sha256:aaaa", sandboxDocker: toolchainDocker(t, workerImageToolchains), imageSourceRoot: "/src/buildgate"}
	t.Setenv(hostcontrol.AutostartEnvVar, "1")
	checks := doctorCheckProjectToolchains(context.Background(), in)
	if len(checks) != 2 {
		t.Fatalf("checks = %+v, want a JDK row and a Maven row", checks)
	}
	for _, row := range checks {
		if row.Err != nil || !strings.Contains(row.Detail, "the sandbox image has no") || !strings.Contains(row.Detail, "a build derives an image") {
			t.Errorf("row = %+v, want a pass naming the derived image", row)
		}
	}
	t.Setenv(hostcontrol.AutostartEnvVar, "0")
	for _, row := range doctorCheckProjectToolchains(context.Background(), in) {
		if row.Err == nil || !row.Advisory || !strings.Contains(row.Err.Error(), "keeps configured image") {
			t.Errorf("row = %+v, want an autostart-off advisory", row)
		}
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
		{"a JDK and Maven, from one image, once", map[string]string{"pom.xml": "<project/>"},
			[]string{"JAVA_MODE=replace", "JAVA_IMAGE=maven:3.9-eclipse-temurin-21"}},
		{"a pinned JDK", map[string]string{"pom.xml": "<project/>", ".java-version": "17\n"},
			[]string{"JAVA_MODE=replace", "JAVA_IMAGE=maven:3.9-eclipse-temurin-17"}},
		{"Ruby and Bundler, on the base image's distribution", map[string]string{"Gemfile": ""},
			[]string{"RUBY_MODE=replace", "RUBY_IMAGE=ruby:4.0-bookworm"}},
		{"a pinned Ruby", map[string]string{"Gemfile": "ruby '3.3.6'\n"},
			[]string{"RUBY_MODE=replace", "RUBY_IMAGE=ruby:3.3.6-bookworm"}},
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

func TestPlanProjectImageReplacesTheJDKOrRubyWhenEitherHalfIsMissing(t *testing.T) {
	plan := func(files map[string]string, out, codename string) (projectImagePlan, error) {
		reqs, err := toolchain.Detect(toolchainRepo(t, files))
		if err != nil {
			t.Fatal(err)
		}
		installed, _ := parseImageToolchains(out)
		return planProjectImage(toolchain.Check(reqs, installed), codename)
	}
	pom := map[string]string{"pom.xml": "<project/>", ".java-version": "17\n"}
	if got, err := plan(pom, "java openjdk version \"21.0.5\"\nApache Maven 3.9.9\n", "bookworm"); err != nil || got.BuildArgs != nil {
		t.Errorf("an image with a newer JDK and Maven planned %v, %v; want nothing", got.BuildArgs, err)
	}
	if got, err := plan(pom, "java openjdk version \"21.0.5\"\n", "bookworm"); err != nil ||
		!reflect.DeepEqual(got.BuildArgs, []string{"JAVA_MODE=replace", "JAVA_IMAGE=maven:3.9-eclipse-temurin-17"}) || !strings.Contains(strings.Join(got.Notes, "\n"), `pom.xml declares "mvn" and the sandbox image has no maven`) {
		t.Errorf("an image with no mvn planned %v, %v, %q", got.BuildArgs, err, got.Notes)
	}
	if got, err := plan(pom, "java openjdk version \"11.0.20\"\nApache Maven 3.9.9\n", "bookworm"); err != nil || len(got.BuildArgs) != 2 || len(got.Notes) != 1 {
		t.Errorf("an image with an older JDK planned %v, %v, %q", got.BuildArgs, err, got.Notes)
	}
	java15 := map[string]string{"pom.xml": "<project/>", ".java-version": "15\n"}
	if got, err := plan(java15, "", "bookworm"); err != nil || !reflect.DeepEqual(got.BuildArgs, []string{"JAVA_MODE=replace", "JAVA_IMAGE=maven:3.9-eclipse-temurin-17"}) {
		t.Errorf("a Java 15 pin planned %v, %v; want the Java 17 image", got.BuildArgs, err)
	}
	gemfile := map[string]string{"Gemfile": "", ".ruby-version": "3.3\n"}
	if got, err := plan(gemfile, "ruby 3.3.6 (2024-11-05 revision 75015d4c1f) [aarch64-linux]\nbundler 2.5.22\n", "bookworm"); err != nil || got.BuildArgs != nil {
		t.Errorf("an image with Ruby 3.3 and Bundler planned %v, %v; want nothing", got.BuildArgs, err)
	}
	if got, err := plan(gemfile, "ruby 3.3.6 (2024-11-05 revision 75015d4c1f) [aarch64-linux]\nbundler \n", "bookworm"); err != nil ||
		!reflect.DeepEqual(got.BuildArgs, []string{"RUBY_MODE=replace", "RUBY_IMAGE=ruby:3.3-bookworm"}) {
		t.Errorf("an image with no bundle planned %v, %v", got.BuildArgs, err)
	}
	if _, err := plan(gemfile, "", ""); err == nil || !strings.Contains(err.Error(), "distribution codename") {
		t.Errorf("a Ruby with no codename planned %v, want a refusal", err)
	}
}

// The probe script runs in a real shell over stand-in executables that print
// what the real tools print, and its output parses back to their versions.
func TestImageToolchainsScriptRoundTrip(t *testing.T) {
	stubs := map[string]string{
		"java":   `echo 'Picked up JAVA_TOOL_OPTIONS: -Djava.io.tmpdir=/scratch/java-tmp -Xss4m' >&2; echo 'openjdk version "1.8.0_432"' >&2; echo 'OpenJDK Runtime Environment (Temurin)(build 1.8.0_432-b06)' >&2`,
		"mvn":    `echo 'Picked up JAVA_TOOL_OPTIONS: -Xss4m'; echo 'Apache Maven 3.9.16 (2bdd9fddda4b155ebf8000e807eb73fd829a51d5)'; echo 'Maven home: /usr/share/maven'`,
		"ruby":   `echo 'ruby 4.0.9 (2026-10-23 revision 3e93b23afc) +PRISM [aarch64-linux]'`,
		"bundle": `echo '4.0.20'`,
		"node":   `echo 'v22.23.2'`,
	}
	bin := t.TempDir()
	for name, body := range stubs {
		if err := writeExecutable(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	grep, err := exec.LookPath("grep")
	if err != nil {
		t.Skip("no grep on this host")
	}
	if err := os.Symlink(grep, filepath.Join(bin, "grep")); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", "-c", imageToolchainsScript)
	cmd.Env = []string{"PATH=" + bin}
	// The exit status is the sourcing of /etc/os-release, which a test host
	// may not have; every tool line is printed before it.
	out, _ := cmd.Output()
	installed, _ := parseImageToolchains(string(out))
	got := map[string]string{}
	for tool, v := range installed {
		got[tool] = v.String()
	}
	want := map[string]string{toolchain.Java: "8.0", toolchain.Maven: "3.9.16", toolchain.Ruby: "4.0.9", toolchain.Bundler: "4.0.20", toolchain.Node: "22.23.2"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parsed %v from %q; want %v", got, out, want)
	}
	// With none of them, nothing is read: no shell error is taken for a version.
	cmd = exec.Command("/bin/sh", "-c", imageToolchainsScript)
	cmd.Env = []string{"PATH=" + t.TempDir()}
	out, _ = cmd.Output()
	if installed, _ := parseImageToolchains(string(out)); len(installed) != 0 {
		t.Errorf("an image with no tools parsed as %v from %q", installed, out)
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
	t.Setenv(hostcontrol.AutostartEnvVar, "0")
	repo := toolchainRepo(t, map[string]string{"go.mod": "module m\n\ngo 1.27\n", "pyproject.toml": "[project]\nrequires-python = \">=3.14\"\n", ".nvmrc": "20\n"})
	in := doctorInputs{targetRepo: repo, sandboxImage: "worker@sha256:aaaa", sandboxDocker: toolchainDocker(t, workerImageToolchains), imageSourceRoot: "/src/buildgate"}
	checks := doctorCheckProjectToolchains(context.Background(), in)
	if len(checks) != 3 {
		t.Fatalf("checks = %+v, want one per declaration", checks)
	}
	goRow, pythonRow, nodeRow := checks[0], checks[1], checks[2]
	for _, row := range []doctorCheck{goRow, pythonRow} {
		if row.Err == nil || !row.Advisory || !strings.Contains(row.Err.Error(), "FACTORYD_AUTOSTART=0") || !strings.Contains(row.Err.Error(), "keeps configured image") || !strings.Contains(row.Err.Error(), "autostart enabled derives") {
			t.Errorf("toolchain row = %+v, want an autostart-off advisory", row)
		}
	}
	for _, want := range []string{`go.mod declares "go 1.27"`, "go 1.26.8", `pyproject.toml declares "requires-python >=3.14"`, "python 3.13.15"} {
		if !strings.Contains(goRow.Err.Error()+pythonRow.Err.Error(), want) {
			t.Errorf("toolchain warnings = %q, want %q", goRow.Err.Error()+pythonRow.Err.Error(), want)
		}
	}
	if nodeRow.Err == nil || !nodeRow.Advisory {
		t.Errorf("Node row = %+v, want a warning", nodeRow)
	}
	if got := doctorCheckProjectToolchains(context.Background(), doctorInputs{targetRepo: toolchainRepo(t, nil), sandboxImage: "worker@sha256:aaaa", sandboxDocker: in.sandboxDocker}); got != nil {
		t.Errorf("a repo that declares nothing has rows: %+v", got)
	}
	// With autostart a build derives the image, so the Go row is not a failure.
	t.Setenv(hostcontrol.AutostartEnvVar, "1")
	if derived := doctorCheckProjectToolchains(context.Background(), in); derived[0].Err != nil || derived[0].Advisory || !strings.Contains(derived[0].Detail, "a build derives an image") || derived[1].Err != nil || derived[1].Advisory {
		t.Errorf("toolchain rows with autostart = %+v, want passing rows with what a build does", derived)
	}
}

func TestDoctorProjectToolchainRowFailsWhenBaseImageDoesNotReportGo(t *testing.T) {
	t.Setenv(hostcontrol.AutostartEnvVar, "1")
	repo := toolchainRepo(t, map[string]string{"go.mod": "module m\n\ngo 1.27\n"})
	in := doctorInputs{targetRepo: repo, sandboxImage: "worker@sha256:aaaa", sandboxDocker: toolchainDocker(t, "Python 3.13.15\nnode v22.23.2\ncodename=bookworm\n"), imageSourceRoot: "/src/buildgate"}
	checks := doctorCheckProjectToolchains(context.Background(), in)
	if len(checks) != 1 || checks[0].Err == nil || checks[0].Advisory || !strings.Contains(checks[0].Err.Error(), "sandbox image has no go") || !strings.Contains(checks[0].Fix, "project-sandbox-image") {
		t.Fatalf("checks = %+v, want a hard failure for an unreported Go toolchain", checks)
	}
}

func TestDoctorProjectToolchainRowFailsWhenNoAutomaticImageCanBeDerived(t *testing.T) {
	t.Setenv(hostcontrol.AutostartEnvVar, "0")
	repo := toolchainRepo(t, map[string]string{"pyproject.toml": "[project]\nrequires-python = \"<3.12\"\n"})
	in := doctorInputs{targetRepo: repo, sandboxImage: "worker@sha256:aaaa", sandboxDocker: toolchainDocker(t, workerImageToolchains), imageSourceRoot: "/src/buildgate"}
	checks := doctorCheckProjectToolchains(context.Background(), in)
	if len(checks) != 1 || checks[0].Err == nil || checks[0].Advisory || !strings.Contains(checks[0].Err.Error(), "no version to install") {
		t.Fatalf("checks = %+v, want a hard failure naming the missing install version", checks)
	}
}

func TestSubmitWarnsOfAToolchainTheSandboxImageLacks(t *testing.T) {
	t.Setenv(hostcontrol.AutostartEnvVar, "0")
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
	// With autostart a build derives the Go image: only Node is worth a line.
	t.Setenv(hostcontrol.AutostartEnvVar, "1")
	warnProjectToolchains(dp, context.Background(), &out, toolchainRepo(t, map[string]string{"go.mod": "module m\n\ngo 1.27\n", ".nvmrc": "20\n"}), settings)
	if strings.Contains(out.String(), "go.mod") || !strings.Contains(out.String(), ".nvmrc") {
		t.Errorf("with autostart the warnings are %q, want Node's only", out.String())
	}
}
