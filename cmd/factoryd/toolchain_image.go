package main

import (
	"context"
	"crypto/sha256"
	"embed"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	pi "buildgate/agent/pi"
	"buildgate/internal/hostcontrol"
	"buildgate/internal/sandbox"
	"buildgate/internal/sanitize"
	"buildgate/internal/toolchain"
)

// toolchainImageRepo is where a derived image is pushed, in the local
// registry `make install` starts: a sandbox image is launched by digest, and
// a pushed image is the one that has one.
const toolchainImageRepo = "localhost:5050/buildgate-toolchain"

// toolchainImageTimeout bounds one derived image's pulls, build and push.
const toolchainImageTimeout = 20 * time.Minute

// agentTestVectors are the two files of this package the build scripts' tests
// read, staged beside them by stageAgentTests.
//
//go:embed testdata/sanitize_vectors.json testdata/criterion_key_vectors.json
var agentTestVectors embed.FS

// agentTestNeedsPytest is a test module no worker image can load.
var agentTestNeedsPytest = regexp.MustCompile(`(?m)^(import|from) pytest`)

// agentTestsNeedingTheCheckout read files of this repository that the binary
// does not carry (internal/meter/testdata), and test the Node proxy rather
// than the Python scripts.
var agentTestsNeedingTheCheckout = map[string]bool{"tests/fill_responses_output_test.py": true}

// stageAgentTests writes buildgate's build scripts, their tests and the files
// those tests read under dest, laid out as in this repository, so the suite
// runs where no checkout is: inside a project's image, on the project's
// Python (internal/sandbox/Dockerfile.project). `make project-sandbox-image`
// reaches it as the hidden `factoryd stage-agent-tests <dest>`.
func stageAgentTests(dest string) error {
	skip := func(path string, data []byte) bool {
		return agentTestsNeedingTheCheckout[path] || (strings.HasPrefix(path, "tests/") && strings.HasSuffix(path, ".py") && agentTestNeedsPytest.Match(data))
	}
	if err := copyEmbedded(pi.Scripts, filepath.Join(dest, "agent", "pi"), skip); err != nil {
		return err
	}
	if err := copyEmbedded(pi.Tests, filepath.Join(dest, "agent", "pi"), skip); err != nil {
		return err
	}
	return copyEmbedded(agentTestVectors, filepath.Join(dest, "cmd", "factoryd"), skip)
}

