package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// testRootCA returns a self-signed CA certificate and its PEM.
func testRootCA(t *testing.T, commonName string) (*x509.Certificate, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// tlsInterceptionDeps is a test deps whose host sees root as the signer of
// the probe host and ships shipped as its public roots, with the session
// config directory (where the bundle is written) under a temp dir.
func tlsInterceptionDeps(t *testing.T, root *x509.Certificate, shipped []byte) *deps {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dp := newTestDeps(t)
	fakeHostOf(dp).tlsRootFn = func(_ context.Context, host string) (*x509.Certificate, error) {
		if host != buildCAProbeHost {
			t.Errorf("probed %q, want %q", host, buildCAProbeHost)
		}
		return root, nil
	}
	fakeHostOf(dp).shippedRootsPEMFn = func(context.Context) ([]byte, error) { return shipped, nil }
	return dp
}

func TestDoctorTLSInterceptionPassesOnAShippedRoot(t *testing.T) {
	public, publicPEM := testRootCA(t, "Public Root")
	dp := tlsInterceptionDeps(t, public, publicPEM)
	check, ok := doctorCheckTLSInterception(context.Background(), dp, true)
	if !ok || check.Err != nil || check.Use != "" {
		t.Fatalf("check = %+v, ok = %v; want a plain pass", check, ok)
	}
	if _, err := os.Stat(buildCABundlePath()); !os.IsNotExist(err) {
		t.Errorf("a bundle was written on a network that does not intercept TLS (stat err %v)", err)
	}
}

func TestDoctorTLSInterceptionWarnsAndNamesWhatToPass(t *testing.T) {
	_, publicPEM := testRootCA(t, "Public Root")
	proxy, _ := testRootCA(t, "Corp Proxy CA")
	dp := tlsInterceptionDeps(t, proxy, publicPEM)
	check, ok := doctorCheckTLSInterception(context.Background(), dp, false)
	if !ok || check.Err == nil || !check.Advisory {
		t.Fatalf("check = %+v, ok = %v; want an advisory warning", check, ok)
	}
	if !strings.Contains(check.Err.Error(), "Corp Proxy CA") {
		t.Errorf("warning does not name the signer: %v", check.Err)
	}
	if !strings.Contains(check.Fix, "-fix") || !strings.Contains(check.Fix, "make install writes") || !strings.Contains(check.Fix, buildCABundlePath()) {
		t.Errorf("fix = %q, want make install, -fix and %q", check.Fix, buildCABundlePath())
	}
	if _, err := os.Stat(buildCABundlePath()); !os.IsNotExist(err) {
		t.Errorf("a bundle was written without -fix (stat err %v)", err)
	}
}

func TestDoctorTLSInterceptionFixWritesTheBundleAndPrintsTheCommand(t *testing.T) {
	public, publicPEM := testRootCA(t, "Public Root")
	proxy, _ := testRootCA(t, "Corp Proxy CA")
	dp := tlsInterceptionDeps(t, proxy, publicPEM)
	check, ok := doctorCheckTLSInterception(context.Background(), dp, true)
	want := "BUILD_CA_BUNDLE=" + buildCABundlePath() + " make install"
	if !ok || check.Err != nil || check.Use != want {
		t.Fatalf("check = %+v, ok = %v; want a pass with use %q", check, ok, want)
	}
	bundle, err := os.ReadFile(buildCABundlePath())
	if err != nil {
		t.Fatal(err)
	}
	if !pemHoldsKeyOf(bundle, proxy) || !pemHoldsKeyOf(bundle, public) {
		t.Errorf("bundle must hold the proxy's root and the shipped roots:\n%s", bundle)
	}

	// With the bundle in place, a run without -fix passes and still prints
	// the command, and leaves the file alone.
	again, ok := doctorCheckTLSInterception(context.Background(), dp, false)
	if !ok || again.Err != nil || again.Use != want {
		t.Fatalf("second check = %+v, ok = %v; want a pass with use %q", again, ok, want)
	}
	var out bytes.Buffer
	runDoctorChecks([]doctorCheck{again}, &out)
	if !strings.Contains(out.String(), "use: "+want) {
		t.Errorf("doctor output does not show what to pass:\n%s", out.String())
	}
}

func TestDoctorTLSInterceptionFixReplacesABundleOfAnotherProxy(t *testing.T) {
	_, publicPEM := testRootCA(t, "Public Root")
	old, oldPEM := testRootCA(t, "Old Proxy CA")
	proxy, _ := testRootCA(t, "Corp Proxy CA")
	dp := tlsInterceptionDeps(t, proxy, publicPEM)
	if err := writeBuildCABundle(buildCABundlePath(), oldPEM); err != nil {
		t.Fatal(err)
	}
	if check, _ := doctorCheckTLSInterception(context.Background(), dp, false); check.Err == nil {
		t.Fatalf("a bundle without this proxy's root passed: %+v", check)
	}
	if check, _ := doctorCheckTLSInterception(context.Background(), dp, true); check.Err != nil {
		t.Fatalf("-fix: %+v", check)
	}
	bundle, err := os.ReadFile(buildCABundlePath())
	if err != nil {
		t.Fatal(err)
	}
	if !pemHoldsKeyOf(bundle, proxy) || pemHoldsKeyOf(bundle, old) {
		t.Errorf("bundle must hold the current proxy's root and not the old one")
	}
}

func TestDoctorTLSInterceptionUntrustedProxyNamesTheManualStep(t *testing.T) {
	proxy, publicPEM := testRootCA(t, "Corp Proxy CA")
	dp := tlsInterceptionDeps(t, proxy, publicPEM)
	fakeHostOf(dp).tlsRootFn = func(context.Context, string) (*x509.Certificate, error) {
		return nil, x509.UnknownAuthorityError{Cert: proxy}
	}
	check, ok := doctorCheckTLSInterception(context.Background(), dp, true)
	if !ok || check.Err == nil || !check.Advisory || !strings.Contains(check.Fix, "BUILD_CA_BUNDLE=/path/to/ca.pem make install") {
		t.Fatalf("check = %+v, ok = %v; want an advisory warning naming BUILD_CA_BUNDLE", check, ok)
	}
}

func TestDoctorTLSInterceptionSkippedWhenNothingToCheckAgainst(t *testing.T) {
	proxy, publicPEM := testRootCA(t, "Corp Proxy CA")
	t.Run("host unreachable", func(t *testing.T) {
		dp := tlsInterceptionDeps(t, proxy, publicPEM)
		fakeHostOf(dp).tlsRootFn = func(context.Context, string) (*x509.Certificate, error) {
			return nil, errors.New("dial tcp: no route to host")
		}
		if check, ok := doctorCheckTLSInterception(context.Background(), dp, true); ok {
			t.Errorf("check reported offline: %+v", check)
		}
	})
	t.Run("no shipped roots", func(t *testing.T) {
		dp := tlsInterceptionDeps(t, proxy, publicPEM)
		fakeHostOf(dp).shippedRootsPEMFn = func(context.Context) ([]byte, error) {
			return nil, errors.New("not supported on linux")
		}
		if check, ok := doctorCheckTLSInterception(context.Background(), dp, true); ok {
			t.Errorf("check reported with no shipped roots: %+v", check)
		}
	})
	t.Run("default test host", func(t *testing.T) {
		if check, ok := doctorCheckTLSInterception(context.Background(), newTestDeps(t), true); ok {
			t.Errorf("newTestDeps' host must not probe: %+v", check)
		}
	})
}

// TestRealTLSRootReportsASignerThisMachineDoesNotTrust runs the real probe
// against a loopback server whose certificate nothing on this machine
// trusts, the way a proxy with an uninstalled CA answers: the error must be
// the one doctorCheckTLSInterception turns into its warning.
func TestRealTLSRootReportsASignerThisMachineDoesNotTrust(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer server.Close()
	t.Setenv("HTTPS_PROXY", "")
	t.Setenv("https_proxy", "")
	_, err := realHost{}.tlsRoot(context.Background(), strings.TrimPrefix(server.URL, "https://"))
	var untrusted x509.UnknownAuthorityError
	if !errors.As(err, &untrusted) {
		t.Fatalf("tlsRoot error = %v (%T), want an x509.UnknownAuthorityError", err, err)
	}
}

// TestBuildCABundleCommandPrintsOnlyABundleToUse covers what the Makefile
// reads from `factoryd build-ca-bundle`: stdout is the bundle path on an
// intercepted network and empty in every other case, and the command never
// fails.
func TestBuildCABundleCommandPrintsOnlyABundleToUse(t *testing.T) {
	public, publicPEM := testRootCA(t, "Public Root")
	proxy, _ := testRootCA(t, "Corp Proxy CA")
	run := func(t *testing.T, dp *deps) (stdout, stderr string) {
		t.Helper()
		var out, errOut bytes.Buffer
		if err := buildCABundleMain(dp, &out, &errOut); err != nil {
			t.Fatalf("build-ca-bundle failed: %v", err)
		}
		return out.String(), errOut.String()
	}
	t.Run("intercepted", func(t *testing.T) {
		dp := tlsInterceptionDeps(t, proxy, publicPEM)
		stdout, stderr := run(t, dp)
		if stdout != buildCABundlePath()+"\n" {
			t.Errorf("stdout = %q, want the bundle path alone", stdout)
		}
		if !strings.Contains(stderr, "Corp Proxy CA") || !strings.Contains(stderr, "BUILD_CA_BUNDLE="+buildCABundlePath()) {
			t.Errorf("stderr does not say what was found and used: %q", stderr)
		}
		bundle, err := os.ReadFile(buildCABundlePath())
		if err != nil || !pemHoldsKeyOf(bundle, proxy) || !pemHoldsKeyOf(bundle, public) {
			t.Errorf("bundle must hold the proxy's root and the shipped roots (read err %v)", err)
		}
	})
	t.Run("not intercepted", func(t *testing.T) {
		if stdout, stderr := run(t, tlsInterceptionDeps(t, public, publicPEM)); stdout != "" || stderr != "" {
			t.Errorf("stdout = %q, stderr = %q; want silence", stdout, stderr)
		}
	})
	t.Run("nothing to check against", func(t *testing.T) {
		if stdout, stderr := run(t, newTestDeps(t)); stdout != "" || stderr != "" {
			t.Errorf("stdout = %q, stderr = %q; want silence", stdout, stderr)
		}
	})
	t.Run("untrusted proxy", func(t *testing.T) {
		dp := tlsInterceptionDeps(t, proxy, publicPEM)
		fakeHostOf(dp).tlsRootFn = func(context.Context, string) (*x509.Certificate, error) {
			return nil, x509.UnknownAuthorityError{Cert: proxy}
		}
		stdout, stderr := run(t, dp)
		if stdout != "" || !strings.Contains(stderr, "BUILD_CA_BUNDLE=/path/to/ca.pem make install") {
			t.Errorf("stdout = %q, stderr = %q; want no path and the manual step", stdout, stderr)
		}
	})
}
