package agent

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/henrygd/beszel"
	entity "github.com/henrygd/beszel/internal/entities/maintenance"
	updateentity "github.com/henrygd/beszel/internal/entities/update"
)

func TestCycleSchedulerPublishesProgressWhileHoldingMaintenanceGate(t *testing.T) {
	for _, state := range []string{"running", "gate held", "reconciliation"} {
		t.Run(state, func(t *testing.T) {
			policy := entity.DefaultPolicy()
			revision := strings.Repeat("a", 64)
			now := time.Now().UTC()
			cycle := &entity.CycleStatus{CycleID: "cycle-00000000000000000001", Source: entity.CycleSourceAutomatic, State: entity.CycleRunning, Stage: entity.CycleStageInstall, PolicyRevision: revision, UpdatedAt: &now, Counts: &entity.CycleCounts{Eligible: 120}}
			caps := &entity.Capabilities{HelperVersion: beszel.PlusVersion, ProtocolVersion: entity.ProtocolVersion, MaxProtocolVersion: entity.ProtocolVersion, UpdateManagement: true, UpdateCycle: true, PrivilegedHelper: true}
			manager := &maintenanceManager{agent: &Agent{}, enabled: true, manageUpdatesEnabled: true, cycleAdoptionChecked: true, caps: caps, cycleStatePath: filepath.Join(t.TempDir(), "cycle.json")}
			manager.running = state == "running"
			manager.cycleGateHeld = state == "gate held"
			manager.cycleReconciliation = state == "reconciliation"
			queries := 0
			manager.callFunc = func(_ context.Context, req entity.Request) (entity.Response, error) {
				if req.Operation != entity.GetUpdateCycleStatus {
					t.Fatalf("poll launched another operation: %s", req.Operation)
				}
				queries++
				return entity.Response{Status: entity.StateCompleted, Result: &entity.Result{UpdateCycle: cycle, Policy: &policy, PolicyRevision: revision, NextCycleID: "cycle-00000000000000000002"}}, nil
			}
			manager.cycleScheduleStep()
			status := &updateentity.Status{}
			manager.attachCycleSnapshot(status)
			if queries != 1 || status.Cycle == nil || status.Cycle.Stage != entity.CycleStageInstall || status.Cycle.Counts.Eligible != 120 || !status.CycleManagementEnabled {
				t.Fatalf("passive progress missing: %#v queries=%d", status, queries)
			}
			if manager.running != (state == "running") || manager.cycleGateHeld != (state == "gate held") || manager.cycleReconciliation != (state == "reconciliation") {
				t.Fatal("status poll released a maintenance gate")
			}
			reloaded := &maintenanceManager{cycleStatePath: manager.cycleStatePath}
			reloaded.loadCycleState()
			if reloaded.cycleStatus == nil || reloaded.cycleStatus.Stage != entity.CycleStageInstall {
				t.Fatal("progress snapshot did not survive Agent restart")
			}
		})
	}
}

func TestCycleSchedulerPreservesMonitorOffAndZeroInterval(t *testing.T) {
	for _, mode := range []string{"monitor", "off", "zero", "management disabled"} {
		t.Run(mode, func(t *testing.T) {
			policy := entity.DefaultPolicy()
			if mode == "monitor" {
				policy.Mode = entity.ModeMonitorOnly
			}
			if mode == "off" {
				policy.Enabled = false
			}
			if mode == "zero" {
				policy.UnattendedUpgradeDays = 0
			}
			caps := &entity.Capabilities{HelperVersion: beszel.PlusVersion, ProtocolVersion: entity.ProtocolVersion, MaxProtocolVersion: entity.ProtocolVersion, UpdateManagement: true, UpdateCycle: true}
			manager := &maintenanceManager{manageUpdatesEnabled: mode != "management disabled", cycleAdoptionChecked: true, caps: caps}
			manager.callFunc = func(_ context.Context, req entity.Request) (entity.Response, error) {
				if req.Operation != entity.GetUpdateCycleStatus {
					t.Fatalf("disabled schedule invoked %s", req.Operation)
				}
				return entity.Response{Status: entity.StateCompleted, Result: &entity.Result{Policy: &policy, PolicyRevision: strings.Repeat("a", 64), NextCycleID: "cycle-00000000000000000001"}}, nil
			}
			manager.cycleScheduleStep()
		})
	}
}

