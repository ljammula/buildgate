package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"buildgate/internal/api"
	"buildgate/internal/projectconfig"
	"buildgate/internal/release"
	"buildgate/internal/run"
	"buildgate/internal/sessionconfig"
)

// apiProjectChecker is `factoryd serve`'s own api.ProjectChecker
// implementation, wired in via api.WithProjectChecker (see serveMain). It
// reports the exact verdict a real API-started run's own project-
// bootstrap preflight (runProjectBootstrapCheck, called from
// runMainWithReady) would reach against the same Workspace/Ticket --
// same artifact paths (projectBootstrapArtifactPaths), same ticket
// resolution (resolvePiTicketPath) -- but through
// evaluateProjectBootstrapChecks directly, never runProjectBootstrapCheck
// itself, so this preview writes no durable ProjectCheckRecord and never
// returns a run-aborting error for a failed check (see that function's own
// doc comment): an operator previewing a project before committing to a
// real run is asking a question, not making one.
//
// architectureRequiredSections is deliberately left nil (policy.
// ArchitectureStructure's own default) rather than threaded through from
// an -architecture-required-sections-style flag: apiStartStarter itself
// does not forward that flag to a spawned API-started run either (it has
// no such flag at all), so a real run against this same request would
// also check against the default sections -- using anything else here
// would make this preview report a verdict a real run would not actually
// reach.
func apiProjectChecker() api.ProjectChecker {
	return func(_ context.Context, req api.ProjectCheckRequest) (api.ProjectCheckResponse, error) {
		cfg, _, err := projectconfig.Load(req.Workspace)
		if err != nil {
			return api.ProjectCheckResponse{}, fmt.Errorf("load %s: %w", projectconfig.FileName, err)
		}
		profile, err := effectivePreflightProfile(req.PreflightProfile, cfg)
		if err != nil {
			return api.ProjectCheckResponse{}, err
		}
		workspaceAbs, err := filepath.Abs(req.Workspace)
		if err != nil {
			return api.ProjectCheckResponse{}, fmt.Errorf("resolve workspace: %w", err)
		}
		specPath, contractPath, architecturePath := projectBootstrapArtifactPaths(workspaceAbs)

		var ticketPath string
		var ticketNumber int
		if strings.TrimSpace(req.Ticket) != "" {
			ticketPath, ticketNumber, err = resolvePiTicketPath(req.Workspace, req.Ticket, "")
			if err != nil {
				return api.ProjectCheckResponse{}, fmt.Errorf("resolve ticket: %w", err)
			}
		}

		// requestTicket is always false here: POST /projects/check previews
		// a plain factoryd <run>/API-started run's own preflight, which can
		// never set -request-ticket (see that flag's own doc comment) --
		// the request pipeline's own tickets are never previewed through
		// this endpoint.
		results, allPassed := evaluateProjectBootstrapChecks(specPath, contractPath, architecturePath, ticketPath, ticketNumber, nil, profile, false)
		checks := make([]api.ProjectCheckResult, len(results))
		for i, r := range results {
			checks[i] = api.ProjectCheckResult{Check: r.Check, Path: r.Path, Passed: r.Passed, Reasons: r.Reasons, Advisory: r.Advisory}
		}
		return api.ProjectCheckResponse{Passed: allPassed, Checks: checks}, nil
	}
}

