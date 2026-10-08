package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"

	"buildgate/internal/hostcontrol"
	"buildgate/internal/sessionconfig"
	"buildgate/internal/toolchain"
)

// imageToolchainsScript prints the versions of the toolchains a build may
// use and the image's distribution codename, one per line; a tool the image
// lacks prints nothing a version is read from.
const imageToolchainsScript = `go version 2>/dev/null
python3 --version 2>/dev/null
echo "node $(node --version 2>/dev/null)"
. /etc/os-release 2>/dev/null; echo "codename=$VERSION_CODENAME"`

// imageToolchains is the real dockerBoundary probe.
func (impl realDocker) imageToolchains(ctx context.Context, dockerBinary, image string) (toolchain.Installed, string, error) {
	return imageToolchains(ctx, dockerBinary, image)
}

// warnProjectToolchains prints, as a request is submitted, each toolchain
// version the repository declares that its builds will not have: a Node
// mismatch always, and a Go or Python one only under FACTORYD_AUTOSTART=0,
// since otherwise a build derives the image that has it (toolchainImageFor).
// It never refuses the request, and says nothing when the image cannot be
// asked.
func warnProjectToolchains(dp *deps, ctx context.Context, w io.Writer, repo string, settings sessionconfig.Settings) {
	if settings.SandboxImage == "" {
		return
	}
	reqs, err := toolchain.Detect(repo)
	if err != nil || len(reqs) == 0 {
		return
	}
	installed, _, err := dp.docker.imageToolchains(ctx, settings.SandboxDocker, settings.SandboxImage)
	if err != nil {
		return
	}
	for _, f := range toolchain.Check(reqs, installed) {
		if f.OK || (f.Tool != toolchain.Node && hostcontrol.AutostartEnabled()) {
			continue
		}
		outcome := "its verify command cannot pass there"
		if f.Tool == toolchain.Node {
			outcome = "it may not run there"
		}
		fmt.Fprintf(w, "warning: %s: %s. `factoryd doctor -target-repo %s` prints the fix.\n", toolchainMismatch(f), outcome, repo)
	}
}

// imageToolchains runs image once, without a network, and reads its Go,
// Python and Node versions and its distribution codename.
func imageToolchains(ctx context.Context, dockerBinary, image string) (toolchain.Installed, string, error) {
	out, err := exec.CommandContext(ctx, dockerBinary, "run", "--rm", "--network", "none",
		"--read-only", "--cap-drop=ALL", "--security-opt=no-new-privileges",
		"--entrypoint", "sh", image, "-c", imageToolchainsScript).Output()
	if err != nil {
		return nil, "", fmt.Errorf("read the toolchains of %s: %w", image, err)
	}
	installed, codename := parseImageToolchains(string(out))
	return installed, codename, nil
}

// parseImageToolchains reads imageToolchainsScript's output.
func parseImageToolchains(out string) (toolchain.Installed, string) {
	installed := toolchain.Installed{}
	codename := ""
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		tool := ""
		switch {
		case strings.HasPrefix(line, "codename="):
			codename = strings.TrimPrefix(line, "codename=")
			continue
		case strings.HasPrefix(line, "go version "):
			tool = toolchain.Go
		case strings.HasPrefix(line, "Python "):
			tool = toolchain.Python
		case strings.HasPrefix(line, "node "):
			tool = toolchain.Node
		}
		if v, ok := toolchain.ParseVersion(line); ok && tool != "" {
			installed[tool] = v
		}
	}
	return installed, codename
}

// imageHas names the image's version of a finding's tool.
func imageHas(f toolchain.Finding) string {
	if f.Installed.Parts == nil {
		return "the sandbox image has no " + f.Tool
	}
	return "the sandbox image has " + f.Tool + " " + f.Installed.String()
}

// toolchainMismatch says what the repository declares and what the image has.
func toolchainMismatch(f toolchain.Finding) string {
	return fmt.Sprintf("%s declares %q and %s", f.Source, f.Spec, imageHas(f))
}

// projectImagePlan is what `make project-sandbox-image` adds to its build so
// that the project's image carries the toolchains the project declares.
type projectImagePlan struct {
	BuildArgs []string // KEY=VALUE, one per --build-arg
	Notes     []string // what the build changes, and what it cannot
}

