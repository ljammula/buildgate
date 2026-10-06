package main

import (
	"fmt"
	"log"
	"time"

	"buildgate/internal/harness"
	"buildgate/internal/modelrole"
	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
	"buildgate/internal/runner"
	"buildgate/internal/sandbox"
	"buildgate/internal/sessionconfig"
)

// requestJobRoleOverride is what a drafting job's session-config role
// (planning or review -- see modelrole.ForStage) resolves to: a real
// route/model selection out of routes:/models:/roles:, plus the
// resolved thinking level for that role.
type requestJobRoleOverride struct {
	Thinking string
	// Harness is the coding-agent CLI the job runs under (the resolved role's
	// harness, never empty for a resolved override).
	Harness string
	// WorkerEnv is that harness's worker environment (harness.Descriptor.WorkerEnv),
	// passed explicitly to the sandbox launch.
	WorkerEnv []string
	// RouteSelection is modelrole.SelectRoute's own result for this
	// stage's role, resolved here rather than in
	// resolveRequestJobRelaySpec so a job with no usable route fails at
	// the same point every other relay-launch site does.
	// resolveRequestJobRelaySpec builds this job's whole RouteSpec from
	// it directly.
	RouteSelection *modelrole.Selection
	// Role is the role this job actually runs as (a stage whose own role
	// is unset falls back to execution).
	Role modelrole.Role
	// Skills are that role's operator skills (roleSkills), mounted
	// read-only at /inputs/skills.
	Skills []sandbox.SkillSource
}

// resolveRequestJobRole resolves stage's role (modelrole.ForStage) against
// cfg.settings and returns the override a drafting job applies. jobName only
// names the caller in an error's text (mirroring resolveRequestJobRelaySpec's
// own jobName parameter). modelChoice is the requester's own per-request
// planning model pick (request.Request.Models["planning"]), and harnessChoice
// the requester's own planning harness pick
// (request.Request.Harnesses["planning"]) -- each is only
// ever applied when this stage's role resolves to planning itself (never
// when it falls back to execution, since a planning-only choice has no
// business being checked against roles.execution.allowed, and never for
// review, which is not requester-selectable at all -- see
// sessionconfig.ValidateRequestModels).
//
// ValidateRouting only requires roles.execution to be configured, so a
// drafting job's own planning/review role can legitimately be unset:
// this falls back to roles.execution's model instead, mirroring what a
// ticket build's own model resolves to -- never refuse the job outright
// just because this one role was left unset.
func resolveRequestJobRole(cfg requestdriver.WorkerConfig, stage modelrole.Stage, jobName, modelChoice, harnessChoice string) (requestJobRoleOverride, error) {
	role := modelrole.ForStage(stage)
	settings := cfg.Settings
	effectiveRole := role
	if role != modelrole.RoleExecution && !modelrole.RoleConfigured(settings, role) {
		effectiveRole = modelrole.RoleExecution
	}
	choice, hChoice := "", ""
	if effectiveRole == modelrole.RolePlanning {
		choice, hChoice = modelChoice, harnessChoice
	}
	sel, err := modelrole.SelectRoute(settings, effectiveRole, choice, hChoice, "", func(_ string, r sessionconfig.Route) error {
		_, err := resolveRouteCredentials(r)
		return err
	})
	if err != nil {
		return requestJobRoleOverride{}, fmt.Errorf("%s: %w", jobName, err)
	}
	harnessDescriptor, err := harness.Lookup(sel.Harness)
	if err != nil {
		return requestJobRoleOverride{}, fmt.Errorf("%s: %w", jobName, err)
	}
	skills, err := roleSkills(cfg.Settings, effectiveRole)
	if err != nil {
		return requestJobRoleOverride{}, fmt.Errorf("%s: %w", jobName, err)
	}
	logRouteSkips(string(effectiveRole), &sel)
	return requestJobRoleOverride{Thinking: sel.Thinking, Harness: sel.Harness, WorkerEnv: harnessDescriptor.WorkerEnv, RouteSelection: &sel, Role: effectiveRole, Skills: skills}, nil
}

// jobSpendFromResult builds the request.JobSpend a drafting job (spec/plan/
// oracle) records from its own runner.Result, or nil per JobSpend's own
// "nil when the job had no relay" contract. RelayImageDigest/
// RelayNetworkName/RelayContainerName/RelayUpstream are the sandbox's own
// evidence that a relay was actually launched for this attempt (see
// runner.Result's doc comments) -- checked instead of RelayWorkerModelID
// alone, which is empty whenever no model id was configured even though a
// relay (and so a real spend) still exists.
func jobSpendFromResult(res runner.Result, role modelrole.Role, now time.Time) *request.JobSpend {
	hadRelay := res.RelayImageDigest != "" || res.RelayNetworkName != "" ||
		res.RelayContainerName != "" || res.RelayUpstream != ""
	if !hadRelay {
		return nil
	}
	return &request.JobSpend{
		Role:         string(role),
		Model:        res.RelayWorkerModelID,
		InputTokens:  res.RelayConsumedInputTokens,
		OutputTokens: res.RelayConsumedOutputTokens,
		CostMicroUSD: res.RelayConsumedCostMicroUSD,
		SpendPartial: res.RelaySpendPartial,
		Skills:       res.Skills,
		SkillsSHA256: res.SkillsSHA256,
		At:           now,
	}
}

// startActiveJob records stage's running job for request id (role,
// model, effort, route) so the console can show it while the job runs,
// and returns the func that clears it. It writes request.ActiveJobPath,
// never request.json, so it can't race the driver's or an operator's own
// writes. A failed write or clear only costs the display: logged, never
// fatal.
func startActiveJob(dataDir, id string, stage modelrole.Stage, o requestJobRoleOverride, now time.Time) func() {
	job := &request.ActiveJob{
		Stage:     string(stage),
		Role:      string(o.Role),
		Thinking:  o.Thinking,
		Harness:   o.Harness,
		StartedAt: now.UTC().Format(time.RFC3339),
	}
	if sel := o.RouteSelection; sel != nil {
		job.Model = sel.ModelName
		job.ModelID = sel.Policy.WorkerModelID
		job.Route = sel.RouteName
	}
	if err := request.WriteActiveJob(dataDir, id, *job); err != nil {
		log.Printf("request %s: record active %s job: %v", id, stage, err)
	}
	return func() {
		if err := request.ClearActiveJob(dataDir, id); err != nil {
			log.Printf("request %s: %v", id, err)
		}
	}
}
