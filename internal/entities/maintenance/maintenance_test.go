package maintenance

import (
	"strings"
	"testing"

	"github.com/fxamacker/cbor/v2"
	powerentity "github.com/henrygd/beszel/internal/entities/power"
)

func TestValidatePolicies(t *testing.T) {
	for _, mode := range []PolicyMode{ModeMonitorOnly, ModeSecurity, ModeOfficialAll} {
		policy := DefaultPolicy()
		policy.Mode = mode
		if err := ValidatePolicy(policy); err != nil {
			t.Fatalf("%s: %v", mode, err)
		}
	}
	custom := DefaultPolicy()
	custom.Mode = ModeCustom
	custom.AllowedRepositories = []string{"0123456789abcdef"}
	if err := ValidatePolicy(custom); err != nil {
		t.Fatal(err)
	}
}

func TestProbeWOLWireRoundTrip(t *testing.T) {
	original := Request{Version: ProtocolVersion, RequestID: "wol-request", Operation: ProbeWOL, Interfaces: []string{"eno1", "enp2s0"}}
	data, err := cbor.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Request
	if err := cbor.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Operation != ProbeWOL || len(decoded.Interfaces) != 2 || decoded.Interfaces[0] != "eno1" {
		t.Fatalf("decoded=%#v", decoded)
	}
	response := Response{Version: ProtocolVersion, Operation: ProbeWOL, Status: StateCompleted, Result: &Result{PowerInterfaces: []powerentity.InterfaceDiagnostic{{Interface: "eno1", WOLSupported: true, WOLEnabled: true}}}}
	data, err = cbor.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	var decodedResponse Response
	if err := cbor.Unmarshal(data, &decodedResponse); err != nil {
		t.Fatal(err)
	}
	if decodedResponse.Result == nil || len(decodedResponse.Result.PowerInterfaces) != 1 || !decodedResponse.Result.PowerInterfaces[0].WOLEnabled {
		t.Fatalf("response=%#v", decodedResponse)
	}
}

func TestRejectsMaliciousPolicyAndRequest(t *testing.T) {
	tests := []string{"../evil", "Debian\";APT::Get::AllowUnauthenticated \"true", "repo\nvalue", strings.Repeat("a", 200)}
	for _, value := range tests {
		policy := DefaultPolicy()
		policy.Mode = ModeCustom
		policy.AllowedRepositories = []string{value}
		if ValidatePolicy(policy) == nil {
			t.Fatalf("accepted %q", value)
		}
	}
	req := Request{Version: ProtocolVersion, RequestID: "request-123", Operation: Operation("shell")}
	if ValidateRequest(req) == nil {
		t.Fatal("accepted unknown operation")
	}
	req.Operation = GetCapabilities
	req.RequestID = "bad\nrequest"
	if ValidateRequest(req) == nil {
		t.Fatal("accepted invalid request ID")
	}
}

func TestProbeWOLRequestValidatesInterfaceNames(t *testing.T) {
	req := Request{Version: ProtocolVersion, RequestID: "wol-request", Operation: ProbeWOL, Interfaces: []string{"eno1"}}
	if err := ValidateRequest(req); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"../ethtool", "/proc/self", strings.Repeat("a", 16)} {
		req.Interfaces = []string{name}
		if err := ValidateRequest(req); err == nil {
			t.Fatalf("accepted unsafe interface name %q", name)
		}
	}
	req.Interfaces = nil
	if err := ValidateRequest(req); err == nil {
		t.Fatal("accepted WOL probe without interfaces")
	}
	req.Operation = GetCapabilities
	req.Interfaces = []string{"eno1"}
	if err := ValidateRequest(req); err == nil {
		t.Fatal("accepted interface list on another operation")
	}
}
