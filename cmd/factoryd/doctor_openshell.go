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
// started first (the meter, then the gateway) and the outcome printed to w.
func doctorOpenShellChecks(dp *deps, ctx context.Context, in doctorInputs, fix bool, w io.Writer) []doctorCheck {
	if in.meterImage == "" {
		return []doctorCheck{{
			Name:     "OpenShell gateway and meter configured",
			Err:      errors.New("meter_image is not set, so the gateway and meter cannot be started and no build can launch"),
			Fix:      "run `make install` from the buildgate checkout: it builds the meter image and writes meter_image",
			Advisory: true,
		}}
	}
	if fix {
		startSandboxRuntimeIfDown(dp, ctx, w, in.meterImage)
	}
	return []doctorCheck{
		doctorCheckOpenShellImages(ctx, in.sandboxDocker),
		doctorCheckOpenShellGateway(dp, ctx),
		doctorCheckOpenShellGatewayConfig(),
		doctorCheckMeter(dp, ctx),
	}
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

func doctorCheckOpenShellGateway(dp *deps, ctx context.Context) doctorCheck {
	name := "OpenShell gateway healthy"
	if err := dp.sandbox.gatewayHealthy(ctx); err != nil {
		return doctorCheck{Name: name, Err: err, Advisory: true,
			Fix: "run `factoryd doctor -fix` to start it (needs Docker); its stack is `~/.config/factoryd/openshell/docker-compose.yml`"}
	}
	return doctorCheck{Name: name}
}

func doctorCheckMeter(dp *deps, ctx context.Context) doctorCheck {
	name := "meter healthy"
	if err := dp.sandbox.meterHealthy(ctx); err != nil {
		return doctorCheck{Name: name, Err: err, Advisory: true,
			Fix: "run `factoryd doctor -fix` to start it (needs Docker); its stack is `~/.config/factoryd/openshell/docker-compose.yml`"}
	}
	return doctorCheck{Name: name}
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
	return hostcontrol.RenderGatewayConfig()
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
