package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"
	"github.com/henrygd/beszel"
	"github.com/henrygd/beszel/internal/common"
	entity "github.com/henrygd/beszel/internal/entities/maintenance"
	powerentity "github.com/henrygd/beszel/internal/entities/power"
)

func TestMaintenanceManagerRefusesRequestsDuringUpgradeDrain(t *testing.T) {
	originalPath := upgradeDrainPath
	upgradeDrainPath = filepath.Join(t.TempDir(), "upgrade-in-progress")
	t.Cleanup(func() { upgradeDrainPath = originalPath })
	if err := os.WriteFile(upgradeDrainPath, []byte(`{"pid":`+fmt.Sprint(os.Getpid())+`}`), 0o644); err != nil {
		t.Fatal(err)
	}
	m := &maintenanceManager{enabled: true, replays: map[string]entity.Response{}, agent: &Agent{}}
	for _, operation := range []entity.Operation{entity.GetCapabilities, entity.RunUpdateDryRun, entity.ApplyUpdatePolicy, entity.SchedulePoweroff, entity.CancelPoweroff} {
		request := entity.Request{Version: entity.ProtocolVersion, RequestID: "drain-test", Operation: operation}
		if entity.IsLongOperation(operation) {
			request.IdempotencyKey = "drain-test"
		}
		if operation == entity.ApplyUpdatePolicy {
			policy := entity.DefaultPolicy()
			request.Policy = &policy
		}
		response := m.handle(request)
		if response.Status != entity.StateFailed || response.ErrorCode != "upgrade_in_progress" || !response.Retryable || response.Stage != "agent_drain" {
			t.Fatalf("operation %s was not drained: %#v", operation, response)
		}
	}
}

func TestHelperCompatibilityMatchesBuiltAgentVersion(t *testing.T) {
	version := beszel.PlusVersion
	switch {
	case version == "0.3.1-dev", version == "0.3.1":
	case strings.HasPrefix(version, "0.3.1-snapshot.") && len(strings.TrimPrefix(version, "0.3.1-snapshot.")) == 40:
	default:
		t.Fatalf("unexpected Agent build version %q; run this test with the dev, snapshot or release build version", version)
	}
	caps := &entity.Capabilities{
		HelperVersion:      version,
		MinAgentVersion:    "0.3.0",
		ProtocolVersion:    entity.ProtocolVersion,
		MaxProtocolVersion: entity.ProtocolVersion,
		UpdateManagement:   true,
		UpdateCycle:        true,
	}
	if !helperCompatible(caps) {
		t.Fatalf("matching Agent/helper pair %q was rejected", version)
	}

	wrongHelper := *caps
	wrongHelper.HelperVersion = "0.3.0"
	if helperCompatible(&wrongHelper) {
		t.Fatal("helper with a different product version was accepted")
	}

	wrongProtocol := *caps
	wrongProtocol.ProtocolVersion = entity.ProtocolVersion - 1
	if helperCompatible(&wrongProtocol) {
		t.Fatal("helper with an older protocol was accepted")
	}
	wrongMaximum := *caps
	wrongMaximum.MaxProtocolVersion = entity.ProtocolVersion - 1
	if helperCompatible(&wrongMaximum) {
		t.Fatal("helper with an incompatible maximum protocol was accepted")
	}
}

func TestCycleOperationsStillRequireTheDedicatedCapability(t *testing.T) {
	caps := &entity.Capabilities{
		HelperVersion:      beszel.PlusVersion,
		MinAgentVersion:    "0.3.0",
		ProtocolVersion:    entity.ProtocolVersion,
		MaxProtocolVersion: entity.ProtocolVersion,
		UpdateManagement:   true,
		UpdateCycle:        false,
	}
	called := false
	m := &maintenanceManager{enabled: true, manageUpdatesEnabled: true, caps: caps, agent: &Agent{}, replays: map[string]entity.Response{}}
	m.callFunc = func(context.Context, entity.Request) (entity.Response, error) {
		called = true
		return entity.Response{}, nil
	}
	req := entity.Request{
		Version:        entity.ProtocolVersion,
		RequestID:      "cycle-request-123",
		Operation:      entity.RunUnattendedUpgrades,
		IdempotencyKey: "cycle-attempt-123",
		CycleID:        "cycle-00000000000000000001",
		Source:         entity.CycleSourceManual,
		PolicyRevision: strings.Repeat("a", 64),
	}
	response := m.handle(req)
	if response.Status != entity.StateFailed || response.ErrorCode != "update_cycle_unsupported" || called {
		t.Fatalf("cycle without dedicated capability was not refused before IPC: response=%#v called=%v", response, called)
	}
}

