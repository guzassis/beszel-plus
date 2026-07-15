//go:build linux

package agent

import (
	"context"
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
		d.Reason = err.Error()
		return d
	}
	explicitInterface := strings.TrimSpace(os.Getenv("POWER_INTERFACE"))
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
		item.WOLSupported, item.WOLEnabled, err = ethtoolWOL(iface.Name)
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
	} else if _, err := exec.LookPath("ethtool"); err != nil {
		d.State = powerentity.EthtoolMissing
		d.Reason = "ethtool is unavailable"
	} else if item.WOLProbeError != "" {
		d.State = powerentity.Unknown
		d.Reason = "wol_probe_failed"
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
func ethtoolWOL(name string) (supported, enabled bool, probeErr error) {
	path, err := exec.LookPath("ethtool")
	if err != nil {
		return false, false, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, name).Output()
	if err != nil {
		return false, false, err
	}
	return parseEthtoolWOL(out)
}

func parseEthtoolWOL(out []byte) (supported, enabled bool, probeErr error) {
	for line := range strings.Lines(string(out)) {
		line = strings.TrimSpace(line)
		if value, ok := strings.CutPrefix(line, "Supports Wake-on:"); ok {
			supported = strings.Contains(strings.TrimSpace(value), "g")
		}
		if value, ok := strings.CutPrefix(line, "Wake-on:"); ok {
			enabled = strings.Contains(strings.TrimSpace(value), "g")
		}
	}
	return supported, enabled, nil
}

func selectPowerInterface(items []powerentity.InterfaceDiagnostic, explicit string) powerentity.InterfaceDiagnostic {
	if explicit != "" {
		for _, item := range items {
			if item.Interface == explicit {
				return item
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
