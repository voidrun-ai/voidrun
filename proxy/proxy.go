package proxy

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"voidrun/model"
	"voidrun/service"
)

// Server is the forward proxy that handles all outbound VM traffic.
// It dispatches connections into three tiers:
//   - Tier 1 (MITM): TLS termination + secret placeholder substitution
//   - Tier 2 (Tunnel): Raw TCP passthrough via kernel splice
//   - Tier 3 (Block): Immediately reject disallowed domains
type Server struct {
	listenAddr  string
	policyCache *service.PolicyCache
	certManager *CertManager
	connPool    *ConnPool
	metrics     *Metrics
	listener    net.Listener
	wg          sync.WaitGroup
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

// Start begins listening and serving proxy connections. Blocks until ctx is cancelled.
func (s *Server) Start(ctx context.Context) error {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", s.listenAddr)
	if err != nil {
		return fmt.Errorf("proxy listen %s: %w", s.listenAddr, err)
	}
	s.listener = ln
	log.Printf("[proxy] listening on %s", s.listenAddr)

	go func() {
		<-ctx.Done()
		ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				break // shutdown
			}
			log.Printf("[proxy] accept error: %v", err)
			continue
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handleConnection(conn)
		}()
	}

	s.wg.Wait()
	return nil
}

func (s *Server) handleConnection(conn net.Conn) {
	defer conn.Close()
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[proxy] panic handling %s: %v", conn.RemoteAddr(), r)
		}
	}()

	// Set deadline for initial HTTP request read
	conn.SetReadDeadline(time.Now().Add(30 * time.Second))

	// Read the first line to determine if it's CONNECT or regular HTTP
	buf := make([]byte, 4096)
	n, err := conn.Read(buf)
	if err != nil {
		return
	}
	conn.SetReadDeadline(time.Time{}) // clear deadline

	data := string(buf[:n])
	vmIP := extractIP(conn.RemoteAddr().String())

	if strings.HasPrefix(data, "CONNECT ") {
		s.handleCONNECT(conn, data, vmIP)
	} else {
		s.handleHTTP(conn, buf[:n], vmIP)
	}
}

// handleCONNECT processes HTTPS tunnel requests.
// Dispatches to Tier 1 (MITM), Tier 2 (tunnel), or Tier 3 (block).
func (s *Server) handleCONNECT(clientConn net.Conn, firstLine string, vmIP string) {
	// Parse "CONNECT host:port HTTP/1.1\r\n..."
	parts := strings.Fields(firstLine)
	if len(parts) < 2 {
		writeHTTPError(clientConn, 400, "bad request")
		return
	}
	target := parts[1] // "host:port"
	host, port := splitHostPort(target)
	if host == "" {
		writeHTTPError(clientConn, 400, "bad connect target")
		return
	}
	if port == "" {
		port = "443"
	}

	// Policy lookup — Tier 3 check
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	pol, err := s.policyCache.Get(ctx, vmIP)
	if err != nil {
		log.Printf("[proxy] policy error for %s: %v", vmIP, err)
		writeHTTPError(clientConn, 403, "policy error")
		s.metrics.RecordBlocked(vmIP, host)
		return
	}
	if pol == nil {
		writeHTTPError(clientConn, 403, "unknown vm")
		s.metrics.RecordBlocked(vmIP, host)
		return
	}

	if !domainAllowed(pol, host) {
		writeHTTPError(clientConn, 403, "domain blocked")
		s.metrics.RecordBlocked(vmIP, host)
		return
	}

	// Determine tier: does this domain have secret mappings?
	mappings := secretsForHost(pol, host)

	log.Printf("[proxy] CONNECT %s:%s from %s, mappings=%d, allowed_domains=%v", host, port, vmIP, len(mappings), pol.AllowedDomains)

	// Send 200 Connection Established
	_, err = clientConn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
	if err != nil {
		return
	}

	s.metrics.RecordRequest(vmIP, host)

	if len(mappings) > 0 {
		// Tier 1: MITM TLS with secret substitution
		log.Printf("[proxy] Tier 1 MITM for %s:%s (secrets=%d)", host, port, len(mappings))
		s.handleMITM(clientConn, host, port, vmIP, mappings)
		log.Printf("[proxy] Tier 1 MITM done for %s:%s", host, port)
	} else {
		// Tier 2: Raw TCP tunnel (kernel splice via io.Copy)
		log.Printf("[proxy] Tier 2 tunnel for %s:%s", host, port)
		s.handleTunnel(clientConn, host, port)
	}
}

