package transport

import (
	"testing"

	"github.com/fxamacker/cbor/v2"
	"github.com/henrygd/beszel/internal/common"
	entity "github.com/henrygd/beszel/internal/entities/maintenance"
)

func TestMaintenanceGenericResponse(t *testing.T) {
	want := entity.Response{Version: entity.ProtocolVersion, RequestID: "request-123", Operation: entity.GetCapabilities, Status: entity.StateCompleted}
	data, err := cbor.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var got entity.Response
	if err := UnmarshalResponse(common.AgentResponse{Data: data}, common.MaintenanceRequest, &got); err != nil {
		t.Fatal(err)
	}
	if got.RequestID != want.RequestID || got.Status != want.Status {
		t.Fatalf("got=%#v", got)
	}
}

func TestOldAgentRejectsMaintenanceWithoutBreakingLegacyActions(t *testing.T) {
	var response entity.Response
	if err := UnmarshalResponse(common.AgentResponse{}, common.MaintenanceRequest, &response); err == nil {
		t.Fatal("old agent unexpectedly accepted maintenance")
	}
}
