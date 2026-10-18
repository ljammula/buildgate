package main

import (
	"path/filepath"

	"buildgate/internal/sessionconfig"
)

// serveStableStartTokenFileName is the file `factoryd install-service`
// writes the serve LaunchAgent's stable start token into
// (<effective config dir>/serve-start-token, mode 0600), and the one
// `serveMain` reads from -- before ever generating a fresh per-process
// token -- when FACTORYD_API_START_TOKEN is unset. Without this, every
// `launchctl kickstart`/reboot-triggered restart of a supervised `serve`
// would mint a
// brand new generateStartToken() value, silently invalidating every
// browser's already-`captureStartTokenFromLocation`-stored token and
// 403ing New run/release/stats/daemon screens until the operator noticed
// the service had restarted and went hunting for a fresh console link in
// its log file.
const serveStableStartTokenFileName = "serve-start-token"

// resolveEffectiveConfigPath resolves the session config path used to
// locate the stable start-token file: explicit (a real -config flag
// value), made absolute, if given; otherwise the first of
// sessionconfig.DefaultPaths() that already exists on disk. Mirrors
// resolveServiceConfigPath's own algorithm (install_service.go) but never
// errors on "nothing found" -- a caller here (serveMain,
// quickstartEnsureServe, consoleMain, install-service) treats that as
// simply "no stable token file possible", falling back to an ephemeral
// per-process token, not a fatal condition the way install-service's own
// LaunchAgent setup must treat it.
//
// Every one of `serve` (-config), `factoryd quickstart` (its own already-
// resolved session config path), `factoryd console` (-config), and
// `factoryd install-service` (its own -config) now call this exact same
// function so they can never derive four different token-file locations
// for what is supposed to be one shared, stable secret (an adversarial
// review, 2026-09-24): before this fix `serve` had no -config
// flag at all and always used sessionconfig.LoadDefault()'s fixed
// default-path search, while install-service's plist could point -config
// at a non-default path -- a supervised serve LaunchAgent's own token
// file (keyed off that resolution) would then silently disagree with
// what a bare `factoryd serve`/`console` invocation (the default search)
// derived, each reading and writing a DIFFERENT token file with no error
// ever surfaced -- just a permanently 403ing console.
func resolveEffectiveConfigPath(explicit string) (path string, found bool) {
	if explicit != "" {
		abs, err := filepath.Abs(sessionconfig.ResolveArg(explicit))
		if err != nil {
			return "", false
		}
		return abs, true
	}
	p, found, err := sessionconfig.ResolvePath()
	if err != nil || !found {
		return "", false
	}
	return p, true
}

// serveStableStartTokenPathFor returns the stable start-token file path
// for configPath (as resolveEffectiveConfigPath resolved it) -- always
// the config's own directory, never a separate location a caller could
// derive differently.
func serveStableStartTokenPathFor(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), serveStableStartTokenFileName)
}
