package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/henrygd/beszel"
	"github.com/henrygd/beszel/agent/utils"
	entity "github.com/henrygd/beszel/internal/entities/maintenance"
)

const maintenanceSocket = "/run/beszel-maintenance.sock"

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
	mu      sync.Mutex
	agent   *Agent
	socket  string
	enabled bool
	running bool
	last    entity.Response
	replays map[string]entity.Response
	caps    *entity.Capabilities
}

func newMaintenanceManager(agent *Agent) *maintenanceManager {
	raw, _ := utils.GetEnv("OS_UPDATE_MANAGEMENT")
	m := &maintenanceManager{agent: agent, socket: maintenanceSocket, enabled: strings.EqualFold(raw, "true"), replays: make(map[string]entity.Response)}
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
	}
	m.mu.Lock()
	m.caps = response.Result.Capabilities
	m.mu.Unlock()
	go m.retryPersistedPolicy()
}

func (m *maintenanceManager) retryPersistedPolicy() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req := entity.Request{Version: entity.ProtocolVersion, RequestID: "pending-policy-status", Operation: entity.GetOperationStatus}
	response, err := m.call(ctx, req)
	if err != nil || response.Status != entity.StateFailed || !response.Retryable || response.Result == nil || response.Result.Policy == nil {
		return
	}
	now := time.Now().UTC().Format("20060102T150405.000000000")
	retry := entity.Request{Version: entity.ProtocolVersion, RequestID: "policy-retry-" + now, Operation: entity.ApplyUpdatePolicy, IdempotencyKey: "policy-retry-" + now, Policy: response.Result.Policy}
	m.handle(retry)
}

func (m *maintenanceManager) handle(req entity.Request) entity.Response {
	if err := entity.ValidateRequest(req); err != nil {
		return maintenanceFailure(req, err.Error())
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
	if maintenanceWriteOperation(req.Operation) && !helperCompatible(m.capabilities()) {
		response := maintenanceFailure(req, "Maintenance helper version is incompatible with this Agent.")
		response.ErrorCode = "helper_incompatible"
		response.Stage = "capability_negotiation"
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
		defer m.agent.updateManager.endMaintenance()
	}
	response, err := m.call(ctx, req)
	if err != nil {
		response = maintenanceFailure(req, err.Error())
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
	if req.Operation == entity.RunUnattendedUpgrades && response.Status == entity.StateCompleted && m.agent.updateManager != nil {
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
		time.AfterFunc(5*time.Minute, func() {
			retry := req
			suffix := time.Now().UTC().Format("20060102T150405.000000000")
			retry.RequestID = "policy-retry-" + suffix
			retry.IdempotencyKey = retry.RequestID
			m.handle(retry)
		})
	}
}

func (m *maintenanceManager) call(ctx context.Context, req entity.Request) (entity.Response, error) {
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
	case entity.ApplyUpdatePolicy, entity.InstallUpdateDependencies, entity.RunUnattendedUpgrades:
		return true
	default:
		return false
	}
}

func maintenanceFailure(req entity.Request, message string) entity.Response {
	now := time.Now().UTC()
	return entity.Response{Version: entity.ProtocolVersion, RequestID: req.RequestID, Operation: req.Operation, Status: entity.StateFailed, Error: sanitizeUpdateText(message, 512), IdempotencyKey: req.IdempotencyKey, FinishedAt: &now}
}
