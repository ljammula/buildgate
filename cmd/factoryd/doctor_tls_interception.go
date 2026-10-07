package main

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"buildgate/internal/sessionconfig"
)

// buildCAProbeHost is the public registry doctor asks for its certificate:
// the console's `npm ci` in `make install` downloads from it, and the image
// builds' downloads cross the same proxy.
const buildCAProbeHost = "registry.npmjs.org"

// buildCABundlePath is where `doctor -fix` writes the PEM that
// `BUILD_CA_BUNDLE=<path> make install` takes on a network that intercepts
// TLS. Nothing reads it unless the operator passes it.
func buildCABundlePath() string {
	return filepath.Join(sessionconfig.ConfigDir(), "build-ca.pem")
}

// buildCAUse is the command the check prints, for the operator to copy.
func buildCAUse(path string) string {
	return fmt.Sprintf("BUILD_CA_BUNDLE=%s make install", path)
}

// doctorCheckTLSInterception reports whether this network re-signs TLS (a
// corporate proxy) and names what `make install` must then be given. Without
// that CA the image builds cannot download, and the console's npm cannot
// verify its registry: under some Node versions it dies with "Exit handler
// never called!" and never names the certificate.
//
// Intercepted means the root that signs buildCAProbeHost's certificate is
// trusted by this machine but is not one the operating system ships. The
// check is advisory, since an installed factoryd runs without the bundle; it
// is skipped (ok false) when the host cannot be reached or the system's
// shipped roots cannot be listed (any platform but macOS). With fix, the
// shipped roots and the proxy's root are written to buildCABundlePath: pip
// reads the bundle in place of its own roots, so it holds both.
func doctorCheckTLSInterception(ctx context.Context, dp *deps, fix bool) (check doctorCheck, ok bool) {
	name := fmt.Sprintf("build CA bundle (TLS to %s)", buildCAProbeHost)
	root, err := dp.host.tlsRoot(ctx, buildCAProbeHost)
	var untrusted x509.UnknownAuthorityError
	if errors.As(err, &untrusted) {
		return doctorCheck{
			Name:     name,
			Advisory: true,
			Err:      fmt.Errorf("this network intercepts TLS with a CA this machine does not trust: %v", err),
			Fix:      "get the proxy's CA as a PEM file (with any public roots the proxy does not replace) and pass it: " + buildCAUse("/path/to/ca.pem"),
		}, true
	}
	if err != nil {
		return doctorCheck{}, false
	}
	shipped, err := dp.host.shippedRootsPEM(ctx)
	if err != nil {
		return doctorCheck{}, false
	}
	if pemHoldsKeyOf(shipped, root) {
		return doctorCheck{Name: name, Detail: "not intercepted, none needed"}, true
	}
	path := buildCABundlePath()
	signer := root.Subject.CommonName
	if signer == "" {
		signer = root.Subject.String()
	}
	if existing, readErr := os.ReadFile(path); readErr == nil && pemHoldsKeyOf(existing, root) {
		return doctorCheck{Name: name, Detail: "intercepted by " + signer, Use: buildCAUse(path)}, true
	}
	intercepted := fmt.Errorf("this network intercepts TLS: %s is signed by %q, not a root the system ships; make install needs that CA", buildCAProbeHost, signer)
	if !fix {
		return doctorCheck{
			Name:     name,
			Advisory: true,
			Err:      intercepted,
			Fix:      fmt.Sprintf("rerun with -fix to write that CA and the system's roots to %s, then pass it: %s", path, buildCAUse(path)),
		}, true
	}
	bundle := append(bytes.TrimRight(shipped, "\n"), '\n')
	bundle = append(bundle, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: root.Raw})...)
	if err := writeBuildCABundle(path, bundle); err != nil {
		return doctorCheck{Name: name, Advisory: true, Err: fmt.Errorf("%v; could not write %s: %v", intercepted, path, err)}, true
	}
	return doctorCheck{Name: name, Detail: "intercepted by " + signer + "; wrote " + path, Use: buildCAUse(path)}, true
}

// writeBuildCABundle replaces path with bundle in one rename.
func writeBuildCABundle(path string, bundle []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, bundle, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// pemHoldsKeyOf reports whether bundle holds a certificate with cert's
// subject and public key: the same CA, whichever of its certificates
// (a reissue, a cross-sign) the bundle carries.
func pemHoldsKeyOf(bundle []byte, cert *x509.Certificate) bool {
	for rest := bundle; ; {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			return false
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		candidate, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			continue
		}
		if bytes.Equal(candidate.RawSubject, cert.RawSubject) && bytes.Equal(candidate.RawSubjectPublicKeyInfo, cert.RawSubjectPublicKeyInfo) {
			return true
		}
	}
}

// tlsRoot returns the root of the chain this machine verified host's
// certificate against, reached the way the build's downloads are (the
// environment's HTTPS proxy, when set).
func (impl realHost) tlsRoot(ctx context.Context, host string) (*x509.Certificate, error) {
	if !probePublicTLS {
		return nil, errors.New("this build does not probe public hosts")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, "https://"+host+"/", nil)
	if err != nil {
		return nil, err
	}
	transport := &http.Transport{Proxy: http.ProxyFromEnvironment}
	defer transport.CloseIdleConnections()
	resp, err := transport.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.TLS == nil || len(resp.TLS.VerifiedChains) == 0 || len(resp.TLS.VerifiedChains[0]) == 0 {
		return nil, fmt.Errorf("no verified certificate chain for %s", host)
	}
	chain := resp.TLS.VerifiedChains[0]
	return chain[len(chain)-1], nil
}

// macOSShippedRootsKeychain holds the roots Apple ships. A CA an
// administrator or a device-management profile adds lands in another
// keychain, which is what tells a proxy's CA from a public one.
const macOSShippedRootsKeychain = "/System/Library/Keychains/SystemRootCertificates.keychain"

// shippedRootsPEM lists, as PEM, the public roots the operating system
// ships. macOS only.
func (impl realHost) shippedRootsPEM(ctx context.Context) ([]byte, error) {
	if runtime.GOOS != "darwin" {
		return nil, fmt.Errorf("listing the system's shipped roots is not supported on %s", runtime.GOOS)
	}
	out, err := exec.CommandContext(ctx, "/usr/bin/security", "find-certificate", "-a", "-p", macOSShippedRootsKeychain).Output()
	if err != nil {
		return nil, fmt.Errorf("security find-certificate: %w", err)
	}
	return out, nil
}
