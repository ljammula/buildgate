package workflow

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Autofix commands are carried on RunWorkflowInput and nothing runs them
// yet. The change that makes an Activity read them deletes this test
// deliberately.
func TestNoActivityReadsAutofixYet(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if f == "workflow_types.go" || strings.HasSuffix(f, "_test.go") {
			continue
		}
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, id := range []string{"AutofixCommands"} {
			if strings.Contains(string(data), id) {
				t.Errorf("%s reads %s: nothing may run autofix commands yet", f, id)
			}
		}
	}
}
