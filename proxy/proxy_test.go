package proxy

import (
	"bytes"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

// --- CertManager persistence ---

// TestCertManagerEphemeral verifies that an empty certDir produces a working in-memory CA.
func TestCertManagerEphemeral(t *testing.T) {
	cm, err := NewCertManager("")
	if err != nil {
		t.Fatalf("NewCertManager: %v", err)
	}
	if len(cm.caPEM) == 0 {
		t.Fatal("expected non-empty CA PEM")
	}
	cert, err := cm.GetCertificate("example.com")
	if err != nil {
		t.Fatalf("GetCertificate: %v", err)
	}
	if cert == nil {
		t.Fatal("expected non-nil cert")
	}
}

// TestCertManagerPersistence is the critical regression test for the restart bug:
// a new CertManager with the same certDir must load the exact same CA.
func TestCertManagerPersistence(t *testing.T) {
	dir := t.TempDir()

	cm1, err := NewCertManager(dir)
	if err != nil {
		t.Fatalf("first create: %v", err)
	}

	// Files should exist
	if _, statErr := os.Stat(filepath.Join(dir, "ca.crt")); statErr != nil {
		t.Fatalf("ca.crt not created: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "ca.key")); statErr != nil {
		t.Fatalf("ca.key not created: %v", statErr)
	}

	// Second load must return identical CA PEM.
	cm2, err := NewCertManager(dir)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}

	if !bytes.Equal(cm1.caPEM, cm2.caPEM) {
		t.Fatal("CA PEM changed on reload — server restart would break all VM trust")
	}
	t.Logf("CA PEM stable across restart (%d bytes)", len(cm1.caPEM))
}

// TestCertManagerKeyPerms verifies the private key file is written with 0600.
func TestCertManagerKeyPerms(t *testing.T) {
	dir := t.TempDir()
	if _, err := NewCertManager(dir); err != nil {
		t.Fatalf("NewCertManager: %v", err)
	}

	info, err := os.Stat(filepath.Join(dir, "ca.key"))
	if err != nil {
		t.Fatalf("stat ca.key: %v", err)
	}
	perm := info.Mode().Perm()
	if perm != 0o600 {
		t.Fatalf("ca.key permissions: got %04o, want 0600", perm)
	}
}

// TestCertManagerCorruptRecovery verifies that corrupted files trigger a fresh CA.
func TestCertManagerCorruptRecovery(t *testing.T) {
	dir := t.TempDir()

	// Seed corrupt files.
	if err := os.WriteFile(filepath.Join(dir, "ca.crt"), []byte("not pem"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ca.key"), []byte("also not pem"), 0o600); err != nil {
		t.Fatal(err)
	}

	cm, err := NewCertManager(dir)
	if err != nil {
		t.Fatalf("should recover from corruption: %v", err)
	}
	if len(cm.caPEM) == 0 {
		t.Fatal("expected fresh CA after corrupt recovery")
	}
}

// --- ConnPool TTL + stats ---

// TestConnPoolTTL ensures the idle timeout constant is 90s (tuned for p95 variance reduction).
func TestConnPoolTTL(t *testing.T) {
	if poolIdleTimeout.Seconds() != 90 {
		t.Fatalf("poolIdleTimeout = %v, want 90s", poolIdleTimeout)
	}
}

// TestConnPoolStats verifies that pool misses are counted for cold dials.
// We drive it via the exported Stats() method without needing a real TLS server.
func TestConnPoolStats(t *testing.T) {
	p := NewConnPool()

	// Nothing in pool yet — Get should return nil and increment misses.
	p.Get("example.com:443")
	p.Get("example.com:443")

	hits, misses := p.Stats()
	if hits != 0 {
		t.Fatalf("expected 0 hits, got %d", hits)
	}
	if misses != 2 {
		t.Fatalf("expected 2 misses, got %d", misses)
	}
}

// --- Metrics ---

// TestMetricsSubstitutionCounter verifies the new Substitutions counter increments.
func TestMetricsSubstitutionCounter(t *testing.T) {
	m := NewMetrics()
	m.RecordSubstitution("10.0.0.1")
	m.RecordSubstitution("10.0.0.1")
	m.RecordSubstitution("10.0.0.2")

	snaps := m.Snapshot()
	totals := map[string]int64{}
	for _, s := range snaps {
		totals[s.VMIP] = s.Substitutions
	}
	if totals["10.0.0.1"] != 2 {
		t.Fatalf("10.0.0.1 substitutions: got %d, want 2", totals["10.0.0.1"])
	}
	if totals["10.0.0.2"] != 1 {
		t.Fatalf("10.0.0.2 substitutions: got %d, want 1", totals["10.0.0.2"])
	}
}

// TestMetricsConcurrency stress-tests the map+atomic pattern under concurrent writes.
func TestMetricsConcurrency(t *testing.T) {
	m := NewMetrics()
	const goroutines = 50
	const ops = 200

	var done atomic.Int64
	for i := range goroutines {
		ip := "10.0.0." + string(rune('0'+i%10))
		go func() {
			for range ops {
				m.RecordRequest(ip, "example.com")
				m.RecordSubstitution(ip)
				m.RecordBytes(ip, 1024)
			}
			done.Add(1)
		}()
	}

	for done.Load() < goroutines {
		// spin
	}

	snaps := m.Snapshot()
	if len(snaps) == 0 {
		t.Fatal("no snapshots after concurrent writes")
	}
	t.Logf("concurrent metrics: %d VMs tracked", len(snaps))
}
