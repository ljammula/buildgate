// Package sessionconfig loads the operator's session-side factoryd
// configuration: the sandbox/relay/registry-proxy values `factoryd
// worker` needs that belong to this machine and its model route rather
// than to any one repository (that side is internal/projectconfig's
// .factory.yml). Its whole purpose is that the documented command is
// `factoryd worker` alone instead of a dozen flags.
//
// Every key mirrors a flag name with underscores (a Tier-1 flag still on
// some entry point's CLI, or a Tier-2 knob that flags-consolidate
// (2026-09-10) removed from the CLI entirely). A key that is absent stays
// absent -- ApplySettings overlays only the keys actually present onto
// DefaultSettings' hard defaults -- so the caller gets exactly the
// precedence chain the flags-consolidate task defines: defaults, then this
// file, then .factory.yml (internal/projectconfig, repo-scoped only), then
// an explicit Tier-1 CLI flag.
package sessionconfig

import (
	"bytes"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"

	"buildgate/internal/modelhost"
	"buildgate/internal/sandbox"
)

// Config is the on-disk schema. Pointer fields distinguish "absent" from
// a zero value; relay_worker_model_extra_json is a YAML mapping and is
// serialized to the JSON object string the flag takes. Every tag carries
// ,omitempty -- required for ApplySettings' own documented "a key that is
// absent stays absent" contract to hold for a config written by
// yaml.Marshal (not just a hand-edited file): without it, yaml.v3
// marshals a nil pointer as `null`, a nil slice as `[]`, and a nil map as
// `{}` for every unset field rather than omitting the key outright, and
// `[]`/`{}` both unmarshal back into a non-nil-but-empty value on the
// next load -- indistinguishable from an operator deliberately emptying
// e.g. compose_services_allowed_registries, which ApplySettings then
// honors as a real override of the non-empty default (found via
// `factoryd quickstart`'s own generated config, which is the one thing in
// this codebase that yaml.Marshal's a *Config at all: it silently zeroed
// every repo's compose_services_allowed_registries to fail-closed).
type Config struct {
	DataDir         *string `yaml:"data_dir,omitempty"`
	OpenPullRequest *bool   `yaml:"open_pull_request,omitempty"`
	SandboxImage    *string `yaml:"sandbox_image,omitempty"`
	// MeterImage is buildgate's own meter image (internal/meter), written by
	// `make install` through `factoryd configure-images`. Only the OpenShell
	// stack's start and doctor read it.
	MeterImage *string `yaml:"meter_image,omitempty"`
	// ImageSourceRoot is the absolute path of the buildgate checkout
	// `factoryd configure-images` was run from -- written alongside
	// SandboxImage/MeterImage/RegistryProxyImage by `make install`'s own
	// local-images target, so a later staleness check
	// (internal/imageinputs.Hash) knows which checkout's current source
	// to compare each configured image's own stamped
	// buildgate.inputs-hash label against. Never read by any
	// build/launch path -- advisory only.
	ImageSourceRoot *string `yaml:"image_source_root,omitempty"`
	// ModelHostConcurrency is internal/modelhost's own concurrency ceiling
	// for relay_upstream -- see that package's doc comment for why a
	// single-instance model host needs one at all. Default 1 (one
	// model-bound job at a time); 0 disables the lock outright.
	ModelHostConcurrency *int `yaml:"model_host_concurrency,omitempty"`
	// MaxParallelJobs caps how many model jobs and builds `factoryd worker`
	// runs at once across all requests (its jobs task queue). Default 3;
	// at least 1.
	MaxParallelJobs                *int    `yaml:"max_parallel_jobs,omitempty"`
	RegistryProxy                  *bool   `yaml:"registry_proxy,omitempty"`
	RegistryProxyImage             *string `yaml:"registry_proxy_image,omitempty"`
	RegistryProxyNPMUpstream       *string `yaml:"registry_proxy_npm_upstream,omitempty"`
	RegistryProxyPyPIUpstream      *string `yaml:"registry_proxy_pypi_upstream,omitempty"`
	RegistryProxyPyPIFilesUpstream *string `yaml:"registry_proxy_pypi_files_upstream,omitempty"`
	RegistryProxyGoUpstream        *string `yaml:"registry_proxy_go_upstream,omitempty"`
	RegistryProxyGoSumDBUpstream   *string `yaml:"registry_proxy_gosumdb_upstream,omitempty"`
	// Workspaces allowlists the absolute workspace paths POST /requests
	// (internal/api) will accept from an HTTP caller in
	// addition to a workspace that is already the Workspace of some existing
	// internal/request.Request under the data dir. Unlike every other
	// session-config key, this one has no CLI-flag equivalent to be an
	// "explicit vs. config default" precedence about: `factoryd submit`
	// is a local operator action that can point at any workspace on this
	// machine, so it never consulted this list; only the HTTP route
	// needs a bound on which host paths a browser-driven caller can name
	// a run against, the same allowlist-not-arbitrary-path posture
	// `-api-allowed-sandbox-images` already gives POST /runs' own
	// sandbox_image (safety-contract.md's Control-plane API trust
	// boundary row).
	Workspaces []string `yaml:"workspaces,omitempty"`

	// SkillDirs lists the host directories (absolute, or ~/ prefixed)
	// searched, in order, for the skill folders roles.<role>.skills name.
	// See ResolveRoleSkills and ValidateSkills.
	SkillDirs []string `yaml:"skill_dirs,omitempty"`
	// DesignGuideDirs lists the host directories (absolute, or ~/ prefixed)
	// searched, in order, for the <name>.md a target repository's
	// .factory.yml design_guide names. See LoadDesignGuide.
	DesignGuideDirs []string `yaml:"design_guide_dirs,omitempty"`

	// EgressCABundle is a PEM file on this machine (e.g. a corporate TLS-
	// interception proxy's CA) bind-mounted read-only into the relay and
	// registry-proxy containers -- see -egress-ca-bundle's own flag help.
	EgressCABundle *string `yaml:"egress_ca_bundle,omitempty"`

	// ComposeServices mirrors RegistryProxy's own three-tier resolution
	// (explicit -compose-services flag > this key > the flag's own true
	// default) -- see -compose-services' own flag help.
	ComposeServices                  *bool    `yaml:"compose_services,omitempty"`
	ComposeServicesMemory            *string  `yaml:"compose_services_memory,omitempty"`
	ComposeServicesCPUs              *string  `yaml:"compose_services_cpus,omitempty"`
	ComposeServicesMaxServices       *int     `yaml:"compose_services_max_services,omitempty"`
	ComposeServicesReadyTimeout      *string  `yaml:"compose_services_ready_timeout,omitempty"`
	ComposeServicesAllowedRegistries []string `yaml:"compose_services_allowed_registries,omitempty"`
	// ComposeServicesWorkerEnv supplies application-specific endpoint values
	// to the worker after the host has launched the validated sidecars. It
	// stays on the worker's session configuration and is intentionally not
	// carried in Temporal workflow input.
	ComposeServicesWorkerEnv map[string]string `yaml:"compose_services_worker_env,omitempty"`
	// ComposeServicesRequireDigest, opt-in and false by default, refuses any
	// sidecar image ParseFile would otherwise allow unless it is pinned by
	// digest (an "@sha256:..." reference), the same convention the worker
	// and relay canonical images already use unconditionally. Sidecar
	// images are not digest-pinned by default (Fable review 2026-09-15 #6,
	// containment-matrix.md's Package registry row): a target repo's own
	// compose file names a registry/tag, and a tag can move between two
	// runs of the same accepted ticket. Left off by default because the
	// default allow-list target (Docker Hub official images by tag) is
	// what most target repos already write, and forcing digests would turn
	// "just works" into "every project needs its compose file rewritten
	// first" -- see containment-matrix.md for the accepted residual this
	// documents.
	ComposeServicesRequireDigest *bool `yaml:"compose_services_require_digest,omitempty"`
	// ComposeServicesConcurrency caps how many runs on this machine -- any
	// data dir, any factoryd process -- may have compose sidecars up at
	// once (sandbox.AcquireComposeServicesGate). Default 1: a second
	// sidecar stack plus worker rarely fits one Docker VM. 0 disables it.
	ComposeServicesConcurrency *int `yaml:"compose_services_concurrency,omitempty"`

	// Everything below was a CLI flag on `factoryd`/`factoryd worker`/
	// `factoryd daemon`/`factoryd doctor` until the flags-consolidate
	// change (2026-09-10): a per-subsystem tuning knob with one sensible
	// value, registered as a flag because the PR that added it also added
	// a flag, then re-registered on every entry point that needed it. Each
	// now has a hard default equal to its former flag default (see
	// DefaultSettings), and for most of them this file is the only way to
	// change it -- no entry point has a CLI flag left.
	//
	// The exceptions are build_app_max_attempts/verify_max_attempts just
	// below and relay_allow_plaintext_upstream/relay_allow_no_credential/
	// relay_worker_model_extra_json further down: POST /runs sets each of
	// those per request, so `factoryd <run>` and `worker` kept a real
	// flag for them. The key here still applies to any invocation that
	// leaves that flag unset -- these are defaults, not dead keys.
	BuildAppMaxAttempts *int `yaml:"build_app_max_attempts,omitempty"`
	VerifyMaxAttempts   *int `yaml:"verify_max_attempts,omitempty"`

	// PRPollInterval/PRIgnoreAuthors/PRTrustedAuthors mirror worker's
	// own -pr-poll-interval/-pr-ignore-authors/-pr-trusted-authors flags.
	// Like BuildAppMaxAttempts/VerifyMaxAttempts above, applySessionConfig
	// fills the flag directly from this Config field rather than through
	// Settings -- these are per-invocation Tier-1 values, not daemon-wide
	// Tier-2 settings.
	PRPollInterval   *string  `yaml:"pr_poll_interval,omitempty"`
	PRIgnoreAuthors  []string `yaml:"pr_ignore_authors,omitempty"`
	PRTrustedAuthors []string `yaml:"pr_trusted_authors,omitempty"`

	// MaxReviewRounds mirrors worker's own -max-review-rounds flag:
	// the most corrective-build rounds a ticket's PR may go through
	// before its request is halted with "review rounds
	// exhausted". Like PRPollInterval, this is a per-invocation Tier-1
	// value (applySessionConfig fills the flag directly from this
	// field), not a daemon-wide Tier-2 setting.
	MaxReviewRounds *int `yaml:"max_review_rounds,omitempty"`

	// ReviewCorrectiveRounds mirrors worker's own
	// -review-corrective-rounds flag: the most
	// automatic corrective builds a ticket gets when its run quarantines
	// with its failed gates a subset of {spec_conformity, code_review},
	// before the request quarantines as it does today, and the most fix
	// attempts a PR-review corrective round gets when the same two gates
	// alone quarantine it. Like
	// MaxReviewRounds, this is a per-invocation Tier-1 value
	// (applySessionConfig fills the flag directly from this field), not a
	// daemon-wide Tier-2 setting. 0 disables the corrective round
	// entirely. The prior key, conformity_corrective_rounds, is not
	// accepted at all (KnownFields rejects it by name) -- it covered only
	// spec_conformity and a stale config must fail loudly, not silently
	// keep running under the old, narrower behavior.
	ReviewCorrectiveRounds *int `yaml:"review_corrective_rounds,omitempty"`

	// HITLReminderInterval mirrors worker's own
	// -hitl-reminder-interval flag: how often a request waiting in
	// spec_review or plan_review is re-reminded. Like PRPollInterval,
	// this is a per-invocation Tier-1 value (applySessionConfig fills the
	// flag directly from this field), not a daemon-wide Tier-2 setting.
	HITLReminderInterval *string `yaml:"hitl_reminder_interval,omitempty"`

	// AdvanceOn mirrors worker's own -advance-on flag: whether
	// a request's ticket build advances to the next ticket as soon as a
	// ticket's run is accepted ("accepted", the default) or only once its
	// PR is approved ("pr_approved"). Like PRPollInterval/
	// HITLReminderInterval above, this is a per-invocation Tier-1 value
	// (applySessionConfig fills the flag directly from this field), not a
	// daemon-wide Tier-2 setting.
	AdvanceOn *string `yaml:"advance_on,omitempty"`

	// ConformityPolicy mirrors worker's own -conformity-policy flag:
	// build_app.py's own --conformity-policy (required|advisory, governs
	// the per-criterion spec-conformity review -spec-acceptance-criteria
	// enables). Like AdvanceOn above, this is a per-invocation Tier-1
	// value (applySessionConfig fills the flag directly from this
	// field), not a daemon-wide Tier-2 setting.
	ConformityPolicy *string `yaml:"conformity_policy,omitempty"`

	// CodeReviewPolicy mirrors worker's own -code-review-policy flag:
	// the standalone AI code-review pass's own policy (off|advisory|
	// required, agent/pi/scripts/code_review.py, internal/codereview).
	// Like ConformityPolicy above, this is a per-invocation Tier-1 value
	// (applySessionConfig fills the flag directly from this field), not
	// a daemon-wide Tier-2 setting.
	CodeReviewPolicy *string `yaml:"code_review_policy,omitempty"`

	SandboxDocker    *string `yaml:"sandbox_docker,omitempty"`
	SandboxUser      *string `yaml:"sandbox_user,omitempty"`
	SandboxWorkerUID *int    `yaml:"sandbox_worker_uid,omitempty"`
	SandboxMemory    *string `yaml:"sandbox_memory,omitempty"`
	SandboxCPUs      *string `yaml:"sandbox_cpus,omitempty"`
	SandboxTmpfsSize *string `yaml:"sandbox_tmpfs_size,omitempty"`

	MeterMaxRequestBytes     *int64  `yaml:"meter_max_request_bytes,omitempty"`
	MeterRequestsPerMinute   *int    `yaml:"meter_requests_per_minute,omitempty"`
	MeterTokenBudget         *int    `yaml:"meter_token_budget,omitempty"`
	MeterTokenBudgetWindow   *string `yaml:"meter_token_budget_window,omitempty"`
	MeterCostBudgetMicroUSD  *int64  `yaml:"meter_cost_budget_micro_usd,omitempty"`
	MeterCostBudgetWindow    *string `yaml:"meter_cost_budget_window,omitempty"`
	MeterTokenCeiling        *int    `yaml:"meter_token_ceiling,omitempty"`
	MeterCostCeilingMicroUSD *int64  `yaml:"meter_cost_ceiling_micro_usd,omitempty"`

	// RequestTokenBudget/RequestCostBudgetMicroUSD cap one request's own
	// total spend across every stage it can launch a relay-backed job
	// through: spec/plan/oracle drafting plus every ticket build and
	// every corrective round (spec_conformity or PR-review). Unlike
	// MeterTokenCeiling/MeterCostCeilingMicroUSD above (a per-job ceiling
	// the relay itself enforces mid-job), these are checked host-side,
	// before a job is even launched -- see cmd/factoryd's
	// checkLaunchBudget. 0/absent means unlimited, matching every other
	// budget-shaped key in this file.
	RequestTokenBudget        *int   `yaml:"request_token_budget,omitempty"`
	RequestCostBudgetMicroUSD *int64 `yaml:"request_cost_budget_micro_usd,omitempty"`
	// MonthlyTokenBudget/MonthlyCostBudgetMicroUSD cap total spend across
	// every request in <data-dir>/requests during the current calendar
	// month (UTC) -- on a single-operator setup, the per-developer
	// budget. See checkLaunchBudget's own doc comment.
	MonthlyTokenBudget        *int   `yaml:"monthly_token_budget,omitempty"`
	MonthlyCostBudgetMicroUSD *int64 `yaml:"monthly_cost_budget_micro_usd,omitempty"`

	RegistryProxyCacheBytes            *int64  `yaml:"registry_proxy_cache_bytes,omitempty"`
	RegistryProxyMaxObjectBytes        *int64  `yaml:"registry_proxy_max_object_bytes,omitempty"`
	RegistryProxyMaxConcurrentUpstream *int    `yaml:"registry_proxy_max_concurrent_upstream,omitempty"`
	RegistryProxyUpstreamTimeout       *string `yaml:"registry_proxy_upstream_timeout,omitempty"`

	ReleaseProtectedPaths                 *string `yaml:"release_protected_paths,omitempty"`
	ReleaseMaxFilesChanged                *int    `yaml:"release_max_files_changed,omitempty"`
	ReleaseMaxInsertions                  *int    `yaml:"release_max_insertions,omitempty"`
	ReleaseRollbackPlan                   *string `yaml:"release_rollback_plan,omitempty"`
	ReleaseAllowOverrides                 *bool   `yaml:"release_allow_overrides,omitempty"`
	ReleaseAllowDependencyLockfileChanges *bool   `yaml:"release_allow_dependency_lockfile_changes,omitempty"`
	ReleaseAllowUnsandboxed               *bool   `yaml:"release_allow_unsandboxed,omitempty"`
	ReleaseAllowSkippedProjectCheck       *bool   `yaml:"release_allow_skipped_project_check,omitempty"`

	// Roles picks a model (see Config.Models) and a Pi reasoning
	// ("thinking") level per kind of work -- planning, execution, review.
	// Required for any launch that needs a relay: see ValidateRouting.
	Roles *Roles `yaml:"roles,omitempty"`

	// Routes/Models are the only session-config schema: an
	// operator-configured set of named upstream/credential routes and
	// named models, each of which picks its own ordered fallback list of
	// routes -- never another model. See internal/sessionconfig/routing.go.
	Routes map[string]Route `yaml:"routes,omitempty"`
	Models map[string]Model `yaml:"models,omitempty"`

	// presentKeys records every top-level YAML key the source config file
	// actually contained, set only by Load (never yaml-tagged itself, so
	// no config key could ever collide with it and no hand-built Config
	// can set it) -- a generic replacement for one hand-maintained bool
	// per key: a pointer field set to a YAML null (a bare key with
	// nothing after it, or a comment-only block under it) decodes to nil,
	// indistinguishable from the key being absent entirely, so this is
	// the only way to tell "explicitly present" from "never written" once
	// decoding has happened. Load's legacy-key refusal reads this rather
	// than a per-key bool that could silently fall out of sync with a
	// renamed or added field.
	presentKeys map[string]bool
}

