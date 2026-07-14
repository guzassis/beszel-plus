//go:build linux

package agent

import "testing"

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

func TestDisabledPowerDiagnostics(t *testing.T) {
	diagnostics := collectPowerDiagnostics(false)
	if diagnostics.Enabled || diagnostics.State != "disabled" || diagnostics.CollectedAt.IsZero() {
		t.Fatalf("unexpected diagnostics: %#v", diagnostics)
	}
}
