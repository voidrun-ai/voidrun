package runtime

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestNetworkNSCreationAndIptables(t *testing.T) {
	bridgeName := "br-test1"
	exec.Command("ip", "link", "add", "name", bridgeName, "type", "bridge").Run()
	defer exec.Command("ip", "link", "del", bridgeName).Run()

	nameservers := []string{"8.8.8.8", "1.1.1.1"}
	nsName, _, err := CreateSandboxNetNS(bridgeName, "02:00:00:00:00:01", "test", nameservers)
	if err != nil {
		t.Fatalf("CreateSandboxNetNS failed: %v", err)
	}
	defer DeleteSandboxNetNS(nsName)

	// Check iptables inside ns
	out, err := exec.Command("ip", "netns", "exec", nsName, "iptables", "-L", "FORWARD", "-n").CombinedOutput()
	if err != nil {
		t.Fatalf("iptables failed: %v", err)
	}
	
	outStr := string(out)
	t.Logf("Iptables Output:\n%s", outStr)

	// Verify no blanket 53/67
	if strings.Contains(outStr, "dpt:67") {
		t.Errorf("Should not contain dpt:67 (DHCP)")
	}

	// Verify DNS IPs
	if !strings.Contains(outStr, "8.8.8.8") || !strings.Contains(outStr, "1.1.1.1") {
		t.Errorf("Should contain nameserver IPs")
	}

	// Find index of DNS accept vs 169.254 drop
	idx169 := strings.Index(outStr, "169.254.169.254")
	idxDNS := strings.Index(outStr, "8.8.8.8")
	if idxDNS < idx169 {
		t.Errorf("DNS rules should be AFTER the drops! idx169: %d, idxDNS: %d", idx169, idxDNS)
	}

	// SEC-01: conntrack ESTABLISHED,RELATED ACCEPT must sit AFTER the
	// destination DROPs. Otherwise a stale conntrack entry would let a
	// pre-existing flow reach a destination that became forbidden after
	// the entry was created.
	idxEstablished := strings.Index(outStr, "ESTABLISHED")
	if idxEstablished < 0 {
		t.Errorf("ESTABLISHED rule missing from FORWARD chain")
	}
	if idxEstablished < idx169 {
		t.Errorf("ESTABLISHED rule must come AFTER destination DROPs (SEC-01). idxEstablished=%d, idx169=%d", idxEstablished, idx169)
	}
	if idxEstablished < idxDNS {
		t.Errorf("ESTABLISHED rule must come AFTER DNS ACCEPTs (SEC-01). idxEstablished=%d, idxDNS=%d", idxEstablished, idxDNS)
	}
}

func TestForceKillByPIDFile(t *testing.T) {
	cmd := exec.Command("sleep", "300")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("Failed to start sleep: %v", err)
	}
	pid := cmd.Process.Pid
	cmd.Process.Release()
	
	// Create dummy pid file
	SetInstancesRoot("/tmp/voidrun-test")
	os.MkdirAll("/tmp/voidrun-test/test-sandbox", 0755)
	defer os.RemoveAll("/tmp/voidrun-test")
	
	pidFile := GetPIDPath("test-sandbox")
	if err := os.WriteFile(pidFile, []byte(fmt.Sprintf("%d", pid)), 0644); err != nil {
		cmd.Process.Kill()
		t.Fatalf("Failed to write pid file: %v", err)
	}

	// Wait a moment
	time.Sleep(100 * time.Millisecond)

	// Test force kill
	if err := forceKillByPIDFile("test-sandbox"); err != nil {
		t.Errorf("forceKillByPIDFile failed: %v", err)
	}

	// Check if process is still in process table
	if process, err := os.FindProcess(pid); err == nil {
		if err := process.Signal(syscall.Signal(0)); err == nil {
			statData, _ := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
			fields := strings.Fields(string(statData))
			if len(fields) >= 3 {
				state := fields[2]
				if state != "Z" && state != "X" {
					t.Errorf("Process should have been killed, but it is alive (state: %s)", state)
				}
			}
		}
	}
}

// TestForceKillByPIDFile_RefusesNonCH verifies SEC-04: if the pidfile points
// at a process whose cmdline is not the configured cloud-hypervisor binary
// (e.g. PID was reused after the real CLH exited), forceKillByPIDFile must
// refuse to SIGKILL it.
func TestForceKillByPIDFile_RefusesNonCH(t *testing.T) {
	// Save and restore CHBinary so we don't leak state into other tests.
	prev := CHBinary
	CHBinary = "/nonexistent/path/to/cloud-hypervisor"
	defer func() { CHBinary = prev }()

	cmd := exec.Command("sleep", "300")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("Failed to start sleep: %v", err)
	}
	pid := cmd.Process.Pid
	cmd.Process.Release()
	defer func() {
		if p, err := os.FindProcess(pid); err == nil {
			_ = p.Signal(syscall.SIGKILL)
		}
	}()

	SetInstancesRoot("/tmp/voidrun-test-sec04")
	if err := os.MkdirAll("/tmp/voidrun-test-sec04/sec04-sandbox", 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	defer os.RemoveAll("/tmp/voidrun-test-sec04")

	pidFile := GetPIDPath("sec04-sandbox")
	if err := os.WriteFile(pidFile, []byte(fmt.Sprintf("%d", pid)), 0644); err != nil {
		t.Fatalf("Failed to write pid file: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	if err := forceKillByPIDFile("sec04-sandbox"); err != nil {
		t.Errorf("forceKillByPIDFile should swallow PID-mismatch and return nil, got: %v", err)
	}

	// The sleep process must STILL be alive — the cmdline check should have
	// stopped the SIGKILL.
	p, err := os.FindProcess(pid)
	if err != nil {
		t.Fatalf("process %d unexpectedly gone: %v", pid, err)
	}
	if err := p.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("process %d unexpectedly dead: %v", pid, err)
	}
	statData, _ := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	fields := strings.Fields(string(statData))
	if len(fields) >= 3 {
		state := fields[2]
		if state == "Z" || state == "X" {
			t.Errorf("Process should be alive, but is %s — SEC-04 check failed to protect it", state)
		}
	}
}
