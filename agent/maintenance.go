package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/henrygd/beszel"
	"github.com/henrygd/beszel/agent/utils"
	entity "github.com/henrygd/beszel/internal/entities/maintenance"
	powerentity "github.com/henrygd/beszel/internal/entities/power"
	updateentity "github.com/henrygd/beszel/internal/entities/update"
)

const maintenanceSocket = "/run/beszel-maintenance.sock"

var cycleReconcileInterval = 15 * time.Second

// SmokeMaintenance performs a complete single-request Unix socket exchange.
func SmokeMaintenance(ctx context.Context, socket string) error {
	if socket == "" {
		socket = maintenanceSocket
	}
	manager := &maintenanceManager{socket: socket}
	response, err := manager.call(ctx, entity.Request{Version: entity.ProtocolVersion, RequestID: "installer-smoke", Operation: entity.GetCapabilities})
	if err != nil {
		return err
	}
	if response.Status != entity.StateCompleted || response.Result == nil || !helperCompatible(response.Result.Capabilities) {
		return errors.New("Maintenance helper version is incompatible with this Agent.")
	}
	return nil
}

type maintenanceManager struct {
	mu                   sync.Mutex
	cycleStateMu         sync.Mutex
	agent                *Agent
	socket               string
	enabled              bool
	manageUpdatesEnabled bool
	running              bool
	last                 entity.Response
	replays              map[string]entity.Response
	caps                 *entity.Capabilities
	cycleStatePath       string
	cycleStatus          *entity.CycleStatus
	cyclePolicy          *entity.Policy
	nextCycleID          string
	cyclePolicyRevision  string
	cycleAdoptionChecked bool
	cycleAdoptionRunning bool
	cycleGateHeld        bool
	cycleGateToken       bool
	cycleReconciliation  bool
	callFunc             func(context.Context, entity.Request) (entity.Response, error)
}

type agentCycleState struct {
	Cycle          *entity.CycleStatus `json:"cycle,omitempty"`
	Policy         *entity.Policy      `json:"policy,omitempty"`
	NextCycleID    string              `json:"next_cycle_id,omitempty"`
	PolicyRevision string              `json:"policy_revision,omitempty"`
}

func newMaintenanceManager(agent *Agent) *maintenanceManager {
	raw, _ := utils.GetEnv("OS_UPDATE_MANAGEMENT")
	powerRaw, _ := utils.GetEnv("POWER_MANAGEMENT")
	manageUpdates := strings.EqualFold(strings.TrimSpace(raw), "true")
	m := &maintenanceManager{agent: agent, socket: maintenanceSocket, enabled: manageUpdates || strings.EqualFold(powerRaw, "true"), manageUpdatesEnabled: manageUpdates, replays: make(map[string]entity.Response)}
	if agent.dataDir != "" {
		m.cycleStatePath = filepath.Join(agent.dataDir, "update-cycle-state.json")
		m.loadCycleState()
	}
	m.caps = &entity.Capabilities{UpdateMonitoring: agent.updateManager != nil}
	if agent.updateManager != nil {
		agent.updateManager.setEligibilityCheck(func(ctx context.Context) eligibilityCheckResult {
			now := time.Now().UTC().Format("20060102T150405.000000000")
			response, err := m.call(ctx, entity.Request{Version: entity.ProtocolVersion, RequestID: "eligibility-" + now, Operation: entity.RunUpdateDryRun})
			if err != nil {
				return eligibilityCheckResult{status: "unavailable", errorCode: "helper_unavailable", retryable: true}
			}
			if response.Status != entity.StateCompleted || response.Result == nil {
				code := response.ErrorCode
				if code == "" {
					code = "helper_dry_run_failed"
				}
				status := "unavailable"
				if code == "apt_lock_busy" {
					status = "busy"
				}
				return eligibilityCheckResult{status: status, errorCode: code, retryable: response.Retryable}
			}
			return eligibilityCheckResult{output: []byte(response.Result.Output), status: "available"}
		})
	}
	go m.runCapabilityRefresh()
	if manageUpdates {
		go m.runCycleScheduler()
	}
	return m
}

func (m *maintenanceManager) runCapabilityRefresh() {
	m.refreshCapabilities()
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		m.refreshCapabilities()
	}
}

func (m *maintenanceManager) capabilities() *entity.Capabilities {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.caps == nil {
		return &entity.Capabilities{}
	}
	copy := *m.caps
	return &copy
}

