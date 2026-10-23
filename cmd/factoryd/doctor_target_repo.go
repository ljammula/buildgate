package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"buildgate/internal/composeservices"
	"buildgate/internal/languageprofile"
	"buildgate/internal/projectconfig"
	"buildgate/internal/requestsubmit"
	"buildgate/internal/run"
	"buildgate/internal/sandbox"
	"buildgate/internal/sessionconfig"
)

// doctorRegisterTargetRepoFlag registers -target-repo directly on flags,
// like doctorRegisterListModelsFlag.
func doctorRegisterTargetRepoFlag(flags *flag.FlagSet) *string {
	return flags.String("target-repo", "", "A repository checkout to check as a run and a submit would: its verify command, root AGENTS.md, preflight, compose file and declared Go, Python and Node versions. Empty skips these checks.")
}

// composeServicesSpecAtCommit loads the compose spec a run against dir at
// commit would use, with the operator's compose_services_* settings. A
// run and doctor -target-repo share it so their verdicts
// cannot drift.
func composeServicesSpecAtCommit(settings sessionconfig.Settings, dir, commit string) (sandbox.ComposeServicesSpec, error) {
	spec, err := sandbox.LoadComposeServicesSpecFromGit(dir, commit,
		composeservices.Options{AllowedImageRegistries: settings.ComposeServicesAllowedRegistries, MaxServices: settings.ComposeServicesMaxServices, RequireDigest: settings.ComposeServicesRequireDigest},
		composeservices.SynthesizeOptions{MemoryLimit: settings.ComposeServicesMemory, CPUs: settings.ComposeServicesCPUs, PIDsLimit: composeservices.DefaultPIDsLimit},
		settings.ComposeServicesReadyTimeout,
	)
	if err != nil {
		return sandbox.ComposeServicesSpec{}, err
	}
	spec.WorkerEnvironment = settings.ComposeServicesWorkerEnv
	return spec, nil
}

