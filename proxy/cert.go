package proxy

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// CertManager handles CA key generation and dynamic per-host certificate creation.
// It maintains an LRU-style cache of generated leaf certificates.
type CertManager struct {
	caCert    *x509.Certificate
	caKey     *ecdsa.PrivateKey
	caPEM     []byte
	mu        sync.RWMutex
	cache     map[string]*tls.Certificate
	cacheSize int
	sfGroup   singleflight.Group // deduplicates concurrent generation for the same host
}

const maxCertCache = 10000

// NewCertManager loads a persistent CA from certDir, or generates a new one and
// saves it there. If certDir is empty, falls back to an ephemeral in-memory CA.
//
// Persistence prevents "trust breakage" after a process restart: VMs receive the
// CA cert once at boot and trust it for the life of the VM; a fresh CA on restart
// would cause every subsequent MITM handshake to fail silently.
func NewCertManager(certDir string) (*CertManager, error) {
	if certDir != "" {
		cm, err := loadOrCreateCA(certDir)
		if err != nil {
			return nil, err
		}
		return cm, nil
	}
	return generateCA()
}

// loadOrCreateCA loads the CA from disk, or generates + saves it if absent.
func loadOrCreateCA(certDir string) (*CertManager, error) {
	certPath := filepath.Join(certDir, "ca.crt")
	keyPath := filepath.Join(certDir, "ca.key")

	certPEMBytes, certErr := os.ReadFile(certPath)
	keyPEMBytes, keyErr := os.ReadFile(keyPath)

	if certErr == nil && keyErr == nil {
		// Both files exist — try to load them.
		cm, err := parseCAPEM(certPEMBytes, keyPEMBytes)
		if err == nil {
			return cm, nil
		}
		// Corrupted files — fall through to regenerate.
	} else if !errors.Is(certErr, os.ErrNotExist) && !errors.Is(keyErr, os.ErrNotExist) {
		// Unexpected read error (permissions, etc.)
		return nil, fmt.Errorf("read CA files: cert=%v key=%v", certErr, keyErr)
	}

	// Generate a fresh CA and persist it.
	cm, err := generateCA()
	if err != nil {
		return nil, err
	}

	if err := os.MkdirAll(certDir, 0o700); err != nil {
		return nil, fmt.Errorf("create cert dir %s: %w", certDir, err)
	}

	keyDER, err := x509.MarshalECPrivateKey(cm.caKey)
	if err != nil {
		return nil, fmt.Errorf("marshal CA key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	if err := os.WriteFile(certPath, cm.caPEM, 0o644); err != nil {
		return nil, fmt.Errorf("write CA cert: %w", err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return nil, fmt.Errorf("write CA key: %w", err)
	}

	return cm, nil
}

// parseCAPEM reconstructs a CertManager from PEM-encoded cert and key bytes.
func parseCAPEM(certPEM, keyPEM []byte) (*CertManager, error) {
	certBlock, _ := pem.Decode(certPEM)
	if certBlock == nil {
		return nil, fmt.Errorf("no certificate pem block")
	}
	caCert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse CA cert: %w", err)
	}

	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		return nil, fmt.Errorf("no private key pem block")
	}
	caKey, err := x509.ParseECPrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse CA key: %w", err)
	}

	return &CertManager{
		caCert:    caCert,
		caKey:     caKey,
		caPEM:     certPEM,
		cache:     make(map[string]*tls.Certificate),
		cacheSize: maxCertCache,
	}, nil
}

// generateCA creates a fresh ECDSA P-256 CA keypair in memory.
func generateCA() (*CertManager, error) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate CA key: %w", err)
	}

	serialNumber, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("generate serial: %w", err)
	}

	caTemplate := &x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			Organization: []string{"VoidRun Proxy CA"},
			CommonName:   "VoidRun MITM CA",
		},
		NotBefore:             time.Now().Add(-1 * time.Hour),
		NotAfter:              time.Now().Add(10 * 365 * 24 * time.Hour), // 10 years
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            0,
	}

	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, fmt.Errorf("create CA cert: %w", err)
	}

	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		return nil, fmt.Errorf("parse CA cert: %w", err)
	}

	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})

	return &CertManager{
		caCert:    caCert,
		caKey:     caKey,
		caPEM:     caPEM,
		cache:     make(map[string]*tls.Certificate),
		cacheSize: maxCertCache,
	}, nil
}

