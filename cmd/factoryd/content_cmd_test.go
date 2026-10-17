package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"buildgate/internal/sandbox"
	"buildgate/internal/sessionconfig"
)

func TestPromptEntriesUseHarnessAllowlistAndIdentifyRoleAndStage(t *testing.T) {
	dir := t.TempDir()
	for name, text := range map[string]string{
		"draft_spec.prompt.md":                "draft spec\n",
		"build_corrective.agent.prompt.md":    "fix build\n",
		"code_review.json_contract.prompt.md": "review contract\n",
		"not-allowlisted.prompt.md":           "ignore me\n",
		"build_app.py":                        "not a prompt\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := promptEntries(os.DirFS(dir), ".")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("got %d prompt entries, want 3: %+v", len(entries), entries)
	}
	got := map[string]contentEntry{}
	for _, e := range entries {
		got[e.name] = e
		if e.source != "embedded (edit source-controlled agent/pi/scripts/)" || !strings.Contains(e.path, "agent/pi/scripts/") {
			t.Errorf("%s source/path = %q / %q", e.name, e.source, e.path)
		}
		if e.hash != contentHash(e.data) {
			t.Errorf("%s hash = %s, want raw source hash %s", e.name, e.hash, contentHash(e.data))
		}
	}
	for name, want := range map[string][2]string{
		"draft_spec.prompt.md":                {"planning", "spec drafting"},
		"build_corrective.agent.prompt.md":    {"execution", "corrective build"},
		"code_review.json_contract.prompt.md": {"review", "code review"},
	} {
		if got[name].role != want[0] || got[name].stage != want[1] {
			t.Errorf("%s role/stage = %s/%s, want %s/%s", name, got[name].role, got[name].stage, want[0], want[1])
		}
	}
	if sandbox.HarnessSiblingModules["not-allowlisted.prompt.md"] {
		t.Fatal("test prompt unexpectedly entered the harness allowlist")
	}
}

type contentTestSources struct {
	scripts, skillPath, shadowPath, unusedPath, guidePath string
	skillText, guideText                                  string
	settings                                              sessionconfig.Settings
}

func newContentTestSources(t *testing.T) contentTestSources {
	t.Helper()
	sources := contentTestSources{scripts: t.TempDir()}
	teamSkills := t.TempDir()
	makeTeamSkill := func(name, text string) string {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(teamSkills, name), 0o750); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(teamSkills, name, "SKILL.md")
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	sources.skillText = "---\nname: team-skill\n---\nUse the project's conventions.\n"
	sources.skillPath = makeTeamSkill("team-skill", sources.skillText)
	shadowText := "---\nname: go-service\n---\nUse the team's Go conventions.\n"
	sources.shadowPath = makeTeamSkill("go-service", shadowText)
	sources.unusedPath = makeTeamSkill("unused-skill", "---\nname: unused-skill\n---\nNot selected.\n")
	guides := t.TempDir()
	sources.guidePath = filepath.Join(guides, "team-guide.md")
	sources.guideText = "# Team guide\n\n## Spec decisions\nAsk about data ownership.\n\n## Plan rules\nKeep migrations reversible.\n"
	if err := os.WriteFile(sources.guidePath, []byte(sources.guideText), 0o600); err != nil {
		t.Fatal(err)
	}
	sources.settings = sessionconfig.Settings{
		SkillDirs:       []string{teamSkills},
		DesignGuideDirs: []string{guides},
		Roles: &sessionconfig.Roles{
			Planning:  &sessionconfig.RoleConfig{Skills: []string{"go-service"}},
			Execution: &sessionconfig.RoleConfig{Skills: []string{"team-skill"}},
		},
	}
	return sources
}

func contentEntryMap(entries []contentEntry) map[string]contentEntry {
	byKey := map[string]contentEntry{}
	for _, entry := range entries {
		byKey[entry.kind+":"+entry.name+":"+entry.role] = entry
	}
	return byKey
}