// doctorTargetRepoChecks reports the compose-services verdict a run
// against repo's HEAD would reach: one check per service (FAIL for a
// rejected one, warn for a build-skipped one), a FAIL for a whole-file
// rejection, and an advisory memory check.
func doctorTargetRepoChecks(ctx context.Context, in doctorInputs) []doctorCheck {
	prefix := "compose services in " + in.targetRepo
	if !in.composeServices {
		return []doctorCheck{{Name: prefix + ": compose_services is off, so a run launches none of them"}}
	}
	// The loader reads each candidate file with `git show` and treats any
	// failure as "absent", so a path that is not a checkout would read as
	// a repo with no compose file. Confirm the checkout first.
	if out, err := exec.CommandContext(ctx, "git", "-C", in.targetRepo, "rev-parse", "--verify", "HEAD").CombinedOutput(); err != nil {
		return []doctorCheck{{Name: prefix, Err: fmt.Errorf("not a git checkout with a HEAD commit: %s", strings.TrimSpace(string(out))),
			Fix: "pass a git checkout of the target repo to -target-repo"}}
	}
	spec, err := composeServicesSpecAtCommit(in.settings, in.targetRepo, "HEAD")
	if err != nil {
		return []doctorCheck{{Name: prefix, Err: err}}
	}
	if len(spec.ComposeYAML) == 0 {
		return []doctorCheck{{Name: prefix + ": no compose file at HEAD, so a run launches no sidecars"}}
	}
	verdict, err := sandbox.ValidateComposeServices(spec)
	if err != nil {
		return []doctorCheck{{Name: prefix, Err: err}}
	}

	var checks []doctorCheck
	for _, s := range verdict.Services {
		checks = append(checks, doctorCheck{Name: fmt.Sprintf("compose service %s (%s): %s", s.Name, s.Image, strings.Join(sandbox.ComposeServiceWorkerEnv(s), " "))})
	}
	for _, r := range verdict.Rejected {
		fix := "a run against this repo halts before its build until this is fixed in the compose file or the compose_services_* settings"
		if r.AllowRegistry != "" {
			fix += "; `factoryd doctor -target-repo " + in.targetRepo + " -fix` offers to add " + r.AllowRegistry + " to compose_services_allowed_registries"
		}
		checks = append(checks, doctorCheck{Name: "compose service " + r.Service, Err: fmt.Errorf("rejected: %s", r.Reason), Fix: fix})
	}
	for _, r := range verdict.Skipped {
		checks = append(checks, doctorCheck{Name: "compose service " + r.Service, Err: fmt.Errorf("skipped: %s", r.Reason), Advisory: true})
	}
	if verdict.Reason != "" && len(verdict.Rejected) == 0 {
		// Whole-file rejections (dangling depends_on, BG_SERVICE_*
		// collision) name no single rejected service above.
		checks = append(checks, doctorCheck{Name: prefix, Err: fmt.Errorf("rejected: %s", verdict.Reason),
			Fix: "a run against this repo halts before its build until the compose file is fixed"})
	}
	if verdict.Reason == "" {
		checks = append(checks, doctorCheckComposeMemory(ctx, in.sandboxDocker, in.settings, verdict.Services))
	}
	if verdict.Reason == "" && len(verdict.Forwards) > 0 {
		described := make([]string, len(verdict.Forwards))
		for i, forward := range verdict.Forwards {
			port, target, _ := strings.Cut(forward, "=")
			described[i] = "localhost:" + port + " -> " + target
		}
		checks = append(checks, doctorCheck{Name: "compose ports published on the worker's localhost: " + strings.Join(described, ", ")})
		if in.sandboxImage != "" {
			check := doctorCheckExecutableInImage(ctx, in.sandboxDocker, in.sandboxImage, "bg-forward", "bg-forward")
			if check.Err != nil {
				check.Fix = "rebuild the worker image from this factoryd's checkout (make install) and re-point sandbox_image: without bg-forward a run against this repo cannot start its worker"
			}
			checks = append(checks, check)
		}
	}
	return checks
}

// doctorCheckComposeMemory warns (advisory) when the worker's memory limit
// plus each sidecar's effective limit (its own narrower mem_limit, else
// compose_services_memory) exceeds the Docker VM's total memory. These are
// limits, not measured use: the run still starts and usually fits (the
// worker rarely uses its whole limit), but nothing stops the containers
// together from growing past the VM, where the kernel OOM-kills one. The
// wording says so, since the warning fires on every run of such a setup.
func doctorCheckComposeMemory(ctx context.Context, dockerBinary string, settings sessionconfig.Settings, services []composeservices.ServiceSpec) doctorCheck {
	name := fmt.Sprintf("memory limits: worker (%s) + %d sidecar(s) within Docker's memory", settings.SandboxMemory, len(services))
	worker, err := composeservices.MemoryBytes(settings.SandboxMemory)
	if err != nil {
		return doctorCheck{Name: name, Err: fmt.Errorf("sandbox_memory: %w", err), Advisory: true}
	}
	var sidecars int64
	for _, s := range services {
		limit, err := composeservices.EffectiveMemoryBytes(s, settings.ComposeServicesMemory)
		if err != nil {
			return doctorCheck{Name: name, Err: fmt.Errorf("compose_services_memory: %w", err), Advisory: true}
		}
		sidecars += limit
	}
	out, err := exec.CommandContext(ctx, dockerBinary, "info", "--format", "{{.MemTotal}}").Output()
	if err != nil {
		return doctorCheck{Name: name, Err: fmt.Errorf("%s info: %w", dockerBinary, err), Advisory: true}
	}
	total, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return doctorCheck{Name: name, Err: fmt.Errorf("parse Docker MemTotal %q: %w", strings.TrimSpace(string(out)), err), Advisory: true}
	}
	if need := worker + sidecars; need > total {
		return doctorCheck{Name: name, Advisory: true,
			Err: fmt.Errorf("limits add up to %s (worker %s + sidecars %s), Docker has %s -- limits, not measured use: a run can still fit, but if every container grew to its limit one would be OOM-killed", formatGiB(need), formatGiB(worker), formatGiB(sidecars), formatGiB(total)),
			Fix: "only if a run hits an OOM kill: lower compose_services_memory (or sandbox_memory), declare a smaller mem_limit per service, or give the Docker VM more memory"}
	}
	return doctorCheck{Name: name}
}

