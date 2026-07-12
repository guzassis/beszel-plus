package hub

import (
	"testing"

	entity "github.com/henrygd/beszel/internal/entities/maintenance"
)

func TestMaintenanceCapabilityChecksAreOperationSpecific(t *testing.T) {
	caps := &entity.Capabilities{UpdateManagement: true, PrivilegedHelper: true, PolicyRead: true}
	if !maintenanceCapabilityAvailable(caps, entity.GetUpdatePolicy) {
		t.Fatal("policy read capability was rejected")
	}
	for _, operation := range []entity.Operation{entity.ApplyUpdatePolicy, entity.RunUpdateDryRun, entity.RunUnattendedUpgrades} {
		if maintenanceCapabilityAvailable(caps, operation) {
			t.Fatalf("operation %s accepted without its capability", operation)
		}
	}
}