func (m *maintenanceManager) refreshCapabilities() {
	base := &entity.Capabilities{UpdateMonitoring: m.agent.updateManager != nil}
	if !m.enabled {
		m.mu.Lock()
		m.caps = base
		m.mu.Unlock()
		return
	}
	req := entity.Request{Version: entity.ProtocolVersion, RequestID: "capabilities", Operation: entity.GetCapabilities}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	response, err := m.call(ctx, req)
	if err != nil || response.Result == nil || response.Result.Capabilities == nil {
		m.mu.Lock()
		m.caps = base
		m.mu.Unlock()
		return
	}
	response.Result.Capabilities.UpdateMonitoring = base.UpdateMonitoring
	if !helperCompatible(response.Result.Capabilities) {
		response.Result.Capabilities.UpdateManagement = false
		response.Result.Capabilities.PolicyWrite = false
		response.Result.Capabilities.RunUpgrade = false
		response.Result.Capabilities.UpdateCycle = false
	}
	if !response.Result.Capabilities.UpdateCycle {
		response.Result.Capabilities.UpdateManagement = false
		response.Result.Capabilities.PolicyRead = false
		response.Result.Capabilities.PolicyWrite = false
		response.Result.Capabilities.DryRun = false
		response.Result.Capabilities.RunUpgrade = false
	}
	if !m.manageUpdatesEnabled {
		response.Result.Capabilities.UpdateManagement = false
		response.Result.Capabilities.PolicyRead = false
		response.Result.Capabilities.PolicyWrite = false
		response.Result.Capabilities.DryRun = false
		response.Result.Capabilities.RunUpgrade = false
		response.Result.Capabilities.UpdateCycle = false
	}
	m.mu.Lock()
	m.caps = response.Result.Capabilities
	m.mu.Unlock()
	go func() {
		m.ensureLegacyPolicyAdopted()
		m.retryPersistedPolicy()
	}()
}

func (m *maintenanceManager) probeWOL(ctx context.Context, interfaces []string) ([]powerentity.InterfaceDiagnostic, error) {
	if !m.enabled {
		return nil, errors.New("privileged helper unavailable")
	}
	requestID := "wol-probe-" + time.Now().UTC().Format("20060102T150405.000000000")
	response, err := m.call(ctx, entity.Request{Version: entity.ProtocolVersion, RequestID: requestID, Operation: entity.ProbeWOL, Interfaces: interfaces})
	if err != nil {
		return nil, err
	}
	if response.Status != entity.StateCompleted || response.Result == nil {
		if response.Error != "" {
			return nil, errors.New(response.Error)
		}
		return nil, errors.New("privileged WOL probe failed")
	}
	return response.Result.PowerInterfaces, nil
}

