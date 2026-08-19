package tls

import (
	"context"
	"log"
	"sync"
	"time"
)

// AlertHandler is a callback function for certificate expiration alerts
type AlertHandler func(certInfo *CertificateInfo, message string)

// CertMonitor monitors certificates for expiration
type CertMonitor struct {
	config  *Config
	handler AlertHandler
	stopCh  chan struct{}
	mu      sync.Mutex
	running bool
}

// NewCertMonitor creates a new certificate expiration monitor
func NewCertMonitor(config *Config, handler AlertHandler) *CertMonitor {
	return &CertMonitor{
		config:  config,
		handler: handler,
		stopCh:  make(chan struct{}),
	}
}

// Start begins the certificate monitoring goroutine
func (m *CertMonitor) Start() {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.running {
		return
	}

	m.running = true
	go m.monitorLoop()
	log.Printf("[TLS] Certificate expiration monitor started (check interval: %s, warning threshold: %d days)",
		m.config.ExpirationCheckInterval, m.config.ExpirationWarningDays)
}

// Stop stops the certificate monitoring goroutine
func (m *CertMonitor) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()

	if !m.running {
		return
	}

	m.running = false
	close(m.stopCh)
	m.stopCh = make(chan struct{})
	log.Println("[TLS] Certificate expiration monitor stopped")
}

// CheckNow performs an immediate certificate check
func (m *CertMonitor) CheckNow() error {
	return m.checkCertificates()
}

// monitorLoop is the main monitoring loop
func (m *CertMonitor) monitorLoop() {
	// Perform initial check
	if err := m.checkCertificates(); err != nil {
		log.Printf("[TLS] Certificate check error: %v", err)
	}

	ticker := time.NewTicker(m.config.ExpirationCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-m.stopCh:
			return
		case <-ticker.C:
			if err := m.checkCertificates(); err != nil {
				log.Printf("[TLS] Certificate check error: %v", err)
			}
		}
	}
}

// checkCertificates checks all configured certificates for expiration
func (m *CertMonitor) checkCertificates() error {
	if !m.config.Enabled {
		return nil
	}

	// Check server certificate
	if m.config.CertFile != "" {
		info, err := CheckCertificateExpiration(m.config.CertFile, m.config.ExpirationWarningDays)
		if err != nil {
			if m.handler != nil {
				if info != nil {
					m.handler(info, err.Error())
				} else {
					m.handler(&CertificateInfo{FilePath: m.config.CertFile}, err.Error())
				}
			}
			// Return nil to continue monitoring even if a cert is expiring
			// The alert handler already received the warning
			return nil
		}
		log.Printf("[TLS] Server certificate OK (expires in %d days, subject: %s)",
			info.DaysUntilExpiration, info.Subject)
	}

	// Check client CA certificate if mTLS is enabled
	if m.config.MTLS && m.config.ClientCAFile != "" {
		info, err := CheckCertificateExpiration(m.config.ClientCAFile, m.config.ExpirationWarningDays)
		if err != nil {
			if m.handler != nil {
				if info != nil {
					m.handler(info, "Client CA: "+err.Error())
				} else {
					m.handler(&CertificateInfo{FilePath: m.config.ClientCAFile}, "Client CA: "+err.Error())
				}
			}
			return nil
		}
		log.Printf("[TLS] Client CA certificate OK (expires in %d days, subject: %s)",
			info.DaysUntilExpiration, info.Subject)
	}

	return nil
}

// RunExpirationCheck performs a one-time certificate expiration check without starting a monitor
func RunExpirationCheck(config *Config) error {
	if !config.Enabled {
		return nil
	}

	if config.CertFile != "" {
		info, err := CheckCertificateExpiration(config.CertFile, config.ExpirationWarningDays)
		if err != nil {
			if info != nil {
				log.Printf("[TLS] WARNING: Server certificate %s - %s", info.Subject, err)
			}
			return err
		}
		log.Printf("[TLS] Server certificate check passed (subject: %s, expires: %s, days remaining: %d, self-signed: %v)",
			info.Subject, info.NotAfter.Format(time.DateOnly), info.DaysUntilExpiration, info.IsSelfSigned)
	}

	if config.MTLS && config.ClientCAFile != "" {
		info, err := CheckCertificateExpiration(config.ClientCAFile, config.ExpirationWarningDays)
		if err != nil {
			if info != nil {
				log.Printf("[TLS] WARNING: Client CA certificate %s - %s", info.Subject, err)
			}
			return err
		}
		log.Printf("[TLS] Client CA certificate check passed (subject: %s, expires: %s, days remaining: %d)",
			info.Subject, info.NotAfter.Format(time.DateOnly), info.DaysUntilExpiration)
	}

	return nil
}

// CheckContext is a context-aware certificate check for graceful shutdown
func (m *CertMonitor) CheckContext(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return m.checkCertificates()
	}
}
