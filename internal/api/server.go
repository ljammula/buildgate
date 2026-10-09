// Package api exposes durable factoryd run records over HTTP and SSE, plus
// authenticated run-start and operator-override endpoints.
package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"buildgate/internal/consoleweb"
	"buildgate/internal/daemonheartbeat"
	"buildgate/internal/handoff"
	"buildgate/internal/notify"
	"buildgate/internal/progress"
	"buildgate/internal/release"
	"buildgate/internal/request"
	"buildgate/internal/run"
	"buildgate/internal/sessionconfig"
	"buildgate/internal/triage"
)

const defaultPollInterval = time.Second

type Option func(*Server)

// ErrConflict is returned by a RunStarter when the requested run cannot be
// started because an equivalent run is already present or in progress.
var ErrConflict = errors.New("run already exists")

// ErrInvalidStartRequest distinguishes caller input errors from execution or
// infrastructure failures reported by a RunStarter.
var ErrInvalidStartRequest = errors.New("invalid start request")

// ErrDaemonConflict reports that a daemon is already managed for a
// repository. It is mapped to HTTP 409 by the daemon lifecycle handlers.
var ErrDaemonConflict = errors.New("daemon already exists")

// ErrDaemonNotFound reports that no daemon is managed for a repository. It is
// mapped to HTTP 404 by the daemon lifecycle handlers.
var ErrDaemonNotFound = errors.New("daemon not found")

// ErrInvalidDaemonRequest distinguishes malformed daemon lifecycle input.
var ErrInvalidDaemonRequest = errors.New("invalid daemon request")

// StartRequest is the JSON request accepted by POST /runs. The execution
// policy and validation remain in the injected RunStarter; the API only
// decodes the transport shape and authenticates the caller.
type StartRequest struct {
	ID                  string `json:"id,omitempty"`
	Ticket              string `json:"ticket"`
	Workspace           string `json:"workspace,omitempty"`
	WorkspacePath       string `json:"workspace_path,omitempty"`
	Spec                string `json:"spec,omitempty"`
	SpecPath            string `json:"spec_path,omitempty"`
	BuildAppInterpreter string `json:"build_app_interpreter,omitempty"`
	BuildAppScript      string `json:"build_app_script,omitempty"`
	BuildAppMaxAttempts int    `json:"build_app_max_attempts,omitempty"`
	MaxRounds           int    `json:"max_rounds,omitempty"`
	TimeoutMinutes      int    `json:"timeout_minutes,omitempty"`
	Timeout             string `json:"timeout,omitempty"`
	VerifyCommand       string `json:"verify_command,omitempty"`
	VerifyMaxAttempts   int    `json:"verify_max_attempts,omitempty"`
	// FullSuiteCommand is cmd/factoryd's -full-suite-command: an optional
	// repo-wide regression check (gap 3 of the plan's 2026-08-28 readiness
	// review), run after canonical verification passes. Empty (default)
	// skips it entirely, the same convention as every other optional gate.
	FullSuiteCommand string `json:"full_suite_command,omitempty"`
	// FullSuiteCadence runs the declared full-suite command on every Nth
	// valid prior-run chain slice; zero or one preserves the default behavior.
	FullSuiteCadence     int    `json:"full_suite_cadence,omitempty"`
	RequireDeclaredScope bool   `json:"require_declared_scope,omitempty"`
	TemporalAddress      string `json:"temporal_address,omitempty"`
	Repository           string `json:"repository,omitempty"`
	// SkipProjectCheck bypasses factoryd <run>'s mandatory project-bootstrap
	// preflight (converted from opt-in to required, 2026-08-29), mirroring
	// the CLI's own -skip-project-check -- the only escape hatch for a
	// project that hasn't adopted the spec/contract/architecture
	// convention yet. Without this field, POST /runs had no way to reach
	// that escape hatch at all: every API-started run against such a
	// project failed synchronously, with no path to the bypass the CLI
	// itself advertises (found via a GitHub Codex App review round,
	// 2026-08-29).
	SkipProjectCheck bool `json:"skip_project_check,omitempty"`
	// PreflightProfile mirrors factoryd <run>'s own -preflight-profile:
	// "" (default, strict) or "brownfield" -- a narrower escape hatch than
	// SkipProjectCheck for an existing repo that only needs its own real
	// Verify-Command and a ticket, not three hand-authored docs (see that
	// CLI flag's own doc comment).
	PreflightProfile string `json:"preflight_profile,omitempty"`
	// OpenPullRequest mirrors factoryd <run>'s own -open-pull-request: on
	// acceptance, push this run's own branch and open a draft, evidence-
	// carrying pull request (see that flag's own doc comment). Off by
	// default, the same as the CLI flag -- an API caller must opt in
	// explicitly to this outward-facing side effect too.
	OpenPullRequest bool `json:"open_pull_request,omitempty"`
	// SandboxImage mirrors factoryd <run>'s own -sandbox-image flag. Before
	// this field existed, POST /runs had no way to reach sandboxed execution
	// at all -- only a bare CLI invocation could ever route a run through
	// Docker containment, so every API-started, serve-daemon-managed run
	// executed unsandboxed on the host regardless of operator intent (found
	// via the 2026-09-03 Opus factory-pipeline review).
	// SandboxImage must be digest-pinned (name@sha256:...), same as the CLI
	// flag; validation stays in the underlying factoryd <run> invocation,
	// not duplicated here.
	SandboxImage string `json:"sandbox_image,omitempty"`
	// SandboxDocker/SandboxUser are retained only so a request still naming
	// them gets a specific, actionable rejection rather than the generic
	// unknown-field error DisallowUnknownFields would produce. Neither is
	// honored any more: flags-consolidate (2026-09-10) made both
	// session-config-only (sandbox_docker/sandbox_user), so factoryd <run>
	// has no flag left to forward them through -- the daemon's own session
	// config decides, identically for every run it starts. The rejection is
	// deliberate rather than a silent ignore, since sandbox_user in
	// particular decides what identity a container's code runs as.
	SandboxDocker string `json:"sandbox_docker,omitempty"`
	SandboxUser   string `json:"sandbox_user,omitempty"`
	// AllowUnsandboxed no longer has any effect: Docker containment is
	// unconditional, with no host-execution opt-out anywhere in factoryd
	// (the CLI's own -allow-unsandboxed is gone too). The field stays here
	// only so a caller that still sends it gets an explicit rejection
	// instead of silently having it ignored, matching SandboxDocker/
	// SandboxUser's own treatment above.
	AllowUnsandboxed bool `json:"allow_unsandboxed,omitempty"`
}

// RunStarter starts one run and returns its initial durable record. A starter
// should return the record after it is durably created; execution may continue
// asynchronously. RunStarter is deliberately injectable so the HTTP handler
// can be tested without a Temporal server or subprocesses.
type RunStarter func(context.Context, StartRequest) (*run.Run, error)

// ProjectCheckRequest is POST /projects/check's request body: the same
// Workspace/Ticket/Repository an operator would otherwise only discover
// were wrong by actually starting a real run and watching it fail closed
// on the project-bootstrap preflight. Ticket is optional -- an operator
// checking a project before it has any tickets drafted yet still gets a
// verdict on product_spec_frozen/program_design_structure/
// architecture_structure alone, matching cmd/factoryd's own `check-project`
// CLI subcommand where -ticket is one of several independently optional
// flags.
type ProjectCheckRequest struct {
	Repository string `json:"repository,omitempty"`
	Workspace  string `json:"workspace"`
	Ticket     string `json:"ticket,omitempty"`
	// PreflightProfile mirrors cmd/factoryd's own -preflight-profile:
	// "" (default, strict) or "brownfield" (skip product_spec_frozen/
	// program_design_structure entirely, treat architecture_structure as
	// advisory -- see that flag's own doc comment). Lets this preview
	// report the verdict a real brownfield-profile run would actually
	// reach, instead of always previewing the strict profile regardless
	// of what the caller intends to start.
	PreflightProfile string `json:"preflight_profile,omitempty"`
}

// ProjectCheckResult is one project-bootstrap structural check's outcome --
// the API's own copy of cmd/factoryd's identically-shaped ProjectCheckResult
// (an API package cannot import a main package, so this is a deliberate,
// narrow duplication of the wire shape alone, not the checking logic
// itself, which stays exactly where it already lives).
type ProjectCheckResult struct {
	Check   string   `json:"check"`
	Path    string   `json:"path"`
	Passed  bool     `json:"passed"`
	Reasons []string `json:"reasons,omitempty"`
	// Advisory mirrors cmd/factoryd's own ProjectCheckResult.Advisory --
	// true only for architecture_structure under the brownfield profile.
	Advisory bool `json:"advisory,omitempty"`
}

// ProjectCheckResponse is POST /projects/check's response: every declared
// check's own result, plus the overall verdict. Never partial -- every
// check ProjectChecker was asked to run appears here exactly once,
// regardless of whether it passed.
type ProjectCheckResponse struct {
	Passed bool                 `json:"passed"`
	Checks []ProjectCheckResult `json:"checks"`
}

// ProjectChecker evaluates a project's bootstrap artifacts against
// req.Workspace (and, if req.Ticket is set, the ticket file that
// identifier resolves to) and reports each check's own pass/fail verdict.
// Deliberately side-effect-free from this Server's own perspective: unlike
// RunStarter, a ProjectChecker call is a preview an operator can run
// before committing to anything, not a decision this Server's caller is
// making -- cmd/factoryd's own implementation writes no durable
// ProjectCheckRecord for this path, on purpose (see its own doc comment).
// A non-nil error here means the check itself could not be evaluated at
// all (a malformed request), not that a check failed -- a failed check is
// success from ProjectChecker's own perspective: it evaluated the project
// and got a real, reportable answer.
type ProjectChecker func(context.Context, ProjectCheckRequest) (ProjectCheckResponse, error)

// ProjectStats is GET /projects/{project}/stats's response: the same
// acceptance-rate/override-rate numbers `factoryd override-rate` already
// computes (CLI-only, repo-wide, not project-scoped), promoted to a
// per-project figure a team lead can actually decide on.
// OverrideRatePercent/MedianAcceptedCostMicroUSD are pointers, not zero
// values, so a project with zero accepted runs (nothing to compute a
// rate or median against) renders as "no data" rather than a misleading
// 0%/$0.
type ProjectStats struct {
	Project             string `json:"project"`
	TotalRuns           int    `json:"total_runs"`
	Accepted            int    `json:"accepted"`
	AcceptedViaOverride int    `json:"accepted_via_override"`
	OverrideRatePercent *int   `json:"override_rate_percent,omitempty"`
	// QuarantinedByCause keys are policy.Check names (e.g.
	// "canonical_verify", "required_files_changed", "diff_scope") --
	// one run quarantined on multiple failed checks counts once under
	// each, the same way the quarantine's own log line already names
	// every failing check together.
	QuarantinedByCause         map[string]int `json:"quarantined_by_cause,omitempty"`
	Halted                     int            `json:"halted"`
	MedianAcceptedCostMicroUSD *int64         `json:"median_accepted_cost_micro_usd,omitempty"`
	// MedianAcceptedCostSubscriptionBilled is true when at least one
	// accepted run contributing to MedianAcceptedCostMicroUSD was billed
	// to a ChatGPT/Copilot subscription (run.SubscriptionBilled) rather
	// than a metered API key. Like CostSummary.SubscriptionBilled, a
	// truthful "any" treatment for a figure that can mix subscription and
	// metered runs across a whole project, not a per-run breakdown this
	// aggregate doesn't carry. The console renders this true with the
	// softer subscriptionCostAggregateSuffix wording ("includes
	// subscription-billed runs"), not cmd/factoryd's own single-run
	// subscriptionCostSuffix ("billed to your subscription"): the median
	// can mix runs, so asserting the WHOLE figure was subscription-billed
	// can be false.
	MedianAcceptedCostSubscriptionBilled bool `json:"median_accepted_cost_subscription_billed,omitempty"`
	// MedianAcceptedTokens is the median total relay token count (input +
	// output) across accepted runs, computed the same way
	// MedianAcceptedCostMicroUSD is -- nil for a project with zero
	// accepted runs, never a misleading 0.
	MedianAcceptedTokens *int64 `json:"median_accepted_tokens,omitempty"`
}

// ProjectStatsProvider computes ProjectStats for project. A non-nil error
// means the computation itself failed (e.g. the durable run store could
// not be read), never that the project has no runs -- a project with zero
// runs is a valid, reportable ProjectStats{Project: project}, not an
// error.
type ProjectStatsProvider func(ctx context.Context, project string) (ProjectStats, error)

// DaemonStatus is the read-only status exposed for one serve-managed
// factoryd supervise child.
type DaemonStatus struct {
	Repository         string `json:"repository"`
	State              string `json:"state"`
	PID                int    `json:"pid,omitempty"`
	StartedAt          string `json:"started_at,omitempty"`
	HeartbeatUpdatedAt string `json:"heartbeat_updated_at,omitempty"`
}

// DaemonController owns the lifecycle of serve-managed supervisor children.
// The interface keeps HTTP tests independent of subprocesses and Temporal.
type DaemonController interface {
	List(context.Context) ([]DaemonStatus, error)
	Start(context.Context, string) (DaemonStatus, error)
	Stop(context.Context, string) error
}

// WithPollInterval changes how often SSE streams check the durable record.
func WithPollInterval(interval time.Duration) Option {
	return func(s *Server) {
		if interval > 0 {
			s.pollInterval = interval
		}
	}
}

// WithOverrideToken enables POST /runs/{id}/override, requiring every
// request to present it as `Authorization: Bearer <token>`. The endpoint is
// independently authenticated from POST /runs, and every read route this
// Server exposes remains read-only. Found via
// review: the original server's routes were all read-only, which is
// exactly why it was never designed with any authentication boundary —
// registering a write route with that same lack of auth would let any
// network client reachable by the listen address (by default, all
// interfaces) mark a quarantined run accepted or halted under an arbitrary
// claimed `by` identity. Without this option, overrideRun refuses every
// request outright (fail closed) rather than leave the endpoint open by
// default.
//
// This token is a genuine authentication boundary against a network
// caller, but NOT against an untrusted build_app.py/canonical-verify
// subprocess running under the same Unix account as this Server's own
// process — found via review, environment-variable hygiene alone can't
// close that gap. cmd/factoryd's own `realMain` already refuses to run at
// all if this token is present in a *run* invocation's environment (see
// overrideTokenEnvironmentVariable's doc comment), which closes the
// "shared EnvironmentFile" misconfiguration class. But the token still
// necessarily lives in *this* process's (`factoryd serve`'s) own
// environment, and on Linux any other process sharing this one's UID —
// not just its children — can read that directly via
// /proc/<this-pid>/environ, with or without ever inheriting it. There is
// no code-level fix for that: it requires running `factoryd serve` under
// a Unix account distinct from whatever account executes build_app.py/
// canonical-verify, so that OS-level permissions (not merely env-var
// hygiene) are the actual boundary. Documented as a known, explicitly
// tracked deployment precondition (see CLAIMS.md), not silently assumed.
func WithOverrideToken(token string) Option {
	return func(s *Server) {
		s.overrideToken = token
	}
}

// WithStartToken enables POST /runs, requiring Authorization: Bearer <token>.
// An empty token disables the endpoint and makes it fail closed.
func WithStartToken(token string) Option {
	return func(s *Server) {
		s.startToken = token
	}
}

// WithAuthToken is a concise alias for WithStartToken for callers that use a
// single bearer credential for their control-plane write routes.
func WithAuthToken(token string) Option { return WithStartToken(token) }

// WithReadToken gates this Server's read routes (GET /runs, GET /runs/{id},
// GET /runs/{id}/events, GET /runs/{id}/diff, GET /projects) behind
// Authorization: Bearer <token>. Unlike WithOverrideToken/WithStartToken, an
// empty (the default, unset) token does NOT disable these routes -- they
// predate this option and are read-only, so leaving it unset preserves the
// prior unauthenticated-read behavior for a caller that already relies on
// it from a trusted network. What actually bounds exposure by default is
// the listen address itself (`factoryd serve`'s own -addr now defaults to
// loopback) -- this token is for an operator who deliberately binds wider
// than that and wants the read surface gated too (found via the
// 2026-09-05 Opus review, S3: GET /runs/{id}/diff served every run's
// complete unified source diff, absolute host paths, and provider/model
// identity to any unauthenticated caller that could reach the port).
func WithReadToken(token string) Option {
	return func(s *Server) {
		s.readToken = token
	}
}

// WithCORSAllowOrigin lets a single named browser origin (e.g. the
// console's own Vite dev server, http://localhost:PORT) read this Server's
// responses cross-origin. Empty (the default) keeps CORS off entirely --
// identical to this Server's behavior before this option existed. Never
// reflects the request's own Origin back: only an exact match against the
// one origin configured here gets Access-Control-Allow-Origin at all, so
// this only changes what a browser page permits itself to *read* from a
// port it can already reach -- it grants no new network reachability, and
// the existing token gates (WithReadToken/WithStartToken/
// WithOverrideToken) still run first and are completely unaffected (found
// via a live Playwright run against the documented USAGE.md §9 console
// quickstart: `factoryd serve` sets no CORS headers at all, so that
// flow's browser fetches fail closed with a CORS error rather than ever
// reaching authorization).
//
// "*" is refused here too, not just by serveMain's own
// validateCORSAllowOrigin flag check: a caller of this option directly
// (a test, another cmd entry point) must get the same guarantee this
// comment promises, not one that only holds for callers that happen to
// route through that one flag-parsing helper (found via adversarial
// review: the guarantee was previously documented here but enforced only
// at the CLI layer). ServeHTTP additionally never emits a literal "*" as
// Access-Control-Allow-Origin regardless of how corsAllowOrigin was set,
// as a second, redundant backstop.
func WithCORSAllowOrigin(origin string) Option {
	return func(s *Server) {
		if origin == "*" {
			return
		}
		s.corsAllowOrigin = origin
	}
}

// WithListenAddr tells this Server the -addr it is about to be served on
// (the exact string cmd/factoryd's serveMain passes to http.Server.Addr),
// purely so ServeHTTP's own Host-header check (DNS-rebinding defense) and
// authorizeRequestWrite's loopback-write relaxation ("no token for
// the single-machine case") know what a legitimate same-origin request's
// Host header looks like. Without this option -- every existing caller,
// including every httptest-based test in this package -- neither behavior
// activates: the Host check is skipped and every write route keeps
// requiring a bearer token exactly as before, so this option is additive,
// never a silent behavior change for a caller that doesn't opt in.
//
// addr must parse as host:port (net.SplitHostPort); a bare host, a port-
// only form, or anything else that fails to parse leaves this Server with
// loopback=false -- fails closed (both behaviors stay off) rather than
// guessing. host is loopback only for the literal strings "127.0.0.1",
// "localhost", or "::1" -- an empty host (the bind-all wildcard form,
// e.g. ":8090") is deliberately NOT loopback, and neither is any other
// literal address: an operator who binds wider than loopback gets
// exactly today's behavior (a bearer token is required for every write,
// and refused entirely if none is configured), matching the plan's own
// "with a wider bind ... behaviour is unchanged" scope decision.
func WithListenAddr(addr string) Option {
	return func(s *Server) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return
		}
		s.loopback = host == "127.0.0.1" || host == "localhost" || host == "::1"
		s.hostCheckPort = port
	}
}

// WithAllowedHosts adds extra Host header values ServeHTTP's own Host
// check (DNS-rebinding defense, below) accepts in addition to this
// Server's own true loopback address -- `factoryd serve -allowed-host`
// (added by an adversarial review, 2026-09-24). Without this, the Host
// check -- unconditional across every route once WithListenAddr is
// loopback, reads included -- 403s a request reaching this Server through
// anything that changes the Host header the server actually sees: an
// `ssh -L 9000:localhost:8090` tunnel (Host becomes "localhost:9000"), a
// reverse proxy, or `tailscale serve`, all regressions against the
// pre-Host-check behavior for an operator who runs factoryd on one
// machine and the browser on another. Each entry in hosts is an exact
// "host[:port]" string matched verbatim against r.Host (e.g.
// "localhost:9000", or a Tailscale MagicDNS name); no wildcard or port
// range is supported.
//
// Hosts accepted this way are for READS (and the console's own static
// assets) ONLY -- they never qualify for the no-token write relaxation.
// loopbackSameOriginWrite (authorizeRequestWrite's own gate) consults
// hostMatchesLoopback exclusively, never this list, so a write reaching
// this Server through an allowed host still requires the override token
// exactly as it did before this option existed; only a request whose Host
// names the server's own true loopback address can ever skip the token.
func WithAllowedHosts(hosts []string) Option {
	return func(s *Server) {
		for _, h := range hosts {
			if h == "" {
				continue
			}
			if s.allowedHosts == nil {
				s.allowedHosts = make(map[string]bool)
			}
			s.allowedHosts[h] = true
		}
	}
}

// WithTemporalUIURL sets GET /console-config.json's own temporal_ui_url:
// the base URL `factoryd serve -temporal-ui-url` names, which the
// console joins with a run's own TemporalWorkflowID to build an "Open in
// Temporal UI" link. Empty (the default) omits the field from
// /console-config.json entirely, matching that route's existing
// "omitted means not configured" convention (see consoleConfig's own doc
// comment).
func WithTemporalUIURL(url string) Option {
	return func(s *Server) {
		s.temporalUIURL = url
	}
}

// WithRunStarter supplies the execution implementation used by POST /runs.
// Without one, the endpoint is unavailable and fails closed rather than
// pretending a run was started.
func WithRunStarter(starter RunStarter) Option {
	return func(s *Server) {
		s.runStarter = starter
	}
}

// WithProjectChecker supplies the implementation used by POST
// /projects/check -- the console's "check project setup" preview. Without
// one, the endpoint is unavailable and fails closed rather than reporting
// a fabricated verdict.
func WithProjectChecker(checker ProjectChecker) Option {
	return func(s *Server) {
		s.projectChecker = checker
	}
}

// WithProjectStatsProvider supplies the implementation used by
// GET /projects/{project}/stats. Without one, the endpoint is unavailable
// and fails closed rather than reporting fabricated numbers.
func WithProjectStatsProvider(provider ProjectStatsProvider) Option {
	return func(s *Server) {
		s.projectStatsProvider = provider
	}
}

// WithDaemonController supplies the lifecycle implementation used by
// GET/POST /daemons. Without one, those routes fail closed as unavailable.
func WithDaemonController(controller DaemonController) Option {
	return func(s *Server) {
		s.daemonController = controller
	}
}

// WithPROpener supplies the implementation POST /requests/{id}/retry
// passes to request.Retry for its "accepted run, no PR yet" case.
// Without one, retry falls back to request.Retry's own nil-PROpener
// behavior: rebuilding the ticket, same as before this option existed.
func WithPROpener(opener request.PROpener) Option {
	return func(s *Server) {
		s.prOpener = opener
	}
}

