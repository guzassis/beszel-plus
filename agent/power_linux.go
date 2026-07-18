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
	powerexec "github.com/henrygd/beszel/internal/power"
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
			// Keep this classification stable; the raw LookPath error varies by
			// libc and must not turn a missing tool into an opaque probe failure.
			item.WOLProbeError = "ethtool is unavailable"
		} else {
			item.WOLProbePath = ethtoolPath
			if supported, enabled, probeErr := ethtoolWOL(ethtoolPath, iface.Name); probeErr != nil {
				item.WOLProbeError = sanitizeUpdateText(probeErr.Error(), 256)
			} else {
				item.WOLSupported, item.WOLEnabled = supported, enabled
			}
		}
		d.Interfaces = append(d.Interfaces, item)
	}
	if len(d.Interfaces) == 0 {
		d.State = powerentity.NoPhysicalEthernet
		d.Reason = "no physical ethernet interface"
		return d
	}
	return setPowerReadiness(d, explicitInterface)
}

// collectPowerDiagnostics enriches the unprivileged topology probe with a
// narrowly scoped root ethtool probe when the kernel hides WOL fields from the
// Agent user. The Agent itself remains unprivileged.
func (a *Agent) collectPowerDiagnostics(enabled bool) *powerentity.Diagnostics {
	d := collectPowerDiagnostics(enabled)
	if !enabled || a.maintenanceManager == nil || !needsPrivilegedWOL(d) {
		return d
	}
	names := make([]string, 0, len(d.Interfaces))
	for _, item := range d.Interfaces {
		names = append(names, item.Interface)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	privileged, err := a.maintenanceManager.probeWOL(ctx, names)
	if err != nil {
		return d
	}
	return mergePrivilegedWOL(d, privileged)
}

func mergePrivilegedWOL(d *powerentity.Diagnostics, privileged []powerentity.InterfaceDiagnostic) *powerentity.Diagnostics {
	byName := make(map[string]powerentity.InterfaceDiagnostic, len(privileged))
	for _, item := range privileged {
		byName[item.Interface] = item
	}
	for i := range d.Interfaces {
		item, ok := byName[d.Interfaces[i].Interface]
		if !ok {
			continue
		}
		d.Interfaces[i].WOLSupported = item.WOLSupported
		d.Interfaces[i].WOLEnabled = item.WOLEnabled
		d.Interfaces[i].WOLProbeError = item.WOLProbeError
		d.Interfaces[i].WOLProbePath = item.WOLProbePath
	}
	return setPowerReadiness(d, strings.TrimSpace(os.Getenv("POWER_INTERFACE")))
}

func needsPrivilegedWOL(d *powerentity.Diagnostics) bool {
	if d == nil || !d.Enabled {
		return false
	}
	for _, item := range d.Interfaces {
		if item.WOLProbeError != "" {
			return true
		}
	}
	return false
}

func setPowerReadiness(d *powerentity.Diagnostics, explicitInterface string) *powerentity.Diagnostics {
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
	} else if strings.Contains(item.WOLProbeError, "ethtool is unavailable") {
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
	supported, enabled, parseErr := powerexec.ParseEthtoolWOL(out)
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
	return powerexec.ParseEthtoolWOL(out)
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
