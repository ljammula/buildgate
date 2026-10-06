package main

import "buildgate/internal/consolelink"

// consoleLinkEnvVar is the operator's machine-wide default for where the
// Flutter console web app (console/, not the factoryd API) is served --
// distinct from the console's own API_BASE_URL, which points the other
// way (console -> factoryd API). Left unset, factoryd prints no link:
// there is no sensible default, since the console has no fixed serving
// address of its own (dev `flutter run -d chrome`, a reverse proxy, a
// hosted build all pick their own port).
const consoleLinkEnvVar = consolelink.EnvVar

// resolveConsoleBaseURL resolves the console base URL for dataDir: see
// consolelink.BaseURL (the flag, FACTORYD_CONSOLE_URL, then the address a
// serve for dataDir recorded while it answers).
func resolveConsoleBaseURL(flagValue, dataDir string) string {
	return consolelink.BaseURL(flagValue, dataDir)
}

// consoleRequestURL returns the console's deep link to request id under
// base (as returned by resolveConsoleBaseURL), matching the path-based
// routing console/lib/main.dart's usePathUrlStrategy() serves --
// /requests/{id} loads that request directly, no client-side navigation
// needed. Returns "" when base is "", so callers skip printing anything
// rather than a broken partial URL.
func consoleRequestURL(base, id string) string {
	return consolelink.RequestURL(base, id)
}
