//go:build linux

package agent

import (
	"os"
	"path/filepath"
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
	if got := selectPowerInterface(items, "enp0s0"); got.Interface != "enp1s0" {
		t.Fatalf("stale explicit selection returned %q, want enp1s0", got.Interface)
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

func TestEthtoolWOLKeepsValidFieldsWhenCommandReturnsWarning(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ethtool")
	script := "#!/bin/sh\nprintf '%s\\n' 'Supports Wake-on: pumbg' 'Wake-on: g' >&1\nprintf '%s\\n' 'netlink warning' >&2\nexit 1\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	supported, enabled, err := ethtoolWOL(path, "enp1s0")
	if err != nil || !supported || !enabled {
		t.Fatalf("supported=%v enabled=%v err=%v", supported, enabled, err)
	}
}

func TestParseEthtoolWOLRejectsIncompleteOutput(t *testing.T) {
	if _, _, err := parseEthtoolWOL([]byte("netlink error: Operation not permitted\n")); err == nil {
		t.Fatal("expected incomplete ethtool output to fail")
	}
}

func TestDisabledPowerDiagnostics(t *testing.T) {
	diagnostics := collectPowerDiagnostics(false)
	if diagnostics.Enabled || diagnostics.State != "disabled" || diagnostics.CollectedAt.IsZero() {
		t.Fatalf("unexpected diagnostics: %#v", diagnostics)
	}
}

func TestMergePrivilegedWOLProbeMakesIncompleteUnprivilegedProbeReady(t *testing.T) {
	diagnostics := &powerentity.Diagnostics{
		Enabled: true,
		Interfaces: []powerentity.InterfaceDiagnostic{{
			Interface: "eno1", Type: "ethernet", MAC: "3c:7c:3f:79:dd:69", IP: "192.168.1.2",
			Physical: true, Carrier: true, WOLProbeError: "ethtool output did not contain complete Wake-on fields",
		}},
	}
	got := mergePrivilegedWOL(diagnostics, []powerentity.InterfaceDiagnostic{{
		Interface: "eno1", Type: "ethernet", Physical: true, WOLSupported: true, WOLEnabled: true,
		WOLProbePath: "/usr/sbin/ethtool",
	}})
	if got.State != powerentity.Ready || got.SelectedInterface != "eno1" || got.Interfaces[0].WOLProbeError != "" {
		t.Fatalf("privileged probe was not merged: %#v", got)
	}
}
