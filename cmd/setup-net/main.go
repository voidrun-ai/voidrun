package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"os/exec"
	"strings"

	"voidrun/internal/config"
)

func main() {
	flag.Parse()

	// Load configuration from environment
	cfg := config.New()

	fmt.Println("[Net] Configuring Cloud Hypervisor Network from Config...")

	bridge := cfg.Network.BridgeName
	gatewayIP := cfg.Network.GetCleanGateway()
	gatewayWithMask := cfg.Network.GatewayIP
	subnet := cfg.Network.NetworkCIDR

	fmt.Printf("   [Bridge] %s\n", bridge)
	fmt.Printf("   [Gateway] %s\n", gatewayWithMask)
	fmt.Printf("   [Subnet] %s\n", subnet)

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
	masqueradeRule := fmt.Sprintf("-s %s ! -d %s -j MASQUERADE", subnetCIDR, subnetCIDR)
	if !iptablesRuleExists("nat", "POSTROUTING", masqueradeRule) {
		if err := run("iptables", "-t", "nat", "-I", "POSTROUTING", "1", "-s", subnetCIDR, "!", "-d", subnetCIDR, "-j", "MASQUERADE"); err != nil {
			log.Printf("Warning: Could not add NAT rule: %v", err)
		}
	}

	// 7. Forwarding Rules - Outbound
	fmt.Printf("   + Allow Outbound: %s -> %s\n", bridge, wanIface)
	outboundRule := fmt.Sprintf("-i %s -o %s -j ACCEPT", bridge, wanIface)
	if !iptablesRuleExists("filter", "FORWARD", outboundRule) {
		if err := run("iptables", "-I", "FORWARD", "1", "-i", bridge, "-o", wanIface, "-j", "ACCEPT"); err != nil {
			log.Printf("Warning: Could not add outbound rule: %v", err)
		}
	}

	// 8. Forwarding Rules - Inbound (Established)
	fmt.Printf("   + Allow Inbound (Established): %s -> %s\n", wanIface, bridge)
	inboundRule := fmt.Sprintf("-i %s -o %s -m state --state RELATED,ESTABLISHED -j ACCEPT", wanIface, bridge)
	if !iptablesRuleExists("filter", "FORWARD", inboundRule) {
		if err := run("iptables", "-I", "FORWARD", "1", "-i", wanIface, "-o", bridge, "-m", "state", "--state", "RELATED,ESTABLISHED", "-j", "ACCEPT"); err != nil {
			log.Printf("Warning: Could not add inbound rule: %v", err)
		}
	}

	fmt.Println("[Net] Host Network Configured Successfully.")
}

// run executes a command and returns error if it fails
func run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %v: %w", name, args, err)
	}
	return nil
}

// bridgeExists checks if a bridge interface exists
func bridgeExists(bridge string) bool {
	cmd := exec.Command("ip", "link", "show", bridge)
	return cmd.Run() == nil
}

// hasIP checks if an IP is already assigned to an interface
func hasIP(iface, ip string) bool {
	cmd := exec.Command("ip", "addr", "show", iface)
	output, err := cmd.Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(output), ip)
}

// detectWAN finds the default WAN interface
func detectWAN() (string, error) {
	cmd := exec.Command("bash", "-c", "ip route show default | awk '/default/ {print $5; exit}'")
	output, err := cmd.Output()
	if err != nil {
		return "", err
	}
	wanIface := strings.TrimSpace(string(output))
	if wanIface == "" {
		return "", fmt.Errorf("no default route found")
	}
	return wanIface, nil
}

// iptablesRuleExists checks if an iptables rule already exists
func iptablesRuleExists(table, chain, rule string) bool {
	// Build the iptables check command
	parts := strings.Fields(fmt.Sprintf("-t %s -C %s %s", table, chain, rule))
	cmd := exec.Command("iptables", parts...)
	return cmd.Run() == nil
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