func formatGiB(b int64) string {
	return fmt.Sprintf("%.1f GiB", float64(b)/(1<<30))
}

// doctorTargetRepoSubmitChecks reports what `factoryd submit` would refuse
// against repo with no per-request flags: a missing verify command (a
// warning, since -verify-command supplies one), a root AGENTS.md that is
// missing or empty at HEAD (a FAIL) and the project-bootstrap
// preflight under the repo's own .factory.yml profile (a FAIL, the same
// checks submit runs). Without it, doctor -target-repo passed a repo that
// submit then refused.
func doctorTargetRepoSubmitChecks(repo string) []doctorCheck {
	abs, err := filepath.Abs(repo)
	if err != nil {
		return []doctorCheck{{Name: "submit checks for " + repo, Err: err}}
	}
	cfg, _, err := projectconfig.Load(abs)
	if err != nil {
		return []doctorCheck{{Name: "submit checks for " + repo, Err: err, Fix: "fix the repo's committed " + projectconfig.FileName}}
	}
	if cfg == nil {
		cfg = &projectconfig.Config{}
	}

	var checks []doctorCheck
	if err := requestsubmit.RequireAgentsFile(abs); err != nil {
		checks = append(checks, doctorCheck{Name: run.RootInstructionFile + " for " + repo, Err: err,
			Fix: "run `factoryd doctor -target-repo <repo> -fix` to draft it in the sandbox, review and commit the uncommitted file, then run this again; submit, quickstart and a run read committed HEAD"})
	} else {
		checks = append(checks, doctorCheck{Name: run.RootInstructionFile + " for " + repo + ": committed at HEAD"})
	}
	if cfg.VerifyCommand == "" {
		checks = append(checks, doctorCheck{Name: "verify command for " + repo, Advisory: true,
			Err: fmt.Errorf("no verify_command in its committed %s", projectconfig.FileName),
			Fix: fmt.Sprintf("pass -verify-command '<cmd>' on each submit, or commit verify_command: \"<cmd>\" in %s", projectconfig.FileName)})
	} else {
		checks = append(checks, doctorCheck{Name: fmt.Sprintf("verify command for %s: %s", repo, cfg.VerifyCommand)})
	}

	name := "submit preflight for " + repo
	if cfg.PreflightProfile == projectconfig.PreflightProfileBrownfield {
		return append(checks, doctorCheck{Name: name + ": preflight_profile brownfield"})
	}
	if failures := requestsubmit.ProjectBootstrapFailures(abs, cfg.PreflightProfile); len(failures) > 0 {
		return append(checks, doctorCheck{Name: name, Err: fmt.Errorf("%s", strings.Join(failures, " | ")),
			Fix: fmt.Sprintf("commit preflight_profile: brownfield in %s (or submit with -preflight-profile brownfield) if this repo has not adopted spec/spec.md, spec/contract.md and ARCHITECTURE.md", projectconfig.FileName)})
	}
	return append(checks, doctorCheck{Name: name + ": spec/spec.md, spec/contract.md and ARCHITECTURE.md pass"})
}