// RoleConfig is one role's model/thinking choice inside a roles: block --
// see Roles' own doc comment. Like Model, it must never be able to name
// an upstream, an allowed path prefix, a build script, or an interpreter:
// it only ever names a models: key plus a Pi reasoning-effort level. See
// TestRoleNeverSetsUpstreamOrPath.
type RoleConfig struct {
	// Model is a models: key (see Config.Models), never a raw worker
	// model id or route. Required whenever a role is present.
	Model string `yaml:"model"`
	// Thinking is a Pi reasoning-effort level: "off", "minimal", "low",
	// "medium", "high", "xhigh", or "max". Empty means don't pass a level
	// at all. See ValidateRouting for the silent-clamp guard this feeds
	// into: Pi (0.84.4) sends "xhigh"/"max" only when the model's
	// extra_json declares them in thinking_level_map, and sends any
	// effort at all only when the model's reasoning: true -- otherwise it
	// silently clamps to "high" or sends nothing.
	Thinking string `yaml:"thinking,omitempty"`
	// Allowed is the closed set of models: entries this role may resolve
	// to -- Model above remains the default pick. Empty means "just
	// [Model]". ValidateRouting checks thinking against every entry, and
	// the review-independence rule below; nothing resolves a role's
	// Allowed into an actual per-request choice yet.
	Allowed []string `yaml:"allowed,omitempty"`
	// Harness is the coding-agent CLI this role's jobs run under: a
	// harness registry name (internal/harness), empty meaning "pi".
	Harness string `yaml:"harness,omitempty"`
	// AllowedHarnesses is the closed set of harnesses a request may pick for
	// this role (Harness remains the default). Empty means "just [Harness]".
	AllowedHarnesses []string `yaml:"allowed_harnesses,omitempty"`
	// AllowSharedModel waives the independence rule (a reviewer must not
	// share the builder's own model) for this role. Only consulted on
	// Review; meaningless, and never checked, on Planning/Execution.
	AllowSharedModel bool `yaml:"allow_shared_model,omitempty"`
	// Skills names the operator skills this role's worker sees: skill
	// folder NAMES only, never paths, resolved through Config.SkillDirs
	// (ResolveRoleSkills). A path-shaped entry is refused by ValidateSkills.
	Skills []string `yaml:"skills,omitempty"`
}