// WithResumePreflight supplies the check POST /requests/{id}/resume runs
// before it accepts "round" for a lost build: whether the kept worktree can be
// continued. A refusal is a 409 naming the reasons. Without one the check is
// skipped here, and the build's own re-check still refuses an unsafe resume.
func WithResumePreflight(p request.ResumePreflight) Option {
	return func(s *Server) {
		s.resumePreflight = p
	}
}

// RequestWaker tells the worker that drives requestID that its request.json
// changed. Called after a successful write; an error is logged, never shown
// to the caller, since the write itself succeeded.
type RequestWaker func(ctx context.Context, requestID string) error

// WithRequestWaker supplies the RequestWaker POST /requests and the decision
// routes (approve, reject, retry, cancel) call after saving. Without one
// nothing is woken: a worker drains request.json on its own.
func WithRequestWaker(w RequestWaker) Option {
	return func(s *Server) {
		s.requestWaker = w
	}
}

// wakeRequest calls the configured RequestWaker, if any.
func (s *Server) wakeRequest(ctx context.Context, requestID string) {
	if s.requestWaker == nil {
		return
	}
	if err := s.requestWaker(ctx, requestID); err != nil {
		log.Printf("wake request %s: %v (the decision is saved; the worker picks it up at its next start)", requestID, err)
	}
}

// WithReleasePolicy configures the internal/release.MergePolicy overrideRun
// evaluates and records a decision against when it promotes a run to
// accepted. Without this option, the Server's zero-value policy applies --
// internal/release's own safe, restrictive default.
func WithReleasePolicy(policy release.MergePolicy) Option {
	return func(s *Server) {
		s.releasePolicy = policy
	}
}

type Server struct {
	dataDir      string
	pollInterval time.Duration
	// console serves the embedded console bundle (internal/consoleweb),
	// built once here and shared by the GET / mount and the deep-link path.
	console       http.Handler
	overrideToken string
	startToken    string
	readToken     string
	// mcpToken supplies POST /mcp's bearer token (see WithMCPToken); nil or
	// "" leaves the endpoint off.
	mcpToken func() string
	// mcpHandler is the MCP SDK's transport, behind serveMCP's token check.
	mcpHandler http.Handler
	// mcpSubmits are the times of the submit_request calls that created a
	// request within mcpSubmitWindow (see mcpSubmitAllowed).
	mcpSubmitMu          sync.Mutex
	mcpSubmits           []time.Time
	runStarter           RunStarter
	projectChecker       ProjectChecker
	projectStatsProvider ProjectStatsProvider
	daemonController     DaemonController
	// prOpener is request.Retry's own "accepted run, no PR yet" side
	// effect (see WithPROpener) -- nil is safe (Retry's own documented
	// fallback), so a Server built without this option still handles
	// retry, just via the pre-#5b rebuild behavior.
	prOpener request.PROpener
	// resumePreflight checks a "round" resume of a lost build (see
	// WithResumePreflight); nil skips it.
	resumePreflight request.ResumePreflight
	// requestWaker wakes a request's worker after a write (see WithRequestWaker); nil is safe.
	requestWaker RequestWaker
	// releasePolicy is used only to record a release decision (evaluate +
	// durably save, no merge/push/deploy side effect -- see
	// internal/release/decision.go) when this Server's own overrideRun
	// promotes a run to accepted. Its zero value is internal/release's own
	// safe, restrictive default (see MergePolicy's doc comment), so an
	// unconfigured Server still records decisions, just against that
	// default.
	releasePolicy   release.MergePolicy
	startMu         sync.Mutex
	mux             *http.ServeMux
	corsAllowOrigin string
	// loopback and hostCheckPort are set by WithListenAddr, and only by
	// it -- every existing caller (every httptest-based test in this
	// package included) that doesn't call it gets loopback=false, so
	// ServeHTTP's own Host check and the loopback-write relaxation
	// below are both inert unless an operator opts in by telling this
	// Server its real -addr (see WithListenAddr's own doc comment).
	loopback      bool
	hostCheckPort string
	// allowedHosts is WithAllowedHosts' own set of extra Host header
	// values ServeHTTP's Host check accepts alongside hostMatchesLoopback
	// -- see hostAllowed and WithAllowedHosts' own doc comments. nil (the
	// default) accepts none, so a caller that never opts in gets exactly
	// today's loopback-only behavior.
	allowedHosts map[string]bool
	// allowedWorkspaces is WithWorkspaces' own list -- see that Option's
	// doc comment and workspaceAllowed (request_create.go) for how POST
	// /requests and GET /workspaces use it.
	allowedWorkspaces []string
	// temporalUIURL is GET /console-config.json's own temporal_ui_url --
	// the base URL the console builds an "Open in Temporal UI" link
	// from, joined with a run's own TemporalWorkflowID. Empty (default)
	// omits the field entirely; see WithTemporalUIURL.
	temporalUIURL string
	// sessionTokenCeiling/sessionCostCeilingMicroUSD are the session
	// config's own effective relay ceilings (sessionconfig.Settings.
	// EffectiveRelayCeilings), set by WithSessionRelayCeilings and
	// threaded into requestsubmit.Params.SessionTokenCeiling/
	// SessionCostCeilingMicroUSD by createRequest, so POST /requests'
	// tighten-only .factory.yml check compares a repo's committed
	// ceiling against the ceiling this server would actually launch a
	// run under, not requestsubmit's own hardcoded legacy defaults. Zero
	// (the default for a Server built without that option, e.g. most
	// tests) means "compare against those legacy defaults instead," same
	// as before this option existed.
	sessionTokenCeiling        int64
	sessionCostCeilingMicroUSD int64
	// settings is the daemon's own resolved session config, set by
	// WithSessionRoles -- POST /requests' only use of it today is
	// validating createRequestBody.Models against roles.<role>.allowed
	// (sessionconfig.ValidateRequestModels, called inside
	// requestsubmit.Submit). The zero value (a Server built without that
	// option, e.g. most tests) has no roles: configured at all, so a
	// caller that never sends "models" is unaffected and one that does
	// gets ValidateRequestModels' own legacy-mode refusal.
	settings sessionconfig.Settings
}

// WithSessionRelayCeilings sets the session config's own effective relay
// ceilings (sessionconfig.Settings.EffectiveRelayCeilings) POST /requests
// compares a workspace's .factory.yml against -- see Server.
// sessionTokenCeiling's own doc comment.
func WithSessionRelayCeilings(tokenCeiling int64, costCeilingMicroUSD int64) Option {
	return func(s *Server) {
		s.sessionTokenCeiling = tokenCeiling
		s.sessionCostCeilingMicroUSD = costCeilingMicroUSD
	}
}

// WithSessionRoles sets the daemon's own resolved session config so POST
// /requests can validate a "models" body field against roles.<role>.
// allowed -- see Server.settings' own doc comment.
func WithSessionRoles(settings sessionconfig.Settings) Option {
	return func(s *Server) {
		s.settings = settings
	}
}

func NewServer(dataDir string, opts ...Option) *Server {
	s := &Server{
		dataDir:      dataDir,
		pollInterval: defaultPollInterval,
		mux:          http.NewServeMux(),
	}
	for _, opt := range opts {
		opt(s)
	}
	s.mux.HandleFunc("GET /healthz", s.healthz)
	s.mux.HandleFunc("GET /projects", s.listProjects)
	s.mux.HandleFunc("POST /projects/check", s.checkProject)
	s.mux.HandleFunc("GET /runs", s.listRuns)
	s.mux.HandleFunc("GET /queue-run", s.getWorkerStatus)
	s.mux.HandleFunc("POST /runs", s.startRun)
	s.mux.HandleFunc("GET /daemons", s.listDaemons)
	s.mux.HandleFunc("POST /daemons/start", s.startDaemon)
	s.mux.HandleFunc("POST /daemons/stop", s.stopDaemon)
	s.mux.HandleFunc("POST /runs/{id}/start", s.startRunAtID)
	s.mux.HandleFunc("GET /runs/{id}", s.getRun)
	s.mux.HandleFunc("GET /runs/{id}/events", s.streamRunEvents)
	s.mux.HandleFunc("GET /runs/{id}/progress", s.streamRunProgress)
	s.mux.HandleFunc("GET /runs/{id}/log", s.streamRunLog)
	s.mux.HandleFunc("GET /runs/{id}/diff", s.getRunDiff)
	s.mux.HandleFunc("GET /runs/{id}/release", s.getRunRelease)
	s.mux.HandleFunc("GET /runs/{id}/handoff", s.getRunHandoff)
	s.mux.HandleFunc("GET /projects/{project}/release", s.getProjectRelease)
	s.mux.HandleFunc("GET /projects/{project}/stats", s.getProjectStats)
	s.mux.HandleFunc("GET /projects/{project}/observations", s.getProjectObservations)
	s.mux.HandleFunc("POST /runs/{id}/override", s.overrideRun)
	s.mux.HandleFunc("GET /requests", s.listRequests)
	s.mux.HandleFunc("POST /requests", s.createRequest)
	s.mux.HandleFunc("GET /workspaces", s.listWorkspaces)
	s.mux.HandleFunc("GET /requests/events", s.streamRequestEvents)
	s.mux.HandleFunc("GET /requests/{id}", s.getRequest)
	s.mux.HandleFunc("POST /requests/{id}/approve", s.approveRequest)
	s.mux.HandleFunc("POST /requests/{id}/reject", s.rejectRequest)
	s.mux.HandleFunc("POST /requests/{id}/retry", s.retryRequest)
	s.mux.HandleFunc("POST /requests/{id}/resume", s.resumeRequest)
	s.mux.HandleFunc("POST /requests/{id}/cancel", s.cancelRequest)
	s.mux.HandleFunc("PUT /requests/{id}/spec", s.updateRequestSpec)
	s.mux.HandleFunc("PUT /requests/{id}/tickets/{n}", s.updateRequestTicket)
	s.mux.HandleFunc("GET /requests/{id}/oracle", s.listRequestOracle)
	s.mux.HandleFunc("GET /requests/{id}/oracle/{name}", s.getRequestOracleFile)
	s.mux.HandleFunc("PUT /requests/{id}/oracle/{name}", s.putRequestOracleFile)
	s.mux.HandleFunc("GET /requests/{id}/tickets/{n}/oracle", s.listTicketOracle)
	s.mux.HandleFunc("GET /requests/{id}/tickets/{n}/oracle/{name}", s.getTicketOracleFile)
	s.mux.HandleFunc("GET /requests/{id}/revisions", s.listRequestRevisions)
	s.mux.HandleFunc("GET /requests/{id}/revisions/{n}", s.getRequestRevision)
	s.mux.HandleFunc("GET /console-config.json", s.consoleConfig)
	s.registerMCP()
	// Registered last, and unauthenticated like every other static asset a
	// browser needs before it can even attempt a request: net/http's
	// ServeMux dispatches by pattern specificity, not registration order,
	// so every API route above still wins over this catch-all "/" for its
	// own path -- ServeHTTP's own serveConsoleDeepLink call runs first for
	// the three routes that also need the console shell despite an exact
	// API pattern above matching them. Serves the embedded console bundle
	// (internal/consoleweb) -- same origin as this API, so a browser needs
	// no CORS configuration (-cors-allow-origin) to load it at all.
	s.console = consoleweb.Handler()
	s.mux.Handle("GET /", s.console)
	return s
}

// consoleConfigView is GET /console-config.json's JSON shape.
type consoleConfigView struct {
	// TemporalUIURL is the -temporal-ui-url flag's value, omitted when
	// unset (see WithTemporalUIURL's doc comment).
	TemporalUIURL string `json:"temporal_ui_url,omitempty"`
	// WritesEnabled reports whether a same-origin request from this
	// console (the very request this route just answered) would be
	// allowed to call the request write routes (approve, reject, spec/
	// ticket/oracle edits, retry, cancel) with no bearer token of its own
	// -- true exactly when the loopback relaxation applies to this
	// request (loopbackSameOriginWrite), false when an override token is
	// configured (the console has no way to supply one -- see this
	// route's own doc comment on why a token is never handed out here)
	// or this Server isn't bound to loopback at all. The console uses
	// this instead of gating its own buttons on a baked-in token that no
	// shipped binary ever has.
	WritesEnabled bool `json:"writes_enabled"`
	// ReleasePolicyWarning surfaces a release-policy denial in the
	// console: non-empty
	// exactly when s.releasePolicy.CanNeverAllow() -- the same condition
	// `factoryd doctor`'s own doctorCheckReleasePolicy warns about -- so
	// a request board that would otherwise show every PR silently
	// withheld can instead tell the operator why and how to fix it.
	// Read-only: this route never accepts a write, so exposing the
	// reason (never a credential -- MergePolicy carries none) here does
	// not weaken any trust boundary. Omitted/empty when the policy is
	// usable.
	ReleasePolicyWarning string `json:"release_policy_warning,omitempty"`
}

// consoleConfig serves the console's own runtime-config discovery point.
// Unauthenticated, like the console's other static assets --
// FACTORYD_API_READ_TOKEN/FACTORYD_API_START_TOKEN, if set, remain a
// build-time VITE_ variable (console/src/app/config.ts),
// not something a runtime config route could hand a browser without
// defeating the point of gating those tokens at all.
func (s *Server) consoleConfig(w http.ResponseWriter, r *http.Request) {
	view := consoleConfigView{
		TemporalUIURL: s.temporalUIURL,
		WritesEnabled: s.overrideToken == "" && s.loopbackSameOriginWrite(r),
	}
	if s.releasePolicy.CanNeverAllow() {
		view.ReleasePolicyWarning = "release policy denies every PR unconditionally (release_max_files_changed/release_max_insertions is 0 or release_rollback_plan is empty) -- add release_max_files_changed, release_max_insertions, and release_rollback_plan to your session config (factoryd init-config's own scaffold now includes them)"
	}
	writeJSON(w, http.StatusOK, view)
}

// consoleDeepLinkPatterns are the GET routes the console's router
// (console/src/routes/paths.ts) also makes a directly navigable browser URL
// (its request detail, run detail and project release deep links) --
// see ServeHTTP's own comment on the ambiguity this creates once the
// console shares an origin with this Server. The runs list and project
// stats page routes need no entry here at all: `/`, `/ops`, and
// `/triage` don't collide with an existing API GET pattern, so
// net/http's own least-specific-pattern fallback ("GET /", the console
// mount below) already serves the shell for those on any method,
// including a browser's plain navigation. Only a page route that IS also
// a real API GET pattern needs disambiguating, which is every entry below.
var consoleDeepLinkPatterns = map[string]bool{
	"GET /requests/{id}":                   true,
	"GET /runs":                            true,
	"GET /runs/{id}":                       true,
	"GET /projects/{project}/release":      true,
	"GET /projects/{project}/stats":        true,
	"GET /projects/{project}/observations": true,
}

// wantsHTMLNavigation reports whether r's Accept header's first
// preference is text/html -- what every mainstream browser sends for a
// real top-level navigation (a typed URL, a refresh, a followed link),
// and what neither curl, a Go http.Client/httptest request, nor a
// browser's own fetch()/XHR call (used by console/src/api/http.ts,
// including for the very same paths this disambiguates) ever sends
// without a caller explicitly setting it. A conservative, narrow signal
// on purpose: it only ever changes behavior for the three routes
// consoleDeepLinkPatterns names, so every other route -- and every
// existing caller of those three routes that doesn't send this header --
// keeps exactly its prior behavior.
func wantsHTMLNavigation(r *http.Request) bool {
	first, _, _ := strings.Cut(r.Header.Get("Accept"), ",")
	return strings.TrimSpace(first) == "text/html"
}

// serveConsoleDeepLink serves the embedded console shell for r and
// reports true if r is a real browser navigation (wantsHTMLNavigation) to
// one of consoleDeepLinkPatterns; otherwise it does nothing and reports
// false, leaving r to the normal mux dispatch that already serves that
// path's real JSON. s.mux.Handler peeks at which pattern r would match
// without invoking its handler -- the same lookup s.mux.ServeHTTP itself
// does internally, so calling it here first is side-effect-free.
func (s *Server) serveConsoleDeepLink(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodGet || !wantsHTMLNavigation(r) {
		return false
	}
	if _, pattern := s.mux.Handler(r); consoleDeepLinkPatterns[pattern] {
		s.console.ServeHTTP(w, r)
		return true
	}
	return false
}

func (s *Server) startRun(w http.ResponseWriter, r *http.Request) {
	s.startRunWithID(w, r, "")
}

func (s *Server) startRunAtID(w http.ResponseWriter, r *http.Request) {
	s.startRunWithID(w, r, r.PathValue("id"))
}

func (s *Server) startRunWithID(w http.ResponseWriter, r *http.Request, pathID string) {
	authorized := s.authorizeStart(r)
	if !authorized {
		writeError(w, http.StatusForbidden, "start endpoint is not authorized")
		return
	}
	if s.runStarter == nil {
		writeError(w, http.StatusNotFound, "start endpoint is not configured")
		return
	}

	var req StartRequest
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "decode request body")
		return
	}
	// Reject a second JSON value instead of silently accepting a request whose
	// body contains valid JSON followed by unrelated bytes.
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		writeError(w, http.StatusBadRequest, "request body must contain one JSON object")
		return
	}
	if pathID != "" {
		if req.ID != "" && req.ID != pathID {
			writeError(w, http.StatusBadRequest, "request id does not match path id")
			return
		}
		req.ID = pathID
	}
	if req.Ticket == "" || (req.Workspace == "" && req.WorkspacePath == "") || (req.Spec == "" && req.SpecPath == "") {
		writeError(w, http.StatusBadRequest, "ticket, workspace, and spec are required")
		return
	}
	s.startMu.Lock()
	defer s.startMu.Unlock()
	if req.ID != "" {
		if _, err := s.loadRun(req.ID); err == nil {
			writeError(w, http.StatusConflict, ErrConflict.Error())
			return
		} else if !errors.Is(err, os.ErrNotExist) {
			writeError(w, http.StatusInternalServerError, "check existing run")
			return
		}
	}
	started, err := s.runStarter(r.Context(), req)
	if errors.Is(err, ErrInvalidStartRequest) {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if errors.Is(err, ErrConflict) {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "start run")
		return
	}
	if started == nil {
		writeError(w, http.StatusInternalServerError, "start run returned no record")
		return
	}
	writeJSON(w, http.StatusAccepted, started)
}

// checkProject serves the console's "check project setup" preview: the
// same project-bootstrap verdict POST /runs would fail closed on, without
// ever starting a run or persisting a decision. Gated identically to
// POST /runs (authorizeStart) rather than treated as a read -- it accepts
// an arbitrary Workspace path and reports back what its
// filesystem content is, the same category of information POST /runs
// already discloses through its own failure messages, so it must not be
// reachable by a caller that couldn't reach that path via a real run
// either.
//
// Considered and accepted, not a new gap this introduces: unlike POST
// /runs, a call here has no side effect and no per-request cost (no
// worktree, no container, no relay), so a caller already holding Start
// capability for a repository can probe far more paths per second than a
// real run attempt ever allowed -- but that caller was already capable of
// learning "does this artifact exist and is it readable" one real,
// slower run at a time; this only changes the cost of information that
// authorization boundary already grants access to, not what is
// reachable. A future per-caller rate limit, if this class of use ever
// warrants one, belongs at this Server's general request-handling layer
// (nothing here does that today for any route), not invented narrowly
// for this one endpoint.
func (s *Server) checkProject(w http.ResponseWriter, r *http.Request) {
	authorized := s.authorizeStart(r)
	if !authorized {
		writeError(w, http.StatusForbidden, "start endpoint is not authorized")
		return
	}
	if s.projectChecker == nil {
		writeError(w, http.StatusNotFound, "project check endpoint is not configured")
		return
	}

	var req ProjectCheckRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "decode request body")
		return
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		writeError(w, http.StatusBadRequest, "request body must contain one JSON object")
		return
	}
	if strings.TrimSpace(req.Workspace) == "" {
		writeError(w, http.StatusBadRequest, "workspace is required")
		return
	}
	result, err := s.projectChecker(r.Context(), req)
	if err != nil {
		// ProjectChecker's own contract: a non-nil error means the check
		// itself could not be evaluated (a malformed Workspace/Ticket),
		// never that a check failed -- a failed check is success from its
		// own perspective (see its doc comment), so every error here is a
		// caller-input problem, not an internal fault.
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}

type daemonRequest struct {
	Repository string `json:"repository"`
}

func decodeDaemonRequest(r *http.Request) (daemonRequest, error) {
	var request daemonRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return daemonRequest{}, fmt.Errorf("decode request body: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return daemonRequest{}, errors.New("request body must contain one JSON object")
	}
	if strings.TrimSpace(request.Repository) == "" {
		return daemonRequest{}, errors.New("repository is required")
	}
	return request, nil
}

func (s *Server) listDaemons(w http.ResponseWriter, r *http.Request) {
	authorized := s.authorizeStart(r)
	if !authorized {
		writeError(w, http.StatusForbidden, "daemon endpoint is not authorized")
		return
	}
	if s.daemonController == nil {
		writeError(w, http.StatusNotFound, "daemon lifecycle is not configured")
		return
	}
	statuses, err := s.daemonController.List(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list daemons")
		return
	}
	writeJSON(w, http.StatusOK, statuses)
}

func (s *Server) startDaemon(w http.ResponseWriter, r *http.Request) {
	authorized := s.authorizeStart(r)
	if !authorized {
		writeError(w, http.StatusForbidden, "daemon endpoint is not authorized")
		return
	}
	if s.daemonController == nil {
		writeError(w, http.StatusNotFound, "daemon lifecycle is not configured")
		return
	}
	request, err := decodeDaemonRequest(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	status, err := s.daemonController.Start(r.Context(), request.Repository)
	switch {
	case errors.Is(err, ErrInvalidDaemonRequest):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, ErrDaemonConflict):
		writeError(w, http.StatusConflict, err.Error())
	case err != nil:
		writeError(w, http.StatusInternalServerError, "start daemon")
	default:
		writeJSON(w, http.StatusAccepted, status)
	}
}

