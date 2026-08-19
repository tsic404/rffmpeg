package tls

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Config holds TLS/mTLS configuration
type Config struct {
	// Enable TLS
	Enabled bool `json:"enabled" yaml:"enabled"`

	// Server certificate and key paths
	CertFile string `json:"cert_file" yaml:"cert_file"`
	KeyFile  string `json:"key_file" yaml:"key_file"`

	// CA certificate for client verification (mTLS)
	ClientCAFile string `json:"client_ca_file" yaml:"client_ca_file"`

	// mTLS mode - require client certificate verification
	MTLS bool `json:"mtls" yaml:"mtls"`

	// Minimum TLS version (default: TLS 1.2)
	MinVersion uint16 `json:"min_version" yaml:"min_version"`

	// Certificate expiration warning threshold (days before expiration)
	ExpirationWarningDays int `json:"expiration_warning_days" yaml:"expiration_warning_days"`

	// Certificate expiration check interval
	ExpirationCheckInterval time.Duration `json:"expiration_check_interval" yaml:"expiration_check_interval"`
}

// DefaultConfig returns a Config with sensible defaults
func DefaultConfig() *Config {
	return &Config{
		Enabled:                 false,
		MinVersion:              tls.VersionTLS12,
		ExpirationWarningDays:   30,
		ExpirationCheckInterval: 24 * time.Hour,
	}
}

// LoadCertificate loads the server certificate and key
func (c *Config) LoadCertificate() (tls.Certificate, error) {
	if c.CertFile == "" || c.KeyFile == "" {
		return tls.Certificate{}, fmt.Errorf("certificate file and key file are required for TLS")
	}

	cert, err := tls.LoadX509KeyPair(c.CertFile, c.KeyFile)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("failed to load certificate: %w", err)
	}

	return cert, nil
}

// LoadClientCA loads the client CA certificate for mTLS
func (c *Config) LoadClientCA() (*x509.CertPool, error) {
	if c.ClientCAFile == "" {
		return nil, fmt.Errorf("client CA file is required for mTLS")
	}

	caCert, err := os.ReadFile(c.ClientCAFile)
	if err != nil {
		return nil, fmt.Errorf("failed to read client CA file: %w", err)
	}

	caCertPool := x509.NewCertPool()
	if !caCertPool.AppendCertsFromPEM(caCert) {
		return nil, fmt.Errorf("failed to parse client CA certificate")
	}

	return caCertPool, nil
}

// ToTLSConfig creates a crypto/tls.Config from this Config
func (c *Config) ToTLSConfig() (*tls.Config, error) {
	if !c.Enabled {
		return nil, nil
	}

	// Load server certificate
	cert, err := c.LoadCertificate()
	if err != nil {
		return nil, err
	}

	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   c.MinVersion,
	}

	// Configure mTLS if enabled
	if c.MTLS {
		caCertPool, err := c.LoadClientCA()
		if err != nil {
			return nil, err
		}

		tlsConfig.ClientCAs = caCertPool
		tlsConfig.ClientAuth = tls.RequireAndVerifyClientCert
	}

	return tlsConfig, nil
}

// Validate validates the TLS configuration
func (c *Config) Validate() error {
	if !c.Enabled {
		return nil
	}

	if c.CertFile == "" {
		return fmt.Errorf("cert_file is required when TLS is enabled")
	}
	if c.KeyFile == "" {
		return fmt.Errorf("key_file is required when TLS is enabled")
	}

	// Check if certificate files exist
	if _, err := os.Stat(c.CertFile); os.IsNotExist(err) {
		return fmt.Errorf("certificate file not found: %s", c.CertFile)
	}
	if _, err := os.Stat(c.KeyFile); os.IsNotExist(err) {
		return fmt.Errorf("key file not found: %s", c.KeyFile)
	}

	// For mTLS, check client CA
	if c.MTLS {
		if c.ClientCAFile == "" {
			return fmt.Errorf("client_ca_file is required when mTLS is enabled")
		}
		if _, err := os.Stat(c.ClientCAFile); os.IsNotExist(err) {
			return fmt.Errorf("client CA file not found: %s", c.ClientCAFile)
		}
	}

	// Validate minimum TLS version
	validVersions := map[uint16]string{
		tls.VersionTLS10: "TLS 1.0",
		tls.VersionTLS11: "TLS 1.1",
		tls.VersionTLS12: "TLS 1.2",
		tls.VersionTLS13: "TLS 1.3",
	}
	if _, ok := validVersions[c.MinVersion]; !ok {
		return fmt.Errorf("invalid TLS version: %d", c.MinVersion)
	}

	return nil
}

// ResolvePaths resolves relative paths relative to a base directory
func (c *Config) ResolvePaths(baseDir string) error {
	if c.CertFile != "" && !filepath.IsAbs(c.CertFile) {
		c.CertFile = filepath.Join(baseDir, c.CertFile)
	}
	if c.KeyFile != "" && !filepath.IsAbs(c.KeyFile) {
		c.KeyFile = filepath.Join(baseDir, c.KeyFile)
	}
	if c.ClientCAFile != "" && !filepath.IsAbs(c.ClientCAFile) {
		c.ClientCAFile = filepath.Join(baseDir, c.ClientCAFile)
	}
	return nil
}
