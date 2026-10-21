package workflow

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"buildgate/internal/sandbox"
)

// ensureSidecars starts the registry proxy and the compose services an
// attempt of a sandboxed step needs, in that order. The first attempt of a
// build launch that has either is the run's "install" start-up step (progress
// contract): step_start before, step_end after, naming what started.
// A later attempt starts the same sidecars without marking the step again.
func (a *Activities) ensureSidecars(ctx context.Context, input RunWorkflowInput, attempt int, registryProxy *sandbox.RegistryProxyLifecycle, compose *sandbox.ComposeServicesLifecycle) error {
	mark := attempt == 1 && ctx.Value(buildLaunchKey{}) != nil && (registryProxy != nil || !compose.Disabled())
	logDir := a.logDirFor(input)
	if mark {
		progressMarkLog(ctx, logDir, "install", "step_start", "", "", "")
	}
	err := startSidecars(ctx, attempt, registryProxy, compose)
	if mark {
		outcome := "pass"
		if err != nil {
			outcome = "fail"
		}
		progressMarkLog(ctx, logDir, "install", "step_end", outcome, installDetail(registryProxy != nil, compose.LaunchedServices(), err), "")
	}
	return err
}

// startSidecars is the Ensure calls themselves.
func startSidecars(ctx context.Context, attempt int, registryProxy *sandbox.RegistryProxyLifecycle, compose *sandbox.ComposeServicesLifecycle) error {
	// Ensure runs before the worker timeout is computed, not after: only the
	// first attempt that needs the proxy pays for its startup, and computing
	// the timeout first would let that time eat into the fixed teardown
	// margin. Compose runs after it for the identical reason:
	// EnsureForAttempt can run a real `docker compose up --wait`, bounded by
	// ComposeServicesReadyTimeout, already folded into the margin.
	if registryProxy != nil {
		if err := registryProxy.Ensure(ctx, ""); err != nil {
			return fmt.Errorf("start registry proxy: %w", err)
		}
	}
	if compose != nil {
		if err := compose.EnsureForAttempt(ctx, attempt); err != nil {
			return fmt.Errorf("start compose services: %w", err)
		}
	}
	return nil
}

// installDetail names what the install step started, or why it failed.
func installDetail(proxy bool, started []sandbox.ComposeServiceRecord, err error) string {
	var parts []string
	if proxy {
		parts = append(parts, "registry proxy")
	}
	if len(started) > 0 {
		names := make([]string, len(started))
		for i, s := range started {
			names[i] = s.Name
		}
		parts = append(parts, "compose services: "+strings.Join(names, ", "))
	}
	detail := strings.Join(parts, "; ")
	if err != nil {
		detail = strings.TrimSpace(detail + " " + err.Error())
	}
	return detail
}

// relativeLog is path relative to the run's directory logDir, for a progress
// line's Log; "" when path is empty or not inside it.
func relativeLog(logDir, path string) string {
	if logDir == "" || path == "" {
		return ""
	}
	dir, err := filepath.Abs(logDir)
	if err != nil {
		return ""
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return ""
	}
	rel, err := filepath.Rel(dir, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return ""
	}
	return rel
}
