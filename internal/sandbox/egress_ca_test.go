package sandbox

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// generateTestCABundle writes a minimal self-signed certificate's PEM
// encoding to a temp file and returns its path -- a valid input for
// ValidateEgressCABundle/egressCABundleDockerArgs in tests that need one.
func generateTestCABundle(t *testing.T) string {
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

func TestValidateEgressCABundle(t *testing.T) {
	t.Run("missing file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "does-not-exist.pem")
		if err := ValidateEgressCABundle(path); err == nil || !strings.Contains(err.Error(), path) {
			t.Fatalf("ValidateEgressCABundle(missing) = %v, want an error naming %s", err, path)
		}
	})
	t.Run("non-PEM file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "not-pem.pem")
		if err := os.WriteFile(path, []byte("this is not a certificate"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := ValidateEgressCABundle(path); err == nil || !strings.Contains(err.Error(), path) {
			t.Fatalf("ValidateEgressCABundle(non-PEM) = %v, want an error naming %s", err, path)
		}
	})
	t.Run("valid PEM", func(t *testing.T) {
		if err := ValidateEgressCABundle(generateTestCABundle(t)); err != nil {
			t.Fatalf("ValidateEgressCABundle(valid) = %v, want nil", err)
		}
	})
}