func (s *Server) stopDaemon(w http.ResponseWriter, r *http.Request) {
	authorized := s.authorizeStart(r)
	if !authorized {
		writeError(w, http.StatusForbidden, "daemon endpoint is not authorized")
		return
	}
	if s.daemonController == nil {
		writeError(w, http.StatusNotFound, "daemon lifecycle is not configured")
		return
	}
	request, err := decodeDaemonRequest(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	err = s.daemonController.Stop(r.Context(), request.Repository)
	switch {
	case errors.Is(err, ErrInvalidDaemonRequest):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, ErrDaemonNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, ErrDaemonConflict):
		writeError(w, http.StatusConflict, err.Error())
	case err != nil:
		writeError(w, http.StatusInternalServerError, "stop daemon")
	default:
		writeJSON(w, http.StatusOK, map[string]string{"repository": request.Repository, "state": "stopped"})
	}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// DNS-rebinding defense: while bound to loopback
	// (WithListenAddr), every route -- reads and the console's
	// own static assets included, not just the write routes
	// authorizeRequestWrite relaxes below -- refuses a request whose Host
	// header doesn't name this server's own loopback address. Without
	// this, a page in the operator's browser at an attacker-controlled
	// domain that resolves to 127.0.0.1 (DNS rebinding) could already
	// read GET /runs/{id}/diff (unauthenticated by default -- see
	// WithReadToken's own doc comment) purely by getting the operator to
	// load it; a real same-origin request from the console itself always
	// carries a Host header naming this server, so this never affects
	// one. Runs first, before CORS: a rebinding request gets no CORS
	// headers either way, and this check is unconditional regardless of
	// method or route. hostAllowed also accepts any -allowed-host value
	// (WithAllowedHosts) -- an operator reaching this server
	// through an ssh tunnel, reverse proxy, or `tailscale serve`, where
	// the Host header this server sees isn't its own loopback address --
	// but never treats an allowed host as loopback for
	// loopbackSameOriginWrite's own no-token write relaxation; see that
	// function's own doc comment.
	if s.loopback && !s.hostAllowed(r) {
		writeError(w, http.StatusForbidden, "Host header does not match this server's own loopback address (127.0.0.1, localhost, or [::1], with this server's own port) or a configured -allowed-host -- refused to defend against DNS rebinding")
		return
	}
	if s.corsAllowOrigin != "" && s.corsAllowOrigin != "*" {
		// Always vary on Origin once CORS is configured at all, even for a
		// non-matching Origin -- a shared cache (or the browser's own HTTP
		// cache) must not reuse this response for a different origin's
		// request to the same URL.
		w.Header().Add("Vary", "Origin")
		if origin := r.Header.Get("Origin"); origin != "" && origin == s.corsAllowOrigin {
			w.Header().Set("Access-Control-Allow-Origin", s.corsAllowOrigin)
			if r.Method == http.MethodOptions {
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
	}
	// Three of the console's own directly-navigable browser URLs
	// (the request detail, run detail and project release deep links of
	// console/src/routes/paths.ts) are identically shaped to an existing single-resource
	// API route this Server already serves as JSON: GET /requests/{id},
	// GET /runs/{id}, GET /projects/{project}/release. That collision is
	// harmless while the console is served from its own dev-server origin
	// (the two are different URLs from the browser's perspective), but
	// mounting the console at this Server's own GET / (below) puts both
	// on the same origin, same path -- and net/http's own pattern
	// precedence (more specific patterns win) always routes a plain
	// request for one of those three paths to the existing JSON handler,
	// never the console shell. A real page load of one of those URLs
	// still needs the shell, not raw JSON, for the console's own router
	// to ever run in the first place -- and the console's own already-
	// running JS still needs the real JSON from that identical path once
	// it has (see the request page's own getRequest call in console/src/api/requests.ts).
	// serveConsoleDeepLink resolves the ambiguity: only a real top-level
	// browser navigation (as opposed to curl, a Go http.Client, an
	// existing test, or the console's own fetch()/XHR call) gets the
	// shell here; anything else reaches the JSON handler exactly as
	// before this mount existed.
	if s.serveConsoleDeepLink(w, r) {
		return
	}
	s.mux.ServeHTTP(w, r)
}

// healthz is a bare liveness check for this Server process — no dataDir
// read, no dependency on the daemon-side Temporal Worker actually
// servicing anything, just "did an HTTP handler run". Closes one of the
// plan's own named Phase 6 gaps ("no supervisor, health-check endpoint, or
// internal/api-driven lifecycle management yet"): an external process
// supervisor (systemd, launchd, a container orchestrator's liveness probe)
// needs something to poll that doesn't itself depend on the state it's
// meant to detect the absence of. Unauthenticated, like every other read
// route this Server exposes.
func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) listRuns(w http.ResponseWriter, r *http.Request) {
	authorized := s.authorizeRead(r)
	if !authorized {
		writeError(w, http.StatusForbidden, "read endpoint is not authorized")
		return
	}
	entries, err := os.ReadDir(filepath.Join(s.dataDir, "runs"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusInternalServerError, "read runs")
		return
	}

	runs := make([]*run.Run, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		r, err := run.Load(s.dataDir, entry.Name())
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "load run")
			return
		}
		runs = append(runs, r)
	}
	sort.Slice(runs, func(i, j int) bool {
		return createdAtAfter(runs[i].CreatedAt, runs[j].CreatedAt)
	})

	views := make([]runView, len(runs))
	for i, loaded := range runs {
		views[i] = s.runViewFor(loaded)
	}
	writeJSON(w, http.StatusOK, views)
}

// runView is GET /runs and GET /runs/{id}'s per-run JSON shape: the
// durable run.Run plus a handful of fields computed at read time from its
// progress feed (see internal/progress.Summary) so an operator watching
// the run list can tell "queued behind another run" / "stuck on stage X"
// / "silence" apart without opening the SSE stream -- see
// progress-contract.md's "Server-computed run view fields" addition
// (2026-09-18). Every field is omitempty and left zero on any error
// reading the feed: a stalled-indicator computation must never turn into
// a reason GET /runs fails outright.
type runView struct {
	*run.Run
	LastProgressAt string `json:"last_progress_at,omitempty"`
	CurrentStage   string `json:"current_stage,omitempty"`
	CurrentRound   int    `json:"current_round,omitempty"`
	MaxRounds      int    `json:"max_rounds,omitempty"`
	WaitingReason  string `json:"waiting_reason,omitempty"`
	// Stalled/StalledSinceSeconds are internal/progress.Stalled's verdict
	// (see its own doc comment) -- the single shared "silence is a bug"
	// rule, computed here so `factoryd status`, `factoryd watch`, and this
	// console can never disagree about what counts as stalled.
	Stalled             bool `json:"stalled,omitempty"`
	StalledSinceSeconds int  `json:"stalled_since_seconds,omitempty"`
	// ByModel is this run's own per-(role, model) token+cost breakdown
	// across its attempts, the same ModelUsage shape CostSummary.ByModel
	// uses -- computed by addRunModelUsage/finish so the run detail screen
	// renders role + model + tokens + cost the same way a request's
	// cost_summary does.
	ByModel []ModelUsage `json:"by_model,omitempty"`
	// TokensComplete is false when ByModel is a lower bound -- an attempt's
	// relay spend was only partly recovered after a relay crash, or the
	// AgentEvidence fallback had a round with no usable token figure (see
	// addRunModelUsage). Mirrors CostSummary.TokensComplete.
	TokensComplete bool `json:"tokens_complete"`
	// ComposePhases is what each phase launched from the target repo's
	// compose file (its services.<phase>.json), so the run page can show
	// which sidecars ran -- found missing in the 2026-09-29
	// todo-kafka-service demo. Build first, then verify, full suite, and
	// any gate in name order. Filled only for one run's own view (GET
	// /runs/{id} and its event stream), never the runs list; empty for a
	// run with no compose services.
	ComposePhases []composePhaseView `json:"compose_phases,omitempty"`
}

// composePhaseView and composeServiceView are this package's copy of the
// wire shape internal/sandbox.ComposeServicesReport writes -- read, not
// imported, like QueueEntry: the console's API has no business importing
// the Docker-facing sandbox package for a JSON file.
type composePhaseView struct {
	Phase          string               `json:"phase"`
	Enabled        bool                 `json:"enabled"`
	DisabledReason string               `json:"disabled_reason,omitempty"`
	Services       []composeServiceView `json:"services,omitempty"`
}

type composeServiceView struct {
	Name   string `json:"name"`
	Alias  string `json:"alias"`
	Port   int    `json:"port,omitempty"`
	Image  string `json:"image"`
	Digest string `json:"digest,omitempty"`
}

// composePhaseOrder ranks the phases every run has ahead of named gates.
var composePhaseOrder = map[string]int{"build": 0, "verify": 1, "full_suite_verify": 2}

// readComposePhases reads each services*.json under the run's compose
// directory. An unreadable file is skipped: this is display evidence, and
// the run record itself is unaffected.
func readComposePhases(dataDir, runID string) []composePhaseView {
	paths, _ := filepath.Glob(filepath.Join(run.Dir(dataDir, runID), "compose", "services*.json"))
	var phases []composePhaseView
	for _, path := range paths {
		b, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var phase composePhaseView
		if err := json.Unmarshal(b, &phase); err != nil {
			continue
		}
		name := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(path), "services"), ".json")
		phase.Phase = strings.TrimPrefix(name, ".")
		phases = append(phases, phase)
	}
	sort.Slice(phases, func(i, j int) bool {
		ri, iKnown := composePhaseOrder[phases[i].Phase]
		rj, jKnown := composePhaseOrder[phases[j].Phase]
		switch {
		case iKnown && jKnown:
			return ri < rj
		case iKnown != jKnown:
			return iKnown
		default:
			return phases[i].Phase < phases[j].Phase
		}
	})
	return phases
}

// runViewFor builds r's runView, reading its progress feed only when r is
// not yet confirmed terminal (run.Run.TerminalConfirmed, not the narrower
// terminal helper below) -- a finished run's progress.jsonl no longer
// changes, so re-reading it on every run-list poll would be pure waste. A
// read failure is logged and otherwise ignored, per this feed's own
// "never fail or halt a run" contract: the view's progress fields are
// simply left empty.
//
// TerminalConfirmed, not terminal(r): the two answer different questions
// and must not be conflated. terminal(r) decides whether streamRunEvents
// may safely close its SSE connection, and deliberately excludes
// StateQuarantined --
// a quarantined run is not "final" for a live watcher, since
// ApplyOverride can still move it to accepted or halted later (see that
// function's own doc comment). But for the narrower question this
// function asks -- "will this run's progress.jsonl ever change again" --
// a quarantined run's build has already stopped for good even though its
// *record* can still be overridden, so its stale last-progress timestamp
// must not keep being fed into progress.Stalled: before this fix, a
// quarantined run's runView kept computing Stalled fresh on every poll
// and reported a red "stalled" chip five minutes after quarantine, even
// though `factoryd status` already correctly treated it as done.
func (s *Server) runViewFor(r *run.Run) runView {
	view := runView{Run: r}
	acc := newModelUsageAccumulator()
	view.TokensComplete = addRunModelUsage(r, acc)
	view.ByModel = acc.finish()
	if r.TerminalConfirmed() {
		return view
	}
	sum, err := progress.Summary(progress.Path(s.dataDir, r.ID))
	if err != nil {
		log.Printf("run %s: failed to read progress summary: %v", r.ID, err)
		return view
	}
	view.LastProgressAt = sum.LastAt
	view.CurrentStage = sum.CurrentStage
	view.CurrentRound = sum.CurrentRound
	view.MaxRounds = sum.MaxRounds
	view.WaitingReason = sum.WaitingReason
	stalled, since := progress.Stalled(sum, r.CreatedAt, time.Now())
	view.Stalled = stalled
	if stalled {
		view.StalledSinceSeconds = int(since.Seconds())
	}
	return view
}

// WorkerStatus is GET /queue-run's response: whether a live `factoryd
// worker` is currently draining this data directory, derived from the
// same on-disk heartbeat file cmd/factoryd's worker writes (see
// daemonheartbeat.WorkerID's own doc comment). State is "absent" (no
// heartbeat file was ever written -- worker has never run against this
// data dir), "stale" (a heartbeat exists but is older than
// daemonheartbeat.WorkerStaleAfter -- worker crashed or the process
// was killed without a graceful shutdown), or "alive" (a fresh
// heartbeat). Exists because GET /daemons -- the console's prior signal
// for its "worker is not running" strip -- only ever lists daemons
// `factoryd serve` itself manages via s.daemonController and 404s
// otherwise (see listDaemons), so the strip never fired for the common
// setup where an operator runs `factoryd worker` by hand instead of
// through `factoryd serve`'s own daemon lifecycle.
type WorkerStatus struct {
	State string `json:"state"`
	// LastHeartbeat is the heartbeat's UpdatedAt (RFC3339Nano), or "" when
	// State is "absent".
	LastHeartbeat string `json:"last_heartbeat"`
}

// getWorkerStatus serves GET /queue-run. Read-only; gated by
// authorizeRead like every other read route this Server exposes -- see
// WithReadToken's doc comment.
func (s *Server) getWorkerStatus(w http.ResponseWriter, r *http.Request) {
	authorized := s.authorizeRead(r)
	if !authorized {
		writeError(w, http.StatusForbidden, "read endpoint is not authorized")
		return
	}
	hb, err := daemonheartbeat.Read(daemonheartbeat.WorkerPath(s.dataDir))
	if err != nil {
		writeJSON(w, http.StatusOK, WorkerStatus{State: "absent"})
		return
	}
	if daemonheartbeat.Stale(hb, time.Now(), daemonheartbeat.WorkerStaleAfter) {
		writeJSON(w, http.StatusOK, WorkerStatus{State: "stale", LastHeartbeat: hb.UpdatedAt})
		return
	}
	writeJSON(w, http.StatusOK, WorkerStatus{State: "alive", LastHeartbeat: hb.UpdatedAt})
}

// ProjectSummary is one entry in GET /projects's response: a distinct
// ProjectPath an operator has already run against, with the most recently
// used execution locations for that project so the console's "New run"
// intake can offer them as a quick-fill instead of requiring the operator
// to retype full paths for a project they've already used. Derived
// entirely from existing durable run records — a "project and repository
// selection" screen does not need a new mutable domain concept of its
// own to exist; every run already records exactly this.
type ProjectSummary struct {
	ProjectPath string `json:"project_path"`
	// Project is the release-decision/kill-switch project identifier this
	// project's runs are actually recorded and gated under (internal/
	// release.ProjectFromWorkspace's derivation from ProjectPath). Exposed
	// so `factoryd kill-switch -project` has a real, discoverable id to
	// pass rather than an operator retyping one from memory that may not
	// match what any run actually derived (found via the 2026-09-05 Opus
	// review, S4 -- the kill switch previously bound to runs through an
	// undiscoverable derived string with no lookup and no error on a
	// mismatch).
	Project       string `json:"project"`
	WorkspacePath string `json:"workspace_path"`
	SpecPath      string `json:"spec_path"`
	// Repository carries the most recent run's r.Repository forward so a
	// repeat run against this project can prefill it too — found via
	// review: without this, the console's project-selection screen only
	// ever prefilled Workspace/Spec, still leaving Repository (required by
	// NewRunScreen whenever a run needs the shared per-repository task
	// queue) to be retyped every time despite this screen's whole purpose
	// being to avoid exactly that. Empty when the most recent run never
	// used one.
	Repository string `json:"repository,omitempty"`
	RunCount   int    `json:"run_count"`
	LastRunAt  string `json:"last_run_at"`
}

// listProjects groups every durable run record by ProjectPath and returns
// one summary per distinct project, most-recently-used first, so the
// console can offer "run this again against the same project" without a
// human retyping workspace/spec paths already on file from a prior run.
// Read-only; gated by authorizeRead like every other read route this
// Server exposes -- see WithReadToken's doc comment for its default-open,
// opt-in-to-lock-down semantics.
func (s *Server) listProjects(w http.ResponseWriter, r *http.Request) {
	authorized := s.authorizeRead(r)
	if !authorized {
		writeError(w, http.StatusForbidden, "read endpoint is not authorized")
		return
	}
	entries, err := os.ReadDir(filepath.Join(s.dataDir, "runs"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusInternalServerError, "read runs")
		return
	}

	// aggregate pairs a ProjectSummary with the run count already folded
	// into it and is otherwise just that summary — kept as its own type in
	// case a future tie-breaker needs a field beyond what's in the public
	// response shape.
	type aggregate struct {
		summary *ProjectSummary
	}
	byPath := make(map[string]*aggregate)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		r, err := run.Load(s.dataDir, entry.Name())
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "load run")
			return
		}
		if r.ProjectPath == "" {
			// A run started before ProjectPath was populated, or one
			// missing it for some other reason, has nothing to group by —
			// omitted rather than surfaced as a nameless project.
			continue
		}
		agg, ok := byPath[r.ProjectPath]
		if !ok {
			// r.Project falls back to a fresh derivation for a run
			// recorded before that field existed, rather than leaving
			// this summary's own Project empty for every run predating
			// the fix that added it.
			agg = &aggregate{summary: &ProjectSummary{ProjectPath: r.ProjectPath, Project: release.ProjectOf(r)}}
			byPath[r.ProjectPath] = agg
		}
		agg.summary.RunCount++
		// createdAtAfter alone is enough now that CreatedAt is generated
		// with RFC3339Nano precision (see cmd/factoryd's own doc comment
		// on that): two runs against the same project can no longer
		// collide at whole-second resolution the way they used to. Found
		// via review: an earlier version broke same-second ties using
		// run.json's own filesystem mtime, but that's mutable — it
		// changes on every subsequent state save, not just at creation —
		// so a long-lived run updated later would incorrectly outrank a
		// genuinely newer one instead of reflecting real creation order. A
		// handful of pre-existing runs recorded before this precision
		// change could still collide at whole-second resolution; ties
		// there simply keep whichever this loop saw first, the same
		// behavior every caller of this endpoint already had before this
		// feature existed.
		if !ok || createdAtAfter(r.CreatedAt, agg.summary.LastRunAt) {
			// r.ProjectPath, not r.WorkspacePath (found via a real
			// GitHub-Codex-App review comment, 2026-08-29): for an
			// isolated run, WorkspacePath is that
			// one run's own private worktree, not a reusable location —
			// quick-filling a *new* run's -workspace with it points at a
			// directory that belongs to a specific past run and may
			// already have been deleted by internal/release.Rollback.
			// ProjectPath is the stable shared checkout every run against
			// this project, isolated or not, actually starts from.
			agg.summary.WorkspacePath = r.ProjectPath
			agg.summary.Project = release.ProjectOf(r)
			agg.summary.SpecPath = r.SpecPath
			agg.summary.Repository = r.Repository
			agg.summary.LastRunAt = r.CreatedAt
		}
	}

	aggregates := make([]*aggregate, 0, len(byPath))
	for _, agg := range byPath {
		aggregates = append(aggregates, agg)
	}
	sort.Slice(aggregates, func(i, j int) bool {
		return createdAtAfter(aggregates[i].summary.LastRunAt, aggregates[j].summary.LastRunAt)
	})
	projects := make([]*ProjectSummary, len(aggregates))
	for i, agg := range aggregates {
		projects[i] = agg.summary
	}
	writeJSON(w, http.StatusOK, projects)
}

func (s *Server) getRun(w http.ResponseWriter, r *http.Request) {
	authorized := s.authorizeRead(r)
	if !authorized {
		writeError(w, http.StatusForbidden, "read endpoint is not authorized")
		return
	}
	loaded, err := s.loadRun(r.PathValue("id"))
	if errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusNotFound, "run not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "load run")
		return
	}
	view := s.runViewFor(loaded)
	// Only a single run's view carries its compose phases: GET /runs
	// would otherwise read every run's reports on every poll.
	view.ComposePhases = readComposePhases(s.dataDir, loaded.ID)
	writeJSON(w, http.StatusOK, view)
}

// getRunDiff returns the full unified diff between a run's BaseSHA and the
// workspace state at the moment its evidence was collected — the console's
// own "diff" screen calling this needs to show an operator *what changed*,
// not just the file-count/insertion/deletion evidence `GetRun`'s own
// `diff_stat` already carries.
//
// Served from a durable file on disk (`run.DiffPath`), not recomputed from
// the live git workspace on every request, and not carried on the run
// record itself — found via review, this endpoint used to shell out to
// `runner.GitDiffIncludingWorktree` against the workspace path at request
// time, which was wrong two ways: (1) a later run reusing the same
// workspace, or any other later edit, would make an older run's diff
// silently show that later, unrelated content instead of its own — there
// is no revision to diff against once a run finishes, since ResultSHA can
// equal BaseSHA for a quarantined run and the worktree itself keeps
// moving; and (2) an unauthenticated read route executing `git diff`
// against a workspace on demand let that workspace's own
// `.gitattributes`/`.git/config` (an external diff driver, a textconv
// filter) run arbitrary commands as this process, and let concurrent
// requests spawn unbounded git subprocesses. Snapshotting the diff once,
// during the trusted run itself (see `CollectEvidenceActivity` and
// cmd/factoryd's apply path), straight to a file, and serving
// that fixed, pre-bounded file here closes all three: the content is
// pinned to the run that produced it, no subprocess runs on this request
// path at all, and its size is capped by `runner.MaxStoredDiffBytes` at
// collection time — a second review round additionally found that storing
// the diff text as a field on `run.Run` itself bloated every ordinary
// `GET /runs`/`GET /runs/{id}`/SSE response with a potentially multi-
// megabyte blob no other caller needed, and risked exceeding Temporal's
// own Activity-result payload limit for a Temporal-routed run; the file is
// now the sole source of truth, with only a small `DiffAvailable`/
// `DiffTruncated` flag pair on the run record itself (see `GetRun`'s
// response).
func (s *Server) getRunDiff(w http.ResponseWriter, r *http.Request) {
	authorized := s.authorizeRead(r)
	if !authorized {
		writeError(w, http.StatusForbidden, "read endpoint is not authorized")
		return
	}
	loaded, err := s.loadRun(r.PathValue("id"))
	if errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusNotFound, "run not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "load run")
		return
	}
	if loaded.ResultSHA == "" {
		// A run that hasn't reached a result yet (still slice_running/
		// verifying, or halted before ever producing one) has nothing to
		// diff — a caller-input/timing problem, not a server fault.
		writeError(w, http.StatusConflict, "run has no result yet")
		return
	}
	if !loaded.DiffAvailable {
		// Evidence collection ran (ResultSHA is set) but either predates
		// this field or itself failed to compute the diff (a warning-and-
		// continue path, same as DiffStat).
		writeError(w, http.StatusNotFound, "diff was not collected for this run")
		return
	}
	diff, err := os.ReadFile(run.DiffPath(s.dataDir, loaded.ID))
	if err != nil {
		// DiffAvailable said this file should exist; most likely the data
		// directory was pruned or moved out from under a still-referenced
		// run record — an operational problem, not a caller error.
		writeError(w, http.StatusInternalServerError, "read diff")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"diff": string(diff), "truncated": loaded.DiffTruncated})
}

// ReleaseView is GET /runs/{id}/release's response: the durable release
// decision recorded for one run, plus the current state and attributable
// transition history of the project kill switch that decision was evaluated
// against. Both are read-only — nothing in this codebase acts on an allowed
// decision, and this route deliberately exposes no way to change the kill
// switch (see getRunRelease's doc comment).
type ReleaseView struct {
	RunID   string `json:"run_id"`
	Project string `json:"project"`
	// Decision is null for a run that has none recorded — nothing records
	// one until a run is accepted. A client must render that as "no
	// decision", never as an allowed one.
	Decision *release.Decision `json:"decision"`
	// RecordingFailure is set instead of Decision when RecordDecision
	// evaluated this run but could not durably save the decision (e.g. an
	// unreadable/corrupted kill-switch.json) -- see
	// release.DecisionRecordingFailedError's own doc comment. A client
	// must render this distinctly from Decision == nil ("no decision"):
	// this run WAS evaluated.
	RecordingFailure *release.DecisionFailure  `json:"recording_failure,omitempty"`
	KillSwitch       *release.KillSwitchRecord `json:"kill_switch"`
}