// Roles is the roles: session-config block: a model alias and thinking
// level per kind of work -- planning, execution, review -- instead of
// every stage sharing this session's one default model/route. Each field
// is independently optional; a nil field means "session default" for that
// role, the same "absent key changes nothing" contract every other
// Config/Settings field already gives (see Config's own doc comment).
// ValidateRoles enforces that a present role names a real model_aliases
// entry and, when it sets Thinking, that the alias actually supports it.
type Roles struct {
	Planning  *RoleConfig `yaml:"planning,omitempty"`
	Execution *RoleConfig `yaml:"execution,omitempty"`
	Review    *RoleConfig `yaml:"review,omitempty"`
}

// validThinkingLevels is the closed set RoleConfig.Thinking may name --
// Pi's own reasoning-effort vocabulary (0.84.4).
var validThinkingLevels = map[string]bool{
	"off": true, "minimal": true, "low": true, "medium": true,
	"high": true, "xhigh": true, "max": true,
}

// Settings is the fully-resolved, concrete-valued set of every knob a
// session config file can supply: the Tier-2 knobs (a CLI flag removed
// entirely by the flags-consolidate change, 2026-09-10) plus the handful
// of relay/registry-proxy values that already had no flag default of
// their own worth repeating (RelayAllowedPathPrefix and friends). Every
// field has a hard default equal to that knob's former CLI flag default
// (DefaultSettings), overridden only by the matching Config field of the
// same session config file (ApplySettings) -- never by a CLI flag, since
// none of these have one anymore. Values, not pointers: unlike Config, a
// Settings is always fully populated, so callers read it directly instead
// of dereferencing.
type Settings struct {
	SandboxImage       string
	MeterImage         string
	RegistryProxyImage string
	// ImageSourceRoot mirrors Config.ImageSourceRoot -- see that field's
	// own doc comment. Advisory only: a staleness check treats an empty
	// value as "cannot verify", never as "fresh" or "stale".
	ImageSourceRoot     string
	BuildAppMaxAttempts int
	VerifyMaxAttempts   int

	SandboxDocker    string
	SandboxUser      string
	SandboxWorkerUID int
	SandboxMemory    string
	SandboxCPUs      string
	SandboxTmpfsSize string

	ModelHostConcurrency     int
	MaxParallelJobs          int
	MeterMaxRequestBytes     int64
	MeterRequestsPerMinute   int
	MeterTokenBudget         int
	MeterTokenBudgetWindow   time.Duration
	MeterCostBudgetMicroUSD  int64
	MeterCostBudgetWindow    time.Duration
	MeterTokenCeiling        int
	MeterCostCeilingMicroUSD int64

	// RequestTokenBudget/RequestCostBudgetMicroUSD/MonthlyTokenBudget/
	// MonthlyCostBudgetMicroUSD mirror the identically-named Config
	// fields above -- see their own doc comments.
	RequestTokenBudget        int
	RequestCostBudgetMicroUSD int64
	MonthlyTokenBudget        int
	MonthlyCostBudgetMicroUSD int64

	RegistryProxy                      bool
	RegistryProxyConfigured            bool
	RegistryProxyNPMUpstream           string
	RegistryProxyPyPIUpstream          string
	RegistryProxyPyPIFilesUpstream     string
	RegistryProxyGoUpstream            string
	RegistryProxyGoSumDBUpstream       string
	RegistryProxyCacheBytes            int64
	RegistryProxyMaxObjectBytes        int64
	RegistryProxyMaxConcurrentUpstream int
	RegistryProxyUpstreamTimeout       time.Duration

	ComposeServices                  bool
	ComposeServicesConfigured        bool
	ComposeServicesMemory            string
	ComposeServicesCPUs              string
	ComposeServicesMaxServices       int
	ComposeServicesReadyTimeout      time.Duration
	ComposeServicesAllowedRegistries []string
	ComposeServicesWorkerEnv         map[string]string
	ComposeServicesRequireDigest     bool
	ComposeServicesConcurrency       int

	ReleaseProtectedPaths                 string
	ReleaseMaxFilesChanged                int
	ReleaseMaxInsertions                  int
	ReleaseRollbackPlan                   string
	ReleaseAllowOverrides                 bool
	ReleaseAllowDependencyLockfileChanges bool
	ReleaseAllowUnsandboxed               bool
	ReleaseAllowSkippedProjectCheck       bool

	// Roles mirrors Config.Roles -- see that field's own doc comment. nil
	// (the default, no roles: key present) means no role override
	// anywhere: every caller that consults it must behave exactly as it
	// did before this field existed.
	Roles *Roles

	// Routes/Models mirror Config.Routes/Config.Models -- see those
	// fields' own doc comments. These are the only schema: a launch that
	// needs a relay resolves its model/route through Routes/Models/
	// Roles.Allowed alone (ValidateRouting); a launch with no routes:/
	// models:/roles: at all (an offline build with an explicit
	// -build-app-script) needs no relay and is still valid.
	Routes map[string]Route
	Models map[string]Model

	// PresentKeys mirrors Config's own presentKeys -- every top-level YAML
	// key the source config file actually wrote, regardless of what its
	// decoded value came out as (a pointer field set to a YAML null still
	// counts). This is Load's only way to tell "explicitly set to the
	// empty/zero value" from "never set at all" once only a Settings is
	// in hand (Config's own pointer fields have already collapsed into
	// concrete Settings values by the time ApplySettings returns) -- a
	// single, generic mechanism rather than one hand-maintained bool per
	// legacy key, which could silently fall out of sync with a renamed or
	// added field. Every caller that resolves Settings from a real config
	// file gets this populated the same way, including `factoryd doctor`
	// (see that command's own doc comment on why it does not rebuild a
	// separate, poorer-fidelity Config of its own for this check).
	PresentKeys map[string]bool

	// Workspaces mirrors Config.Workspaces -- see that field's own doc
	// comment. Consulted only by POST /requests' workspace allowlist,
	// never by `factoryd submit`.
	Workspaces []string

	// SkillDirs mirrors Config.SkillDirs -- see that field's doc comment.
	SkillDirs []string
	// DesignGuideDirs mirrors Config.DesignGuideDirs.
	DesignGuideDirs []string
}