func (m *maintenanceManager) retryPersistedPolicy() {
	if upgradeDrainActive() {
		slog.Info("pending policy retry suspended: Agent upgrade in progress")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req := entity.Request{Version: entity.ProtocolVersion, RequestID: "pending-policy-status", Operation: entity.GetOperationStatus}
	response, err := m.call(ctx, req)
	if err != nil || response.Status != entity.StateFailed || !response.Retryable || response.Result == nil || response.Result.Policy == nil {
		return
	}
	if response.NextAttemptAt != nil && time.Now().Before(*response.NextAttemptAt) {
		return
	}
	// The helper persists the bounded retry schedule. Capability refreshes must
	// not create an independent retry loop or log spam.
	now := time.Now().UTC().Format("20060102T150405.000000000")
	retry := entity.Request{Version: entity.ProtocolVersion, RequestID: "policy-retry-" + now, Operation: entity.ApplyUpdatePolicy, IdempotencyKey: "policy-retry-" + now, Policy: response.Result.Policy, ExpectedPolicyRevision: response.Result.ExpectedPolicyRevision}
	m.handle(retry)
}

func (m *maintenanceManager) ensureLegacyPolicyAdopted() {
	if !m.manageUpdatesEnabled || !helperCompatible(m.capabilities()) || !m.capabilities().UpdateManagement || !m.capabilities().UpdateCycle {
		return
	}
	m.mu.Lock()
	if m.cycleAdoptionChecked || m.cycleAdoptionRunning || m.running {
		m.mu.Unlock()
		return
	}
	m.cycleAdoptionRunning = true
	m.running = true
	m.mu.Unlock()

	var gateHeld bool
	if m.agent.updateManager != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := m.agent.updateManager.beginMaintenance(ctx)
		cancel()
		if err != nil {
			m.finishPolicyAdoption(false, entity.Response{})
			return
		}
		gateHeld = true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	attempt := time.Now().UTC().Format("20060102T150405.000000000")
	request := entity.Request{Version: entity.ProtocolVersion, RequestID: "policy-adoption-" + attempt, Operation: entity.AdoptUpdatePolicy, IdempotencyKey: "policy-adoption-" + attempt}
	response, err := m.call(ctx, request)
	cancel()
	if gateHeld {
		m.agent.updateManager.endMaintenance()
	}
	if err != nil {
		m.finishPolicyAdoption(false, entity.Response{})
		return
	}
	m.finishPolicyAdoption(response.Status == entity.StateCompleted, response)
}

func (m *maintenanceManager) finishPolicyAdoption(checked bool, response entity.Response) {
	m.mu.Lock()
	m.cycleAdoptionRunning = false
	m.running = false
	if response.Status != "" {
		m.last = response
	}
	if checked {
		m.cycleAdoptionChecked = true
	}
	m.mu.Unlock()
}

func (m *maintenanceManager) handle(req entity.Request) entity.Response {
	if err := entity.ValidateRequest(req); err != nil {
		return maintenanceFailure(req, err.Error())
	}
	if upgradeDrainActive() {
		response := maintenanceFailure(req, "Agent upgrade is in progress; maintenance is temporarily unavailable")
		response.ErrorCode = "upgrade_in_progress"
		response.Stage = "agent_drain"
		response.Retryable = true
		return response
	}
	if !m.enabled {
		if req.Operation == entity.GetCapabilities {
			return entity.Response{
				Version: req.Version, RequestID: req.RequestID, Operation: req.Operation,
				Status: entity.StateCompleted, Progress: 100,
				Result: &entity.Result{Capabilities: m.capabilities()},
			}
		}
		return maintenanceFailure(req, "update management disabled")
	}
	if maintenanceUpdateOperation(req.Operation) && !m.manageUpdatesEnabled {
		return maintenanceFailure(req, "OS update management disabled")
	}
	if maintenanceWriteOperation(req.Operation) && !helperCompatible(m.capabilities()) {
		response := maintenanceFailure(req, "Maintenance helper version is incompatible with this Agent.")
		response.ErrorCode = "helper_incompatible"
		response.Stage = "capability_negotiation"
		return response
	}
	if (req.Operation == entity.RunUnattendedUpgrades || req.Operation == entity.GetUpdateCycleStatus || req.Operation == entity.AdoptUpdatePolicy) && !m.capabilities().UpdateCycle {
		response := maintenanceFailure(req, "Maintenance helper does not support update cycles")
		response.ErrorCode = "update_cycle_unsupported"
		return response
	}
	m.mu.Lock()
	if req.IdempotencyKey != "" {
		if cached, ok := m.replays[req.IdempotencyKey]; ok {
			m.mu.Unlock()
			if cached.Operation != req.Operation {
				return maintenanceFailure(req, "idempotency key reused for another operation")
			}
			return cached
		}
	}
	if entity.IsLongOperation(req.Operation) {
		if m.running {
			m.mu.Unlock()
			return maintenanceFailure(req, "another maintenance operation is running")
		}
		if req.Operation == entity.RunUnattendedUpgrades && m.cycleGateHeld {
			m.mu.Unlock()
			response := maintenanceFailure(req, "an update cycle is still being reconciled")
			response.ErrorCode = "cycle_reconciliation_active"
			response.Retryable = true
			return response
		}
		now := time.Now().UTC()
		queued := entity.Response{Version: entity.ProtocolVersion, RequestID: req.RequestID, Operation: req.Operation, Status: entity.StateQueued, Progress: 0, IdempotencyKey: req.IdempotencyKey, StartedAt: &now}
		m.running, m.last = true, queued
		if req.IdempotencyKey != "" {
			m.replays[req.IdempotencyKey] = queued
		}
		m.mu.Unlock()
		go m.run(req)
		return queued
	}
	m.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	response, err := m.call(ctx, req)
	if err != nil {
		if req.Operation == entity.GetOperationStatus {
			m.mu.Lock()
			fallback := m.last
			m.mu.Unlock()
			if fallback.Status != "" {
				return fallback
			}
		}
		return maintenanceFailure(req, err.Error())
	}
	return response
}

func (m *maintenanceManager) run(req entity.Request) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Hour)
	defer cancel()
	gateHeld := false
	if m.agent.updateManager != nil {
		gateCtx, gateCancel := context.WithTimeout(ctx, 10*time.Second)
		if err := m.agent.updateManager.beginMaintenance(gateCtx); err != nil {
			gateCancel()
			response := maintenanceFailure(req, "update collection is still running")
			response.ErrorCode = "update_collection_busy"
			response.Stage = "agent_coordination"
			response.Retryable = true
			m.mu.Lock()
			m.running = false
			m.last = response
			if req.IdempotencyKey != "" {
				m.replays[req.IdempotencyKey] = response
			}
			m.mu.Unlock()
			return
		}
		gateCancel()
		gateHeld = true
	}
	response, err := m.call(ctx, req)
	callFailed := err != nil
	if err != nil {
		response = maintenanceFailure(req, err.Error())
	}
	cycleStatus := responseCycleStatus(response)
	cycleUnresolved := req.Operation == entity.RunUnattendedUpgrades && (callFailed || (cycleStatus != nil && cycleStatus.State == entity.CycleRunning))
	if cycleStatus != nil {
		m.storeCycleSnapshot(cycleStatus, nil, "", cycleStatus.PolicyRevision)
	}
	m.mu.Lock()
	m.running = false
	m.last = response
	if req.IdempotencyKey != "" {
		m.replays[req.IdempotencyKey] = response
	}
	if len(m.replays) > 128 {
		m.replays = map[string]entity.Response{req.IdempotencyKey: response}
	}
	m.mu.Unlock()
	if cycleUnresolved {
		m.retainCycleGate(gateHeld, req.CycleID, req.Source, req.PolicyRevision)
		gateHeld = false
	}
	if gateHeld && m.agent.updateManager != nil {
		m.agent.updateManager.endMaintenance()
	}
	if req.Operation == entity.RunUnattendedUpgrades && response.Status == entity.StateCompleted && cycleStatus != nil && (cycleStatus.State == entity.CycleCompleted || cycleStatus.State == entity.CycleCompletedWithPending) && cycleStatus.Source == entity.CycleSourceManual && m.agent.updateManager != nil {
		finished := time.Now().UTC()
		if response.FinishedAt != nil {
			finished = *response.FinishedAt
		}
		m.agent.updateManager.markManualUpgrade(finished)
	}
	if m.agent.updateManager != nil {
		go m.agent.updateManager.refresh()
	}
	go m.refreshCapabilities()
	if req.Operation == entity.ApplyUpdatePolicy && response.Status == entity.StateFailed && response.Retryable {
		delay := 5 * time.Minute
		if response.NextAttemptAt != nil {
			delay = time.Until(*response.NextAttemptAt)
			if delay < 0 {
				delay = 0
			}
		}
		time.AfterFunc(delay, m.retryPersistedPolicy)
	}
}

