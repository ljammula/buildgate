package sandbox

import (
	"os"
	"strings"
	"testing"
)

func TestWorkerImageCarriesPinnedNodePackageManagers(t *testing.T) {
	data, err := os.ReadFile("Dockerfile")
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	text := string(data)
	for _, want := range []string{
		"ARG PNPM_VERSION=10.17.1",
		"npm install -g --ignore-scripts pnpm@${PNPM_VERSION}",
		"COPY --from=pnpm /usr/local/lib/node_modules/pnpm /usr/local/lib/node_modules/pnpm",
		"ln -s /usr/local/lib/node_modules/npm/bin/npx-cli.js /usr/local/bin/npx",
		"ln -s /usr/local/lib/node_modules/pnpm/bin/pnpm.cjs /usr/local/bin/pnpm",
		"npx --version && pnpm --version",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("Dockerfile missing %q", want)
		}
	}
}

func TestWorkerImageCarriesPinnedGitWithBuildgateSmoke(t *testing.T) {
	data, err := os.ReadFile("Dockerfile")
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	text := string(data)
	for _, want := range []string{
		"ARG GIT_VERSION=2.51.2",
		"ARG GIT_SHA256=9b44c2b337ec838e10aad42439d390963904449710d30c9e7e4ba449f45da98f",
		"echo \"${GIT_SHA256}  git.tar.gz\" | sha256sum -c -",
		"COPY --from=git /usr/local/bin/git* /usr/local/bin/",
		"git -C \"$smoke\" init -q",
		"git -C \"$smoke\" add proof.txt",
		"git -C \"$smoke\" commit -qm image-proof",
		"git -C \"$sha256\" init -q --object-format=sha256",
		"git -C \"$sha256\" rev-parse --show-object-format",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("Dockerfile missing %q", want)
		}
	}
}

func TestWorkerImageCarriesOpenSSHClientWithoutServerState(t *testing.T) {
	data, err := os.ReadFile("Dockerfile")
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	text := string(data)
	for _, want := range []string{
		"openssh-client",
		"command -v ssh-keygen",
		"test ! -e /usr/sbin/sshd",
		"for key in /etc/ssh/ssh_host_*; do test ! -e \"$key\"; done",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("Dockerfile missing %q", want)
		}
	}
	if strings.Contains(text, "openssh-server") {
		t.Error("Dockerfile installs the OpenSSH server, want the client only")
	}
}
