//go:build linux

package main

import (
	"bytes"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"voidrun/config"
)

func main() {
	flag.Parse()

	// Fix #5: Fail fast if not running as root (CAP_NET_ADMIN required).
	if os.Geteuid() != 0 {
		log.Fatal("[Net] must be run as root (needs CAP_NET_ADMIN for bridge/iptables)")
	}

	// Load configuration from environment
	cfg := config.New()

	fmt.Println("[Net] Configuring Cloud Hypervisor Network from Config...")

	bridge := cfg.Network.BridgeName
	gatewayIP := cfg.Network.GetCleanGateway()
	gatewayWithMask := cfg.Network.GatewayIP
	subnet := cfg.Network.NetworkCIDR
	proxyPort := cfg.Network.ProxyPort

	fmt.Printf("   [Bridge]     %s\n", bridge)
	fmt.Printf("   [Gateway]    %s\n", gatewayWithMask)
	fmt.Printf("   [Subnet]     %s\n", subnet)
	fmt.Printf("   [ProxyPort]  %d (enabled=%v)\n", proxyPort, cfg.Network.ProxyEnabled)

	// 1. Create bridge if it doesn't exist
	if !bridgeExists(bridge) {
		fmt.Printf("   + Creating Host Bridge %s...\n", bridge)
		if err := run("ip", "link", "add", "name", bridge, "type", "bridge"); err != nil {
			log.Fatalf("Failed to create bridge: %v", err)
		}

		// Set dummy MAC for stability
		if err := run("ip", "link", "set", "dev", bridge, "address", "fe:54:00:00:00:01"); err != nil {
			log.Printf("Warning: Could not set MAC address: %v", err)
		}
	}

	// 2. Add IP address (only if not already assigned)
	if !hasIP(bridge, gatewayIP) {
		fmt.Printf("   + Assigning IP %s to bridge...\n", gatewayWithMask)
		if err := run("ip", "addr", "add", gatewayWithMask, "dev", bridge); err != nil {
			log.Fatalf("Failed to assign IP to bridge: %v", err)
		}
	}

	// 3. Bring bridge up
	fmt.Println("   + Bringing bridge up...")
	if err := run("ip", "link", "set", bridge, "up"); err != nil {
		log.Fatalf("Failed to bring bridge up: %v", err)
	}

	// 4. Enable IP forwarding
	fmt.Println("   + Enabling IP forwarding...")
	if err := run("sysctl", "-w", "net.ipv4.ip_forward=1"); err != nil {
		log.Printf("Warning: Could not enable IP forwarding: %v", err)
	}

	// 5. Detect WAN Interface
	wanIface, err := detectWAN()
	if err != nil {
		log.Fatalf("Could not detect WAN interface: %v", err)
	}
	fmt.Printf("   + Detected WAN Interface: %s\n", wanIface)

	// Extract subnet for iptables rules
	subnetCIDR := extractSubnet(subnet)

	// 6. Add NAT Masquerade rule
	fmt.Println("   + Enabling NAT (Masquerade)...")
	if !iptablesRuleExists("nat", "POSTROUTING", "-s", subnetCIDR, "!", "-d", subnetCIDR, "-j", "MASQUERADE") {
		if err := ipt("-t", "nat", "-I", "POSTROUTING", "1", "-s", subnetCIDR, "!", "-d", subnetCIDR, "-j", "MASQUERADE"); err != nil {
			log.Printf("Warning: Could not add NAT rule: %v", err)
		}
	}

	// 7. Forwarding Rules - Outbound
	fmt.Printf("   + Allow Outbound: %s -> %s\n", bridge, wanIface)
	if !iptablesRuleExists("filter", "FORWARD", "-i", bridge, "-o", wanIface, "-j", "ACCEPT") {
		if err := ipt("-I", "FORWARD", "1", "-i", bridge, "-o", wanIface, "-j", "ACCEPT"); err != nil {
			log.Printf("Warning: Could not add outbound rule: %v", err)
		}
	}

	// 8. Forwarding Rules - Inbound (Established)
	fmt.Printf("   + Allow Inbound (Established): %s -> %s\n", wanIface, bridge)
	if !iptablesRuleExists("filter", "FORWARD", "-i", wanIface, "-o", bridge, "-m", "state", "--state", "RELATED,ESTABLISHED", "-j", "ACCEPT") {
		if err := ipt("-I", "FORWARD", "1", "-i", wanIface, "-o", bridge, "-m", "state", "--state", "RELATED,ESTABLISHED", "-j", "ACCEPT"); err != nil {
			log.Printf("Warning: Could not add inbound rule: %v", err)
		}
	}

	// 9. Redirect VM HTTP (port 80) through the Go forward proxy.
	// Transparently intercepts outbound HTTP from VMs; VMs need no proxy config.
	// Skip this rule when PROXY_ENABLED=false so traffic routes normally without the proxy.
	proxyPortStr := strconv.Itoa(proxyPort)
	redirectArgs := []string{"-i", bridge, "-p", "tcp", "--dport", "80", "-j", "REDIRECT", "--to-port", proxyPortStr}
	if cfg.Network.ProxyEnabled {
		fmt.Printf("   + Redirect VM HTTP (port 80) → localhost:%d via proxy...\n", proxyPort)
		// Flush any stale REDIRECT rules first (e.g. port changed from 8080→8081).
		removeAllProxyRedirects(bridge)
		if !iptablesRuleExists("nat", "PREROUTING", redirectArgs...) {
			if err := ipt(append([]string{"-t", "nat", "-I", "PREROUTING", "1"}, redirectArgs...)...); err != nil {
				log.Printf("Warning: Could not add proxy redirect rule: %v", err)
			}
		}
	} else {
		// Fix #6: Remove ANY existing port-80 REDIRECT on this bridge, regardless of
		// the --to-port value. This handles the case where the proxy port was changed
		// in the config between enabling and disabling the proxy.
		fmt.Println("   ~ Skipping proxy redirect (PROXY_ENABLED=false) — removing stale rules...")
		removeAllProxyRedirects(bridge)
	}

	fmt.Println("[Net] Host Network Configured Successfully.")
}

