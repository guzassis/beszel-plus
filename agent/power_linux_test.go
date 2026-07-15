//go:build linux

package agent

import (
	"testing"

	powerentity "github.com/henrygd/beszel/internal/entities/power"
)

func TestPowerInterfaceVirtualFiltering(t *testing.T) {
	for _, name := range []string{"lo", "docker0", "veth123", "br-deadbeef", "tailscale0", "wg0", "virbr0"} {
		if !isVirtualPowerInterface(name) {
			t.Errorf("%s should be ignored", name)
		}
	}
	if isVirtualPowerInterface("enp3s0") {
		t.Fatal("physical ethernet name was ignored")
	}
}

func TestSelectPowerInterfacePrefersReadyWOL(t *testing.T) {
	items := []powerentity.InterfaceDiagnostic{
		{Interface: "enp0s0", Carrier: true, IP: "192.0.2.10"},
		{Interface: "enp1s0", Carrier: true, IP: "192.0.2.11", WOLSupported: true, WOLEnabled: true},
	}
	if got := selectPowerInterface(items, ""); got.Interface != "enp1s0" {
		t.Fatalf("selected %q, want enp1s0", got.Interface)
	}
	if got := selectPowerInterface(items, "enp0s0"); got.Interface != "enp0s0" {
		t.Fatalf("explicit selection returned %q", got.Interface)
	}
}

func TestSelectPowerInterfaceDoesNotTreatProbeFailureAsUnsupported(t *testing.T) {
	items := []powerentity.InterfaceDiagnostic{
		{Interface: "enp0s0", Carrier: true, IP: "192.0.2.10", WOLProbeError: "permission denied"},
		{Interface: "enp1s0", Carrier: true, IP: "192.0.2.11", WOLSupported: true},
	}
	if got := selectPowerInterface(items, ""); got.Interface != "enp1s0" {
		t.Fatalf("selected %q, want successfully probed interface", got.Interface)
	}
}

func TestParseEthtoolWOLMagicPacket(t *testing.T) {
	supported, enabled, err := parseEthtoolWOL([]byte("Supports Wake-on: pumbg\nWake-on: g\n"))
	if err != nil || !supported || !enabled {
		t.Fatalf("supported=%v enabled=%v err=%v", supported, enabled, err)
	}
}

func TestDisabledPowerDiagnostics(t *testing.T) {
	diagnostics := collectPowerDiagnostics(false)
	if diagnostics.Enabled || diagnostics.State != "disabled" || diagnostics.CollectedAt.IsZero() {
		t.Fatalf("unexpected diagnostics: %#v", diagnostics)
	}
}
