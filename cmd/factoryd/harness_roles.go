package main

import (
	"fmt"
	"sort"

	"buildgate/internal/harness"
	"buildgate/internal/modelrole"
	"buildgate/internal/sessionconfig"
)

// requireHarnessSandboxImage refuses a role whose harness needs its own worker
// image (harness.Descriptor.RequiresSandboxImage: the canonical Pi worker
// image does not contain its binary) when no explicit sandbox image is set.
func requireHarnessSandboxImage(role string, d harness.Descriptor, sandboxImage string) error {
	if d.RequiresSandboxImage && sandboxImage == "" {
		return fmt.Errorf("roles.%s: harness %s requires an explicit digest-pinned sandbox_image (`make %s-image` builds it); the canonical Pi worker image does not contain its binary", role, d.Name, d.Name)
	}
	return nil
}

// harnessRoles is the default harness each configured-or-defaulted role runs
// under, keyed by role name, for every role a session can launch a job for
// (planning, execution, review). An unconfigured role resolves to pi, exactly
// as modelrole.RoleHarness does.
func harnessRoles(settings sessionconfig.Settings) (map[string]harness.Descriptor, error) {
	out := map[string]harness.Descriptor{}
	for _, role := range sessionRoles {
		name, err := modelrole.RoleHarness(settings, role, "")
		if err != nil {
			return nil, err
		}
		d, err := harness.Lookup(name)
		if err != nil {
			return nil, fmt.Errorf("roles.%s: %w", role, err)
		}
		out[string(role)] = d
	}
	return out, nil
}

var sessionRoles = []modelrole.Role{modelrole.RolePlanning, modelrole.RoleExecution, modelrole.RoleReview}

// harnessRoleSets is every harness each role can resolve to: its default
// harness plus everything in allowed_harnesses (a request may pick any of
// them), deduplicated and sorted by name. An unconfigured role is just pi.
// Requirements (worker image, binary in the image, worker model) must hold for
// the whole set, not only the default, or a per-request pick would fail at the
// build instead of at startup.
func harnessRoleSets(settings sessionconfig.Settings) (map[string][]harness.Descriptor, error) {
	out := map[string][]harness.Descriptor{}
	for _, role := range sessionRoles {
		names := []string{harness.Pi}
		if settings.Roles != nil {
			var rc *sessionconfig.RoleConfig
			switch role {
			case modelrole.RolePlanning:
				rc = settings.Roles.Planning
			case modelrole.RoleExecution:
				rc = settings.Roles.Execution
			case modelrole.RoleReview:
				rc = settings.Roles.Review
			}
			if rc != nil {
				names = append([]string{rc.HarnessName()}, rc.AllowedHarnessNames()...)
			}
		}
		seen := map[string]bool{}
		var ds []harness.Descriptor
		for _, n := range names {
			d, err := harness.Lookup(n)
			if err != nil {
				return nil, fmt.Errorf("roles.%s: %w", role, err)
			}
			if !seen[d.Name] {
				seen[d.Name] = true
				ds = append(ds, d)
			}
		}
		sort.Slice(ds, func(a, b int) bool { return ds[a].Name < ds[b].Name })
		out[string(role)] = ds
	}
	return out, nil
}

// sortedMapKeys returns m's keys in sorted order, for a stable role order in
// error reporting.
func sortedMapKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// requireSessionHarnessSandboxImage applies requireHarnessSandboxImage to every
// harness every role can resolve to, in a stable role order, for the entry
// points (worker, serve's API) that accept work before any single role is
// resolved.
func requireSessionHarnessSandboxImage(settings sessionconfig.Settings, sandboxImage string) error {
	roles, err := harnessRoleSets(settings)
	if err != nil {
		return err
	}
	for _, role := range sortedMapKeys(roles) {
		for _, d := range roles[role] {
			if err := requireHarnessSandboxImage(role, d, sandboxImage); err != nil {
				return err
			}
		}
	}
	return nil
}

// reviewHarnessFor is the harness every review job (conformity, code and
// combined review) runs under: roles.review's, or, with no roles.review (the
// steps then run on the execution role's own model and relay), the execution
// role's CONFIGURED harness. Review is never requester-selectable, so a
// request's -execution-harness pick never reaches it.
func reviewHarnessFor(settings sessionconfig.Settings) (string, error) {
	return modelrole.RoleHarness(settings, reviewRoleFor(settings), "")
}

// reviewRoleFor is the role review jobs run as: roles.review, or
// roles.execution when roles.review is unset. Harness and skills follow it.
func reviewRoleFor(settings sessionconfig.Settings) modelrole.Role {
	if !modelrole.RoleConfigured(settings, modelrole.RoleReview) {
		return modelrole.RoleExecution
	}
	return modelrole.RoleReview
}

// resolveRunHarnesses is the one place a run resolves the harness of each job
// kind: the execution jobs (build, corrective rounds) take the request's
// per-run pick (-execution-harness, "" for none) within
// roles.execution.allowed_harnesses; every review job takes reviewHarnessFor,
// which never sees that pick. run_ticket.go feeds these into the build and
// review argv and, for the Temporal path, RunWorkflowInput.Harness/ReviewHarness.
func resolveRunHarnesses(settings sessionconfig.Settings, executionChoice string) (execution, review string, err error) {
	execution, err = modelrole.RoleHarness(settings, modelrole.RoleExecution, executionChoice)
	if err != nil {
		return "", "", err
	}
	review, err = reviewHarnessFor(settings)
	return execution, review, err
}
