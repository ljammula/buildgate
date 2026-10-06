package main

import (
	"fmt"

	"buildgate/internal/modelrole"
	"buildgate/internal/sessionconfig"
)

// tier2SettingsOverride, when non-nil, is used by resolveSettings instead
// of loading the session config from disk itself. It has exactly two
// writers, both of which own this whole process for the duration:
//
//   - worker_config.go, immediately around its own in-process drain loop.
//     worker and runMainWithReady share this process (never a
//     subprocess -- see ticketRunner's own doc comment), so a queued
//     entry's Tier-2 settings are exactly the same sessionconfig.Settings
//     worker's own startup (and its doctor preflight) already resolved
//     from whatever -config path it was given, rather than a second,
//     independent load of the default session config path that would
//     ignore that -config entirely. Cleared on return, so a later bare
//     `factoryd <run>` invocation in the same process (there is none in
//     production, but tests share a binary) resolves its own settings
//     normally.
//   - serve_cmd.go, once, after every startup validation that can still
//     return early and before the server accepts its first connection, so
//     that an API-started run picks up serve's own daemon-static release/
//     sandbox-resource flags (runMainWithReady has no CLI flag left to
//     forward them through). Deliberately never cleared -- see that
//     assignment's own comment.
//
// Neither writer can observe the other: `worker` and `serve` are
// separate commands, and `serve`'s own daemon supervisor spawns real
// subprocesses. resolveSettings returns a *copy* (Settings is all value
// types), so a run that mutates its own resolved settings -- as
// runMainWithReady does, applying .factory.yml over them -- cannot reach
// this shared value or another concurrently-running API-started run's.
var tier2SettingsOverride *sessionconfig.Settings

// resolveSettings returns sessionconfig.DefaultSettings() overlaid with
// whatever session config is in effect: tier2SettingsOverride when
// worker_config.go set one, else the first of sessionconfig.DefaultPaths()
// that exists. This is the Tier-2 half of the settings precedence chain
// (defaults, then session config, then .factory.yml, then an explicit
// CLI Tier-1 flag) -- applyProjectConfigDefaults applies the .factory.yml
// half, for the handful of these fields it also overlays
// (release-protected-paths, plus the relay token/cost ceilings), and the
// flags remaining in runMainWithReady's own var block are the CLI half.
func resolveSettings() (sessionconfig.Settings, error) {
	if tier2SettingsOverride != nil {
		return *tier2SettingsOverride, nil
	}
	return loadDefaultSettings()
}

// loadDefaultSettings is resolveSettings' own "no override in effect"
// branch, factored out so a caller that DOES need to install an override
// (serveMain, building apiStartTier2Settings) can start from the exact
// same defaults-then-session-config-file resolution a bare `factoryd
// <run>`/`worker` invocation would use, rather than from
// sessionconfig.DefaultSettings() alone -- which is what apiStartTier2Settings
// did before this fix, silently discarding every session-config field it
// didn't itself overlay (relay token/cost budgets and ceilings,
// registry-proxy upstreams and sizing, sandbox_docker/sandbox_user) for
// the life of the `serve` process.
func loadDefaultSettings() (sessionconfig.Settings, error) {
	return loadSettingsForConfig("")
}

// loadConfigForPath loads configPath when non-empty, else falls back to
// sessionconfig.LoadDefault's own "first of DefaultPaths() that exists"
// search -- the "-config wins, otherwise search defaults" precedence
// every command with a -config flag is supposed to give it (worker's
// own applySessionConfig, quickstartResolveExistingConfig). Factored out
// so loadSettingsForConfig and any caller that needs the raw
// *sessionconfig.Config (not just Settings) -- doctorMain's own
// -hitl-reminder-interval/-data-dir resolution, doctorApplyFixes -- share
// one implementation instead of each hand-rolling this same branch.
func loadConfigForPath(configPath string) (cfg *sessionconfig.Config, path string, found bool, err error) {
	if configPath == "" {
		return sessionconfig.LoadDefault()
	}
	configPath = sessionconfig.ResolveArg(configPath)
	cfg, err = sessionconfig.Load(configPath)
	if err != nil {
		return nil, configPath, false, err
	}
	return cfg, configPath, true, nil
}

