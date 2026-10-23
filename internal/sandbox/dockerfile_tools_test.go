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
