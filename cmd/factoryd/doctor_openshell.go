package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/pelletier/go-toml/v2"

	"buildgate/internal/hostcontrol"
	"buildgate/internal/sandbox"
)

// doctorOpenShellChecks are the advisory checks of the OpenShell gateway and
// the meter, which every build launches through: a failing row warns without
// failing doctor. Without meter_image the stack cannot be started, and the
// one row says so. With fix, a gateway or meter that does not answer is
// started first (the meter, then the gateway) and the outcome printed to w;
// a sandbox whose build is gone is reclaimed (doctorSandboxOwnersCheck);
// a stack that answers but must be restarted (stackRestartReasons) is stopped
// before that, unless a build is using it.
func doctorOpenShellChecks(dp *deps, ctx context.Context, in doctorInputs, fix bool, w io.Writer) []doctorCheck {
	if in.meterImage == "" {
		return []doctorCheck{{
			Name:     "OpenShell gateway and meter configured",
			Err:      errors.New("meter_image is not set, so the gateway and meter cannot be started and no build can launch"),
			Fix:      "run `make install` from the buildgate checkout: it builds the meter image and writes meter_image",
			Advisory: true,
		}}
	}
	// A sandbox whose build is gone is reclaimed before the restart below,
	// which refuses while any sandbox exists.
	sandboxes, sandboxesListed := doctorSandboxOwnersCheck(dp, ctx, in, fix, w)
	if fix {
		if reasons := stackRestartReasons(in.meterImage); len(reasons) > 0 {
			stopStackForRestart(dp, w, reasons)
		}
		startSandboxRuntimeIfDown(dp, ctx, w, in.meterImage)
	}
	checks := []doctorCheck{
		doctorCheckOpenShellImages(ctx, in.sandboxDocker),
		doctorCheckOpenShellGateway(dp, ctx),
		doctorCheckOpenShellGatewayConfig(),
	}
	if sandboxesListed {
		checks = append(checks, sandboxes)
	}
	if check, ok := doctorCheckSupervisorTrustsInterceptingCA(); ok {
		checks = append(checks, check)
	}
	return append(checks, doctorCheckMeter(dp, ctx), doctorCheckMeterRunsConfiguredImage(in.meterImage))
}

// stackRestartReason is why a stack that was started has to be stopped and
// started again: a start that finds the gateway and the meter answering
// leaves them as they are, and each takes what is named here only when it
// starts.
type stackRestartReason int

const (
	// supervisorLacksCA: the gateway starts supervisors from an image
	// without this network's intercepting CA.
	supervisorLacksCA stackRestartReason = iota
	// meterImageDiffers: the meter was started from an image other than the
	// configured meter_image.
	meterImageDiffers
	// meterImageUnrecorded: the stack was started with no record of its
	// meter's image, so the meter cannot be shown to be the configured one.
	meterImageUnrecorded
)

// goal is what the restart is for, as the line -fix prints ends.
func (r stackRestartReason) goal() string {
	switch r {
	case supervisorLacksCA:
		return "sandbox supervisors trust this network's CA"
	case meterImageDiffers:
		return "the meter runs the configured image"
	default:
		return "the meter is known to run the configured image"
	}
}

// stackRestartReasons is every reason the stack this machine last started is
// not the one its configuration calls for; none for a stack never started.
func stackRestartReasons(meterImage string) []stackRestartReason {
	var reasons []stackRestartReason
	if stale, known := supervisorImageStale(); known && stale {
		reasons = append(reasons, supervisorLacksCA)
	}
	if _, reason, stale := meterImageStale(meterImage); stale {
		reasons = append(reasons, reason)
	}
	return reasons
}

// supervisorImageStale reports whether the stack last started with a
// supervisor image other than the one this network's intercepting CA calls
// for. known is false when the network has no such CA on record or the stack
// was never started: a start renders the right image.
func supervisorImageStale() (stale, known bool) {
	caPath := interceptingCAPath()
	if caPath == "" {
		return false, false
	}
	caPEM, err := os.ReadFile(caPath)
	if err != nil {
		return false, false
	}
	stack, err := hostcontrol.OpenShellStackPath()
	if err != nil {
		return false, false
	}
	config, err := os.ReadFile(filepath.Join(stack, hostcontrol.OpenShellGatewayConfigFile))
	if err != nil {
		return false, false
	}
	return !strings.Contains(string(config), `"`+hostcontrol.SupervisorImageFor(caPEM)+`"`), true
}

