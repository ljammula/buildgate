package workflow

import (
	"buildgate/internal/runner"
	"buildgate/internal/sandbox"
	"context"
	"errors"
	"fmt"
	"time"
)

// A repository command (canonical verify, the baseline verify, the full
// suite, a named gate) that fails while the run's registry proxy cannot
// reach a package registry has not judged the work. The proxy counts the
// requests its upstream failed (sandbox.RegistryProxyLifecycle.UpstreamFailures)
// and runSandboxWithRetries reads that count before and after each launch.
// The decision is made from the command's exit code and that count only,
// never from what the command printed:
//
//   - failed, and the proxy failed at least one request during the launch:
//     the command is run once more, in the same Activity, on the worktree as
//     the first run left it. No model is called, so nothing is spent.
//   - the second run passes: the step passed.
//   - the second run fails and the proxy again failed a request: the step is
//     unresolved (errDependencyFetch, DependencyFetchFailureType).
//   - the second run fails and the proxy failed nothing: an ordinary failure.
//
// Without a registry proxy, or with a count that could not be read on either
// side of the launch, nothing here changes what the step does.

// errDependencyFetch is the launch error of a command that failed twice, each
// time while the registry proxy could not reach a package registry.
var errDependencyFetch = errors.New("the command failed twice, and both times the registry proxy could not reach a package registry")

// registryFetches is what the last launch on a context learned from the
// registry proxy: how many requests the proxy's upstream failed while the
// launch's sandbox existed. known is false when there was no proxy or its
// count could not be read before or after the launch.
type registryFetches struct {
	failures int
	known    bool
}

type registryFetchesKey struct{}

// withRegistryFetches returns ctx marked so that runSandboxWithRetries reads
// the registry proxy's count around each launch, and where it leaves the
// result of the last one.
func withRegistryFetches(ctx context.Context) (context.Context, *registryFetches) {
	fetches := &registryFetches{}
	return context.WithValue(ctx, registryFetchesKey{}, fetches), fetches
}

// registryFetchWatch is one launch's reading of the proxy's count.
type registryFetchWatch struct {
	proxy  *sandbox.RegistryProxyLifecycle
	into   *registryFetches
	before int
}

// watchRegistryFetches reads the proxy's count before a launch. It reads
// nothing for a launch whose context is not marked (withRegistryFetches) or
// that has no proxy, and it clears what an earlier launch on the context
// left.
func watchRegistryFetches(ctx context.Context, proxy *sandbox.RegistryProxyLifecycle) registryFetchWatch {
	into, _ := ctx.Value(registryFetchesKey{}).(*registryFetches)
	if into == nil {
		return registryFetchWatch{}
	}
	*into = registryFetches{}
	before, known := proxy.UpstreamFailures(ctx)
	if !known {
		return registryFetchWatch{}
	}
	return registryFetchWatch{proxy: proxy, into: into, before: before}
}

// done reads the count again once the launch's sandbox is gone, before the
// proxy is torn down, and records the difference.
func (w registryFetchWatch) done(ctx context.Context) {
	if w.into == nil {
		return
	}
	after, known := w.proxy.UpstreamFailures(ctx)
	if !known || after < w.before {
		return
	}
	*w.into = registryFetches{failures: after - w.before, known: true}
}

// commandLaunch is one call of runSandboxWithRetries with everything bound
// but the context, the log path and the attempt hooks.
type commandLaunch func(ctx context.Context, logPath func(int) string, before func(int) error, after func(int, runner.Result, error) error) (runner.Result, error)

// rerunAfterFetchFailure makes launch and, when its command failed while the
// registry proxy could not reach a registry, makes it once more (see the
// comment at the top of this file). The second launch's attempts are numbered
// after the first's, so each has its own intent, journal entry and log. before
// and after must not be nil.
//
// The second launch is not made when less of the Activity's time is left
// than twice what the first run took, and one that the deadline left no room
// to start (errOutOfTime) is as if it had not been made: the first run's
// failure stands, as it would without this rule. Any other error of the
// second launch is returned as that launch's own.
func rerunAfterFetchFailure(ctx context.Context, logPath func(int) string, before func(int) error, after func(int, runner.Result, error) error, launch commandLaunch) (runner.Result, error) {
	fetchCtx, fetches := withRegistryFetches(ctx)
	made := 0
	first, err := launch(fetchCtx, logPath, before, func(attempt int, res runner.Result, runErr error) error {
		made = max(made, attempt)
		return after(attempt, res, runErr)
	})
	if err != nil || first.ExitCode == 0 || !fetches.failed() || !timeToRunAgain(ctx, first) {
		return first, err
	}
	second, err := launch(fetchCtx,
		func(attempt int) string { return logPath(made + attempt) },
		func(attempt int) error { return before(made + attempt) },
		func(attempt int, res runner.Result, runErr error) error { return after(made+attempt, res, runErr) },
	)
	switch {
	case errors.Is(err, errOutOfTime) && !errors.Is(err, sandbox.ErrCleanupUnconfirmed):
		return first, nil
	case err != nil:
		return second, err
	case second.ExitCode != 0 && fetches.failed():
		return second, fmt.Errorf("%w (exit code %d)", errDependencyFetch, second.ExitCode)
	}
	return second, nil
}

// failed reports a launch during which the proxy is known to have failed a
// request.
func (f *registryFetches) failed() bool {
	return f.known && f.failures > 0
}

// timeToRunAgain reports whether ctx leaves at least twice the time first
// took: a second run that the deadline cut off would turn a judged failure
// into a timeout.
func timeToRunAgain(ctx context.Context, first runner.Result) bool {
	deadline, ok := ctx.Deadline()
	return !ok || time.Until(deadline) >= 2*first.FinishedAt.Sub(first.StartedAt)
}