// doctorCheckRequiredBinaries checks a repo's committed required_binaries
// against the configured worker image. It starts the operator-selected image
// as the resolved worker identity with a fixed shell probe: no network,
// mounts, credentials, writable filesystem, or added capabilities are provided.
func doctorCheckRequiredBinaries(ctx context.Context, in doctorInputs) []doctorCheck {
	if in.targetRepo == "" {
		return nil
	}
	cfg, _, err := projectconfig.Load(in.targetRepo)
	if err != nil {
		return []doctorCheck{{Name: "required binaries for " + in.targetRepo, Err: err}}
	}
	var requested []string
	configured := make(map[string]bool)
	if cfg != nil {
		requested = append(requested, cfg.RequiredBinaries...)
		for _, name := range cfg.RequiredBinaries {
			configured[name] = true
		}
	}
	if matches, detectErr := languageprofile.Detect(in.targetRepo); detectErr == nil && len(matches) == 1 && matches[0].Profile.ToolchainProvisioning != languageprofile.ToolchainAutomatic {
		requested = append(requested, matches[0].Profile.RequiredBinaries...)
	}
	requested = uniqueProfileBinaries(requested)
	if len(requested) == 0 {
		return nil
	}
	if in.sandboxImage == "" {
		return []doctorCheck{{Name: "required binaries for " + in.targetRepo, Advisory: true,
			Err: errors.New("no sandbox image is configured to check these executables"),
			Fix: "configure an operator-selected sandbox_image, then run doctor -target-repo again"}}
	}
	probeCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	user, _ := sandbox.ResolveDefaultWorkerIdentity(in.settings.SandboxUser, in.settings.SandboxWorkerUID, os.Getgid())
	missingNames, err := probeRequiredBinaries(probeCtx, in.sandboxDocker, in.sandboxImage, user, requested)
	if err != nil {
		return []doctorCheck{{Name: "required binaries in configured base sandbox image (" + in.sandboxImage + ")",
			Err: fmt.Errorf("could not inspect image: %w", err),
			Fix: "confirm Docker is reachable and the configured sandbox_image can start a shell probe"}}
	}
	missing := map[string]bool{}
	for _, name := range missingNames {
		missing[name] = true
	}
	checks := make([]doctorCheck, 0, len(requested))
	for _, name := range requested {
		check := doctorCheck{Name: fmt.Sprintf("required executable %s available as configured/default isolated worker %s in base image (%s)", name, user, in.sandboxImage)}
		if !configured[name] {
			check.Name = fmt.Sprintf("profile executable hint %s available as configured/default isolated worker %s in base image (%s)", name, user, in.sandboxImage)
			check.Advisory = true
		}
		if missing[name] {
			check.Err = fmt.Errorf("%q is not executable on PATH as the configured/default isolated worker identity %s", name, user)
			if check.Advisory {
				check.Fix = fmt.Sprintf("profile hint only: add %s to .factory.yml required_binaries if the repository's reviewed commands need it, then ensure it exists in the final worker image", name)
			} else {
				check.Fix = fmt.Sprintf("install %s in the operator-selected base sandbox image, then run doctor -target-repo again; this is an early base-image check, while each run checks required_binaries in its final image", name)
			}
		}
		checks = append(checks, check)
	}
	return checks
}

func uniqueProfileBinaries(values []string) []string {
	seen := make(map[string]bool, len(values))
	unique := make([]string, 0, len(values))
	for _, value := range values {
		if !seen[value] {
			seen[value] = true
			unique = append(unique, value)
		}
	}
	return unique
}

// requiredBinaryProbe checks PATH entries directly so shell built-ins do not
// count as executables, while an executable sharing a built-in's name does.
const requiredBinaryProbe = `set -f; for name do found=; old_ifs=$IFS; IFS=:; for dir in $PATH; do [ -n "$dir" ] || dir=.; if [ -f "$dir/$name" ] && [ -x "$dir/$name" ]; then found=1; break; fi; done; IFS=$old_ifs; [ "$found" = 1 ] || printf '%s\n' "$name"; done`

const requiredBinaryProbeOutputLimit = 16 * 1024

type requiredBinaryProbeOutput struct {
	mu       sync.Mutex
	data     []byte
	exceeded bool
	cancel   context.CancelFunc
}

