package registryproxy

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeTestCABundle writes a one-certificate PEM bundle and returns its path.
func writeTestCABundle(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate test key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test-egress-ca"},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(time.Hour),
		IsCA:         true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create test certificate: %v", err)
	}
	path := filepath.Join(t.TempDir(), "egress-ca.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatalf("write test CA bundle: %v", err)
	}
	return path
}

// TestOutboundTransportWithBundlePreservesDefaultTransportBehavior: adding a
// CA bundle keeps http.DefaultTransport's proxy, timeout and HTTP/2 settings.
func TestOutboundTransportWithBundlePreservesDefaultTransportBehavior(t *testing.T) {
	rt, err := outboundTransport(writeTestCABundle(t))
	if err != nil {
		t.Fatalf("outboundTransport: %v", err)
	}
	transport, ok := rt.(*http.Transport)
	if !ok {
		t.Fatalf("outboundTransport returned %T, want *http.Transport", rt)
	}
	if transport.Proxy == nil {
		t.Error("Proxy = nil, want http.ProxyFromEnvironment (HTTP(S)_PROXY support lost)")
	}
	if !transport.ForceAttemptHTTP2 {
		t.Error("ForceAttemptHTTP2 = false, want true (silently downgraded to HTTP/1.1)")
	}
	if transport.TLSClientConfig == nil || transport.TLSClientConfig.RootCAs == nil {
		t.Error("TLSClientConfig.RootCAs = nil, want the bundle's cert pool")
	}
}

func TestOutboundTransportWithoutBundleReturnsDefaultTransport(t *testing.T) {
	rt, err := outboundTransport("")
	if err != nil {
		t.Fatalf("outboundTransport: %v", err)
	}
	if rt != http.DefaultTransport {
		t.Errorf("outboundTransport(\"\") = %v, want http.DefaultTransport unchanged", rt)
	}
}
