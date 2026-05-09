package proxy

import (
	"crypto/tls"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// ConnPool manages reusable TLS connections to upstream hosts.
// Avoids paying TLS handshake cost on every request to the same destination.
type ConnPool struct {
	mu             sync.Mutex
	conns          map[string][]*pooledConn
	maxIdlePerHost int

	// Global lifetime counters — exposed via Stats().
	poolHits   atomic.Int64
	poolMisses atomic.Int64
}

type pooledConn struct {
	conn      *tls.Conn
	idleSince time.Time
}

const (
	poolMaxIdlePerHost = 100
	poolIdleTimeout    = 90 * time.Second
)

func NewConnPool() *ConnPool {
	p := &ConnPool{
		conns:          make(map[string][]*pooledConn),
		maxIdlePerHost: poolMaxIdlePerHost,
	}
	go p.reapLoop()
	return p
}

// Get retrieves an idle connection from the pool, or returns nil if none available.
func (p *ConnPool) Get(hostport string) *tls.Conn {
	p.mu.Lock()
	defer p.mu.Unlock()

	conns := p.conns[hostport]
	for len(conns) > 0 {
		// Pop from end (LIFO — most recently used is warmest)
		pc := conns[len(conns)-1]
		conns = conns[:len(conns)-1]
		p.conns[hostport] = conns

		// Check if too old
		if time.Since(pc.idleSince) > poolIdleTimeout {
			pc.conn.Close()
			continue
		}

		// Quick liveness check: set a tight deadline and try reading
		// If the connection was closed by the server, Read returns immediately
		pc.conn.SetReadDeadline(time.Now().Add(1 * time.Millisecond))
		buf := []byte{0}
		_, err := pc.conn.Read(buf)
		pc.conn.SetReadDeadline(time.Time{})

		if err != nil {
			// Connection dead (EOF, timeout is fine — means no data waiting)
			// net.Error timeout means the connection is alive (no data to read)
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				p.poolHits.Add(1)
				return pc.conn // alive — reuse
			}
			pc.conn.Close()
			continue
		}
		// Got unexpected data — connection is in a weird state, discard
		pc.conn.Close()
		continue
	}
	p.poolMisses.Add(1)
	return nil
}

// Put returns a connection to the pool for reuse.
func (p *ConnPool) Put(hostport string, conn *tls.Conn) {
	p.mu.Lock()
	defer p.mu.Unlock()

	conns := p.conns[hostport]
	if len(conns) >= p.maxIdlePerHost {
		conn.Close()
		return
	}
	p.conns[hostport] = append(conns, &pooledConn{
		conn:      conn,
		idleSince: time.Now(),
	})
}

// Dial gets a connection from the pool or dials a fresh TLS 1.3 connection.
func (p *ConnPool) Dial(hostport, serverName string) (*tls.Conn, error) {
	if conn := p.Get(hostport); conn != nil {
		return conn, nil
	}

	// Dial fresh
	tcpConn, err := net.DialTimeout("tcp", hostport, 10*time.Second)
	if err != nil {
		return nil, err
	}

	tlsConn := tls.Client(tcpConn, &tls.Config{
		ServerName: serverName,
		MinVersion: tls.VersionTLS12,
	})
	tlsConn.SetDeadline(time.Now().Add(10 * time.Second))
	if err := tlsConn.Handshake(); err != nil {
		tcpConn.Close()
		return nil, err
	}
	tlsConn.SetDeadline(time.Time{})

	return tlsConn, nil
}

// reapLoop periodically removes stale connections.
func (p *ConnPool) reapLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		p.mu.Lock()
		for host, conns := range p.conns {
			alive := conns[:0]
			for _, pc := range conns {
				if time.Since(pc.idleSince) > poolIdleTimeout {
					pc.conn.Close()
				} else {
					alive = append(alive, pc)
				}
			}
			if len(alive) == 0 {
				delete(p.conns, host)
			} else {
				p.conns[host] = alive
			}
		}
		p.mu.Unlock()
	}
}

// Stats returns the lifetime pool hit and miss counts.
func (p *ConnPool) Stats() (hits, misses int64) {
	return p.poolHits.Load(), p.poolMisses.Load()
}
