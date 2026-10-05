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

func TestUpdateCycleOperationsRequireProtocolAndCapability(t *testing.T) {
	valid := &entity.Capabilities{UpdateManagement: true, PrivilegedHelper: true, RunUpgrade: true, UpdateCycle: true, ProtocolVersion: entity.ProtocolVersion}
	if !maintenanceCapabilityAvailable(valid, entity.RunUnattendedUpgrades) || !maintenanceCapabilityAvailable(valid, entity.GetUpdateCycleStatus) {
		t.Fatal("cycle operations rejected when protocol and capability are available")
	}

	cases := []struct {
		name string
		caps *entity.Capabilities
	}{
		{name: "legacy helper with no cycle capability", caps: &entity.Capabilities{UpdateManagement: true, PrivilegedHelper: true, RunUpgrade: true}},
		{name: "old protocol despite capability bit", caps: &entity.Capabilities{UpdateManagement: true, PrivilegedHelper: true, RunUpgrade: true, UpdateCycle: true, ProtocolVersion: entity.ProtocolVersion - 1}},
		{name: "cycle capability without management", caps: &entity.Capabilities{PrivilegedHelper: true, RunUpgrade: true, UpdateCycle: true, ProtocolVersion: entity.ProtocolVersion}},
		{name: "cycle capability without privileged helper", caps: &entity.Capabilities{UpdateManagement: true, RunUpgrade: true, UpdateCycle: true, ProtocolVersion: entity.ProtocolVersion}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, op := range []entity.Operation{entity.RunUnattendedUpgrades, entity.GetUpdateCycleStatus} {
				if maintenanceCapabilityAvailable(tc.caps, op) {
					t.Errorf("operation %q accepted for unsupported capabilities", op)
				}
			}
		})
	}
	if !maintenanceCapabilityAvailable(&entity.Capabilities{UpdateManagement: true, PrivilegedHelper: true}, entity.GetOperationStatus) {
		t.Fatal("non-cycle operation was coupled to the cycle capability")
	}
}

func TestCycleOperationsAreIdentifiedForPollingAudit(t *testing.T) {
	if !isCycleOperation(entity.RunUnattendedUpgrades) || !isCycleOperation(entity.GetUpdateCycleStatus) {
		t.Fatal("cycle operations must use cycle transition events instead of per-request audit events")
	}
	if isCycleOperation(entity.GetOperationStatus) || isCycleOperation(entity.GetUpdatePolicy) {
		t.Fatal("generic operations were classified as cycle polling")
	}
}