// getRunRelease serves the console's release screen: what the factory
// decided about releasing one run, and why. Read-only in the strongest
// sense — it evaluates nothing (the decision it returns was recorded when
// the run was accepted, by whichever execution or override path accepted
// it), performs no merge/push/deploy, and offers no kill-switch mutation.
// Engaging and disengaging the kill switch stays CLI-only on purpose, so
// reaching for it never depends on a healthy `factoryd serve` (see
// CLAIMS.md); surfacing its state here does not weaken that.
//
// Authenticated with the same start token the daemon lifecycle routes use,
// and fails closed when no token is configured: unlike a run record, the
// kill-switch history carries operator attribution (who halted releases for
// this project, and the reason they gave), which is exactly the audit
// evidence that should not be readable by any client that can reach this
// Server's listen address.
func (s *Server) getRunRelease(w http.ResponseWriter, r *http.Request) {
	authorized := s.authorizeStart(r)
	if !authorized {
		writeError(w, http.StatusForbidden, "release endpoint is not authorized")
		return
	}
	loaded, err := s.loadRun(r.PathValue("id"))
	if errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusNotFound, "run not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "load run")
		return
	}
	// The same id every writer of a decision uses — see release.ProjectOf,
	// and cmd/factoryd's own recordReleaseDecision. Never derived from
	// WorkspacePath: an isolated run's WorkspacePath is a per-run
	// worktree whose parent is "workspaces", not the project.
	project := release.ProjectOf(loaded)
	// A run recorded before ProjectPath was populated (or with a path whose
	// parent is the filesystem root) derives no usable project id, so
	// neither durable record can be located at all — a property of that run,
	// not a server fault, and the same 409 treatment getRunDiff gives a run
	// with no result yet. validRunID is the single-path-component rule
	// internal/release itself enforces on a project, applied here so the
	// distinction reaches the client as 409 rather than a bare 500.
	if !validRunID(project) {
		writeError(w, http.StatusConflict, "run has no releasable project")
		return
	}
	decision, err := release.LoadDecision(s.dataDir, project, loaded.ID)
	var recordingFailure *release.DecisionFailure
	if err != nil {
		var failedErr *release.DecisionRecordingFailedError
		if errors.As(err, &failedErr) {
			recordingFailure = &failedErr.Failure
		} else {
			writeError(w, http.StatusInternalServerError, "load release decision")
			return
		}
	}
	killSwitch, err := release.LoadKillSwitch(s.dataDir, project)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "load kill switch")
		return
	}
	writeJSON(w, http.StatusOK, ReleaseView{RunID: loaded.ID, Project: project, Decision: decision, RecordingFailure: recordingFailure, KillSwitch: killSwitch})
}

// ProjectReleaseView is GET /projects/{project}/release's response: the
// current state and attributable transition history of one project's kill
// switch, addressed directly by project id rather than derived from a run.
// getRunRelease alone left a real gap (Phase 1, CLAIMS.md): a project whose
// kill switch is engaged but which has no runs yet — or none recently —
// had no console surface at all, because GET /projects only lists projects
// aggregated out of existing run records. This route needs no run to
// exist; the kill switch is keyed by project id on disk independently of
// any run (see release.killSwitchPath).
type ProjectReleaseView struct {
	Project    string                    `json:"project"`
	KillSwitch *release.KillSwitchRecord `json:"kill_switch"`
}

// getProjectRelease serves the console's project-level release surface: the
// kill switch's state and history for a project id an operator already
// knows (the same id `factoryd kill-switch -project` takes), independent of
// whether that project has any runs. Read-only, same authorization and
// rationale as getRunRelease — the kill-switch history carries operator
// attribution, and engaging/disengaging stays CLI-only on purpose.
func (s *Server) getProjectRelease(w http.ResponseWriter, r *http.Request) {
	authorized := s.authorizeStart(r)
	if !authorized {
		writeError(w, http.StatusForbidden, "release endpoint is not authorized")
		return
	}
	project := r.PathValue("project")
	if !validRunID(project) {
		writeError(w, http.StatusBadRequest, "project must be a single path component")
		return
	}
	killSwitch, err := release.LoadKillSwitch(s.dataDir, project)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "load kill switch")
		return
	}
	writeJSON(w, http.StatusOK, ProjectReleaseView{Project: project, KillSwitch: killSwitch})
}

// getProjectStats serves the console's per-team acceptance-rate screen --
// see ProjectStats' own doc comment. Gated identically to getProjectRelease
// immediately above (authorizeStart).
func (s *Server) getProjectStats(w http.ResponseWriter, r *http.Request) {
	authorized := s.authorizeStart(r)
	if !authorized {
		writeError(w, http.StatusForbidden, "stats endpoint is not authorized")
		return
	}
	if s.projectStatsProvider == nil {
		writeError(w, http.StatusNotFound, "stats endpoint is not configured")
		return
	}
	project := r.PathValue("project")
	if !validRunID(project) {
		writeError(w, http.StatusBadRequest, "project must be a single path component")
		return
	}
	stats, err := s.projectStatsProvider(r.Context(), project)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "compute project stats")
		return
	}
	writeJSON(w, http.StatusOK, stats)
}

// writeSSEHeaders sets the two headers common to every Server-Sent-Events
// handler here (streamRunEvents, streamRunProgress, streamRequestEvents).
func writeSSEHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
}

func (s *Server) streamRunEvents(w http.ResponseWriter, r *http.Request) {
	authorized := s.authorizeRead(r)
	if !authorized {
		writeError(w, http.StatusForbidden, "read endpoint is not authorized")
		return
	}
	loaded, err := s.loadRun(r.PathValue("id"))
	if errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusNotFound, "run not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "load run")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	writeSSEHeaders(w)

	// s.runViewFor, not the bare *run.Run: found via review, sending the
	// unwrapped record here dropped Stalled/LastProgressAt/CurrentStage
	// from every event after the first (getRun's own response includes
	// them), so a console tab watching this stream would see its stalled
	// chip silently clear on the very next state event and never recover
	// -- state/updated_at don't change from mere silence, so nothing ever
	// re-sent the field. lastStalled is tracked below for the same reason:
	// a run going stalled, or coming back from it, changes neither.
	view := s.runViewFor(loaded)
	view.ComposePhases = readComposePhases(s.dataDir, loaded.ID)
	if err := writeStateEvent(w, flusher, view); err != nil || terminal(loaded) {
		return
	}
	lastState, lastUpdatedAt, lastStalled := loaded.State, loaded.UpdatedAt, view.Stalled
	ticker := time.NewTicker(s.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			loaded, err := s.loadRun(r.PathValue("id"))
			if err != nil {
				return
			}
			view := s.runViewFor(loaded)
			if loaded.State == lastState && loaded.UpdatedAt == lastUpdatedAt && view.Stalled == lastStalled {
				continue
			}
			view.ComposePhases = readComposePhases(s.dataDir, loaded.ID)
			if err := writeStateEvent(w, flusher, view); err != nil {
				return
			}
			lastState, lastUpdatedAt, lastStalled = loaded.State, loaded.UpdatedAt, view.Stalled
			if terminal(loaded) {
				return
			}
		}
	}
}

// streamRunProgress serves GET /runs/{id}/progress: the run progress feed
// described in progress-contract.md, as SSE "progress" events. Sends
// every line already in the run's progress.jsonl sidecar first, then
// follows the file (polling at s.pollInterval, the same as
// streamRunEvents/streamRunLog), ending once the run has reached a
// terminal state and the file has been drained -- same read
// authorization and 404 handling as streamRunEvents. A run with no
// progress.jsonl yet (e.g. still queued) is not an error: progress.
// ReadFrom on a missing file simply returns no lines, and this keeps
// polling for it to appear.
func (s *Server) streamRunProgress(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeRead(r) {
		writeError(w, http.StatusForbidden, "read endpoint is not authorized")
		return
	}
	id := r.PathValue("id")
	loaded, err := s.loadRun(id)
	if errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusNotFound, "run not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "load run")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	writeSSEHeaders(w)

	path := progress.Path(s.dataDir, id)
	offset, finished, err := drainProgress(w, flusher, path, 0)
	if err != nil {
		return
	}
	ticker := time.NewTicker(s.pollInterval)
	defer ticker.Stop()
	if terminal(loaded) {
		if !finished {
			s.drainProgressOnceMore(r, w, flusher, ticker, path, offset)
		}
		return
	}

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			offset, finished, err = drainProgress(w, flusher, path, offset)
			if err != nil {
				return
			}
			if finished {
				return
			}
			loaded, err := s.loadRun(id)
			if err != nil {
				return
			}
			if terminal(loaded) {
				s.drainProgressOnceMore(r, w, flusher, ticker, path, offset)
				return
			}
		}
	}
}

// drainProgress forwards every complete progress line from offset on as an
// SSE event and reports the new offset and whether the run's own
// "finished" line was among them.
func drainProgress(w http.ResponseWriter, flusher http.Flusher, path string, offset int64) (newOffset int64, finished bool, err error) {
	lines, newOffset, err := progress.ReadFrom(path, offset)
	if err != nil {
		return offset, false, err
	}
	for _, line := range lines {
		if err := writeProgressEvent(w, flusher, line); err != nil {
			return newOffset, false, err
		}
		var e progress.Event
		if json.Unmarshal([]byte(line), &e) == nil && e.Source == "factory" && e.Stage == "finished" {
			finished = true
		}
	}
	return newOffset, finished, nil
}

// drainProgressOnceMore waits one poll tick and drains again: the
// "finished" line is appended after run.json's own terminal save
// (cmd/factoryd's save()), so a record read as terminal may be a few
// milliseconds ahead of the file, whether that read happened on connect or
// mid-follow. One more drain closes that gap rather than ending the stream
// without its last line.
func (s *Server) drainProgressOnceMore(r *http.Request, w http.ResponseWriter, flusher http.Flusher, ticker *time.Ticker, path string, offset int64) {
	select {
	case <-r.Context().Done():
	case <-ticker.C:
		_, _, _ = drainProgress(w, flusher, path, offset)
	}
}

// writeProgressEvent writes one already-serialized progress.jsonl line as
// an SSE "progress" event and flushes immediately -- the line is written
// verbatim (not re-marshaled) since progress.ReadFrom already returns raw
// JSON text.
func writeProgressEvent(w http.ResponseWriter, flusher http.Flusher, rawJSON string) error {
	if _, err := fmt.Fprintf(w, "event: progress\ndata: %s\n\n", rawJSON); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}

// logPathForRun resolves the build-loop log file to stream for r: the
// most recent "build"-kind Attempt's own recorded LogPath when one
// exists (covers a retried build, whose second attempt writes a
// differently-named file -- see
// cmd/factoryd/run_ticket.go and internal/workflow/activities.go's own
// build_app.log/build_app.attemptN.log naming), falling back to the
// conventional first-attempt path while the attempt is still running and
// has not yet been recorded into Run.Attempts -- the case 3.2 exists for,
// tailing an in-progress run.
//
// The direct (non-Temporal) path (cmd/factoryd/run_ticket.go) writes
// that first-attempt file as the plain "build_app.log". The Temporal
// workflow path (internal/workflow/activity_records.go's
// activityExecutionLogPathForExecution) instead names it
// "<workflowID>-<runID>-<activityID>-build_app.log" inside the same
// directory -- a name this process cannot reconstruct without the live
// activity's own identifiers, which aren't available here. Found in
// review: without accounting for this, an in-progress Temporal build's
// log was never resolved, defeating live-tailing for exactly the
// in-progress case this endpoint exists for. Globbing for the
// "*-build_app.log" suffix recovers it -- at most one build activity is
// ever in flight for a given run at a time, so at most one match exists.
//
// Known gap: a run's SECOND build attempt, before it has itself
// completed, is not resolved to its own (still being written) log file
// by this fallback -- streamRunLog would keep serving the first
// attempt's already-complete log until the retry finishes and updates
// Run.Attempts. Accepted for now: retries are the failure path, not the
// common one this live-tail feature targets.
func logPathForRun(dataDir string, r *run.Run) string {
	for i := len(r.Attempts) - 1; i >= 0; i-- {
		if r.Attempts[i].Kind == "build" && r.Attempts[i].LogPath != "" {
			return r.Attempts[i].LogPath
		}
	}
	dir := run.Dir(dataDir, r.ID)
	if matches, err := filepath.Glob(filepath.Join(dir, "*-build_app.log")); err == nil && len(matches) > 0 {
		sort.Strings(matches)
		return matches[len(matches)-1]
	}
	return filepath.Join(dir, "build_app.log")
}

// withinEvidenceDir reports whether path resolves to a location inside
// this run's own evidence directory (run.Dir(dataDir, id)) -- the
// boundary streamRunLog must never cross. Required because logPathForRun
// can return a value taken from Run.Attempts[i].LogPath, durable data
// this process itself wrote but did not construct fresh from a
// known-safe id at the point of use, so this check treats it with the
// same care as any other untrusted-input-to-filesystem-path code in this
// repo (see safety-contract.md's Filesystem trust-boundary row: only
// factoryd writes evidence, and the API must never read outside it).
func withinEvidenceDir(dataDir, id, path string) bool {
	dir, err := filepath.Abs(run.Dir(dataDir, id))
	if err != nil {
		return false
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(dir, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	// A lexical containment check alone is not enough: path (or an
	// ancestor of it, including the evidence dir itself) may be a symlink
	// planted by a corrupted or hand-edited run record, pointing outside
	// the evidence dir. Resolve symlinks on both sides before the final
	// comparison so that case can't slip past this check. EvalSymlinks
	// requires the target to exist; a log file that hasn't been created
	// yet is not a containment violation, so only re-validate once it
	// does.
	resolvedDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return false
	}
	resolvedAbs, err := filepath.EvalSymlinks(abs)
	if err != nil {
		// Path doesn't exist yet (e.g. log not written for a run that
		// hasn't started building). The lexical check above already
		// passed; nothing to resolve.
		return true
	}
	resolvedRel, err := filepath.Rel(resolvedDir, resolvedAbs)
	if err != nil {
		return false
	}
	return resolvedRel != ".." && !strings.HasPrefix(resolvedRel, ".."+string(filepath.Separator))
}

// streamRunLog serves GET /runs/{id}/log: the build-loop log for that
// run, read only from within its own evidence directory. The log is
// worker-authored content -- untrusted from the API's own trust
// perspective (safety-contract.md's Acceptance/
// Filesystem trust-boundary rows) -- so it is written back to the client
// as plain text only, never interpreted, and its path is validated via
// withinEvidenceDir before every open, regardless of where it was
// resolved from.
//
// Without ?follow=1, returns the log's current contents and closes.
// With ?follow=1, keeps the connection open and writes newly appended
// bytes as the file grows -- the same poll-and-diff shape
// streamRunEvents uses, applied to file size instead of run state --
// until the run reaches a terminal state with nothing left to flush, or
// the client disconnects.
//
// Read-token gated, like GET /runs/{id}/events.
func (s *Server) streamRunLog(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeRead(r) {
		writeError(w, http.StatusForbidden, "log endpoint is not authorized")
		return
	}
	id := r.PathValue("id")
	loaded, err := s.loadRun(id)
	if errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusNotFound, "run not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "load run")
		return
	}

	logPath := logPathForRun(s.dataDir, loaded)
	if !withinEvidenceDir(s.dataDir, loaded.ID, logPath) {
		// Never expected in practice -- logPathForRun only ever returns
		// a path it built itself or one this same process previously
		// recorded into this run's own evidence -- refusing rather than
		// trusting it anyway is defense in depth against a corrupted or
		// hand-edited run.json, not a path this code should ever
		// legitimately take.
		writeError(w, http.StatusInternalServerError, "log path outside evidence directory")
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")

	f, err := os.Open(logPath)
	if errors.Is(err, os.ErrNotExist) {
		if r.URL.Query().Get("follow") != "1" {
			// Not written yet -- e.g. a queued run whose build attempt
			// hasn't started. Empty body, not an error, matching
			// readRequestFileBestEffort's "missing file is an expected
			// state" convention elsewhere in this file.
			w.WriteHeader(http.StatusOK)
			return
		}
		// Found in review: under follow=1, returning here instead of
		// waiting closed the stream immediately -- the console's
		// watchRunLog then reported "closed" for a run that had only
		// just started, before its build attempt had written its first
		// byte, and never picked the log up once it appeared. Poll for
		// the file to show up, the same way the loop below polls for it
		// to grow, giving up only once the run itself reaches a terminal
		// state without the file ever having been created.
		flusher, ok := w.(http.Flusher)
		if !ok {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusOK)
		flusher.Flush()
		ticker := time.NewTicker(s.pollInterval)
		defer ticker.Stop()
		for f == nil {
			select {
			case <-r.Context().Done():
				return
			case <-ticker.C:
				loaded, loadErr := s.loadRun(id)
				if loadErr != nil {
					return
				}
				// Re-resolve, not just re-open the same logPath computed
				// before this loop started. Found in review: the
				// Temporal path's log glob (logPathForRun) can find
				// nothing on the very first tick (the activity hasn't
				// created its prefixed file yet), which freezes logPath
				// at the direct-run fallback name that Temporal never
				// writes -- retrying that same wrong path forever. Every
				// tick must look again for whichever name actually shows
				// up, exactly as the non-waiting path above already does
				// once.
				logPath = logPathForRun(s.dataDir, loaded)
				if !withinEvidenceDir(s.dataDir, loaded.ID, logPath) {
					return
				}
				f, err = os.Open(logPath)
				if err != nil && !errors.Is(err, os.ErrNotExist) {
					return
				}
				if f != nil {
					break
				}
				if terminal(loaded) {
					// The run finished without ever writing this log
					// (e.g. it failed before the build attempt started).
					return
				}
			}
		}
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, "open log")
		return
	}
	defer f.Close()

	if _, err := io.Copy(w, f); err != nil {
		return
	}
	if r.URL.Query().Get("follow") != "1" {
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		return
	}
	flusher.Flush()

	ticker := time.NewTicker(s.pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			n, err := io.Copy(w, f)
			if err != nil {
				return
			}
			if n > 0 {
				flusher.Flush()
			}
			loaded, err := s.loadRun(id)
			if err != nil {
				return
			}
			if terminal(loaded) && n == 0 {
				return
			}
		}
	}
}

// overrideRequest is the JSON body POST /runs/{id}/override expects.
// Mirrors cmd/factoryd's own "override" CLI subcommand (run.ApplyOverride's
// existing caller) exactly, rather than inventing a second validation path
// for the same operation — see run.ApplyOverride's own doc comment for the
// invariants this endpoint doesn't re-decide (quarantined-only, both by and
// reason required).
type overrideRequest struct {
	By     string `json:"by"`
	Reason string `json:"reason"`
	State  string `json:"state"`
}