func (out *requiredBinaryProbeOutput) Write(p []byte) (int, error) {
	out.mu.Lock()
	defer out.mu.Unlock()
	remaining := requiredBinaryProbeOutputLimit - len(out.data)
	if remaining > 0 {
		if len(p) > remaining {
			out.data = append(out.data, p[:remaining]...)
		} else {
			out.data = append(out.data, p...)
		}
	}
	if len(p) > remaining && !out.exceeded {
		out.exceeded = true
		out.cancel()
	}
	return len(p), nil
}

func (out *requiredBinaryProbeOutput) snapshot() ([]byte, bool) {
	out.mu.Lock()
	defer out.mu.Unlock()
	return append([]byte(nil), out.data...), out.exceeded
}

func probeRequiredBinaries(ctx context.Context, dockerBinary, image, user string, names []string) ([]string, error) {
	containerName, err := requiredBinaryProbeContainerName()
	if err != nil {
		return nil, fmt.Errorf("name required-binary probe container: %w", err)
	}
	args := []string{"run", "--rm", "--network=none", "--read-only", "--cap-drop=ALL",
		"--name", containerName, "--security-opt=no-new-privileges", "--user=" + user, "--pids-limit=32", "--memory=128m",
		"--env", "PATH=" + sandbox.WorkerPath, "--entrypoint", "sh", image, "-c", requiredBinaryProbe, "required-binaries"}
	args = append(args, names...)
	commandCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	// The probe's answer is its stdout; Docker writes everything else (pull
	// progress on an image's first use, its own errors) to stderr, which is
	// kept apart so it can only explain a failure, never read as a name.
	out := &requiredBinaryProbeOutput{cancel: cancel}
	diag := &requiredBinaryProbeOutput{cancel: func() {}} // truncated, never stops the probe
	cmd := exec.CommandContext(commandCtx, dockerBinary, args...)
	cmd.Stdout, cmd.Stderr = out, diag
	err = cmd.Run()
	output, exceeded := out.snapshot()
	diagnostics, diagTruncated := diag.snapshot()
	if diagTruncated {
		diagnostics = append(diagnostics, []byte(" [stderr truncated]")...)
	}
	if err != nil {
		if cleanupErr := removeRequiredBinaryProbeContainer(dockerBinary, containerName); cleanupErr != nil {
			return nil, fmt.Errorf("required-binary probe failed: %w; container cleanup failed: %v", err, cleanupErr)
		}
	}
	if exceeded {
		return nil, fmt.Errorf("required-binary probe output exceeded %d bytes", requiredBinaryProbeOutputLimit)
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(string(output)+"\n"+string(diagnostics)))
	}
	var missing []string
	for _, name := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		if name != "" {
			missing = append(missing, name)
		}
	}
	return missing, nil
}

func requiredBinaryProbeContainerName() (string, error) {
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", err
	}
	return "factoryd-required-binaries-" + hex.EncodeToString(suffix[:]), nil
}

func removeRequiredBinaryProbeContainer(dockerBinary, containerName string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, dockerBinary, "rm", "--force", containerName).CombinedOutput()
	if err == nil || strings.Contains(string(out), "No such container") {
		return nil
	}
	return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
}

// doctorCheckBaseline runs the repository's verify and full-suite commands
// through a real run that stops after its baseline (factoryd
// -baseline-check): the check a build would otherwise reach only after its
// spec and plan had spent model calls. It runs only when no earlier row
// failed, since a check on a broken stack would only repeat that failure.
func doctorCheckBaseline(in doctorInputs, failedBefore int) doctorCheck {
	name := "baseline check: the verify and full-suite commands pass in the sandbox on " + in.targetRepo
	if in.baselineCheck == nil {
		return doctorCheck{Name: name, Err: errors.New("skipped"), Advisory: true}
	}
	if failedBefore > 0 {
		return doctorCheck{Name: name, Err: errors.New("skipped: fix the failures above first"), Advisory: true}
	}
	if err := in.baselineCheck(in.targetRepo); err != nil {
		return doctorCheck{Name: name, Err: errors.New(baselineCheckSentence(err)),
			Fix: "fix verify_command or full_suite_command in the repository's .factory.yml (the line above names the logs), then run doctor -target-repo again"}
	}
	return doctorCheck{Name: name}
}

