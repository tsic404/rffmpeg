package tls

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// generateTestCertificate creates a test certificate and key for testing
func generateTestCertificate(t *testing.T, certPath, keyPath string, daysUntilExpiration int) {
	t.Helper()

	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("Failed to generate private key: %v", err)
	}

	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			CommonName: "test.example.com",
		},
		NotBefore: time.Now(),
		NotAfter:  time.Now().AddDate(0, 0, daysUntilExpiration),
		KeyUsage:  x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{
			x509.ExtKeyUsageServerAuth,
		},
		DNSNames: []string{"test.example.com", "localhost"},
	}

	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &privateKey.PublicKey, privateKey)
	if err != nil {
		t.Fatalf("Failed to create certificate: %v", err)
	}

	certFile, err := os.Create(certPath)
	if err != nil {
		t.Fatalf("Failed to create cert file: %v", err)
	}
	defer certFile.Close()

	if err := pem.Encode(certFile, &pem.Block{Type: "CERTIFICATE", Bytes: certDER}); err != nil {
		t.Fatalf("Failed to write cert: %v", err)
	}

	keyFile, err := os.Create(keyPath)
	if err != nil {
		t.Fatalf("Failed to create key file: %v", err)
	}
	defer keyFile.Close()

	keyDER := x509.MarshalPKCS1PrivateKey(privateKey)
	if err := pem.Encode(keyFile, &pem.Block{Type: "RSA PRIVATE KEY", Bytes: keyDER}); err != nil {
		t.Fatalf("Failed to write key: %v", err)
	}
}

// generateTestCACertificate creates a test CA certificate for mTLS testing
func generateTestCACertificate(t *testing.T, caPath string) {
	t.Helper()

	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("Failed to generate private key: %v", err)
	}

	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			CommonName: "Test CA",
		},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().AddDate(1, 0, 0),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &privateKey.PublicKey, privateKey)
	if err != nil {
		t.Fatalf("Failed to create CA certificate: %v", err)
	}

	caFile, err := os.Create(caPath)
	if err != nil {
		t.Fatalf("Failed to create CA file: %v", err)
	}
	defer caFile.Close()

	if err := pem.Encode(caFile, &pem.Block{Type: "CERTIFICATE", Bytes: certDER}); err != nil {
		t.Fatalf("Failed to write CA cert: %v", err)
	}
}

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()

	if cfg.Enabled != false {
		t.Error("Default config should have TLS disabled")
	}
	if cfg.MinVersion != tls.VersionTLS12 {
		t.Error("Default TLS version should be TLS 1.2")
	}
	if cfg.ExpirationWarningDays != 30 {
		t.Error("Default expiration warning should be 30 days")
	}
}

func TestConfigValidate(t *testing.T) {
	// Test disabled TLS (should pass)
	cfg := DefaultConfig()
	if err := cfg.Validate(); err != nil {
		t.Errorf("Disabled TLS should validate: %v", err)
	}

	// Create temp directory for test files
	tmpDir := t.TempDir()
	certPath := filepath.Join(tmpDir, "cert.pem")
	keyPath := filepath.Join(tmpDir, "key.pem")
	caPath := filepath.Join(tmpDir, "ca.pem")

	generateTestCertificate(t, certPath, keyPath, 365)
	generateTestCACertificate(t, caPath)

	// Test enabled TLS with valid files
	cfg = DefaultConfig()
	cfg.Enabled = true
	cfg.CertFile = certPath
	cfg.KeyFile = keyPath

	if err := cfg.Validate(); err != nil {
		t.Errorf("Valid TLS config should pass: %v", err)
	}

	// Test mTLS with valid files
	cfg.MTLS = true
	cfg.ClientCAFile = caPath

	if err := cfg.Validate(); err != nil {
		t.Errorf("Valid mTLS config should pass: %v", err)
	}

	// Test missing cert file
	cfg = DefaultConfig()
	cfg.Enabled = true
	cfg.CertFile = "/nonexistent/cert.pem"
	cfg.KeyFile = keyPath

	if err := cfg.Validate(); err == nil {
		t.Error("Missing cert file should fail validation")
	}

	// Test missing key file
	cfg = DefaultConfig()
	cfg.Enabled = true
	cfg.CertFile = certPath
	cfg.KeyFile = "/nonexistent/key.pem"

	if err := cfg.Validate(); err == nil {
		t.Error("Missing key file should fail validation")
	}
}

func TestLoadCertificate(t *testing.T) {
	tmpDir := t.TempDir()
	certPath := filepath.Join(tmpDir, "cert.pem")
	keyPath := filepath.Join(tmpDir, "key.pem")

	generateTestCertificate(t, certPath, keyPath, 365)

	cfg := &Config{
		CertFile: certPath,
		KeyFile:  keyPath,
	}

	cert, err := cfg.LoadCertificate()
	if err != nil {
		t.Fatalf("Failed to load certificate: %v", err)
	}

	if cert.PrivateKey == nil {
		t.Error("Certificate should have private key")
	}
	if len(cert.Certificate) == 0 {
		t.Error("Certificate should have certificate data")
	}
}

