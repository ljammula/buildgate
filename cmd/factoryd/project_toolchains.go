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
// lacks prints nothing a version is read from. java is asked only when it is
// there, since a shell's "java: not found" carries a line number, and only
// the version lines of java and mvn are read: a JVM prints "Picked up
// JAVA_TOOL_OPTIONS: ..." first when that variable is set.
const imageToolchainsScript = `go version 2>/dev/null
python3 --version 2>/dev/null
echo "node $(node --version 2>/dev/null)"
command -v java >/dev/null 2>&1 && echo "java $(java -version 2>&1 | grep -m 1 ' version ')"
mvn -v 2>/dev/null | grep -m 1 '^Apache Maven '
ruby --version 2>/dev/null
echo "bundler $(bundle --version 2>/dev/null)"
. /etc/os-release 2>/dev/null; echo "codename=$VERSION_CODENAME"`

// imageToolchainLines maps the start of each line imageToolchainsScript
// prints to the tool whose version follows it.
var imageToolchainLines = []struct{ prefix, tool string }{
	{"go version ", toolchain.Go},
	{"Python ", toolchain.Python},
	{"node ", toolchain.Node},
	{"java ", toolchain.Java},
	{"Apache Maven ", toolchain.Maven},
	{"ruby ", toolchain.Ruby},
	{"bundler ", toolchain.Bundler},
}

// imageToolchains is the real dockerBoundary probe.
func (impl realDocker) imageToolchains(ctx context.Context, dockerBinary, image string) (toolchain.Installed, string, error) {
	return imageToolchains(ctx, dockerBinary, image)
}

func (impl realDocker) requiredBinaries(ctx context.Context, dockerBinary, image, user string, names []string) ([]string, error) {
	return probeRequiredBinaries(ctx, dockerBinary, image, user, names)
}

// warnProjectToolchains prints, as a request is submitted, each toolchain
// version the repository declares that its builds will not have: a Node
// mismatch always, and any other one only under FACTORYD_AUTOSTART=0,
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

// imageToolchains runs image once, without a network, and reads the
// versions of the tools in imageToolchainLines and its distribution codename.
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
		if value, found := strings.CutPrefix(line, "codename="); found {
			codename = value
			continue
		}
		for _, known := range imageToolchainLines {
			rest, found := strings.CutPrefix(line, known.prefix)
			if !found {
				continue
			}
			if v, ok := toolchain.ParseVersion(rest); ok {
				if known.tool == toolchain.Java {
					v = toolchain.JavaVersion(v)
				}
				installed[known.tool] = v
			}
			break
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
// internal/sandbox/Dockerfile.project. Go, Python, Java with Maven, and Ruby
// with Bundler are installed from their official images; the Python and Ruby
// images are the ones built on the base image's distribution (codename),
// since the interpreter links against its libraries. Node is not installed:
// the coding agents run on the image's Node.
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
		install := toolchainInstall(f.Tool)
		if install == nil {
			continue
		}
		args, what, err := install(f, codename)
		if err != nil {
			return plan, fmt.Errorf("%s, and %w", toolchainMismatch(f), err)
		}
		if plan.has(args[0]) {
			continue // Java and Maven, or Ruby and Bundler, come from one image
		}
		plan.BuildArgs = append(plan.BuildArgs, args...)
		plan.Notes = append(plan.Notes, fmt.Sprintf("installing %s: %s", what, toolchainMismatch(f)))
	}
	return plan, nil
}

func (plan projectImagePlan) has(arg string) bool {
	for _, have := range plan.BuildArgs {
		if have == arg {
			return true
		}
	}
	return false
}

// toolchainInstall is, for a tool a derived image can install, what turns a
// finding into the build arguments that install its version and a name for
// what they install, the first argument being the stage's MODE switch; nil for
// a tool no derived image installs.
func toolchainInstall(tool string) func(f toolchain.Finding, codename string) ([]string, string, error) {
	switch tool {
	case toolchain.Go:
		return installGo
	case toolchain.Python:
		return installPython
	case toolchain.Java, toolchain.Maven:
		return installJavaAndMaven
	case toolchain.Ruby, toolchain.Bundler:
		return installRubyAndBundler
	}
	return nil
}

func installGo(f toolchain.Finding, _ string) ([]string, string, error) {
	return []string{"GO_MODE=replace", "GO_IMAGE=golang:" + f.Install.String()}, "Go " + f.Install.String(), nil
}