func TestContentEntriesListAllSkillsAndShadowSources(t *testing.T) {
	sources := newContentTestSources(t)
	entries, err := contentEntries(sources.settings, os.DirFS(sources.scripts))
	if err != nil {
		t.Fatal(err)
	}
	byKey := contentEntryMap(entries)
	external := byKey["skill:team-skill:execution, review"]
	if external.source != "team file (editable)" || external.path != sources.skillPath || external.hash != contentHash([]byte(sources.skillText)) || external.selector != "skill:team-skill" || external.selection != "selected: execution, review; runtime validation pending" {
		t.Errorf("external skill entry = %+v", external)
	}
	shadow := byKey["skill:go-service:planning"]
	if shadow.source != "team file (editable)" || shadow.path != sources.shadowPath || shadow.selector != "skill:go-service" || shadow.selection != "selected: planning; runtime validation pending" {
		t.Errorf("shadowing team skill entry = %+v", shadow)
	}
	var shown bytes.Buffer
	if err := showContent(entries, "skill:go-service", &shown); err != nil || !strings.Contains(shown.String(), "Use the team's Go conventions.") {
		t.Errorf("show effective team skill = %v, output %q", err, shown.String())
	}
	unused := byKey["skill:unused-skill:"]
	if unused.source != "team file (editable)" || unused.path != sources.unusedPath || unused.selection != "readable, not selected; runtime validation pending" {
		t.Errorf("unselected team skill entry = %+v", unused)
	}
}