// handleHTTP handles plain HTTP proxy requests (port 80 redirected via iptables).
func (s *Server) handleHTTP(clientConn net.Conn, initialData []byte, vmIP string) {
	// Parse host from Host header or request line
	host := extractHTTPHost(initialData)
	if host == "" {
		writeHTTPError(clientConn, 400, "missing host")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	pol, err := s.policyCache.Get(ctx, vmIP)
	if err != nil || pol == nil {
		writeHTTPError(clientConn, 403, "blocked")
		s.metrics.RecordBlocked(vmIP, host)
		return
	}

	if !domainAllowed(pol, host) {
		writeHTTPError(clientConn, 403, "domain blocked")
		s.metrics.RecordBlocked(vmIP, host)
		return
	}

	s.metrics.RecordRequest(vmIP, host)

	// Apply header/param injection for HTTP traffic
	initialData = injectHTTPHeaders(initialData, pol)

	// Connect to upstream and relay
	port := "80"
	upstream, err := net.DialTimeout("tcp", net.JoinHostPort(host, port), 10*time.Second)
	if err != nil {
		writeHTTPError(clientConn, 502, "upstream unreachable")
		s.metrics.RecordError(vmIP, host)
		return
	}
	defer upstream.Close()

	// Send the initial request data we already read
	upstream.Write(initialData)

	// Bidirectional relay
	done := make(chan struct{})
	go func() {
		io.Copy(upstream, clientConn)
		done <- struct{}{}
	}()
	io.Copy(clientConn, upstream)
	<-done
}

// --- Helpers ---

func extractIP(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return host
}

func splitHostPort(target string) (string, string) {
	host, port, err := net.SplitHostPort(target)
	if err != nil {
		// target might be just "host" without port
		return target, ""
	}
	return host, port
}

func extractHTTPHost(data []byte) string {
	lines := strings.Split(string(data), "\r\n")
	for _, line := range lines {
		if strings.HasPrefix(strings.ToLower(line), "host:") {
			host := strings.TrimSpace(line[5:])
			// Strip port if present
			if h, _, err := net.SplitHostPort(host); err == nil {
				return h
			}
			return host
		}
	}
	// Try from request line: "GET http://host/path HTTP/1.1"
	if len(lines) > 0 {
		parts := strings.Fields(lines[0])
		if len(parts) >= 2 && strings.HasPrefix(parts[1], "http") {
			// parse URL
			if idx := strings.Index(parts[1], "://"); idx >= 0 {
				hostPath := parts[1][idx+3:]
				if slashIdx := strings.Index(hostPath, "/"); slashIdx >= 0 {
					hostPath = hostPath[:slashIdx]
				}
				if h, _, err := net.SplitHostPort(hostPath); err == nil {
					return h
				}
				return hostPath
			}
		}
	}
	return ""
}

func writeHTTPError(conn net.Conn, status int, msg string) {
	resp := fmt.Sprintf("HTTP/1.1 %d %s\r\nContent-Type: application/json\r\nConnection: close\r\n\r\n{\"error\":\"%s\"}", status, http.StatusText(status), msg)
	conn.Write([]byte(resp))
}

func injectHTTPHeaders(data []byte, pol *model.NetworkPolicy) []byte {
	if len(pol.InjectHeaders) == 0 {
		return data
	}
	// Find end of first line (request line), inject headers after it
	s := string(data)
	idx := strings.Index(s, "\r\n")
	if idx < 0 {
		return data
	}
	var headers strings.Builder
	for k, v := range pol.InjectHeaders {
		headers.WriteString(k)
		headers.WriteString(": ")
		headers.WriteString(v)
		headers.WriteString("\r\n")
	}
	return []byte(s[:idx+2] + headers.String() + s[idx+2:])
}

// domainAllowed checks if a host is allowed by the network policy.
func domainAllowed(pol *model.NetworkPolicy, host string) bool {
	if host == "" {
		return false
	}
	host = strings.ToLower(strings.TrimSpace(host))

	// Blocklist first
	for _, pattern := range pol.BlockedDomains {
		if matchDomain(host, pattern) {
			return false
		}
	}

	// Empty allowlist = deny all
	if len(pol.AllowedDomains) == 0 {
		return false
	}

	for _, pattern := range pol.AllowedDomains {
		if matchDomain(host, pattern) {
			return true
		}
	}
	return false
}

func matchDomain(host, pattern string) bool {
	pattern = strings.ToLower(pattern)
	if pattern == "*" {
		return true
	}
	if pattern == host {
		return true
	}
	// "*.github.com" matches "api.github.com" and "github.com"
	if strings.HasPrefix(pattern, "*.") {
		suffix := pattern[1:] // ".github.com"
		if strings.HasSuffix(host, suffix) {
			return true
		}
		if host == pattern[2:] { // bare domain match
			return true
		}
	}
	return false
}

// secretsForHost returns the secret mappings that apply to the given host.
func secretsForHost(pol *model.NetworkPolicy, host string) []model.SecretMapping {
	if len(pol.SecretMappings) == 0 {
		return nil
	}
	var result []model.SecretMapping
	for _, m := range pol.SecretMappings {
		for _, h := range m.Hosts {
			if matchDomain(host, h) {
				result = append(result, m)
				break
			}
		}
	}
	return result
}
