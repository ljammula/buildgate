package sessionconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"buildgate/internal/workerskills"
)

func mkSkill(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(p, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p, "SKILL.md"), []byte("---\nname: "+name+"\n---\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	return p
}

func skillSettings(dirs []string, exec ...string) Settings {
	return Settings{SkillDirs: dirs, Roles: &Roles{Execution: &RoleConfig{Model: "m", Skills: exec}}}
}

func TestRoleSkillsNameKeysOnly(t *testing.T) {
	d := t.TempDir()
	mkSkill(t, d, "x")
	for _, bad := range []string{"../x", "/abs/x", "a/b", "X", "", "-x", "x y"} {
		err := ValidateSkills(skillSettings([]string{d}, bad))
		if err == nil || !strings.Contains(err.Error(), "bare skill name") {
			t.Errorf("name %q: err = %v, want bare-name refusal", bad, err)
		}
	}
}

func TestResolveRoleSkills(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	mkSkill(t, a, "one")
	mkSkill(t, b, "one")
	mkSkill(t, b, "two")
	s := skillSettings([]string{a, b}, "one", "two")
	if err := ValidateSkills(s); err != nil {
		t.Fatal(err)
	}
	refs, err := ResolveRoleSkills(s, "execution")
	if err != nil {
		t.Fatal(err)
	}
	want := []SkillRef{{Name: "one", Dir: filepath.Join(a, "one")}, {Name: "two", Dir: filepath.Join(b, "two")}}
	if len(refs) != 2 || refs[0] != want[0] || refs[1] != want[1] {
		t.Fatalf("refs = %v, want %v", refs, want)
	}
	for _, role := range []string{"planning", "review"} {
		if refs, err := ResolveRoleSkills(s, role); refs != nil || err != nil {
			t.Errorf("%s: got %v, %v want nil, nil", role, refs, err)
		}
	}
	if refs, err := ResolveRoleSkills(Settings{}, "execution"); refs != nil || err != nil {
		t.Errorf("nil roles: got %v, %v", refs, err)
	}
	if _, err := ResolveRoleSkills(s, "bogus"); err == nil {
		t.Error("unknown role accepted")
	}
}

func TestResolveRoleSkillsSymlinkedSkillDir(t *testing.T) {
	real := t.TempDir()
	mkSkill(t, real, "linked")
	link := filepath.Join(t.TempDir(), "skills")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	s := skillSettings([]string{link}, "linked")
	if err := ValidateSkills(s); err != nil {
		t.Fatal(err)
	}
	refs, err := ResolveRoleSkills(s, "execution")
	if err != nil || len(refs) != 1 || refs[0].Dir != filepath.Join(link, "linked") {
		t.Fatalf("refs = %v, err = %v", refs, err)
	}
}

func TestResolveRoleSkillsHomeExpansion(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	mkSkill(t, filepath.Join(home, "sk"), "h")
	s := skillSettings([]string{"~/sk"}, "h")
	if err := ValidateSkills(s); err != nil {
		t.Fatal(err)
	}
	refs, err := ResolveRoleSkills(s, "execution")
	if err != nil || refs[0].Dir != filepath.Join(home, "sk", "h") {
		t.Fatalf("refs = %v, err = %v", refs, err)
	}
}

func TestValidateSkillsRefusals(t *testing.T) {
	d := t.TempDir()
	mkSkill(t, d, "ok")
	if err := os.MkdirAll(filepath.Join(d, "nomd"), 0o750); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(file, nil, 0o640); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		s    Settings
		want string
	}{
		{"relative dir", skillSettings([]string{"rel/dir"}), "absolute"},
		{"missing dir", skillSettings([]string{filepath.Join(d, "gone")}), "skill_dirs entry"},
		{"dir is file", skillSettings([]string{file}), "not a directory"},
		{"duplicate", skillSettings([]string{d}, "ok", "ok"), "twice"},
		{"not built in and no skill_dirs", skillSettings(nil, "ok"), "nor built in"},
		{"unknown name", skillSettings([]string{d}, "nope"), `"nope"`},
		{"missing SKILL.md", skillSettings([]string{d}, "nomd"), `"nomd" is neither in skill_dirs`},
	}
	for _, c := range cases {
		err := ValidateSkills(c.s)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want containing %q", c.name, err, c.want)
		}
	}
	if err := ValidateSkills(Settings{}); err != nil {
		t.Errorf("empty config: %v", err)
	}
}

func TestSkillsConfigKeysLoad(t *testing.T) {
	d := t.TempDir()
	mkSkill(t, d, "demo")
	cfg, err := Load(writeConfig(t, "skill_dirs: ["+d+"]\nroles:\n  execution:\n    model: m\n    skills: [demo]\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	s, err := cfg.ApplySettings(DefaultSettings())
	if err != nil {
		t.Fatal(err)
	}
	if len(s.SkillDirs) != 1 || s.Roles.Execution.Skills[0] != "demo" {
		t.Fatalf("settings = %+v", s)
	}
}

// A role may name buildgate's built-in skills with no skill_dirs at all,
// and an operator skill_dirs entry of the same name overrides the built-in.
func TestResolveRoleSkillsBuiltinsAndOverride(t *testing.T) {
	got, err := ResolveRoleSkills(skillSettings(nil, "buildgate-tdd", "go-service"), "execution")
	if err != nil || len(got) != 2 {
		t.Fatalf("built-ins: %v %v", got, err)
	}
	for _, r := range got {
		if !r.Builtin || r.Dir != "" {
			t.Errorf("%s: want a built-in ref with no folder, got %+v", r.Name, r)
		}
	}
	if err := ValidateSkills(skillSettings(nil, "buildgate-tdd")); err != nil {
		t.Fatalf("ValidateSkills with only built-ins: %v", err)
	}
	d := t.TempDir()
	mkSkill(t, d, "go-service")
	got, err = ResolveRoleSkills(skillSettings([]string{d}, "go-service"), "execution")
	if err != nil || len(got) != 1 || got[0].Dir != filepath.Join(d, "go-service") || got[0].Builtin {
		t.Fatalf("override: %v %v", got, err)
	}
}

// init-config's scaffold lists every built-in skill, so the opt-in example
// never drifts from what ships.
func TestExampleListsEveryBuiltinSkill(t *testing.T) {
	for _, name := range workerskills.Names() {
		if !strings.Contains(Example, name) {
			t.Errorf("sessionconfig.Example does not mention built-in skill %q", name)
		}
	}
}