func responseCycleStatus(response entity.Response) *entity.CycleStatus {
	if response.Result == nil || response.Result.UpdateCycle == nil {
		return nil
	}
	return cloneAgentCycleStatus(response.Result.UpdateCycle)
}

func terminalCycleState(state entity.CycleState) bool {
	return state == entity.CycleCompleted || state == entity.CycleCompletedWithPending || state == entity.CycleFailed || state == entity.CycleCanceled
}

func cloneAgentCycleStatus(status *entity.CycleStatus) *entity.CycleStatus {
	if status == nil {
		return nil
	}
	copy := *status
	if status.Counts != nil {
		counts := *status.Counts
		counts.Packages = append([]entity.CyclePackage(nil), status.Counts.Packages...)
		copy.Counts = &counts
	}
	if status.InitialCounts != nil {
		counts := *status.InitialCounts
		counts.Packages = append([]entity.CyclePackage(nil), status.InitialCounts.Packages...)
		copy.InitialCounts = &counts
	}
	return &copy
}

func (m *maintenanceManager) loadCycleState() {
	if m.cycleStatePath == "" {
		return
	}
	data, err := os.ReadFile(m.cycleStatePath)
	if err != nil {
		return
	}
	var state agentCycleState
	if json.Unmarshal(data, &state) != nil {
		return
	}
	m.cycleStatus = cloneAgentCycleStatus(state.Cycle)
	if state.Policy != nil {
		policy := *state.Policy
		policy.AllowedRepositories = append([]string(nil), state.Policy.AllowedRepositories...)
		m.cyclePolicy = &policy
	}
	m.nextCycleID = state.NextCycleID
	m.cyclePolicyRevision = state.PolicyRevision
}

