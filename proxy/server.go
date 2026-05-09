// Forward proxy built on github.com/elazarl/goproxy.
//
// 3-tier dispatch:
//   - Tier 1 (MITM):   TLS termination + request header secret substitution + InjectHeaders
//   - Tier 2 (Tunnel): Raw TCP passthrough — goproxy OkConnect (kernel splice)
//   - Tier 3 (Block):  goproxy RejectConnect → HTTP 403
package proxy

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	gp "github.com/elazarl/goproxy"
	"voidrun/model"
	"voidrun/service"
)

// PolicyStore is the minimal interface the proxy needs from the policy cache.
type PolicyStore interface {
	Get(ctx context.Context, vmIP string) (*model.NetworkPolicy, error)
}

const (
	upstreamIdleTimeout  = 90 * time.Second
	upstreamMaxIdle      = 1000
	upstreamMaxIdleConns = 100
)

// Server is the forward proxy server.
type Server struct {
	listenAddr        string
	policyCache       PolicyStore
	certManager       *CertManager
	connPool          *ConnPool
	metrics           *Metrics
	httpServer        *http.Server
	upstreamTLSConfig *tls.Config // nil = connPool default; non-nil overrides (testing only)
}

// SetUpstreamTLSConfig overrides the TLS configuration used for upstream connections.
// Intended for testing only.
func (s *Server) SetUpstreamTLSConfig(cfg *tls.Config) {
	s.upstreamTLSConfig = cfg
}

// NewServer creates a new forward proxy server.
// certDir is the directory where the CA cert/key are persisted across restarts.
// Pass an empty string to use an ephemeral (in-memory) CA.
func NewServer(listenAddr, certDir string, policyCache *service.PolicyCache) (*Server, error) {
	cm, err := NewCertManager(certDir)
	if err != nil {
		return nil, fmt.Errorf("init cert manager: %w", err)
	}
	return &Server{
		listenAddr:  listenAddr,
		policyCache: policyCache,
		certManager: cm,
		connPool:    NewConnPool(),
		metrics:     NewMetrics(),
	}, nil
}

// CACertPEM returns the CA certificate in PEM format for guest injection.
func (s *Server) CACertPEM() []byte {
	return s.certManager.CACertPEM()
}

// GetMetrics exposes proxy metrics for external reporting.
func (s *Server) GetMetrics() *Metrics {
	return s.metrics
}

