package main

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
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
// TLS. The Makefile takes it from `factoryd build-ca-bundle` when
// BUILD_CA_BUNDLE is not given.
func buildCABundlePath() string {
	return filepath.Join(sessionconfig.ConfigDir(), "build-ca.pem")
}

// buildCAUse is the command the check prints, for the operator to copy.
func buildCAUse(path string) string {
	return fmt.Sprintf("BUILD_CA_BUNDLE=%s make install", path)
}

// buildCAFinding is what this machine can tell about TLS interception on
// its network.
type buildCAFinding struct {
	// known is false when there was nothing to check against: the probe
	// host is unreachable, or the system's shipped roots cannot be listed
	// (any platform but macOS).
	known bool
	// intercepted: the root that signs buildCAProbeHost's certificate is
	// not one the operating system ships (a corporate proxy re-signs TLS).
	intercepted bool
	// signer names the intercepting CA; empty when the machine does not
	// trust it, in which case untrusted holds the verification error and
	// no bundle can be built from the keychain.
	signer    string
	untrusted error
	// bundle is buildCABundlePath when that file holds the signer's root,
	// already or because write was set. writeErr says why writing failed.
	bundle   string
	writeErr error
}

// findBuildCA probes buildCAProbeHost. With write, an intercepting CA the
// machine trusts is written, with the system's shipped roots, to
// buildCABundlePath: pip reads the bundle in place of its own roots, so it
// holds both.
func findBuildCA(ctx context.Context, dp *deps, write bool) buildCAFinding {
	root, err := dp.host.tlsRoot(ctx, buildCAProbeHost)
	var untrusted x509.UnknownAuthorityError
	if errors.As(err, &untrusted) {
		return buildCAFinding{known: true, intercepted: true, untrusted: err}
	}
	if err != nil {
		return buildCAFinding{}
	}
	shipped, err := dp.host.shippedRootsPEM(ctx)
	if err != nil {
		return buildCAFinding{}
	}
	if pemHoldsKeyOf(shipped, root) {
		return buildCAFinding{known: true}
	}
	finding := buildCAFinding{known: true, intercepted: true, signer: root.Subject.CommonName}
	if finding.signer == "" {
		finding.signer = root.Subject.String()
	}
	path := buildCABundlePath()
	if existing, readErr := os.ReadFile(path); readErr == nil && pemHoldsKeyOf(existing, root) {
		finding.bundle = path
		return finding
	}
	if !write {
		return finding
	}
	bundle := append(bytes.TrimRight(shipped, "\n"), '\n')
	bundle = append(bundle, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: root.Raw})...)
	if finding.writeErr = writeBuildCABundle(path, bundle); finding.writeErr == nil {
		finding.bundle = path
	}
	return finding
}

// doctorCheckTLSInterception reports whether this network re-signs TLS (a
// corporate proxy) and what `make install` uses for it. Without that CA the
// image builds cannot download, and the console's npm cannot verify its
// registry: under some Node versions it dies with "Exit handler never
// called!" and never names the certificate. `make install` finds and writes
// the bundle itself (buildCABundleMain); this row says what it will find.
//
// The check is advisory, since an installed factoryd runs without the
// bundle, and skipped (ok false) when findBuildCA had nothing to check
// against. With fix it writes the bundle.
func doctorCheckTLSInterception(ctx context.Context, dp *deps, fix bool) (check doctorCheck, ok bool) {
	name := fmt.Sprintf("build CA bundle (TLS to %s)", buildCAProbeHost)
	finding := findBuildCA(ctx, dp, fix)
	path := buildCABundlePath()
	switch {
	case !finding.known:
		return doctorCheck{}, false
	case !finding.intercepted:
		return doctorCheck{Name: name, Detail: "not intercepted, none needed"}, true
	case finding.untrusted != nil:
		return doctorCheck{
			Name:     name,
			Advisory: true,
			Err:      fmt.Errorf("this network intercepts TLS with a CA this machine does not trust: %v", finding.untrusted),
			Fix:      buildCAManualStep,
		}, true
	case finding.bundle != "":
		return doctorCheck{Name: name, Detail: "intercepted by " + finding.signer + "; make install uses this bundle", Use: buildCAUse(path)}, true
	}
	intercepted := fmt.Errorf("this network intercepts TLS: %s is signed by %q, not a root the system ships; make install needs that CA", buildCAProbeHost, finding.signer)
	if finding.writeErr != nil {
		return doctorCheck{Name: name, Advisory: true, Err: fmt.Errorf("%v; could not write %s: %v", intercepted, path, finding.writeErr)}, true
	}
	return doctorCheck{
		Name:     name,
		Advisory: true,
		Err:      intercepted,
		Fix:      fmt.Sprintf("make install writes that CA and the system's roots to %s and uses it; doctor -fix writes it now", path),
	}, true
}

// buildCAManualStep is the one case nothing can figure out: the proxy's CA
// is not in the keychain, so the operator has to supply it.
var buildCAManualStep = "get the proxy's CA as a PEM file (with any public roots the proxy does not replace) and pass it: " + buildCAUse("/path/to/ca.pem")

// buildCABundleMain is the hidden `factoryd build-ca-bundle` subcommand the
// Makefile runs when BUILD_CA_BUNDLE is not given: on a network that
// intercepts TLS it writes the bundle and prints its path on stdout, the
// value the Makefile then uses; otherwise it prints nothing. What it found
// goes to stderr. It always succeeds: a machine it cannot read (offline, not
// a Mac) builds as it did without it.
func buildCABundleMain(dp *deps, stdout, stderr io.Writer) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	finding := findBuildCA(ctx, dp, true)
	switch {
	case !finding.known || !finding.intercepted:
	case finding.untrusted != nil:
		fmt.Fprintf(stderr, "warning: this network intercepts TLS with a CA this machine does not trust (%v) -- %s\n", finding.untrusted, buildCAManualStep)
	case finding.bundle == "":
		fmt.Fprintf(stderr, "warning: this network intercepts TLS (signed by %q) and %s could not be written: %v\n", finding.signer, buildCABundlePath(), finding.writeErr)
	default:
		fmt.Fprintf(stderr, "note: this network intercepts TLS (signed by %q) -- building with BUILD_CA_BUNDLE=%s\n", finding.signer, finding.bundle)
		fmt.Fprintln(stdout, finding.bundle)
	}
	return nil
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
