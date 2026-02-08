package service

import (
	"context"
	"net"
	"net/http"
	"strings"
	"time"

	"voidrun/pkg/machine"
)

// VMHTTPClient is a shared HTTP client for VM vsock communication
var VMHTTPClient *http.Client

// InitVMHTTPClient creates and returns a shared HTTP client optimized for vsock communication
func InitVMHTTPClient() *http.Client {
	if VMHTTPClient != nil {
		return VMHTTPClient
	}

	tr := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			// addr comes in as "vmID:80" — extract vmID
			vmID := strings.Split(addr, ":")[0]
			return machine.DialVsock(vmID, 1024, 5*time.Second)
		},
		// Connection Pooling
		MaxIdleConns:        1000,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     2 * time.Minute,
		ForceAttemptHTTP2:   false,
		// Performance
		DisableCompression: true,
		WriteBufferSize:    64 * 1024,
		ReadBufferSize:     64 * 1024,
		// Header timeout for large transfers
		ResponseHeaderTimeout: 30 * time.Second,
	}

	VMHTTPClient = &http.Client{
		Transport: tr,
		Timeout:   0, // No global timeout — large files need time
	}

	return VMHTTPClient
}

// GetVMHTTPClient returns the shared VM HTTP client, initializing if needed
func GetVMHTTPClient() *http.Client {
	if VMHTTPClient == nil {
		return InitVMHTTPClient()
	}
	return VMHTTPClient
}