func installPython(f toolchain.Finding, codename string) ([]string, string, error) {
	if len(f.Install.Parts) < 2 || codename == "" {
		return nil, "", fmt.Errorf("a Python image needs a major.minor version and the base image's distribution codename (got %q, %q)", f.Install, codename)
	}
	return []string{"PYTHON_MODE=replace",
		fmt.Sprintf("PYTHON_IMAGE=python:%s-slim-%s", f.Install, codename),
		"PYTHON_VERSION=" + f.Install.MajorMinor().String()}, "Python " + f.Install.String(), nil
}

// mavenImageLine is the Maven release line of the official maven image a JDK
// and Maven are copied from.
const mavenImageLine = "3.9"

// installJavaAndMaven installs the JDK of the finding's major and Maven from
// one official maven image. The JDK is a Temurin build, which runs on any
// glibc distribution, so the image is not chosen by codename.
func installJavaAndMaven(f toolchain.Finding, _ string) ([]string, string, error) {
	major := f.Install.Parts[0]
	return []string{"JAVA_MODE=replace", fmt.Sprintf("JAVA_IMAGE=maven:%s-eclipse-temurin-%d", mavenImageLine, major)},
		fmt.Sprintf("Java %d and Maven %s", major, mavenImageLine), nil
}

// installRubyAndBundler installs the finding's Ruby, with the Bundler it
// ships, from the official ruby image built on the base image's distribution.
func installRubyAndBundler(f toolchain.Finding, codename string) ([]string, string, error) {
	if codename == "" {
		return nil, "", errors.New("a Ruby image needs the base image's distribution codename")
	}
	return []string{"RUBY_MODE=replace", fmt.Sprintf("RUBY_IMAGE=ruby:%s-%s", f.Install, codename)},
		"Ruby " + f.Install.String() + " and Bundler", nil
}

// derivedWhenAbsent reports whether a build installs tool into a derived
// image when the base image has none of it. Go and Python are part of every
// worker image, so an image that reports neither is not one a derivation is
// built on; Java, Maven, Ruby and Bundler are in no worker image.
func derivedWhenAbsent(tool string) bool {
	switch tool {
	case toolchain.Java, toolchain.Maven, toolchain.Ruby, toolchain.Bundler:
		return true
	}
	return false
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
// declaration. A Go, Python, Java, Maven, Ruby or Bundler mismatch is what a
// build derives an image for (toolchainImageFor), and the row says so when
// autostart is enabled. With FACTORYD_AUTOSTART=0 the configured image is
// kept, so a derivable mismatch is an advisory warning. A Go or Python the
// image does not report, or a declaration the planner cannot install, fails.
// A Node mismatch warns.
func doctorCheckProjectToolchains(ctx context.Context, in doctorInputs) []doctorCheck {
	prefix := "toolchains for " + in.targetRepo
	reqs, err := toolchain.Detect(in.targetRepo)
	if err != nil {
		return []doctorCheck{{Name: prefix, Err: err, Advisory: true}}
	}
	if len(reqs) == 0 || in.sandboxImage == "" {
		return nil
	}
	installed, codename, err := imageToolchains(ctx, in.sandboxDocker, in.sandboxImage)
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
		case toolchainInstall(f.Tool) != nil:
			checks = append(checks, derivedToolchainCheck(name, f, codename, in))
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

// derivedToolchainCheck is doctor's row for a toolchain the sandbox image
// lacks: it passes when the project-image planner can derive an image with
// the declared version, as the build does, and fails when it cannot. The
// build that derives is the worker's or the run's own process: one started
// with FACTORYD_AUTOSTART=0 keeps the configured image, and the row says so.
func derivedToolchainCheck(name string, f toolchain.Finding, codename string, in doctorInputs) doctorCheck {
	if f.Installed.Parts == nil && !derivedWhenAbsent(f.Tool) {
		return doctorCheck{Name: name, Err: errors.New(imageHas(f)), Fix: projectImageFix(in)}
	}
	plan, err := planProjectImage([]toolchain.Finding{f}, codename)
	if err == nil && len(plan.BuildArgs) == 0 {
		err = errors.New("the project-image planner produced no derived-image arguments")
	}
	if err != nil {
		return doctorCheck{Name: name, Err: fmt.Errorf("%s; cannot derive a project image: %w", imageHas(f), err), Fix: projectImageFix(in)}
	}
	if !hostcontrol.AutostartEnabled() {
		return doctorCheck{
			Name:     name,
			Err:      fmt.Errorf("%s; a build started with %s=0 keeps configured image %s, while a build with autostart enabled derives an image with the declared version", toolchainMismatch(f), hostcontrol.AutostartEnvVar, in.sandboxImage),
			Advisory: true,
		}
	}
	detail := imageHas(f) + "; a build derives an image from it with the declared version, on first use"
	return doctorCheck{Name: name, Detail: detail}
}