// run executes a command and returns an error containing stderr for diagnostics.
func run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg != "" {
			return fmt.Errorf("%s %v: %w: %s", name, args, err, msg)
		}
		return fmt.Errorf("%s %v: %w", name, args, err)
	}
	return nil
}

// ipt runs an iptables command with -w (wait for xtables lock) to avoid
// "Another app is currently holding the xtables lock" crashes.
func ipt(args ...string) error {
	return run("iptables", append([]string{"-w"}, args...)...)
}

// iptablesRuleExists checks if an iptables rule already exists.
// Takes table, chain, and rule args as a proper string slice — no string
// concatenation or strings.Fields splitting.
func iptablesRuleExists(table, chain string, ruleArgs ...string) bool {
	args := append([]string{"-w", "-t", table, "-C", chain}, ruleArgs...)
	cmd := exec.Command("iptables", args...)
	return cmd.Run() == nil
}

// removeAllProxyRedirects deletes every PREROUTING REDIRECT rule on this bridge
// for dport 80, regardless of the --to-port value. This handles stale rules left
// behind after a proxy port change.
func removeAllProxyRedirects(bridge string) {
	cmd := exec.Command("iptables", "-w", "-t", "nat", "-S", "PREROUTING")
	out, err := cmd.Output()
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(out), "\n") {
		// Match lines like: -A PREROUTING -i vmbr0 -p tcp -m tcp --dport 80 -j REDIRECT --to-ports 3128
		if !strings.Contains(line, bridge) || !strings.Contains(line, "--dport 80") || !strings.Contains(line, "REDIRECT") {
			continue
		}
		// Convert "-A PREROUTING ..." to "-D PREROUTING ..." for deletion
		delRule := strings.Replace(line, "-A PREROUTING", "-D PREROUTING", 1)
		delArgs := strings.Fields(delRule)
		if len(delArgs) > 0 {
			fmt.Printf("   - Removing stale rule: %s\n", line)
			_ = ipt(append([]string{"-t", "nat"}, delArgs...)...)
		}
	}
}

// bridgeExists checks if a bridge interface exists
func bridgeExists(bridge string) bool {
	cmd := exec.Command("ip", "link", "show", bridge)
	return cmd.Run() == nil
}

// hasIP checks if an IP is already assigned to an interface.
// Uses exact IPv4 comparison to avoid false positives from substring matches
// (e.g. "192.168.100.1" matching within "192.168.100.10").
func hasIP(iface, ip string) bool {
	cmd := exec.Command("ip", "addr", "show", iface)
	output, err := cmd.Output()
	if err != nil {
		return false
	}
	want := net.ParseIP(ip)
	if want == nil || want.To4() == nil {
		return false
	}
	for _, line := range strings.Split(string(output), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "inet ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		addr := strings.Split(fields[1], "/")[0]
		if got := net.ParseIP(addr); got != nil && got.To4() != nil && got.Equal(want) {
			return true
		}
	}
	return false
}

// detectWAN finds the default WAN interface by parsing `ip route show default`
// in pure Go — no dependency on bash or awk.
func detectWAN() (string, error) {
	cmd := exec.Command("ip", "route", "show", "default")
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("ip route show default: %w", err)
	}
	// Output: "default via 10.0.0.1 dev eth0 proto ..."
	// The interface name follows "dev".
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		for i, f := range fields {
			if f == "dev" && i+1 < len(fields) {
				return fields[i+1], nil
			}
		}
	}
	return "", fmt.Errorf("no default route found")
}

// extractSubnet extracts just the subnet from CIDR notation
// e.g., "192.168.100.0/22" -> "192.168.100.0/22"
func extractSubnet(cidr string) string {
	_, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		return cidr
	}
	return ipNet.String()
}
