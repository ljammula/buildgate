package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// dispatchGoldenSubcommands is every subcommand name factoryd dispatches on.
// Each is run with an undefined flag, which its own flag set rejects before
// the command does anything, so the first lines it prints name the command
// that was reached.
var dispatchGoldenSubcommands = []string{
	"override", "serve", "daemon", "supervise", "reset-stop-line", "override-rate",
	"check-project", "check-ticket", "ticket-template", "init", "onboard", "doctor",
	"intake", "kill-switch", "reconcile", "status", "inbox", "stop", "uninstall",
	"upgrade", "use", "watch", "logs", "cost", "submit", "approve", "reject", "cancel",
	"worker", "retry", "resume", "amend-scope", "init-config", "install-service",
	"uninstall-service", "quickstart", "console", "install-skill", "configure-images",
	"image-inputs-hash", "restart", "project-image-args", "image-reuse", "setup",
	"mcp", "memory", "stats", "build-ca-bundle",
}

// dispatchGoldenUndefinedFlag is the flag no command defines.
const dispatchGoldenUndefinedFlag = "-zz-not-a-flag"

// dispatchGoldenCases is the argument lists the golden file records.
func dispatchGoldenCases() [][]string {
	cases := [][]string{
		{},
		{"-h"},
		{"-help"},
		{"--help"},
		{"help"},
		{"help", "status"},
		{"version"},
		{"version", dispatchGoldenUndefinedFlag},
		{"no-such-command"},
		{"no-such-command", dispatchGoldenUndefinedFlag},
		{dispatchGoldenUndefinedFlag},
		{"STATUS", dispatchGoldenUndefinedFlag},
		{"-ticket", "t", "status"},
		// Two arguments: one is the destination directory it would write.
		{"stage-agent-tests", "a", "b"},
		{"stage-agent-tests"},
	}
	for _, name := range dispatchGoldenSubcommands {
		cases = append(cases, []string{name, dispatchGoldenUndefinedFlag})
	}
	return cases
}

var (
	dispatchGoldenTimestamp = regexp.MustCompile(`^\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2} `)
	dispatchGoldenVersion   = regexp.MustCompile(`^factoryd version .*$`)
	dispatchGoldenDoctor    = regexp.MustCompile(`^factoryd doctor: running .*$`)
)

// dispatchGoldenHead returns the first lines of output with the parts that
// differ between machines and runs replaced.
func dispatchGoldenHead(output, home, workDir string) []string {
	const keep = 3
	var lines []string
	for _, line := range strings.Split(output, "\n") {
		if len(lines) == keep {
			break
		}
		line = dispatchGoldenTimestamp.ReplaceAllString(line, "")
		line = dispatchGoldenVersion.ReplaceAllString(line, "factoryd version <version>")
		line = dispatchGoldenDoctor.ReplaceAllString(line, "factoryd doctor: running <binary> (<version>)")
		line = strings.ReplaceAll(line, binPath, "<binary>")
		line = strings.ReplaceAll(line, workDir, "<workdir>")
		line = strings.ReplaceAll(line, home, "<home>")
		lines = append(lines, strings.TrimRight(line, " \t"))
	}
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// TestSubcommandDispatchMatchesGolden pins which command each first argument
// reaches: for every subcommand name, and for the help, unknown and empty
// forms, the exit code and the first lines of stdout and stderr. Regenerate
// with FACTORYD_UPDATE_GOLDEN=1 only for a deliberate CLI change.
func TestSubcommandDispatchMatchesGolden(t *testing.T) {
	home := t.TempDir()
	xdg := filepath.Join(home, "xdg")
	if err := os.MkdirAll(filepath.Join(xdg, "factoryd"), 0o750); err != nil {
		t.Fatal(err)
	}
	workDir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(workDir); err == nil {
		workDir = resolved
	}
	var got strings.Builder
	for _, args := range dispatchGoldenCases() {
		cmd := exec.Command(binPath, args...)
		cmd.Dir = workDir
		cmd.Env = append(os.Environ(), "HOME="+home, "XDG_CONFIG_HOME="+xdg, "FACTORYD_PROFILE=", "TEMPORAL_ADDRESS=")
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		exitCode := 0
		if err := cmd.Run(); err != nil {
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) {
				t.Fatalf("factoryd %v: %v", args, err)
			}
			exitCode = exitErr.ExitCode()
		}
		fmt.Fprintf(&got, "%s\nexit %d\n", strings.TrimSpace("$ factoryd "+strings.Join(args, " ")), exitCode)
		for _, line := range dispatchGoldenHead(stdout.String(), home, workDir) {
			fmt.Fprintf(&got, "stdout: %s\n", line)
		}
		for _, line := range dispatchGoldenHead(stderr.String(), home, workDir) {
			fmt.Fprintf(&got, "stderr: %s\n", line)
		}
		got.WriteString("\n")
	}

	goldenPath := filepath.Join("testdata", "subcommand_dispatch.golden.txt")
	if os.Getenv("FACTORYD_UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(goldenPath, []byte(got.String()), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatal(err)
	}
	if got.String() != string(want) {
		t.Fatalf("subcommand dispatch differs from %s\n--- got ---\n%s", goldenPath, got.String())
	}
}

// TestEverySubcommandIsInTheDispatchTable fails when a subcommand is added
// to the dispatch table without a golden case, or dropped from it.
func TestEverySubcommandIsInTheDispatchTable(t *testing.T) {
	want := map[string]bool{"stage-agent-tests": true, "version": true}
	for _, name := range dispatchGoldenSubcommands {
		want[name] = true
	}
	table := subcommands()
	for name := range want {
		if table[name] == nil {
			t.Errorf("subcommand %q is not in the dispatch table", name)
		}
	}
	for name := range table {
		if !want[name] {
			t.Errorf("subcommand %q has no case in dispatchGoldenSubcommands", name)
		}
		if topLevelHelpTokens[name] {
			t.Errorf("subcommand %q is a top-level help token and can never be reached", name)
		}
	}
}
