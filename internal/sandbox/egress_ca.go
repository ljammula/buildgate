package sandbox

import (
	"crypto/x509"
	"fmt"
	"os"
	"path/filepath"
)

// EgressCABundleContainerPath is the fixed in-container path a configured
// -egress-ca-bundle is mounted at in the registry-proxy container -- see
// RegistryProxySpec.CABundlePath.
// The parent is part of the distroless image's existing system CA directory:
// Docker's legacy --volume syntax creates a missing file destination as a
// directory, which would make the proxy fail before it can start when the
// parent path is absent.
// Exported so cmd/factoryd's doctor package can mount the same bundle at
// the same path for its own route-upstream reachability probe container.
const EgressCABundleContainerPath = "/etc/ssl/certs/factoryd-egress-ca.pem"

// ValidateEgressCABundle checks that path names a readable file containing
// at least one PEM certificate, returning an error naming path otherwise.
// Called wherever -egress-ca-bundle/egress_ca_bundle is accepted, so a
// corporate TLS-interception CA that doesn't exist or doesn't parse is
// caught at startup rather than as an opaque certificate error from deep
// inside a registry-proxy container.
func ValidateEgressCABundle(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("egress CA bundle %s: %w", path, err)
	}
	if !x509.NewCertPool().AppendCertsFromPEM(data) {
		return fmt.Errorf("egress CA bundle %s: no PEM certificate found", path)
	}
	return nil
}

// egressCABundleDockerArgs returns the `docker run` --volume/--env pair
// that bind-mounts path read-only into a launched registry-proxy
// container at egressCABundleContainerPath and points
// FACTORYD_EGRESS_CA_BUNDLE at it, or nil when path is empty.
// cmd/registry-proxy reads that env var and, when
// set, adds its certificate(s) to its own outbound http.Transport's
// RootCAs (see internal/registryproxy.
// Config.CABundlePath's own doc comment for why that -- not SSL_CERT_FILE
// -- is the mechanism: the image is gcr.io/distroless/static-debian12,
// which already ships its own system CA roots, and SSL_CERT_FILE replaces
// Go's default trust file list rather than adding to it, which would drop
// every public CA the same container also needs to reach api.anthropic.com/
// registry.npmjs.org/etc. Appending to x509.SystemCertPool() in-process
// adds the corporate CA without removing anything, regardless of which
// base image either Dockerfile ever moves to.
// stageEgressCABundle copies path's contents to a fresh 0644 file in a
// dedicated per-launch temp directory and returns that copy's path plus a
// cleanup func that removes the directory -- the mount source
// egressCABundleDockerArgs should be given, not the operator's own path.
// The registry-proxy container runs as UID 65532 (see
// LaunchRegistryProxy's own --user), so a read-only bind mount
// preserves the host file's own permissions -- an operator's PEM kept at a
// stricter mode (0600 is a common habit for anything under a "cert"
// directory, even though a CA certificate is public) would otherwise mount
// unreadable inside the container and fail the server at startup with a
// confusing "permission denied", not a real trust-store error. Since a CA
// certificate is public, copying it out costs nothing in confidentiality
// and avoids requiring the operator to chmod their own file. Returns
// ("", func(){}, nil) when path is empty.
func stageEgressCABundle(path, parent string) (stagedPath string, cleanup func(), err error) {
	if path == "" {
		return "", func() {}, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", nil, fmt.Errorf("read CA bundle: %w", err)
	}
	dir, err := os.MkdirTemp(parent, ".factoryd-egress-ca-")
	if err != nil {
		return "", nil, fmt.Errorf("create egress CA bundle staging directory: %w", err)
	}
	staged := filepath.Join(dir, "egress-ca.pem")
	if err := os.WriteFile(staged, data, 0o644); err != nil {
		os.RemoveAll(dir)
		return "", nil, fmt.Errorf("stage egress CA bundle: %w", err)
	}
	return staged, func() { _ = os.RemoveAll(dir) }, nil
}

func egressCABundleDockerArgs(path string) []string {
	if path == "" {
		return nil
	}
	return []string{
		"--volume", path + ":" + EgressCABundleContainerPath + ":ro",
		"--env", "FACTORYD_EGRESS_CA_BUNDLE=" + EgressCABundleContainerPath,
	}
}