func TestLoadClientCA(t *testing.T) {
	tmpDir := t.TempDir()
	caPath := filepath.Join(tmpDir, "ca.pem")

	generateTestCACertificate(t, caPath)

	cfg := &Config{
		ClientCAFile: caPath,
	}

	pool, err := cfg.LoadClientCA()
	if err != nil {
		t.Fatalf("Failed to load client CA: %v", err)
	}

	if pool == nil {
		t.Error("CA pool should not be nil")
	}
}

func TestToTLSConfig(t *testing.T) {
	tmpDir := t.TempDir()
	certPath := filepath.Join(tmpDir, "cert.pem")
	keyPath := filepath.Join(tmpDir, "key.pem")
	caPath := filepath.Join(tmpDir, "ca.pem")

	generateTestCertificate(t, certPath, keyPath, 365)
	generateTestCACertificate(t, caPath)

	// Test TLS config
	cfg := DefaultConfig()
	cfg.Enabled = true
	cfg.CertFile = certPath
	cfg.KeyFile = keyPath

	tlsConfig, err := cfg.ToTLSConfig()
	if err != nil {
		t.Fatalf("Failed to create TLS config: %v", err)
	}

	if tlsConfig == nil {
		t.Fatal("TLS config should not be nil")
	}
	if len(tlsConfig.Certificates) != 1 {
		t.Error("Should have one certificate")
	}
	if tlsConfig.MinVersion != tls.VersionTLS12 {
		t.Error("Min version should be TLS 1.2")
	}

	// Test mTLS config
	cfg.MTLS = true
	cfg.ClientCAFile = caPath

	tlsConfig, err = cfg.ToTLSConfig()
	if err != nil {
		t.Fatalf("Failed to create mTLS config: %v", err)
	}

	if tlsConfig.ClientCAs == nil {
		t.Error("mTLS should have client CA pool")
	}
	if tlsConfig.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Error("mTLS should require and verify client cert")
	}

	// Test disabled TLS
	cfg = DefaultConfig()
	tlsConfig, err = cfg.ToTLSConfig()
	if err != nil {
		t.Fatalf("Disabled TLS should not error: %v", err)
	}
	if tlsConfig != nil {
		t.Error("Disabled TLS should return nil config")
	}
}

func TestLoadCertificateInfo(t *testing.T) {
	tmpDir := t.TempDir()
	certPath := filepath.Join(tmpDir, "cert.pem")
	keyPath := filepath.Join(tmpDir, "key.pem")

	generateTestCertificate(t, certPath, keyPath, 365)

	info, err := LoadCertificateInfo(certPath)
	if err != nil {
		t.Fatalf("Failed to load certificate info: %v", err)
	}

	if info.Subject != "test.example.com" {
		t.Errorf("Subject should be test.example.com, got %s", info.Subject)
	}
	if info.IsExpired {
		t.Error("Certificate should not be expired")
	}
	if !info.IsSelfSigned {
		t.Error("Test certificate is self-signed")
	}
	if info.DaysUntilExpiration < 360 || info.DaysUntilExpiration > 366 {
		t.Errorf("Days until expiration should be around 365, got %d", info.DaysUntilExpiration)
	}
}

func TestCheckCertificateExpiration(t *testing.T) {
	tmpDir := t.TempDir()
	certPath := filepath.Join(tmpDir, "cert.pem")
	keyPath := filepath.Join(tmpDir, "key.pem")

	// Test certificate that expires in 10 days
	generateTestCertificate(t, certPath, keyPath, 10)

	// Check with 30 day warning threshold (should warn)
	info, err := CheckCertificateExpiration(certPath, 30)
	if err == nil {
		t.Error("Should warn about certificate expiring soon")
	}
	if info == nil {
		t.Fatal("Should return certificate info even when warning")
	}
	if info.DaysUntilExpiration > 10 {
		t.Errorf("Days until expiration should be around 10, got %d", info.DaysUntilExpiration)
	}

	// Check with 5 day warning threshold (should not warn)
	info, err = CheckCertificateExpiration(certPath, 5)
	if err != nil {
		t.Errorf("Should not warn with 5 day threshold: %v", err)
	}

	// Test expired certificate
	certPath2 := filepath.Join(tmpDir, "cert2.pem")
	keyPath2 := filepath.Join(tmpDir, "key2.pem")
	generateTestCertificate(t, certPath2, keyPath2, -1)

	info, err = CheckCertificateExpiration(certPath2, 30)
	if err == nil {
		t.Error("Should error for expired certificate")
	}
	if info == nil {
		t.Fatal("Should return certificate info for expired cert")
	}
	if !info.IsExpired {
		t.Error("Certificate should be marked as expired")
	}
}
