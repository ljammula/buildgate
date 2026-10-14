package sessionconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSpecGuidance(t *testing.T, dir, name, body string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name+".md")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const validSpecGuidance = "---\nname: team-spec\nslot: spec-drafting\nformat_version: 1\n---\nPrefer concrete acceptance criteria.\n"

func TestResolveSpecDraftGuidanceIsOptInAndPlanningOnly(t *testing.T) {
	if got, err := ResolveSpecDraftGuidance(Settings{}); err != nil || got != nil {
		t.Fatalf("unset guidance = %v, %v; want nil, nil", got, err)
	}
	dir := t.TempDir()
	path := writeSpecGuidance(t, dir, "team-spec", validSpecGuidance)
	s := Settings{
		SpecDraftGuidanceDirs: []string{dir},
		Roles:                 &Roles{Planning: &RoleConfig{SpecDraftGuidance: "team-spec"}},
	}
	got, err := ResolveSpecDraftGuidance(s)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "team-spec" || got.Path != path || got.Text != "Prefer concrete acceptance criteria." || got.SHA256 == "" {
		t.Fatalf("resolved guidance = %+v", got)
	}
	if err := ValidateSpecDraftGuidance(s); err != nil {
		t.Fatalf("ValidateSpecDraftGuidance: %v", err)
	}
	// A selection on another role is refused by semantic validation.
	execution := Settings{SpecDraftGuidanceDirs: []string{dir}, Roles: &Roles{Execution: &RoleConfig{SpecDraftGuidance: "team-spec"}}}
	if err := ValidateSpecDraftGuidance(execution); err == nil || !strings.Contains(err.Error(), "planning-only") {
		t.Fatalf("execution guidance validation error = %v, want planning-only refusal", err)
	}
}

func TestResolveSpecDraftGuidanceRejectsInvalidSources(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		want string
	}{
		{"wrong front matter name", []byte(strings.Replace(validSpecGuidance, "team-spec", "other", 1)), "name must match"},
		{"unknown front matter field", []byte(strings.Replace(validSpecGuidance, "slot: spec-drafting", "slot: spec-drafting\nrole: planning", 1)), "field role not found"},
		{"unknown slot", []byte(strings.Replace(validSpecGuidance, "spec-drafting", "review", 1)), "unsupported slot"},
		{"unsupported version", []byte(strings.Replace(validSpecGuidance, "format_version: 1", "format_version: 2", 1)), "unsupported format_version"},
		{"placeholder", []byte(strings.Replace(validSpecGuidance, "concrete acceptance criteria", "{unknown} criteria", 1)), "placeholders are not supported"},
		{"invalid utf8", append([]byte(validSpecGuidance), 0xff), "not valid UTF-8"},
		{"no front matter", []byte("Use concrete criteria"), "must start with YAML front matter"},
		{"oversized", []byte("---\nname: team-spec\nslot: spec-drafting\nformat_version: 1\n---\n" + strings.Repeat("x", specDraftGuidanceMaxBytes)), "exceeds"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeSpecGuidance(t, dir, "team-spec", string(tc.data))
			s := Settings{SpecDraftGuidanceDirs: []string{dir}, Roles: &Roles{Planning: &RoleConfig{SpecDraftGuidance: "team-spec"}}}
			if _, err := ResolveSpecDraftGuidance(s); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ResolveSpecDraftGuidance error = %v, want containing %q", err, tc.want)
			}
		})
	}
}

func TestResolveSpecDraftGuidanceRejectsSymlinkAndInvalidDirectory(t *testing.T) {
	real := t.TempDir()
	writeSpecGuidance(t, real, "team-spec", validSpecGuidance)
	linkDir := t.TempDir()
	link := filepath.Join(linkDir, "team-spec.md")
	if err := os.Symlink(filepath.Join(real, "team-spec.md"), link); err != nil {
		t.Fatal(err)
	}
	s := Settings{SpecDraftGuidanceDirs: []string{linkDir}, Roles: &Roles{Planning: &RoleConfig{SpecDraftGuidance: "team-spec"}}}
	if _, err := ResolveSpecDraftGuidance(s); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("symlink error = %v, want regular-file refusal", err)
	}
	if err := ValidateSpecDraftGuidance(Settings{SpecDraftGuidanceDirs: []string{"relative"}}); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("relative directory error = %v, want absolute-path refusal", err)
	}
}

func TestLoadSpecDraftGuidanceConfig(t *testing.T) {
	dir := t.TempDir()
	writeSpecGuidance(t, dir, "team-spec", validSpecGuidance)
	cfg, err := Load(writeConfig(t, "spec_draft_guidance_dirs: ["+dir+"]\nroles:\n  planning:\n    model: m\n    spec_draft_guidance: team-spec\n"))
	if err != nil {
		t.Fatal(err)
	}
	settings, err := cfg.ApplySettings(DefaultSettings())
	if err != nil {
		t.Fatal(err)
	}
	if len(settings.SpecDraftGuidanceDirs) != 1 || settings.Roles == nil || settings.Roles.Planning == nil || settings.Roles.Planning.SpecDraftGuidance != "team-spec" {
		t.Fatalf("settings did not retain spec-draft guidance config: %+v", settings)
	}
}
