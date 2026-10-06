package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeImageDocker is a `docker` whose `image inspect` succeeds or fails.
func fakeImageDocker(t *testing.T, present bool) string {
	t.Helper()
	exit := "1"
	if present {
		exit = "0"
	}
	binary := filepath.Join(t.TempDir(), "docker")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nexit "+exit+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return binary
}

func TestDoctorOpenShellWarnsWithTheInstallFixWithoutAMeterImage(t *testing.T) {
	dp := newTestDeps(t)
	started := false
	fakeSandboxOf(dp).startStackFn = func(context.Context, io.Writer, string) error {
		started = true
		return nil
	}
	checks := doctorOpenShellChecks(dp, context.Background(), doctorInputs{}, true, io.Discard)
	if len(checks) != 1 {
		t.Fatalf("checks = %d, want 1", len(checks))
	}
	c := checks[0]
	if c.Err == nil || !c.Advisory || !strings.Contains(c.Err.Error(), "meter_image is not set") || !strings.Contains(c.Fix, "make install") {
		t.Errorf("check %+v, want an advisory warning naming meter_image and `make install`", c)
	}
	if started {
		t.Error("-fix started the stack with no meter image")
	}
}

func TestDoctorOpenShellImagesWarnWhenAPinnedImageIsMissing(t *testing.T) {
	c := doctorCheckOpenShellImages(context.Background(), fakeImageDocker(t, false))
	if c.Err == nil || !c.Advisory || !strings.Contains(c.Fix, "make openshell-images") {
		t.Fatalf("check = %+v, want an advisory failure fixed by make openshell-images", c)
	}
	for _, image := range []string{wantGatewayImage, wantSandboxImage, wantSupervisorImage} {
		if !strings.Contains(c.Err.Error(), image) {
			t.Errorf("error %q does not name %s", c.Err, image)
		}
	}
	if c := doctorCheckOpenShellImages(context.Background(), fakeImageDocker(t, true)); c.Err != nil {
		t.Errorf("all images present: %+v", c)
	}
}

func TestDoctorOpenShellGatewayAndMeterHealth(t *testing.T) {
	dp := newTestDeps(t)
	ctx := context.Background()
	if c := doctorCheckOpenShellGateway(dp, ctx); c.Err == nil || !c.Advisory {
		t.Errorf("unhealthy gateway: %+v, want an advisory failure", c)
	}
	if c := doctorCheckMeter(dp, ctx); c.Err == nil || !c.Advisory {
		t.Errorf("unhealthy meter: %+v, want an advisory failure", c)
	}
	fakeSandboxOf(dp).gatewayHealthyFn = func(context.Context) error { return nil }
	fakeSandboxOf(dp).meterHealthyFn = func(context.Context) error { return nil }
	if c := doctorCheckOpenShellGateway(dp, ctx); c.Err != nil {
		t.Errorf("healthy gateway: %+v", c)
	}
	if c := doctorCheckMeter(dp, ctx); c.Err != nil {
		t.Errorf("healthy meter: %+v", c)
	}
}

func TestDoctorOpenShellChecksRunAllFourWithAMeterImage(t *testing.T) {
	dp := newTestDeps(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	fakeSandboxOf(dp).gatewayHealthyFn = func(context.Context) error { return errors.New("down") }
	checks := doctorOpenShellChecks(dp, context.Background(), doctorInputs{
		meterImage: "localhost:5050/factoryd-meter@sha256:aaaa", sandboxDocker: fakeImageDocker(t, true),
	}, false, io.Discard)
	if len(checks) != 4 {
		t.Fatalf("checks = %d, want 4", len(checks))
	}
	for _, c := range checks {
		if strings.Contains(c.Name, "not configured") {
			t.Errorf("check %q still says not configured", c.Name)
		}
	}
}

func TestDoctorOpenShellGatewayConfigFailsWhenTLSIsOff(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if c := doctorCheckOpenShellGatewayConfig(); c.Err != nil {
		t.Fatalf("the rendered default config must pass: %+v", c)
	}
	stack := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "factoryd", "openshell")
	if err := os.MkdirAll(stack, 0o700); err != nil {
		t.Fatal(err)
	}
	insecure := "[openshell.gateway]\ndisable_tls = true\n[openshell.gateway.mtls_auth]\nenabled = true\n"
	if err := os.WriteFile(filepath.Join(stack, "gateway.toml"), []byte(insecure), 0o600); err != nil {
		t.Fatal(err)
	}
	c := doctorCheckOpenShellGatewayConfig()
	if c.Err == nil || !c.Advisory || !strings.Contains(c.Err.Error(), "disable_tls") {
		t.Errorf("check = %+v, want an advisory failure naming disable_tls", c)
	}
}

func TestGatewayConfigSecure(t *testing.T) {
	for _, tc := range []struct {
		name, config, wantErr string
	}{
		{"secure", "[openshell.gateway]\ndisable_tls = false\n[openshell.gateway.mtls_auth]\nenabled = true\n", ""},
		{"tls off", "[openshell.gateway]\ndisable_tls = true\n[openshell.gateway.mtls_auth]\nenabled = true\n", "disable_tls"},
		{"tls unset", "[openshell.gateway.mtls_auth]\nenabled = true\n", "disable_tls"},
		{"mtls off", "[openshell.gateway]\ndisable_tls = false\n[openshell.gateway.mtls_auth]\nenabled = false\n", "mtls_auth"},
		{"mtls missing", "[openshell.gateway]\ndisable_tls = false\n", "mtls_auth"},
		{"not toml", "disable_tls = = false", "does not parse"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := gatewayConfigSecure(tc.config)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("err = %v, want nil", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Errorf("err = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestDoctorFixStartsTheOpenShellStackOnlyWhenItDoesNotAnswer(t *testing.T) {
	in := doctorInputs{meterImage: "localhost:5050/factoryd-meter@sha256:aaaa"}
	for _, tc := range []struct {
		name               string
		gatewayUp, meterUp bool
		fix                bool
		meterImage         string
		wantStarts         int
	}{
		{"both down, fix", false, false, true, in.meterImage, 1},
		{"meter down, fix", true, false, true, in.meterImage, 1},
		{"gateway down, fix", false, true, true, in.meterImage, 1},
		{"both up, fix", true, true, true, in.meterImage, 0},
		{"both down, no fix", false, false, false, in.meterImage, 0},
		{"both down, fix, no meter image", false, false, true, "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dp := newTestDeps(t)
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			health := func(up bool) func(context.Context) error {
				return func(context.Context) error {
					if up {
						return nil
					}
					return errors.New("down")
				}
			}
			fakeSandboxOf(dp).gatewayHealthyFn = health(tc.gatewayUp)
			fakeSandboxOf(dp).meterHealthyFn = health(tc.meterUp)
			var started []string
			fakeSandboxOf(dp).startStackFn = func(_ context.Context, _ io.Writer, meterImage string) error {
				started = append(started, meterImage)
				return errors.New("docker is not usable")
			}
			var out strings.Builder
			doctorOpenShellChecks(dp, context.Background(), doctorInputs{meterImage: tc.meterImage, sandboxDocker: fakeImageDocker(t, true)}, tc.fix, &out)
			if len(started) != tc.wantStarts {
				t.Fatalf("starts = %v, want %d", started, tc.wantStarts)
			}
			if tc.wantStarts == 1 && (started[0] != in.meterImage || !strings.Contains(out.String(), "OpenShell not started: docker is not usable")) {
				t.Errorf("started with %q, output %q", started[0], out.String())
			}
		})
	}
}
