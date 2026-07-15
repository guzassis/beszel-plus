package hub

import (
	"testing"

	powerentity "github.com/henrygd/beszel/internal/entities/power"
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