// planProjectImage turns the findings into build arguments of
// internal/sandbox/Dockerfile.project. Go and Python are installed from their
// official images; the Python image is the one built on the base image's
// distribution (codename), since the interpreter links against its libraries.
// Node is not installed: the coding agents run on the image's Node.
func planProjectImage(findings []toolchain.Finding, codename string) (projectImagePlan, error) {
	var plan projectImagePlan
	for _, f := range findings {
		if f.OK {
			continue
		}
		if f.Tool == toolchain.Node {
			plan.Notes = append(plan.Notes, "warning: "+toolchainMismatch(f)+"; the image keeps its own Node, which the coding agents run on")
			continue
		}
		if f.Install.Parts == nil {
			return plan, fmt.Errorf("%s, and the declaration names no version to install", toolchainMismatch(f))
		}
		switch f.Tool {
		case toolchain.Go:
			plan.BuildArgs = append(plan.BuildArgs, "GO_MODE=replace", "GO_IMAGE=golang:"+f.Install.String())
			plan.Notes = append(plan.Notes, fmt.Sprintf("installing Go %s: %s", f.Install, toolchainMismatch(f)))
		case toolchain.Python:
			if len(f.Install.Parts) < 2 || codename == "" {
				return plan, fmt.Errorf("%s, and a Python image needs a major.minor version and the base image's distribution codename (got %q, %q)", toolchainMismatch(f), f.Install, codename)
			}
			plan.BuildArgs = append(plan.BuildArgs, "PYTHON_MODE=replace",
				fmt.Sprintf("PYTHON_IMAGE=python:%s-slim-%s", f.Install, codename),
				"PYTHON_VERSION="+f.Install.MajorMinor().String())
			plan.Notes = append(plan.Notes, fmt.Sprintf("installing Python %s: %s", f.Install, toolchainMismatch(f)))
		}
	}
	return plan, nil
}

// projectImageArgsMain is the hidden `factoryd project-image-args` subcommand
// `make project-sandbox-image` runs: it prints one KEY=VALUE build argument
// per line on stdout, and what they do on stderr.
func projectImageArgsMain(dp *deps, args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("project-image-args", flag.ContinueOnError)
	projectDir := flags.String("project-dir", "", "the project whose toolchain declarations to read")
	baseImage := flags.String("base-image", "", "the worker image the project image is built on")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *projectDir == "" || *baseImage == "" {
		return errors.New("project-image-args needs -project-dir and -base-image")
	}
	reqs, err := toolchain.Detect(*projectDir)
	if err != nil {
		return err
	}
	if len(reqs) == 0 {
		return nil
	}
	installed, codename, err := imageToolchains(context.Background(), dp.docker.dockerBinary(), *baseImage)
	if err != nil {
		return err
	}
	plan, err := planProjectImage(toolchain.Check(reqs, installed), codename)
	for _, note := range plan.Notes {
		fmt.Fprintln(stderr, note)
	}
	if err != nil {
		return err
	}
	for _, arg := range plan.BuildArgs {
		fmt.Fprintln(stdout, arg)
	}
	return nil
}

// doctorCheckProjectToolchains holds the toolchain versions repo declares
// against the sandbox image a build of it would start from: one row per
// declaration. A Go or Python the image lacks is what a build derives an
// image for (toolchainImageFor), and the row says so; under
// FACTORYD_AUTOSTART=0 nothing is derived, the verify command cannot pass,
// and the row fails with the command that builds the image. A Node mismatch
// warns.
func doctorCheckProjectToolchains(ctx context.Context, in doctorInputs) []doctorCheck {
	prefix := "toolchains for " + in.targetRepo
	reqs, err := toolchain.Detect(in.targetRepo)
	if err != nil {
		return []doctorCheck{{Name: prefix, Err: err, Advisory: true}}
	}
	if len(reqs) == 0 || in.sandboxImage == "" {
		return nil
	}
	installed, _, err := imageToolchains(ctx, in.sandboxDocker, in.sandboxImage)
	if err != nil {
		return []doctorCheck{{Name: prefix, Err: err, Advisory: true}}
	}
	var checks []doctorCheck
	for _, f := range toolchain.Check(reqs, installed) {
		name := fmt.Sprintf("%s: %s declares %q", prefix, f.Source, f.Spec)
		switch {
		case f.OK:
			checks = append(checks, doctorCheck{Name: name, Detail: "sandbox image has " + f.Tool + " " + f.Installed.String()})
		case f.Tool == toolchain.Node:
			checks = append(checks, doctorCheck{Name: name, Advisory: true, Err: errors.New(imageHas(f)),
				Fix: "the image's Node also runs the coding agents and is not replaced per project; build on it, or supply your own worker image"})
		case hostcontrol.AutostartEnabled():
			checks = append(checks, doctorCheck{Name: name, Detail: imageHas(f) + "; a build derives an image from it with the declared version, on first use"})
		default:
			checks = append(checks, doctorCheck{Name: name, Err: errors.New(imageHas(f)), Fix: projectImageFix(in)})
		}
	}
	return checks
}

// projectImageFix is the two commands that give repo an image with its
// toolchains.
func projectImageFix(in doctorInputs) string {
	source := in.imageSourceRoot
	if source == "" {
		source = "<buildgate checkout>"
	}
	repo, err := filepath.Abs(in.targetRepo)
	if err != nil {
		repo = in.targetRepo
	}
	return fmt.Sprintf("build this project's image, which installs what it declares: `make -C %s project-sandbox-image PROJECT_DIR=%s BASE_IMAGE=%s`, then `factoryd configure-images -sandbox-image <the ref it prints>`", source, repo, in.sandboxImage)
}
