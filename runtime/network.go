package runtime

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"strings"

	"voidrun/config"
	"voidrun/model"
	"voidrun/util"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

const maxIfaceNameLen = 15

// CreateSandboxNetNS creates a fully isolated network namespace for a sandbox.
// It wires it to the host bridge via a veth pair and applies strict firewall rules.
// Returns (nsName, tapName, error). tapName is always "tap0" inside the netns.
func CreateSandboxNetNS(bridgeName, macAddr, netPrefix string, nameservers []string) (nsName, tapName string, err error) {
	defer util.Track("network:CreateSandboxNetNS")()
	// Calculate how many random hex bytes we can fit.
	// Interface name budget: maxIfaceNameLen (15). Separator "-vh-" is 4 chars.
	// So random hex can use at most maxIfaceNameLen - 4 - len(netPrefix) characters.
	randHexLen := maxIfaceNameLen - 4 - len(netPrefix)
	randBytes := randHexLen / 2
	if randBytes < 1 {
		randBytes = 1
	}

	var lastErr error
	for i := 0; i < 5; i++ {
		randPart, err := randomHex(randBytes)
		if err != nil {
			return "", "", err
		}

		ns := netPrefix + "-ns-" + randPart
		hostVeth := netPrefix + "-vh-" + randPart
		nsVeth := netPrefix + "-vn-" + randPart

		if setupErr := setupNetNS(ns, hostVeth, nsVeth, bridgeName, macAddr, nameservers); setupErr != nil {
			lastErr = setupErr
			continue
		}
		return ns, "tap0", nil
	}
	return "", "", fmt.Errorf("failed to create sandbox netns after 5 attempts, last error: %w", lastErr)
}

// EnsureSandboxNetNS checks if the network namespace exists, and if not, recreates it
// with the exact name stored in the spec.
func EnsureSandboxNetNS(cfg config.Config, spec *model.SandboxSpec) error {
	defer util.Track("network:EnsureSandboxNetNS")()
	if spec.NetNSName == "" {
		// If there is no NetNSName, we need to create one.
		return ConfigureNetwork(cfg, spec)
	}

	_, err := os.Stat("/var/run/netns/" + spec.NetNSName)
	if err == nil {
		// Namespace already exists, nothing to do
		return nil
	}

	// Namespace doesn't exist, we must recreate it exactly as it was.
	var hostVeth, nsVeth string
	nsName := spec.NetNSName

	if strings.Contains(nsName, "-ns-") {
		hostVeth = strings.Replace(nsName, "-ns-", "-vh-", 1)
		nsVeth = strings.Replace(nsName, "-ns-", "-vn-", 1)
	} else if len(nsName) > 3 {
		// Legacy format
		suffix := nsName[3:]
		hostVeth = "veth-h-" + suffix
		nsVeth = "veth-n-" + suffix
	} else {
		return fmt.Errorf("unrecognized netns name format: %s", nsName)
	}

	log.Printf("   [Net] Recreating missing NetNS %s (hostVeth: %s, nsVeth: %s)\n", nsName, hostVeth, nsVeth)
	return setupNetNS(nsName, hostVeth, nsVeth, cfg.Network.BridgeName, spec.MacAddress, cfg.Network.Nameservers)
}