func TestCycleSnapshotIgnoresDelayedResponsesAndPersistsNewestState(t *testing.T) {
	manager := &maintenanceManager{cycleStatePath: filepath.Join(t.TempDir(), "cycle.json")}
	now := time.Now().UTC()
	current := &entity.CycleStatus{CycleID: "cycle-00000000000000000002", State: entity.CycleCompleted, UpdatedAt: &now}
	manager.storeCycleSnapshot(current, nil, "", "")
	oldTime := now.Add(-time.Minute)
	for _, delayed := range []*entity.CycleStatus{
		{CycleID: "cycle-00000000000000000001", State: entity.CycleRunning, UpdatedAt: &now},
		{CycleID: current.CycleID, State: entity.CycleRunning, UpdatedAt: &oldTime},
		{CycleID: current.CycleID, State: entity.CycleRunning},
	} {
		manager.storeCycleSnapshot(delayed, nil, "", "")
	}
	reloaded := &maintenanceManager{cycleStatePath: manager.cycleStatePath}
	reloaded.loadCycleState()
	if reloaded.cycleStatus == nil || reloaded.cycleStatus.CycleID != current.CycleID || reloaded.cycleStatus.State != entity.CycleCompleted {
		t.Fatalf("delayed response replaced verified progress: %#v", reloaded.cycleStatus)
	}
}

func TestCycleReconciliationRecoversIPCFailureBeforeClaim(t *testing.T) {
	previous := cycleReconcileInterval
	cycleReconcileInterval = time.Millisecond
	t.Cleanup(func() { cycleReconcileInterval = previous })
	for _, scenario := range []string{"accepted", "revoked", "busy then accepted"} {
		t.Run(scenario, func(t *testing.T) {
			manager := &maintenanceManager{agent: &Agent{}}
			var mu sync.Mutex
			runs := 0
			manager.callFunc = func(_ context.Context, req entity.Request) (entity.Response, error) {
				mu.Lock()
				defer mu.Unlock()
				switch req.Operation {
				case entity.GetUpdateCycleStatus:
					return entity.Response{Status: entity.StateFailed, ErrorCode: "cycle_expired"}, nil
				case entity.GetPoweroffStatus:
					return entity.Response{Status: entity.StateCompleted}, nil
				case entity.RunUnattendedUpgrades:
					runs++
					if scenario == "revoked" {
						return entity.Response{Status: entity.StateFailed, ErrorCode: "policy_disabled"}, nil
					}
					if scenario == "busy then accepted" && runs == 1 {
						return entity.Response{Status: entity.StateFailed, Error: "another maintenance operation is running"}, nil
					}
					return entity.Response{Status: entity.StateCompleted, Result: &entity.Result{UpdateCycle: &entity.CycleStatus{CycleID: req.CycleID, Source: req.Source, PolicyRevision: req.PolicyRevision, State: entity.CycleCompleted}}}, nil
				default:
					return entity.Response{}, fmt.Errorf("unexpected operation %s", req.Operation)
				}
			}
			manager.retainCycleGate(false, "cycle-00000000000000000001", entity.CycleSourceAutomatic, strings.Repeat("a", 64))
			waitForMaintenance(t, func() bool {
				manager.mu.Lock()
				defer manager.mu.Unlock()
				return !manager.cycleGateHeld && !manager.cycleReconciliation
			})
			mu.Lock()
			defer mu.Unlock()
			want := 1
			if scenario == "busy then accepted" {
				want = 2
			}
			if runs != want {
				t.Fatalf("unclaimed cycle replayed %d times, want %d", runs, want)
			}
		})
	}
}
