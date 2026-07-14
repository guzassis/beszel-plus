package power

import (
	"bytes"
	"context"
	"net"
	"testing"

	powerentity "github.com/henrygd/beszel/internal/entities/power"
)

type fakeUDP struct{ packets [][]byte }

func (f *fakeUDP) Write(p []byte) (int, error) {
	f.packets = append(f.packets, append([]byte(nil), p...))
	return len(p), nil
}
func (*fakeUDP) Close() error { return nil }

func TestMagicPacketAndThreeSends(t *testing.T) {
	fake := &fakeUDP{}
	executor := LocalHubPowerExecutor{Dial: func(string, *net.UDPAddr, *net.UDPAddr) (UDPWriter, error) { return fake, nil }}
	if err := executor.Wake(context.Background(), WakeRequest{MAC: "02:11:22:33:44:55", Broadcast: "192.168.1.255", Port: 9}); err != nil {
		t.Fatal(err)
	}
	if len(fake.packets) != 3 {
		t.Fatalf("got %d packets", len(fake.packets))
	}
	if len(fake.packets[0]) != 102 {
		t.Fatalf("got packet length %d", len(fake.packets[0]))
	}
	mac, _ := net.ParseMAC("02:11:22:33:44:55")
	expected := append(bytes.Repeat([]byte{0xff}, 6), bytes.Repeat(mac, 16)...)
	if !bytes.Equal(fake.packets[0], expected) {
		t.Fatal("magic packet payload is incorrect")
	}
}

func TestRejectsMulticastMAC(t *testing.T) {
	err := (LocalHubPowerExecutor{}).Wake(context.Background(), WakeRequest{MAC: "01:11:22:33:44:55", Broadcast: "192.168.1.255"})
	if err == nil {
		t.Fatal("expected invalid multicast MAC")
	}
}

func TestValidateReadiness(t *testing.T) {
	diagnostics := &powerentity.Diagnostics{Enabled: true, Interfaces: []powerentity.InterfaceDiagnostic{{
		Interface: "enp1s0", Type: "ethernet", MAC: "02:11:22:33:44:55", Physical: true,
		Carrier: true, WOLSupported: true, WOLEnabled: true,
	}}}
	if err := ValidateReadiness(diagnostics, "enp1s0", "02-11-22-33-44-55"); err != nil {
		t.Fatal(err)
	}
	broken := *diagnostics
	broken.Interfaces = append([]powerentity.InterfaceDiagnostic(nil), diagnostics.Interfaces...)
	broken.Interfaces[0].WOLEnabled = false
	if err := ValidateReadiness(&broken, "enp1s0", "02:11:22:33:44:55"); err == nil {
		t.Fatal("expected disabled magic-packet wake to be rejected")
	}
	if err := ValidateReadiness(diagnostics, "enp2s0", "02:11:22:33:44:55"); err == nil {
		t.Fatal("expected mismatched interface to be rejected")
	}
}
