package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/cenkalti/backoff/v4"
)

func DialVsock(sbxID string, port uint32, timeout time.Duration) (net.Conn, error) {
	if timeout <= 0 {
		timeout = 2 * time.Second
	}

	socketPath := GetVsockPath(sbxID)
	if _, err := os.Stat(socketPath); err != nil {
		return nil, fmt.Errorf("vsock socket not found: %w", err)
	}

	conn, err := net.DialTimeout("unix", socketPath, timeout)
	if err != nil {
		return nil, fmt.Errorf("failed to dial vsock unix socket: %w", err)
	}

	defer func() {
		if conn != nil {
			conn.Close()
		}
	}()

	// Handshake with deadline
	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		return nil, fmt.Errorf("set deadline failed: %w", err)
	}

	// Send handshake
	handshake := fmt.Sprintf("CONNECT %d\n", port)
	if _, err := io.WriteString(conn, handshake); err != nil {
		return nil, fmt.Errorf("handshake write failed: %w", err)
	}

	// Read response byte-by-byte (critical: prevents data loss)
	var line strings.Builder
	line.Grow(32)    // Pre-allocate typical response size
	buf := [1]byte{} // Array avoids heap allocation

	for {
		if _, err := conn.Read(buf[:]); err != nil {
			return nil, fmt.Errorf("handshake read failed: %w", err)
		}

		if buf[0] == '\n' {
			break
		}

		line.WriteByte(buf[0])

		// Safety: Prevent infinite loop on malformed response
		if line.Len() > 64 {
			return nil, fmt.Errorf("handshake response exceeded 64 bytes")
		}
	}

	// Validate response (accept "OK" or "OK <port>")
	response := strings.TrimSpace(line.String())
	if !strings.HasPrefix(response, "OK") {
		return nil, fmt.Errorf("vsock handshake failed, server replied: %q", response)
	}

	// Clear deadline for normal operation
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return nil, fmt.Errorf("failed to clear deadline: %w", err)
	}

	// Success: prevent defer from closing
	result := conn
	conn = nil
	return result, nil
}

// ProbeVsock is a lightweight readiness check: dial + CONNECT handshake + close.
// Returns nil if the guest agent on the given port is reachable.
func Probe(sbxID string, port uint32, timeout time.Duration) error {
	socketPath := GetVsockPath(sbxID)

	conn, err := net.DialTimeout("unix", socketPath, timeout)
	if err != nil {
		return err
	}
	defer conn.Close()

	conn.SetDeadline(time.Now().Add(timeout))

	// Send CONNECT handshake
	if _, err := fmt.Fprintf(conn, "CONNECT %d\n", port); err != nil {
		return err
	}

	// Read just enough to see "OK"
	var buf [16]byte
	n, err := conn.Read(buf[:])
	if err != nil {
		return err
	}
	if n < 2 || buf[0] != 'O' || buf[1] != 'K' {
		return fmt.Errorf("agent not ready: %q", string(buf[:n]))
	}

	return nil
}

func isTransientVsockErr(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, io.EOF) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, net.ErrClosed)
}

// DialVsockWithRetry wraps DialVsock and retries only on transient handshake
// errors (EOF / ECONNRESET / EPIPE / net.ErrClosed) that occur during the
// post-create-async or post-restore agent warmup window. Non-transient errors
// (e.g. socket missing) short-circuit via backoff.Permanent. ctx cancellation
// aborts between attempts. This is the single retry policy used by every
// vsock entry point: sandboxHTTPClient.DialContext, raw vsock callers in
// service.SessionExecService, and service.VsockWSDialer.
func DialVsockWithRetry(ctx context.Context, sbxID string, port uint32, perAttemptTimeout time.Duration, maxAttempts uint64) (net.Conn, error) {
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	if ctx == nil {
		ctx = context.Background()
	}

	var conn net.Conn
	op := func() error {
		c, err := DialVsock(sbxID, port, perAttemptTimeout)
		if err == nil {
			conn = c
			return nil
		}
		if !isTransientVsockErr(err) {
			return backoff.Permanent(err)
		}
		return err
	}

	b := backoff.NewExponentialBackOff()
	b.InitialInterval = 20 * time.Millisecond
	b.MaxInterval = 200 * time.Millisecond
	b.RandomizationFactor = 0.3 // built-in jitter
	b.MaxElapsedTime = 0        // bound only by maxAttempts

	policy := backoff.WithMaxRetries(backoff.WithContext(b, ctx), maxAttempts-1)
	err := backoff.RetryNotify(op, policy, func(e error, d time.Duration) {
		log.Printf("[agent_client] retrying transient vsock dial error for %s in %s: %v", sbxID, d, e)
	})
	if err != nil {
		return nil, err
	}
	return conn, nil
}
