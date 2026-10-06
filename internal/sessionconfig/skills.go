package sessionconfig

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"buildgate/internal/workerskills"
)

// SkillRef is one resolved skill: its name and either the absolute path of
// its folder in skill_dirs (~ expanded, symlinks NOT resolved -- the
// snapshot step resolves and checks them) or Builtin, for one of
// buildgate's embedded skills (Dir empty).
type SkillRef struct {
	Name    string
	Dir     string
	Builtin bool
}

var skillNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

func expandHome(path string) (string, error) {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if path == "~" {
		return home, nil
	}
	return filepath.Join(home, strings.TrimPrefix(path, "~/")), nil
}

func roleConfig(s Settings, role string) (*RoleConfig, error) {
	switch role {
	case "planning", "execution", "review":
	default:
		return nil, fmt.Errorf("skills: unknown role %q (want planning, execution or review)", role)
	}
	if s.Roles == nil {
		return nil, nil
	}
	switch role {
	case "planning":
		return s.Roles.Planning, nil
	case "execution":
		return s.Roles.Execution, nil
	}
	return s.Roles.Review, nil
}

func expandedSkillDirs(s Settings) ([]string, error) {
	dirs := make([]string, 0, len(s.SkillDirs))
	for _, d := range s.SkillDirs {
		e, err := expandHome(d)
		if err != nil {
			return nil, fmt.Errorf("skills: expand skill_dirs entry %q: %w", d, err)
		}
		dirs = append(dirs, e)
	}
	return dirs, nil
}

// ResolveRoleSkills resolves role's skill names: for each name, in order,
// the first skill_dirs entry holding <dir>/<name>/SKILL.md (symlinks
// followed) wins, then buildgate's built-in skills (internal/workerskills),
// so an operator folder can add skills or override a built-in one. A role
// that is absent or names no skills yields nil, nil.
func ResolveRoleSkills(s Settings, role string) ([]SkillRef, error) {
	rc, err := roleConfig(s, role)
	if err != nil {
		return nil, err
	}
	if rc == nil || len(rc.Skills) == 0 {
		return nil, nil
	}
	dirs, err := expandedSkillDirs(s)
	if err != nil {
		return nil, err
	}
	refs := make([]SkillRef, 0, len(rc.Skills))
	for _, name := range rc.Skills {
		if !skillNameRE.MatchString(name) {
			return nil, fmt.Errorf("skills: role %s: skill %q is not a bare skill name (want %s)", role, name, skillNameRE)
		}
		found := ""
		for _, d := range dirs {
			cand := filepath.Join(d, name)
			if info, err := os.Stat(filepath.Join(cand, "SKILL.md")); err == nil && info.Mode().IsRegular() {
				found = cand
				break
			}
		}
		if found != "" {
			refs = append(refs, SkillRef{Name: name, Dir: found})
			continue
		}
		if !workerskills.Has(name) {
			return nil, fmt.Errorf("skills: role %s: skill %q is neither in skill_dirs %v nor built in (built-in: %s)", role, name, dirs, strings.Join(workerskills.Names(), ", "))
		}
		refs = append(refs, SkillRef{Name: name, Builtin: true})
	}
	return refs, nil
}

// ValidateSkills checks skill_dirs and every role's skills: dirs absolute
// (after ~ expansion) and existing, names bare and unique per role, and
// every name resolvable (in skill_dirs or built in). An empty config
// passes.
func ValidateSkills(s Settings) error {
	dirs, err := expandedSkillDirs(s)
	if err != nil {
		return err
	}
	for i, d := range dirs {
		if !filepath.IsAbs(d) {
			return fmt.Errorf("skills: skill_dirs entry %q must be an absolute path (or start with ~/)", s.SkillDirs[i])
		}
		info, err := os.Stat(d)
		if err != nil {
			return fmt.Errorf("skills: skill_dirs entry %q: %w", s.SkillDirs[i], err)
		}
		if !info.IsDir() {
			return fmt.Errorf("skills: skill_dirs entry %q is not a directory", s.SkillDirs[i])
		}
	}
	for _, role := range []string{"planning", "execution", "review"} {
		rc, err := roleConfig(s, role)
		if err != nil {
			return err
		}
		if rc == nil || len(rc.Skills) == 0 {
			continue
		}
		seen := map[string]bool{}
		for _, name := range rc.Skills {
			if !skillNameRE.MatchString(name) {
				return fmt.Errorf("skills: role %s: skill %q is not a bare skill name (want %s)", role, name, skillNameRE)
			}
			if seen[name] {
				return fmt.Errorf("skills: role %s: skill %q is listed twice", role, name)
			}
			seen[name] = true
		}
		if _, err := ResolveRoleSkills(s, role); err != nil {
			return err
		}
	}
	return nil
}
