package api

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// cliFlag matches a command-line flag in a message: a dash and a name that
// start a word (`pass -verify-command`, "(-data-dir"), not a dash inside a
// word or a path.
var cliFlag = regexp.MustCompile(`(^|[^A-Za-z0-9_./-])--?[A-Za-z][A-Za-z-]*`)

// cliCommand matches a factoryd command line, which an MCP caller has no
// shell to run.
var cliCommand = regexp.MustCompile("`factoryd [a-z]")

// TestMCPToolErrorsNameNoFlag: an MCP caller has a tool's arguments and
// nothing else, so no tool error tells it to pass a command-line flag, and
// the refusal for a workspace with no verify command says what it can do.
func TestMCPToolErrorsNameNoFlag(t *testing.T) {
	listed := func(t *testing.T, factoryYML string) (dataDir, workspace string) {
		workspace = initGitWorkspace(t, "app")
		if factoryYML != "" {
			writeCreateTestFactoryYML(t, workspace, factoryYML)
		}
		return t.TempDir(), workspace
	}
	submit := func(workspace string) string {
		return `{"workspace":` + jsonString(workspace) + `,"text":"Add a thing"}`
	}
	cases := []struct {
		name string
		// setup returns the data dir, the allowlisted workspace, the tool
		// and its arguments.
		setup func(t *testing.T) (dataDir, workspace, tool, arguments string)
		want  []string
	}{
		{"no verify command", func(t *testing.T) (string, string, string, string) {
			dataDir, workspace := listed(t, "")
			return dataDir, workspace, "submit_request", submit(workspace)
		}, []string{"no verify command resolvable", "verify_command", ".factory.yml", "list_workspaces"}},
		{"data dir inside the workspace", func(t *testing.T) (string, string, string, string) {
			_, workspace := listed(t, "verify_command: \"make ci-verify\"\npreflight_profile: brownfield\n")
			dataDir := filepath.Join(workspace, "data")
			if err := os.MkdirAll(dataDir, 0o750); err != nil {
				t.Fatal(err)
			}
			return dataDir, workspace, "submit_request", submit(workspace)
		}, []string{"data dir", "inside"}},
		{"project-bootstrap preflight", func(t *testing.T) (string, string, string, string) {
			dataDir, workspace := listed(t, "verify_command: \"make ci-verify\"\n")
			return dataDir, workspace, "submit_request", submit(workspace)
		}, []string{"preflight", "preflight_profile: brownfield"}},
		{"no AGENTS.md", func(t *testing.T) (string, string, string, string) {
			workspace := filepath.Join(t.TempDir(), "app")
			if err := os.MkdirAll(workspace, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(workspace, ".factory.yml"), []byte("verify_command: \"make ci-verify\"\npreflight_profile: brownfield\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			for _, args := range [][]string{{"init", "-q"}, {"add", "."}, {"-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "-m", "factory config"}} {
				if out, err := exec.Command("git", append([]string{"-C", workspace}, args...)...).CombinedOutput(); err != nil {
					t.Fatalf("git %v: %v: %s", args, err, out)
				}
			}
			return t.TempDir(), workspace, "submit_request", submit(workspace)
		}, []string{"AGENTS.md"}},
		{"workspace not listed", func(t *testing.T) (string, string, string, string) {
			dataDir, workspace := listed(t, "")
			return dataDir, workspace, "submit_request", submit(initGitWorkspace(t, "other"))
		}, []string{"not allowlisted"}},
		{"empty text", func(t *testing.T) (string, string, string, string) {
			dataDir, workspace := listed(t, "")
			return dataDir, workspace, "submit_request", `{"workspace":` + jsonString(workspace) + `,"text":""}`
		}, nil},
		{"unknown request", func(t *testing.T) (string, string, string, string) {
			dataDir, workspace := listed(t, "")
			return dataDir, workspace, "get_request", `{"id":"no-such-request"}`
		}, nil},
		{"unknown run", func(t *testing.T) (string, string, string, string) {
			dataDir, workspace := listed(t, "")
			return dataDir, workspace, "get_run", `{"id":"no-such-run"}`
		}, nil},
		{"unknown run diff", func(t *testing.T) (string, string, string, string) {
			dataDir, workspace := listed(t, "")
			return dataDir, workspace, "get_run_diff", `{"id":"no-such-run"}`
		}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dataDir, workspace, tool, arguments := tc.setup(t)
			server := mcpTestServer(dataDir, WithWorkspaces([]string{workspace}))
			text, isError := mcpCall(t, server, tool, arguments)
			if !isError {
				t.Fatalf("%s %s: not an error: %s", tool, arguments, text)
			}
			var body struct {
				Error string `json:"error"`
			}
			if err := json.Unmarshal([]byte(text), &body); err != nil || body.Error == "" {
				t.Fatalf("error is not the route's error body: %v: %s", err, text)
			}
			text = body.Error
			if flag := cliFlag.FindString(text); flag != "" {
				t.Errorf("error names a flag (%q) an MCP caller cannot pass: %s", strings.TrimSpace(flag), text)
			}
			if cliCommand.MatchString(text) {
				t.Errorf("error names a factoryd command an MCP caller cannot run: %s", text)
			}
			for _, want := range tc.want {
				if !strings.Contains(text, want) {
					t.Errorf("error lacks %q: %s", want, text)
				}
			}
		})
	}
}