// Start begins listening and serving proxy connections. Blocks until ctx is cancelled.
func (s *Server) Start(ctx context.Context) error {
	gpProxy := gp.NewProxyHttpServer()
	gpProxy.Verbose = false

	gpProxy.Tr = &http.Transport{
		MaxIdleConns:          upstreamMaxIdle,
		MaxIdleConnsPerHost:   upstreamMaxIdleConns,
		IdleConnTimeout:       upstreamIdleTimeout,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: 120 * time.Second,
		DisableKeepAlives:     false,
		DialTLSContext: func(dialCtx context.Context, network, addr string) (net.Conn, error) {
			host, _, _ := net.SplitHostPort(addr)
			if s.upstreamTLSConfig != nil {
				cfg := s.upstreamTLSConfig.Clone()
				if cfg.ServerName == "" {
					cfg.ServerName = host
				}
				tcpConn, err := net.DialTimeout(network, addr, 10*time.Second)
				if err != nil {
					return nil, err
				}
				tlsConn := tls.Client(tcpConn, cfg)
				tlsConn.SetDeadline(time.Now().Add(10 * time.Second))
				if err := tlsConn.HandshakeContext(dialCtx); err != nil {
					tcpConn.Close()
					return nil, err
				}
				tlsConn.SetDeadline(time.Time{})
				return tlsConn, nil
			}
			return s.connPool.Dial(addr, host)
		},
	}

	// HandleConnect: 3-tier dispatch at CONNECT time.
	gpProxy.OnRequest().HandleConnectFunc(func(host string, ctx *gp.ProxyCtx) (*gp.ConnectAction, string) {
		vmIP := proxyExtractIP(ctx.Req.RemoteAddr)
		hostname, _, _ := net.SplitHostPort(host)
		if hostname == "" {
			hostname = host
		}

		reqCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		pol, err := s.policyCache.Get(reqCtx, vmIP)
		if err != nil {
			log.Printf("[proxy] policy error for %s: %v", vmIP, err)
			s.metrics.RecordBlocked(vmIP, hostname)
			return gp.RejectConnect, host
		}
		if pol == nil {
			log.Printf("[proxy] unknown vm %s", vmIP)
			s.metrics.RecordBlocked(vmIP, hostname)
			return gp.RejectConnect, host
		}

		if !proxyDomainAllowed(pol, hostname) {
			log.Printf("[proxy] Tier 3 block %s for %s", hostname, vmIP)
			s.metrics.RecordBlocked(vmIP, hostname)
			return gp.RejectConnect, host
		}

		s.metrics.RecordRequest(vmIP, hostname)
		mappings := proxySecretsForHost(pol, hostname)

		if len(mappings) > 0 {
			log.Printf("[proxy] Tier 1 MITM %s (secrets=%d)", hostname, len(mappings))
			captured := hostname
			return &gp.ConnectAction{
				Action: gp.ConnectMitm,
				TLSConfig: func(sniHost string, _ *gp.ProxyCtx) (*tls.Config, error) {
					h := sniHost
					if h == "" {
						h = captured
					}
					return s.certManager.TLSConfigForHost(h), nil
				},
			}, host
		}

		// Tier 2: raw TCP tunnel (kernel splice).
		log.Printf("[proxy] Tier 2 tunnel %s", hostname)
		return gp.OkConnect, host
	})

	// Per-request secret substitution + static header injection.
	// Runs for Tier 1 (MITM) sessions only — Tier 2 raw bytes bypass this entirely.
	gpProxy.OnRequest().DoFunc(func(req *http.Request, ctx *gp.ProxyCtx) (*http.Request, *http.Response) {
		vmIP := proxyExtractIP(req.RemoteAddr)
		if vmIP == "" {
			return req, nil
		}

		host := req.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}

		reqCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		pol, err := s.policyCache.Get(reqCtx, vmIP)
		if err != nil || pol == nil {
			return req, nil
		}

		// Secret placeholder substitution in existing headers.
		mappings := proxySecretsForHost(pol, host)
		substituted := false
		for _, m := range mappings {
			for key, vals := range req.Header {
				for i, v := range vals {
					if strings.Contains(v, m.Placeholder) {
						req.Header[key][i] = strings.ReplaceAll(v, m.Placeholder, m.Value)
						substituted = true
					}
				}
			}
		}
		if substituted {
			s.metrics.RecordSubstitution(vmIP)
		}

		// Static header injection — adds/overwrites headers the VM never sent.
		for k, v := range pol.InjectHeaders {
			req.Header.Set(k, v)
		}

		return req, nil
	})

	ln, err := net.Listen("tcp", s.listenAddr)
	if err != nil {
		return fmt.Errorf("proxy listen %s: %w", s.listenAddr, err)
	}
	log.Printf("[proxy] listening on %s", s.listenAddr)

	s.httpServer = &http.Server{Handler: gpProxy}

	go func() {
		<-ctx.Done()
		s.httpServer.Close()
	}()

	if serveErr := s.httpServer.Serve(ln); serveErr != nil && ctx.Err() == nil {
		return serveErr
	}
	return nil
}

// --- helpers ---

func proxyExtractIP(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return host
}

func proxyDomainAllowed(pol *model.NetworkPolicy, host string) bool {
	if host == "" {
		return false
	}
	host = strings.ToLower(strings.TrimSpace(host))
	for _, pattern := range pol.BlockedDomains {
		if proxyMatchDomain(host, pattern) {
			return false
		}
	}
	if len(pol.AllowedDomains) == 0 {
		return false
	}
	for _, pattern := range pol.AllowedDomains {
		if proxyMatchDomain(host, pattern) {
			return true
		}
	}
	return false
}

func proxyMatchDomain(host, pattern string) bool {
	pattern = strings.ToLower(pattern)
	if pattern == "*" {
		return true
	}
	if pattern == host {
		return true
	}
	if strings.HasPrefix(pattern, "*.") {
		suffix := pattern[1:]
		if strings.HasSuffix(host, suffix) {
			return true
		}
		if host == pattern[2:] {
			return true
		}
	}
	return false
}

func proxySecretsForHost(pol *model.NetworkPolicy, host string) []model.SecretMapping {
	if len(pol.SecretMappings) == 0 {
		return nil
	}
	var result []model.SecretMapping
	for _, m := range pol.SecretMappings {
		for _, h := range m.Hosts {
			if proxyMatchDomain(host, h) {
				result = append(result, m)
				break
			}
		}
	}
	return result
}