// overrideRun lets an operator move a quarantined run to a terminal state
// (accepted or halted) over HTTP — the execution/override endpoint the
// plan doc's Phase 1 status previously called out as missing, so a future
// override screen has something to call instead of shelling out to
// `factoryd override` on the machine running the API server.
func (s *Server) overrideRun(w http.ResponseWriter, r *http.Request) {
	authorized := s.authorizeOverride(r)
	if !authorized {
		// Same response whether the token is simply unconfigured (the
		// endpoint is disabled) or a real token was presented and didn't
		// match — not distinguishing the two here avoids confirming to an
		// unauthorized caller that overrides are even possible against
		// this deployment.
		writeError(w, http.StatusForbidden, "override endpoint is not authorized")
		return
	}

	var req overrideRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "decode request body")
		return
	}

	var newState run.State
	switch req.State {
	case string(run.StateAccepted):
		newState = run.StateAccepted
	case string(run.StateHalted):
		newState = run.StateHalted
	default:
		writeError(w, http.StatusBadRequest, fmt.Sprintf("state must be %q or %q", run.StateAccepted, run.StateHalted))
		return
	}

	id := r.PathValue("id")
	// Validated before run.WithLock ever touches the filesystem, not
	// inside the closure below (where loadRun already re-validates it,
	// too late) — found via review: WithLock creates the run's directory
	// and a .lock file inside it (MkdirAll + O_CREATE) before loadRun ever
	// gets a chance to reject a malformed id, so an unvalidated id let an
	// authenticated but otherwise arbitrary caller create directories and
	// lock files anywhere reachable via a traversal sequence, not just
	// beneath the runs directory.
	if !validRunID(id) {
		writeError(w, http.StatusNotFound, "run not found")
		return
	}

	// Serializes the whole load/apply/save sequence against every other
	// caller that can mutate this exact run's durable record — not just
	// other requests to this Server, but a completely separate process
	// (`cmd/factoryd`'s own `override` CLI subcommand shares this same
	// lock). See run.WithLock's own doc comment for why an in-process
	// mutex alone (an earlier version of this fix used one) cannot
	// exclude a different process at all.
	if err := run.WithLock(s.dataDir, id, func() error {
		loaded, err := s.loadRun(id)
		if errors.Is(err, os.ErrNotExist) {
			writeError(w, http.StatusNotFound, "run not found")
			return nil
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "load run")
			return nil
		}

		if err := loaded.ApplyOverride(req.By, req.Reason, newState, func() string {
			return time.Now().Format(time.RFC3339)
		}); err != nil {
			// ApplyOverride's own errors (not quarantined, missing
			// by/reason) are all caller-input problems, never an internal
			// failure — StatusBadRequest, not 500, mirroring how they're
			// surfaced to a human operator via the CLI's own error message
			// today.
			writeError(w, http.StatusBadRequest, err.Error())
			return nil
		}
		// Matches cmd/factoryd's own save() helper: notify.PrepareHalt/
		// DispatchDiscord give an override-to-halted through this HTTP
		// route the same halt alert save() already gives every CLI- or
		// workflow-initiated one (found via a real GitHub Codex App
		// review of this PR: this route called run.Run.Save directly,
		// bypassing that hook entirely -- internal/api cannot import
		// cmd/factoryd, so PrepareHalt/DispatchDiscord live in
		// internal/notify instead, the one package both already depend
		// on for exactly this). Split the same way save() does:
		// PrepareHalt (local-only) before the save, DispatchDiscord
		// (network-bound) only after it succeeds. UpdatedAt is stamped by
		// Persist below, not here.
		// Mirrors cmd/factoryd's own save() helper's identical guard
		// (sandbox_exec.go): an override through this HTTP route is a
		// second place a quarantined/halted run's durable record is
		// saved, and until this fix it never got a Triage sentence at
		// all -- internal/api cannot import cmd/factoryd (see this
		// route's own doc comment for why), so it calls the relocated
		// internal/triage package directly instead. r.Triage == "" guards
		// against overwriting an already-set sentence, same as save()'s.
		if (loaded.State == run.StateQuarantined || loaded.State == run.StateHalted) && loaded.Triage == "" {
			loaded.Triage = triage.Run(loaded, s.dataDir)
		}
		// The run has just left the state its handoff describes (or become
		// stopped): rewrite or remove it before the record is written, as
		// cmd/factoryd's save does.
		if err := handoff.Sync(loaded, s.dataDir); err != nil {
			log.Printf("run %s: could not bring its handoff up to date after an override: %v", id, err)
		}
		haltNotification, dispatchHalt := notify.PrepareHalt(loaded, s.dataDir)
		// Persist is the one funnel every run-record write goes through
		// (internal/run/run.go): it stamps UpdatedAt/FactorydVersion,
		// writes run.json, then appends the durable event log entry --
		// replacing this route's former separate Save+RecordEvent pair
		// (found via review: this route called run.Run.Save directly,
		// bypassing RecordEvent entirely, so an override never appended
		// to events.db).
		if err := loaded.Persist(s.dataDir); err != nil {
			if dispatchHalt {
				// See cmd/factoryd's own save() helper for why this must
				// be rolled back rather than left staged on a save
				// failure.
				loaded.Notifications = loaded.Notifications[:len(loaded.Notifications)-1]
			}
			writeError(w, http.StatusInternalServerError, "save overridden run")
			return nil
		}
		// Mirrors cmd/factoryd's own "override" CLI subcommand (found via
		// codex review round 3, 2026-08-28): the run-time rollback defer
		// deliberately leaves a quarantined isolated run's worktree in
		// place in case an operator later promotes it to accepted, but an
		// override *to* halted through this HTTP route is that decision
		// resolved the other way, and nothing else ever revisits the
		// worktree afterward. loaded.Branch is only ever set for a run
		// that ran isolated, so this is a no-op for every
		// other override.
		if newState == run.StateHalted && loaded.Branch != "" {
			// Retained before the rollback below removes the one place it
			// exists, not left unretained (found via the same review
			// round, against the Temporal-path equivalent of this same
			// gap): a run overridden to halted may have a build_app.py
			// report already sitting in its isolated worktree. Best-effort
			// and non-fatal: a run whose build never got far enough to
			// write one simply has nothing here to retain.
			s.retainBuildArtifacts(id, loaded.WorkspacePath)
			if err := release.Rollback(loaded.ProjectPath, loaded.WorkspacePath, loaded.Branch, loaded.OnBranch != ""); err != nil {
				log.Printf("run %s: rollback of isolated workspace after override failed: %v", id, err)
			} else {
				// The override is the human decision that ends a kept
				// worktree's wait; the worktree is gone, so the flag is too.
				if loaded.KeptForResume {
					loaded.KeptForResume = false
					if err := loaded.Persist(s.dataDir); err != nil {
						log.Printf("run %s: clear kept-for-resume after override rollback: %v", id, err)
					}
				}
			}
		}
		// This runs inside run.WithLock's own callback, so calling a
		// blocking dispatch here directly would hold this run's
		// cross-process lock (and this HTTP response) for up to 15s
		// across its three serial 5s-timeout channels -- the same class
		// of bug as PR #86's WithLock-reentrancy deadlock, just latency
		// instead of a full deadlock. DispatchExternal itself dispatches
		// on its own goroutine and returns immediately, so this handler
		// is never blocked on it.
		if dispatchHalt {
			notify.DispatchExternal(haltNotification)
		}
		// Found via Codex review of PR #45: an override to accepted is
		// exactly the AllowOverrides=true case internal/release.MergePolicy
		// exists to evaluate, but this endpoint never called it -- an
		// overridden run had no release-decision file at all. Mirrors
		// cmd/factoryd's own "override" CLI subcommand; best-effort and
		// logged, same as every other supplementary-evidence write here.
		if newState == run.StateAccepted {
			if _, err := release.RecordDecision(s.dataDir, release.ProjectOf(loaded), *loaded, s.releasePolicy); err != nil {
				log.Printf("run %s: warning: could not record release decision for override: %v", id, err)
			}
		}
		// The same view GET /runs/{id} returns, compose phases included:
		// the console replaces its run with this response, and a bare
		// record would drop its Compose services section until reload.
		view := s.runViewFor(loaded)
		view.ComposePhases = readComposePhases(s.dataDir, loaded.ID)
		writeJSON(w, http.StatusOK, view)
		return nil
	}); err != nil {
		// Only WithLock's own infrastructure failures (couldn't create
		// the lock directory/file, couldn't flock it) reach here — every
		// outcome of the load/apply/save sequence itself already wrote
		// its own response and returned nil from inside the closure
		// above.
		writeError(w, http.StatusInternalServerError, "acquire override lock")
	}
}

// approveRequestBody is POST /requests/{id}/approve's JSON request body.
// Body is optional (an empty body decodes to the zero value, see
// approveRequest) so an older console build that sends no body at all
// keeps working exactly as before.
type approveRequestBody struct {
	// By optionally names the operator approving, echoed onto
	// Request.ApprovedBy. Falls back to requestAPIPrincipal when absent,
	// matching rejectRequestBody's own By field (symmetric identity on
	// both routes).
	By string `json:"by,omitempty"`
	// ExpectedSHA256 optionally names, per relPath ("spec.md" or
	// "tickets/NNN.spec.md"), the SHA-256 the caller's own last fetch of
	// that file's content hashed to, binding the approval to the
	// artifact actually shown. request.Approve refuses with a 400 naming
	// request.ErrApprovalStale when a covered relPath's
	// current hash disagrees, rather than approving content the operator
	// never actually saw. Omitted (the default) keeps the prior,
	// unconditional behavior -- an older console build, or any other
	// caller with nothing to compare against.
	ExpectedSHA256 map[string]string `json:"expected_sha256,omitempty"`
}

// rejectRequestBody is POST /requests/{id}/reject's JSON request body.
type rejectRequestBody struct {
	Reason string `json:"reason"`
	// By optionally names the operator rejecting, recorded on the
	// rejection note and on Request.Rejections. Falls back to
	// requestAPIPrincipal when absent, exactly like approveRequestBody's
	// own By field.
	By string `json:"by,omitempty"`
	// To optionally routes this call to request.SendBack instead of
	// request.Reject: "plan" or "spec", meaningful only for a
	// request currently quarantined or halted -- see rejectRequest's own
	// doc comment. Omitted (the default) keeps the prior, Reject-only
	// behavior for every existing caller.
	To string `json:"to,omitempty"`
	// Anchors optionally ties notes to places in the reviewed files
	// (request.RejectionAnchor). With at least one, Reason may be empty.
	// Refused together with To: a send-back has no document under review.
	Anchors []request.RejectionAnchor `json:"anchors,omitempty"`
}

// retryOrCancelRequestBody is POST /requests/{id}/retry and POST
// /requests/{id}/cancel's shared JSON request body -- the same "by"/
// "reason" shape rejectRequestBody uses. Reason is optional for retry
// (request.Retry takes none; kept only so a client that always sends
// one, mirroring reject, isn't refused with an unknown-field error) and
// is recorded verbatim on cancel's own History entry.
type retryOrCancelRequestBody struct {
	By     string `json:"by,omitempty"`
	Reason string `json:"reason,omitempty"`
	// From is retry's only: "scratch" rebuilds a ticket from the base
	// commit (request.RetryFromScratch); "" or "attempt" is the default.
	// Any other value is refused. Cancel ignores it.
	From string `json:"from,omitempty"`
}

// resumeRequestBody is POST /requests/{id}/resume's JSON body. From is
// request.ResumeRound (the default when omitted) or request.ResumeScratch.
type resumeRequestBody struct {
	From string `json:"from,omitempty"`
	By   string `json:"by,omitempty"`
}

// requestAPIPrincipal is "by" for every request-approval-state-changing
// route this Server exposes (POST /requests/{id}/approve,
// POST /requests/{id}/reject). Unlike the CLI, which resolves the real OS
// user (see cmd/factoryd's own currentOSUser), this Server's bearer-token
// authentication (see authorizeRequestWrite) carries no caller identity to
// resolve one from -- "api" names the actor precisely, an authenticated
// caller of this endpoint, without fabricating a more specific identity
// this Server has no way to actually verify.
const requestAPIPrincipal = "api"

// requestSummaryView is GET /requests's per-request JSON shape: the
// durable request.Request plus Title, derived at read time from
// request.md's first line (see request.Title) rather than persisted --
// the console request board needs a per-row title, and deriving it
// here keeps request.Request's own on-disk layout untouched.
type requestSummaryView struct {
	*request.Request
	Title       string      `json:"title"`
	CostSummary CostSummary `json:"cost_summary"`
	// WaitingOn is the id of the request the driver is advancing while
	// this one waits its turn (request.WaitingOn, the same function
	// `factoryd status` uses, so the console and CLI agree) (C5).
	WaitingOn string `json:"waiting_on,omitempty"`
	// ActiveJob is the request's running drafting job, read from its own
	// file (request.LoadActiveJob), never from request.json.
	ActiveJob *request.ActiveJob `json:"active_job,omitempty"`
	// NextAction: see requestDetailView.NextAction. Derived in memory from
	// the loaded request, so a list of N requests costs no extra reads.
	NextAction string `json:"next_action,omitempty"`
}

// CostSummary is requestSummaryView's own "cost_summary" field: a
// best-effort dollar rollup of a request's spec-drafting,
// plan-drafting, and per-ticket build costs.
// Lives here, not on request.Request, because computing Runs requires
// internal/run (each ticket's own Run.AgentEvidence), a package
// internal/request does not import.
type CostSummary struct {
	Spec float64 `json:"spec"`
	Plan float64 `json:"plan"`
	// Oracle is the oracle-drafting stage's own relay-measured cost
	// (request.OracleDraft.Spend), rolled in here for the same reason
	// Spec/Plan are: a request opted into -draft-oracles spends real
	// money on that stage too, and Total (and CostPerAcceptedTicketMicroUSD
	// below) must include it to be a true "total request spend". Unlike
	// Spec/Plan, this has no pre-existing self-reported-Usage fallback --
	// draft_acceptance_oracles.py's own evidence.json was never rolled up
	// here before Spend existed, so this field is simply 0 for a request
	// that never drafted oracles or whose draft ran with no relay.
	Oracle   float64 `json:"oracle,omitempty"`
	Runs     float64 `json:"runs"`
	Total    float64 `json:"total"`
	Currency string  `json:"currency"`
	// Complete is false whenever any ticket's run evidence could not be
	// read or carried no cost figure -- Total is then a lower bound, not
	// an exact figure, and the console must render it as "≥" rather than
	// "=".
	Complete bool `json:"complete"`
	// SubscriptionBilled is true when ANY run contributing to Runs (a
	// ticket's build, or a corrective PR-review round) was billed to a
	// ChatGPT/Copilot subscription (run.SubscriptionBilled) rather than a
	// metered API key. A truthful treatment for a total that can mix
	// subscription and metered runs: Runs (and so Total) is a real
	// relay-priced number either way, but at least part of it was never
	// actually charged to the operator in dollars, so the console must
	// label the whole figure rather than silently presenting it as a
	// real charge. Spec/Plan are
	// deliberately excluded from this: they come from SpecEvidence/
	// PlanEvidence's own agent-self-reported Usage["total_cost_usd"], not
	// the relay ledger's per-attempt RelayConsumedCostMicroUSD/
	// RelayCredentialMode this flag is derived from (see costFromUsage's
	// own doc comment) -- there is no credential-mode evidence for that
	// figure to check. The console renders this true with the softer
	// subscriptionCostAggregateSuffix wording ("includes
	// subscription-billed runs"), not cmd/factoryd's own single-run
	// subscriptionCostSuffix ("billed to your subscription") -- an
	// adversarial review of #18's fix found: Total mixes Spec/Plan (which
	// this flag can't speak to at all) and every contributing run, so
	// asserting the WHOLE figure was subscription-billed can be false.
	SubscriptionBilled bool `json:"subscription_billed,omitempty"`
	// Tokens counts what each source reports as processed tokens: pi's
	// usage totalTokens (input + output + cache read/write) for drafting
	// and the AgentEvidence fallback, and the relay's upstream-reported
	// input + output for relay-metered runs. OpenAI-style upstreams include
	// cached tokens in their input count, so the two agree there; the
	// Anthropic Messages API reports cache reads/writes separately, which
	// the relay does not add, so relay-metered runs on that route
	// undercount relative to drafting.
	//
	// Tokens is the total token count across every ByModel entry -- the
	// operator's requested "model id and tokens spent" headline figure.
	// The console renders this, never a dollar figure (the operator
	// explicitly does not want dollars in the console; the Spec/Plan/Runs/
	// Total dollar fields above stay on the API for any other caller, but
	// the console itself stops rendering them).
	Tokens int64 `json:"tokens"`
	// TokensComplete mirrors Complete's own semantics (see its doc
	// comment) but judged on token availability, not dollar availability:
	// false whenever any contributing evidence could not be read or
	// carried no usable token figure, so Tokens is a lower bound.
	TokensComplete bool `json:"tokens_complete"`
	// ByModel is the per-role/model token+cost breakdown, sorted by role
	// then model, keyed by the factory-authored role/model id evidence
	// records (never anything an agent self-reports) -- see ModelUsage's
	// own doc comment. Empty (not present in JSON) when nothing recorded a
	// model id at all yet.
	ByModel []ModelUsage `json:"by_model,omitempty"`
	// AcceptedTickets is how many of req.Tickets have a run that reached
	// run.StateAccepted -- the same gate-passing acceptance PR-review
	// hands off to a human for merge (SC-001: merge/deploy stay
	// permanently human actions, so "accepted" here never means "merged").
	AcceptedTickets int `json:"accepted_tickets,omitempty"`
	// CostPerAcceptedTicketMicroUSD is Total (converted to micro-USD),
	// including every drafting stage, every ticket's first build, and
	// every corrective round (conformity or PR-review) that ran --
	// divided by AcceptedTickets. 0 when AcceptedTickets is 0: an
	// undefined "cost per ticket" must never render as a misleadingly
	// real $0.00 or a divide-by-zero, so a caller checks AcceptedTickets
	// before treating this as meaningful.
	CostPerAcceptedTicketMicroUSD int64 `json:"cost_per_accepted_ticket_micro_usd,omitempty"`
}

// ModelUsage is one CostSummary.ByModel entry: a per-(role, model) token
// and relay-measured cost rollup. Model=="" means tokens whose
// contributing evidence recorded no model id at all (an older run, or a
// relay that doesn't pin one) -- still counted in Tokens. Role=="" means
// tokens/cost whose contributing evidence recorded no factory-authored
// role at all: run.Attempt.Role is empty for any attempt Kind other than
// "build"/"spec_conformity" (verify/full_suite_verify/reference_oracle,
// none of which ever call a model, so this case should never actually
// carry nonzero tokens/cost) and for an attempt recorded before Role
// existed -- a display layer must render this "unknown", never coerce it
// to "execution": an empty Role is not documented anywhere to mean
// execution, only spec_conformity's own "review" and build's own
// "execution" are ever actually written.
type ModelUsage struct {
	Role         string `json:"role,omitempty"`
	Model        string `json:"model"`
	Tokens       int64  `json:"tokens"`
	CostMicroUSD int64  `json:"cost_micro_usd,omitempty"`
}

// modelUsageKey is modelUsageAccumulator's own grouping key -- (role,
// model), not model alone, so a run whose attempts span both an
// "execution" and a "review" role (e.g. a build plus its own
// spec_conformity attempt) never blends their separate spends into one
// entry.
type modelUsageKey struct {
	Role  string
	Model string
}

// retainBuildArtifacts copies what a build left in workspace that a
// rollback would delete into the run's own directory: the agent's report
// and each failed round's saved output. Both are byte copies of
// agent-written files (evidence.RetainFile's rules), best-effort.
func (s *Server) retainBuildArtifacts(id, workspace string) {
	if err := run.RetainBuildArtifacts(workspace, run.Dir(s.dataDir, id)); err != nil {
		log.Printf("run %s: before override rollback: %v", id, err)
	}
}

// modelUsageAccumulator rolls up ModelUsage entries in first-seen order as
// ComputeCostSummary (and the run-detail equivalent) walk a request's or
// run's evidence. finish sorts by (role, model) for a deterministic
// caller-facing order, rather than exposing this accumulator's own
// first-seen iteration order.
type modelUsageAccumulator struct {
	order   []modelUsageKey
	byModel map[modelUsageKey]*ModelUsage
}

func newModelUsageAccumulator() *modelUsageAccumulator {
	return &modelUsageAccumulator{byModel: map[modelUsageKey]*ModelUsage{}}
}

// add records tokens/cost observed for role/model (either may be "" --
// unknown). A zero-tokens-and-zero-cost observation (e.g. drafting
// evidence with a recorded-but-empty usage snapshot) must not create a
// spurious ByModel entry -- an entry exists only once it actually
// contributed something.
func (a *modelUsageAccumulator) add(role, model string, tokens, costMicroUSD int64) {
	if tokens == 0 && costMicroUSD == 0 {
		return
	}
	key := modelUsageKey{Role: role, Model: model}
	mu, ok := a.byModel[key]
	if !ok {
		mu = &ModelUsage{Role: role, Model: model}
		a.byModel[key] = mu
		a.order = append(a.order, key)
	}
	mu.Tokens += tokens
	mu.CostMicroUSD += costMicroUSD
}

// finish returns the accumulated entries sorted by role, then model --
// deterministic regardless of the map iteration/first-seen order above,
// so a caller (the console, `factoryd cost`) never has to sort this
// itself and two runs of the same rollup never disagree on order.
func (a *modelUsageAccumulator) finish() []ModelUsage {
	out := make([]ModelUsage, 0, len(a.order))
	for _, key := range a.order {
		out = append(out, *a.byModel[key])
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Role != out[j].Role {
			return out[i].Role < out[j].Role
		}
		return out[i].Model < out[j].Model
	})
	return out
}

// usageTotalTokens extracts the "totalTokens" figure from a SpecEvidence/
// PlanEvidence/AgentEvidenceRound Usage map -- ok is false exactly when
// usage carries no usable such figure, mirroring costFromUsage's own "no
// usable figure" contract.
func usageTotalTokens(usage map[string]any) (int64, bool) {
	if usage == nil {
		return 0, false
	}
	v, present := usage["totalTokens"]
	if !present || v == nil {
		return 0, false
	}
	f, ok := v.(float64)
	if !ok {
		return 0, false
	}
	return int64(f), true
}

// costFromUsage extracts the "total_cost_usd" figure build_app.py/
// draft_spec.py/plan_tickets.py's own parse_sonnet_usage/parse_usage
// helpers write into a SpecEvidence/PlanEvidence/AgentEvidenceRound Usage
// map, reporting ok=false when usage is nil, has no such key, or the
// value there is null or not a number -- every case in which this usage
// snapshot carries no cost figure at all.
func costFromUsage(usage map[string]any) (float64, bool) {
	if usage == nil {
		return 0, false
	}
	v, present := usage["total_cost_usd"]
	if !present || v == nil {
		return 0, false
	}
	f, ok := v.(float64)
	return f, ok
}

// pastSpecDrafting reports whether state is only reachable after
// CompleteSpecDrafting has run -- i.e. spec.md was produced, whether or
// not SpecEvidence survived alongside it. Deliberately excludes the
// terminal states (quarantined/halted/cancelled): those are reachable
// from any non-terminal state, including spec_drafting itself, so
// missing SpecEvidence there may just mean drafting never got the chance
// to finish, not a lost evidence file.
func pastSpecDrafting(state request.State) bool {
	switch state {
	case request.StateSpecReview, request.StateOracleDrafting, request.StateOracleReview,
		request.StatePlanning, request.StatePlanReview,
		request.StateBuilding, request.StatePRReview, request.StateDone:
		return true
	default:
		return false
	}
}

// specCostUnrecorded reports a request past spec drafting with no spec
// evidence whose drafting really did cost something. A spec the operator
// handed over that no model has revised cost nothing: its missing
// evidence is an exact zero, not a gap.
func specCostUnrecorded(req *request.Request) bool {
	return pastSpecDrafting(req.State) && !req.SpecAsHandedOver()
}

// planCostUnrecorded is specCostUnrecorded for the plan.
func planCostUnrecorded(req *request.Request) bool {
	return pastPlanning(req.State) && !req.PlanAsHandedOver()
}

// pastPlanning is pastSpecDrafting's own reasoning one state later: only
// reachable after CompletePlanning has run.
func pastPlanning(state request.State) bool {
	switch state {
	case request.StatePlanReview, request.StateBuilding, request.StatePRReview, request.StateDone:
		return true
	default:
		return false
	}
}

// ComputeCostSummary rolls up req's own spec/plan drafting cost plus every
// run it ever spent on into one CostSummary -- listRequests' per-request
// "cost_summary" field. Scans dataDir/runs once (run.ListByRequestID) to
// find every such run. A caller rolling this up for many requests at once
// (cmd/factoryd's `cost` subcommand with no -request, listRequests,
// streamRequestEvents) should call run.ListByRequestID itself and pass
// the result to ComputeCostSummaryWithRuns for every request instead --
// one scan for the whole batch rather than one per request.
func (s *Server) ComputeCostSummary(req *request.Request) CostSummary {
	runsByRequest, err := run.ListByRequestID(s.dataDir)
	if err != nil {
		log.Printf("request %s: cost summary: list runs: %v", req.ID, err)
	}
	return s.computeCostSummary(req, runsByRequest)
}

// ComputeCostSummaryWithRuns is ComputeCostSummary's own body, exported
// for a caller that already built runsByRequest (run.ListByRequestID)
// itself -- see ComputeCostSummary's own doc comment on why. runsByRequest
// may be nil (an empty or unreadable runs directory) -- every request
// then just finds no runs, same as an empty slice.
func (s *Server) ComputeCostSummaryWithRuns(req *request.Request, runsByRequest map[string][]*run.Run) CostSummary {
	return s.computeCostSummary(req, runsByRequest)
}

