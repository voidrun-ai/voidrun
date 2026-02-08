package service

import (
	"context"
	"net"
	"net/http"
	"time"

	"voidrun/pkg/machine"

	"github.com/gorilla/websocket"
)

// VsockWSDialer establishes WebSocket connections to the agent over vsock.
type VsockWSDialer struct {
	dialer websocket.Dialer
}

// NewVsockWSDialer creates a new dialer using machine.DialVsock.
func NewVsockWSDialer() *VsockWSDialer {
	return &VsockWSDialer{
		dialer: websocket.Dialer{
			NetDialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				host, _, err := net.SplitHostPort(addr)
				if err != nil {
					// If no port provided, use full addr as host
					host = addr
				}
				return machine.DialVsock(host, 1024, 5*time.Second)
			},
		},
	}
}

// DialContext dials the given WebSocket URL using vsock transport.
func (d *VsockWSDialer) DialContext(ctx context.Context, urlStr string, hdr http.Header) (*websocket.Conn, *http.Response, error) {
	return d.dialer.DialContext(ctx, urlStr, hdr)
}
