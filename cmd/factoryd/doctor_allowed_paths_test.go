package main

import (
	"context"
	"slices"
	"strings"
	"testing"

	"buildgate/internal/projectconfig"
)

func TestDoctorProposesAllowedPathsFromTheCommitsTopLevel(t *testing.T) {
	repo := commitFiles(t, map[string]string{
		".factory.yml":    "verify_command: \"go test ./...\"\n",
		".github/ci.yml":  "on: push\n",
		"go.mod":          "module x\n",
		"cmd/x/main.go":   "package main\n",
		"internal/a/a.go": "package a\n",
		"README.md":       "x\n",
		"weird[name].txt": "x\n",
	})
	check := doctorCheckAllowedPaths(context.Background(), repo)
	if check.Err == nil || !check.Advisory {
		t.Fatalf("check = %+v, want an advisory row for a repository with no allowed_paths", check)
	}
	const head = "allowed_paths: ["
	i := strings.Index(check.Fix, head)
	if i < 0 {
		t.Fatalf("fix proposes no allowed_paths:\n%s", check.Fix)
	}
	cfg, err := projectconfig.Parse([]byte(check.Fix[i:]))
	if err != nil {
		t.Fatalf("the proposed field does not parse: %v\n%s", err, check.Fix[i:])
	}
	want := []string{"cmd/", "internal/", "README.md", "go.mod"}
	if !slices.Equal(cfg.AllowedPaths, want) {
		t.Errorf("proposed %q, want %q (no dot-entries, no glob names)", cfg.AllowedPaths, want)
	}
}

func TestDoctorSaysWhatACommittedAllowedPathsBounds(t *testing.T) {
	repo := commitFiles(t, map[string]string{
		".factory.yml": "verify_command: \"go test ./...\"\nallowed_paths: [\"src/\", \"go.mod\"]\n",
		"src/a.go":     "package a\n",
	})
	check := doctorCheckAllowedPaths(context.Background(), repo)
	if check.Err != nil || !strings.Contains(check.Detail, "2 pattern(s)") {
		t.Errorf("check = %+v, want an ok row naming 2 patterns", check)
	}
}
