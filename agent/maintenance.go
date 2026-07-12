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

	"github.com/henrygd/beszel/agent/utils"
	entity "github.com/henrygd/beszel/internal/entities/maintenance"
)

const maintenanceSocket = "/run/beszel-maintenance.sock"

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
	m.mu.Lock()
	m.caps = response.Result.Capabilities
	m.mu.Unlock()
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

func maintenanceFailure(req entity.Request, message string) entity.Response {
	now := time.Now().UTC()
	return entity.Response{Version: entity.ProtocolVersion, RequestID: req.RequestID, Operation: req.Operation, Status: entity.StateFailed, Error: sanitizeUpdateText(message, 512), IdempotencyKey: req.IdempotencyKey, FinishedAt: &now}
}
