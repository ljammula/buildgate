package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHostSideImageDownloadsSupportBuildCABundle(t *testing.T) {
	t.Parallel()

	root := filepath.Join("..", "..")
	recipes := map[string][]string{
		filepath.Join(root, "internal", "sandbox", "Dockerfile"): {
			"RUN --mount=type=secret,id=build-ca,required=false",
			"NODE_EXTRA_CA_CERTS=/run/secrets/build-ca",
			"SSL_CERT_FILE=/run/secrets/build-ca",
		},
		filepath.Join(root, "internal", "registryproxy", "Dockerfile"): {
			"RUN --mount=type=secret,id=build-ca,required=false",
			"SSL_CERT_FILE=/run/secrets/build-ca",
		},
		filepath.Join(root, "internal", "sandbox", "Dockerfile.project"): {
			"RUN --mount=type=secret,id=build-ca,required=false",
			"NODE_EXTRA_CA_CERTS=/run/secrets/build-ca",
			"SSL_CERT_FILE=/run/secrets/build-ca",
			"PIP_CERT=/run/secrets/build-ca",
		},
	}
	for path, required := range recipes {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		text := string(data)
		for _, want := range required {
			if !strings.Contains(text, want) {
				t.Errorf("%s missing %q", path, want)
			}
		}
	}

	makefile, err := os.ReadFile(filepath.Join(root, "Makefile"))
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	makeText := string(makefile)
	for _, want := range []string{
		"BUILD_CA_BUNDLE ?=",
		`--secret "id=build-ca,src=$$ca_bundle"`,
		`ca_bundle="$(BUILD_CA_BUNDLE)"`,
	} {
		if !strings.Contains(makeText, want) {
			t.Errorf("Makefile missing %q", want)
		}
	}
}