// computeCostSummary is ComputeCostSummary's own unexported body -- see
// ComputeCostSummaryWithRuns's doc comment for why it's also reachable
// from outside this package.
func (s *Server) computeCostSummary(req *request.Request, runsByRequest map[string][]*run.Run) CostSummary {
	cs := CostSummary{Currency: "usd", Complete: true, TokensComplete: true}
	acc := newModelUsageAccumulator()
	// A present SpecEvidence/PlanEvidence record with no usable dollar
	// figure (real drafting evidence from parse_usage() records token
	// counters and deliberately omits total_cost_usd -- see
	// costFromUsage's own doc comment) must mark this rollup incomplete,
	// the same as a ticket's missing run evidence does below -- found in
	// review: leaving Spec/Plan at a bare zero here presented a request
	// mid-drafting as an exact, misleadingly reassuring "$0.00" instead
	// of an honest "≥ $0.00" while its real cost was simply unknown.
	if req.SpecEvidence != nil {
		if sp := req.SpecEvidence.Spend; sp != nil {
			// A Spend record is relay-measured, real spend -- prefer it
			// over the self-reported Usage figure below (costFromUsage)
			// for both dollars and tokens, never both, so this job's cost
			// is never double-counted (found in review: the two sources
			// describe the same single pi invocation, not two separate
			// ones).
			cs.Spec = float64(sp.CostMicroUSD) / 1e6
			if sp.SpendPartial {
				cs.Complete = false
			}
			acc.add(sp.Role, sp.Model, sp.InputTokens+sp.OutputTokens, sp.CostMicroUSD)
		} else {
			if v, ok := costFromUsage(req.SpecEvidence.Usage); ok {
				cs.Spec = v
			} else {
				cs.Complete = false
			}
			if total, ok := usageTotalTokens(req.SpecEvidence.Usage); ok {
				acc.add("", req.SpecEvidence.Model, total, 0)
			} else {
				cs.TokensComplete = false
			}
		}
	} else if specCostUnrecorded(req) {
		// Found in review: spec_drafting can succeed (the request has
		// already moved on) while its own best-effort evidence file came
		// back absent or malformed -- SpecEvidence stays nil, but a real
		// cost was still incurred. Leaving Complete untouched in that
		// case presented an already-spent, merely unrecorded cost as an
		// exact, reassuring "$0.00" rather than the honest "≥ $0.00" a
		// present-but-unusable record already gets above. A request that
		// hasn't reached spec_review yet (still submitted/spec_drafting,
		// or quarantined/halted/cancelled before ever getting there) is
		// not covered by this check -- SpecEvidence there is absent
		// because there has been no chance yet to produce it, which is a
		// genuine zero, not a gap.
		cs.Complete = false
		cs.TokensComplete = false
	}
	if req.PlanEvidence != nil {
		if sp := req.PlanEvidence.Spend; sp != nil {
			// Same "prefer Spend, never both" reasoning as SpecEvidence
			// above.
			cs.Plan = float64(sp.CostMicroUSD) / 1e6
			if sp.SpendPartial {
				cs.Complete = false
			}
			acc.add(sp.Role, sp.Model, sp.InputTokens+sp.OutputTokens, sp.CostMicroUSD)
		} else {
			if v, ok := costFromUsage(req.PlanEvidence.Usage); ok {
				cs.Plan = v
			} else {
				cs.Complete = false
			}
			if total, ok := usageTotalTokens(req.PlanEvidence.Usage); ok {
				acc.add("", req.PlanEvidence.Model, total, 0)
			} else {
				cs.TokensComplete = false
			}
		}
	} else if planCostUnrecorded(req) {
		// Same reasoning as pastSpecDrafting above, one state later.
		cs.Complete = false
		cs.TokensComplete = false
	}
	// OracleDraft has no pre-Spend self-reported-Usage rollup to fall back
	// to (see CostSummary.Oracle's own doc comment) -- a nil Spend here
	// (no relay, or a request that never drafted oracles at all) simply
	// contributes nothing, never marks the rollup incomplete: unlike
	// Spec/Plan, there was never a "we know a real cost was incurred but
	// can't read it" case to distinguish from "this stage never ran".
	if req.OracleDraft != nil && req.OracleDraft.Spend != nil {
		sp := req.OracleDraft.Spend
		cs.Oracle = float64(sp.CostMicroUSD) / 1e6
		if sp.SpendPartial {
			cs.Complete = false
		}
		acc.add(sp.Role, sp.Model, sp.InputTokens+sp.OutputTokens, sp.CostMicroUSD)
	}
	// Runs: every run.Run record this request ever spent on, found by
	// scanning dataDir/runs for RequestID == req.ID (runsByRequest, built
	// once by run.ListByRequestID -- see ComputeCostSummary's own doc
	// comment), not by walking Ticket.RunID (the latest build only) and
	// Ticket.Rounds[].RunID (corrective rounds only). Found live (a Flutter + Go app repo
	// M-E1, 2026-09-28, request
	// feature-habit-insights-endpoint-and-mcp-20260928-082441): a ticket
	// retried after a quarantined first build left that first build's run
	// reachable from neither field -- Ticket.RunID had already been
	// overwritten by the retry's own run id, and Ticket.Rounds only ever
	// records corrective rounds, never a full retry -- silently dropping
	// $6.42 of $19.81 in real run spend (plus a second ticket's own
	// quarantined-then-corrected $1.26) from the request's reported
	// total. Every run a ticket build, a spec_conformity corrective
	// round, or a PR-review corrective round starts sets RequestID at
	// that same start point (request_driver.go, pr_review_driver.go), so
	// this scan sees all three without needing a second, ticket-shaped
	// implementation alongside it.
	runs := runsByRequest[req.ID]
	byID := make(map[string]*run.Run, len(runs))
	seen := make(map[string]bool, len(runs))
	for _, r := range runs {
		if r == nil || seen[r.ID] {
			// Dedup by run id: runsByRequest is scanned fresh, so this
			// only ever guards against a caller passing a map with
			// duplicate entries, not anything the scan itself produces.
			continue
		}
		seen[r.ID] = true
		byID[r.ID] = r
		runCost, complete, subscriptionBilled := runCostFromRun(r)
		cs.Runs += runCost
		if !complete {
			cs.Complete = false
		}
		if subscriptionBilled {
			cs.SubscriptionBilled = true
		}
		if !addRunModelUsage(r, acc) {
			cs.TokensComplete = false
		}
	}
	for _, ticket := range req.Tickets {
		if ticket.RunID == "" {
			continue
		}
		// Prefer the run already loaded by the scan above (byID) over a
		// second run.Load. A miss here means either a run that predates
		// the RequestID field (never seen by the scan at all) or a
		// runsByRequest the caller built for a different request set --
		// fall back to loading it directly, exactly the single Load
		// ComputeCostSummary always did for a ticket's own run before
		// this rollup switched to scanning every RequestID-tagged run.
		// Folded into cs.Runs/ByModel/Complete/TokensComplete here, not
		// just used for AcceptedTickets below, and deduped via seen/byID
		// the same as every run the scan itself found -- so a ticket
		// whose run predates RequestID is never silently left out of the
		// dollar total, and a ticket.RunID that can't be loaded at all
		// (a genuinely missing/corrupt record, not a known StartFailure)
		// still marks the whole rollup "≥", exactly as it did before.
		ticketRun := byID[ticket.RunID]
		if ticketRun == nil {
			loadedRun, err := run.Load(s.dataDir, ticket.RunID)
			if err != nil {
				cs.Complete = false
				cs.TokensComplete = false
			} else if !seen[loadedRun.ID] {
				seen[loadedRun.ID] = true
				byID[loadedRun.ID] = loadedRun
				ticketRun = loadedRun
				runCost, complete, subscriptionBilled := runCostFromRun(loadedRun)
				cs.Runs += runCost
				if !complete {
					cs.Complete = false
				}
				if subscriptionBilled {
					cs.SubscriptionBilled = true
				}
				if !addRunModelUsage(loadedRun, acc) {
					cs.TokensComplete = false
				}
			} else {
				ticketRun = byID[loadedRun.ID]
			}
		}
		// AcceptedTickets: this ticket's own run.StateAccepted is the
		// same gate-passing acceptance PR-review polls for -- see
		// CostSummary.AcceptedTickets' own doc comment on why this is
		// never "merged".
		if ticketRun != nil && ticketRun.State == run.StateAccepted {
			cs.AcceptedTickets++
		}
	}
	cs.Total = cs.Spec + cs.Plan + cs.Oracle + cs.Runs
	cs.ByModel = acc.finish()
	for _, m := range cs.ByModel {
		cs.Tokens += m.Tokens
	}
	if cs.AcceptedTickets > 0 {
		// Total's own dollar figure already sums every source this
		// rollup can see: both drafting stages (Spec/Plan/Oracle, each
		// preferring the exact relay-measured Spend when one exists) and
		// every ticket's build plus every corrective round (conformity or
		// PR-review) that ran, accepted or not -- exactly "total request
		// spend incl. drafting, failed and corrective rounds". Rounding
		// through float64 dollars here (rather than summing micro-USD
		// integers along the way) matches Total's own existing precision;
		// unlike a small per-model integer, a real-world total request
		// cost is nowhere near the magnitude float64 loses integer
		// precision at.
		cs.CostPerAcceptedTicketMicroUSD = int64(math.Round(cs.Total * 1e6 / float64(cs.AcceptedTickets)))
	}
	return cs
}

// addRunModelUsage adds r's own token/model contribution into acc,
// mirroring runCostFromRun's own relay-first, AgentEvidence-fallback
// preference (see that function's doc comment) applied to tokens instead
// of dollars. Returns false when r has no token information of either
// kind -- the same completeness contract runCostFromRun documents for
// cost. Takes an already-loaded *run.Run (not a runID) so a caller that
// scanned dataDir/runs once (computeCostSummary) or already has one in
// hand (runViewFor) never needs a second run.Load for the same run.
func addRunModelUsage(r *run.Run, acc *modelUsageAccumulator) bool {
	var relayTokens int64
	var relayCostMicroUSD int64
	var runModel string
	for _, attempt := range r.Attempts {
		relayTokens += attempt.RelayConsumedInputTokens + attempt.RelayConsumedOutputTokens
		relayCostMicroUSD += attempt.RelayConsumedCostMicroUSD
		if runModel == "" && attempt.RelayWorkerModelID != "" {
			runModel = attempt.RelayWorkerModelID
		}
	}
	// relayCostMicroUSD > 0, not just relayTokens > 0: a relay attempt's
	// cost is ordinarily derived from its own token counts, but this
	// rollup must not silently drop a real, nonzero relay spend recorded
	// with no accompanying tokens (found while adding cost tracking here
	// -- a run whose only relay-billed attempts had cost but zero tokens
	// used to fall through to the AgentEvidence-fallback branch below,
	// which returns incomplete/nothing for a run with no AgentEvidence at
	// all, silently losing that spend from ByModel entirely even though
	// runCost's own dollar total already counted it).
	if relayTokens > 0 || relayCostMicroUSD > 0 {
		complete := true
		for _, attempt := range r.Attempts {
			// A crashed relay's spend recovered from its usage ledger is
			// an honest lower bound, not the confirmed total (see
			// run.Attempt.RelaySpendPartial), so the rollup must read "≥".
			if attempt.RelaySpendPartial {
				complete = false
			}
			tokens := attempt.RelayConsumedInputTokens + attempt.RelayConsumedOutputTokens
			if tokens == 0 && attempt.RelayConsumedCostMicroUSD == 0 {
				continue
			}
			// attempt.Role, not a single run-wide runModel: a run can span
			// both an "execution" (build) and a "review" (spec_conformity)
			// attempt, each its own relay spend, and ModelUsage's own doc
			// comment on Role=="" ("unknown", never coerced to
			// "execution") applies here unchanged.
			acc.add(attempt.Role, attempt.RelayWorkerModelID, tokens, attempt.RelayConsumedCostMicroUSD)
		}
		return complete
	}
	if r.AgentEvidence == nil || len(r.AgentEvidence.Rounds) == 0 {
		return false
	}
	complete := true
	for _, round := range r.AgentEvidence.Rounds {
		total, ok := usageTotalTokens(round.Usage)
		if !ok {
			complete = false
			continue
		}
		// Role "" (unknown): AgentEvidenceRound is pi's own self-reported
		// evidence, which carries no factory-authored role at all (unlike
		// run.Attempt.Role) -- there is nothing trustworthy to label this
		// with. Cost is deliberately left 0 here: this fallback only ever
		// applies to a run with no relay spend recorded at all (see the
		// relayTokens>0 branch above), so there is no relay-measured
		// figure to attribute to any one model in this per-model
		// breakdown -- runCost's own AgentEvidence-derived dollar sum
		// still reaches cs.Runs/Total, just not per-model here.
		acc.add("", runModel, total, 0)
	}
	return complete
}

// runCostFromRun returns r's own contribution to a cost rollup: the
// authoritative RelayConsumedCostMicroUSD sum across its attempts when it
// spent anything through a relay, else the AgentEvidence-derived
// token-usage sum -- see ComputeCostSummary's own doc comments on why
// each is preferred/needed. complete is false when r has no cost
// information of either kind. Takes an already-loaded *run.Run, not a
// runID, for the same reason addRunModelUsage does (see its own doc
// comment) -- factored out of the former runCost(runID) once
// computeCostSummary started working from runsByRequest's already-loaded
// records instead of Load-ing each ticket/round RunID individually.
// subscriptionBilled is run.SubscriptionBilled(r.Attempts) -- always
// evaluated against the run's own attempts regardless of which cost
// source below was used, since it is independent evidence
// (RelayCredentialMode, not a dollar figure) recorded on the same attempts.
func runCostFromRun(r *run.Run) (cost float64, complete bool, subscriptionBilled bool) {
	subscriptionBilled = run.SubscriptionBilled(r.Attempts)
	// RelayConsumedCostMicroUSD (run.Attempt) is the authoritative
	// dollar figure for a relay-backed run -- populated at relay
	// cleanup from the relay's own usage log, not derived from the
	// agent's self-reported token counts. Found in review: ordinary
	// Pi build evidence (AgentEvidence.Rounds[].Usage) carries token
	// counters but deliberately omits total_cost_usd, so relying on
	// costFromUsage alone reported "≥ $0.00" for every normal
	// relay-backed run even when its real cost was nonzero. Prefer
	// the relay figure whenever this run actually spent anything
	// through it; only fall back to the AgentEvidence-derived sum
	// below for a run with no relay spend recorded at all (an older
	// evidence shape, or an engine that doesn't run behind a relay).
	var relayCost float64
	for _, attempt := range r.Attempts {
		relayCost += float64(attempt.RelayConsumedCostMicroUSD) / 1e6
	}
	if relayCost > 0 {
		return relayCost, true, subscriptionBilled
	}
	if r.AgentEvidence == nil || len(r.AgentEvidence.Rounds) == 0 {
		if len(r.Attempts) == 0 {
			// Never actually started: no build/verify attempt was ever
			// invoked, e.g. an infrastructure failure before the first
			// one. A genuine, exact zero cost, not evidence this rollup
			// failed to find something. Before this rollup switched to
			// scanning every RequestID-tagged run directly, a corrective
			// round in exactly this state was identified by its own
			// Round.StartFailure flag and skipped outright -- found in
			// review: counting it anyway hit this same zero-Attempts,
			// no-AgentEvidence run and returned complete=false, so a
			// plain infrastructure hiccup (already excluded from the
			// max_review_rounds budget for the same reason) made the
			// whole request's cost display "≥" forever. Checking
			// len(r.Attempts) directly here reaches the same
			// zero-cost/complete outcome for that case (and, now,
			// identically for a ticket's own first build never getting
			// off the ground) without needing Round.StartFailure's
			// separate bookkeeping at all.
			return 0, true, subscriptionBilled
		}
		return 0, false, subscriptionBilled
	}
	complete = true
	for _, round := range r.AgentEvidence.Rounds {
		if v, ok := costFromUsage(round.Usage); ok {
			cost += v
		} else {
			complete = false
		}
	}
	return cost, complete, subscriptionBilled
}

// requestTicketView is one entry of requestDetailView's own Tickets
// field: a ticket plus its plan file's content, read at request time --
// the console request board's detail screen renders a ticket's plan as
// monospace text without a second round trip to fetch it.
type requestTicketView struct {
	request.Ticket
	Content string `json:"content,omitempty"`
	// FullPath is where this ticket's plan file actually lives on disk,
	// home-relativized (see homeRelativePath) -- lets the console tell the
	// operator exactly what to edit in place (USAGE.md's documented
	// spec_review/plan_review workflow) without them reconstructing
	// -data-dir/requests/<id>/<SpecPath> by hand. Only correct when the
	// console's viewer and this server share a home directory (see
	// homeRelativePath's own doc comment).
	FullPath string `json:"full_path,omitempty"`
	// ActiveRoundRunID is the run of a PR-review corrective round that is
	// under way on this ticket, set only on GET /requests/{id} for a request
	// in pr_review. The ticket itself records a round when the round ends
	// (request.Round), so until then nothing on the request named it: the
	// page showed "ready for review" over a build in progress (found on a
	// real corrective round, 2026-10-06).
	ActiveRoundRunID string `json:"active_round_run_id,omitempty"`
}

// activeRoundRunIDs returns, per ticket index, the run of a PR-review
// corrective round still under way for req: a run that names req as its
// request, carries the round ticket id of that ticket
// ("<request>-NNN-review<k>-..."), is not yet recorded in the ticket's
// rounds, and has not reached a terminal state. Best effort: an unreadable
// runs directory yields none.
func activeRoundRunIDs(dataDir string, req *request.Request) map[int]string {
	if req.State != request.StatePRReview {
		return nil
	}
	byRequest, err := run.ListByRequestID(dataDir)
	if err != nil {
		return nil
	}
	out := map[int]string{}
	for _, ticket := range req.Tickets {
		recorded := map[string]bool{}
		for _, round := range ticket.Rounds {
			recorded[round.RunID] = true
		}
		prefix := fmt.Sprintf("%s-%03d-review", req.ID, ticket.Index)
		for _, candidate := range byRequest[req.ID] {
			if strings.HasPrefix(candidate.ID, prefix) && !recorded[candidate.ID] && !candidate.TerminalConfirmed() {
				out[ticket.Index] = candidate.ID
			}
		}
	}
	return out
}

// requestDetailView is GET /requests/{id}'s JSON shape: requestSummaryView
// plus the file contents the console request board's detail screen
// renders directly -- Spec (spec.md, once spec_drafting has written it)
// and each ticket's own
// plan file. Tickets here deliberately shadows the embedded
// request.Request's own Tickets field (encoding/json resolves a
// same-tagged name conflict in favor of the shallower field), carrying
// every Ticket field plus Content.
type requestDetailView struct {
	*request.Request
	Title string `json:"title"`
	// ActiveJob: see requestSummaryView.ActiveJob.
	ActiveJob *request.ActiveJob `json:"active_job,omitempty"`
	Spec      string             `json:"spec,omitempty"`
	// SpecFullPath is spec.md's own real path, home-relativized the same
	// way requestTicketView.FullPath is -- see that field's doc comment.
	SpecFullPath string              `json:"spec_full_path,omitempty"`
	Tickets      []requestTicketView `json:"tickets,omitempty"`
	CostSummary  CostSummary         `json:"cost_summary"`
	// ApproveNextState is the state an Approve call would move this
	// request to right now -- request.NextApprovalState, the same
	// function approve() itself branches on -- omitted outside a review
	// state (spec_review/oracle_review/plan_review). The console's own
	// approve-confirm sheet must not guess this from a hardcoded
	// table ("spec_review -> planning"), which is simply wrong for a
	// -draft-oracles request (it goes to oracle_drafting) -- the server
	// names its own real next state instead.
	ApproveNextState request.State `json:"approve_next_state,omitempty"`
	// NextAction is request.Request.NextAction()'s own one-sentence "what
	// does the operator do next", empty when nothing currently waits on
	// them: the console must show the same server-derived sentence the
	// CLI's `factoryd watch`/`status` do, not compute its own from a
	// client-side state table that can drift out of sync with the actual
	// halt cause (found live: the console's own "Next" told the operator
	// to retry a release-policy denial that its own Detail line, right
	// below, said would just be denied again).
	NextAction string `json:"next_action,omitempty"`
	// CanSendBack is true exactly when POST /requests/{id}/reject with a
	// "to" field would be legal for this request right now:
	// quarantined or halted (not with HaltOracleMaterialize, which has its
	// own narrower recovery), with no ticket yet accepted
	// (request.Request.AnyTicketAccepted) -- the console computes its
	// "Send back" button's enabled state from this rather than
	// re-deriving request.SendBack's own precondition client-side, the
	// same reasoning ApproveNextState exists for. The "plan" target's own
	// extra precondition is CanSendBackToPlan, below.
	CanSendBack bool `json:"can_send_back,omitempty"`
	// CanSendBackToPlan is CanSendBack narrowed to the "plan" target
	// (request.Request.SendBackPlanAllowed: an approved spec.md, and no
	// oracle stage skipped for a -draft-oracles request). The console
	// defaults its send-back dialog to "spec" and disables "plan" when
	// this is false, instead of letting the operator's first choice hit a
	// 409 (adversarial review of SendBack, round 2, 2026-09-26).
	CanSendBackToPlan bool `json:"can_send_back_to_plan,omitempty"`
}

// readRequestFileBestEffort reads path and returns its content, or ""
// when the file does not exist yet (e.g. spec.md before spec_drafting
// has written it) -- a missing file here is an expected, not an error,
// state for a view field that exists purely to save the console a second
// round trip.
func readRequestFileBestEffort(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}

// homeRelativePath returns path with this process's own home directory
// prefix replaced by "~", for a friendlier and less identity-revealing
// display in the console than the raw absolute path -- still directly
// usable (a shell, $EDITOR, or most editors expand "~" back to the real
// path themselves) unlike a path that redacted the username outright.
// Falls back to path unchanged when it isn't absolute, isn't under the
// home directory, or the home directory can't be determined -- the
// console always has something to show, just not the shortened form.
// Only correct when the console's own viewer and this server process
// share a home directory, true for every deployment this repo documents
// today (single-engineer, co-located); a multi-host deployment, where
// viewer and server don't share a home directory, would need this
// reconsidered.
func homeRelativePath(path string) string {
	if !filepath.IsAbs(path) {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}
	rel, err := filepath.Rel(home, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return path
	}
	if rel == "." {
		return "~"
	}
	return filepath.Join("~", rel)
}