func (m *maintenanceManager) storeCycleSnapshot(cycle *entity.CycleStatus, policy *entity.Policy, nextCycleID, revision string) {
	m.cycleStateMu.Lock()
	defer m.cycleStateMu.Unlock()
	m.mu.Lock()
	if cycle != nil && !olderAgentCycleSnapshot(cycle, m.cycleStatus) {
		m.cycleStatus = cloneAgentCycleStatus(cycle)
	}
	if policy != nil {
		copy := *policy
		copy.AllowedRepositories = append([]string(nil), policy.AllowedRepositories...)
		m.cyclePolicy = &copy
	}
	if nextCycleID != "" {
		m.nextCycleID = nextCycleID
	}
	if revision != "" {
		m.cyclePolicyRevision = revision
	}
	state := agentCycleState{Cycle: cloneAgentCycleStatus(m.cycleStatus), NextCycleID: m.nextCycleID, PolicyRevision: m.cyclePolicyRevision}
	if m.cyclePolicy != nil {
		policyCopy := *m.cyclePolicy
		policyCopy.AllowedRepositories = append([]string(nil), m.cyclePolicy.AllowedRepositories...)
		state.Policy = &policyCopy
	}
	path := m.cycleStatePath
	m.mu.Unlock()
	if path == "" {
		return
	}
	data, err := json.Marshal(state)
	if err != nil {
		slog.Warn("could not serialize update cycle snapshot", "error", err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		slog.Warn("could not create update cycle state directory", "error", err)
		return
	}
	temp := path + ".tmp"
	if err := os.WriteFile(temp, append(data, '\n'), 0o600); err != nil {
		slog.Warn("could not persist update cycle snapshot", "error", err)
		return
	}
	if err := os.Rename(temp, path); err != nil {
		_ = os.Remove(temp)
		slog.Warn("could not replace update cycle snapshot", "error", err)
	}
}

func olderAgentCycleSnapshot(incoming, current *entity.CycleStatus) bool {
	if current == nil {
		return false
	}
	if incoming.CycleID != current.CycleID {
		return incoming.CycleID < current.CycleID
	}
	return current.UpdatedAt != nil && (incoming.UpdatedAt == nil || incoming.UpdatedAt.Before(*current.UpdatedAt))
}

func (m *maintenanceManager) cycleSnapshot() (cycle *entity.CycleStatus, policy *entity.Policy, nextCycleID, revision string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cycle = cloneAgentCycleStatus(m.cycleStatus)
	if m.cyclePolicy != nil {
		copy := *m.cyclePolicy
		copy.AllowedRepositories = append([]string(nil), m.cyclePolicy.AllowedRepositories...)
		policy = &copy
	}
	return cycle, policy, m.nextCycleID, m.cyclePolicyRevision
}

func (m *maintenanceManager) attachCycleSnapshot(status *updateentity.Status) {
	if status == nil {
		return
	}
	cycle, policy, _, _ := m.cycleSnapshot()
	status.Cycle = cycle
	status.UpdatePolicy = policy
	status.Capabilities = m.capabilities()
	status.CycleManagementEnabled = m.manageUpdatesEnabled && status.Capabilities != nil && status.Capabilities.UpdateCycle
}

func (m *maintenanceManager) runCycleScheduler() {
	m.cycleScheduleStep()
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		m.cycleScheduleStep()
	}
}

