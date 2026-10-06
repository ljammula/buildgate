// Package modelrole maps a build pipeline stage to the session-config
// role (planning, execution, review) that governs its model/thinking
// choice, and picks a real route for it out of a routes:/models: config
// (SelectRoute, route.go). A leaf package over sessionconfig,
// internal/meter, and internal/sandbox only -- never imported by cmd/,
// internal/workflow, or internal/api, so the stage table stays reusable
// wherever a stage needs to pick its model/route without pulling in the
// build loop itself.
package modelrole

import (
	"fmt"
)

// Role is one of the three kinds of work a session config's roles: block
// can configure separately.
type Role string

const (
	RolePlanning  Role = "planning"
	RoleExecution Role = "execution"
	RoleReview    Role = "review"
)

// Stage is a build pipeline stage name, matching the stage constants this
// package's own tests and callers use.
type Stage string

const (
	StageSpecDrafting         Stage = "spec_drafting"
	StagePlanning             Stage = "planning"
	StageOracleDrafting       Stage = "oracle_drafting"
	StageBuilding             Stage = "building"
	StageSpecConformity       Stage = "spec_conformity"
	StageConformityCorrective Stage = "conformity_corrective"
	StagePRCorrective         Stage = "pr_corrective"
)

// ForStage maps a pipeline stage to the Role that governs its model
// choice. An unknown Stage panics rather than returning a zero Role --
// every stage this package's own callers can name is a compiled-in
// constant above, so an unrecognized value can only mean a caller passed
// a typo or this table fell out of sync with a new stage, either of which
// should fail loudly at the call site rather than silently resolve no
// role.
//
// StageOracleDrafting maps to RoleReview, not RoleExecution, by operator
// decision: an oracle is an independent check of the builder's own work
// (the same reason spec_conformity is review, not execution), so drafting
// one belongs with review rather than with the stages that write the
// ticket's actual diff.
func ForStage(stage Stage) Role {
	switch stage {
	case StageSpecDrafting, StagePlanning:
		return RolePlanning
	case StageBuilding, StageConformityCorrective, StagePRCorrective:
		return RoleExecution
	case StageSpecConformity, StageOracleDrafting:
		return RoleReview
	default:
		panic(fmt.Sprintf("modelrole: unknown stage %q", stage))
	}
}

// isValidRole reports whether r is one of the three compiled-in Role
// constants -- Resolve's own "unknown Role panics" guard, mirroring
// ForStage's treatment of an unrecognized Stage.
func isValidRole(r Role) bool {
	switch r {
	case RolePlanning, RoleExecution, RoleReview:
		return true
	default:
		return false
	}
}
