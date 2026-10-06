package main

import "time"

// requestJobScriptTimeoutSlack is how much longer a sandboxed drafting
// job's own container is kept alive than its script's own configured
// --timeout-minutes/--criterion-timeout-minutes budget (see
// requestJobContainerDeadline), so the script's own Pi timeout always
// fires -- and its own "agent timed out" evidence path runs -- before
// Docker kills the container out from under it. Found via review: without
// this, the container was stopped at (approximately) the script's own
// configured budget, before the script's own timer could fire at all.
const requestJobScriptTimeoutSlack = 90 * time.Second

// requestJobContainerDeadline is the duration a sandboxed drafting job's
// own sandboxCtx (context.WithTimeout) should use, given scriptBudget --
// the exact duration the script itself is told via
// --timeout-minutes/--criterion-timeout-minutes, unchanged by this
// function. hasRelay/hasRegistryProxy/composeReadyTimeout are
// sandboxAttemptMargin's own parameters (sandbox_exec.go): every drafting
// job passes (true, false, 0) since a relay is mandatory
// (resolveRequestJobRelaySpec/resolveOracleRelaySpec both refuse to run
// without one) and none of the three configures a registry proxy or
// compose services.
//
// The margin folded in here is exactly what runSandboxWithRetries will
// later subtract from time-until-deadline to get its own worker timeout
// (sandbox_exec.go's `timeout` local) -- so it cancels out, and that
// worker timeout works out to exactly scriptBudget+
// requestJobScriptTimeoutSlack, always requestJobScriptTimeoutSlack ahead
// of the script's own configured budget, regardless of
// hasRegistryProxy/composeReadyTimeout (as long as the same values are
// used to build this deadline and passed to runSandboxWithRetries, which
// they are for every drafting job).
//
// Current state, not a residual to close: this container deadline runs
// from context creation, not from when the script's own process actually
// starts running -- time this attempt spends blocked on the single-instance
// model-host lock (relay upstream contention) still counts against it. Under
// heavy contention, this container's own deadline (and so
// runSandboxWithRetries' worker timeout) can therefore fire before the
// script's own --timeout-minutes Pi timer ever starts counting, the
// opposite of what requestJobScriptTimeoutSlack is meant to guarantee.
func requestJobContainerDeadline(scriptBudget time.Duration, hasRegistryProxy bool, composeReadyTimeout time.Duration) time.Duration {
	return scriptBudget + sandboxAttemptMargin(hasRegistryProxy, composeReadyTimeout) + requestJobScriptTimeoutSlack
}
