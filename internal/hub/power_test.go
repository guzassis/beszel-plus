package hub

import (
	"testing"

	powerentity "github.com/henrygd/beszel/internal/entities/power"
	powerexec "github.com/henrygd/beszel/internal/power"
)

func TestPowerManagementAvailableFromAgentDiagnostics(t *testing.T) {
	if !powerManagementAvailable(false, &powerentity.Diagnostics{Enabled: true, State: powerentity.Unsupported}) {
		t.Fatal("shutdown must remain available when power management is enabled but WOL is unsupported")
	}
	if powerManagementAvailable(false, &powerentity.Diagnostics{Enabled: false}) {
		t.Fatal("disabled diagnostics unexpectedly enabled power management")
	}
	if !powerManagementAvailable(true, nil) {
		t.Fatal("stored configuration should remain backward compatible")
	}
}

func TestSelectWakeInterfaceUsesAgentDiagnosticAndIgnoresLegacyHubInterface(t *testing.T) {
	diagnostics := &powerentity.Diagnostics{
		Enabled:           true,
		SelectedInterface: "enp1s0",
		Interfaces: []powerentity.InterfaceDiagnostic{
			{Interface: "enp1s0", Type: "ethernet", Physical: true, Carrier: true, MAC: "02:11:22:33:44:55", IP: "192.168.1.20", Broadcast: "192.168.1.255", WOLSupported: true, WOLEnabled: true},
		},
	}
	selected, err := selectWakeInterface(diagnostics, "eth0")
	if err != nil {
		t.Fatal(err)
	}
	if selected.Interface != "enp1s0" || selected.MAC != "02:11:22:33:44:55" {
		t.Fatalf("selected wrong Agent interface: %#v", selected)
	}
}

func TestSelectWakeInterfaceFallsBackFromStaleConfiguredInterface(t *testing.T) {
	diagnostics := &powerentity.Diagnostics{
		Enabled: true,
		Interfaces: []powerentity.InterfaceDiagnostic{
			{Interface: "eth0", Type: "ethernet", Physical: true, Carrier: true, MAC: "02:11:22:33:44:66", WOLSupported: false},
			{Interface: "enp1s0", Type: "ethernet", Physical: true, Carrier: true, MAC: "02:11:22:33:44:55", WOLSupported: true, WOLEnabled: true},
		},
	}
	selected, err := selectWakeInterface(diagnostics, "eth0")
	if err != nil {
		t.Fatal(err)
	}
	if selected.Interface != "enp1s0" {
		t.Fatalf("selected stale interface %q", selected.Interface)
	}
}

func TestSelectWakeNetworkUsesMostSpecificEnabledSubnet(t *testing.T) {
	network, ok := selectWakeNetwork([]powerexec.Network{
		{Interface: "enp1s0", Prefix: "192.168.0.0/16", Broadcast: "192.168.255.255", Enabled: true},
		{Interface: "enp2s0", Prefix: "192.168.1.0/24", Broadcast: "192.168.1.255", Enabled: true},
		{Interface: "eth9", Prefix: "192.168.1.0/24", Broadcast: "192.168.1.255", Enabled: false},
	}, "192.168.1.20")
	if !ok || network.Interface != "enp2s0" {
		t.Fatalf("selected network %#v, ok=%v", network, ok)
	}
}
