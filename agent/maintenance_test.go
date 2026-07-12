package agent

import (
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"
	"github.com/henrygd/beszel/internal/common"
	entity "github.com/henrygd/beszel/internal/entities/maintenance"
)

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
	m := &maintenanceManager{enabled: true, replays: map[string]entity.Response{request.IdempotencyKey: cached}, agent: &Agent{}}
	if got := m.handle(request); got.Status != cached.Status || got.RequestID != cached.RequestID {
		t.Fatalf("replay was not idempotent: %#v", got)
	}
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