// EffectiveRelayCeilings returns the absolute, run-scoped relay ceilings
// (sandbox.RoutePolicy.TokenCeiling/CostCeilingMicroUSD) a relay launch
// actually uses: s.MeterTokenCeiling/MeterCostCeilingMicroUSD when
// positive, else 5x the corresponding window budget
// (MeterTokenBudget/MeterCostBudgetMicroUSD) -- the same "0 means
// unset, not disabled" default cmd/factoryd's own -meter-token-ceiling/
// -meter-cost-ceiling-micro-usd flag help documents (a relay ceiling
// must always be positive; the standalone relay binary's own CLI, where
// 0 does mean disabled, is a different surface). The single shared
// implementation every relay-launch site (the direct build, the
// drafting jobs, and modelrole.SelectRoute's own candidate-policy
// construction) calls, so the 5x multiplier has exactly one place to
// change.
func (s Settings) EffectiveRelayCeilings() (tokenCeiling int, costCeilingMicroUSD int64) {
	tokenCeiling = s.MeterTokenCeiling
	if tokenCeiling <= 0 {
		tokenCeiling = 5 * s.MeterTokenBudget
	}
	costCeilingMicroUSD = s.MeterCostCeilingMicroUSD
	if costCeilingMicroUSD <= 0 {
		costCeilingMicroUSD = 5 * s.MeterCostBudgetMicroUSD
	}
	return tokenCeiling, costCeilingMicroUSD
}

