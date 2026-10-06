package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"buildgate/internal/hostcontrol"
	"buildgate/internal/sanitize"
	"buildgate/internal/sessionconfig"
)

// ensureSandboxRuntime starts the OpenShell gateway and the meter when a
// command is about to launch workers and either service does not answer. It
// does nothing with FACTORYD_AUTOSTART=0, without a meter image, or in a
// build with no gateway runtime. A start that fails prints one line to w;
// the launch itself then fails naming `factoryd doctor -fix`.
func ensureSandboxRuntime(dp *deps, ctx context.Context, w io.Writer, meterImage string) {
	if dp.sandbox.runtime() == nil || !hostcontrol.AutostartEnabled() || meterImage == "" {
		return
	}
	startSandboxRuntimeIfDown(dp, ctx, w, meterImage)
}

// startSandboxRuntimeIfDown starts the stack when the gateway or the meter
// does not answer, and reports a failed start on w.
func startSandboxRuntimeIfDown(dp *deps, ctx context.Context, w io.Writer, meterImage string) {
	if dp.sandbox.gatewayHealthy(ctx) == nil && dp.sandbox.meterHealthy(ctx) == nil {
		return
	}
	if err := dp.sandbox.startStack(ctx, w, meterImage); err != nil {
		fmt.Fprintf(w, "OpenShell not started: %s\n", sanitize.Line(err.Error()))
	}
}

// meterLedgerRoot is where the meter writes every run's ledger and factoryd
// reads it: under the data root, the one host directory the Docker VM shares
// read-write. No worker mounts it.
func meterLedgerRoot() string {
	return filepath.Join(sessionconfig.DataRoot(), "meter-ledgers")
}

// startStack is the real sandboxRuntimeBoundary start.
func (impl realSandboxRuntime) startStack(ctx context.Context, w io.Writer, meterImage string) error {
	stack, err := openShellStack(meterImage)
	if err != nil {
		return err
	}
	return hostcontrol.StartOpenShell(impl.dp, ctx, w, stack)
}

// openShellStack is the stack this machine runs: one gateway and one meter
// for every profile, the gateway seeing the home directory read-only so it
// can check a sandbox's bind sources (the data root, and each repository's
// .git, which a worktree binds). It creates the ledger root, which the meter's
// container binds.
func openShellStack(meterImage string) (hostcontrol.OpenShellStack, error) {
	ledgers := meterLedgerRoot()
	if err := os.MkdirAll(ledgers, 0o750); err != nil {
		return hostcontrol.OpenShellStack{}, fmt.Errorf("create meter ledger directory: %w", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return hostcontrol.OpenShellStack{}, fmt.Errorf("resolve home directory: %w", err)
	}
	return hostcontrol.OpenShellStack{
		MeterImage:     meterImage,
		MeterLedgerDir: ledgers,
		HomeDir:        home,
		Timeouts:       hostcontrol.DefaultOpenShellTimeouts(),
	}, nil
}
