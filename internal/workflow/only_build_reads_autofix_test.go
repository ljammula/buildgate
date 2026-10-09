package workflow

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Autofix commands run inside the build round only, so the field is carried
// by workflow_types.go and read by the build activity and by nothing else:
// a verify, gate or review step that read it would run a formatter on a tree
// after the build's result was fixed.
func TestOnlyTheBuildReadsAutofix(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if f == "workflow_types.go" || f == "activities_build.go" || strings.HasSuffix(f, "_test.go") {
			continue
		}
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "AutofixCommands") {
			t.Errorf("%s reads AutofixCommands: only the build activity may", f)
		}
	}
}

func TestBuildLaunchPassesAutofixCommandsToTheBuildScript(t *testing.T) {
	a, launches := launchRecorder(t)
	input := fixtureInput()
	input.VerifyCommand = "make verify"
	input.AutofixCommands = []string{"gofmt -w .", "ruff check --fix"}
	execActivity(t, a.RunBuildActivity, input)
	if len(*launches) != 1 {
		t.Fatalf("%d build launches, want 1", len(*launches))
	}
	joined := strings.Join((*launches)[0], "\x00")
	for _, c := range input.AutofixCommands {
		if !strings.Contains(joined, "\x00--autofix-command\x00"+c) {
			t.Errorf("build argv %q lacks --autofix-command %q", (*launches)[0], c)
		}
	}

	a, launches = launchRecorder(t)
	input.AutofixCommands = nil
	execActivity(t, a.RunBuildActivity, input)
	if got := (*launches)[0]; strings.Contains(strings.Join(got, " "), "--autofix-command") {
		t.Errorf("a build without autofix commands carries autofix: %q", got)
	}
}
