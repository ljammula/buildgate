package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"buildgate/internal/hostcontrol"
	"buildgate/internal/sandbox"
)

// TestEnsureSandboxRuntimeStartsTheStackOnlyWhenItMay covers each condition
// under which a command about to launch workers starts the gateway and the
// meter, and the one line a failed start prints.
func TestEnsureSandboxRuntimeStartsTheStackOnlyWhenItMay(t *testing.T) {
	const image = "localhost:5050/factoryd-meter@sha256:aaaa"
	down := errors.New("down")
	for _, tc := range []struct {
		name               string
		autostart          string
		meterImage         string
		noRuntime          bool
		gatewayErr, mtrErr error
		startErr           error
		wantStarts         int
		wantOutput         string
	}{
		{name: "gateway down", meterImage: image, gatewayErr: down, wantStarts: 1},
		{name: "meter down", meterImage: image, mtrErr: down, wantStarts: 1},
		{name: "both answer", meterImage: image},
		{name: "autostart off", autostart: "0", meterImage: image, gatewayErr: down},
		{name: "no meter image", gatewayErr: down},
		{name: "no gateway runtime in this build", meterImage: image, noRuntime: true, gatewayErr: down},
		{name: "start fails", meterImage: image, gatewayErr: down, startErr: errors.New("docker is not running"), wantStarts: 1, wantOutput: "OpenShell not started: docker is not running\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dp := newTestDeps(t)
			t.Setenv(hostcontrol.AutostartEnvVar, tc.autostart)
			fake := fakeSandboxOf(dp)
			if !tc.noRuntime {
				fake.runtimeFn = func() sandbox.Runtime { return &fakeSandboxes{} }
			}
			fake.gatewayHealthyFn = func(context.Context) error { return tc.gatewayErr }
			fake.meterHealthyFn = func(context.Context) error { return tc.mtrErr }
			var started []string
			fake.startStackFn = func(_ context.Context, _ io.Writer, meterImage string) error {
				started = append(started, meterImage)
				return tc.startErr
			}
			var out bytes.Buffer
			ensureSandboxRuntime(dp, context.Background(), &out, tc.meterImage)
			if len(started) != tc.wantStarts || (tc.wantStarts == 1 && started[0] != image) {
				t.Errorf("starts = %v, want %d with %q", started, tc.wantStarts, image)
			}
			if out.String() != tc.wantOutput || strings.Contains(out.String(), "\x1b") {
				t.Errorf("output = %q, want %q", out.String(), tc.wantOutput)
			}
		})
	}
}

// On a network that re-signs TLS the recorded bundle is every command's
// egress CA unless the operator named another; elsewhere nothing is set.
func TestDefaultEgressCABundleIsTheRecordedInterceptingCA(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	unset := ""
	if defaultEgressCABundle(&unset); unset != "" {
		t.Fatalf("with no recorded bundle the egress CA = %q, want none", unset)
	}
	if err := os.MkdirAll(filepath.Dir(buildCABundlePath()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(buildCABundlePath(), []byte("pem"), 0o644); err != nil {
		t.Fatal(err)
	}
	if defaultEgressCABundle(&unset); unset != buildCABundlePath() {
		t.Errorf("unset egress CA = %q, want the recorded bundle %q", unset, buildCABundlePath())
	}
	named := "/operator/ca.pem"
	if defaultEgressCABundle(&named); named != "/operator/ca.pem" {
		t.Errorf("an egress CA the operator named became %q", named)
	}
}
