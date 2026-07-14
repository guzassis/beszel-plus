package hub

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	maintenanceentity "github.com/henrygd/beszel/internal/entities/maintenance"
	powerentity "github.com/henrygd/beszel/internal/entities/power"
	powerexec "github.com/henrygd/beszel/internal/power"
	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
)

type powerAPIRequest struct {
	SystemID     string `json:"system_id"`
	Action       string `json:"action"`
	DelaySeconds uint32 `json:"delay_seconds,omitempty"`
}

func (h *Hub) handlePowerNetworks(e *core.RequestEvent) error {
	var networks []powerexec.Network
	records, _ := h.FindAllRecords("power_networks")
	for _, record := range records {
		networks = append(networks, powerexec.Network{ID: record.Id, Name: record.GetString("name"), Interface: record.GetString("interface"), Source: record.GetString("source"), Prefix: record.GetString("prefix"), Broadcast: record.GetString("broadcast"), Enabled: record.GetBool("enabled")})
	}
	return e.JSON(http.StatusOK, map[string]any{"networks": networks})
}

func (h *Hub) syncPowerNetworks() error {
	networks, err := powerexec.LocalNetworks()
	if err != nil {
		return err
	}
	collection, err := h.FindCachedCollectionByNameOrId("power_networks")
	if err != nil {
		return err
	}
	for _, network := range networks {
		record, findErr := h.FindFirstRecordByFilter("power_networks", "interface = {:interface} && source = 'discovered'", dbx.Params{"interface": network.Interface})
		if findErr != nil {
			record = core.NewRecord(collection)
		}
		record.Set("name", network.Name)
		record.Set("interface", network.Interface)
		record.Set("source", "discovered")
		record.Set("prefix", network.Prefix)
		record.Set("broadcast", network.Broadcast)
		record.Set("enabled", network.Enabled)
		if err := h.Save(record); err != nil {
			return err
		}
	}
	return nil
}

