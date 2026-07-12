package hub

import (
	"context"
	"net/http"
	"time"

	maintenanceentity "github.com/henrygd/beszel/internal/entities/maintenance"
	"github.com/henrygd/beszel/internal/entities/system"
	"github.com/pocketbase/pocketbase/core"
)

type maintenanceAPIRequest struct {
	SystemID string                    `json:"system_id"`
	Request  maintenanceentity.Request `json:"request"`
}

func (h *Hub) handleMaintenance(e *core.RequestEvent) error {
	var body maintenanceAPIRequest
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("Invalid maintenance request.", err)
	}
	if e.Auth == nil || e.Auth.GetString("role") != "admin" {
		h.auditMaintenance(e.Auth, body.SystemID, "unauthorized_attempt", 0)
		return e.ForbiddenError("Administrative permission is required for maintenance operations.", nil)
	}
	if err := maintenanceentity.ValidateRequest(body.Request); err != nil {
		h.auditMaintenance(e.Auth, body.SystemID, "operation_refused", 0)
		return e.BadRequestError(err.Error(), err)
	}
	sys, err := h.sm.GetSystem(body.SystemID)
	if err != nil {
		return e.NotFoundError("System not found.", err)
	}
	if !sys.HasUser(h, e.Auth) {
		h.auditMaintenance(e.Auth, body.SystemID, "unauthorized_attempt", 0)
		return e.ForbiddenError("You do not have access to this system.", nil)
	}
	if !sys.SupportsMaintenanceProtocol() {
		h.auditMaintenance(e.Auth, body.SystemID, "agent_incompatible", 0)
		return e.BadRequestError("Agent version does not support maintenance operations.", nil)
	}
	if body.Request.Operation != maintenanceentity.GetCapabilities {
		record, findErr := h.FindRecordById("systems", body.SystemID)
		if findErr != nil {
			return findErr
		}
		var info system.Info
		_ = record.UnmarshalJSONField("info", &info)
		caps := (*maintenanceentity.Capabilities)(nil)
		if info.Updates != nil {
			caps = info.Updates.Capabilities
		}
		if caps == nil || !caps.PrivilegedHelper || !maintenanceCapabilityAvailable(caps, body.Request.Operation) {
			h.auditMaintenance(e.Auth, body.SystemID, "helper_incompatible", 0)
			return e.BadRequestError("The Agent does not advertise the capability required for this operation.", nil)
		}
	}
	timeout := 45 * time.Second
	if maintenanceentity.IsLongOperation(body.Request.Operation) {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(e.Request.Context(), timeout)
	defer cancel()
	response, err := sys.RequestMaintenance(ctx, body.Request)
	if err != nil {
		h.auditMaintenance(e.Auth, body.SystemID, "operation_timeout", 0)
		return e.InternalServerError("Maintenance request failed.", err)
	}
	event := "operation_" + string(response.Operation) + "_" + string(response.Status)
	h.auditMaintenance(e.Auth, body.SystemID, event, float64(response.Progress))
	return e.JSON(http.StatusOK, response)
}

func maintenanceCapabilityAvailable(caps *maintenanceentity.Capabilities, operation maintenanceentity.Operation) bool {
	if caps == nil || !caps.UpdateManagement {
		return false
	}
	switch operation {
	case maintenanceentity.GetUpdatePolicy, maintenanceentity.DetectRepositories:
		return caps.PolicyRead
	case maintenanceentity.ValidateUpdatePolicy, maintenanceentity.ApplyUpdatePolicy:
		return caps.PolicyWrite
	case maintenanceentity.RunUpdateDryRun:
		return caps.DryRun
	case maintenanceentity.RunUnattendedUpgrades:
		return caps.RunUpgrade
	case maintenanceentity.InstallUpdateDependencies, maintenanceentity.GetOperationStatus:
		return true
	default:
		return false
	}
}

func (h *Hub) auditMaintenance(user *core.Record, systemID, event string, value float64) {
	if user == nil || systemID == "" {
		return
	}
	collection, err := h.FindCachedCollectionByNameOrId("alerts_history")
	if err != nil {
		return
	}
	record := core.NewRecord(collection)
	record.Set("user", user.Id)
	record.Set("system", systemID)
	record.Set("name", "maintenance:"+event)
	record.Set("value", value)
	if err := h.SaveNoValidate(record); err != nil {
		h.Logger().Error("Failed to audit maintenance event", "event", event, "err", err)
	}
}
