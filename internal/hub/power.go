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

type wakeTarget struct {
	MAC       string
	Broadcast string
	Interface string
	Port      int
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
	if !powerManagementEnabled(record) {
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

func powerManagementEnabled(record *core.Record) bool {
	diagnostics, err := storedPowerDiagnostics(record)
	if err != nil {
		diagnostics = nil
	}
	return powerManagementAvailable(record.GetBool("power_management_enabled"), diagnostics)
}

func powerManagementAvailable(configured bool, diagnostics *powerentity.Diagnostics) bool {
	return configured || diagnostics != nil && diagnostics.Enabled
}

func (h *Hub) wakeSystem(e *core.RequestEvent, record *core.Record, requestID string) error {
	if record.GetString("status") == "up" {
		h.auditPower(record.Id, "wake", "already_online", requestID, "", e.Auth.Id)
		return e.JSON(http.StatusOK, map[string]any{"request_id": requestID, "state": "already_online"})
	}
	target, err := h.resolveWakeTarget(record)
	if err != nil {
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
	ctx, cancel := context.WithTimeout(e.Request.Context(), 5*time.Second)
	defer cancel()
	err = h.powerExecutor.Wake(ctx, powerexec.WakeRequest{MAC: target.MAC, Broadcast: target.Broadcast, Interface: target.Interface, Port: target.Port})
	if err != nil {
		h.auditPower(record.Id, "wake", "failed", requestID, "wol_send_failed", e.Auth.Id)
		return e.BadRequestError("Wake-on-LAN failed", err)
	}
	h.auditPower(record.Id, "wake", "packet_sent", requestID, "", e.Auth.Id)
	go h.waitForPowerOnline(record.Id, requestID, e.Auth.Id)
	return e.JSON(http.StatusAccepted, map[string]any{"request_id": requestID, "state": "packet_sent", "wait_timeout_seconds": 180})
}

func (h *Hub) resolveWakeTarget(record *core.Record) (wakeTarget, error) {
	diagnostics, err := storedPowerDiagnostics(record)
	if err != nil {
		return wakeTarget{}, errors.New("power diagnostics are unavailable")
	}
	selected, err := selectWakeInterface(diagnostics, record.GetString("wol_interface"))
	if err != nil {
		return wakeTarget{}, err
	}
	if err := powerexec.ValidateReadiness(diagnostics, selected.Interface, selected.MAC); err != nil {
		return wakeTarget{}, err
	}
	broadcast, hubInterface, err := h.resolveWakeNetwork(record, selected.IP, selected.Broadcast)
	if err != nil {
		return wakeTarget{}, err
	}
	port := record.GetInt("wol_port")
	if port == 0 {
		port = 9
	}
	return wakeTarget{MAC: selected.MAC, Broadcast: broadcast, Interface: hubInterface, Port: port}, nil
}

func selectWakeInterface(diagnostics *powerentity.Diagnostics, configured string) (powerentity.InterfaceDiagnostic, error) {
	if diagnostics == nil || !diagnostics.Enabled {
		return powerentity.InterfaceDiagnostic{}, errors.New("power diagnostics are unavailable")
	}
	// The Agent's selected interface is authoritative. Older installations may
	// have stored the Hub's interface name in wol_interface, so use it only when
	// it actually matches a diagnostic interface.
	preferred := strings.TrimSpace(diagnostics.SelectedInterface)
	if preferred == "" {
		preferred = strings.TrimSpace(configured)
	}
	var fallback *powerentity.InterfaceDiagnostic
	for i := range diagnostics.Interfaces {
		item := &diagnostics.Interfaces[i]
		if item.Interface == preferred {
			if item.Physical && item.Type == "ethernet" && item.Carrier && item.WOLSupported && item.WOLEnabled {
				return *item, nil
			}
		}
		if fallback == nil && item.Physical && item.Type == "ethernet" && item.Carrier && item.WOLSupported && item.WOLEnabled {
			fallback = item
		}
	}
	if fallback != nil {
		return *fallback, nil
	}
	return powerentity.InterfaceDiagnostic{}, errors.New("no ready physical Ethernet interface was reported")
}

func (h *Hub) resolveWakeNetwork(record *core.Record, targetIP, targetBroadcast string) (broadcast, hubInterface string, err error) {
	if networkID := record.GetString("power_network_id"); networkID != "" {
		if network, findErr := h.FindRecordById("power_networks", networkID); findErr == nil && network.GetBool("enabled") {
			if broadcast = network.GetString("broadcast"); broadcast != "" {
				return broadcast, network.GetString("interface"), nil
			}
		}
	}
	if networks, findErr := h.FindAllRecords("power_networks"); findErr == nil {
		candidates := make([]powerexec.Network, 0, len(networks))
		for _, network := range networks {
			candidates = append(candidates, powerexec.Network{Interface: network.GetString("interface"), Prefix: network.GetString("prefix"), Broadcast: network.GetString("broadcast"), Enabled: network.GetBool("enabled")})
		}
		if selected, ok := selectWakeNetwork(candidates, targetIP); ok {
			return selected.Broadcast, selected.Interface, nil
		}
	}
	if broadcast = strings.TrimSpace(record.GetString("wol_broadcast")); broadcast != "" {
		return broadcast, "", nil
	}
	if broadcast = strings.TrimSpace(targetBroadcast); broadcast != "" {
		return broadcast, "", nil
	}
	return "", "", errors.New("no broadcast address is available for the Agent network")
}

func selectWakeNetwork(networks []powerexec.Network, targetIP string) (powerexec.Network, bool) {
	ip := net.ParseIP(strings.TrimSpace(targetIP)).To4()
	if ip == nil {
		return powerexec.Network{}, false
	}
	var selected powerexec.Network
	bestPrefix := -1
	for _, network := range networks {
		if !network.Enabled || network.Broadcast == "" {
			continue
		}
		_, subnet, err := net.ParseCIDR(network.Prefix)
		if err != nil || !subnet.Contains(ip) {
			continue
		}
		prefix, _ := subnet.Mask.Size()
		if prefix > bestPrefix {
			selected, bestPrefix = network, prefix
		}
	}
	return selected, bestPrefix >= 0
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
