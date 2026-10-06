package meter

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

// writeTestCABundle writes a minimal self-signed certificate's PEM
// encoding to a temp file and returns its path.
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

// TestOutboundTransportWithBundlePreservesDefaultTransportBehavior guards
// against a bare &http.Transport{} silently dropping Proxy (HTTP(S)_PROXY
// support) and HTTP/2 -- found via adversarial review: only TLSClientConfig
// should differ from http.DefaultTransport.
func TestOutboundTransportWithBundlePreservesDefaultTransportBehavior(t *testing.T) {
	rt, err := OutboundTransport(writeTestCABundle(t))
	if err != nil {
		t.Fatalf("OutboundTransport: %v", err)
	}
	transport, ok := rt.(*http.Transport)
	if !ok {
		t.Fatalf("OutboundTransport returned %T, want *http.Transport", rt)
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

// TestOutboundTransportWithoutBundleReturnsDefaultTransport confirms the
// unset case is unchanged: exactly http.DefaultTransport, no wrapping.
func TestOutboundTransportWithoutBundleReturnsDefaultTransport(t *testing.T) {
	rt, err := OutboundTransport("")
	if err != nil {
		t.Fatalf("OutboundTransport: %v", err)
	}
	if rt != http.DefaultTransport {
		t.Errorf("OutboundTransport(\"\") = %v, want http.DefaultTransport unchanged", rt)
	}
}
