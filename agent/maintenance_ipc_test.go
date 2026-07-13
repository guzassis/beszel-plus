package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"testing"
	"time"

	entity "github.com/henrygd/beszel/internal/entities/maintenance"
	"github.com/henrygd/beszel/internal/maintenance"
)

func TestMaintenanceCallHalfClosesWriteAndReadsResponse(t *testing.T) {
	socket := fmt.Sprintf("/tmp/beszel-ipc-%d-%d.sock", os.Getpid(), time.Now().UnixNano())
	t.Cleanup(func() { _ = os.Remove(socket) })
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	done := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			done <- err
			return
		}
		defer conn.Close()
		done <- maintenance.NewHelper().Serve(context.Background(), conn, conn)
	}()

	manager := &maintenanceManager{socket: socket}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	response, err := manager.call(ctx, entity.Request{Version: entity.ProtocolVersion, RequestID: "capabilities", Operation: entity.GetCapabilities})
	if err != nil {
		t.Fatal(err)
	}
	if response.Status != entity.StateCompleted || response.Result == nil || response.Result.Capabilities == nil {
		t.Fatalf("unexpected response: %+v", response)
	}
	if err := <-done; err != nil {
		t.Fatalf("helper failed after complete exchange: %v", err)
	}
}

func TestHelperHandlesClientClosingBeforeResponse(t *testing.T) {
	server, client := net.Pipe()
	done := make(chan error, 1)
	go func() {
		defer server.Close()
		done <- maintenance.NewHelper().Serve(context.Background(), server, server)
	}()
	request := entity.Request{Version: entity.ProtocolVersion, RequestID: "capabilities", Operation: entity.GetCapabilities}
	if err := json.NewEncoder(client).Encode(request); err != nil {
		t.Fatal(err)
	}
	client.Close()
	select {
	case <-done: // A write error is controlled; the process is not signaled.
	case <-time.After(2 * time.Second):
		t.Fatal("helper did not terminate after client closed")
	}
}