func (h *Hub) handlePower(e *core.RequestEvent) error {
	var req powerAPIRequest
	decoder := json.NewDecoder(e.Request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil || req.SystemID == "" {
		return e.BadRequestError("invalid power request", err)
	}
	record, err := h.FindRecordById("systems", req.SystemID)
	if err != nil {
		return e.NotFoundError("system not found", err)
	}
	if !record.GetBool("power_management_enabled") {
		return e.BadRequestError("power management is disabled for this system", nil)
	}
	requestID := uuid.NewString()
	switch req.Action {
	case "wake":
		return h.wakeSystem(e, record, requestID)
	case "shutdown", "cancel-shutdown", "shutdown-status":
		return h.agentPower(e, record, req, requestID)
	default:
		return e.BadRequestError("unknown power action", nil)
	}
}

func (h *Hub) wakeSystem(e *core.RequestEvent, record *core.Record, requestID string) error {
	if record.GetString("status") == "up" {
		h.auditPower(record.Id, "wake", "already_online", requestID, "", e.Auth.Id)
		return e.JSON(http.StatusOK, map[string]any{"request_id": requestID, "state": "already_online"})
	}
	if !record.GetBool("wol_enabled") {
		return e.BadRequestError("Wake-on-LAN is disabled for this system", nil)
	}
	diagnostics, diagnosticsErr := storedPowerDiagnostics(record)
	if diagnosticsErr != nil {
		return e.BadRequestError("Wake-on-LAN readiness is unavailable", diagnosticsErr)
	}
	if err := powerexec.ValidateReadiness(diagnostics, record.GetString("wol_interface"), record.GetString("wol_mac")); err != nil {
		return e.BadRequestError("Wake-on-LAN readiness check failed", err)
	}
	if h.isHubHost(record.GetString("host")) {
		return e.BadRequestError("the Hub cannot Wake-on-LAN itself", nil)
	}
	h.powerMu.Lock()
	last := h.powerLast[record.Id]
	if time.Since(last) < 15*time.Second {
		h.powerMu.Unlock()
		return e.JSON(http.StatusTooManyRequests, map[string]any{"request_id": requestID, "state": "queued", "retry_after": 15})
	}
	h.powerLast[record.Id] = time.Now()
	h.powerMu.Unlock()
	broadcast := record.GetString("wol_broadcast")
	interfaceName := record.GetString("wol_interface")
	if broadcast == "" && record.GetString("power_network_id") != "" {
		if network, err := h.FindRecordById("power_networks", record.GetString("power_network_id")); err == nil && network.GetBool("enabled") {
			broadcast = network.GetString("broadcast")
			if interfaceName == "" {
				interfaceName = network.GetString("interface")
			}
		}
	}
	if broadcast == "" {
		return e.BadRequestError("no enabled power network or broadcast override is assigned", nil)
	}
	port := record.GetInt("wol_port")
	if port == 0 {
		port = 9
	}
	ctx, cancel := context.WithTimeout(e.Request.Context(), 5*time.Second)
	defer cancel()
	err := h.powerExecutor.Wake(ctx, powerexec.WakeRequest{MAC: record.GetString("wol_mac"), Broadcast: broadcast, Interface: interfaceName, Port: port})
	if err != nil {
		h.auditPower(record.Id, "wake", "failed", requestID, "wol_send_failed", e.Auth.Id)
		return e.BadRequestError("Wake-on-LAN failed", err)
	}
	h.auditPower(record.Id, "wake", "packet_sent", requestID, "", e.Auth.Id)
	go h.waitForPowerOnline(record.Id, requestID, e.Auth.Id)
	return e.JSON(http.StatusAccepted, map[string]any{"request_id": requestID, "state": "packet_sent", "wait_timeout_seconds": 180})
}

func storedPowerDiagnostics(record *core.Record) (*powerentity.Diagnostics, error) {
	data, err := json.Marshal(record.Get("info"))
	if err != nil {
		return nil, err
	}
	var info struct {
		Power *powerentity.Diagnostics `json:"power"`
	}
	if err := json.Unmarshal(data, &info); err != nil {
		return nil, err
	}
	if info.Power == nil {
		return nil, errors.New("Agent has not reported power diagnostics")
	}
	return info.Power, nil
}

func (h *Hub) waitForPowerOnline(systemID, requestID, actor string) {
	deadline := time.Now().Add(3 * time.Minute)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	h.auditPower(systemID, "wake", "waiting", requestID, "", actor)
	for time.Now().Before(deadline) {
		<-ticker.C
		if record, err := h.FindRecordById("systems", systemID); err == nil && record.GetString("status") == "up" {
			h.auditPower(systemID, "wake", "online", requestID, "", actor)
			return
		}
	}
	h.auditPower(systemID, "wake", "timeout", requestID, "wol_timeout", actor)
}

func (h *Hub) agentPower(e *core.RequestEvent, record *core.Record, request powerAPIRequest, requestID string) error {
	system, err := h.sm.GetSystem(record.Id)
	if err != nil {
		return e.BadRequestError("Agent unavailable", err)
	}
	operation := maintenanceentity.GetPoweroffStatus
	switch request.Action {
	case "shutdown":
		operation = maintenanceentity.SchedulePoweroff
	case "cancel-shutdown":
		operation = maintenanceentity.CancelPoweroff
	}
	idempotency := ""
	if operation == maintenanceentity.SchedulePoweroff {
		idempotency = "power-" + requestID
	}
	req := maintenanceentity.Request{Version: maintenanceentity.ProtocolVersion, RequestID: requestID, Operation: operation, IdempotencyKey: idempotency, DelaySeconds: request.DelaySeconds}
	ctx, cancel := context.WithTimeout(e.Request.Context(), 15*time.Second)
	defer cancel()
	response, err := system.RequestMaintenance(ctx, req)
	if err != nil {
		h.auditPower(record.Id, request.Action, "failed", requestID, "agent_unavailable", e.Auth.Id)
		return e.BadRequestError("power operation failed", err)
	}
	h.auditPower(record.Id, request.Action, string(response.Status), requestID, response.ErrorCode, e.Auth.Id)
	return e.JSON(http.StatusOK, response)
}

func (h *Hub) auditPower(systemID, operation, state, requestID, errorCode, actor string) {
	collection, err := h.FindCachedCollectionByNameOrId("power_operations")
	if err != nil {
		return
	}
	record := core.NewRecord(collection)
	record.Set("system", systemID)
	record.Set("operation", operation)
	record.Set("state", state)
	record.Set("request_id", requestID)
	record.Set("error_code", errorCode)
	record.Set("actor", actor)
	_ = h.Save(record)
}

func (h *Hub) isHubHost(host string) bool {
	host = strings.Trim(host, "[]")
	if host == "localhost" || host == "127.0.0.1" || host == "::1" {
		return true
	}
	target := net.ParseIP(host)
	if target == nil {
		resolved, _ := net.LookupIP(host)
		if len(resolved) > 0 {
			target = resolved[0]
		}
	}
	if target == nil {
		return false
	}
	interfaces, _ := net.InterfaceAddrs()
	for _, addr := range interfaces {
		ip, _, _ := net.ParseCIDR(addr.String())
		if ip != nil && ip.Equal(target) {
			return true
		}
	}
	return false
}