// meterImageStale reports whether the stack's meter was last started from an
// image other than meterImage, with that image (started) and which of the
// two meter reasons applies. A stack never started is not stale: its first
// start takes meterImage.
func meterImageStale(meterImage string) (started string, reason stackRestartReason, stale bool) {
	started, everStarted := hostcontrol.StartedMeterImage()
	switch {
	case !everStarted || started == meterImage:
		return started, 0, false
	case started == "":
		return "", meterImageUnrecorded, true
	}
	return started, meterImageDiffers, true
}

// stopStackForRestart stops the stack so that the start after it takes what
// reasons name: a gateway reads its configuration, and the meter's container
// its image, once, at a start. A build in any profile's data dir keeps the
// stack running, and the rows then say what to run.
func stopStackForRestart(dp *deps, w io.Writer, reasons []stackRestartReason) {
	profiles, err := loadProfiles()
	if err != nil {
		return
	}
	dirs, _ := distinctDataDirs(profiles)
	goals := make([]string, len(reasons))
	for i, r := range reasons {
		goals[i] = r.goal()
	}
	fmt.Fprintf(w, "openshell: restarting so that %s\n", strings.Join(goals, " and "))
	stopOpenShellIfStarted(dp, w, dirs)
}

// doctorCheckSupervisorTrustsInterceptingCA is the row of a network that
// re-signs TLS: every model call's TLS connection is the supervisor's, so a
// supervisor without that CA refuses the upstream's certificate and the
// worker sees only a connection error. Skipped (ok false) elsewhere.
func doctorCheckSupervisorTrustsInterceptingCA() (check doctorCheck, ok bool) {
	stale, known := supervisorImageStale()
	if !known {
		return doctorCheck{}, false
	}
	name := "OpenShell supervisor trusts this network's CA"
	if !stale {
		return doctorCheck{Name: name}, true
	}
	return doctorCheck{
		Name:     name,
		Advisory: true,
		Err:      errors.New("this network re-signs TLS and the gateway starts supervisors from an image without its CA: a model call from a sandbox fails as a connection error"),
		Fix:      "run `factoryd stop -all`, then `factoryd doctor -fix`: the gateway takes its supervisor image when it starts",
	}, true
}

// doctorCheckOpenShellImages reports the pinned OpenShell images that are not
// present locally; they are pulled, never built.
func doctorCheckOpenShellImages(ctx context.Context, dockerBinary string) doctorCheck {
	name := "OpenShell images present"
	var missing []string
	for _, image := range []string{hostcontrol.OpenShellGatewayImage, hostcontrol.OpenShellSandboxImage, hostcontrol.OpenShellSupervisorImage} {
		if !sandbox.ImagePresent(ctx, dockerBinary, image) {
			missing = append(missing, image)
		}
	}
	if len(missing) == 0 {
		return doctorCheck{Name: name}
	}
	return doctorCheck{
		Name:     name,
		Err:      fmt.Errorf("not present locally: %s", strings.Join(missing, ", ")),
		Fix:      "run `make openshell-images` to pull them by their pinned digests",
		Advisory: true,
	}
}

// doctorOpenShellGatewayCheckName names doctorCheckOpenShellGateway's row.
const doctorOpenShellGatewayCheckName = "OpenShell gateway healthy"

func doctorCheckOpenShellGateway(dp *deps, ctx context.Context) doctorCheck {
	name := doctorOpenShellGatewayCheckName
	if err := dp.sandbox.gatewayHealthy(ctx); err != nil {
		return openShellDownCheck(dp, ctx, name, err, hostcontrol.OpenShellGatewayAddr, hostcontrol.OpenShellGatewayHealthAddr)
	}
	return doctorCheck{Name: name}
}

// openShellDownCheck is the row of a stack service that does not answer on
// addrs. A container outside the stack that publishes one of those ports is
// why it cannot start, so the row names it instead of asking for a start.
func openShellDownCheck(dp *deps, ctx context.Context, name string, err error, addrs ...string) doctorCheck {
	if holders := dp.sandbox.portHolders(ctx, addrs...); len(holders) > 0 {
		return doctorCheck{Name: name, Err: fmt.Errorf("%v; %s", err, strings.Join(holders, "; ")), Advisory: true,
			Fix: "stop that container or publish it on another host port, then run `factoryd doctor -fix`"}
	}
	return doctorCheck{Name: name, Err: err, Advisory: true,
		Fix: "run `factoryd doctor -fix` to start it (needs Docker); its stack is `~/.config/factoryd/openshell/docker-compose.yml`"}
}

