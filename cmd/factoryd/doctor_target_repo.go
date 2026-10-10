package main

import (
	"context"
	"flag"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"buildgate/internal/composeservices"
	"buildgate/internal/projectconfig"
	"buildgate/internal/requestsubmit"
	"buildgate/internal/run"
	"buildgate/internal/sandbox"
	"buildgate/internal/sessionconfig"
)

// doctorRegisterTargetRepoFlag registers -target-repo directly on flags,
// like doctorRegisterListModelsFlag.
func doctorRegisterTargetRepoFlag(flags *flag.FlagSet) *string {
	return flags.String("target-repo", "", "a target repo checkout to check as a run and a submit would: its compose file at HEAD (one line per service with its verdict and BG_SERVICE_* variables, and a warning when the worker plus its sidecars exceed Docker's memory), its verify command, the root AGENTS.md and the project-bootstrap preflight submit refuses on, and the Go, Python and Node versions it declares against the sandbox image's; empty skips these checks")
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
			Fix: "commit it, then run this again: submit and quickstart refuse the repo until then, and no flag or config key skips the check"})
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