func TestContentEntriesIncludeOperatorSkill(t *testing.T) {
	entries, err := contentEntries(sessionconfig.Settings{}, os.DirFS(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.selector != "operator-skill:buildgate" {
			continue
		}
		if entry.path != "internal/operatorskill/buildgate/SKILL.md" || entry.hash != contentHash(entry.data) {
			t.Fatalf("operator skill entry = %+v", entry)
		}
		return
	}
	t.Fatal("operator skill is missing from content inventory")
}

func TestContentEntriesIncludeInlinePromptSourceMetadata(t *testing.T) {
	entries, err := contentEntries(sessionconfig.Settings{}, os.DirFS(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.selector != "prompt-source:draft-spec.context" {
			continue
		}
		if entry.kind != "prompt-source" || entry.role != "planning" || entry.path != "agent/pi/scripts/draft_spec.py" || entry.hash != contentHash(entry.data) || !strings.Contains(string(entry.data), "source_sha256") || entry.selection != "metadata only; composed at runtime" {
			t.Fatalf("inline prompt source entry = %+v", entry)
		}
		var shown bytes.Buffer
		if err := showContent(entries, entry.selector, &shown); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(shown.String(), "build_prompt") || !strings.Contains(shown.String(), "draft_spec.prompt.md") || !strings.Contains(shown.String(), "metadata only; composed at runtime") {
			t.Fatalf("inline source display metadata = %s", shown.String())
		}
		return
	}
	t.Fatal("inline prompt source metadata missing from content catalog")
}

func TestContentEntriesIncludeLanguageProfileMetadata(t *testing.T) {
	entries, err := contentEntries(sessionconfig.Settings{}, os.DirFS(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.selector != "language-profile:rust" {
			continue
		}
		if entry.kind != "language-profile" || entry.path != "internal/languageprofile/profile.go" || entry.hash != contentHash(entry.data) || !strings.Contains(string(entry.data), "cargo test --workspace") || !strings.Contains(string(entry.data), "ProjectImageManifests") || !strings.Contains(string(entry.data), "RegistryProxyEcosystems") || !strings.Contains(string(entry.data), "BaselineFailureFormat") {
			t.Fatalf("language profile entry = %+v", entry)
		}
		return
	}
	t.Fatal("language profile metadata missing from content catalog")
}

func TestContentEntriesListSelectedEditableSpecDraftGuidance(t *testing.T) {
	first, second := t.TempDir(), t.TempDir()
	one := "---\nname: team-spec\nslot: spec-drafting\nformat_version: 1\n---\nAsk about the user-visible behavior.\n"
	two := "---\nname: team-spec\nslot: spec-drafting\nformat_version: 1\n---\nLater duplicate.\n"
	for dir, body := range map[string]string{first: one, second: two} {
		if err := os.WriteFile(filepath.Join(dir, "team-spec.md"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	settings := sessionconfig.Settings{
		SpecDraftGuidanceDirs: []string{first, second},
		Roles:                 &sessionconfig.Roles{Planning: &sessionconfig.RoleConfig{SpecDraftGuidance: "team-spec"}},
	}
	entries, err := contentEntries(settings, os.DirFS(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	var selected, duplicate contentEntry
	for _, entry := range entries {
		if entry.selector == "spec-guidance:team-spec" {
			selected = entry
		}
		if strings.HasPrefix(entry.selector, "team-spec-guidance:team-spec:") {
			duplicate = entry
		}
	}
	if selected.selector != "spec-guidance:team-spec" || selected.selection != "selected: planning/spec drafting" || selected.hash != contentHash([]byte(one)) {
		t.Fatalf("selected spec guidance = %+v", selected)
	}
	var shown bytes.Buffer
	if err := showContent(entries, selected.selector, &shown); err != nil || !strings.Contains(shown.String(), "Ask about the user-visible behavior") {
		t.Fatalf("show selected guidance = %v, output %q", err, shown.String())
	}
	if duplicate.selector != "team-spec-guidance:team-spec:"+contentHash([]byte(two))+":2" || duplicate.selection != "shadowed duplicate; runtime validation pending" {
		t.Fatalf("shadowed guidance = %+v", duplicate)
	}
}

func TestContentEntriesShowsMissingSelectedSpecDraftGuidance(t *testing.T) {
	settings := sessionconfig.Settings{
		SpecDraftGuidanceDirs: []string{filepath.Join(t.TempDir(), "missing")},
		Roles:                 &sessionconfig.Roles{Planning: &sessionconfig.RoleConfig{SpecDraftGuidance: "team-spec"}},
	}
	entries, err := contentEntries(settings, os.DirFS(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	var entry contentEntry
	for _, candidate := range entries {
		if candidate.selector == "spec-guidance:team-spec" {
			entry = candidate
			break
		}
	}
	if entry.name != "team-spec" || entry.selection != "selected but unavailable" || !strings.Contains(entry.problem, "not found") {
		t.Fatalf("missing selected spec guidance = %+v", entry)
	}
	var shown bytes.Buffer
	if err := showContent(entries, entry.selector, &shown); err != nil || !strings.Contains(shown.String(), "selected but unavailable") {
		t.Fatalf("show missing selected guidance = %v, output %q", err, shown.String())
	}
}

func TestContentEntriesListDesignGuides(t *testing.T) {
	sources := newContentTestSources(t)
	entries, err := contentEntries(sources.settings, os.DirFS(sources.scripts))
	if err != nil {
		t.Fatal(err)
	}
	guide := contentEntryMap(entries)["guide:team-guide:planning"]
	if guide.source != "team file (editable)" || guide.path != sources.guidePath || guide.hash != contentHash([]byte(sources.guideText)) {
		t.Errorf("guide entry = %+v", guide)
	}
	var shown bytes.Buffer
	if err := showContent(entries, "guide:team-guide", &shown); err != nil || !strings.Contains(shown.String(), "Ask about data ownership") {
		t.Errorf("show guide = %v, output %q", err, shown.String())
	}
}

func TestContentEntriesListsInvalidGuideWithoutHidingCatalog(t *testing.T) {
	sources := newContentTestSources(t)
	invalidPath := filepath.Join(sources.settings.DesignGuideDirs[0], "notes.md")
	if err := os.WriteFile(invalidPath, []byte("unfinished guide\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	entries, err := contentEntries(sources.settings, os.DirFS(sources.scripts))
	if err != nil {
		t.Fatalf("invalid unselected guide hid the catalog: %v", err)
	}
	entry := contentEntryMap(entries)["guide:notes:planning"]
	if entry.path != invalidPath || entry.problem == "" || entry.hash != "" {
		t.Fatalf("invalid guide entry = %+v", entry)
	}
	var shown bytes.Buffer
	if err := showContent(entries, "guide:notes", &shown); err != nil || !strings.Contains(shown.String(), "content unavailable") {
		t.Fatalf("show invalid guide = %v, output %q", err, shown.String())
	}
}

func TestContentEntriesShowsSelectedUnreadableSkill(t *testing.T) {
	scripts, skillRoot := t.TempDir(), t.TempDir()
	target := filepath.Join(t.TempDir(), "actual-skill.md")
	if err := os.WriteFile(target, []byte("---\nname: linked-skill\n---\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	linkDir := filepath.Join(skillRoot, "linked-skill")
	if err := os.MkdirAll(linkDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(linkDir, "SKILL.md")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	settings := sessionconfig.Settings{SkillDirs: []string{skillRoot}, Roles: &sessionconfig.Roles{Execution: &sessionconfig.RoleConfig{Skills: []string{"linked-skill"}}}}
	entries, err := contentEntries(settings, os.DirFS(scripts))
	if err != nil {
		t.Fatal(err)
	}
	entry := contentEntryMap(entries)["skill:linked-skill:execution, review"]
	if entry.problem == "" || entry.selection != "selected: execution, review; runtime validation pending" {
		t.Fatalf("selected invalid skill was hidden: %+v", entry)
	}
}

func TestContentShowsASymlinkedTeamSkillAsUnavailable(t *testing.T) {
	root := t.TempDir()
	teamDir := filepath.Join(root, "go-service")
	if err := os.MkdirAll(teamDir, 0o750); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "SKILL.md")
	if err := os.WriteFile(target, []byte("---\nname: go-service\n---\nexternal\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(teamDir, "SKILL.md")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	entries, err := contentEntries(sessionconfig.Settings{SkillDirs: []string{root}, Roles: &sessionconfig.Roles{Execution: &sessionconfig.RoleConfig{Skills: []string{"go-service"}}}}, os.DirFS(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	var team contentEntry
	for _, entry := range entries {
		if entry.source == "team file (editable)" && entry.name == "go-service" {
			team = entry
		}
	}
	if team.problem == "" || team.selector != "skill:go-service" {
		t.Fatalf("symlinked selected team skill should be unavailable at the effective selector: %+v", team)
	}
}

func TestContentReaderBoundsFileAndAggregateBytes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "large.md")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", contentEntryMaxBytes+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	reader := &contentReader{remaining: contentReadBudget}
	if _, err := reader.read(path); err == nil || !strings.Contains(err.Error(), "inventory limit") {
		t.Fatalf("oversized content read err = %v", err)
	}
	if err := os.WriteFile(path, []byte("content"), 0o600); err != nil {
		t.Fatal(err)
	}
	reader.remaining = 2
	if _, err := reader.read(path); err == nil || !strings.Contains(err.Error(), "read limit") {
		t.Fatalf("aggregate budget err = %v", err)
	}
}

func TestContentEntriesQualifyDuplicateTeamSkillSources(t *testing.T) {
	one, two, scripts := t.TempDir(), t.TempDir(), t.TempDir()
	texts := []string{"---\nname: shared\n---\nfirst source\n", "---\nname: shared\n---\nsecond source\n"}
	for i, root := range []string{one, two} {
		path := filepath.Join(root, "shared", "SKILL.md")
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(texts[i]), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := contentEntries(sessionconfig.Settings{SkillDirs: []string{one, two}}, os.DirFS(scripts))
	if err != nil {
		t.Fatal(err)
	}
	winner := "skill:shared"
	duplicate := "team-skill:shared:" + contentHash([]byte(texts[1])) + ":2"
	for selector, want := range map[string]string{winner: "first source", duplicate: "second source"} {
		var shown bytes.Buffer
		if err := showContent(entries, selector, &shown); err != nil {
			t.Fatalf("show %s: %v", selector, err)
		}
		if !strings.Contains(shown.String(), want) {
			t.Errorf("show %s = %q, want %q", selector, shown.String(), want)
		}
	}
}

func TestContentEntriesSelectFirstResolvableTeamSkill(t *testing.T) {
	first, second, scripts := t.TempDir(), t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(first, "shared"), 0o750); err != nil {
		t.Fatal(err)
	}
	validPath := filepath.Join(second, "shared", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(validPath), 0o750); err != nil {
		t.Fatal(err)
	}
	valid := "---\nname: shared\n---\nvalid source\n"
	if err := os.WriteFile(validPath, []byte(valid), 0o600); err != nil {
		t.Fatal(err)
	}
	settings := sessionconfig.Settings{SkillDirs: []string{first, second}, Roles: &sessionconfig.Roles{Execution: &sessionconfig.RoleConfig{Skills: []string{"shared"}}}}
	entries, err := contentEntries(settings, os.DirFS(scripts))
	if err != nil {
		t.Fatal(err)
	}
	var winner contentEntry
	for _, entry := range entries {
		if entry.selector == "skill:shared" {
			winner = entry
		}
	}
	if winner.path != validPath || winner.problem != "" || winner.selection != "selected: execution, review; runtime validation pending" {
		t.Fatalf("catalog winner = %+v", winner)
	}
	var shown bytes.Buffer
	if err := showContent(entries, "skill:shared", &shown); err != nil || !strings.Contains(shown.String(), "valid source") {
		t.Fatalf("show winner = %v, output %q", err, shown.String())
	}
}

func TestContentSkillDirectoriesExpandBareTilde(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	got, err := contentSkillRoots(sessionconfig.Settings{SkillDirs: []string{"~"}})
	if err != nil || len(got) != 1 || got[0] != home {
		t.Fatalf("contentSkillRoots(~) = %v, %v; want %s", got, err, home)
	}
}

func TestContentCatalogSurvivesMissingSelectedSkill(t *testing.T) {
	root := t.TempDir()
	missing := filepath.Join(root, "missing-skill")
	if err := os.MkdirAll(missing, 0o750); err != nil {
		t.Fatal(err)
	}
	settings := sessionconfig.Settings{SkillDirs: []string{root}, Roles: &sessionconfig.Roles{Execution: &sessionconfig.RoleConfig{Skills: []string{"missing-skill"}}}}
	entries, err := contentEntries(settings, os.DirFS(t.TempDir()))
	if err != nil {
		t.Fatalf("missing selected skill hid the content catalog: %v", err)
	}
	byKey := contentEntryMap(entries)
	if entry := byKey["skill:missing-skill:"]; entry.problem == "" {
		t.Fatalf("missing skill source was not marked unavailable: %+v", entry)
	}
	var out bytes.Buffer
	if err := showContent(entries, "skill-config:execution", &out); err != nil || !strings.Contains(out.String(), "content unavailable") {
		t.Fatalf("show invalid skill configuration = %v, output %q", err, out.String())
	}
}

func TestContentCatalogSurvivesMissingConfiguredSkillDirectory(t *testing.T) {
	missingRoot := filepath.Join(t.TempDir(), "removed-skills")
	entries, err := contentEntries(sessionconfig.Settings{SkillDirs: []string{missingRoot}}, os.DirFS(t.TempDir()))
	if err != nil {
		t.Fatalf("missing skill directory hid the content catalog: %v", err)
	}
	var row contentEntry
	for _, entry := range entries {
		if entry.selector == "skill-dir:1" {
			row = entry
		}
	}
	if row.problem == "" || contentStatus(row) == "available" {
		t.Fatalf("missing skill directory row = %+v", row)
	}
}

func TestContentCatalogSurvivesMissingDesignGuideDirectory(t *testing.T) {
	missingRoot := filepath.Join(t.TempDir(), "removed-guides")
	entries, err := contentEntries(sessionconfig.Settings{DesignGuideDirs: []string{missingRoot}}, os.DirFS(t.TempDir()))
	if err != nil {
		t.Fatalf("missing guide directory hid the content catalog: %v", err)
	}
	byKey := contentEntryMap(entries)
	if entry := byKey["guide:design guide directories:"]; entry.problem == "" {
		t.Fatalf("missing guide directory row = %+v", entry)
	}
}

func TestShowContentLabelsSourceHashAndSanitizesDisplay(t *testing.T) {
	raw := []byte("token=ghp_123456789012345678901234567890\n")
	sum := sha256.Sum256(raw)
	entries := []contentEntry{{kind: "prompt", name: "example.prompt.md", selector: "prompt:example.prompt.md", role: "execution", stage: "build", source: "embedded", path: "agent/pi/scripts/example.prompt.md", hash: hex.EncodeToString(sum[:]), data: raw}}
	var out bytes.Buffer
	if err := showContent(entries, "prompt:example.prompt.md", &out); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	if !strings.Contains(text, "sha256: "+hex.EncodeToString(sum[:])) || !strings.Contains(text, "token=[redacted]") {
		t.Fatalf("show output missing source hash or sanitized content: %s", text)
	}
	if strings.Contains(text, "123456789012345678901234567890") {
		t.Fatalf("show output leaked source token: %s", text)
	}
	if err := showContent(entries, "../prompt:example.prompt.md", &out); err == nil {
		t.Fatal("malformed selector succeeded")
	}
	if err := showContent(entries, "skill:missing", &out); err == nil {
		t.Fatal("unknown source succeeded")
	}
}

func TestContentRunResolvesSelectedSkillsFromConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	skillRoot := filepath.Join(home, "skills")
	if err := os.MkdirAll(filepath.Join(skillRoot, "team-skill"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillRoot, "team-skill", "SKILL.md"), []byte("---\nname: team-skill\n---\nteam instructions\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(home, "factoryd.yml")
	if err := os.WriteFile(config, []byte("skill_dirs:\n  - "+skillRoot+"\nroles:\n  execution:\n    skills: [team-skill]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := contentRun([]string{"-config", config}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "team-skill") || !strings.Contains(out.String(), "team file (editable)") || !strings.Contains(out.String(), filepath.Join(skillRoot, "team-skill", "SKILL.md")) {
		t.Fatalf("content inventory did not show the selected team skill: %s", out.String())
	}
	if _, err := os.Stat(filepath.Join(home, "cache", "factoryd", "harness")); !os.IsNotExist(err) {
		t.Fatalf("read-only content inventory created a harness cache: %v", err)
	}
}

func TestContentRunDoesNotPrintCredentialsOrSavedRunPrompts(t *testing.T) {
	home := t.TempDir()
	secret := "ghp_123456789012345678901234567890"
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	t.Setenv("FACTORYD_API_READ_TOKEN", secret)
	config := filepath.Join(home, "factoryd.yml")
	configText := "compose_services_worker_env:\n  API_TOKEN: " + secret + "\n"
	if err := os.WriteFile(config, []byte(configText), 0o600); err != nil {
		t.Fatal(err)
	}
	runPrompts := filepath.Join(home, "data", "runs", "run-1", "prompts", "build-1")
	if err := os.MkdirAll(runPrompts, 0o750); err != nil {
		t.Fatal(err)
	}
	privateRunInput := "private run prompt: " + secret
	if err := os.WriteFile(filepath.Join(runPrompts, "build-round-1.md"), []byte(privateRunInput), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := contentRun([]string{"-config", config}, &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), secret) || strings.Contains(out.String(), "private run prompt") || strings.Contains(out.String(), "API_TOKEN") {
		t.Fatalf("inventory printed a credential or run-only input: %s", out.String())
	}
}