// absPathBestEffort makes path absolute, resolving a relative one against
// this process's own working directory -- the same base resolveTicketSpecPath
// and request.SpecPath's caller already resolve relative paths against, so
// the path shown to the operator matches the one actually read from. Falls
// back to path unchanged if os.Getwd fails (rare, and homeRelativePath's own
// filepath.IsAbs guard already covers it doing nothing useful with a
// relative path either way).
func absPathBestEffort(path string) string {
	if path == "" || filepath.IsAbs(path) {
		return path
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	return abs
}

// resolveTicketSpecPath resolves a Ticket.SpecPath for reading. Absolute
// as-is. A relative value is ambiguous between two real conventions:
//   - internal/requestdriver/request_driver.go's own advancePlanning stores the
//     full path it built at plan time, filepath.Join(request.Dir(dataDir,
//     id), "tickets", filename) -- relative to the process's working
//     directory whenever -data-dir itself is relative (the default,
//     "data"), not relative to the request's own directory.
//   - Some other paths (older records, test fixtures, ticketSpecRelPaths'
//     own "tickets/NNN.spec.md" convention in internal/request/approve.go)
//     store the bare form, relative to the request's own directory.
//
// Found in review: this used to assume only the second form
// unconditionally, so a relative -data-dir (the common case) doubled the
// path -- request.Dir(dataDir, id) joined onto a value that already
// contained that same prefix -- and every ticket's content silently read
// back empty. Try the stored value as-is (relative to the working
// directory) first, since that's what the real production write path
// actually produces; fall back to resolving it against the request's own
// directory only if that doesn't exist.
func resolveTicketSpecPath(dataDir, id, specPath string) string {
	if specPath == "" || filepath.IsAbs(specPath) {
		return specPath
	}
	if _, err := os.Stat(specPath); err == nil {
		return specPath
	}
	return filepath.Join(request.Dir(dataDir, id), specPath)
}

// buildRequestDetailView assembles the requestDetailView for an
// already-loaded request -- shared by getRequest and the approve/reject
// handlers below so a request's title, spec, and per-ticket plan content
// don't vanish from the console's detail screen depending on which of
// those three endpoints last touched it.
func (s *Server) buildRequestDetailView(dataDir, id string, loaded *request.Request) requestDetailView {
	specPath := request.SpecPath(dataDir, id)
	view := requestDetailView{
		Request:      loaded,
		Title:        request.Title(dataDir, id),
		Spec:         readRequestFileBestEffort(specPath),
		SpecFullPath: homeRelativePath(absPathBestEffort(specPath)),
		CostSummary:  s.ComputeCostSummary(loaded),
		ActiveJob:    request.LoadActiveJob(dataDir, id),
		NextAction:   loaded.NextAction(),
		CanSendBack: (loaded.State == request.StateQuarantined ||
			(loaded.State == request.StateHalted && loaded.HaltKind != request.HaltOracleMaterialize)) &&
			!loaded.AnyTicketAccepted(),
	}
	view.CanSendBackToPlan = view.CanSendBack && loaded.SendBackPlanAllowed()
	if next, ok := request.NextApprovalState(loaded); ok {
		view.ApproveNextState = next
	}
	if len(loaded.Tickets) > 0 {
		view.Tickets = make([]requestTicketView, 0, len(loaded.Tickets))
		activeRounds := activeRoundRunIDs(dataDir, loaded)
		for _, ticket := range loaded.Tickets {
			resolved := resolveTicketSpecPath(dataDir, id, ticket.SpecPath)
			view.Tickets = append(view.Tickets, requestTicketView{
				Ticket:           ticket,
				Content:          readRequestFileBestEffort(resolved),
				FullPath:         homeRelativePath(absPathBestEffort(resolved)),
				ActiveRoundRunID: activeRounds[ticket.Index],
			})
		}
	}
	return view
}

// listRequests serves GET /requests: every request currently on disk
// under -data-dir/requests, oldest-submitted first (see request.List).
// Gated by the read token (WithReadToken/authorizeRead), like GET /runs
// so the console's request board needs no write
// credential to list requests. Approve and reject below stay behind the
// override token. An operator who treats draft specs and tickets as
// sensitive sets a read token; unset, read routes are open by design
// (see WithReadToken's own doc comment).
func (s *Server) listRequests(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeRead(r) {
		writeError(w, http.StatusForbidden, "requests endpoint is not authorized")
		return
	}
	requests, err := request.List(s.dataDir)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list requests")
		return
	}
	active, slots := daemonheartbeat.WorkerActiveRequests(s.dataDir, time.Now())
	waiting := request.WaitingOn(requests, active, slots)
	// One dataDir/runs scan for every request in this list, not one per
	// request -- see run.ListByRequestID's own doc comment. computeCostSummary
	// (via requestSummaryViewFor) looks each request's runs up in this
	// same map rather than triggering its own scan.
	runsByRequest, err := run.ListByRequestID(s.dataDir)
	if err != nil {
		log.Printf("list requests: list runs: %v", err)
	}
	views := make([]requestSummaryView, 0, len(requests))
	for _, req := range requests {
		views = append(views, s.requestSummaryViewFor(req, waiting[req.ID], runsByRequest))
	}
	writeJSON(w, http.StatusOK, views)
}

// requestSummaryViewFor builds one request's requestSummaryView --
// listRequests' per-row shape, reused by streamRequestEvents so an SSE
// event carries exactly the same JSON shape as a GET /requests list
// item. waitingOn is request.WaitingOn's answer for req ("" when none).
// runsByRequest is a run.ListByRequestID result the caller built once for
// its whole batch of requests (listRequests, streamRequestEvents) -- see
// computeCostSummary's own doc comment for why this is passed through
// rather than each request's cost summary re-scanning dataDir/runs.
func (s *Server) requestSummaryViewFor(req *request.Request, waitingOn string, runsByRequest map[string][]*run.Run) requestSummaryView {
	// The board needs no edit history, and each edit carries its diff: only
	// GET /requests/{id} sends them.
	listed := *req
	listed.Edits = nil
	view := requestSummaryView{
		Request:     &listed,
		Title:       request.Title(s.dataDir, req.ID),
		CostSummary: s.computeCostSummary(req, runsByRequest),
		ActiveJob:   request.LoadActiveJob(s.dataDir, req.ID),
		NextAction:  req.NextAction(),
	}
	view.WaitingOn = waitingOn
	return view
}

// streamRequestEvents serves GET /requests/events: an SSE "state" event
// per request, re-emitted whenever that request's durable record
// changes -- the request-board equivalent of streamRunEvents, and the
// same wire shape (event: state / data: …) so the console's existing
// SSE parser is reused verbatim.
//
// request.Request has no in-process write notification, and its store's
// single write path (internal/request/store.go's Save) is reachable
// from a separate factoryd process just as easily as from this one, so
// an in-process hook could not see every write anyway. This polls
// request.List on the same schedule streamRunEvents polls a single run,
// diffing each request's UpdatedAt to detect one that changed since the
// last poll -- the same poll-and-diff shape already proven out there,
// generalized from one record to a set of them.
//
// Gated by the read token, exactly like GET /requests.
func (s *Server) streamRequestEvents(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeRead(r) {
		writeError(w, http.StatusForbidden, "requests endpoint is not authorized")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	writeSSEHeaders(w)

	// lastPayload holds the last JSON this stream actually sent for each
	// request id, keyed for comparison rather than UpdatedAt alone.
	// Found in review: several real writes -- advancePRReadyOrApproved
	// recording a ticket's PRState, or its RunID once a build starts --
	// happen without a state transition, so they never change UpdatedAt.
	// Comparing only that field silently dropped SSE events for exactly
	// the ticket-rollup and cost_summary changes those writes produce,
	// while the board still labeled the connection "Live" -- those
	// updates then depended entirely on the separate 5s poll fallback,
	// defeating the point of this endpoint. Comparing the full emitted
	// JSON catches any change this view's shape can express, not just
	// the ones that happen to bump a timestamp.
	// lastPayload holds the last JSON this stream actually sent for each
	// request id, keyed for comparison rather than UpdatedAt alone.
	// Found in review: several real writes -- advancePRReadyOrApproved
	// recording a ticket's PRState, or its RunID once a build starts --
	// happen without a state transition, so they never change UpdatedAt.
	// Comparing only that field silently dropped SSE events for exactly
	// the ticket-rollup and cost_summary changes those writes produce,
	// while the board still labeled the connection "Live" -- those
	// updates then depended entirely on the separate 5s poll fallback,
	// defeating the point of this endpoint. Comparing the full emitted
	// JSON catches any change this view's shape can express, not just
	// the ones that happen to bump a timestamp.
	lastPayload := map[string]string{}
	emitChanged := func() error {
		requests, err := request.List(s.dataDir)
		if err != nil {
			return err
		}
		active, slots := daemonheartbeat.WorkerActiveRequests(s.dataDir, time.Now())
		waiting := request.WaitingOn(requests, active, slots)
		// One scan per poll tick for every request, not one per request
		// -- same reasoning as listRequests above.
		runsByRequest, err := run.ListByRequestID(s.dataDir)
		if err != nil {
			log.Printf("stream requests: list runs: %v", err)
		}
		for _, req := range requests {
			view := s.requestSummaryViewFor(req, waiting[req.ID], runsByRequest)
			b, err := json.Marshal(view)
			if err != nil {
				return err
			}
			payload := string(b)
			if lastPayload[req.ID] == payload {
				continue
			}
			if _, err := fmt.Fprintf(w, "event: state\ndata: %s\n\n", b); err != nil {
				return err
			}
			flusher.Flush()
			lastPayload[req.ID] = payload
		}
		return nil
	}

	if err := emitChanged(); err != nil {
		return
	}
	ticker := time.NewTicker(s.pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			if err := emitChanged(); err != nil {
				return
			}
		}
	}
}

// getRequest serves GET /requests/{id}: one request's full durable
// record, gated the same as listRequests (the read token, like GET /runs,
// so the console's request board works without the override token).
func (s *Server) getRequest(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeRead(r) {
		writeError(w, http.StatusForbidden, "requests endpoint is not authorized")
		return
	}
	id := r.PathValue("id")
	if !validRequestID(id) {
		writeError(w, http.StatusNotFound, "request not found")
		return
	}
	loaded, err := request.Load(s.dataDir, id)
	if errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusNotFound, "request not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "load request")
		return
	}
	writeJSON(w, http.StatusOK, s.buildRequestDetailView(s.dataDir, id, loaded))
}

// approveRequest serves POST /requests/{id}/approve: advances a request
// out of spec_review or plan_review, via internal/request.Approve -- the
// same function cmd/factoryd's own `approve` CLI subcommand calls, so
// neither path can diverge on what counts as a legal approval. Gated
// like POST /runs/{id}/override (WithOverrideToken): approving a request
// is the same class of operator write as overriding a run's terminal
// state. Responds with the same requestDetailView shape as GET
// /requests/{id}, not the raw request.Request, so the console's detail
// screen keeps showing the title and spec/ticket content after approval
// instead of losing them to the leaner durable record. The request body
// is optional; when present, its "by" field names the approving operator
// (falls back to requestAPIPrincipal when absent or the body is empty),
// symmetric with rejectRequest's own "by" handling below.
func (s *Server) approveRequest(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeRequestWrite(r) {
		writeError(w, http.StatusForbidden, "requests endpoint is not authorized")
		return
	}
	id := r.PathValue("id")
	if !validRequestID(id) {
		writeError(w, http.StatusNotFound, "request not found")
		return
	}
	var body approveRequestBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "decode request body")
		return
	}
	by := body.By
	if by == "" {
		by = requestAPIPrincipal
	}
	// ApproveShown, not Approve: the API must not pin oracle files the
	// client never displayed (the CLI path stays unconditional).
	loaded, err := request.ApproveShown(s.dataDir, id, by, time.Now(), body.ExpectedSHA256)
	if errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusNotFound, "request not found")
		return
	}
	if errors.Is(err, request.ErrOracleNotShown) {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	// request.ErrIllegalTransition is a precondition failure -- the
	// request just isn't in a state this call can act on -- not a
	// malformed request, so it maps to 409 like every other
	// "the resource changed under you" refusal on this route
	// (ErrOracleNotShown, ErrApprovalStale below). Every other
	// request.Approve error (a hash mismatch, an unusable oracle
	// directory, ...) is still a caller-input/precondition problem, never
	// an internal failure, so it keeps its prior 400 -- mirrors
	// overrideRun's own treatment of ApplyOverride's errors.
	if errors.Is(err, request.ErrIllegalTransition) {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	// request.ErrApprovalStale (an adversarial review, 2026-09-24, found):
	// the comment above already documented this as a 409 alongside
	// ErrOracleNotShown/ErrIllegalTransition, but no branch actually
	// mapped it -- a stale-hash approve (the exact "the resource changed
	// under you" case that comment names) fell through to the generic
	// err != nil 400 below instead.
	if errors.Is(err, request.ErrApprovalStale) {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	// The spec is as it was shown; it is the operator's answer that is
	// missing, so this is the same precondition class.
	if errors.Is(err, request.ErrOpenDecisions) {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.wakeRequest(r.Context(), id)
	writeJSON(w, http.StatusOK, s.buildRequestDetailView(s.dataDir, id, loaded))
}

// rejectRequest serves POST /requests/{id}/reject: sends a request in
// spec_review or plan_review back to the prior drafting state, via
// internal/request.Reject -- shared with cmd/factoryd's own `reject` CLI
// subcommand, and gated the same as approveRequest. Responds with the
// same requestDetailView shape as GET /requests/{id}, for the same
// reason approveRequest does. The request body's optional "by" field
// names the rejecting operator the same way approveRequest's does,
// falling back to requestAPIPrincipal when absent.
//
// The body's optional "to" field ("plan" or "spec") routes instead to
// internal/request.SendBack, the same way `factoryd reject
// -to` does -- meaningful only for a request
// currently quarantined or halted; SendBack itself refuses (409, via
// ErrIllegalTransition) a "to" given for any other state, so this handler
// need not special-case that here.
func (s *Server) rejectRequest(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeRequestWrite(r) {
		writeError(w, http.StatusForbidden, "requests endpoint is not authorized")
		return
	}
	id := r.PathValue("id")
	if !validRequestID(id) {
		writeError(w, http.StatusNotFound, "request not found")
		return
	}
	var body rejectRequestBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "decode request body")
		return
	}
	by := body.By
	if by == "" {
		by = requestAPIPrincipal
	}
	var (
		loaded *request.Request
		err    error
	)
	if body.To != "" && len(body.Anchors) > 0 {
		writeError(w, http.StatusBadRequest, "anchors apply to a review rejection, not to a send-back (to)")
		return
	}
	if body.To != "" {
		target := request.SendBackTarget(body.To)
		if !target.Valid() {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("to must be %q or %q, got %q", request.SendBackToPlan, request.SendBackToSpec, body.To))
			return
		}
		loaded, err = request.SendBack(s.dataDir, id, by, body.Reason, target, time.Now())
	} else {
		loaded, err = request.RejectAnchored(s.dataDir, id, by, body.Reason, body.Anchors, time.Now())
	}
	if errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusNotFound, "request not found")
		return
	}
	// See approveRequest's identical ErrIllegalTransition handling:
	// "cannot reject from this state" is a precondition failure (409), not
	// a malformed request (400) -- an empty reason (checked first, inside
	// request.Reject/request.SendBack, before either ever loads r) is
	// still a genuine 400.
	if errors.Is(err, request.ErrIllegalTransition) {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.wakeRequest(r.Context(), id)
	writeJSON(w, http.StatusOK, s.buildRequestDetailView(s.dataDir, id, loaded))
}

// retryRequest serves POST /requests/{id}/retry: recovers a quarantined or
// halted request via the shared internal/request.Retry -- the same function
// `factoryd retry <id>`'s own retryRequest branch calls, so neither path
// can diverge on what counts as a legal retry (see request.Retry's own
// doc comment for exactly which shapes it recognizes). Gated like
// approveRequest/rejectRequest (authorizeRequestWrite, the loopback
// relaxation included): retry is a state-changing operator write, the
// same class as approve/reject, not a new trust boundary of its own --
// it is not merge or deploy, so SC-001 is unaffected (this route only
// ever moves a request back into the same build/review/drafting pipeline
// approve/reject/the request driver already drive it through). The
// request body's optional "by" field names the retrying operator the same
// way approveRequest's does, falling back to requestAPIPrincipal when
// absent; "reason" is the operator's own stated reason for retrying,
// folded into request.Retry's own "retried"/"retried after: ..." History
// text (see retryReason) rather than discarded (an adversarial
// review, 2026-09-24, found: the console already requires one before it will
// call this route, but this handler decoded it and never passed it
// anywhere). 409 on a state this request cannot be
// retried from (request.ErrIllegalTransition), naming it.
func (s *Server) retryRequest(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeRequestWrite(r) {
		writeError(w, http.StatusForbidden, "requests endpoint is not authorized")
		return
	}
	id := r.PathValue("id")
	if !validRequestID(id) {
		writeError(w, http.StatusNotFound, "request not found")
		return
	}
	var body retryOrCancelRequestBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "decode request body")
		return
	}
	by := body.By
	if by == "" {
		by = requestAPIPrincipal
	}
	retry := request.Retry
	switch body.From {
	case "", "attempt":
	case "scratch":
		retry = request.RetryFromScratch
	default:
		writeError(w, http.StatusBadRequest, `"from" must be "attempt" or "scratch"`)
		return
	}
	loaded, err := retry(s.dataDir, id, by, body.Reason, time.Now(), s.prOpener)
	if errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusNotFound, "request not found")
		return
	}
	if errors.Is(err, request.ErrIllegalTransition) {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.wakeRequest(r.Context(), id)
	writeJSON(w, http.StatusOK, s.buildRequestDetailView(s.dataDir, id, loaded))
}

// resumeRequest serves POST /requests/{id}/resume: the operator's decision for
// a request waiting in resume_review, via the shared request.ResumeRequest the
// `factoryd resume` command also calls. The body's "from" is "round" (the
// default: continue the lost step, a build in its kept worktree) or "scratch"
// (start the lost build over); cancelling goes through POST
// /requests/{id}/cancel. Gated like retryRequest (authorizeRequestWrite): a
// state-changing operator write that only sends a request back into the
// pipeline, not merge or deploy. 409 for a request not in resume_review, or
// for a "round" whose preconditions do not hold (the reasons are in the
// message); 400 for an unknown "from".
func (s *Server) resumeRequest(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeRequestWrite(r) {
		writeError(w, http.StatusForbidden, "requests endpoint is not authorized")
		return
	}
	id := r.PathValue("id")
	if !validRequestID(id) {
		writeError(w, http.StatusNotFound, "request not found")
		return
	}
	var body resumeRequestBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "decode request body")
		return
	}
	by := body.By
	if by == "" {
		by = requestAPIPrincipal
	}
	from := body.From
	if from == "" {
		from = request.ResumeRound
	}
	loaded, err := request.ResumeRequest(s.dataDir, id, from, by, time.Now(), s.resumePreflight)
	if errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusNotFound, "request not found")
		return
	}
	var preflightErr *request.ResumePreflightError
	if errors.As(err, &preflightErr) {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	if errors.Is(err, request.ErrIllegalTransition) {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// A decision that continues no kept build (a drafting or planning rerun,
	// or pr_review, whose corrective rounds are never kept) ends the wait of
	// any worktree kept for this request: reap it, as cancel does.
	if loaded.State != request.StateBuilding {
		if err := clearKeptRunsOfRequest(s.dataDir, id); err != nil {
			log.Printf("request %s: could not discard a kept worktree on resume: %v", id, err)
		}
	}
	s.wakeRequest(r.Context(), id)
	writeJSON(w, http.StatusOK, s.buildRequestDetailView(s.dataDir, id, loaded))
}

// cancelRequest serves POST /requests/{id}/cancel: withdraws a request via
// internal/request.Cancel -- the first caller Request.Cancel ever
// had; also wired into `factoryd cancel` (cmd/factoryd/cancel.go). Gated
// like approveRequest/rejectRequest/retryRequest. The request body's
// optional "by"/"reason" fields name the cancelling operator and why,
// recorded verbatim on the appended History entry (Cancel's own reason
// parameter) -- falling back to requestAPIPrincipal/"cancelled" when
// absent, the same defaulting rejectRequest's own body uses. 409 on a
// state this request cannot be cancelled from (already a terminal state).
//
// Gap: this only ever marks the request record cancelled. If a ticket
// build is in flight (state building), cmd/factoryd's request driver
// notices the cancel before it persists the build's outcome and discards
// the result instead of resurrecting the request (request_driver.go's
// stillInState, wired into advanceBuilding/advanceSpecDrafting/
// advanceOracleDrafting/advancePlanning -- found via adversarial review,
// 2026-09-24: the driver held its own unlocked *request.Request from a
// pre-job request.List and would otherwise overwrite this Cancel with a
// stale in-memory Save once the job finished). But the underlying sandbox
// run itself is not stopped -- there is no context-cancel or signal hook
// from this route to the in-flight run_ticket.go execution, so a build
// already running when cancel is called keeps running to its own
// conclusion (using compute/relay budget, possibly opening a PR) even
// though the request it belongs to is now cancelled. Wiring that requires
// a cancellable context threaded from here through ticketRunner/runner
// down to the run itself, which does not exist today.
func (s *Server) cancelRequest(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeRequestWrite(r) {
		writeError(w, http.StatusForbidden, "requests endpoint is not authorized")
		return
	}
	id := r.PathValue("id")
	if !validRequestID(id) {
		writeError(w, http.StatusNotFound, "request not found")
		return
	}
	var body retryOrCancelRequestBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "decode request body")
		return
	}
	by := body.By
	if by == "" {
		by = requestAPIPrincipal
	}
	unlock, err := request.Lock(s.dataDir, id)
	if errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusNotFound, "request not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "lock request")
		return
	}
	// Released before the wake below, so a slow Temporal never holds the
	// request lock the worker also takes.
	release := sync.OnceFunc(unlock)
	defer release()
	loaded, err := request.Load(s.dataDir, id)
	if errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusNotFound, "request not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "load request")
		return
	}
	if err := loaded.Cancel(by, body.Reason, time.Now()); err != nil {
		if errors.Is(err, request.ErrIllegalTransition) {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := loaded.Save(s.dataDir); err != nil {
		writeError(w, http.StatusInternalServerError, "save request")
		return
	}
	release()
	// Cancelling is the decision that ends a lost build's wait: reap every
	// worktree of this request kept for a resume.
	if err := clearKeptRunsOfRequest(s.dataDir, id); err != nil {
		log.Printf("request %s: could not discard a kept worktree on cancel: %v", id, err)
	}
	s.wakeRequest(r.Context(), id)
	writeJSON(w, http.StatusOK, s.buildRequestDetailView(s.dataDir, id, loaded))
}

// clearKeptRunsOfRequest reaps every worktree of requestID's runs that was
// kept for a resume decision; see release.ClearKeptRunsOfRequest.
func clearKeptRunsOfRequest(dataDir, requestID string) error {
	return release.ClearKeptRunsOfRequest(dataDir, requestID)
}

// maxRequestContentBytes caps the JSON body updateRequestSpec/
// updateRequestTicket will read for a single edit -- a sane ceiling well
// above any real spec.md or ticket plan (a few KB), so an operator's own
// edit always fits while an oversized/pathological body is refused before
// it is ever fully buffered or written to disk.
const maxRequestContentBytes = 1 << 20 // 1 MiB

// updateRequestContentBody is PUT /requests/{id}/spec, PUT
// /requests/{id}/tickets/{n}, and PUT /requests/{id}/oracle/{name}'s shared
// JSON request body.
type updateRequestContentBody struct {
	Content string `json:"content"`
	// By is the operator saving the edit, recorded on the request's edit
	// history by the spec and ticket routes (the oracle route ignores it).
	// Like approve's "by", it is the client's own claim: the token carries
	// no identity.
	By string `json:"by,omitempty"`
	// BaseSHA256 is the hex sha256 of the content the client started
	// editing from -- optional, back-compat (empty applies this write
	// unconditionally, the CLI's own behavior and every caller's from
	// before the base_sha256 conflict check existed).
	// When present, it must match the file's current on-disk content
	// before this write is applied; this closes the editor/Approve race
	// (a Save can clobber newer text) in its two shapes: an Approve
	// landing between fetch and Save, and a Reject's redraft landing
	// between fetch and a stale Save that would otherwise silently
	// overwrite it.
	BaseSHA256 string `json:"base_sha256,omitempty"`
}

// verifyBaseSHA256 checks body.BaseSHA256 (see updateRequestContentBody's
// own doc comment) against currentContent's actual sha256, reading
// currentPath itself when currentContent is nil (the oracle route's caller
// doesn't have it in hand already -- see putRequestOracleFile). ok=true
// with nothing written means either BaseSHA256 was empty (unconditional,
// back-compat) or it matched; ok=false means a 409 response naming the
// current hash was already written and the caller must not proceed. A
// missing currentPath hashes as empty content ("no file yet"), so an edit
// that expects an as-yet-undrafted file to still not exist is checkable
// the same way.
func verifyBaseSHA256(w http.ResponseWriter, baseSHA256 string, currentPath string, currentContent []byte) bool {
	if baseSHA256 == "" {
		return true
	}
	if currentContent == nil {
		currentContent, _ = os.ReadFile(currentPath) // missing file -- nil/empty, handled like any other mismatch below
	}
	sum := sha256.Sum256(currentContent)
	current := hex.EncodeToString(sum[:])
	if strings.EqualFold(baseSHA256, current) {
		return true
	}
	writeJSON(w, http.StatusConflict, map[string]string{
		"error":          "content changed since it was fetched -- refresh and re-apply your edit",
		"current_sha256": current,
	})
	return false
}

// updateRequestSpec serves PUT /requests/{id}/spec: the console's "edit
// in place" alternative to opening spec.md in an editor (USAGE.md's own
// documented spec_review workflow) -- an operator can edit and save the
// drafted spec without leaving the console, then Approve as usual. Gated
// like approveRequest/rejectRequest (WithOverrideToken), since this is
// the same class of operator write. Allowed only in spec_review -- the
// one state a spec is still being reviewed rather than already approved
// or consumed by planning -- and only for content that passes the exact
// structural check the request driver itself applies
// (request.ValidateSpecSkeleton, internal/requestdriver/request_driver.go's own
// advanceSpecDrafting call) before this write, so an operator can never
// save a spec the pipeline would halt on later. Approval, and the
// SHA-256 hash it records, happens after this call as its own separate
// step (POST /requests/{id}/approve) -- an edit here never needs to touch
// ApprovedSHA256 itself, since spec_review is a pre-approval state.
func (s *Server) updateRequestSpec(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeRequestWrite(r) {
		writeError(w, http.StatusForbidden, "requests endpoint is not authorized")
		return
	}
	id := r.PathValue("id")
	if !validRequestID(id) {
		writeError(w, http.StatusNotFound, "request not found")
		return
	}
	// Held across the state check and the write so the request driver's
	// own read-modify-write (request.Lock, internal/requestdriver/request_driver.go)
	// and a concurrent approve cannot interleave with this edit.
	unlock, err := request.Lock(s.dataDir, id)
	if errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusNotFound, "request not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "lock request")
		return
	}
	defer unlock()
	loaded, err := request.Load(s.dataDir, id)
	if errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusNotFound, "request not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "load request")
		return
	}
	if loaded.State != request.StateSpecReview {
		writeError(w, http.StatusConflict, fmt.Sprintf("request %s: cannot edit spec.md from state %q (must be %q)", id, loaded.State, request.StateSpecReview))
		return
	}
	body, ok := decodeRequestContentBody(w, r)
	if !ok {
		return
	}
	content := body.Content
	specPath := request.SpecPath(s.dataDir, id)
	if !verifyBaseSHA256(w, body.BaseSHA256, specPath, nil) {
		return
	}
	if err := request.ValidateSpecSkeleton(content); err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	// ValidateSpecSkeleton only requires the "## Acceptance criteria"
	// section to be non-blank, not that it actually parses as a numbered
	// list -- SpecAcceptanceCriteria is the stricter parser planning
	// itself relies on (SpecAcceptanceCriteriaCount), and an edit that
	// passes the skeleton check but parses to zero criteria (prose with no
	// leading "1." markers, say) would otherwise be saved silently and
	// only fail once planning ran, silently ignoring a criterion the
	// operator could see.
	if _, err := request.SpecAcceptanceCriteria(content); err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if err := s.saveOperatorEdit(loaded, body.By, specPath, content); err != nil {
		writeError(w, http.StatusInternalServerError, "write spec")
		return
	}
	writeJSON(w, http.StatusOK, s.buildRequestDetailView(s.dataDir, id, loaded))
}

