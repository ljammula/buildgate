package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"buildgate/internal/languageprofile"
	"buildgate/internal/projectconfig"
)

// sandboxRegistryOfEcosystem maps a language profile's registry proxy
// ecosystem to the registry name a sandbox block lists.
var sandboxRegistryOfEcosystem = map[string]string{"npm": "npm", "PyPI": "pypi", "Go modules": "go", "Maven Central": "maven", "RubyGems": "rubygems"}

// sandboxBlockDraft is the `sandbox:` block a repository's one detected
// language profile suggests, as .factory.yml text, or "" when the profile has
// no install candidate or its lockfile is absent. The operator reviews and
// commits it, then approves it (factoryd sandbox approve); nothing here
// writes or approves anything.
func sandboxBlockDraft(repoRoot string) string {
	matches, err := languageprofile.Detect(repoRoot)
	if err != nil || len(matches) != 1 {
		return ""
	}
	profile := matches[0].Profile
	if profile.InstallCommand == "" {
		return ""
	}
	if info, err := os.Lstat(filepath.Join(repoRoot, profile.InstallRequires)); err != nil || !info.Mode().IsRegular() {
		return ""
	}
	for _, p := range profile.CachePaths {
		// A cache path the repository tracks would have the install write
		// over committed files.
		if out, err := exec.Command("git", "-C", repoRoot, "ls-files", "--", p).Output(); err != nil || len(strings.TrimSpace(string(out))) > 0 {
			return ""
		}
	}
	var registries []string
	for _, eco := range profile.RegistryProxyEcosystems {
		if name, ok := sandboxRegistryOfEcosystem[eco]; ok {
			registries = append(registries, name)
		}
	}
	var b strings.Builder
	b.WriteString("# sandbox: the dependency install each step runs first. It takes effect only after the operator approves this exact block (factoryd sandbox approve).\n")
	b.WriteString("sandbox:\n")
	fmt.Fprintf(&b, "  version: %d\n", projectconfig.SandboxVersion)
	fmt.Fprintf(&b, "  install: [%s]\n", quotedYAMLList([]string{profile.InstallCommand}))
	fmt.Fprintf(&b, "  cache_paths: [%s]\n", quotedYAMLList(profile.CachePaths))
	fmt.Fprintf(&b, "  registries: [%s]\n", strings.Join(registries, ", "))
	return b.String()
}

// sandboxDraftNote is the line printed after a .factory.yml that carries a
// drafted sandbox block was written: commit it, then approve the block.
func sandboxDraftNote(repoRoot string) string {
	if sandboxBlockDraft(repoRoot) == "" {
		return ""
	}
	q := strconv.Quote(repoRoot)
	return fmt.Sprintf("The file includes a sandbox: block drafted from the detected language profile. After committing it, run `factoryd sandbox show -repo %s` to read it and `factoryd sandbox approve -repo %s` to approve it: runs against this repository are refused until you do.", q, q)
}

// printFactoryYMLNotes prints what follows writing a .factory.yml for
// repoRoot: commit it, and approve a drafted sandbox block.
func printFactoryYMLNotes(repoRoot string) {
	fmt.Println(factoryYMLCommitReminder())
	if note := sandboxDraftNote(repoRoot); note != "" {
		fmt.Println(note)
	}
}

// factoryYMLWithSandbox is factoryYMLContent plus the drafted sandbox block
// for repoRoot, when its profile has one.
func factoryYMLWithSandbox(repoRoot, verifyCommand, preflightProfile string) string {
	return factoryYMLContent(verifyCommand, preflightProfile) + sandboxBlockDraft(repoRoot)
}