// setupNetNS performs all the steps to create a fully wired and firewalled netns.
func setupNetNS(nsName, hostVeth, nsVeth, bridgeName, macAddr string, nameservers []string) error {
	defer util.Track("network:setupNetNS - " + nsName)()
	var ok bool
	// Cleanup guard: on any failure, tear down everything we created so far.
	defer func() {
		if !ok {
			teardownNetNS(nsName, hostVeth)
		}
	}()

	// 1. Create the network namespace
	if err := exec.Command("ip", "netns", "add", nsName).Run(); err != nil {
		return fmt.Errorf("ip netns add %s: %w", nsName, err)
	}

	// 2. Create veth pair in the host namespace (direct netlink syscall, no fork)
	vethLink := &netlink.Veth{
		LinkAttrs: netlink.LinkAttrs{Name: hostVeth},
		PeerName:  nsVeth,
	}
	if err := netlink.LinkAdd(vethLink); err != nil {
		return fmt.Errorf("create veth pair: %w", err)
	}

	// 3. Get the netns handle by name, then move nsVeth into it
	nsHandle, err := netns.GetFromName(nsName)
	if err != nil {
		return fmt.Errorf("open netns handle %s: %w", nsName, err)
	}
	defer nsHandle.Close()

	nsVethLink, err := netlink.LinkByName(nsVeth)
	if err != nil {
		return fmt.Errorf("find nsVeth %s: %w", nsVeth, err)
	}
	if err := netlink.LinkSetNsFd(nsVethLink, int(nsHandle)); err != nil {
		return fmt.Errorf("move veth into netns: %w", err)
	}

	// 4. Attach host veth to bridge and bring it UP
	hostVethLink, err := netlink.LinkByName(hostVeth)
	if err != nil {
		return fmt.Errorf("find hostVeth %s: %w", hostVeth, err)
	}
	brLink, err := netlink.LinkByName(bridgeName)
	if err != nil {
		return fmt.Errorf("find bridge %s: %w", bridgeName, err)
	}
	if err := netlink.LinkSetMaster(hostVethLink, brLink); err != nil {
		return fmt.Errorf("attach hostVeth to bridge: %w", err)
	}
	if err := netlink.LinkSetUp(hostVethLink); err != nil {
		return fmt.Errorf("up hostVeth: %w", err)
	}

	// 5. Batch configuration inside netns (br0, tap0, iptables)
	// WARNING: The iptables-restore heredoc block (<<EOF ... EOF) MUST NOT be indented.
	// bash requires the closing EOF to be at the exact start of the line, and iptables-restore
	// requires its rules (e.g. *filter) to have no leading whitespace.
	var dnsRules string
	for _, ns := range nameservers {
		dnsRules += fmt.Sprintf("-A FORWARD -m physdev --physdev-in tap0 -p udp --dport 53 -d %s -j ACCEPT\n", ns)
		dnsRules += fmt.Sprintf("-A FORWARD -m physdev --physdev-in tap0 -p tcp --dport 53 -d %s -j ACCEPT\n", ns)
	}

	// SEC-01: the conntrack ESTABLISHED,RELATED ACCEPT must sit *after* the
	// destination DROPs, not before them. iptables is first-match-wins, so a
	// stale conntrack entry would otherwise short-circuit policy updates and
	// keep a guest's existing flow alive to a destination that became
	// forbidden after the entry was created (e.g. 169.254.169.254, or a
	// private range added to the blocklist later).
	//
	// With the ACCEPT at the end:
	//   - Anti-spoofing MAC check still runs first (always).
	//   - Destination DROPs run next; any packet from the guest to a forbidden
	//     destination is dropped regardless of conntrack state.
	//   - DNS ACCEPTs whitelist the explicit DNS resolvers.
	//   - ESTABLISHED,RELATED ACCEPT is the final fall-through for legitimate
	//     return traffic, ready for the day we tighten the default policy from
	//     ACCEPT to DROP.
	//
	// Note: this reapplication is per-netns-creation. Existing snapshotted
	// sandboxes keep their old (vulnerable) rule order until SEC-02 (reapply
	// on restore) ships.
	script := fmt.Sprintf(`
set -e
ip link add br0 type bridge
ip link set %s master br0
ip link set %s up
ip link set br0 up

ip tuntap add name tap0 mode tap
ip link set tap0 master br0
ip link set tap0 up

iptables-restore <<EOF
*filter
:INPUT ACCEPT [0:0]
:FORWARD ACCEPT [0:0]
:OUTPUT ACCEPT [0:0]
-A FORWARD -m physdev --physdev-in tap0 -m mac ! --mac-source %s -j DROP
-A FORWARD -m physdev --physdev-in tap0 -d 169.254.169.254 -j DROP
-A FORWARD -m physdev --physdev-in tap0 -d 10.0.0.0/8 -j DROP
-A FORWARD -m physdev --physdev-in tap0 -d 172.16.0.0/12 -j DROP
-A FORWARD -m physdev --physdev-in tap0 -d 192.168.0.0/16 -j DROP
%s-A FORWARD -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT
COMMIT
EOF
`,
		nsVeth, nsVeth, macAddr, dnsRules)

	cmd := exec.Command("ip", "netns", "exec", nsName, "bash", "-c", script)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("netns batch setup failed: %v, output: %s", err, string(out))
	}

	ok = true
	return nil
}

// DeleteSandboxNetNS destroys the network namespace and all resources inside it.
// The kernel automatically garbage-collects: tap0, br0, veth-n, and all iptables rules.
func DeleteSandboxNetNS(nsName string) error {
	defer util.Track("network:DeleteSandboxNetNS - " + nsName)
	if nsName == "" {
		return nil
	}

	var hostVeth string
	if strings.Contains(nsName, "-ns-") {
		hostVeth = strings.Replace(nsName, "-ns-", "-vh-", 1)
	} else if len(nsName) > 3 {
		suffix := nsName[3:] // strip "vr-" prefix
		hostVeth = "veth-h-" + suffix
	}

	if hostVeth != "" {
		if link, err := netlink.LinkByName(hostVeth); err == nil {
			if delErr := netlink.LinkDel(link); delErr != nil {
				log.Printf("   [Net] Warning: failed to delete %s: %v\n", hostVeth, delErr)
			}
		}
	}
	// Delete the namespace — kernel cleans up everything inside atomically
	if out, err := exec.Command("ip", "netns", "del", nsName).CombinedOutput(); err != nil {
		if strings.Contains(string(out), "No such file or directory") {
			return nil // Already deleted or never created
		}
		return fmt.Errorf("ip netns del %s: %w (output: %s)", nsName, err, string(out))
	}
	return nil
}

// teardownNetNS is a best-effort cleanup used during failed creation attempts.
func teardownNetNS(nsName, hostVeth string) {
	if link, err := netlink.LinkByName(hostVeth); err == nil {
		netlink.LinkDel(link)
	}
	exec.Command("ip", "netns", "del", nsName).Run()
}

// randomHex generates n random bytes encoded as a hex string.
func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// GenerateMAC generates a deterministic locally-administered MAC address from an IP.
func GenerateMAC(ip string) string {
	mac := "02:00:00:00:00:01"
	parsedIP := net.ParseIP(ip)
	if parsedIP == nil {
		return mac
	}
	ipv4 := parsedIP.To4()
	if ipv4 == nil {
		return mac
	}
	return fmt.Sprintf("02:00:%02x:%02x:%02x:%02x", ipv4[0], ipv4[1], ipv4[2], ipv4[3])
}

// DeleteTap is kept as a no-op stub for backward compatibility during migration.
// TODO: Remove after all callers are fully migrated to DeleteSandboxNetNS.
func DeleteTap(tapName string) error {
	if tapName == "" {
		return nil
	}
	link, err := netlink.LinkByName(tapName)
	if err != nil {
		return nil
	}
	return netlink.LinkDel(link)
}

// EnsureTapBridge ensures that the tap interface inside the netns is attached to br0.
func EnsureTapBridge(nsName, tapName string) error {
	cmd := exec.Command("ip", "netns", "exec", nsName, "ip", "link", "set", tapName, "master", "br0")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to set tap master bridge: %v, output: %s", err, string(out))
	}
	return nil
}
