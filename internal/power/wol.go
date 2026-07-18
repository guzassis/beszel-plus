// Package power implements Hub-local Wake-on-LAN without shelling out or
// opening a listening port.
package power

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	powerentity "github.com/henrygd/beszel/internal/entities/power"
)

// ParseEthtoolWOL extracts the magic-packet capability and current setting
// from ethtool output. Both fields are required; incomplete output is usually
// a permission or netlink failure and must not be interpreted as unsupported.
func ParseEthtoolWOL(out []byte) (supported, enabled bool, probeErr error) {
	var supportsFound, enabledFound bool
	for line := range strings.Lines(string(out)) {
		line = strings.ToLower(strings.TrimSpace(line))
		if value, ok := strings.CutPrefix(line, "supports wake-on:"); ok {
			supportsFound = true
			supported = wakeOnToken(value)
		}
		if value, ok := strings.CutPrefix(line, "wake-on:"); ok {
			enabledFound = true
			enabled = wakeOnToken(value)
		}
	}
	if !supportsFound || !enabledFound {
		return false, false, errors.New("ethtool output did not contain complete Wake-on fields")
	}
	return supported, enabled, nil
}

func wakeOnToken(value string) bool {
	for _, token := range strings.FieldsFunc(value, func(r rune) bool {
		return r == ' ' || r == '\t' || r == ','
	}) {
		if token == "g" || (strings.Contains(token, "g") && strings.Trim(token, "pumbg") == "") {
			return true
		}
	}
	return false
}

type Network struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Interface string `json:"interface"`
	Source    string `json:"source"`
	Prefix    string `json:"prefix"`
	Broadcast string `json:"broadcast"`
	Enabled   bool   `json:"enabled"`
}

type WakeRequest struct {
	MAC, Broadcast, Interface string
	Port                      int
}

type PowerExecutor interface {
	Wake(context.Context, WakeRequest) error
}

type UDPWriter interface {
	Write([]byte) (int, error)
	Close() error
}
type DialUDP func(network string, laddr, raddr *net.UDPAddr) (UDPWriter, error)

type LocalHubPowerExecutor struct {
	Dial  DialUDP
	Sends int
}

func (e LocalHubPowerExecutor) Wake(ctx context.Context, req WakeRequest) error {
	mac, err := net.ParseMAC(req.MAC)
	if err != nil || len(mac) != 6 {
		return errors.New("invalid MAC address")
	}
	if mac[0]&1 != 0 || allBytes(mac, 0) || allBytes(mac, 0xff) {
		return errors.New("MAC address must be unicast")
	}
	ip := net.ParseIP(req.Broadcast).To4()
	if ip == nil {
		return errors.New("invalid IPv4 broadcast")
	}
	if req.Port == 0 {
		req.Port = 9
	}
	if req.Port != 7 && req.Port != 9 {
		return errors.New("WOL port must be 7 or 9")
	}
	packet := MagicPacket(mac)
	dial := e.Dial
	if dial == nil {
		dial = func(network string, laddr, raddr *net.UDPAddr) (UDPWriter, error) {
			return net.DialUDP(network, laddr, raddr)
		}
	}
	var local *net.UDPAddr
	if req.Interface != "" {
		localIP, err := interfaceIPv4(req.Interface)
		if err != nil {
			return err
		}
		local = &net.UDPAddr{IP: localIP}
	}
	conn, err := dial("udp4", local, &net.UDPAddr{IP: ip, Port: req.Port})
	if err != nil {
		return err
	}
	defer conn.Close()
	sends := e.Sends
	if sends <= 0 {
		sends = 3
	}
	for i := 0; i < sends; i++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if _, err := conn.Write(packet); err != nil {
			return err
		}
		if i+1 < sends {
			time.Sleep(75 * time.Millisecond)
		}
	}
	return nil
}

func interfaceIPv4(name string) (net.IP, error) {
	iface, err := net.InterfaceByName(name)
	if err != nil {
		return nil, errors.New("configured WOL interface not found")
	}
	addrs, _ := iface.Addrs()
	for _, addr := range addrs {
		ip, _, err := net.ParseCIDR(addr.String())
		if err == nil && ip.To4() != nil {
			return ip.To4(), nil
		}
	}
	return nil, errors.New("configured WOL interface has no IPv4 address")
}

func MagicPacket(mac net.HardwareAddr) []byte {
	packet := make([]byte, 6+16*6)
	for i := 0; i < 6; i++ {
		packet[i] = 0xff
	}
	for i := 0; i < 16; i++ {
		copy(packet[6+i*6:], mac)
	}
	return packet
}
func allBytes(value []byte, expected byte) bool {
	for _, b := range value {
		if b != expected {
			return false
		}
	}
	return true
}

// ValidateReadiness ensures a stored Agent diagnostic still proves that the
// selected physical Ethernet interface is safe to use for Wake-on-LAN. Stale
// diagnostics are accepted because the target is normally offline at wake
// time; callers must surface the age as an administrative warning.
func ValidateReadiness(diagnostics *powerentity.Diagnostics, interfaceName, configuredMAC string) error {
	if diagnostics == nil || !diagnostics.Enabled {
		return errors.New("power diagnostics are unavailable")
	}
	configured, err := net.ParseMAC(configuredMAC)
	if err != nil || len(configured) != 6 || configured[0]&1 != 0 || allBytes(configured, 0) || allBytes(configured, 0xff) {
		return errors.New("stored WOL MAC address is invalid")
	}
	for _, item := range diagnostics.Interfaces {
		if interfaceName != "" && item.Interface != interfaceName {
			continue
		}
		observed, err := net.ParseMAC(item.MAC)
		if err != nil || !bytes.Equal(observed, configured) {
			continue
		}
		if !item.Physical || item.Type != "ethernet" {
			return errors.New("selected WOL interface is not physical Ethernet")
		}
		if !item.Carrier {
			return errors.New("Ethernet carrier was not observed")
		}
		if !item.WOLSupported {
			return errors.New("selected interface does not support magic-packet wake")
		}
		if !item.WOLEnabled {
			return errors.New("magic-packet wake is not enabled on the selected interface")
		}
		return nil
	}
	return errors.New("stored WOL interface does not match the latest Agent diagnostic")
}

func LocalNetworks() ([]Network, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	var result []Network
	preferred := strings.TrimSpace(os.Getenv("POWER_INTERFACE"))
	for _, iface := range interfaces {
		lower := strings.ToLower(iface.Name)
		if iface.Flags&net.FlagLoopback != 0 || iface.Flags&net.FlagBroadcast == 0 || virtualName(lower) {
			continue
		}
		if _, err := os.Stat(filepath.Join("/sys/class/net", iface.Name, "wireless")); err == nil && preferred != iface.Name {
			continue
		}
		addrs, _ := iface.Addrs()
		for _, addr := range addrs {
			ip, subnet, err := net.ParseCIDR(addr.String())
			if err != nil || ip.To4() == nil {
				continue
			}
			ones, _ := subnet.Mask.Size()
			broadcast := make(net.IP, 4)
			for i := range broadcast {
				broadcast[i] = ip.To4()[i] | ^subnet.Mask[i]
			}
			result = append(result, Network{ID: iface.Name, Name: iface.Name, Interface: iface.Name, Source: "discovered", Prefix: fmt.Sprintf("%s/%d", subnet.IP, ones), Broadcast: broadcast.String(), Enabled: preferred == "" || preferred == iface.Name})
		}
	}
	return result, nil
}

func virtualName(name string) bool {
	for _, prefix := range []string{"docker", "veth", "br-", "virbr", "tailscale", "wg", "tun", "tap"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}