// CACertPEM returns the CA certificate in PEM format for injection into guests.
func (cm *CertManager) CACertPEM() []byte {
	return cm.caPEM
}

// CACertPool returns an x509.CertPool containing the proxy CA, ready to use
// as tls.Config.RootCAs in test clients that need to trust the proxy's MITM certs.
func (cm *CertManager) CACertPool() (*x509.CertPool, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(cm.caPEM) {
		return nil, fmt.Errorf("failed to parse CA PEM")
	}
	return pool, nil
}

// GetCertificate returns a TLS certificate for the given hostname, generating one
// if not cached. Uses ECDSA P-256 for fast key generation (~0.2ms).
// singleflight deduplicates concurrent callers for the same host so the
// certificate is generated exactly once even under high concurrency.
func (cm *CertManager) GetCertificate(hostname string) (*tls.Certificate, error) {
	cm.mu.RLock()
	if cert, ok := cm.cache[hostname]; ok {
		cm.mu.RUnlock()
		return cert, nil
	}
	cm.mu.RUnlock()

	// Collapse all concurrent callers for the same hostname into one generation.
	v, err, _ := cm.sfGroup.Do(hostname, func() (any, error) {
		// Re-check cache now that we hold the exclusive slot — a previous
		// call that just finished may have already populated it.
		cm.mu.RLock()
		if cert, ok := cm.cache[hostname]; ok {
			cm.mu.RUnlock()
			return cert, nil
		}
		cm.mu.RUnlock()

		cert, err := cm.generateCert(hostname)
		if err != nil {
			return nil, err
		}

		cm.mu.Lock()
		// Evict randomly if cache is full (simple strategy, avoids LRU overhead)
		if len(cm.cache) >= cm.cacheSize {
			count := 0
			for k := range cm.cache {
				delete(cm.cache, k)
				count++
				if count >= 100 { // evict 100 at a time
					break
				}
			}
		}
		cm.cache[hostname] = cert
		cm.mu.Unlock()

		return cert, nil
	})
	if err != nil {
		return nil, err
	}
	return v.(*tls.Certificate), nil
}

func (cm *CertManager) generateCert(hostname string) (*tls.Certificate, error) {
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate leaf key: %w", err)
	}

	serialNumber, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("generate serial: %w", err)
	}

	template := &x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			CommonName: hostname,
		},
		NotBefore:             time.Now().Add(-1 * time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour), // short-lived: 24h
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	// Go requires IP SANs for IP address targets; DNS SANs are rejected for IPs.
	if ip := net.ParseIP(hostname); ip != nil {
		template.IPAddresses = []net.IP{ip}
	} else {
		template.DNSNames = []string{hostname}
	}

	leafDER, err := x509.CreateCertificate(rand.Reader, template, cm.caCert, &leafKey.PublicKey, cm.caKey)
	if err != nil {
		return nil, fmt.Errorf("create leaf cert: %w", err)
	}

	leafCertPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER})
	leafKeyDER, err := x509.MarshalECPrivateKey(leafKey)
	if err != nil {
		return nil, fmt.Errorf("marshal leaf key: %w", err)
	}
	leafKeyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: leafKeyDER})

	tlsCert, err := tls.X509KeyPair(leafCertPEM, leafKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("create tls keypair: %w", err)
	}

	return &tlsCert, nil
}

// TLSConfigForHost returns a *tls.Config that presents a dynamically-generated
// certificate for the given hostname, signed by our CA.
// If hostname includes a port (as goproxy passes CONNECT targets), the port is
// stripped so the cert is keyed and generated for the clean hostname/IP only.
func (cm *CertManager) TLSConfigForHost(hostname string) *tls.Config {
	if h, _, err := net.SplitHostPort(hostname); err == nil {
		hostname = h // strip port e.g. "httpbin.org:443" → "httpbin.org"
	}
	return &tls.Config{
		GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			name := hello.ServerName
			if name == "" {
				name = hostname
			}
			return cm.GetCertificate(name)
		},
		MinVersion: tls.VersionTLS13,
	}
}
