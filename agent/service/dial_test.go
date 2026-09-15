package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"log/slog"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestTransportCredsEmptyDirIsInsecureByChoice pins the one case that must
// still succeed: no -tls-dir at all is the explicit, deliberate opt-out, not
// a failure.
func TestTransportCredsEmptyDirIsInsecureByChoice(t *testing.T) {
	if _, err := transportCreds("", discardLogger()); err != nil {
		t.Errorf("empty -tls-dir must connect insecure, not fail: %v", err)
	}
}

// TestTransportCredsFailsClosedOnMissingMaterial is the actual fix: a
// -tls-dir that does not exist (or is empty) used to fall back to an
// insecure connection with only a logged warning. It must now refuse.
func TestTransportCredsFailsClosedOnMissingMaterial(t *testing.T) {
	dir := t.TempDir() // exists, but holds none of ca.pem/agent-cert.pem/agent-key.pem
	_, err := transportCreds(dir, discardLogger())
	if err == nil {
		t.Fatal("missing mTLS material must fail closed (return an error), not silently connect insecure")
	}
}

// TestTransportCredsFailsClosedOnInvalidCA covers the second silent-fallback
// site that existed: a ca.pem that parses as a file but not as a certificate.
func TestTransportCredsFailsClosedOnInvalidCA(t *testing.T) {
	dir := t.TempDir()
	writeValidAgentCertAndKey(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "ca.pem"), []byte("not a certificate"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := transportCreds(dir, discardLogger())
	if err == nil {
		t.Fatal("an invalid CA certificate must fail closed, not silently connect insecure")
	}
}

// TestTransportCredsFailsClosedOnPartialMaterial covers a cert present with
// no matching key (or vice versa) -- a half-provisioned -tls-dir, which is
// exactly the shape a partially-failed onboarding or a manual mistake leaves
// behind.
func TestTransportCredsFailsClosedOnPartialMaterial(t *testing.T) {
	dir := t.TempDir()
	writeValidCA(t, dir)
	// agent-cert.pem present, agent-key.pem missing.
	certPEM, _ := generateSelfSignedCert(t)
	if err := os.WriteFile(filepath.Join(dir, "agent-cert.pem"), certPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := transportCreds(dir, discardLogger())
	if err == nil {
		t.Fatal("a cert with no matching key must fail closed, not silently connect insecure")
	}
}

// TestTransportCredsSucceedsOnValidMaterial is the control: real material
// must still work, or the fail-closed change would have broken the actual
// mTLS path along with fixing the fallback.
func TestTransportCredsSucceedsOnValidMaterial(t *testing.T) {
	dir := t.TempDir()
	writeValidCA(t, dir)
	writeValidAgentCertAndKey(t, dir)
	opt, err := transportCreds(dir, discardLogger())
	if err != nil {
		t.Fatalf("valid mTLS material must succeed: %v", err)
	}
	if opt == nil {
		t.Fatal("valid mTLS material returned a nil DialOption")
	}
}

func writeValidCA(t *testing.T, dir string) {
	t.Helper()
	certPEM, _ := generateSelfSignedCert(t)
	if err := os.WriteFile(filepath.Join(dir, "ca.pem"), certPEM, 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeValidAgentCertAndKey(t *testing.T, dir string) {
	t.Helper()
	certPEM, keyPEM := generateSelfSignedCert(t)
	if err := os.WriteFile(filepath.Join(dir, "agent-cert.pem"), certPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "agent-key.pem"), keyPEM, 0o644); err != nil {
		t.Fatal(err)
	}
}

func generateSelfSignedCert(t *testing.T) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 64))
	if err != nil {
		t.Fatal(err)
	}
	tmpl := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM
}