// DefaultSettings returns Settings populated with every knob's former CLI
// flag default -- omitting a session config file, or any field of one,
// changes nothing relative to the flag surface these replaced.
func DefaultSettings() Settings {
	return Settings{
		BuildAppMaxAttempts: 2,
		VerifyMaxAttempts:   2,

		SandboxDocker:    "docker",
		SandboxWorkerUID: sandbox.DefaultWorkerUID,
		SandboxMemory:    "4g",
		SandboxCPUs:      "2",
		SandboxTmpfsSize: "1g",

		MeterMaxRequestBytes:    1 << 20,
		MeterRequestsPerMinute:  60,
		MeterTokenBudget:        1_000_000,
		MeterTokenBudgetWindow:  time.Hour,
		MeterCostBudgetMicroUSD: 5_000_000,
		MeterCostBudgetWindow:   time.Hour,
		ModelHostConcurrency:    modelhost.DefaultConcurrency,
		MaxParallelJobs:         DefaultMaxParallelJobs,

		RegistryProxyNPMUpstream:           "https://registry.npmjs.org",
		RegistryProxyPyPIUpstream:          "https://pypi.org/simple",
		RegistryProxyPyPIFilesUpstream:     "https://files.pythonhosted.org",
		RegistryProxyGoUpstream:            "https://proxy.golang.org",
		RegistryProxyGoSumDBUpstream:       "https://sum.golang.org",
		RegistryProxyCacheBytes:            1 << 30,
		RegistryProxyMaxObjectBytes:        256 << 20,
		RegistryProxyMaxConcurrentUpstream: 16,
		RegistryProxyUpstreamTimeout:       60 * time.Second,

		ComposeServicesMemory:            "2g",
		ComposeServicesCPUs:              "1",
		ComposeServicesMaxServices:       8,
		ComposeServicesReadyTimeout:      3 * time.Minute,
		ComposeServicesAllowedRegistries: []string{"docker.io/library/"},
		ComposeServicesConcurrency:       1,
	}
}

