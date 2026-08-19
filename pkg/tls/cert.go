package tls

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"time"
)

// CertificateInfo contains information about a certificate
type CertificateInfo struct {
	// File path to the certificate
	FilePath string `json:"file_path"`

	// Subject common name
	Subject string `json:"subject"`

	// Issuer common name
	Issuer string `json:"issuer"`

	// NotBefore is the certificate validity start time
	NotBefore time.Time `json:"not_before"`

	// NotAfter is the certificate validity end time
	NotAfter time.Time `json:"not_after"`

	// DaysUntilExpiration is the number of days until the certificate expires
	DaysUntilExpiration int `json:"days_until_expiration"`

	// IsExpired indicates if the certificate has expired
	IsExpired bool `json:"is_expired"`

	// IsSelfSigned indicates if the certificate is self-signed
	IsSelfSigned bool `json:"is_self_signed"`

	// DNSNames are the DNS names in the certificate
	DNSNames []string `json:"dns_names"`

	// IPAddresses are the IP addresses in the certificate
	IPAddresses []string `json:"ip_addresses"`
}

// LoadCertificateInfo loads and parses certificate information from a file
func LoadCertificateInfo(certFile string) (*CertificateInfo, error) {
	certPEM, err := os.ReadFile(certFile)
	if err != nil {
		return nil, fmt.Errorf("failed to read certificate file: %w", err)
	}

	block, _ := pem.Decode(certPEM)
	if block == nil {
		return nil, fmt.Errorf("failed to decode PEM block from certificate file")
	}

	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("failed to parse certificate: %w", err)
	}

	now := time.Now()
	daysUntilExpiration := int(time.Until(cert.NotAfter).Hours() / 24)
	isExpired := now.After(cert.NotAfter)
	isSelfSigned := cert.Subject.CommonName == cert.Issuer.CommonName

	info := &CertificateInfo{
		FilePath:            certFile,
		Subject:             cert.Subject.CommonName,
		Issuer:              cert.Issuer.CommonName,
		NotBefore:           cert.NotBefore,
		NotAfter:            cert.NotAfter,
		DaysUntilExpiration: daysUntilExpiration,
		IsExpired:           isExpired,
		IsSelfSigned:        isSelfSigned,
		DNSNames:            cert.DNSNames,
	}

	// Convert IP addresses to strings
	for _, ip := range cert.IPAddresses {
		info.IPAddresses = append(info.IPAddresses, ip.String())
	}

	return info, nil
}

// LoadCertificatePair loads and validates a certificate-key pair
func LoadCertificatePair(certFile, keyFile string) (tls.Certificate, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("failed to load certificate pair: %w", err)
	}

	return cert, nil
}

// CheckCertificateExpiration checks if a certificate is expiring within the warning threshold
func CheckCertificateExpiration(certFile string, warningDays int) (*CertificateInfo, error) {
	info, err := LoadCertificateInfo(certFile)
	if err != nil {
		return nil, err
	}

	// Check if already expired
	if info.IsExpired {
		return info, fmt.Errorf("certificate has expired on %s", info.NotAfter.Format(time.RFC3339))
	}

	// Check if expiring within warning threshold
	if info.DaysUntilExpiration <= warningDays {
		return info, fmt.Errorf("certificate will expire in %d days (%s)", info.DaysUntilExpiration, info.NotAfter.Format(time.RFC3339))
	}

	return info, nil
}

// GetCertificateChain loads all certificates from a PEM file
func GetCertificateChain(certFile string) ([]*x509.Certificate, error) {
	certPEM, err := os.ReadFile(certFile)
	if err != nil {
		return nil, fmt.Errorf("failed to read certificate file: %w", err)
	}

	var certs []*x509.Certificate
	remaining := certPEM

	for {
		block, rest := pem.Decode(remaining)
		if block == nil {
			break
		}

		if block.Type == "CERTIFICATE" {
			cert, err := x509.ParseCertificate(block.Bytes)
			if err != nil {
				return nil, fmt.Errorf("failed to parse certificate: %w", err)
			}
			certs = append(certs, cert)
		}

		remaining = rest
	}

	if len(certs) == 0 {
		return nil, fmt.Errorf("no certificates found in file")
	}

	return certs, nil
}

// VerifyCertificateChain verifies that a certificate chain is valid
func VerifyCertificateChain(certFile, caFile string) error {
	certs, err := GetCertificateChain(certFile)
	if err != nil {
		return err
	}

	// Load CA certificate
	caPEM, err := os.ReadFile(caFile)
	if err != nil {
		return fmt.Errorf("failed to read CA file: %w", err)
	}

	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(caPEM) {
		return fmt.Errorf("failed to parse CA certificate")
	}

	// Verify the leaf certificate
	opts := x509.VerifyOptions{
		Roots: caPool,
	}

	// Add intermediate certificates if any
	if len(certs) > 1 {
		opts.Intermediates = x509.NewCertPool()
		for _, cert := range certs[1:] {
			opts.Intermediates.AddCert(cert)
		}
	}

	_, err = certs[0].Verify(opts)
	if err != nil {
		return fmt.Errorf("certificate chain verification failed: %w", err)
	}

	return nil
}