func serveMaintenanceOnce(t *testing.T, socket string, response entity.Response) {
	t.Helper()
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Skipf("unix sockets unavailable: %v", err)
	}
	go func() {
		defer listener.Close()
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		var request entity.Request
		_ = json.NewDecoder(conn).Decode(&request)
		response.RequestID, response.Operation = request.RequestID, request.Operation
		_ = json.NewEncoder(conn).Encode(response)
	}()
}

func TestMaintenanceManagerUsesTypedUnixIPC(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "maintenance.sock")
	serveMaintenanceOnce(t, socket, entity.Response{Version: entity.ProtocolVersion, Status: entity.StateCompleted, Result: &entity.Result{Capabilities: &entity.Capabilities{UpdateManagement: true, PrivilegedHelper: true}}})
	m := &maintenanceManager{socket: socket, enabled: true, replays: map[string]entity.Response{}, agent: &Agent{}}
	request := entity.Request{Version: entity.ProtocolVersion, RequestID: "request-123", Operation: entity.GetCapabilities}
	response := m.handle(request)
	if response.Status != entity.StateCompleted || response.Result == nil || !response.Result.Capabilities.PrivilegedHelper {
		t.Fatalf("response=%#v", response)
	}
}

func TestMaintenanceManagerProbesWOLViaTypedIPC(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "maintenance.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Skipf("unix sockets unavailable: %v", err)
	}
	defer listener.Close()
	done := make(chan error, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			done <- acceptErr
			return
		}
		defer conn.Close()
		var request entity.Request
		if decodeErr := json.NewDecoder(conn).Decode(&request); decodeErr != nil {
			done <- decodeErr
			return
		}
		if request.Operation != entity.ProbeWOL || len(request.Interfaces) != 1 || request.Interfaces[0] != "eno1" {
			done <- fmt.Errorf("unexpected WOL probe request: %#v", request)
			return
		}
		response := entity.Response{Version: entity.ProtocolVersion, RequestID: request.RequestID, Operation: request.Operation, Status: entity.StateCompleted, Result: &entity.Result{PowerInterfaces: []powerentity.InterfaceDiagnostic{{Interface: "eno1", Physical: true, Type: "ethernet", WOLSupported: true, WOLEnabled: true}}}}
		done <- json.NewEncoder(conn).Encode(response)
	}()
	m := &maintenanceManager{socket: socket, enabled: true, agent: &Agent{}}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	items, err := m.probeWOL(ctx, []string{"eno1"})
	if err != nil || len(items) != 1 || !items[0].WOLEnabled {
		t.Fatalf("items=%#v err=%v", items, err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestMaintenanceManagerRejectsInvalidAndConcurrentRequests(t *testing.T) {
	m := &maintenanceManager{enabled: true, replays: map[string]entity.Response{}, running: true, last: entity.Response{Status: entity.StateRunning}, agent: &Agent{}}
	invalid := entity.Request{Version: entity.ProtocolVersion, RequestID: "request-123", Operation: entity.Operation("shell")}
	if response := m.handle(invalid); response.Status != entity.StateFailed {
		t.Fatal("unknown operation accepted")
	}
	policy := entity.DefaultPolicy()
	request := entity.Request{Version: entity.ProtocolVersion, RequestID: "request-456", Operation: entity.ApplyUpdatePolicy, Policy: &policy, IdempotencyKey: "concurrent-test"}
	if response := m.handle(request); response.Status != entity.StateFailed {
		t.Fatal("concurrent operation accepted")
	}
}

func TestMaintenanceManagerDoesNotAdvertiseDisabledManagement(t *testing.T) {
	m := &maintenanceManager{enabled: false, replays: map[string]entity.Response{}, caps: &entity.Capabilities{UpdateMonitoring: true}, agent: &Agent{}}
	request := entity.Request{Version: entity.ProtocolVersion, RequestID: "request-disabled", Operation: entity.GetCapabilities}
	response := m.handle(request)
	if response.Status != entity.StateCompleted || response.Result == nil || response.Result.Capabilities == nil || response.Result.Capabilities.UpdateManagement {
		t.Fatalf("disabled management was advertised: %#v", response)
	}
}

func TestMaintenanceHandlerRejectsUnverifiedHub(t *testing.T) {
	request := entity.Request{Version: entity.ProtocolVersion, RequestID: "request-auth", Operation: entity.GetCapabilities}
	data, err := cbor.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	registry := NewHandlerRegistry()
	err = registry.Handle(&HandlerContext{
		Agent:       &Agent{},
		HubVerified: false,
		Request:     &common.HubRequest[cbor.RawMessage]{Action: common.MaintenanceRequest, Data: data},
	})
	if err == nil || !strings.Contains(err.Error(), "hub not verified") {
		t.Fatalf("unverified Hub accepted: %v", err)
	}
}

func TestMaintenanceManagerReturnsIdempotentReplay(t *testing.T) {
	request := entity.Request{Version: entity.ProtocolVersion, RequestID: "request-replay", Operation: entity.RunUpdateDryRun, IdempotencyKey: "replay-key"}
	cached := entity.Response{Version: entity.ProtocolVersion, RequestID: request.RequestID, Operation: request.Operation, Status: entity.StateQueued, IdempotencyKey: request.IdempotencyKey}
	m := &maintenanceManager{enabled: true, manageUpdatesEnabled: true, replays: map[string]entity.Response{request.IdempotencyKey: cached}, agent: &Agent{}}
	if got := m.handle(request); got.Status != cached.Status || got.RequestID != cached.RequestID {
		t.Fatalf("replay was not idempotent: %#v", got)
	}
}

func TestCycleSchedulerRetriesDueCycleAndPersistsSnapshotWithoutUpdateMonitor(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "update-cycle-state.json")
	policy := entity.DefaultPolicy()
	revision := strings.Repeat("a", 64)
	due := time.Now().Add(-time.Minute).UTC()
	current := &entity.CycleStatus{CycleID: "cycle-00000000000000000003", Source: entity.CycleSourceAutomatic, PolicyRevision: revision, State: entity.CycleRetryWait, Attempt: 1, NextAttemptAt: &due}
	caps := &entity.Capabilities{HelperVersion: beszel.PlusVersion, ProtocolVersion: entity.ProtocolVersion, MaxProtocolVersion: entity.ProtocolVersion, UpdateManagement: true, UpdateCycle: true}
	runRequest := make(chan entity.Request, 1)
	m := &maintenanceManager{enabled: true, manageUpdatesEnabled: true, replays: map[string]entity.Response{}, caps: caps, cycleStatePath: statePath, cycleAdoptionChecked: true, agent: &Agent{updateManager: nil}}
	m.callFunc = func(_ context.Context, req entity.Request) (entity.Response, error) {
		switch req.Operation {
		case entity.GetUpdateCycleStatus:
			return entity.Response{Version: entity.ProtocolVersion, RequestID: req.RequestID, Operation: req.Operation, Status: entity.StateCompleted, Result: &entity.Result{UpdateCycle: cloneAgentCycleStatus(current), Policy: &policy, NextCycleID: "cycle-00000000000000000004", PolicyRevision: revision}}, nil
		case entity.GetPoweroffStatus:
			return entity.Response{Version: entity.ProtocolVersion, RequestID: req.RequestID, Operation: req.Operation, Status: entity.StateCompleted, Result: &entity.Result{PoweroffStatus: &powerentity.ShutdownStatus{}}}, nil
		case entity.RunUnattendedUpgrades:
			runRequest <- req
			completed := cloneAgentCycleStatus(current)
			completed.State = entity.CycleCompleted
			completed.Attempt = 2
			return entity.Response{Version: entity.ProtocolVersion, RequestID: req.RequestID, Operation: req.Operation, Status: entity.StateCompleted, Result: &entity.Result{UpdateCycle: completed}}, nil
		case entity.GetCapabilities:
			return entity.Response{Version: entity.ProtocolVersion, RequestID: req.RequestID, Operation: req.Operation, Status: entity.StateCompleted, Result: &entity.Result{Capabilities: caps}}, nil
		case entity.GetOperationStatus:
			return entity.Response{Version: entity.ProtocolVersion, RequestID: req.RequestID, Operation: req.Operation, Status: entity.StateCompleted}, nil
		default:
			return entity.Response{Version: entity.ProtocolVersion, RequestID: req.RequestID, Operation: req.Operation, Status: entity.StateCompleted}, nil
		}
	}
	m.cycleScheduleStep()
	select {
	case got := <-runRequest:
		if got.CycleID != current.CycleID || got.Source != entity.CycleSourceAutomatic || got.PolicyRevision != revision || got.IdempotencyKey == got.CycleID {
			t.Fatalf("scheduler did not retry the due cycle with a fresh attempt key: %#v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("scheduler did not launch the due retry")
	}
	waitForMaintenance(t, func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		return !m.running && m.cycleStatus != nil && m.cycleStatus.State == entity.CycleCompleted
	})
	reloaded := &maintenanceManager{cycleStatePath: statePath}
	reloaded.loadCycleState()
	cycle, _, _, _ := reloaded.cycleSnapshot()
	if cycle == nil || cycle.CycleID != current.CycleID || cycle.State != entity.CycleCompleted || reloaded.agent != nil {
		t.Fatalf("Agent restart did not reload the last cycle snapshot: %#v", cycle)
	}
}

func TestCycleReconciliationRetainsGateAcrossIPCFailureAndRunningState(t *testing.T) {
	previous := cycleReconcileInterval
	cycleReconcileInterval = time.Millisecond
	t.Cleanup(func() { cycleReconcileInterval = previous })
	policyRevision := strings.Repeat("b", 64)
	var mu sync.Mutex
	statusCalls, runCalls := 0, 0
	firstError := make(chan struct{}, 1)
	m := &maintenanceManager{agent: &Agent{}, replays: map[string]entity.Response{}}
	m.callFunc = func(_ context.Context, req entity.Request) (entity.Response, error) {
		mu.Lock()
		defer mu.Unlock()
		switch req.Operation {
		case entity.GetUpdateCycleStatus:
			statusCalls++
			if statusCalls == 1 {
				firstError <- struct{}{}
				return entity.Response{}, errors.New("helper unavailable")
			}
			state := entity.CycleRunning
			if statusCalls >= 3 {
				state = entity.CycleCompleted
			}
			return entity.Response{Version: entity.ProtocolVersion, RequestID: req.RequestID, Operation: req.Operation, Status: entity.StateCompleted, Result: &entity.Result{UpdateCycle: &entity.CycleStatus{CycleID: req.CycleID, Source: entity.CycleSourceAutomatic, PolicyRevision: policyRevision, State: state}}}, nil
		case entity.RunUnattendedUpgrades:
			runCalls++
			return entity.Response{Version: entity.ProtocolVersion, RequestID: req.RequestID, Operation: req.Operation, Status: entity.StateRunning, Result: &entity.Result{UpdateCycle: &entity.CycleStatus{CycleID: req.CycleID, Source: req.Source, PolicyRevision: req.PolicyRevision, State: entity.CycleRunning}}}, nil
		case entity.GetPoweroffStatus:
			return entity.Response{Version: entity.ProtocolVersion, RequestID: req.RequestID, Operation: req.Operation, Status: entity.StateCompleted, Result: &entity.Result{PoweroffStatus: &powerentity.ShutdownStatus{}}}, nil
		default:
			return entity.Response{}, errors.New("unexpected reconciliation request")
		}
	}
	m.retainCycleGate(false, "cycle-00000000000000000009", entity.CycleSourceAutomatic, policyRevision)
	select {
	case <-firstError:
	case <-time.After(time.Second):
		t.Fatal("reconciliation did not query helper status")
	}
	m.mu.Lock()
	heldAfterIPCError := m.cycleGateHeld
	m.mu.Unlock()
	if !heldAfterIPCError {
		t.Fatal("collector gate was released after helper IPC failure")
	}
	waitForMaintenance(t, func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		return !m.cycleGateHeld && !m.cycleReconciliation
	})
	mu.Lock()
	defer mu.Unlock()
	if statusCalls < 3 || runCalls != 1 {
		t.Fatalf("running orphan was not reconciled before gate release: status calls=%d run calls=%d", statusCalls, runCalls)
	}
}

func waitForMaintenance(t *testing.T, predicate func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if predicate() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("maintenance state did not converge before deadline")
}

func TestMaintenanceIPCContextTimeout(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "maintenance.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Skipf("unix sockets unavailable: %v", err)
	}
	defer listener.Close()
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			defer conn.Close()
			time.Sleep(100 * time.Millisecond)
		}
	}()
	m := &maintenanceManager{socket: socket, enabled: true, agent: &Agent{}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	_, err = m.call(ctx, entity.Request{Version: entity.ProtocolVersion, RequestID: "request-789", Operation: entity.GetCapabilities})
	if err == nil {
		t.Fatal("expected timeout")
	}
}