// baselineCheckSentence is the operator's part of a failed check's error:
// from its "baseline verify failed" or "baseline check failed" sentence on,
// without the Temporal wrapping around it.
func baselineCheckSentence(err error) string {
	text := err.Error()
	for _, marker := range []string{"baseline check failed", "baseline verify failed"} {
		if i := strings.Index(text, marker); i >= 0 {
			text = text[i:]
			if j := strings.Index(text, " (type: "); j >= 0 {
				text = text[:j]
			}
			return text
		}
	}
	return text
}

// doctorBaselineCheckRunner is the real doctorInputs.baselineCheck: the
// single-ticket run in this process, under the same session config and data
// dir, with extra flags (quickstart's -verify-command) appended.
func doctorBaselineCheckRunner(dp *deps, configPath, dataDir, temporalAddress string, extra ...string) func(string) error {
	return func(repo string) error {
		args := []string{"-baseline-check", "-workspace", repo}
		if dataDir != "" {
			args = append(args, "-data-dir", dataDir)
		}
		if configPath != "" {
			args = append(args, "-config", configPath)
		}
		if temporalAddress != "" {
			args = append(args, "-temporal-address", temporalAddress)
		}
		return dp.host.baselineCheck(append(args, extra...))
	}
}

// doctorExplicitDataDir is doctor's -data-dir when the operator passed it,
// for the check to use; empty lets the check resolve the session config's.
func doctorExplicitDataDir(flags *flag.FlagSet, dataDir string) string {
	if flagsWasVisited(flags, "data-dir") {
		return dataDir
	}
	return ""
}

// baselineCheck is the real hostBoundary.baselineCheck.
func (impl realHost) baselineCheck(args []string) error {
	return runMain(impl.dp, args)
}

// doctorCheckUnpushedCommits warns when the repository's HEAD has commits no
// branch on origin has, as of the checkout's last fetch: a run builds on
// HEAD, and no pull request opens while its base carries commits no gate
// checked (uncheckedBaseCommitsRefusal). It reads local refs only: no
// network, nothing changed. Advisory: a run with pull requests off is
// unaffected, and a repository with no origin has nothing to compare.
func doctorCheckUnpushedCommits(ctx context.Context, repo string) doctorCheck {
	name := "HEAD of " + repo + " is on origin, so a pull request carries only the run's own work"
	git := func(args ...string) (string, error) {
		out, err := exec.CommandContext(ctx, "git", append([]string{"-C", repo}, args...)...).Output()
		return strings.TrimSpace(string(out)), err
	}
	if _, err := git("remote", "get-url", "origin"); err != nil {
		return doctorCheck{Name: name, Err: errors.New("skipped: the repository has no origin remote"), Advisory: true}
	}
	out, err := git("log", "--format=%h %s", "HEAD", "--not", "--remotes=origin")
	if err != nil {
		return doctorCheck{Name: name, Err: fmt.Errorf("skipped: %v", err), Advisory: true}
	}
	if out == "" {
		return doctorCheck{Name: name}
	}
	commits := strings.Split(out, "\n")
	shown := commits
	if len(shown) > 5 {
		shown = shown[:5]
	}
	return doctorCheck{Name: name, Advisory: true,
		Err: fmt.Errorf("%d commit(s) are on no origin branch as of the last fetch (%s): with pull requests on, no pull request opens until they are pushed or dropped", len(commits), strings.Join(shown, "; ")),
		Fix: "git push them (or drop them) before submitting"}
}
