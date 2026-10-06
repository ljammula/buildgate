package main

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"buildgate/internal/modelrole"
	"buildgate/internal/sandbox"
	"buildgate/internal/sessionconfig"
)

// roleSkills resolves role's roles.<role>.skills through skill_dirs into
// the sources runSandboxWithRetries snapshots and mounts read-only at
// /inputs/skills. Nil when the role names none. Resolved again at each
// launch (not only at startup) so a skill removed since then fails the
// launch instead of silently disappearing from it.
func roleSkills(settings sessionconfig.Settings, role modelrole.Role) ([]sandbox.SkillSource, error) {
	refs, err := sessionconfig.ResolveRoleSkills(settings, string(role))
	if err != nil {
		return nil, fmt.Errorf("roles.%s.skills: %w", role, err)
	}
	return skillSources(refs), nil
}

// skillSources converts resolved session-config skills to sandbox sources.
func skillSources(refs []sessionconfig.SkillRef) []sandbox.SkillSource {
	if len(refs) == 0 {
		return nil
	}
	out := make([]sandbox.SkillSource, len(refs))
	for i, r := range refs {
		out[i] = sandbox.SkillSource{Name: r.Name, Dir: r.Dir, Builtin: r.Builtin}
	}
	return out
}

// roleSkillSet carries a run's execution and review skills to the Temporal
// input (RunWorkflowInput.Skills/ReviewSkills).
type roleSkillSet struct {
	Execution, Review []sandbox.SkillSource
}

// doctorCheckSkills validates skill_dirs and roles.<role>.skills, then
// dry-runs each role's snapshot (against -workspace when given, so a
// same-name repo skill shows up here rather than at launch) and lists
// what each role's worker would get.
func doctorCheckSkills(in doctorInputs) doctorCheck {
	name := "skills"
	if err := sessionconfig.ValidateSkills(in.settings); err != nil {
		return doctorCheck{Name: name, Err: err, Fix: "fix skill_dirs or the named roles.<role>.skills entry in your session config"}
	}
	var parts []string
	for _, role := range []modelrole.Role{modelrole.RolePlanning, modelrole.RoleExecution, modelrole.RoleReview} {
		skills, err := roleSkills(in.settings, role)
		if err != nil {
			return doctorCheck{Name: name, Err: err, Fix: "fix the named roles.<role>.skills entry in your session config"}
		}
		if len(skills) == 0 {
			continue
		}
		dryRun, err := os.MkdirTemp("", "factoryd-doctor-skills-")
		if err != nil {
			return doctorCheck{Name: name, Err: err}
		}
		// No -workspace: an empty scratch folder stands in, so nothing is
		// resolved against the current directory.
		workDir := in.workspace
		if workDir == "" {
			workDir = filepath.Join(dryRun, "workspace")
			err = os.Mkdir(workDir, 0o750)
		}
		if err == nil {
			_, err = sandbox.SnapshotSkills(workDir, filepath.Join(dryRun, "skills"), skills)
		}
		_ = os.RemoveAll(dryRun)
		if err != nil {
			return doctorCheck{Name: name, Err: err, Fix: "fix the named skill folder (or rename the repository's own skill of that name)"}
		}
		names := make([]string, len(skills))
		for i, s := range skills {
			from := s.Dir
			if s.Builtin {
				from = "built-in"
			}
			names[i] = s.Name + " (" + from + ")"
		}
		parts = append(parts, string(role)+": "+strings.Join(names, ", "))
	}
	if len(parts) == 0 {
		return doctorCheck{Name: name, Detail: "none configured"}
	}
	return doctorCheck{Name: name, Detail: strings.Join(parts, "; ")}
}

// checkSkillsFunc is a Temporal Worker's Activities.CheckSkills: skills from
// Workflow input are accepted only when they equal, name for name and
// folder for folder, what this Worker's own settings resolve for role
// ("execution", or "review" with its fallback to execution).
func checkSkillsFunc(settings sessionconfig.Settings) func(role string, skills []sandbox.SkillSource) error {
	return func(role string, skills []sandbox.SkillSource) error {
		var r modelrole.Role
		switch role {
		case string(modelrole.RoleExecution):
			r = modelrole.RoleExecution
		case string(modelrole.RoleReview):
			r = reviewRoleFor(settings)
		default:
			return fmt.Errorf("skills: unknown role %q", role)
		}
		own, err := roleSkills(settings, r)
		if err != nil {
			return err
		}
		if !slices.Equal(own, skills) {
			return fmt.Errorf("skills: the %s role's skills in this run's input differ from this Worker's own roles.%s.skills/skill_dirs", role, r)
		}
		return nil
	}
}