func (m *maintenanceManager) cycleScheduleStep() {
	if !m.manageUpdatesEnabled || upgradeDrainActive() {
		return
	}
	m.mu.Lock()
	ready := m.cycleAdoptionChecked
	blocked := m.running || m.cycleGateHeld || m.cycleReconciliation
	m.mu.Unlock()
	if !ready {
		return
	}
	caps := m.capabilities()
	if !helperCompatible(caps) || !caps.UpdateManagement || !caps.UpdateCycle {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	query := entity.Request{Version: entity.ProtocolVersion, RequestID: "cycle-snapshot-" + time.Now().UTC().Format("150405.000000000"), Operation: entity.GetUpdateCycleStatus}
	response, err := m.call(ctx, query)
	if err != nil || response.Status != entity.StateCompleted || response.Result == nil || response.Result.Policy == nil {
		return
	}
	cycle := cloneAgentCycleStatus(response.Result.UpdateCycle)
	policy := response.Result.Policy
	m.storeCycleSnapshot(cycle, policy, response.Result.NextCycleID, response.Result.PolicyRevision)
	// Poll the durable helper state during an active call, without launching a
	// second installation or releasing the maintenance gate.
	if blocked {
		return
	}
	if cycle != nil && !terminalCycleState(cycle.State) {
		if cycle.State == entity.CycleRunning || cycle.State == entity.CycleQueued {
			m.launchCycle(cycle.CycleID, cycle.Source, cycle.PolicyRevision)
		} else if cycle.State == entity.CycleRetryWait && policy.UnattendedUpgradeDays > 0 && cycle.NextAttemptAt != nil && !time.Now().Before(*cycle.NextAttemptAt) {
			if !m.poweroffScheduled(ctx) {
				m.launchCycle(cycle.CycleID, cycle.Source, cycle.PolicyRevision)
			}
		} else if (policy.Mode == entity.ModeMonitorOnly || !policy.Enabled || policy.UnattendedUpgradeDays == 0 || response.Result.PolicyRevision != cycle.PolicyRevision) && cycle.State == entity.CycleRetryWait {
			if !m.poweroffScheduled(ctx) {
				m.launchCycle(cycle.CycleID, cycle.Source, cycle.PolicyRevision)
			}
		}
		return
	}
	if !policy.Enabled || policy.Mode == entity.ModeMonitorOnly || policy.UnattendedUpgradeDays == 0 {
		return
	}
	if cycle != nil && cycle.NextRunAt != nil && time.Now().Before(*cycle.NextRunAt) {
		return
	}
	if response.Result.NextCycleID == "" || response.Result.PolicyRevision == "" || m.poweroffScheduled(ctx) {
		return
	}
	m.launchCycle(response.Result.NextCycleID, entity.CycleSourceAutomatic, response.Result.PolicyRevision)
}

func (m *maintenanceManager) poweroffScheduled(ctx context.Context) bool {
	response, err := m.call(ctx, entity.Request{Version: entity.ProtocolVersion, RequestID: "poweroff-gate-" + time.Now().UTC().Format("150405.000000000"), Operation: entity.GetPoweroffStatus})
	return err != nil || (response.Result != nil && response.Result.PoweroffStatus != nil && response.Result.PoweroffStatus.Scheduled)
}

func (m *maintenanceManager) launchCycle(id string, source entity.CycleSource, revision string) {
	if id == "" || revision == "" || (source != entity.CycleSourceAutomatic && source != entity.CycleSourceManual) {
		return
	}
	now := time.Now().UTC().Format("20060102T150405.000000000")
	req := entity.Request{Version: entity.ProtocolVersion, RequestID: "cycle-" + now, Operation: entity.RunUnattendedUpgrades, IdempotencyKey: "cycle-attempt-" + now, CycleID: id, Source: source, PolicyRevision: revision}
	m.handle(req)
}

func (m *maintenanceManager) retainCycleGate(gateHeld bool, cycleID string, source entity.CycleSource, revision string) {
	m.mu.Lock()
	m.cycleGateHeld = true
	m.cycleGateToken = gateHeld
	if m.cycleReconciliation {
		m.mu.Unlock()
		return
	}
	m.cycleReconciliation = true
	m.mu.Unlock()
	go func() {
		ticker := time.NewTicker(cycleReconcileInterval)
		defer ticker.Stop()
		for range ticker.C {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			statusReq := entity.Request{Version: entity.ProtocolVersion, RequestID: "cycle-reconcile-status-" + time.Now().UTC().Format("150405.000000000"), Operation: entity.GetUpdateCycleStatus, CycleID: cycleID}
			response, err := m.call(ctx, statusReq)
			cycle := responseCycleStatus(response)
			resolvedWithoutCycle := false
			if err == nil && cycle != nil {
				m.storeCycleSnapshot(cycle, nil, "", cycle.PolicyRevision)
			}
			// IPC can fail before the helper claims the ID. Replaying that same
			// typed request is safe under the helper's flock and watermark.
			if err == nil && ((cycle != nil && cycle.State == entity.CycleRunning) || response.ErrorCode == "cycle_expired") {
				if !m.poweroffScheduled(ctx) {
					runReq := entity.Request{Version: entity.ProtocolVersion, RequestID: "cycle-reconcile-run-" + time.Now().UTC().Format("150405.000000000"), Operation: entity.RunUnattendedUpgrades, IdempotencyKey: "cycle-reconcile-" + cycleID, CycleID: cycleID, Source: source, PolicyRevision: revision}
					runResponse, runErr := m.call(ctx, runReq)
					if runErr == nil {
						if runCycle := responseCycleStatus(runResponse); runCycle != nil {
							m.storeCycleSnapshot(runCycle, nil, "", runCycle.PolicyRevision)
							cycle = runCycle
						} else if runResponse.Status == entity.StateFailed {
							switch runResponse.ErrorCode {
							case "cycle_expired", "policy_disabled", "automatic_schedule_disabled", "policy_revision_conflict":
								resolvedWithoutCycle = true
							}
						}
					}
				}
			}
			cancel()
			if !resolvedWithoutCycle && (err != nil || cycle == nil || cycle.State == entity.CycleRunning || cycle.State == entity.CycleQueued) {
				continue
			}
			m.mu.Lock()
			release := m.cycleGateToken
			m.cycleGateToken = false
			m.cycleGateHeld = false
			m.cycleReconciliation = false
			m.mu.Unlock()
			if release && m.agent.updateManager != nil {
				m.agent.updateManager.endMaintenance()
			}
			if m.agent.updateManager != nil {
				go m.agent.updateManager.refresh()
			}
			return
		}
	}()
}

func (m *maintenanceManager) call(ctx context.Context, req entity.Request) (entity.Response, error) {
	if m.callFunc != nil {
		return m.callFunc(ctx, req)
	}
	dialer := net.Dialer{Timeout: 2 * time.Second}
	conn, err := dialer.DialContext(ctx, "unix", m.socket)
	if err != nil {
		return entity.Response{}, errors.New("privileged helper unavailable")
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return entity.Response{}, err
	}
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return entity.Response{}, errors.New("maintenance socket is not a Unix stream")
	}
	// The helper reads exactly one request through EOF. Half-close only the
	// writing side so its complete response remains readable.
	if err := unixConn.CloseWrite(); err != nil {
		return entity.Response{}, err
	}
	var response entity.Response
	decoder := json.NewDecoder(bufio.NewReaderSize(conn, 64*1024))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&response); err != nil {
		return entity.Response{}, err
	}
	if response.RequestID != req.RequestID && req.Operation != entity.GetOperationStatus {
		return entity.Response{}, errors.New("helper response request ID mismatch")
	}
	return response, nil
}

