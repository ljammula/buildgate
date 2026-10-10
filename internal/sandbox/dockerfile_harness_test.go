package sandbox

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"buildgate/internal/harness"
)

// TestWorkerImageCarriesEveryHarnessAtAnExactVersion pins the worker image's
// harness CLIs: each registered harness whose binary the canonical image
// provides is installed from its own Node stage at an exact version with no
// install scripts, symlinked into the fixed launch PATH, and exercised by the
// build-time smoke line. Codex and Copilot must also carry no credential and
// keep Copilot from updating itself or unpacking into per-container state.
func TestWorkerImageCarriesEveryHarnessAtAnExactVersion(t *testing.T) {
	data, err := os.ReadFile("Dockerfile")
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	text := string(data)

	for _, want := range []string{
		"npm install -g --ignore-scripts @earendil-works/pi-coding-agent@0.84.4",
		"npm install -g --ignore-scripts @openai/codex@0.154.0",
		"ARG COPILOT_VERSION=1.0.88",
		"npm install -g --ignore-scripts @github/copilot@${COPILOT_VERSION}",
		"/opt/copilot-cache/copilot/pkg/*/${COPILOT_VERSION} /opt/copilot-pkg",
		"&& test -f /opt/copilot-pkg/index.js",
		"COPY --from=codex /usr/local/lib/node_modules /usr/local/lib/node_modules",
		"COPY --from=copilot /usr/local/lib/node_modules /usr/local/lib/node_modules",
		"ln -s /usr/local/lib/node_modules/@openai/codex/bin/codex.js /usr/local/bin/codex",
		"ln -s /usr/local/lib/node_modules/@github/copilot/npm-loader.js /usr/local/bin/copilot",
		"ENV COPILOT_AUTO_UPDATE=false",
		"ENV COPILOT_CLI_DIST_DIR=/opt/copilot-pkg",
		"RUN pi --version && codex --version && copilot --version",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("Dockerfile missing %q", want)
		}
	}

	// Every npm install of an agent CLI names an exact x.y.z, never a tag or
	// range.
	installs := regexp.MustCompile(`npm install -g [^\n]*`).FindAllString(text, -1)
	if len(installs) != 3 {
		t.Errorf("Dockerfile has %d agent npm installs, want 3 (pi, codex, copilot): %q", len(installs), installs)
	}
	exact := regexp.MustCompile(`--ignore-scripts @[a-z-]+/[a-z-]+@(\d+\.\d+\.\d+|\$\{COPILOT_VERSION\})$`)
	for _, install := range installs {
		if !exact.MatchString(install) {
			t.Errorf("agent install %q is not an exact-version --ignore-scripts install", install)
		}
	}

	// The image never bakes a credential for any harness.
	for _, forbidden := range []string{"OPENAI_API_KEY", "CODEX_API_KEY", "GITHUB_TOKEN", "GH_TOKEN", "COPILOT_GITHUB_TOKEN", "COPILOT_ALLOW_ALL"} {
		if strings.Contains(text, forbidden) {
			t.Errorf("Dockerfile mentions credential or trust variable %q", forbidden)
		}
	}

	// The registry's binaries that the canonical image must provide are all
	// on the smoke line.
	for _, name := range harness.Names() {
		d, _ := harness.Lookup(name)
		if d.RequiresSandboxImage {
			continue
		}
		if !strings.Contains(text, "ln -s ") || !strings.Contains(text, "/usr/local/bin/"+d.Binary) {
			t.Errorf("Dockerfile does not link harness binary %q into /usr/local/bin", d.Binary)
		}
		if !strings.Contains(text, d.Binary+" --version") {
			t.Errorf("Dockerfile smoke test does not run %s --version", d.Binary)
		}
	}
}

// TestDockerfilesEndInTheSandboxWorkdirOwnedByTheWorkerUser pins the working
// directory the OpenShell runtime starts a worker in: the recipe creates
// /sandbox for 65532 while still root and ends with WORKDIR /sandbox.
func TestDockerfilesEndInTheSandboxWorkdirOwnedByTheWorkerUser(t *testing.T) {
	for _, name := range []string{"Dockerfile"} {
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		text := string(data)
		if !strings.Contains(text, "RUN mkdir -p /sandbox && chown 65532:65532 /sandbox\nUSER 65532:65532\nWORKDIR /sandbox\n") {
			t.Errorf("%s must create /sandbox owned by 65532:65532, then USER 65532:65532, then WORKDIR /sandbox", name)
		}
		if strings.Contains(text, "WORKDIR /workspace") {
			t.Errorf("%s still sets WORKDIR /workspace", name)
		}
	}
}

// TestWorkerDockerfilesEndAsTheNonRootWorkerUser pins the user a worker
// image is left with: the last USER instruction of every stage a worker
// image is built to (the canonical image's last stage, the project recipe's
// "toolchains" target and its last stage) is 65532:65532. The OpenShell
// runtime names no user of its own, so this is the worker's.
func TestWorkerDockerfilesEndAsTheNonRootWorkerUser(t *testing.T) {
	const last = ""
	targets := map[string][]string{
		"Dockerfile":         {last},
		"Dockerfile.project": {"toolchains", last},
	}
	for name, stages := range targets {
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		users := map[string]string{}
		stage := last
		for _, line := range strings.Split(string(data), "\n") {
			fields := strings.Fields(line)
			switch {
			case len(fields) >= 2 && fields[0] == "FROM":
				stage = last
				if len(fields) == 4 && strings.EqualFold(fields[2], "AS") {
					stage = fields[3]
				}
				users[stage] = ""
			case len(fields) == 2 && fields[0] == "USER":
				users[stage] = fields[1]
			}
		}
		for _, target := range stages {
			if got := users[target]; got != "65532:65532" {
				t.Errorf("%s stage %q ends as USER %q, want 65532:65532", name, target, got)
			}
		}
	}
}