// copyEmbedded writes every file of fsys under dest, but those skip names.
func copyEmbedded(fsys fs.FS, dest string, skip func(path string, data []byte) bool) error {
	return fs.WalkDir(fsys, ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, err := fs.ReadFile(fsys, path)
		if err != nil || skip(path, data) {
			return err
		}
		target := filepath.Join(dest, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
}

// stageAgentTestsMain is the hidden `factoryd stage-agent-tests <dest>`.
func stageAgentTestsMain(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: factoryd stage-agent-tests <dest>")
	}
	return stageAgentTests(args[0])
}

// toolchainImageFor returns the image a build of repo runs in. It is base
// when base has the Go and Python versions repo declares, and otherwise an
// image derived from base that has them, built on first use and reused after:
// a build has no network to fetch a toolchain, so without it the build runs to
// its verify command and fails there. The derived image is base plus official
// toolchain images chosen by the declared version; nothing of the repository
// is copied into it or run to build it.
//
// base is returned unchanged when there is nothing to compare (no
// declaration, an image that cannot be asked, a tool the image does not
// report) and under FACTORYD_AUTOSTART=0, which stops factoryd doing anything
// the operator did not ask for. A declaration that names nothing installable,
// or an image that fails to build, refuses the run: it could not pass.
func toolchainImageFor(dp *deps, ctx context.Context, w io.Writer, repo, base, dockerBinary string) (string, error) {
	if base == "" || !hostcontrol.AutostartEnabled() {
		return base, nil
	}
	reqs, err := toolchain.Detect(repo)
	if err != nil {
		fmt.Fprintf(w, "sandbox image: %s; the toolchain versions this repository declares were not checked\n", sanitize.Line(err.Error()))
		return base, nil
	}
	if len(reqs) == 0 {
		return base, nil
	}
	installed, codename, err := dp.docker.imageToolchains(ctx, dockerBinary, base)
	if err != nil {
		return base, nil
	}
	var known []toolchain.Requirement
	for _, req := range reqs {
		if _, reported := installed[req.Tool]; reported {
			known = append(known, req)
		}
	}
	plan, err := planProjectImage(toolchain.Check(known, installed), codename)
	for _, note := range plan.Notes {
		fmt.Fprintln(w, "sandbox image: "+note)
	}
	if err != nil {
		return "", fmt.Errorf("sandbox image: %w", err)
	}
	if len(plan.BuildArgs) == 0 {
		return base, nil
	}
	image, err := dp.docker.toolchainImage(ctx, dockerBinary, base, plan.BuildArgs)
	if err != nil {
		return "", fmt.Errorf("sandbox image: %w", err)
	}
	fmt.Fprintf(w, "sandbox image: %s (%s with the versions above)\n", image, base)
	return image, nil
}

// toolchainImageKey names the image built from these inputs: the base image,
// the build arguments, the Dockerfile, and, when Python is replaced, the
// build scripts whose tests the build runs on it.
func toolchainImageKey(base string, buildArgs []string) string {
	sum := sha256.New()
	fmt.Fprintln(sum, base)
	sorted := append([]string(nil), buildArgs...)
	sort.Strings(sorted)
	fmt.Fprintln(sum, strings.Join(sorted, "\n"))
	sum.Write(sandbox.ProjectDockerfile)
	if replacesPython(buildArgs) {
		_ = fs.WalkDir(pi.Scripts, ".", func(path string, entry fs.DirEntry, err error) error {
			if err == nil && !entry.IsDir() {
				data, _ := fs.ReadFile(pi.Scripts, path)
				fmt.Fprintln(sum, path)
				sum.Write(data)
			}
			return nil
		})
	}
	return fmt.Sprintf("%x", sum.Sum(nil)[:8])
}

func replacesPython(buildArgs []string) bool {
	for _, arg := range buildArgs {
		if arg == "PYTHON_MODE=replace" {
			return true
		}
	}
	return false
}

var imageDigestPattern = regexp.MustCompile(`sha256:[0-9a-f]{64}`)

// toolchainImage is the real dockerBoundary build: the `toolchains` stage of
// Dockerfile.project on base with buildArgs, pushed to the local registry and
// returned by digest. An image already built from the same inputs is reused.
func (impl realDocker) toolchainImage(ctx context.Context, dockerBinary, base string, buildArgs []string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, toolchainImageTimeout)
	defer cancel()
	tag := toolchainImageRepo + ":" + toolchainImageKey(base, buildArgs)
	if out, err := exec.CommandContext(ctx, dockerBinary, "image", "inspect", "--format", "{{range .RepoDigests}}{{println .}}{{end}}", tag).Output(); err == nil {
		for _, ref := range strings.Fields(string(out)) {
			if strings.HasPrefix(ref, toolchainImageRepo+"@") {
				return ref, nil
			}
		}
	}
	dir, err := os.MkdirTemp("", "buildgate-toolchain-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	dockerfile := filepath.Join(dir, "Dockerfile.project")
	if err := os.WriteFile(dockerfile, sandbox.ProjectDockerfile, 0o644); err != nil {
		return "", err
	}
	buildContext := filepath.Join(dir, "context")
	if err := os.MkdirAll(buildContext, 0o755); err != nil {
		return "", err
	}
	if replacesPython(buildArgs) {
		if err := stageAgentTests(filepath.Join(buildContext, ".buildgate-agent")); err != nil {
			return "", err
		}
	}
	args := []string{"build", "-f", dockerfile, "--target", "toolchains", "--label", "buildgate.image=toolchain", "-t", tag, "--build-arg", "BASE_IMAGE=" + base}
	for _, arg := range buildArgs {
		args = append(args, "--build-arg", arg)
	}
	if out, err := exec.CommandContext(ctx, dockerBinary, append(args, buildContext)...).CombinedOutput(); err != nil {
		return "", fmt.Errorf("build the image with this repository's toolchains: %v: %s", err, toolchainBuildFailure(string(out)))
	}
	out, err := exec.CommandContext(ctx, dockerBinary, "push", tag).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("push %s to the local registry (`make install` starts it): %v: %s", tag, err, sanitize.Line(hostcontrol.LastLine(string(out))))
	}
	digests := imageDigestPattern.FindAllString(string(out), -1)
	if len(digests) == 0 {
		return "", fmt.Errorf("push %s printed no digest", tag)
	}
	return toolchainImageRepo + "@" + digests[len(digests)-1], nil
}

// toolchainBuildFailure is the line of a failed build's output that says why:
// the build scripts' verdict on the project's Python when there is one, else
// the last line.
func toolchainBuildFailure(out string) string {
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "buildgate's build scripts fail their own tests") {
			if _, verdict, found := strings.Cut(line, "buildgate's"); found {
				return sanitize.Line("buildgate's" + verdict)
			}
		}
	}
	return sanitize.Line(hostcontrol.LastLine(out))
}