func helperCompatible(caps *entity.Capabilities) bool {
	return caps != nil && caps.HelperVersion == beszel.PlusVersion && caps.ProtocolVersion == entity.ProtocolVersion && caps.MaxProtocolVersion >= entity.ProtocolVersion
}

func maintenanceWriteOperation(operation entity.Operation) bool {
	switch operation {
	case entity.ApplyUpdatePolicy, entity.AdoptUpdatePolicy, entity.InstallUpdateDependencies, entity.RunUnattendedUpgrades, entity.SchedulePoweroff, entity.CancelPoweroff:
		return true
	default:
		return false
	}
}

func maintenanceUpdateOperation(operation entity.Operation) bool {
	switch operation {
	case entity.GetUpdatePolicy, entity.DetectRepositories, entity.ValidateUpdatePolicy, entity.ApplyUpdatePolicy, entity.AdoptUpdatePolicy, entity.InstallUpdateDependencies, entity.RunUpdateDryRun, entity.RunUnattendedUpgrades, entity.GetOperationStatus, entity.GetUpdateCycleStatus:
		return true
	default:
		return false
	}
}

func maintenanceFailure(req entity.Request, message string) entity.Response {
	now := time.Now().UTC()
	return entity.Response{Version: entity.ProtocolVersion, RequestID: req.RequestID, Operation: req.Operation, Status: entity.StateFailed, Error: sanitizeUpdateText(message, 512), IdempotencyKey: req.IdempotencyKey, FinishedAt: &now}
}