// loadSettingsForConfig is loadDefaultSettings generalized to an explicit
// -config path: configPath == "" reproduces loadDefaultSettings' own
// default-path search exactly; a non-empty configPath is loaded directly,
// failing loudly if it doesn't parse rather than silently falling back to
// the default search (the same "explicit -config always wins, and a bad
// one is a hard error" contract applySessionConfig already gives
// worker). Before this existed, `serve` accepted a -config flag but
// only ever used it to locate the stable start-token file --
// baseSettings/-data-dir/release policy all still searched the default
// config paths independently, so a second config.yml sitting at the
// default path silently won over the one actually named on the command
// line. `doctor`
// had no -config flag at all until this same fix added one.
// loadSettingsForConfig deliberately does NOT validate roles: --
// resolving settings must never fail (or print) on its own, since it is
// also `doctor`'s and `init`/`onboard`'s own sandbox-image-prefill route
// (loadDefaultSettings): an early version of this function called
// validateRoles here, which made `doctor` abort before ever printing its
// check table on an invalid roles: block (instead of reporting it
// through the "roles resolve" check row like every other diagnosis) and
// silently broke init/onboard's own
// `if settings, err := loadDefaultSettings(); err == nil && ...`
// sandbox-image fallback (init.go/onboard.go), which have nothing to do
// with roles: at all. Each caller that actually needs roles: enforced
// calls validateRoles explicitly, once, itself: applySessionConfig
// (worker), serveMain (serve), submitMain (submit), and
// runMainWithReady (direct `factoryd run -config`, silently -- see
// validateRoles' own doc comment).
func loadSettingsForConfig(configPath string) (sessionconfig.Settings, error) {
	settings := sessionconfig.DefaultSettings()
	cfg, _, found, err := loadConfigForPath(configPath)
	if err != nil {
		return settings, fmt.Errorf("session config: %w", err)
	}
	if !found {
		return settings, nil
	}
	return cfg.ApplySettings(settings)
}

// validateRoles runs sessionconfig.ValidateRouting and returns its error --
// used both by callers invoked once per process (applySessionConfig for
// worker, serveMain, submitMain) and by `factoryd <run>`, where a
// corrective/PR-review round or a worker drained ticket can invoke it
// many times in one session (worker execs `run` per ticket). Either
// way this stays silent (error only, no warning print) rather than
// repeating the same diagnosis on every invocation. ValidateRouting has
// no unwaived-warning case (see its own doc comment), so there is only
// ever this one behavior. A schema-clean routes:/models:/roles: config
// never refuses to start here -- modelrole.SelectRoute resolves an
// actual route for the direct build/conformity/drafting paths (see
// run_ticket.go/conformity.go/spec_draft_job.go), and the Temporal path
// resolves one too (modelrole.CheckRouteBinding, the Worker's own trust
// check, plus Activities.CheckRoute/ResolveRouteCredentials).
func validateRoles(settings sessionconfig.Settings) error {
	if err := sessionconfig.ValidateSkills(settings); err != nil {
		return fmt.Errorf("roles: %w", err)
	}
	if err := sessionconfig.ValidateDesignGuideDirs(settings); err != nil {
		return err
	}
	if err := sessionconfig.ValidateRouting(settings); err != nil {
		return fmt.Errorf("roles: %w", err)
	}
	// Found live (M3 walk, 2026-09-28): ValidateRouting alone only checks
	// routes:/models:/roles: schema consistency (a model's routes exist,
	// route_ids keys are real routes, thinking is supported) -- it never
	// builds the actual sandbox.RoutePolicy a launch would use, so a
	// roles.execution.allowed entry naming a model whose id can never
	// produce a valid policy (e.g. a slash-containing id, which
	// sandbox.RoutePolicy.Validate rejects because pi's own CLI hangs on
	// it) passed config load, `doctor`, and `submit` cleanly and only
	// failed once a human had already drafted and approved a spec/plan
	// against it and the real build tried to launch. See
	// modelrole.ValidateAllowedPolicies' own doc comment for the full
	// case and why the credential probe stays out of this check.
	if err := modelrole.ValidateAllowedPolicies(settings); err != nil {
		return fmt.Errorf("roles: %w", err)
	}
	return nil
}
