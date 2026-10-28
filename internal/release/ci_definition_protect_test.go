package release

import (
	"reflect"
	"testing"
)

var ciDefinitionProtected = []string{
	".github/workflows/ci.yml", ".github/workflows/sub/x.yaml", ".GitHub/Workflows/CI.yml", ".github/actions/setup/action.yml",
	".gitlab-ci.yml", ".GitLab-CI.yml", ".gitlab/ci/a.yml", ".circleci/config.yml", ".CircleCI/config.yml",
	".buildkite/pipeline.yml", ".travis.yml", "azure-pipelines.yml", ".azure-pipelines/a.yml",
	"bitbucket-pipelines.yml", "Jenkinsfile", "JENKINSFILE", ".drone.yml", ".woodpecker.yml", ".woodpecker/a.yml",
}

var ciDefinitionNotProtected = []string{
	".github/CODEOWNERS", ".github/workflows", ".github/workflowsx/a.yml", ".githubx/workflows/a.yml",
	"docs/.github/workflows/a.yml", "src/Jenkinsfile", "Jenkinsfile.md", ".travis.yml.bak", ".gitlab-ci.yaml",
	".circleci", "azure-pipelines.yaml", "ci/azure-pipelines.yml",
}

func TestProtectedFilesTouchedProtectsTheCIDefinitionPathsInAnyCase(t *testing.T) {
	changed := append(append([]string{"main.go"}, ciDefinitionProtected...), ciDefinitionNotProtected...)
	got := MergePolicy{}.ProtectedFilesTouched(changed)
	if !reflect.DeepEqual(got, ciDefinitionProtected) {
		t.Fatalf("got %v, want %v", got, ciDefinitionProtected)
	}
}

func TestMergePolicyCheckDeniesAChangeToACIDefinitionPath(t *testing.T) {
	for _, p := range ciDefinitionProtected {
		r := cleanRun()
		r.ChangedFiles = append(r.ChangedFiles, p)
		cfg := cleanPolicy()
		cfg.ProtectedPaths = nil
		assertMergePolicyReason(t, r, cfg, p)
	}
	for _, p := range ciDefinitionNotProtected {
		r := cleanRun()
		r.ChangedFiles = append(r.ChangedFiles, p)
		cfg := cleanPolicy()
		cfg.ProtectedPaths = nil
		if passed, reasons := MergePolicyCheck(r, cfg); !passed {
			t.Errorf("%s: denied (%v), want it released", p, reasons)
		}
	}
}
