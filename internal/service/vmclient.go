package service

import (
	"context"
	"net"
	"net/http"
	"strings"
	"time"

	"voidrun/pkg/machine"
)

var SandboxHTTPClient *http.Client

func InitSandboxHTTPClient() *http.Client {
	if SandboxHTTPClient != nil {
		return SandboxHTTPClient
	}

	tr := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			sbxID := strings.Split(addr, ":")[0]
			return machine.DialVsock(sbxID, 1024, 5*time.Second)
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

	SandboxHTTPClient = &http.Client{
		Transport: tr,
		Timeout:   0, // No global timeout — large files need time
	}

	return SandboxHTTPClient
}

func GetSandboxHTTPClient() *http.Client {
	if SandboxHTTPClient == nil {
		return InitSandboxHTTPClient()
	}
	return SandboxHTTPClient
}
