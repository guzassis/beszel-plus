//go:build linux

package agent

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	powerentity "github.com/henrygd/beszel/internal/entities/power"
)

func collectPowerDiagnostics(enabled bool) *powerentity.Diagnostics {
	d := &powerentity.Diagnostics{Enabled: enabled, State: powerentity.Disabled, CollectedAt: time.Now().UTC()}
	if !enabled {
		d.Reason = "power management disabled"
		return d
	}
	interfaces, err := net.Interfaces()
	if err != nil {
		d.State = powerentity.Unknown
		d.Reason = sanitizeUpdateText(err.Error(), 256)
		return d
	}
	explicitInterface := strings.TrimSpace(os.Getenv("POWER_INTERFACE"))
	ethtoolPath, ethtoolErr := exec.LookPath("ethtool")
	for _, iface := range interfaces {
		if iface.Flags&net.FlagLoopback != 0 || isVirtualPowerInterface(iface.Name) {
			continue
		}
		item := powerentity.InterfaceDiagnostic{Interface: iface.Name, MAC: iface.HardwareAddr.String(), Physical: pathExists(filepath.Join("/sys/class/net", iface.Name, "device"))}
		if !item.Physical {
			continue
		}
		if pathExists(filepath.Join("/sys/class/net", iface.Name, "wireless")) && explicitInterface != iface.Name {
			continue
		}
		item.Type = "ethernet"
		item.OperState = readTrim(filepath.Join("/sys/class/net", iface.Name, "operstate"))
		item.Carrier = readTrim(filepath.Join("/sys/class/net", iface.Name, "carrier")) == "1"
		item.Speed = readTrim(filepath.Join("/sys/class/net", iface.Name, "speed"))
		item.Duplex = readTrim(filepath.Join("/sys/class/net", iface.Name, "duplex"))
		if link, err := os.Readlink(filepath.Join("/sys/class/net", iface.Name, "device/driver")); err == nil {
			item.Driver = filepath.Base(link)
		}
		for _, addr := range mustAddrs(&iface) {
			ip, network, err := net.ParseCIDR(addr.String())
			if err != nil || ip.To4() == nil {
				continue
			}
			ones, _ := network.Mask.Size()
			item.IP = ip.String()
			item.Prefix = ones
			broadcast := make(net.IP, len(ip.To4()))
			for i := range broadcast {
				broadcast[i] = ip.To4()[i] | ^network.Mask[i]
			}
			item.Broadcast = broadcast.String()
			break
		}
		if ethtoolErr != nil {
			err = ethtoolErr
		} else {
			item.WOLProbePath = ethtoolPath
			item.WOLSupported, item.WOLEnabled, err = ethtoolWOL(ethtoolPath, iface.Name)
		}
		if err != nil {
			item.WOLProbeError = sanitizeUpdateText(err.Error(), 256)
		}
		d.Interfaces = append(d.Interfaces, item)
	}
	if len(d.Interfaces) == 0 {
		d.State = powerentity.NoPhysicalEthernet
		d.Reason = "no physical ethernet interface"
		return d
	}
	selected := selectPowerInterface(d.Interfaces, explicitInterface)
	d.SelectedInterface = selected.Interface
	item := selected
	if _, err := net.ParseMAC(item.MAC); err != nil {
		d.State = powerentity.InvalidMAC
		d.Reason = "invalid interface MAC"
	} else if !item.Carrier {
		d.State = powerentity.NoCarrier
		d.Reason = "ethernet carrier is down"
	} else if item.IP == "" {
		d.State = powerentity.NetworkUnassigned
		d.Reason = "interface has no IPv4 address"
	} else if ethtoolErr != nil {
		d.State = powerentity.EthtoolMissing
		d.Reason = "ethtool is unavailable to the Agent service"
	} else if item.WOLProbeError != "" {
		d.State = powerentity.Unknown
		d.Reason = item.WOLProbeError
	} else if !item.WOLSupported {
		d.State = powerentity.Unsupported
		d.Reason = "wol_unsupported"
	} else if !item.WOLEnabled {
		d.State = powerentity.WOLNotEnabled
		d.Reason = "wol_not_enabled"
	} else {
		d.State = powerentity.Ready
	}
	return d
}

func mustAddrs(iface *net.Interface) []net.Addr { out, _ := iface.Addrs(); return out }
func pathExists(path string) bool               { _, err := os.Stat(path); return err == nil }
func readTrim(path string) string               { b, _ := os.ReadFile(path); return strings.TrimSpace(string(b)) }
func ethtoolWOL(path, name string) (supported, enabled bool, probeErr error) {
	if path == "" {
		return false, false, fmt.Errorf("ethtool is unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, commandErr := exec.CommandContext(ctx, path, name).CombinedOutput()
	supported, enabled, parseErr := parseEthtoolWOL(out)
	if parseErr != nil {
		if commandErr != nil {
			detail := sanitizeUpdateText(string(out), 256)
			if detail == "" {
				detail = commandErr.Error()
			}
			return false, false, fmt.Errorf("ethtool probe failed: %s", detail)
		}
		return false, false, parseErr
	}
	// Some ethtool/netlink versions return a non-zero status after printing
	// valid Wake-on fields (usually because of a warning on stderr). The fields
	// are authoritative, so keep the parsed result in that case.
	return supported, enabled, nil
}

func parseEthtoolWOL(out []byte) (supported, enabled bool, probeErr error) {
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
		return false, false, fmt.Errorf("ethtool output did not contain complete Wake-on fields")
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

func selectPowerInterface(items []powerentity.InterfaceDiagnostic, explicit string) powerentity.InterfaceDiagnostic {
	if explicit != "" {
		for _, item := range items {
			if item.Interface == explicit {
				// An explicit interface is authoritative when it is a usable WOL
				// candidate. If an old setting points at a stale/non-WOL interface,
				// let the readiness score select a working physical Ethernet NIC.
				if item.Physical && item.Type == "ethernet" && item.Carrier && item.IP != "" && item.WOLProbeError == "" && item.WOLSupported && item.WOLEnabled {
					return item
				}
				break
			}
		}
	}
	best, bestScore := items[0], -1
	for _, item := range items {
		score := 0
		if item.Carrier {
			score += 8
		}
		if item.IP != "" {
			score += 4
		}
		if item.WOLProbeError == "" {
			score += 2
		}
		if item.WOLSupported {
			score += 16
		}
		if item.WOLEnabled {
			score += 32
		}
		if score > bestScore {
			best, bestScore = item, score
		}
	}
	return best
}
func isVirtualPowerInterface(name string) bool {
	for _, prefix := range []string{"docker", "veth", "br-", "virbr", "tailscale", "wg", "tun", "tap", "lo"} {
		if strings.HasPrefix(strings.ToLower(name), prefix) {
			return true
		}
	}
	return false
}