func doctorCheckMeter(dp *deps, ctx context.Context) doctorCheck {
	name := "meter healthy"
	if err := dp.sandbox.meterHealthy(ctx); err != nil {
		return openShellDownCheck(dp, ctx, name, err, hostcontrol.OpenShellMeterAddr)
	}
	return doctorCheck{Name: name}
}

// doctorCheckMeterRunsConfiguredImage is the row of a meter left running
// across a `make install` that built another image: it answers, so nothing
// replaced it, and every build is metered by the superseded one.
func doctorCheckMeterRunsConfiguredImage(meterImage string) doctorCheck {
	name := "OpenShell meter runs the configured image"
	started, reason, stale := meterImageStale(meterImage)
	if !stale {
		return doctorCheck{Name: name}
	}
	err := fmt.Errorf("the meter was started from %s and meter_image is %s", shortImageRef(started), shortImageRef(meterImage))
	if reason == meterImageUnrecorded {
		err = fmt.Errorf("the stack was started with no record of its meter's image, so the meter may not be meter_image %s", shortImageRef(meterImage))
	}
	return doctorCheck{
		Name:     name,
		Advisory: true,
		Err:      err,
		Fix:      "run `factoryd doctor -fix`: it stops the gateway and the meter and starts them from meter_image, but not while a request is building or a sandbox exists; run it again once those builds have finished",
	}
}

// shortImageRef is an image reference with its digest cut to 12 hex digits.
func shortImageRef(ref string) string {
	const marker = "@sha256:"
	if i := strings.Index(ref, marker); i >= 0 && len(ref) > i+len(marker)+12 {
		return ref[:i+len(marker)+12]
	}
	return ref
}

// doctorCheckOpenShellGatewayConfig checks the gateway config the stack runs:
// the rendered file in the stack directory when it exists, else the one a
// start would write.
func doctorCheckOpenShellGatewayConfig() doctorCheck {
	name := "OpenShell gateway config has TLS and mTLS on"
	config, err := openShellGatewayConfig()
	if err != nil {
		return doctorCheck{Name: name, Err: err, Advisory: true}
	}
	if err := gatewayConfigSecure(config); err != nil {
		return doctorCheck{Name: name, Err: err, Advisory: true,
			Fix: "the gateway must run with `disable_tls = false` and `[openshell.gateway.mtls_auth] enabled = true`; remove the edited gateway.toml in the stack directory and start again"}
	}
	return doctorCheck{Name: name}
}

func openShellGatewayConfig() (string, error) {
	stack, err := hostcontrol.OpenShellStackPath()
	if err == nil {
		if b, rerr := os.ReadFile(filepath.Join(stack, hostcontrol.OpenShellGatewayConfigFile)); rerr == nil {
			return string(b), nil
		}
	}
	return hostcontrol.RenderGatewayConfig(hostcontrol.OpenShellSupervisorImage)
}

// gatewayConfigSecure returns an error unless config keeps TLS on and mTLS
// client authentication enabled.
func gatewayConfigSecure(config string) error {
	var doc struct {
		OpenShell struct {
			Gateway struct {
				DisableTLS *bool `toml:"disable_tls"`
				MTLSAuth   struct {
					Enabled *bool `toml:"enabled"`
				} `toml:"mtls_auth"`
			} `toml:"gateway"`
		} `toml:"openshell"`
	}
	if err := toml.Unmarshal([]byte(config), &doc); err != nil {
		return fmt.Errorf("gateway config does not parse: %w", err)
	}
	gw := doc.OpenShell.Gateway
	switch {
	case gw.DisableTLS == nil || *gw.DisableTLS:
		return errors.New("disable_tls is not false: the gateway would serve without TLS")
	case gw.MTLSAuth.Enabled == nil || !*gw.MTLSAuth.Enabled:
		return errors.New("mtls_auth is not enabled: clients would not be authenticated")
	}
	return nil
}
