package session

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/sardorazimov/binboi-go/pkg/api"
)

func TestManagerCreateAndList(t *testing.T) {
	t.Parallel()

	manager := NewManager("local.binboi.test", "X-Binboi-Session")

	created, err := manager.Create(context.Background(), api.CreateSessionRequest{
		Name:   "demo",
		Target: "http://127.0.0.1:3000",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	if created.PublicURL == "" {
		t.Fatal("expected public URL to be populated")
	}

	sessions := manager.List(context.Background())
	if len(sessions) != 1 {
		t.Fatalf("session count mismatch: got %d want 1", len(sessions))
	}
}

func TestManagerRegisterAndHeartbeat(t *testing.T) {
	t.Parallel()

	manager := NewManager("local.binboi.test", "X-Binboi-Session")
	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()
	defer clientConn.Close()

	registered, err := manager.RegisterTunnel(context.Background(), RegisterRequest{
		Protocol:  "http",
		LocalPort: 3000,
		Conn:      serverConn,
	})
	if err != nil {
		t.Fatalf("register tunnel: %v", err)
	}

	if registered.LocalPort != 3000 {
		t.Fatalf("local port mismatch: got %d want 3000", registered.LocalPort)
	}
	if registered.LastHeartbeat == nil {
		t.Fatal("expected last heartbeat to be initialized")
	}

	now := time.Now().UTC()
	if err := manager.TouchHeartbeat(registered.ID, now); err != nil {
		t.Fatalf("touch heartbeat: %v", err)
	}

	sessions := manager.List(context.Background())
	if len(sessions) != 1 {
		t.Fatalf("session count mismatch: got %d want 1", len(sessions))
	}
	if sessions[0].LastHeartbeat == nil || !sessions[0].LastHeartbeat.Equal(now) {
		t.Fatal("expected last heartbeat to be updated")
	}
}