// ApplySettings overlays c's present fields onto s, returning the result.
// An absent Config field (nil pointer) leaves the corresponding s field
// untouched -- the same "only present keys apply" rule Load's KnownFields
// parsing already gives every other field. A malformed duration string is
// an error naming the offending key, not a silently-ignored field.
func (c *Config) ApplySettings(s Settings) (Settings, error) {
	str := func(dst *string, v *string) {
		if v != nil {
			*dst = *v
		}
	}
	boolean := func(dst *bool, v *bool) {
		if v != nil {
			*dst = *v
		}
	}
	integer := func(dst *int, v *int) {
		if v != nil {
			*dst = *v
		}
	}
	integer64 := func(dst *int64, v *int64) {
		if v != nil {
			*dst = *v
		}
	}
	duration := func(dst *time.Duration, name string, v *string) error {
		if v == nil {
			return nil
		}
		d, err := time.ParseDuration(*v)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		*dst = d
		return nil
	}
	// nonNegativeInteger/nonNegativeInteger64 back the four
	// request/monthly budget keys below: unlike every other *int/*int64
	// key in this function (which silently accepts a negative value that
	// then just behaves like 0 downstream), a negative budget has no
	// sane reading -- 0/absent already means "unlimited" -- so it is
	// refused here rather than silently coerced into either meaning.
	nonNegativeInteger := func(dst *int, name string, v *int) error {
		if v == nil {
			return nil
		}
		if *v < 0 {
			return fmt.Errorf("%s: must not be negative, got %d", name, *v)
		}
		*dst = *v
		return nil
	}
	nonNegativeInteger64 := func(dst *int64, name string, v *int64) error {
		if v == nil {
			return nil
		}
		if *v < 0 {
			return fmt.Errorf("%s: must not be negative, got %d", name, *v)
		}
		*dst = *v
		return nil
	}

	str(&s.SandboxImage, c.SandboxImage)
	str(&s.MeterImage, c.MeterImage)
	str(&s.RegistryProxyImage, c.RegistryProxyImage)
	str(&s.ImageSourceRoot, c.ImageSourceRoot)
	str(&s.SandboxDocker, c.SandboxDocker)
	str(&s.SandboxUser, c.SandboxUser)
	integer(&s.SandboxWorkerUID, c.SandboxWorkerUID)
	str(&s.SandboxMemory, c.SandboxMemory)
	str(&s.SandboxCPUs, c.SandboxCPUs)
	str(&s.SandboxTmpfsSize, c.SandboxTmpfsSize)
	integer(&s.BuildAppMaxAttempts, c.BuildAppMaxAttempts)
	integer(&s.VerifyMaxAttempts, c.VerifyMaxAttempts)

	integer(&s.ModelHostConcurrency, c.ModelHostConcurrency)
	if c.MaxParallelJobs != nil {
		if *c.MaxParallelJobs < 1 {
			return s, fmt.Errorf("max_parallel_jobs: must be at least 1, got %d", *c.MaxParallelJobs)
		}
		s.MaxParallelJobs = *c.MaxParallelJobs
	}
	integer64(&s.MeterMaxRequestBytes, c.MeterMaxRequestBytes)
	integer(&s.MeterRequestsPerMinute, c.MeterRequestsPerMinute)
	integer(&s.MeterTokenBudget, c.MeterTokenBudget)
	if err := duration(&s.MeterTokenBudgetWindow, "meter_token_budget_window", c.MeterTokenBudgetWindow); err != nil {
		return s, err
	}
	integer64(&s.MeterCostBudgetMicroUSD, c.MeterCostBudgetMicroUSD)
	if err := duration(&s.MeterCostBudgetWindow, "meter_cost_budget_window", c.MeterCostBudgetWindow); err != nil {
		return s, err
	}
	integer(&s.MeterTokenCeiling, c.MeterTokenCeiling)
	integer64(&s.MeterCostCeilingMicroUSD, c.MeterCostCeilingMicroUSD)
	if err := nonNegativeInteger(&s.RequestTokenBudget, "request_token_budget", c.RequestTokenBudget); err != nil {
		return s, err
	}
	if err := nonNegativeInteger64(&s.RequestCostBudgetMicroUSD, "request_cost_budget_micro_usd", c.RequestCostBudgetMicroUSD); err != nil {
		return s, err
	}
	if err := nonNegativeInteger(&s.MonthlyTokenBudget, "monthly_token_budget", c.MonthlyTokenBudget); err != nil {
		return s, err
	}
	if err := nonNegativeInteger64(&s.MonthlyCostBudgetMicroUSD, "monthly_cost_budget_micro_usd", c.MonthlyCostBudgetMicroUSD); err != nil {
		return s, err
	}

	boolean(&s.RegistryProxy, c.RegistryProxy)
	s.RegistryProxyConfigured = c.RegistryProxy != nil
	str(&s.RegistryProxyNPMUpstream, c.RegistryProxyNPMUpstream)
	str(&s.RegistryProxyPyPIUpstream, c.RegistryProxyPyPIUpstream)
	str(&s.RegistryProxyPyPIFilesUpstream, c.RegistryProxyPyPIFilesUpstream)
	str(&s.RegistryProxyGoUpstream, c.RegistryProxyGoUpstream)
	str(&s.RegistryProxyGoSumDBUpstream, c.RegistryProxyGoSumDBUpstream)
	integer64(&s.RegistryProxyCacheBytes, c.RegistryProxyCacheBytes)
	integer64(&s.RegistryProxyMaxObjectBytes, c.RegistryProxyMaxObjectBytes)
	integer(&s.RegistryProxyMaxConcurrentUpstream, c.RegistryProxyMaxConcurrentUpstream)
	if err := duration(&s.RegistryProxyUpstreamTimeout, "registry_proxy_upstream_timeout", c.RegistryProxyUpstreamTimeout); err != nil {
		return s, err
	}

	boolean(&s.ComposeServices, c.ComposeServices)
	s.ComposeServicesConfigured = c.ComposeServices != nil
	str(&s.ComposeServicesMemory, c.ComposeServicesMemory)
	str(&s.ComposeServicesCPUs, c.ComposeServicesCPUs)
	integer(&s.ComposeServicesMaxServices, c.ComposeServicesMaxServices)
	if err := duration(&s.ComposeServicesReadyTimeout, "compose_services_ready_timeout", c.ComposeServicesReadyTimeout); err != nil {
		return s, err
	}
	if c.ComposeServicesAllowedRegistries != nil {
		s.ComposeServicesAllowedRegistries = c.ComposeServicesAllowedRegistries
	}
	if c.ComposeServicesWorkerEnv != nil {
		s.ComposeServicesWorkerEnv = maps.Clone(c.ComposeServicesWorkerEnv)
	}
	boolean(&s.ComposeServicesRequireDigest, c.ComposeServicesRequireDigest)
	integer(&s.ComposeServicesConcurrency, c.ComposeServicesConcurrency)

	str(&s.ReleaseProtectedPaths, c.ReleaseProtectedPaths)
	integer(&s.ReleaseMaxFilesChanged, c.ReleaseMaxFilesChanged)
	integer(&s.ReleaseMaxInsertions, c.ReleaseMaxInsertions)
	str(&s.ReleaseRollbackPlan, c.ReleaseRollbackPlan)
	boolean(&s.ReleaseAllowOverrides, c.ReleaseAllowOverrides)
	boolean(&s.ReleaseAllowDependencyLockfileChanges, c.ReleaseAllowDependencyLockfileChanges)
	boolean(&s.ReleaseAllowUnsandboxed, c.ReleaseAllowUnsandboxed)
	boolean(&s.ReleaseAllowSkippedProjectCheck, c.ReleaseAllowSkippedProjectCheck)

	if c.Roles != nil {
		s.Roles = c.Roles
	}
	if c.Routes != nil {
		s.Routes = c.Routes
	}
	if c.Models != nil {
		s.Models = c.Models
	}
	if c.Workspaces != nil {
		s.Workspaces = c.Workspaces
	}
	if c.SkillDirs != nil {
		s.SkillDirs = c.SkillDirs
	}
	if c.DesignGuideDirs != nil {
		s.DesignGuideDirs = c.DesignGuideDirs
	}
	// PresentKeys carries every top-level YAML key Load actually saw
	// forward -- see Settings.PresentKeys' own doc comment. A Settings
	// built without going through Load (a hand-constructed Config in a
	// test, or DefaultSettings alone) simply has no PresentKeys, which is
	// correct: there is no source YAML for any key to have been "present"
	// in.
	if c.presentKeys != nil {
		s.PresentKeys = c.presentKeys
	}

	return s, nil
}

