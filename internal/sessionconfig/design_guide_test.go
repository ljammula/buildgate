package sessionconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testGuide = "# Go service guide\n\n## Spec decisions\n\n1. Delivery.\n\n## Plan rules\n\n- Layers.\n"

func writeGuide(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name+".md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadDesignGuideSplitsTheTwoParts(t *testing.T) {
	dir := t.TempDir()
	writeGuide(t, dir, "go-service", testGuide)
	g, err := LoadDesignGuide(Settings{DesignGuideDirs: []string{dir}}, "go-service")
	if err != nil {
		t.Fatal(err)
	}
	if g.SpecDecisions != "1. Delivery." || g.PlanRules != "- Layers." {
		t.Fatalf("parts = %q / %q", g.SpecDecisions, g.PlanRules)
	}
	if len(g.SHA256) != 64 || g.Path != filepath.Join(dir, "go-service.md") {
		t.Fatalf("digest %q path %q", g.SHA256, g.Path)
	}
}

func TestLoadDesignGuideFirstDirWins(t *testing.T) {
	first, second := t.TempDir(), t.TempDir()
	writeGuide(t, first, "go-service", strings.Replace(testGuide, "- Layers.", "- First.", 1))
	writeGuide(t, second, "go-service", testGuide)
	g, err := LoadDesignGuide(Settings{DesignGuideDirs: []string{first, second}}, "go-service")
	if err != nil {
		t.Fatal(err)
	}
	if g.PlanRules != "- First." {
		t.Fatalf("plan rules = %q, want the first directory's", g.PlanRules)
	}
}

func TestLoadDesignGuideRefusals(t *testing.T) {
	dir := t.TempDir()
	writeGuide(t, dir, "no-plan", "## Spec decisions\n\n1. Delivery.\n")
	writeGuide(t, dir, "wrong-order", "## Plan rules\n\n- Layers.\n\n## Spec decisions\n\n1. Delivery.\n")
	writeGuide(t, dir, "empty-part", "## Spec decisions\n\n## Plan rules\n\n- Layers.\n")
	writeGuide(t, dir, "too-big", testGuide+strings.Repeat("x", designGuideMaxBytes))
	writeGuide(t, dir, "not-utf8", testGuide+"\xff\xfe")
	if err := os.Symlink(filepath.Join(dir, "no-plan.md"), filepath.Join(dir, "linked.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "a-dir.md"), 0o755); err != nil {
		t.Fatal(err)
	}
	withDir := Settings{DesignGuideDirs: []string{dir}}
	cases := []struct {
		name     string
		settings Settings
		guide    string
		want     string
	}{
		{"path-shaped name", withDir, "../go-service", "not a bare guide name"},
		{"no dirs configured", Settings{}, "go-service", "no design_guide_dirs"},
		{"relative dir", Settings{DesignGuideDirs: []string{"guides"}}, "go-service", "must be an absolute path"},
		{"missing dir", Settings{DesignGuideDirs: []string{filepath.Join(dir, "absent")}}, "go-service", "design_guide_dirs entry"},
		{"guide not found", withDir, "go-service", "no go-service.md in design_guide_dirs"},
		{"missing plan heading", withDir, "no-plan", "must have the headings"},
		{"headings out of order", withDir, "wrong-order", "must have the headings"},
		{"empty part", withDir, "empty-part", "has no text under"},
		{"over the size limit", withDir, "too-big", "byte limit"},
		{"not UTF-8", withDir, "not-utf8", "not valid UTF-8"},
		{"symlink", withDir, "linked", "not a regular file"},
		{"directory", withDir, "a-dir", "not a regular file"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadDesignGuide(tc.settings, tc.guide)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one containing %q", err, tc.want)
			}
		})
	}
}