// apiProjectStatsProvider is `factoryd serve`'s own api.ProjectStatsProvider
// implementation, wired in via api.WithProjectStatsProvider (see
// serveMain). Scans every durable run record under dataDir belonging to
// project -- the same run.Run.Project field getProjectRelease's own
// GET /projects/{project}/release already trusts as the one authoritative
// project identity -- and computes the same override-rate figure `factoryd override-rate`
// (overrideRateMain) already reports repo-wide, promoted to a per-project
// number, plus quarantine-cause and accepted-cost breakdowns that CLI tool
// does not compute at all. Deliberately a separate implementation from
// overrideRateMain rather than a refactor of it: that command's own
// fixed-format stdout output and repo-wide scan are already tested and
// working, and project-filtering it would change what it reports for
// every existing caller of the CLI, not just add a new one.
func apiProjectStatsProvider(dataDir string) api.ProjectStatsProvider {
	return func(_ context.Context, project string) (api.ProjectStats, error) {
		stats := api.ProjectStats{Project: project}
		entries, err := os.ReadDir(filepath.Join(dataDir, "runs"))
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return stats, nil
			}
			return api.ProjectStats{}, fmt.Errorf("read runs dir: %w", err)
		}

		var acceptedCosts []int64
		var acceptedCostSubscriptionBilled bool
		var acceptedTokens []int64
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			r, err := run.Load(dataDir, entry.Name())
			if err != nil {
				// errors.Is, not os.IsNotExist -- run.Load wraps os.ReadFile's
				// error in fmt.Errorf("read run: %w", ...), and os.IsNotExist
				// does not see through a generic %w wrapper (only the
				// *PathError/*LinkError/*SyscallError shapes it special-cases
				// directly), so it never matched here. Found live during
				// Phase 4 console validation: a non-run directory under
				// data/runs/ -- exactly what the request pipeline's own
				// relay heartbeat bookkeeping legitimately leaves behind
				// under a request's id, with no run.json -- made every
				// GET /projects/{project}/stats call (and so
				// project_stats_screen/ops_screen) fail with a 500, even
				// though GET /runs's own listRuns handler already gets this
				// right via errors.Is right next to it in
				// internal/api/server.go.
				if errors.Is(err, os.ErrNotExist) {
					continue // a run directory that never got a durable run.json
				}
				return api.ProjectStats{}, fmt.Errorf("load run %q: %w", entry.Name(), err)
			}
			// r.Project falls back to the same fresh derivation from
			// r.ProjectPath the project-list endpoint already applies
			// (internal/api/server.go's listProjects) for a run recorded
			// before that field existed -- found via Codex review, PR #64:
			// without it, every run predating r.Project silently dropped
			// out of a project's own stats, and the project-list endpoint
			// itself derives the exact identifier this comparison expects,
			// so a project that endpoint lists could still report zero
			// runs/no data here.
			if release.ProjectOf(r) != project {
				continue
			}
			stats.TotalRuns++
			switch r.State {
			case run.StateAccepted:
				stats.Accepted++
				if len(r.Overrides) > 0 {
					stats.AcceptedViaOverride++
				}
				var cost, tokens int64
				for _, attempt := range r.Attempts {
					cost += attempt.RelayConsumedCostMicroUSD
					tokens += attempt.RelayConsumedInputTokens + attempt.RelayConsumedOutputTokens
				}
				acceptedCosts = append(acceptedCosts, cost)
				acceptedTokens = append(acceptedTokens, tokens)
				if run.SubscriptionBilled(r.Attempts) {
					acceptedCostSubscriptionBilled = true
				}
			case run.StateHalted:
				stats.Halted++
			case run.StateQuarantined:
				for _, gate := range r.GateResults {
					if !gate.Passed {
						if stats.QuarantinedByCause == nil {
							stats.QuarantinedByCause = map[string]int{}
						}
						stats.QuarantinedByCause[gate.Check]++
					}
				}
			}
		}

		if stats.Accepted > 0 {
			percent := (100 * stats.AcceptedViaOverride) / stats.Accepted
			stats.OverrideRatePercent = &percent
		}
		if median := medianInt64(acceptedCosts); median != nil {
			stats.MedianAcceptedCostMicroUSD = median
			stats.MedianAcceptedCostSubscriptionBilled = acceptedCostSubscriptionBilled
		}
		stats.MedianAcceptedTokens = medianInt64(acceptedTokens)
		return stats, nil
	}
}

