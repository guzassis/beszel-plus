//go:build testing

package systems

import (
	"testing"

	maintenanceentity "github.com/henrygd/beszel/internal/entities/maintenance"
	updateentity "github.com/henrygd/beszel/internal/entities/update"
)

func TestUpdateCycleEventsFollowPersistedCycleTransitions(t *testing.T) {
	previous := &updateentity.Status{CycleManagementEnabled: true, Cycle: &maintenanceentity.CycleStatus{
		CycleID: "cycle-00000000000000000001", Attempt: 1, State: maintenanceentity.CycleRunning, Stage: maintenanceentity.CycleStageRefresh,
	}}
	replayed := &updateentity.Status{CycleManagementEnabled: true, Cycle: &maintenanceentity.CycleStatus{
		CycleID: "cycle-00000000000000000001", Attempt: 1, State: maintenanceentity.CycleRunning, Stage: maintenanceentity.CycleStageRefresh,
	}}
	if event, id, _ := updateCycleEvent(previous, replayed); event != "" || id != "" {
		t.Fatalf("same snapshot generated duplicate event %q for %q", event, id)
	}

	progressed := &updateentity.Status{CycleManagementEnabled: true, Cycle: &maintenanceentity.CycleStatus{
		CycleID: "cycle-00000000000000000001", Attempt: 1, State: maintenanceentity.CycleRunning, Stage: maintenanceentity.CycleStageInstall,
	}}
	if event, _, _ := updateCycleEvent(previous, progressed); event != "" {
		t.Fatalf("stage-only progress generated another cycle event: %q", event)
	}

	retrying := &updateentity.Status{CycleManagementEnabled: true, Cycle: &maintenanceentity.CycleStatus{
		CycleID: "cycle-00000000000000000001", Attempt: 1, State: maintenanceentity.CycleRetryWait,
	}}
	if event, id, attempt := updateCycleEvent(previous, retrying); event != "upgrade_retrying" || id != previous.Cycle.CycleID || attempt != 1 {
		t.Fatalf("retry event = %q, %q, %d", event, id, attempt)
	}

	retryStarted := &updateentity.Status{CycleManagementEnabled: true, Cycle: &maintenanceentity.CycleStatus{
		CycleID: "cycle-00000000000000000001", Attempt: 2, State: maintenanceentity.CycleRunning,
	}}
	if event, id, attempt := updateCycleEvent(retrying, retryStarted); event != "upgrade_started" || id != previous.Cycle.CycleID || attempt != 2 {
		t.Fatalf("second attempt event = %q, %q, %d", event, id, attempt)
	}

	completed := &updateentity.Status{CycleManagementEnabled: true, Cycle: &maintenanceentity.CycleStatus{
		CycleID: "cycle-00000000000000000001", Attempt: 2, State: maintenanceentity.CycleCompleted,
	}}
	if event, _, attempt := updateCycleEvent(retryStarted, completed); event != "upgrade_succeeded" || attempt != 2 {
		t.Fatalf("completion event = %q, attempt %d", event, attempt)
	}
	pending := &updateentity.Status{CycleManagementEnabled: true, Cycle: &maintenanceentity.CycleStatus{
		CycleID: "cycle-00000000000000000002", Attempt: 1, State: maintenanceentity.CycleCompletedWithPending,
	}}
	if event, _, _ := updateCycleEvent(nil, pending); event != "upgrade_completed_with_pending" {
		t.Fatalf("pending completion was conflated with an empty successful cycle: %q", event)
	}
}

func TestCycleEventRecordIDUsesCycleAttemptAndOutcome(t *testing.T) {
	base := cycleEventRecordID("system", "user", "cycle-00000000000000000001", 1, "upgrade_started")
	if len(base) != 15 {
		t.Fatalf("PocketBase record ID must be 15 chars, got %q", base)
	}
	if base != cycleEventRecordID("system", "user", "cycle-00000000000000000001", 1, "upgrade_started") {
		t.Fatal("same cycle transition must produce a stable event key")
	}
	for _, other := range []string{
		cycleEventRecordID("system", "user", "cycle-00000000000000000001", 2, "upgrade_started"),
		cycleEventRecordID("system", "user", "cycle-00000000000000000001", 1, "upgrade_failed"),
		cycleEventRecordID("system-2", "user", "cycle-00000000000000000001", 1, "upgrade_started"),
	} {
		if base == other {
			t.Fatalf("distinct cycle event keys collided: %q", base)
		}
	}
}
