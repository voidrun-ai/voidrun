package network

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"voidrun/pkg/timer"

	"github.com/vishvananda/netlink"
)

const maxIfaceNameLen = 15

// CreateRandomTap creates the interface and sets MAC, BUT DOES NOT ATTACH TO BRIDGE yet.
// This prevents EBUSY errors when Cloud Hypervisor tries to configure it.
func CreateRandomTap(macAddr string, tapPrefix string) (string, error) {
	defer timer.Track("CreateRandomTap (Total)")()
	// Validate that TAP prefix leaves room for 6-hex suffix (total ≤ 15)
	if len(tapPrefix)+6 > maxIfaceNameLen {
		return "", fmt.Errorf("tap prefix too long: len=%d; must be ≤ %d (prefix + 6 hex ≤ 15)", len(tapPrefix), maxIfaceNameLen-6)
	}
	for i := 0; i < 5; i++ {
		bytes := make([]byte, 3)
		if _, err := rand.Read(bytes); err != nil {
			return "", err
		}
		tapName := tapPrefix + hex.EncodeToString(bytes)

		if len(tapName) > maxIfaceNameLen {
			return "", fmt.Errorf("tap name too long: len=%d; max=%d; reduce TAP_PREFIX", len(tapName), maxIfaceNameLen)
		}

		tap := &netlink.Tuntap{
			LinkAttrs: netlink.LinkAttrs{Name: tapName},
			Mode:      netlink.TUNTAP_MODE_TAP,
		}

		if err := netlink.LinkAdd(tap); err != nil {
			continue
		}

		// Configure MAC while standalone and DOWN
		tapLink, err := netlink.LinkByName(tapName)
		if err != nil {
			return "", err
		}

		hwAddr, err := net.ParseMAC(macAddr)
		if err != nil {
			netlink.LinkDel(tapLink)
			return "", fmt.Errorf("bad mac: %v", err)
		}

		if err := netlink.LinkSetHardwareAddr(tapLink, hwAddr); err != nil {
			netlink.LinkDel(tapLink)
			return "", fmt.Errorf("failed to set mac: %v", err)
		}

		// RETURN NOW. Do not attach to bridge yet.
		return tapName, nil
	}
	return "", fmt.Errorf("failed to generate unique tap")
}

// EnableTap connects the TAP to the bridge and brings it UP.
// Call this AFTER Cloud Hypervisor has started.
func EnableTap(bridgeName string, tapName string) error {
	tapLink, err := netlink.LinkByName(tapName)
	if err != nil {
		return err
	}

	brLink, err := netlink.LinkByName(bridgeName)
	if err != nil {
		return fmt.Errorf("bridge not found: %v", err)
	}

	if err := netlink.LinkSetMaster(tapLink, brLink); err != nil {
		return fmt.Errorf("failed to attach bridge: %v", err)
	}

	if err := netlink.LinkSetUp(tapLink); err != nil {
		return fmt.Errorf("failed to up tap: %v", err)
	}

	return nil
}

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

func GenerateMAC(ip string) string {
	mac := "AA:FC:00:00:00:01"
	parsedIP := net.ParseIP(ip)
	if parsedIP == nil {
		return mac
	}
	ipv4 := parsedIP.To4()
	if ipv4 == nil {
		return mac
	}
	return fmt.Sprintf("AA:FC:00:00:00:%02X", ipv4[3])
}