// medianInt64 returns the median of values, or nil for an empty slice --
// distinct from returning 0, which a caller (ProjectStats.
// MedianAcceptedCostMicroUSD's own JSON encoding) must not confuse with
// "computed, and the median really is zero" (every accepted run credential-
// free, say). Does not mutate values: sorts a copy.
func medianInt64(values []int64) *int64 {
	if len(values) == 0 {
		return nil
	}
	sorted := append([]int64(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	mid := len(sorted) / 2
	var median int64
	if len(sorted)%2 == 1 {
		median = sorted[mid]
	} else {
		median = (sorted[mid-1] + sorted[mid]) / 2
	}
	return &median
}

func apiStartStarter(dp *deps, dataDir string, inFlight *sync.WaitGroup, releasePolicy release.MergePolicy, sandboxLimits sandboxResourceLimits, sandboxPolicy apiSandboxPolicy) api.RunStarter {
	return func(ctx context.Context, req api.StartRequest) (*run.Run, error) {
		workspace := req.Workspace
		if workspace == "" {
			workspace = req.WorkspacePath
		}
		spec := req.Spec
		if spec == "" {
			spec = req.SpecPath
		}
		if req.Ticket == "" || workspace == "" || spec == "" {
			return nil, fmt.Errorf("%w: ticket, workspace, and spec are required", api.ErrInvalidStartRequest)
		}
		// Validated here, before the spawned runMainWithReady's own copy
		// of this check ever runs (see -preflight-profile's own doc
		// comment): that copy's error is a plain error, not wrapped in
		// api.ErrInvalidStartRequest, so an invalid value would otherwise
		// fall through to the generic 500 "start run" branch below --
		// exactly the swallowed-diagnostic bug errProjectBootstrapCheckFailed
		// exists to prevent, for a different field.
		if req.PreflightProfile != "" && req.PreflightProfile != preflightProfileBrownfield {
			return nil, fmt.Errorf("%w: preflight_profile must be \"\" or %q, got %q", api.ErrInvalidStartRequest, preflightProfileBrownfield, req.PreflightProfile)
		}
		// sandbox_docker/sandbox_user are rejected outright now, not merely
		// narrowed: flags-consolidate (2026-09-10) made both
		// session-config-only, so runMainWithReady defines no
		// -sandbox-docker/-sandbox-user flag for this starter to forward
		// them through and resolves both from the daemon's own
		// sessionconfig.Settings instead. Forwarding them anyway -- which
		// this function did until this was caught in review -- made the
		// spawned run die at flag.Parse ("flag provided but not defined")
		// on any request that set either field. Rejecting beats silently
		// dropping: sandbox_docker is the literal executable
		// internal/sandbox.Run invokes and sandbox_user the identity the
		// container's code runs as, so a caller who named either must learn
		// it had no effect rather than get a run configured differently
		// from what they asked for. (This also subsumes the narrower
		// "must be \"docker\"" check a 2026-09-04 Opus review added here:
		// an authenticated API token holder must never be able to name a
		// host executable this trusted process then runs.)
		if req.SandboxDocker != "" {
			return nil, fmt.Errorf("%w: sandbox_docker is no longer accepted on POST /runs -- it is session-config only (sandbox_docker in the daemon's own config.yml), identically for every run this daemon starts", api.ErrInvalidStartRequest)
		}
		if req.SandboxUser != "" {
			return nil, fmt.Errorf("%w: sandbox_user is no longer accepted on POST /runs -- it is session-config only (sandbox_user in the daemon's own config.yml), identically for every run this daemon starts", api.ErrInvalidStartRequest)
		}
		// SandboxImage names the Docker image the sandboxed worker runs --
		// digest-pinning (checked downstream, not here) only proves the
		// content is immutable, not that it's trustworthy. Before this
		// check, any digest-pinned image from any registry was accepted
		// from POST /runs, which lets an authenticated API token holder
		// supply an image whose entrypoint they fully control, get
		// /workspace bind-mounted rw inside it. Container hardening still applies (this is Medium,
		// not High, in the review below), but the image is the one input
		// deciding what code runs inside the boundary at all, the same
		// shape of gap sandbox_docker was narrowed for just above: an
		// authenticated API caller is untrusted the same way
		// sandbox_docker's caller is. An empty -api-allowed-sandbox-images
		// (the default) accepts only this daemon's own configured sandbox
		// image (see apiSandboxPolicy.imageAllowed), nothing if it has
		// none configured either -- so an operator who wants POST /runs to
		// reach any other image must explicitly allowlist it; the CLI's
		// own -sandbox-image, passed
		// directly rather than through this API, remains a fully trusted
		// context and is unaffected (found via a containment review).
		// resolveAPIImage applies sandboxPolicy's own
		// -api-default-sandbox-image (already proven allowlisted at daemon
		// startup, see serveMain) whenever the request itself leaves
		// SandboxImage empty, and enforces the same allowlist either way --
		// see its own doc comment.
		sandboxImage, err := resolveAPIImage(req, sandboxPolicy)
		if err != nil {
			return nil, err
		}
		// Each role's harness comes from the daemon's own session config
		// (a start request never chooses one). A role whose harness needs its
		// own worker image needs the matching allowlisted input.
		harnessDescriptors, err := harnessRoleSets(sessionconfig.Settings{Roles: sandboxPolicy.roles})
		if err != nil {
			return nil, fmt.Errorf("%w: %v", api.ErrInvalidStartRequest, err)
		}
		for _, role := range sortedMapKeys(harnessDescriptors) {
			for _, d := range harnessDescriptors[role] {
				if d.RequiresSandboxImage && sandboxImage == "" {
					return nil, fmt.Errorf("%w: roles.%s: harness %s requires an allowlisted digest-pinned sandbox_image (configure -api-default-sandbox-image or include sandbox_image in the request)", api.ErrInvalidStartRequest, role, d.Name)
				}
			}
		}
		id := req.ID
		if id == "" {
			// CLI invocations get process-level uniqueness from their PID. API
			// starts share one long-lived PID, so include nanoseconds as well or
			// two requests for the same ticket in one second collide.
			id = fmt.Sprintf("%s-%s-%d", req.Ticket, time.Now().Format("20060102-150405.000000000"), os.Getpid())
		}
		if id == "." || id == ".." || strings.ContainsAny(id, `/\`) {
			return nil, fmt.Errorf("%w: invalid run id %q", api.ErrInvalidStartRequest, id)
		}
		// AllowUnsandboxed no longer has any effect: Docker containment is
		// unconditional, with no host-execution opt-out anywhere in
		// factoryd (the CLI's own -allow-unsandboxed is gone too). The
		// field stays on the request struct only so a caller that still
		// sends it gets this explicit rejection instead of silently having
		// it ignored, matching SandboxDocker/SandboxUser's own treatment
		// above.
		if req.AllowUnsandboxed {
			return nil, fmt.Errorf("%w: allow_unsandboxed is no longer accepted on POST /runs -- Docker containment is unconditional, with no host-execution opt-out", api.ErrInvalidStartRequest)
		}
		if req.Repository != "" && req.TemporalAddress == "" {
			return nil, fmt.Errorf("%w: repository requires temporal_address", api.ErrInvalidStartRequest)
		}
		// Found via a GitHub Codex App review round, 2026-08-29: the id
		// check above only guards against an unsafe *path* component (no
		// "/", no ".." as a whole segment) -- not against an illegal git
		// ref, which is what wsisolation.Prepare actually derives id into
		// ("factoryd/<id>") when isolation is on. An id like "foo..bar" is
		// a perfectly fine path component but two consecutive dots make it
		// an invalid ref, which Prepare only discovers via
		// `git check-ref-format` deep in the background run this starter
		// already launched. Since onReady fires (and POST /runs returns
		// 202) before Prepare ever runs, the caller has already been told
		// the run started by the time it merely halts -- this validates
		// the ref up front, the same way runMainWithReady itself does not
		// have to (a CLI invocation only reports failure once, synchronously,
		// via its own exit code).
		branch := "factoryd/" + id
		if out, err := exec.Command("git", "check-ref-format", "--branch", branch).CombinedOutput(); err != nil {
			return nil, fmt.Errorf("%w: run id %q is not usable as an isolated branch name (%s)", api.ErrInvalidStartRequest, id, strings.TrimSpace(string(out)))
		}
		if req.Timeout != "" {
			if _, err := time.ParseDuration(req.Timeout); err != nil {
				return nil, fmt.Errorf("%w: invalid timeout: %v", api.ErrInvalidStartRequest, err)
			}
		}
		if req.FullSuiteCadence < 0 {
			return nil, fmt.Errorf("%w: full_suite_cadence must be zero or a positive integer", api.ErrInvalidStartRequest)
		}

		args := []string{"-ticket", req.Ticket, "-workspace", workspace, "-spec", spec, "-data-dir", dataDir, "-run-id", id}
		appendStringFlag := func(name, value string) {
			if value != "" {
				args = append(args, name, value)
			}
		}
		appendIntFlag := func(name string, value int) {
			if value != 0 {
				args = append(args, name, strconv.Itoa(value))
			}
		}
		appendStringFlag("-build-app-interpreter", req.BuildAppInterpreter)
		appendStringFlag("-build-app-script", req.BuildAppScript)
		appendIntFlag("-build-app-max-attempts", req.BuildAppMaxAttempts)
		appendIntFlag("-max-rounds", req.MaxRounds)
		appendIntFlag("-timeout-minutes", req.TimeoutMinutes)
		appendStringFlag("-timeout", req.Timeout)
		appendStringFlag("-verify-command", req.VerifyCommand)
		appendIntFlag("-verify-max-attempts", req.VerifyMaxAttempts)
		appendStringFlag("-full-suite-command", req.FullSuiteCommand)
		appendIntFlag("-full-suite-cadence", req.FullSuiteCadence)
		if req.RequireDeclaredScope {
			args = append(args, "-require-declared-scope")
		}
		appendStringFlag("-temporal-address", req.TemporalAddress)
		appendStringFlag("-repository", req.Repository)
		if req.SkipProjectCheck {
			args = append(args, "-skip-project-check")
		}
		appendStringFlag("-preflight-profile", req.PreflightProfile)
		if req.OpenPullRequest {
			args = append(args, "-open-pull-request")
		}
		appendStringFlag("-sandbox-image", sandboxImage)
		// releasePolicy/sandboxLimits are no longer forwarded as argv flags
		// here: `factoryd <run>` (runMainWithReady) removed every -release-*
		// and -sandbox-memory/-cpus/-pids/-tmpfs-size/-worker-uid flag
		// (flags-consolidate, 2026-09-10) in favor of resolveSettings's
		// sessionconfig.Settings. Both are daemon-static for the life of
		// this process -- identical for every request, exactly like before
		// -- so serveMain now builds a Settings value from these same two
		// operator-configured inputs once, at startup, and assigns it to
		// tier2SettingsOverride before ever accepting a connection; every
		// API-started run's own resolveSettings() call picks that up the
		// same way worker_config.go's own entries already do. See serveMain's
		// own comment at that assignment for why this is safe to leave set
		// for the whole daemon lifetime, unlike worker's clear-on-return.
		// releasePolicy/sandboxLimits themselves stay as this function's own
		// parameters (unused in this body now) rather than being dropped
		// from the signature: every existing call site -- serveMain's own,
		// and every test in this package that constructs one directly --
		// already supplies them, and removing the parameters would be pure
		// signature churn with no behavior change now that serveMain builds
		// the same values into tier2SettingsOverride itself.
		//
		// Same rationale again: sandboxPolicy.egressCABundle is this
		// daemon's own -egress-ca-bundle, forwarded so an API-started
		// run's relay/registry-proxy containers trust the same corporate
		// CA the operator configured on `serve` -- see
		// apiSandboxPolicy.egressCABundle's own doc comment.
		if sandboxPolicy.egressCABundle != "" {
			args = append(args, "-egress-ca-bundle", sandboxPolicy.egressCABundle)
		}
		ready := make(chan *run.Run, 1)
		errorsCh := make(chan error, 1)
		inFlight.Add(1)
		go func() {
			defer inFlight.Done()
			// Keep duplicate exclusion inside the background lifecycle. If the
			// HTTP client disconnects before the durable ready record exists,
			// apiStartStarter returns but the authorized run intentionally keeps
			// starting; releasing this lock at client cancellation would let a
			// retry with the same explicit ID race the first record creation.
			apiStartMu.Lock()
			locked := true
			unlock := func() {
				if locked {
					apiStartMu.Unlock()
					locked = false
				}
			}
			defer unlock()
			if _, err := run.Load(dataDir, id); err == nil {
				errorsCh <- api.ErrConflict
				return
			} else if !errors.Is(err, os.ErrNotExist) {
				errorsCh <- fmt.Errorf("check existing run: %w", err)
				return
			}

			readySent := false
			// context.Background(): this run must outlive the HTTP request
			// that started it, and factoryd serve (serveMain) is the sole
			// owner of this process's SIGINT/SIGTERM handling — see
			// runMainWithReady's doc comment for why an embedded run must
			// not install its own competing handler.
			err := runMainWithReady(dp, context.Background(), args, func(record *run.Run) {
				copy := *record
				ready <- &copy
				readySent = true
				unlock()
			})
			if err != nil {
				if readySent {
					log.Printf("api-started run %s failed after its initial record was created: %v", id, err)
					return
				}
				// A project-bootstrap preflight failure is caller-diagnosable
				// (a misconfigured -workspace/-spec pairing, or a project that
				// hasn't adopted the convention) the same way "ticket,
				// workspace, and spec are required" above already is --
				// wrapped in api.ErrInvalidStartRequest so startRunWithID
				// reports it as 400 with its own real message intact, instead
				// of falling through to the generic 500 "start run" every
				// other, genuinely internal runMainWithReady failure still
				// gets. See errProjectBootstrapCheckFailed's own doc comment.
				if errors.Is(err, errProjectBootstrapCheckFailed) {
					err = fmt.Errorf("%w: %s", api.ErrInvalidStartRequest, err.Error())
				}
				errorsCh <- err
				return
			}
			if !readySent {
				errorsCh <- errors.New("run completed before creating its initial durable record")
			}
		}()
		select {
		case record := <-ready:
			return record, nil
		case err := <-errorsCh:
			return nil, err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}