// updateRequestTicket serves PUT /requests/{id}/tickets/{n}: updateRequestSpec's
// own reasoning, one stage later -- editing a drafted ticket plan in place
// during plan_review. n must name an existing ticket (matched against
// loaded.Tickets[].Index, never used to construct a filesystem path
// directly -- the actual write path is the one already recorded on that
// ticket, resolveTicketSpecPath's own resolution, so a caller cannot walk
// this off the tickets directory no matter what n is), and the edited
// content must pass request.ValidateTicketSpecContent -- the same
// structural checks the plan-drafting job itself applies
// (internal/requestdriver/request_driver.go's own writeAndValidateDraftedTickets
// call site) and Approve's own plan_review branch re-checks before
// advancing state -- before it is written.
func (s *Server) updateRequestTicket(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeRequestWrite(r) {
		writeError(w, http.StatusForbidden, "requests endpoint is not authorized")
		return
	}
	id := r.PathValue("id")
	if !validRequestID(id) {
		writeError(w, http.StatusNotFound, "request not found")
		return
	}
	n, err := strconv.Atoi(r.PathValue("n"))
	if err != nil || n <= 0 {
		writeError(w, http.StatusNotFound, "ticket not found")
		return
	}
	// Same lock discipline as updateRequestSpec.
	unlock, err := request.Lock(s.dataDir, id)
	if errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusNotFound, "request not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "lock request")
		return
	}
	defer unlock()
	loaded, err := request.Load(s.dataDir, id)
	if errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusNotFound, "request not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "load request")
		return
	}
	if loaded.State != request.StatePlanReview {
		writeError(w, http.StatusConflict, fmt.Sprintf("request %s: cannot edit a ticket from state %q (must be %q)", id, loaded.State, request.StatePlanReview))
		return
	}
	var ticket *request.Ticket
	for i := range loaded.Tickets {
		if loaded.Tickets[i].Index == n {
			ticket = &loaded.Tickets[i]
			break
		}
	}
	if ticket == nil {
		writeError(w, http.StatusNotFound, "ticket not found")
		return
	}
	body, ok := decodeRequestContentBody(w, r)
	if !ok {
		return
	}
	content := body.Content
	ticketPath := resolveTicketSpecPath(s.dataDir, id, ticket.SpecPath)
	if !verifyBaseSHA256(w, body.BaseSHA256, ticketPath, nil) {
		return
	}
	if err := request.ValidateTicketSpecContent(content); err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if err := s.saveOperatorEdit(loaded, body.By, ticketPath, content); err != nil {
		writeError(w, http.StatusInternalServerError, "write ticket")
		return
	}
	writeJSON(w, http.StatusOK, s.buildRequestDetailView(s.dataDir, id, loaded))
}

// decodeRequestContentBody decodes updateRequestContentBody from r's
// body, capped at maxRequestContentBytes, writing its own error response
// and returning ok=false on any failure -- an oversized body (413), a
// malformed or trailing-data body (400). Shared by updateRequestSpec,
// updateRequestTicket and putRequestOracleFile so none can diverge on
// what counts as a legal body, mirroring startRunWithID's own "reject a
// second JSON value" check. Returns the whole decoded body, not just
// Content, so a caller can also see BaseSHA256.
func decodeRequestContentBody(w http.ResponseWriter, r *http.Request) (updateRequestContentBody, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestContentBytes)
	var body updateRequestContentBody
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(&body); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
		} else {
			writeError(w, http.StatusBadRequest, "decode request body")
		}
		return updateRequestContentBody{}, false
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		writeError(w, http.StatusBadRequest, "request body must contain one JSON object")
		return updateRequestContentBody{}, false
	}
	if len(body.By) > request.MaxEditByLen {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("by is longer than %d bytes", request.MaxEditByLen))
		return updateRequestContentBody{}, false
	}
	return body, true
}

// saveOperatorEdit writes an operator's validated edit of a reviewed file
// and records it on the request (request.RecordEdit: a revision of the text
// it replaced, and an entry in Request.Edits a later rejection hands to the
// drafter). by is the operator the client named, requestAPIPrincipal when it
// named none. The caller holds the request lock and loaded the request under
// it. A file outside the request's own directory (a ticket whose recorded
// spec_path points elsewhere) has no request-relative name to record under:
// it is written as before, unrecorded.
func (s *Server) saveOperatorEdit(loaded *request.Request, by, path, content string) error {
	if by == "" {
		by = requestAPIPrincipal
	}
	// Both made absolute first: a relative -data-dir beside a ticket path
	// recorded absolute is still the same directory.
	dir, dirErr := filepath.Abs(request.Dir(s.dataDir, loaded.ID))
	abs, absErr := filepath.Abs(path)
	rel, err := filepath.Rel(dir, abs)
	if dirErr != nil || absErr != nil || err != nil || !filepath.IsLocal(rel) {
		log.Printf("request %s: edit of %q by %q is not recorded: the file is outside the request's directory", loaded.ID, path, by)
		return writeFileAtomic(path, content)
	}
	return request.RecordEdit(s.dataDir, loaded, by, filepath.ToSlash(rel), content, time.Now())
}

// writeFileAtomic writes content to path via a temp file in the same
// directory followed by a rename, the same write-to-temp-then-rename
// pattern request.Request.Save uses -- a crash mid-write leaves the
// existing spec.md/ticket plan untouched, never a torn file.
func writeFileAtomic(path, content string) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// listRequestRevisions serves GET /requests/{id}/revisions: every
// rejected-spec/plan snapshot recorded for this request, gated the same
// as getRequest -- the read token, since this is read-only.
func (s *Server) listRequestRevisions(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeRead(r) {
		writeError(w, http.StatusForbidden, "requests endpoint is not authorized")
		return
	}
	id := r.PathValue("id")
	if !validRequestID(id) {
		writeError(w, http.StatusNotFound, "request not found")
		return
	}
	if _, err := request.Load(s.dataDir, id); errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusNotFound, "request not found")
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, "load request")
		return
	}
	revisions, err := request.ListRevisions(s.dataDir, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list revisions")
		return
	}
	writeJSON(w, http.StatusOK, revisions)
}

// requestRevisionDetailView is GET /requests/{id}/revisions/{n}'s JSON
// shape: revision n's own metadata plus the content of every file it
// snapshotted, keyed by the same relative path request.Revision.Files
// lists -- a separate type from request.Revision itself since "files"
// means something different at each of the two revision routes (a list
// of paths here vs. their content there).
type requestRevisionDetailView struct {
	Index     int               `json:"index"`
	At        string            `json:"at"`
	By        string            `json:"by"`
	Reason    string            `json:"reason"`
	FromState request.State     `json:"from_state"`
	Files     map[string]string `json:"files"`
	Kind      string            `json:"kind,omitempty"`
}

// getRequestRevision serves GET /requests/{id}/revisions/{n}: one
// revision's own recorded file contents, gated the same as
// listRequestRevisions.
func (s *Server) getRequestRevision(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeRead(r) {
		writeError(w, http.StatusForbidden, "requests endpoint is not authorized")
		return
	}
	id := r.PathValue("id")
	if !validRequestID(id) {
		writeError(w, http.StatusNotFound, "request not found")
		return
	}
	n, err := strconv.Atoi(r.PathValue("n"))
	if err != nil || n <= 0 {
		writeError(w, http.StatusNotFound, "revision not found")
		return
	}
	if _, err := request.Load(s.dataDir, id); errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusNotFound, "request not found")
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, "load request")
		return
	}
	rev, files, err := request.LoadRevision(s.dataDir, id, n)
	if err != nil {
		writeError(w, http.StatusNotFound, "revision not found")
		return
	}
	writeJSON(w, http.StatusOK, requestRevisionDetailView{
		Index:     rev.Index,
		At:        rev.At,
		By:        rev.By,
		Reason:    rev.Reason,
		FromState: rev.FromState,
		Files:     files,
		Kind:      rev.Kind,
	})
}

// validRequestID applies the same path-segment safety validRunID already
// enforces before joining an HTTP path value beneath a durable directory
// -- a request id has the same shape constraints as a run id (a single
// path segment, never "." or "..", no separator of its own), so this
// wraps that shared check under a name that reads correctly at each of
// this handler group's own call sites.
func validRequestID(id string) bool {
	return validRunID(id)
}

// authorizeRequestWrite reports whether r carries a valid
// "Authorization: Bearer <token>" header matching s.overrideToken, OR
// qualifies for the loopback-write relaxation (see
// loopbackSameOriginWrite): no override token was ever configured AND this
// Server is bound to loopback (WithListenAddr) AND r's own Host/Origin
// pair proves it is a real same-origin request from the console itself,
// never a third party. Without a configured token, every existing
// behavior is unchanged -- fail closed -- unless that stricter condition
// holds too; with a token configured, or off loopback, this is byte-for-
// byte the prior behavior (constant-time bearer compare, no relaxation at
// all). Constant-time comparison in the token path: this is a bearer
// credential, and a length/byte-position timing difference in a naive ==
// compare is exactly the kind of side channel that lets a remote caller
// recover it byte by byte.
//
// Gates the request pipeline's own operator writes (approve/reject/retry/
// cancel, the spec/ticket/oracle PUT editors) -- every one of them is
// scoped to a single request.Request the operator is already looking at
// in the console. It is deliberately NOT used by overrideRun: see
// authorizeOverride, below, and its own doc comment for why a release
// override needs the stricter, non-relaxed check (adversarial
// review, 2026-09-24).
func (s *Server) authorizeRequestWrite(r *http.Request) bool {
	if s.overrideToken == "" && s.loopbackSameOriginWrite(r) && loopbackWriteHasJSONContentType(r) {
		return true
	}
	return s.authorize(r, s.overrideToken)
}

// authorizeOverride reports whether r carries a valid "Authorization:
// Bearer <token>" header matching s.overrideToken -- unlike
// authorizeRequestWrite, above, it never grants the loopback-write
// relaxation. overrideRun (POST /runs/{id}/override) moves a quarantined
// run straight to accepted or halted and records a release Decision --
// the one write in this file that bypasses the build/verify pipeline
// outright, not an edit to a request still working its way through
// review. Before this split, authorizeOverride's own loopback relaxation
// meant any same-origin request from a process on the operator's machine
// (not just the console) could flip a quarantined run to accepted with no
// token at all, on a server started with WithOverrideToken precisely to
// require one for this operation (found via adversarial review,
// 2026-09-24). authorize itself already fails closed when s.overrideToken
// is "" (the endpoint's own -override-token flag was never set), so this
// is simply "no relaxation, ever" -- override always costs the token,
// loopback or not.
func (s *Server) authorizeOverride(r *http.Request) bool {
	return s.authorize(r, s.overrideToken)
}

// hostMatchesLoopback reports whether r's Host header is exactly
// "127.0.0.1:<port>", "localhost:<port>", or "[::1]:<port>" for this
// Server's own configured port (s.hostCheckPort, set by WithListenAddr).
// false whenever WithListenAddr was never called (s.hostCheckPort == ""),
// so this can never accidentally pass for a Server that never opted in.
func (s *Server) hostMatchesLoopback(r *http.Request) bool {
	if s.hostCheckPort == "" {
		return false
	}
	switch r.Host {
	case "127.0.0.1:" + s.hostCheckPort, "localhost:" + s.hostCheckPort, "[::1]:" + s.hostCheckPort:
		return true
	default:
		return false
	}
}

// hostAllowed reports whether r's Host header passes ServeHTTP's own Host
// check (DNS-rebinding defense): either this Server's own true loopback
// address (hostMatchesLoopback) or one of the operator-supplied
// -allowed-host values (WithAllowedHosts). Only the former ever feeds
// loopbackSameOriginWrite's no-token write relaxation -- see that
// function's own doc comment and WithAllowedHosts' for why an allowed
// host must never be treated as loopback for that purpose (added by an
// adversarial review, 2026-09-24).
func (s *Server) hostAllowed(r *http.Request) bool {
	return s.hostMatchesLoopback(r) || s.allowedHosts[r.Host]
}

// loopbackSameOriginWrite reports whether r qualifies for the
// loopback-writes-without-token relaxation ("no token for the
// single-machine case"): this Server is bound to loopback AND r's own
// Host header names it AND r's Origin exactly names that same origin, or
// (Origin absent) r's Sec-Fetch-Site says "same-origin". The relaxation
// is for the embedded console specifically, not "any local process" --
// see this function's own Content-Type companion,
// loopbackWriteHasJSONContentType, and the doc comment on
// authorizeRequestWrite's own call site for the rest of the story
// (an adversarial review, 2026-09-24, found: a request with no Origin
// and no Sec-Fetch-Site at all -- which the console's own fetch() always
// sends one of -- used to be treated the same as a real same-origin
// browser request, which is also exactly what a bare `curl` or a classic
// HTML <form> POST looks like). A request that fails this check must
// present the override token (`factoryd approve`/`cancel`/etc. -- see
// this Server's own doc comment -- still work unauthenticated against the
// data directory directly; only the HTTP surface tightened).
func (s *Server) loopbackSameOriginWrite(r *http.Request) bool {
	if !s.loopback || !s.hostMatchesLoopback(r) {
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "http://"+r.Host {
		return true
	}
	return origin == "" && r.Header.Get("Sec-Fetch-Site") == "same-origin"
}

// loopbackWriteHasJSONContentType reports whether r's Content-Type names
// application/json (ignoring any ";charset=..." parameter) -- required,
// in addition to loopbackSameOriginWrite's Origin/Host check, before the
// no-token relaxation applies to an actual write (added by an
// adversarial review, 2026-09-24). A classic HTML <form> POST -- one of
// the few cross-origin requests some browser configurations still send
// with neither an Origin nor a Sec-Fetch-Site header -- can only ever carry
// one of three browser-enumerated Content-Type values (application/
// x-www-form-urlencoded, multipart/form-data, text/plain), never
// application/json, so this closes that gap independently of the
// Origin/Sec-Fetch-Site signals above. The console's own HTTP client (console/src/api/http.ts)
// always sends application/json on every write route this gates.
func loopbackWriteHasJSONContentType(r *http.Request) bool {
	ct := r.Header.Get("Content-Type")
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	return strings.TrimSpace(ct) == "application/json"
}

// authorizeStart gates POST /runs, the daemon lifecycle routes, and the
// two release routes, all of which have always shared s.startToken.
func (s *Server) authorizeStart(r *http.Request) bool {
	return s.authorize(r, s.startToken)
}

// authorizeRead reports whether r may proceed to a read route. Deliberately
// the inverse default of authorize(): an unconfigured s.readToken ("") means
// this read route stays open, not disabled -- see WithReadToken's doc
// comment for why. Once a read token is configured, it behaves exactly like
// authorize() against any other token: a missing/wrong bearer credential is
// rejected. A tool call POST /mcp replayed (mcpCaller) already presented the
// MCP token, which grants every read its tool table names.
func (s *Server) authorizeRead(r *http.Request) bool {
	if mcpCaller(r) {
		return true
	}
	if s.readToken == "" {
		return true
	}
	return s.authorize(r, s.readToken)
}

func (s *Server) authorize(r *http.Request, token string) bool {
	if token == "" {
		return false
	}
	const prefix = "Bearer "
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	presented := strings.TrimPrefix(header, prefix)
	if presented == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(presented), []byte(token)) == 1
}

func (s *Server) loadRun(id string) (*run.Run, error) {
	if !validRunID(id) {
		return nil, os.ErrNotExist
	}
	return run.Load(s.dataDir, id)
}

// validRunID reports whether id is safe to join beneath the durable runs
// directory — a single path segment, not "." or "..", containing no
// separator of its own. run.Load and run.WithLock both blindly
// filepath.Join dataDir/"runs"/id (and, for WithLock, MkdirAll that
// result) without this check themselves, trusting callers to have already
// validated it, so it must be enforced here, before either is ever
// called with a value taken from an HTTP path.
func validRunID(id string) bool {
	return id != "" && id != "." && id != ".." && !strings.ContainsAny(id, `/\`)
}

// terminal reports whether r's current record can never change again, so
// streamRunEvents is safe to close after delivering it. Found via review:
// StateQuarantined does NOT qualify, even though it used to be included
// here (back when nothing could ever move a run out of it) — ApplyOverride
// (the new POST /runs/{id}/override endpoint's own domain logic) can
// transition a quarantined run to accepted or halted at any later time,
// so a client that connected while (or after) a run was already
// quarantined must keep watching to observe that, not have this stream
// close on it immediately. StateAccepted is unconditionally final:
// nothing in this codebase ever moves a run out of it.
//
// StateHalted additionally requires r.HaltConfirmed — found via review,
// second round: a run can be durably recorded StateHalted before its real
// outcome is positively known (see run.Run.HaltConfirmed's own doc
// comment) — specifically, cmd/factoryd's Temporal give-up path, which
// records a halt locally without confirmation that the underlying
// execution actually stopped, precisely so a daemon's reclaim scan keeps
// polling for what really happened. A client that already disconnected on
// seeing that unconfirmed halt would never learn its later reconciliation
// to a different terminal state (StateAccepted or a confirmed
// StateHalted) — this codebase's own daemon-side reclaim logic already
// treats an unconfirmed halt as non-final for exactly that reason
// (terminalReclaimedRunIDs), and a live client watching the same run
// needs the same treatment, not a laxer one.
func terminal(r *run.Run) bool {
	if r.State == run.StateAccepted {
		return true
	}
	return r.State == run.StateHalted && r.HaltConfirmed
}

func createdAtAfter(a, b string) bool {
	aTime, aErr := time.Parse(time.RFC3339, a)
	bTime, bErr := time.Parse(time.RFC3339, b)
	if aErr == nil && bErr == nil {
		return aTime.After(bTime)
	}
	if aErr == nil {
		return true
	}
	if bErr == nil {
		return false
	}
	return a > b
}

// writeStateEvent writes v (any JSON-marshalable value) as one SSE
// "state" event and flushes it immediately -- the wire shape both
// streamRunEvents and streamRequestEvents use, so a console SSE parser
// written against one works verbatim against the other.
func writeStateEvent(w http.ResponseWriter, flusher http.Flusher, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "event: state\ndata: %s\n\n", b); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
