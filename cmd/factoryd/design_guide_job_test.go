package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"buildgate/internal/request"
	"buildgate/internal/sessionconfig"
)

// designGuideRepo commits a .factory.yml with the given content into a new
// repository and returns its path: projectconfig reads the committed file.
func designGuideRepo(t *testing.T, factoryYML string) string {
	t.Helper()
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, ".factory.yml"), []byte(factoryYML), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "-q"},
		{"add", "-A"},
		{"-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-q", "-m", "init"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return repo
}

func designGuideSettings(t *testing.T) sessionconfig.Settings {
	t.Helper()
	dir := t.TempDir()
	guide := "## Spec decisions\n\n1. Delivery.\n\n## Plan rules\n\n- Layers.\n"
	if err := os.WriteFile(filepath.Join(dir, "go-service.md"), []byte(guide), 0o644); err != nil {
		t.Fatal(err)
	}
	return sessionconfig.Settings{DesignGuideDirs: []string{dir}}
}

func TestStageRequestDesignGuideStagesOnlyTheJobsPart(t *testing.T) {
	repo := designGuideRepo(t, "design_guide: go-service\n")
	settings := designGuideSettings(t)
	for _, tc := range []struct {
		part designGuidePart
		want string
	}{{designGuideSpecPart, "1. Delivery.\n"}, {designGuidePlanPart, "- Layers.\n"}} {
		scratch := filepath.Join(t.TempDir(), "scratch")
		guide, path, err := stageRequestDesignGuide(repo, settings, scratch, "", tc.part)
		if err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != tc.want || path != filepath.Join(scratch, designGuideFileName) {
			t.Errorf("part %d: staged %q at %s, want %q", tc.part, got, path, tc.want)
		}
		if guide.Name != "go-service" || len(guide.SHA256) != 64 {
			t.Errorf("guide = %+v", guide)
		}
	}
}

func TestStageRequestDesignGuideWithoutAGuideStagesNothing(t *testing.T) {
	repo := designGuideRepo(t, "verify_command: make verify\n")
	scratch := filepath.Join(t.TempDir(), "scratch")
	guide, path, err := stageRequestDesignGuide(repo, designGuideSettings(t), scratch, "", designGuideSpecPart)
	if err != nil || guide != nil || path != "" {
		t.Fatalf("got (%v, %q, %v), want (nil, \"\", nil)", guide, path, err)
	}
	if _, statErr := os.Stat(scratch); !os.IsNotExist(statErr) {
		t.Errorf("scratch dir was created with no guide to stage")
	}
}

// A repository naming a guide the operator's config cannot resolve must
// fail the drafting job, not draft silently without the team's rules.
func TestStageRequestDesignGuideFailsWhenTheNamedGuideDoesNotResolve(t *testing.T) {
	repo := designGuideRepo(t, "design_guide: python-service\n")
	_, _, err := stageRequestDesignGuide(repo, designGuideSettings(t), filepath.Join(t.TempDir(), "scratch"), "", designGuideSpecPart)
	if err == nil || !strings.Contains(err.Error(), `names design_guide "python-service"`) || !strings.Contains(err.Error(), "no python-service.md") {
		t.Fatalf("err = %v", err)
	}
}

func TestWithDesignGuideRecordsNameAndDigest(t *testing.T) {
	guide := &sessionconfig.DesignGuide{Name: "go-service", SHA256: "abc"}
	spend := withDesignGuide(&request.JobSpend{Role: "planning"}, guide)
	if spend.DesignGuide != "go-service" || spend.DesignGuideSHA256 != "abc" {
		t.Errorf("spend = %+v", spend)
	}
	if withDesignGuide(nil, guide) != nil {
		t.Error("a job with no spend record gained one")
	}
	if got := withDesignGuide(&request.JobSpend{}, nil); got.DesignGuide != "" {
		t.Errorf("spend = %+v, want no guide", got)
	}
}

func TestDoctorCheckDesignGuide(t *testing.T) {
	if c := doctorCheckDesignGuide(doctorInputs{}); c.Err != nil || c.Detail != "none configured" {
		t.Fatalf("no folders: %+v", c)
	}
	settings := designGuideSettings(t)
	if c := doctorCheckDesignGuide(doctorInputs{settings: settings}); c.Err != nil || c.Detail != "available: go-service" {
		t.Fatalf("no workspace: %+v", c)
	}
	named := designGuideRepo(t, "design_guide: go-service\n")
	c := doctorCheckDesignGuide(doctorInputs{settings: settings, workspace: named})
	if c.Err != nil || !strings.HasPrefix(c.Detail, "go-service (") || !strings.Contains(c.Detail, "sha256 ") {
		t.Fatalf("repository naming a guide: %+v", c)
	}
	if c := doctorCheckDesignGuide(doctorInputs{settings: settings, workspace: designGuideRepo(t, "verify_command: make verify\n")}); c.Err != nil || !strings.Contains(c.Detail, "names none") {
		t.Fatalf("repository naming no guide: %+v", c)
	}
	absent := designGuideRepo(t, "design_guide: python-service\n")
	if c := doctorCheckDesignGuide(doctorInputs{settings: settings, workspace: absent}); c.Err == nil || !strings.Contains(c.Fix, "python-service.md") {
		t.Fatalf("repository naming an absent guide: %+v", c)
	}
	missingDir := sessionconfig.Settings{DesignGuideDirs: []string{filepath.Join(t.TempDir(), "absent")}}
	if c := doctorCheckDesignGuide(doctorInputs{settings: missingDir}); c.Err == nil {
		t.Fatalf("missing folder: %+v", c)
	}
}
