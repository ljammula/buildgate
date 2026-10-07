package main

import (
	"buildgate/internal/consolelink"
	"buildgate/internal/requestdriver"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"buildgate/internal/api"
	"buildgate/internal/consoleweb"
	"buildgate/internal/release"
	"buildgate/internal/request"
	"buildgate/internal/run"
	"buildgate/internal/sandbox"
	"buildgate/internal/sessionconfig"
)

// defaultServeAddr is `factoryd serve`'s own -addr default, shared with
// console_link.go's resolveConsoleBaseURL so a submit/quickstart run on a
// machine with no serve process reachable yet can still guess the address
// an operator would get by running `factoryd serve` with no flags -- see
// that function's own doc comment for why this guess is only ever offered
// when this binary actually has a console embedded to serve there.
const defaultServeAddr = consolelink.DefaultServeAddr

// startTokenBytes is the amount of crypto/rand entropy generateStartToken
// reads for an operator-unconfigured start token -- 32 bytes (256 bits),
// matching the sizing every other generated-secret comment in this
// codebase's review history has settled on for "not brute-forceable
// within this process's own lifetime", with ample margin: this token only
// ever needs to resist guessing by a network attacker over HTTP, not
// offline cracking of a stored hash.
const startTokenBytes = 32

// generateStartToken returns a fresh, cryptographically random start
// token (base64url, unpadded -- RawURLEncoding -- so it drops cleanly
// into both a URL fragment and an `Authorization: Bearer` header with no
// escaping) for serveMain to use as s.startToken when the operator left
// FACTORYD_API_START_TOKEN unset. See serveMain's own doc comment at its
// call site for why this exists: without it, an authenticated console had
// no way to ever learn a start-class token at all (F: serve-start-token,
// operator decision 2026-09-24).
func generateStartToken() (string, error) {
	buf := make([]byte, startTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate start token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// consoleLinkWithStartToken builds serveMain's own printed console link,
// carrying startToken in the URL *fragment* (never a query param -- see
// this function's own call site for the security reasoning: a fragment is
// never sent to any server or logged by an intermediary, only read
// client-side). Factored out so TestServeConsoleLinkCarriesStartTokenInFragment
// can check the exact shape without needing consoleweb.Embedded() to be
// true (this binary's own test build never embeds a real console -- see
// that function's own doc comment).
func consoleLinkWithStartToken(addr, startToken string) string {
	return fmt.Sprintf("console: http://%s/#t=%s", addr, startToken)
}

// validateCORSAllowOrigin rejects any -cors-allow-origin shape that would
// silently widen a browser's read access beyond "the one origin an
// operator actually typed", or that could never match a real request in
// the first place: "*" (api.WithCORSAllowOrigin never treats this
// specially -- an operator who passes it would get a literal
// Access-Control-Allow-Origin: * header advertised to every browser, wildly
// wider than intended), and anything that doesn't parse as a bare
// scheme://host[:port] origin -- a path/query/fragment component or
// embedded userinfo (http://user:pass@host) is never part of an Origin
// header a real browser sends, so accepting one here would silently never
// match any real request, leaving CORS quietly dead with no diagnostic
// pointing at why (found via adversarial review: the userinfo case parses
// without error under url.Parse and was missed by the original Path/
// RawQuery/Fragment-only check).
//
// On success, returns origin canonicalized exactly as a browser serializes
// it in its own Origin header -- lowercase scheme/host, default port (80
// for http, 443 for https) omitted -- since api.Server's ServeHTTP later
// does a raw string comparison against the real Origin header. Without
// this, an operator-typed "HTTP://LOCALHOST:8091" or
// "http://localhost:80" would pass validation yet never match any real
// request, silently leaving CORS dead (found via GitHub Codex App review
// of this PR, P2: cmd/factoryd/serve_cmd.go:50).
func validateCORSAllowOrigin(origin string) (string, error) {
	if origin == "" {
		return "", nil
	}
	if origin == "*" {
		return "", fmt.Errorf("-cors-allow-origin: %q is not accepted -- name the exact origin serving the console (e.g. http://localhost:8091), never a wildcard", origin)
	}
	u, err := url.Parse(origin)
	if err != nil || u.Scheme == "" || u.Host == "" || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return "", fmt.Errorf("-cors-allow-origin: %q is not a bare scheme://host[:port] origin", origin)
	}
	scheme := strings.ToLower(u.Scheme)
	defaultPort, ok := map[string]string{"http": "80", "https": "443"}[scheme]
	if !ok {
		return "", fmt.Errorf("-cors-allow-origin: %q: scheme must be http or https (a browser's Origin header never carries any other scheme)", origin)
	}
	// A leading-zero port ("http://localhost:080") parses fine and isn't
	// caught by the plain string comparison below (port == defaultPort
	// never matches "080" == "80"), but a browser's own Origin header
	// never carries one -- it always serializes the canonical decimal
	// port or omits it entirely -- so this origin would otherwise pass
	// validation yet never match any real request, the same silent-dead-
	// CORS failure mode as the two fixes above (found via GitHub Codex
	// App review of this PR, P2, round 3). Rejected outright rather than
	// renormalized, matching this function's existing style of refusing
	// any spelling a browser could never actually send instead of trying
	// to guess what the operator meant.
	if port := u.Port(); port != "" {
		n, convErr := strconv.Atoi(port)
		if convErr != nil || n < 1 || n > 65535 || strconv.Itoa(n) != port {
			return "", fmt.Errorf("-cors-allow-origin: %q: port %q is not a canonical decimal port", origin, port)
		}
	}
	// u.Host (not u.Hostname()+u.Port() reconstructed by hand) is used
	// here specifically because it keeps an IPv6 literal's brackets intact
	// (u.Hostname() strips them, e.g. "[::1]" -> "::1", which would then
	// get rejoined with a port into the invalid, never-matching
	// "::1:8091" -- found via GitHub Codex App review of this PR, P2,
	// round 2, immediately after the first canonicalization fix landed).
	host := strings.ToLower(u.Host)
	if port := u.Port(); port != "" && port == defaultPort {
		host = strings.TrimSuffix(host, ":"+port)
	}
	return scheme + "://" + host, nil
}

// stringListFlag is a flag.Value that accumulates comma-separated values
// across possibly-repeated occurrences of the same flag (-flag=a,b
// -flag=c yields ["a","b","c"]) -- used for -allowed-host (an adversarial
// review, 2026-09-24), where an operator may want to name
// hosts either as one comma-separated value or by repeating the flag.
type stringListFlag struct {
	values []string
}

func (f *stringListFlag) String() string {
	if f == nil {
		return ""
	}
	return strings.Join(f.values, ",")
}

func (f *stringListFlag) Set(s string) error {
	for _, v := range strings.Split(s, ",") {
		if v = strings.TrimSpace(v); v != "" {
			f.values = append(f.values, v)
		}
	}
	return nil
}

// serveFlags bundles every `factoryd serve` flag's pointer, so newServeFlags
// (below) can hand them back to serveMain without an unwieldy multi-value
// return list. Field names match the flag's own local variable name at
// every existing call site.
type serveFlags struct {
	configPath                            *string
	dataDir                               *string
	addr                                  *string
	corsAllowOrigin                       *string
	daemonTemporalAddress                 *string
	daemonBuildAppInterpreter             *string
	daemonBuildAppScript                  *string
	daemonConformityPolicy                *string
	daemonMaxRounds                       *int
	daemonTimeoutMinutes                  *int
	daemonVerifyCommand                   *string
	sandboxMemory                         *string
	sandboxCPUs                           *string
	sandboxTmpfsSize                      *string
	sandboxWorkerUID                      *int
	releaseProtectedPaths                 *string
	releaseMaxFilesChanged                *int
	releaseMaxInsertions                  *int
	releaseRollbackPlan                   *string
	releaseAllowOverrides                 *bool
	releaseAllowDependencyLockfileChanges *bool
	releaseAllowUnsandboxed               *bool
	releaseAllowSkippedProjectCheck       *bool
	apiAllowedSandboxImages               *string
	apiDefaultSandboxImage                *string
	egressCABundle                        *string
	temporalUIURL                         *string
	allowedHosts                          *stringListFlag
}

// newServeFlags builds `factoryd serve`'s FlagSet in isolation from parsing,
// so USAGE.md's doc-vs-flag drift test (TestUSAGEDocFlagsExistOnSubcommand)
// can enumerate its real flags without executing the command.
func newServeFlags() (flags *flag.FlagSet, f serveFlags) {
	flags = flag.NewFlagSet("serve", flag.ContinueOnError)
	// configPath resolves EVERY session-config-derived value on this
	// command: the stable start-token file (resolveEffectiveConfigPath,
	// serveStableStartTokenPathFor -- an adversarial review,
	// 2026-09-24), -data-dir (resolveDataDirFromSessionConfig), and
	// baseSettings (loadSettingsForConfig) -- which resolveServeReleasePolicy
	// (below) in turn derives the release policy from, so -config governs
	// that too.
	//
	// Before this fix, only the start-token file honored this flag:
	// -data-dir/baseSettings/release policy each independently re-ran
	// sessionconfig.LoadDefault's own fixed default-path search, so a
	// second config.yml sitting at the
	// default path silently won over the one actually named on the
	// command line. A live walk (2026-09-25) had `quickstart -config X`
	// spawn `serve -config X`, which then read a DIFFERENT config's
	// sandbox_image, failed its own allowlist check, and crashed with no
	// console started -- worse when it doesn't crash: an API-started run
	// would silently use the other config's route/model/images.
	f.configPath = flags.String("config", "", "session config path; empty searches the first of "+strings.Join(sessionconfig.DefaultPaths(), ", ")+" that exists (the same search `factoryd worker`/`quickstart` use). Resolves -data-dir, baseSettings (relay/sandbox/release-policy defaults for every API-started run and the override endpoint) and the stable start-token file location")
	f.dataDir = flags.String("data-dir", "data", "directory containing durable run records")
	f.addr = flags.String("addr", defaultServeAddr, "HTTP listen address. Loopback-only by default: this Server's read routes (GET /runs, GET /runs/{id}, GET /runs/{id}/events, GET /runs/{id}/diff, GET /projects) carry no auth boundary unless FACTORYD_API_READ_TOKEN is set, and getRunDiff in particular serves every run's complete unified source diff plus absolute host paths and provider/model identity -- binding a wider address (e.g. \":8090\") without also setting that token exposes all of it to any caller that can reach the port (found via the 2026-09-05 Opus review, S3)")
	f.corsAllowOrigin = flags.String("cors-allow-origin", "", "exact browser origin (scheme://host[:port], e.g. http://localhost:8091) to allow reading this Server's responses cross-origin -- needed for the console (USAGE.md §9) when it is served by its own Vite dev server on its own port. Empty (default) keeps CORS off, matching every prior release. Never accepts \"*\": this only changes what a browser page may read from a port it can already reach -- it grants no new network reachability, and the read/start/override token gates above still run first")
	f.daemonTemporalAddress = flags.String("daemon-temporal-address", "", "Temporal server address for serve-managed daemon supervisors; empty disables daemon lifecycle routes")
	f.daemonBuildAppInterpreter = flags.String("daemon-build-app-interpreter", "python3", "daemon supervisor's build_app.py interpreter")
	f.daemonBuildAppScript = flags.String("daemon-build-app-script", "", "daemon supervisor's build_app.py path (default: this version's embedded harness copy)")
	f.daemonConformityPolicy = flags.String("daemon-conformity-policy", "required", "daemon supervisor's build_app.py conformity policy")
	f.daemonMaxRounds = flags.Int("daemon-max-rounds", 3, "daemon supervisor's build_app.py max rounds")
	f.daemonTimeoutMinutes = flags.Int("daemon-timeout-minutes", 45, "daemon supervisor's build_app.py timeout in minutes")
	f.daemonVerifyCommand = flags.String("daemon-verify-command", "make verify", "daemon supervisor's canonical verification command")
	// -daemon-build-app-max-attempts/-daemon-verify-max-attempts and the
	// whole -daemon-sandbox-* resource surface are gone as of
	// flags-consolidate (2026-09-10): `factoryd daemon` -- the child this
	// supervisor spawns -- no longer defines a CLI flag for any of them and
	// resolves them from the session config instead (see daemonMain's own
	// resolveSettings call), so a flag here could only ever be forwarded
	// into a child that rejects it at flag.Parse, or be accepted here and
	// silently dropped. Set sandbox_docker/sandbox_memory/sandbox_cpus/
	// sandbox_tmpfs_size/sandbox_worker_uid/
	// build_app_max_attempts/verify_max_attempts in
	// ~/.config/factoryd/config.yml instead: the supervised child reads the
	// same file this process would.
	//
	// Distinct from the -daemon-* flags above: those configure the
	// long-lived supervised daemon (factoryd daemon, started via
	// daemonConfig below); these configure the `factoryd <run>` run
	// apiStartStarter starts for every API-started request, the same way
	// -daemon-temporal-address is unrelated to -temporal-address. Found
	// missing entirely by a real
	// GitHub Codex App review of this PR (P1): without these,
	// apiStartStarter's spawned subprocess used runMainWithReady's own flag
	// defaults (4g/2/256m) regardless of what an operator configured
	// here, so an API-started run's actual resource ceiling silently
	// diverged from a daemon-recovered run's -- e.g. an operator
	// configuring a 1g ceiling would still see ordinary API runs consume
	// up to 4g.
	f.sandboxMemory = flags.String("sandbox-memory", "4g", "memory limit (Docker --memory syntax) for an API-started run's sandboxed worker container; also caps swap")
	f.sandboxCPUs = flags.String("sandbox-cpus", "2", "CPU limit (Docker --cpus syntax) for an API-started run's sandboxed worker container")
	f.sandboxTmpfsSize = flags.String("sandbox-tmpfs-size", "1g", "size of each of an API-started run's sandboxed worker's writable tmpfs mounts (/tmp and /home/worker) -- see the CLI's own -sandbox-tmpfs-size flag help for why 1g, not 256m")
	// sandboxWorkerUID mirrors the -sandbox-memory/-cpus/-tmpfs-size
	// flags just above and closes the same class of gap their own comment
	// documents: found via code review, this flag was missing entirely, so an
	// API-started run's sandboxed worker silently used sandbox.
	// DefaultWorkerUID regardless of what an operator configured for
	// -sandbox-worker-uid on this same daemon.
	f.sandboxWorkerUID = flags.Int("sandbox-worker-uid", sandbox.DefaultWorkerUID, "dedicated, non-root, non-factoryd UID an API-started run's sandboxed worker container runs as by default -- see runMain's own flag of the same name")
	f.releaseProtectedPaths = flags.String("release-protected-paths", "", "internal/release.MergePolicy.ProtectedPaths for this Server's own override endpoint, comma-separated -- see runMain's own flag of the same name")
	f.releaseMaxFilesChanged = flags.Int("release-max-files-changed", 0, "internal/release.MergePolicy.MaxFilesChanged for this Server's own override endpoint -- see runMain's own flag of the same name")
	f.releaseMaxInsertions = flags.Int("release-max-insertions", 0, "internal/release.MergePolicy.MaxInsertions for this Server's own override endpoint -- see runMain's own flag of the same name")
	f.releaseRollbackPlan = flags.String("release-rollback-plan", "", "internal/release.MergePolicy.RollbackPlan for this Server's own override endpoint -- see runMain's own flag of the same name")
	f.releaseAllowOverrides = flags.Bool("release-allow-overrides", false, "internal/release.MergePolicy.AllowOverrides for this Server's own override endpoint -- see runMain's own flag of the same name")
	f.releaseAllowDependencyLockfileChanges = flags.Bool("release-allow-dependency-lockfile-changes", false, "internal/release.MergePolicy.AllowDependencyLockfileChanges for this Server's own override endpoint -- see runMain's own flag of the same name")
	f.releaseAllowUnsandboxed = flags.Bool("release-allow-unsandboxed", false, "internal/release.MergePolicy.AllowUnsandboxed for this Server's own override endpoint -- see runMain's own flag of the same name")
	f.releaseAllowSkippedProjectCheck = flags.Bool("release-allow-skipped-project-check", false, "internal/release.MergePolicy.AllowSkippedProjectCheck for this Server's own override endpoint -- see runMain's own flag of the same name")
	// apiAllowedSandboxImages narrows the API surface:
	// an authenticated POST /runs caller is untrusted the same way
	// -sandbox-docker's caller already is (see apiStartStarter's own
	// sandbox_docker rejection just above it in this file), so this
	// request field -- previously validated no more strictly than
	// digest-pinning -- gets the same treatment here.
	f.apiAllowedSandboxImages = flags.String("api-allowed-sandbox-images", "", "comma-separated digest-pinned images (name@sha256:...) POST /runs may set as sandbox_image, mirroring -release-protected-paths' comma-separated convention. Empty (default) accepts only this daemon's own configured sandbox image (-api-default-sandbox-image, or failing that the session config's sandbox_image) -- not every digest-pinned image, which was the prior, unrestricted behavior -- because an image is a capability to run arbitrary code inside the sandbox boundary, not merely a filter that's absent until configured (found via a containment review). If this daemon itself has no sandbox image configured, an empty allowlist accepts nothing. The CLI's own -sandbox-image, a fully trusted context, is unaffected and keeps arbitrary digest-pinned values")
	// apiDefaultSandboxImage closes the gap the allowlist above leaves
	// open by itself: an allowlist only says which images a request is
	// *permitted* to name, it does not give a caller with no UI for the
	// field anything to name in the first place. The console is
	// exactly that caller -- see startRun in console/src/api/runs.ts.
	// The default must itself be a member of the allowlist, checked once at
	// startup, not merely at request time.
	f.apiDefaultSandboxImage = flags.String("api-default-sandbox-image", "", "sandbox_image POST /runs uses when the request itself leaves that field empty. Unset (default) falls back to this daemon's own session-config sandbox_image, if any -- an empty request sandbox_image resolves to whichever of the two apiSandboxPolicy.imageAllowed's own implicit default names, or is rejected outright if neither is configured. Must itself be allowlisted by -api-allowed-sandbox-images (or, like an unconfigured allowlist, be the value that implicit default names) -- checked at daemon startup so a bad configuration is never discovered one rejected request at a time. An explicit request sandbox_image always wins over this default")
	f.egressCABundle = flags.String("egress-ca-bundle", "", "PEM file on this host, forwarded as -egress-ca-bundle to every API-started run's own spawned runMainWithReady; see factoryd <run>'s own flag of the same name")
	// temporalUIURL is deliberately CLI-flag-only, not a session config
	// key: unlike data_dir/sandbox_*/release_*, internal/sessionconfig.Settings
	// carries no `serve`-specific settings at all today (no addr, no
	// data_dir field there either -- serve's own -data-dir resolves via
	// resolveDataDirFromSessionConfig's separate, top-level data_dir key,
	// not a Settings field), so adding one here would be the first of its
	// kind rather than following an existing convention.
	f.temporalUIURL = flags.String("temporal-ui-url", "", "base URL of a Temporal Web UI reachable from the operator's browser (e.g. http://localhost:8233), used to build GET /console-config.json's own temporal_ui_url -- the console joins it with a run's own temporal_workflow_id for an \"Open in Temporal UI\" link. Empty (default) omits the field: the console shows no such link")
	// allowedHosts (an adversarial review, 2026-09-24) closes a
	// regression ServeHTTP's own Host check (DNS-rebinding defense, added
	// with -addr's loopback default above) otherwise introduces: bound to
	// loopback but reached through anything that changes the Host header
	// this server actually sees -- `ssh -L 9000:localhost:8090`, a reverse
	// proxy, `tailscale serve` -- every route, reads included, 403s. Each
	// entry is an exact "host[:port]" string (comma-separated, repeatable
	// -- both accumulate into the same set), matched verbatim against the
	// incoming Host header; see api.WithAllowedHosts' own doc comment for
	// why a host accepted this way still requires the override token for
	// every write, exactly like a non-loopback bind, unlike this server's
	// own true loopback address.
	f.allowedHosts = &stringListFlag{}
	flags.Var(f.allowedHosts, "allowed-host", "extra Host header value(s) (host[:port], e.g. localhost:9000 for an ssh -L tunnel, or a Tailscale MagicDNS name) ServeHTTP's loopback Host check accepts in addition to this server's own true loopback address -- comma-separated, and the flag may be repeated (both accumulate). A host accepted this way is for reads only: it never qualifies for the no-override-token write relaxation loopback itself gets -- see api.WithAllowedHosts' own doc comment")
	plainFlagUsage(flags)
	return flags, f
}

// serveRun is the state of one serveMain call: its parameters and every
// value one stage resolves for a later one. A value used inside one stage
// only is a local there.
type serveRun struct {
	// dp is the external boundaries this call reaches.
	dp                       *deps
	flags                    *flag.FlagSet
	sf                       serveFlags
	dataDir                  *string
	addr                     *string
	sandboxMemory            *string
	sandboxCPUs              *string
	sandboxTmpfsSize         *string
	sandboxWorkerUID         *int
	apiAllowedSandboxImages  *string
	apiDefaultSandboxImage   *string
	egressCABundle           *string
	temporalUIURL            *string
	args                     []string
	corsAllowOriginCanonical string
	baseSettings             sessionconfig.Settings
	daemonController         *serveDaemonController
	overrideToken            string
	startToken               string
	readToken                string
	inFlightRuns             sync.WaitGroup
	releasePolicy            release.MergePolicy
	sandboxLimits            sandboxResourceLimits
	sandboxPolicy            apiSandboxPolicy

	exitFuncs
}

func serveMain(dp *deps, args []string) error {
	sv := &serveRun{dp: dp, args: args}
	defer sv.runDeferred()
	if err := sv.loadConfig(); err != nil {
		return err
	}
	if err := sv.resolveTokens(); err != nil {
		return err
	}
	if err := sv.resolveSandboxPolicy(); err != nil {
		return err
	}
	return sv.listenAndServe()
}

// loadConfig parses the flags, loads the session config and builds the daemon controller.
func (sv *serveRun) loadConfig() error {
	var err error
	sv.flags, sv.sf = newServeFlags()
	var daemonTemporalAddress *string
	var corsAllowOrigin *string
	sv.dataDir, sv.addr, corsAllowOrigin, daemonTemporalAddress = sv.sf.dataDir, sv.sf.addr, sv.sf.corsAllowOrigin, sv.sf.daemonTemporalAddress
	daemonBuildAppInterpreter, daemonBuildAppScript, daemonConformityPolicy, daemonMaxRounds := sv.sf.daemonBuildAppInterpreter, sv.sf.daemonBuildAppScript, sv.sf.daemonConformityPolicy, sv.sf.daemonMaxRounds
	var daemonVerifyCommand *string
	var daemonTimeoutMinutes *int
	daemonTimeoutMinutes, daemonVerifyCommand, sv.sandboxMemory, sv.sandboxCPUs = sv.sf.daemonTimeoutMinutes, sv.sf.daemonVerifyCommand, sv.sf.sandboxMemory, sv.sf.sandboxCPUs
	sv.sandboxTmpfsSize, sv.sandboxWorkerUID = sv.sf.sandboxTmpfsSize, sv.sf.sandboxWorkerUID
	// The -release-* flags themselves are read only through sf now, via
	// resolveServeReleasePolicy below -- see that function's own doc
	// comment.
	sv.apiAllowedSandboxImages, sv.apiDefaultSandboxImage, sv.egressCABundle = sv.sf.apiAllowedSandboxImages, sv.sf.apiDefaultSandboxImage, sv.sf.egressCABundle
	sv.temporalUIURL = sv.sf.temporalUIURL
	if err := sv.flags.Parse(sv.args); err != nil {
		return err
	}
	if sv.flags.NArg() != 0 {
		return fmt.Errorf("unexpected positional argument %q", sv.flags.Arg(0))
	}
	sv.corsAllowOriginCanonical, err = validateCORSAllowOrigin(*corsAllowOrigin)
	if err != nil {
		return err
	}
	if err := resolveDataDirFromSessionConfig(sv.flags, sv.dataDir, *sv.sf.configPath); err != nil {
		return err
	}
	if err := validateSandboxResourceLimitFlags("-sandbox", *sv.sandboxMemory, *sv.sandboxCPUs, *sv.sandboxTmpfsSize); err != nil {
		return err
	}
	if err := sandbox.ValidateWorkerUID(*sv.sandboxWorkerUID, os.Getuid()); err != nil {
		return err
	}
	sv.baseSettings, err = loadSettingsForConfig(*sv.sf.configPath)
	if err != nil {
		return err
	}
	// Once, at serve's own process start -- not inside loadSettingsForConfig
	// itself, which doctor and init/onboard's sandbox-image prefill also
	// call and must never fail (or print) over roles: (see
	// loadSettingsForConfig's own doc comment).
	if err := validateRoles(sv.baseSettings); err != nil {
		return err
	}
	if *daemonTemporalAddress != "" {
		// Resolved here, not unconditionally at flag parse: this daemon
		// supervisor path is the only thing on this command that ever
		// touches build_app.py, so a read-only/API-only serve deployment
		// (or a request that always supplies its own -build-app-script)
		// with no -daemon-temporal-address must never fail to start over
		// an unwritable harness cache it was never going to use (found
		// via Codex review of PR #83, round 2).
		if *daemonBuildAppScript != "" {
			resolvedDaemonBuildAppScript, err := resolveHarnessScript(*daemonBuildAppScript, "build_app.py")
			if err != nil {
				return err
			}
			*daemonBuildAppScript = resolvedDaemonBuildAppScript
		}
		daemonConfig := superviseConfig{
			temporalAddress: *daemonTemporalAddress,
			// An adversarial review of Phase A found that without this, the
			// daemon this supervisor spawns for every Temporal-path run
			// always resolved its own settings (sandbox/relay budgets,
			// release policy) from the default session-config search path,
			// regardless of what -config this `serve` process itself uses
			// for every other path (the same -config-not-honored class of
			// bug fixed elsewhere on `serve`'s own -data-dir resolution) --
			// so a Temporal-path run could silently build against a
			// different config entirely.
			configPath:          *sv.sf.configPath,
			dataDir:             *sv.dataDir,
			buildAppInterpreter: *daemonBuildAppInterpreter,
			buildAppScript:      *daemonBuildAppScript,
			conformityPolicy:    *daemonConformityPolicy,
			maxRounds:           *daemonMaxRounds,
			timeoutMinutes:      *daemonTimeoutMinutes,
			verifyCommand:       *daemonVerifyCommand,
			restartBackoff:      time.Second,
			restartBackoffMax:   time.Minute,
			stopTimeout:         10 * time.Second,
		}
		sv.daemonController = newServeDaemonController(sv.dp, daemonConfig)
	}
	return nil
}

// resolveTokens settles the override, start and read tokens and the console's own access.
func (sv *serveRun) resolveTokens() error {
	// TLS belongs at the deployment boundary in a later control-plane
	// slice; every mutating route this service exposes (POST /runs and
	// POST /runs/{id}/override) is gated by its own bearer-token check — see
	// the token environment-variable comments above.
	sv.overrideToken = os.Getenv(overrideTokenEnvironmentVariable)
	sv.startToken = os.Getenv(startTokenEnvironmentVariable)
	sv.readToken = os.Getenv(readTokenEnvironmentVariable)
	// operatorSetStartToken records the source before generateStartToken
	// (below) may fill startToken in -- purely for this startup log's
	// wording, the same "describe what actually happened" reasoning
	// addrIsLoopback just below uses for the override endpoint.
	operatorSetStartToken := sv.startToken != ""
	// The credentials are copied into the API handler's memory and removed
	// from this process environment before any API-started run can launch an
	// untrusted subprocess. This preserves the same environment hygiene as a
	// normal run invocation; runner's child environment filtering is a second
	// defense, not the credential boundary.
	_ = os.Unsetenv(overrideTokenEnvironmentVariable)
	_ = os.Unsetenv(startTokenEnvironmentVariable)
	_ = os.Unsetenv(readTokenEnvironmentVariable)
	// F: serve-start-token (operator decision, 2026-09-24). PR #241 gave
	// the embedded console a way to write override-class routes on a
	// loopback bind with no token, but start-class routes (authorizeStart:
	// POST /runs, the daemon lifecycle routes, GET /projects/{project}/
	// release and /stats) stayed gated behind FACTORYD_API_START_TOKEN --
	// an env var the console has no way to learn, since its tokens are
	// build-time VITE_ variables (console/src/app/config.ts), not something a
	// server can hand it at request time. On a default install (no
	// operator-set token) that left New run/release/stats/ops screens
	// 403ing unconditionally, forever, with no way for the console to
	// ever succeed -- not merely "start disabled until configured" like
	// the override endpoint's own loopback story just above, but a dead
	// end. Generating one here, once per process, and delivering it via
	// the console link this same process prints just below (never logged
	// anywhere else, never sent to the server, never a query param -- see
	// that log line's own doc comment) closes the gap without weakening
	// the gate itself: authorizeStart still 403s any request without a
	// valid bearer token, the token is just no longer something only a
	// human who read this env var name could ever produce. An operator
	// who sets FACTORYD_API_START_TOKEN explicitly gets exactly the prior
	// behavior -- their own token, unchanged, stable across restarts --
	// this only fills the gap when they left it unset.
	// usingStableStartTokenFile records which of the two fallbacks (below)
	// actually supplied startToken, purely for this startup log's wording
	// just below -- an operator running the install-service-managed serve
	// LaunchAgent should see that their already-stored browser token survives a
	// restart, not the same "generated a per-process token" wording that
	// implies it won't.
	usingStableStartTokenFile := false
	if sv.startToken == "" {
		if configPath, ok := resolveEffectiveConfigPath(*sv.sf.configPath); ok {
			stablePath := serveStableStartTokenPathFor(configPath)
			stableToken, issue, err := inspectServeStartTokenFile(stablePath)
			switch {
			case err != nil:
				log.Printf("could not read stable start token file %s (%v) -- falling back to a per-process token", stablePath, err)
			case issue != nil:
				// Refuses to trust an existing-but-invalid file (a
				// symlink, wrong owner, group/other-readable, empty --
				// an adversarial review) rather than silently
				// generating a second token behind an operator's back:
				// they need to see and fix this, not have it masked by a
				// fresh ephemeral token that happens to also work.
				log.Printf("stable start token file %s: %s -- falling back to a per-process token", stablePath, issue.Error())
			case stableToken != "":
				sv.startToken = stableToken
				usingStableStartTokenFile = true
			}
		}
	}
	if sv.startToken == "" {
		generated, err := generateStartToken()
		if err != nil {
			return fmt.Errorf("generate start token: %w", err)
		}
		sv.startToken = generated
	}
	// addrIsLoopback mirrors api.WithListenAddr's own host check, purely
	// for this startup log's wording -- the real decision (whether the
	// relaxation and Host check actually activate) is made once, inside
	// api.WithListenAddr itself, from the identical *addr this log line
	// reports.
	addrIsLoopback := false
	if host, _, err := net.SplitHostPort(*sv.addr); err == nil {
		addrIsLoopback = host == "127.0.0.1" || host == "localhost" || host == "::1"
	}
	switch {
	case sv.overrideToken != "":
		log.Printf("serving API on %s (override endpoint enabled)", *sv.addr)
	case addrIsLoopback:
		// No token configured, but bound to loopback -- the override-class write
		// routes (approve, reject, spec/ticket/oracle edits, retry,
		// cancel) still work for a real same-origin request (the embedded
		// console, or a same-machine curl with no Origin header at all);
		// see api.WithListenAddr's own doc comment for exactly which
		// requests qualify. Not "disabled".
		log.Printf("serving API on %s (override endpoint: no token configured, loopback-only writes allowed for same-origin requests -- set %s to require a token instead, or to widen beyond loopback)", *sv.addr, overrideTokenEnvironmentVariable)
	default:
		log.Printf("serving API on %s (override endpoint disabled — set %s to enable it)", *sv.addr, overrideTokenEnvironmentVariable)
	}
	switch {
	case operatorSetStartToken:
		log.Printf("start-run endpoint enabled (using configured %s)", startTokenEnvironmentVariable)
	case usingStableStartTokenFile:
		log.Printf("start-run endpoint enabled (using this session's stable %s file -- stable across restarts, e.g. a launchd `factoryd install-service` restart)", serveStableStartTokenFileName)
	default:
		log.Printf("start-run endpoint enabled (no %s configured -- generated a per-process token; open the console link below once to use New run/release/stats/daemon screens, or set %s yourself, or run `factoryd install-service` for a token stable across restarts)", startTokenEnvironmentVariable, startTokenEnvironmentVariable)
	}
	if sv.readToken == "" {
		if addrIsLoopback {
			log.Printf("read routes (runs, projects, diffs, event streams) are unauthenticated, but Host-header checked against this loopback address (DNS-rebinding defense) -- set %s to also require a bearer token", readTokenEnvironmentVariable)
		} else {
			log.Printf("read routes (runs, projects, diffs, event streams) are unauthenticated — set %s to require a bearer token, especially if -addr binds beyond loopback", readTokenEnvironmentVariable)
		}
	}
	if sv.corsAllowOriginCanonical != "" {
		log.Printf("CORS: allowing browser reads from origin %s", sv.corsAllowOriginCanonical)
	}
	if consoleweb.Embedded() {
		// The start token rides in the URL *fragment* (#t=..., never a
		// query param): a fragment is never sent to this or any other
		// server (RFC 3986 §3.5 -- the browser strips it before issuing
		// the HTTP request) and never appears in this process's own
		// access logging for that reason, whereas a query param would
		// land in both. console/src/platform/startToken.ts
		// reads it client-side on first load, stores it in this browser's
		// own localStorage (origin-scoped, never sent anywhere by the
		// console either except as this server's own Authorization: Bearer
		// header), and strips it from the address bar immediately after
		// -- so the token's only three homes are this log line (local,
		// operator-owned stdout), the one-time fragment, and that
		// browser's storage for this origin. Always carries a token now:
		// startToken above is either FACTORYD_API_START_TOKEN as set, or
		// this process's own generated fallback -- there is no longer a
		// "no token at all" case to omit the fragment for.
		//
		// EXCEPT when usingStableStartTokenFile: that token is a
		// standing, permanent credential for every start-class route
		// (POST /runs, daemon lifecycle, release/stats), not a per-
		// process secret -- printing it here would put it in this
		// process's own stdout/log file (0644 by default, and
		// `install-service`'s own StandardOutPath/StandardErrorPath
		// plist entries land in `-data-dir/logs/`, readable by anyone who
		// can read that directory) for as long as that log survives
		// (an adversarial review, 2026-09-24). The stable-token
		// case instead prints the link WITHOUT the fragment and points at
		// `factoryd console`, which reads the very same file directly
		// (0600, this operator only) rather than ever putting the value
		// in a log line at all.
		if usingStableStartTokenFile {
			log.Printf("console: http://%s/ (run `factoryd console` for your signed-in link -- this process's own log never prints a stable start token)", *sv.addr)
		} else {
			log.Print(consoleLinkWithStartToken(*sv.addr, sv.startToken))
		}
	} else {
		log.Printf("console: not embedded in this binary (make console-build), dev server: see console/README.md")
	}
	return nil
}

// resolveSandboxPolicy builds the release policy and the sandbox policy API-started runs are held to.
func (sv *serveRun) resolveSandboxPolicy() error {
	// ReadHeaderTimeout is set because bare http.ListenAndServe has none,
	// leaving a slow-header-write client able to hold a connection open
	// indefinitely. No ReadTimeout/WriteTimeout here: /runs/{id}/events is a
	// long-lived SSE stream by design, and a whole-request WriteTimeout
	// would cut it off mid-stream.
	// Tracked separately from http.Server's own request draining — see
	// apiStartStarter's doc comment for why: an API-started run is a
	// detached background goroutine, not an in-flight HTTP request, by the
	// time Shutdown below would otherwise consider this server drained.
	// Built once and given to both apiStartStarter (forwarded into every
	// spawned run's own argv) and api.WithReleasePolicy (used by
	// overrideRun directly) -- found via a local codex review pass: giving
	// each its own separate derivation would silently let an accepted
	// run and a later override of a different run be evaluated under two
	// different policies despite both going through this same server.
	// See resolveServeReleasePolicy's own doc comment for why this is
	// session-config-first rather than serveMain's own raw flags.
	sv.releasePolicy = resolveServeReleasePolicy(sv.flags, sv.baseSettings, sv.sf)
	sv.sandboxLimits = sandboxResourceLimits{memory: *sv.sandboxMemory, cpus: *sv.sandboxCPUs, tmpfsSize: *sv.sandboxTmpfsSize, workerUID: *sv.sandboxWorkerUID}
	var allowedSandboxImages []string
	if *sv.apiAllowedSandboxImages != "" {
		allowedSandboxImages = strings.Split(*sv.apiAllowedSandboxImages, ",")
	}
	sv.sandboxPolicy = apiSandboxPolicy{
		allowedImages:  allowedSandboxImages,
		defaultImage:   *sv.apiDefaultSandboxImage,
		egressCABundle: *sv.egressCABundle,
	}
	if sv.sandboxPolicy.egressCABundle != "" {
		if err := sandbox.ValidateEgressCABundle(sv.sandboxPolicy.egressCABundle); err != nil {
			return fmt.Errorf("-egress-ca-bundle: %w", err)
		}
	}
	// A configured default must itself already be permitted by its own
	// allowlist -- checked here, once, at startup, rather than leaving a
	// misconfigured operator to discover it only when the first request
	// that omits the field gets rejected downstream. See
	// apiDefaultSandboxImage's own flag help for why.
	if sv.sandboxPolicy.defaultImage != "" && !sv.sandboxPolicy.imageAllowed(sv.sandboxPolicy.defaultImage) {
		return fmt.Errorf("-api-default-sandbox-image %q is not allowlisted by -api-allowed-sandbox-images (allowed: %s) -- a daemon must never start with a default it would reject on every request", sv.sandboxPolicy.defaultImage, sv.sandboxPolicy.allowedImagesDescription())
	}

	// apiStartStarter's spawned runMainWithReady no longer accepts -release-*
	// or -sandbox-memory/-cpus/-pids/-tmpfs-size/-worker-uid as CLI flags at
	// all (flags-consolidate, 2026-09-10 folded them into
	// sessionconfig.Settings, resolved via resolveSettings). releasePolicy/
	// sandboxLimits above are this daemon's own trusted, operator-configured
	// values -- identical for every API-started run for as long as this
	// process lives, the same "daemon-static" reasoning their own doc
	// comments already give -- so instead of forwarding them as argv flags
	// (impossible now) they are folded into a Settings value (see
	// apiStartTier2Settings) here, once, and installed as
	// tier2SettingsOverride right before this server starts accepting
	// connections (after every startup validation above that can still
	// return early, so a validation failure here never leaves this
	// package-level override set behind a serveMain call that otherwise
	// never ran a server at all -- tests call serveMain directly and rely on
	// exactly that). Every API-started run's own runMainWithReady call (a
	// goroutine inside this same process, never a subprocess -- see
	// apiStartStarter's own doc comment) then picks this up through its own
	// resolveSettings() call exactly as a worker-drained entry already
	// does via that same variable.
	//
	// baseSettings loads the same on-disk session config a bare `factoryd
	// <run>`/`worker` invocation would when -config is unset
	// (loadSettingsForConfig(""), equivalent to loadDefaultSettings, the
	// same helper resolveSettings itself falls back to), or the config
	// -config names when it is set -- an earlier version of
	// this fix built apiStartTier2Settings straight from
	// sessionconfig.DefaultSettings(), which silently discarded every
	// session-config field outside release/sandbox-resource (relay token/
	// cost budgets and ceilings, registry-proxy upstreams and sizing,
	// sandbox_docker/sandbox_user) for every API-started run, for as long as
	// this process ran, with no error (found via adversarial review,
	// confirmed by a reproduction test). A malformed config file fails
	// serveMain the same way it already fails a bare run via resolveSettings
	// -- refusing to start with a broken config, rather than silently
	// running with defaults, matches run_ticket.go's own treatment of the
	// identical error.
	if sv.sandboxPolicy.defaultImage == "" {
		sv.sandboxPolicy.defaultImage = sv.baseSettings.SandboxImage
	}
	sv.sandboxPolicy.roles = sv.baseSettings.Roles
	if sv.sandboxPolicy.defaultImage != "" && !sv.sandboxPolicy.imageAllowed(sv.sandboxPolicy.defaultImage) {
		return fmt.Errorf("session-config sandbox_image %q is not allowlisted by -api-allowed-sandbox-images (allowed: %s)", sv.sandboxPolicy.defaultImage, sv.sandboxPolicy.allowedImagesDescription())
	}
	warnIfImagesStale(sv.baseSettings)
	//
	// Unlike worker_config.go's own use of tier2SettingsOverride (set, used for
	// one drain loop, then cleared via defer so a later runMainWithReady
	// call in the same process is unaffected), this assignment is NOT
	// cleared: `serve` has no equivalent bounded lifetime to clear it at the
	// end of -- every API-started run's background goroutine must keep
	// resolving these same values for as long as this process runs, and
	// nothing else in this command ever sets or clears
	// tier2SettingsOverride afterward (the daemon-supervisor path,
	// daemonMain/daemon_cmd.go, is a separate `factoryd daemon` process with
	// its own resolveSettings() call and is not reachable from here).
	tier2Settings := apiStartTier2Settings(sv.baseSettings, sv.releasePolicy, sv.sandboxLimits)
	tier2SettingsOverride = &tier2Settings
	return nil
}

// listenAndServe builds the API server and serves until it fails or the process is signalled.
func (sv *serveRun) listenAndServe() error {
	var err error
	serverOptions := []api.Option{
		api.WithOverrideToken(sv.overrideToken),
		api.WithStartToken(sv.startToken),
		api.WithReadToken(sv.readToken),
		api.WithRunStarter(repositoryAPIStarter(apiStartStarter(sv.dp, *sv.dataDir, &sv.inFlightRuns, sv.releasePolicy, sv.sandboxLimits, sv.sandboxPolicy))),
		api.WithProjectChecker(apiProjectChecker()),
		api.WithProjectStatsProvider(apiProjectStatsProvider(*sv.dataDir)),
		api.WithReleasePolicy(sv.releasePolicy),
		// POST /requests/{id}/retry gets the same real PR-open-only retry
		// path `factoryd retry` already wires (retryPullRequestOpener),
		// not just the CLI.
		api.WithPROpener(func(a0 string, a1 string) request.PROpenOutcome { return retryPullRequestOpener(sv.dp, a0, a1) }),
		// POST /requests/{id}/resume refuses "round" for a lost build whose
		// kept worktree cannot be continued, the same check the CLI makes.
		api.WithResumePreflight(func(r *request.Request) ([]string, error) {
			return requestdriver.ResumeGate{Sandboxes: sv.dp.sandbox.runtime()}.RequestResumeRefusals(context.Background(), *sv.dataDir, sv.baseSettings.SandboxDocker, r)
		}),
		// Decisions and new requests wake the live worker's workflow; a
		// no-op without one.
		api.WithRequestWaker(func(ctx context.Context, id string) error {
			return sv.dp.temporal.wakeRequest(ctx, *sv.dataDir, id)
		}),
		// WithListenAddr tells the Server its own real bind address so its
		// Host check and loopback-write relaxation can activate (see
		// that option's own doc comment) -- always passed, not
		// conditional on *addr being loopback: WithListenAddr itself
		// decides that from the address, this call site just needs to
		// name it.
		api.WithListenAddr(*sv.addr),
	}
	// POST /mcp is on only while the token file `factoryd mcp` creates
	// exists beside this serve's session config; with no config there is
	// nowhere for that file to be, so the endpoint stays off.
	if configPath, ok := resolveEffectiveConfigPath(*sv.sf.configPath); ok {
		tokenPath := mcpTokenPathFor(configPath)
		source := mcpTokenSource(tokenPath)
		serverOptions = append(serverOptions, api.WithMCPToken(source))
		if source() != "" {
			log.Printf("MCP endpoint enabled at /mcp (token file %s; `factoryd mcp -disable` turns it off)", tokenPath)
		}
	}
	if *sv.temporalUIURL != "" {
		serverOptions = append(serverOptions, api.WithTemporalUIURL(*sv.temporalUIURL))
	}
	if sv.corsAllowOriginCanonical != "" {
		serverOptions = append(serverOptions, api.WithCORSAllowOrigin(sv.corsAllowOriginCanonical))
	}
	if sv.daemonController != nil {
		serverOptions = append(serverOptions, api.WithDaemonController(sv.daemonController))
	}
	if len(sv.sf.allowedHosts.values) > 0 {
		serverOptions = append(serverOptions, api.WithAllowedHosts(sv.sf.allowedHosts.values))
	}
	// baseSettings.Workspaces is the session config's own `workspaces:`
	// key (internal/sessionconfig.Config.Workspaces) -- POST /requests'
	// and GET /workspaces' own allowlist (see api.WithWorkspaces' doc
	// comment). Always passed, even when empty, so an operator who never
	// configured the key still gets the fallback rule (a workspace an
	// existing request already uses) rather than this option simply
	// never being called.
	serverOptions = append(serverOptions, api.WithWorkspaces(sv.baseSettings.Workspaces))
	// baseSettings' own effective relay ceilings -- POST /requests'
	// tighten-only .factory.yml check (createRequest ->
	// requestsubmit.Submit) compares a workspace's committed ceiling
	// against these, not requestsubmit's own hardcoded legacy defaults.
	// See api.WithSessionRelayCeilings' own doc comment.
	baseTokenCeiling, baseCostCeilingMicroUSD := sv.baseSettings.EffectiveRelayCeilings()
	serverOptions = append(serverOptions, api.WithSessionRelayCeilings(int64(baseTokenCeiling), baseCostCeilingMicroUSD))
	// baseSettings itself -- POST /requests' only use of it is validating
	// a "models" body field against roles.<role>.allowed (see
	// api.WithSessionRoles' own doc comment).
	serverOptions = append(serverOptions, api.WithSessionRoles(sv.baseSettings))
	server := &http.Server{
		Addr:              *sv.addr,
		Handler:           api.NewServer(*sv.dataDir, serverOptions...),
		ReadHeaderTimeout: 5 * time.Second,
	}

	// serveMain is the sole owner of this process's SIGINT/SIGTERM handling
	// (found via review): embedded API-started runs deliberately do NOT
	// install their own signal.NotifyContext (see runMainWithReady's doc
	// comment), so a real operator SIGTERM always reaches here and triggers
	// a graceful HTTP shutdown instead of being silently absorbed by
	// whichever run happened to be in flight.
	signalCtx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	// The "a request is waiting but no worker is live" desktop
	// notification (notifyWorkerStale) used to fire only from `factoryd
	// status`, so it only ever reached an operator who happened to run
	// that command -- exactly the person who already knows the factory is
	// stalled, not the one who walked away. `serve` is the long-lived
	// process an operator otherwise leaves running while away (the
	// console + API), so it is the one place this check can run
	// unattended. Reuses -hitl-reminder-interval's own resolved cadence
	// (session config's hitl_reminder_interval, default 15m -- the same
	// resolution doctorMain already does) rather than adding a second
	// cadence knob; stopped via defer alongside every other component
	// this command owns.
	stopStaleWatcher := startWorkerStaleWatcher(signalCtx, *sv.dataDir, resolveHITLReminderInterval(*sv.sf.configPath))
	defer stopStaleWatcher()

	// Bound before anything else so the console-address record below is
	// only ever written by a serve that actually holds the port: one that
	// fails with "address in use" must not replace, then remove, the
	// record of the serve already listening there.
	listener, err := net.Listen("tcp", server.Addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", server.Addr, err)
	}
	// Where every console link for this data dir points (submit's View
	// line, notifications): consolelink never guesses serve's default
	// port, which another data dir's serve may hold.
	if removeAddress, err := consolelink.RecordServeAddress(*sv.dataDir, *sv.addr); err != nil {
		log.Printf("serve: warning: could not record the console address for %s: %v -- console links will be omitted", *sv.dataDir, err)
	} else {
		defer removeAddress()
	}

	serveErr := make(chan error, 1)
	go func() {
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	select {
	case err := <-serveErr:
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return drainServeComponents(shutdownCtx, func(context.Context) error { return err }, func(context.Context) error {
			if sv.daemonController != nil {
				return sv.daemonController.StopAll(shutdownCtx)
			}
			return nil
		}, &sv.inFlightRuns)
	case <-signalCtx.Done():
		log.Printf("serve: received shutdown signal, draining in-flight requests")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return drainServeComponents(shutdownCtx, server.Shutdown, func(ctx context.Context) error {
			if sv.daemonController != nil {
				return sv.daemonController.StopAll(ctx)
			}
			return nil
		}, &sv.inFlightRuns)
	}
}

func drainServeComponents(ctx context.Context, shutdownHTTP func(context.Context) error, stopDaemons func(context.Context) error, inFlight *sync.WaitGroup) error {
	shutdownErr := shutdownHTTP(ctx)
	daemonErr := stopDaemons(ctx)
	return errors.Join(drainAfterShutdown(shutdownErr, inFlight), daemonErr)
}

// drainAfterShutdown waits out every in-flight API-started run
// unconditionally, whether or not http.Server.Shutdown itself succeeded —
// factored out of serveMain so this ordering is directly unit-testable
// without a real listener or a 30s deadline. Found via review: a
// still-open GET /runs/{id}/events SSE stream for a nonterminal run
// (streamRunEvents) is exactly the kind of long-lived handler Shutdown's
// own bounded shutdownCtx can time out waiting on — returning as soon as
// Shutdown errored, before ever reaching inFlight.Wait(), would abandon
// the background Temporal worker that wait exists to drain, on top of
// reporting a shutdown that didn't actually finish.
//
// inFlight is waited on unbounded, deliberately not bounded by
// shutdownCtx's own remaining time or a fresh deadline of its own —
// bounding it the same way would just reproduce the original drain bug on
// a delay instead of none; a run's own -timeout/-timeout-minutes is what
// actually bounds how long this can take. shutdownErr, if non-nil, is
// still returned (wrapped) after the wait completes, so a genuine
// Shutdown failure is not silently swallowed.
func drainAfterShutdown(shutdownErr error, inFlight *sync.WaitGroup) error {
	if shutdownErr != nil {
		log.Printf("serve: graceful HTTP shutdown did not complete within its deadline, still waiting for in-flight API-started run(s): %v", shutdownErr)
	}
	log.Printf("serve: waiting for any in-flight API-started run(s) to finish")
	inFlight.Wait()
	log.Printf("serve: all API-started runs finished, exiting")
	if shutdownErr != nil {
		return fmt.Errorf("graceful shutdown: %w", shutdownErr)
	}
	return nil
}

// repositoryAPIStarter makes cross-process serialization mandatory for the
// HTTP execution surface. A direct or plain -temporal-address run is valid for
// the CLI's bounded single-run mode, but a long-lived API can accept concurrent
// requests and must route every one through its repository owner Workflow.
func repositoryAPIStarter(next api.RunStarter) api.RunStarter {
	return func(ctx context.Context, req api.StartRequest) (*run.Run, error) {
		if req.Repository == "" || req.TemporalAddress == "" {
			return nil, fmt.Errorf("%w: repository and temporal_address are required for API-started runs", api.ErrInvalidStartRequest)
		}
		return next(ctx, req)
	}
}

// apiStartStarter adapts the API's transport request to the existing
// factoryd run entry point. It starts runMainWithReady in-process (never a
// shell command), preserving the CLI's validation and direct/Temporal routing
// while returning as soon as the initial durable record exists.
//
// inFlight tracks every background run this starter launches (found via
// review): the returned RunStarter hands the durable record back to the
// HTTP handler and returns long before the run itself finishes, so
// http.Server.Shutdown's own request-draining has nothing to wait on for
// it. serveMain adds this same WaitGroup to its own shutdown sequence so
// the process does not exit — silently dropping Temporal workers and
// subprocess supervision still in flight — out from under a run it just
// reported as gracefully drained.
// sandboxResourceLimits is the trusted, operator-configured resource
// ceiling apiStartStarter forwards into every spawned run's own argv --
// analogous to releasePolicy just above (built once from serveMain's own
// flags, never from the request), and deliberately not part of
// api.StartRequest for the same reason SandboxMemory/SandboxCPUs/
// SandboxTmpfsSize aren't request-overridable anywhere else in
// this codebase (see workflow.Activities.SandboxMemory's doc comment): a
// resource ceiling is an operator safety limit, not something an
// authenticated POST /runs caller should raise for itself.
type sandboxResourceLimits struct {
	memory, cpus, tmpfsSize string
	// workerUID mirrors -sandbox-worker-uid (see that flag's own doc
	// comment): the dedicated, non-root, non-factoryd UID an API-started run's
	// sandboxed worker container defaults to. Daemon-static like every
	// other field here, for the same reason -- an authenticated POST
	// /runs caller does not get to choose its own worker identity by
	// asking (found via code review: this field was missing entirely,
	// silently dropping an operator's -sandbox-worker-uid configuration
	// for every API-started run in favor of sandbox.DefaultWorkerUID
	// regardless -- the same class of flag-forwarding gap already closed
	// for -sandbox-memory/cpus/pids/tmpfs-size).
	workerUID int
}

// resolveServeReleasePolicy builds the release.MergePolicy `factoryd
// serve` uses for every API-started run and for its own override
// endpoint, session-config-first -- the same "explicit flag > config
// file > flag default" precedence worker's own applySessionConfig
// (and run_ticket.go's own Tier-1 flags) already use.
//
// Before this fix, every -release-* flag on `serve` defaulted to its
// flag.FlagSet zero value (0 files/insertions allowed, "" rollback plan)
// and that zero value always won over base (the same on-disk session
// config a bare `factoryd <run>`/`worker` already honors via
// loadDefaultSettings) -- so an API-started run denied every PR unless
// the operator repeated every -release-* value AGAIN as a `serve` CLI
// flag, even with a config.yml already carrying a usable release policy
// (see internal/sessionconfig.DefaultReleaseMaxFilesChanged and friends
// for why a config.yml plausibly has one by default now).
// flagsWasVisited(flags, name) is exactly flag.Visit's "was this
// flag actually passed on the command line" check: an explicit flag,
// including an explicit 0/"", still wins over the file.
func resolveServeReleasePolicy(flags *flag.FlagSet, base sessionconfig.Settings, sf serveFlags) release.MergePolicy {
	protectedPaths := *sf.releaseProtectedPaths
	if !flagsWasVisited(flags, "release-protected-paths") {
		protectedPaths = base.ReleaseProtectedPaths
	}
	maxFilesChanged := *sf.releaseMaxFilesChanged
	if !flagsWasVisited(flags, "release-max-files-changed") {
		maxFilesChanged = base.ReleaseMaxFilesChanged
	}
	maxInsertions := *sf.releaseMaxInsertions
	if !flagsWasVisited(flags, "release-max-insertions") {
		maxInsertions = base.ReleaseMaxInsertions
	}
	rollbackPlan := *sf.releaseRollbackPlan
	if !flagsWasVisited(flags, "release-rollback-plan") {
		rollbackPlan = base.ReleaseRollbackPlan
	}
	allowOverrides := *sf.releaseAllowOverrides
	if !flagsWasVisited(flags, "release-allow-overrides") {
		allowOverrides = base.ReleaseAllowOverrides
	}
	allowDependencyLockfileChanges := *sf.releaseAllowDependencyLockfileChanges
	if !flagsWasVisited(flags, "release-allow-dependency-lockfile-changes") {
		allowDependencyLockfileChanges = base.ReleaseAllowDependencyLockfileChanges
	}
	allowUnsandboxed := *sf.releaseAllowUnsandboxed
	if !flagsWasVisited(flags, "release-allow-unsandboxed") {
		allowUnsandboxed = base.ReleaseAllowUnsandboxed
	}
	allowSkippedProjectCheck := *sf.releaseAllowSkippedProjectCheck
	if !flagsWasVisited(flags, "release-allow-skipped-project-check") {
		allowSkippedProjectCheck = base.ReleaseAllowSkippedProjectCheck
	}
	return releasePolicyFromFlags(protectedPaths, maxFilesChanged, maxInsertions, rollbackPlan, allowOverrides, allowDependencyLockfileChanges, allowUnsandboxed, allowSkippedProjectCheck)
}

// apiStartTier2Settings folds the on-disk session config (the same
// defaults-then-file resolution a bare `factoryd <run>`/`worker`
// invocation already gets via loadDefaultSettings) with serveMain's own
// release/sandbox-resource flags into a single sessionconfig.Settings
// value: the Tier-2 half of what an API-started run's own
// resolveSettings() call resolves, once tier2SettingsOverride is set to
// the result (see serveMain's own assignment). releasePolicy is not
// simply serveMain's own raw -release-* flags: the
// caller has already resolved it session-config-first (baseSettings wins for any -release-*
// flag the operator did not pass explicitly; an explicit flag still
// wins) -- see resolveServeReleasePolicy, called just above its
// releasePolicy assignment. sandboxLimits still simply wins over the file
// for the fields it covers, unchanged. Every other Settings field (relay
// token/cost budgets and ceilings, registry-proxy upstreams and sizing,
// sandbox_docker/sandbox_user) comes from the same file a bare run would
// honor, rather than silently reverting to hardcoded defaults for the
// life of this `serve` process (the original bug this function closes).
// Factored out of serveMain so it can be tested directly, without needing
// a real HTTP listener or a full serveMain invocation.
func apiStartTier2Settings(base sessionconfig.Settings, releasePolicy release.MergePolicy, sandboxLimits sandboxResourceLimits) sessionconfig.Settings {
	settings := base
	settings.ReleaseProtectedPaths = strings.Join(releasePolicy.ProtectedPaths, ",")
	settings.ReleaseMaxFilesChanged = releasePolicy.MaxFilesChanged
	settings.ReleaseMaxInsertions = releasePolicy.MaxInsertions
	settings.ReleaseRollbackPlan = releasePolicy.RollbackPlan
	settings.ReleaseAllowOverrides = releasePolicy.AllowOverrides
	settings.ReleaseAllowDependencyLockfileChanges = releasePolicy.AllowDependencyLockfileChanges
	settings.ReleaseAllowUnsandboxed = releasePolicy.AllowUnsandboxed
	settings.ReleaseAllowSkippedProjectCheck = releasePolicy.AllowSkippedProjectCheck
	settings.SandboxMemory = sandboxLimits.memory
	settings.SandboxCPUs = sandboxLimits.cpus
	settings.SandboxTmpfsSize = sandboxLimits.tmpfsSize
	// workerUID: sandboxLimits.workerUID <= 0 (the zero value a
	// sandboxResourceLimits built without this field would still produce)
	// falls back to sandbox.DefaultWorkerUID here, the same "<=0 means
	// unset" convention Activities.sandboxWorkerUID already uses --
	// -sandbox-worker-uid's own flag default is already
	// sandbox.DefaultWorkerUID (never 0) for any real invocation, so this
	// fallback only ever matters for a caller that bypasses flag parsing
	// entirely.
	settings.SandboxWorkerUID = sandboxLimits.workerUID
	if settings.SandboxWorkerUID <= 0 {
		settings.SandboxWorkerUID = sandbox.DefaultWorkerUID
	}
	return settings
}

// apiSandboxPolicy narrows two capabilities found sitting wide open on an
// implicitly-trusted-caller assumption while sandbox_docker (just above)
// was already narrowed on the opposite, untrusted-caller assumption.
// Built once by
// serveMain from its own -api-allowed-sandbox-images flag, never from the
// request, the same way sandboxLimits and releasePolicy are: an
// authenticated POST /runs caller does not get to raise its own ceiling by
// asking.
type apiSandboxPolicy struct {
	// roles is baseSettings.Roles, carried through so an API-started run
	// resolves roles.execution/roles.review the same way the CLI/worker
	// path does (runMainWithReady, modelrole.SelectRoute) -- nil when
	// roles: is absent from session config.
	roles *sessionconfig.Roles
	// allowedImages is the exact set of digest-pinned sandbox_image values
	// POST /runs may request. Empty (the default) means only this
	// daemon's own configured sandbox image (defaultImage below, itself
	// resolved from -api-default-sandbox-image or, failing that, the
	// session config's sandbox_image -- see serveMain) is accepted -- not
	// "anything", the naive reading of an empty allowlist elsewhere in
	// this codebase (e.g. release.MergePolicy.ProtectedPaths) would
	// suggest -- because an image is a capability to run arbitrary code
	// inside the boundary, not a filter that's merely absent until
	// configured. There is no built-in canonical image to fall back to:
	// if this daemon itself has no sandbox image configured either, an
	// empty allowlist accepts NOTHING, rejecting every API-started run
	// closed rather than picking an image out of thin air.
	allowedImages []string
	// defaultImage is the sandbox_image applied when a POST /runs request
	// leaves its own SandboxImage field empty -- this daemon's own
	// -api-default-sandbox-image, falling back to the session config's
	// own sandbox_image when that flag is unset (see serveMain). Also
	// doubles as the implicit allowlist when allowedImages is empty (see
	// imageAllowed) -- always itself validated against allowedImages at
	// daemon startup (see serveMain), never only here, so a misconfigured
	// default fails loud immediately rather than rejecting every request
	// that omits the field. An explicit request SandboxImage always wins
	// over this -- see resolveAPIImages.
	defaultImage string
	// egressCABundle is this daemon's own -egress-ca-bundle, forwarded
	// verbatim into every API-started run's own -egress-ca-bundle
	// (apiStartStarter) -- a daemon-static operator setting, like
	// defaultImage above, never a per-request api.StartRequest field:
	// it names a path on THIS machine, meaningless coming from a caller.
	egressCABundle string
}

// imageAllowed reports whether image may be used as an API-started run's
// sandbox_image. See apiSandboxPolicy.allowedImages's own doc comment for
// why an empty configured allowlist means exactly this daemon's own
// configured defaultImage, not every image -- and, when defaultImage is
// itself unconfigured, means nothing at all.
func (p apiSandboxPolicy) imageAllowed(image string) bool {
	if len(p.allowedImages) == 0 {
		return p.defaultImage != "" && image == p.defaultImage
	}
	for _, allowed := range p.allowedImages {
		if image == allowed {
			return true
		}
	}
	return false
}

// allowedImagesDescription renders p.allowedImages for an error message,
// spelling out the implicit default (or its absence) rather than printing
// an empty list that would read as "nothing is allowed" unconditionally.
func (p apiSandboxPolicy) allowedImagesDescription() string {
	if len(p.allowedImages) == 0 {
		if p.defaultImage == "" {
			return "none configured"
		}
		return p.defaultImage
	}
	return strings.Join(p.allowedImages, ", ")
}

// resolveAPIImage applies sandboxPolicy's configured defaultImage to a POST
// /runs request's own SandboxImage field -- the default fills in only when
// the request field is empty, so an explicit request value always wins --
// and then enforces the allowlist against whichever value ends up in play,
// request-supplied or defaulted. A caller that never mentions sandbox_image
// at all must still only ever reach an operator-approved image, never skip
// the allowlist by omission.
func resolveAPIImage(req api.StartRequest, policy apiSandboxPolicy) (string, error) {
	sandboxImage := req.SandboxImage
	if sandboxImage == "" {
		sandboxImage = policy.defaultImage
	}
	if sandboxImage != "" && !policy.imageAllowed(sandboxImage) {
		return "", fmt.Errorf("%w: sandbox_image %q is not allowlisted for API-started runs -- allowed: %s (configure -api-allowed-sandbox-images, or use the CLI directly for any other image)", api.ErrInvalidStartRequest, sandboxImage, policy.allowedImagesDescription())
	}
	return sandboxImage, nil
}

// resolveDataDirFromSessionConfig (validate.go) resolves -data-dir from the
// session config's data_dir key so `factoryd serve` reads the same
// durable-record directory `factoryd worker` writes to without requiring
// an operator to repeat -data-dir on both commands -- the "one documented
// command" goal, applied to serve/worker
// agreeing on a directory the same way worker's own applySessionConfig
// already lets its sandbox/relay flags agree with `factoryd <run>`) --
// shared with `factoryd status`/`factoryd watch` (see that function's own
// doc comment).