// DefaultReleaseMaxFilesChanged/DefaultReleaseMaxInsertions/
// DefaultReleaseRollbackPlan are the release-policy values a fresh
// session config should carry so a first PR isn't silently denied. The
// shipped zero-value Settings (see applySettings above:
// ReleaseMaxFilesChanged/ReleaseMaxInsertions/ReleaseRollbackPlan default
// to the zero value) deny every PR unconditionally --
// internal/release.MergePolicyCheck treats 0 files allowed and an empty
// rollback plan as always-deny. These are the single source of truth for
// that starting point: both `factoryd quickstart` (a freshly generated
// config) and `factoryd init-config` (the commented Example scaffold
// below, and the same backfill quickstart applies when it reuses an
// existing config missing one of these three keys -- see
// quickstartBackfillReleaseDefaults in cmd/factoryd/quickstart.go) read
// them from here rather than keeping two copies that could drift. The
// numbers are deliberately generous enough to let a normal
// single-ticket PR through
// (a ticket's own diff is rarely more than a few files) while still being
// a real, operator-visible bound rather than "unlimited" -- an operator
// who wants a tighter or looser limit edits config.yml directly, same as
// any other session config key.
const (
	DefaultReleaseMaxFilesChanged = 25
	DefaultReleaseMaxInsertions   = 1000
	// DefaultReleaseRollbackPlan only needs to be non-empty --
	// MergePolicyCheck checks presence, not content (see its own doc
	// comment: "Checking that reference is present does not implement
	// rollback"). `git revert` on the merge commit is the generically
	// correct rollback for any PR this pipeline itself opens.
	DefaultReleaseRollbackPlan = "git revert the merge commit on main"
)

// DefaultMaxParallelJobs is max_parallel_jobs' default: two builds fit a
// 4 GiB Docker VM next to Temporal. A build's sandbox_memory is a ceiling; a
// typical Go build uses about 1.3 GiB (peak 1.26 GiB measured 2026-10-04, a
// small Python build 0.2 GiB).
const DefaultMaxParallelJobs = 2

