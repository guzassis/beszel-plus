package maintenance

import (
	"strings"
	"testing"
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