// Example is the commented config `factoryd init-config` writes and the
// no-config message shows. sandbox_image's digest is a placeholder: `make
// install`/`make sandbox-image` prints a real one, and `factoryd
// configure-images` writes it here -- there is no published image to
// default to instead; every image is built from source.
// registry_proxy_image is omitted entirely -- see the comment below. The
// model id must come from GET /v1/models. The
// release_* keys below are DefaultReleaseMaxFilesChanged/
// DefaultReleaseMaxInsertions/DefaultReleaseRollbackPlan spelled out as
// YAML -- kept in sync with those constants by TestExampleMatchesReleaseDefaults.
const Example = `# factoryd session config: the machine-side values every worker needs.
# Each key mirrors a ` + "`factoryd worker`" + ` flag; a flag given on the command
# line still overrides the value here.
# The coding-agent CLI is chosen per role: roles.<role>.harness (pi by
# default; pifork is opt-in and requires a locally-built, digest-pinned
# pifork worker image in sandbox_image).
sandbox_image: localhost:5050/buildgate-worker@sha256:<digest>
# registry_proxy_image is omitted here: there is no built-in default (make
# install builds every image at once), and it only matters with
# registry_proxy: true.
# routes: names each upstream/credential pairing this session may reach;
# models: names each model id and which route(s) it may resolve through;
# roles: picks a model (and, for a reasoning model, a thinking level) for
# planning/execution/review. See USAGE_REFERENCE.md's own routes:/models:/
# roles: section for the full schema.
routes:
  local:
    # Bare API root, no path: the relay appends the worker's own request path.
    upstream: http://100.x.y.z:8080
    allowed_path_prefix: /v1
    worker_base_path: /v1
    allow_plaintext_upstream: true
    allow_no_credential: true
    # Default is x-api-key; a credentialed OpenAI-compatible endpoint usually wants Authorization instead (sent as "Bearer <key>").
    # credential_header: Authorization
    # credential_env defaults to ANTHROPIC_API_KEY; set only to have the daemon read a different env var for this route's credential.
    # credential_env: ANTHROPIC_API_KEY
  # Route the relay through GitHub Copilot instead of a static credential -- see USAGE.md §11.
  # copilot:
  #   credential_mode: github-copilot
  #   github_token_file: ~/.pi/agent/auth.json
  #   # github_token_key defaults to "github-copilot" (pi's own auth.json provider id); set only if your pi fork registers it under another id.
  #   # github_token_key: github-copilot
models:
  local-model:
    id: <id from GET /v1/models, never hardcoded>
    # api: openai-completions
    routes: [local]
    context_window: 131072
roles:
  planning: { model: local-model }
  execution: { model: local-model }
  # Optional worker skills (none by default): uncomment to give build rounds
  # buildgate's built-in skills, or add skill_dirs: [<folder>] for your own
  # (a same-name skill there overrides the built-in). Built in:
  # buildgate-tdd, buildgate-diagnosing, go-service, typescript-service,
  # flutter-app, kafka-processing, postgres-change, temporal-go.
  # execution: { model: local-model, skills: [buildgate-tdd, buildgate-diagnosing, go-service] }
  # allow_shared_model waives the rule that review must not resolve to the
  # same backend as execution -- this example has only one model to pick.
  review: { model: local-model, allow_shared_model: true }
# A single-instance model host (a local/private/Tailscale route upstream)
# is locked host-wide by default so two independent factoryd runs never
# call it concurrently -- one job at a time. 0 disables the lock; a remote
# multi-tenant SaaS upstream (chatgpt.com, api.githubcopilot.com,
# api.anthropic.com) is unlocked by default already, see internal/modelhost.
# model_host_concurrency: 1
# How many model jobs and builds factoryd worker runs at once, across
# all requests. Each build's sandbox takes up to sandbox_memory.
# max_parallel_jobs: 2
# Folders holding team design guides (<name>.md); a repository selects one
# with design_guide: <name> in its .factory.yml.
# design_guide_dirs: [~/code/team-standards]
registry_proxy: true
# egress_ca_bundle: /path/to/corp-ca.pem
# open_pull_request: true
# data_dir: data
# workspaces: absolute paths POST /requests (the console's "New request"
# form) may submit against, in addition to any workspace an existing
# request already names. Only consulted by that HTTP route, never by
# ` + "`factoryd submit`" + `, which can already target any local path.
# workspaces:
#   - /home/you/code/some-service
# Release policy: without these three, MergePolicyCheck denies every PR
# (0 files allowed, empty rollback plan) -- see DefaultReleaseMaxFilesChanged
# and friends above.
release_max_files_changed: 25
release_max_insertions: 1000
release_rollback_plan: "git revert the merge commit on main"
`

// DefaultPaths lists where a config is written and, absent a profile, looked
// for, in order: $XDG_CONFIG_HOME/factoryd/config.yml (~/.config when
// unset), then ~/.factory/config.yml. The first is where init-config
// writes. Readers use ResolvePath, which puts the active profile first.
func DefaultPaths() []string {
	home, _ := os.UserHomeDir()
	return []string{
		filepath.Join(ConfigDir(), "config.yml"),
		filepath.Join(home, ".factory", "config.yml"),
	}
}

// LoadDefault loads the config ResolvePath finds. found is false, with a
// nil error, when there is none.
func LoadDefault() (cfg *Config, path string, found bool, err error) {
	path, found, err = ResolvePath()
	if err != nil || !found {
		return nil, path, false, err
	}
	cfg, err = Load(path)
	return cfg, path, err == nil, err
}

// Load parses path. A key that moved under routes:/models:/roles: in a
// past schema version (legacyRoutingKeys) is refused with a message
// naming the key and pointing at the current schema, rather than the
// generic "field X not found" KnownFields would otherwise produce for a
// deleted field -- this is an error message, not a shim: no such key is
// ever read. Any other unknown key is that generic error, naming it.
func Load(path string) (*Config, error) {
	if resolved := ResolveArg(path); resolved != path {
		path = resolved
		if _, err := os.Stat(path); err != nil {
			return nil, fmt.Errorf("profile %s: %w; list profiles with `factoryd use`", filepath.Base(path), err)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	presentKeys := yamlTopLevelKeys(data)
	for _, key := range legacyRoutingKeys {
		if presentKeys[key] {
			return nil, fmt.Errorf("parse %s: %s: %s", path, key, legacyRoutingKeyHintFor(key))
		}
	}
	if modelName, key, found := retiredModelPriceKey(data); found {
		return nil, fmt.Errorf("parse %s: models.%s.%s: moved to internal/prices/prices.yml (see USAGE_REFERENCE.md \"Model prices\")", path, modelName, key)
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var cfg Config
	if err := dec.Decode(&cfg); err != nil && err != io.EOF {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	cfg.presentKeys = presentKeys
	return &cfg, nil
}

// yamlTopLevelKeys returns every key in data's top-level YAML mapping,
// regardless of that key's own value -- including a null value (a bare
// `relay_upstream:` line with nothing after it, or a comment-only block
// under it), which a plain struct-field decode cannot distinguish from
// the key being absent entirely (both decode a pointer field to nil).
// Used only by Load, for Config.presentKeys.
func yamlTopLevelKeys(data []byte) map[string]bool {
	var node yaml.Node
	if err := yaml.Unmarshal(data, &node); err != nil || len(node.Content) == 0 {
		return nil
	}
	doc := node.Content[0]
	if doc.Kind != yaml.MappingNode {
		return nil
	}
	keys := make(map[string]bool, len(doc.Content)/2)
	for i := 0; i+1 < len(doc.Content); i += 2 {
		keys[doc.Content[i].Value] = true
	}
	return keys
}
