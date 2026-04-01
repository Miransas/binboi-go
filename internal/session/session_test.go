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
	if created.Subdomain == "" {
		t.Fatal("expected subdomain to be populated")
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

	result, err := manager.RegisterTunnel(context.Background(), RegisterRequest{
		Protocol:  "http",
		LocalPort: 3000,
		Conn:      serverConn,
	})
	if err != nil {
		t.Fatalf("register tunnel: %v", err)
	}
	registered := result.Session
	if result.Resumed {
		t.Fatal("expected initial register, not resume")
	}

	if registered.LocalPort != 3000 {
		t.Fatalf("local port mismatch: got %d want 3000", registered.LocalPort)
	}
	if registered.Subdomain == "" {
		t.Fatal("expected subdomain")
	}
	if registered.LastHeartbeat == nil {
		t.Fatal("expected last heartbeat to be initialized")
	}
	if lookedUp, ok := manager.LookupByHost(hostFromPublicURL(registered.PublicURL)); !ok || lookedUp.ID != registered.ID {
		t.Fatal("expected route lookup by public host to return registered session")
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

	manager.DetachConnection(registered.ID, now)
	sessions = manager.List(context.Background())
	if sessions[0].Connection != "disconnected" {
		t.Fatalf("connection state mismatch: got %q want disconnected", sessions[0].Connection)
	}
	if lookedUp, ok := manager.LookupByHost(hostFromPublicURL(registered.PublicURL)); !ok || lookedUp.Connection != "disconnected" {
		t.Fatal("expected disconnected tunnel to remain known for inactive routing decisions")
	}
}

func TestManagerResumeTunnel(t *testing.T) {
	t.Parallel()

	manager := NewManager("local.binboi.test", "X-Binboi-Session")
	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()
	defer clientConn.Close()

	initial, err := manager.RegisterTunnel(context.Background(), RegisterRequest{
		Protocol:  "http",
		LocalPort: 3000,
		Conn:      serverConn,
	})
	if err != nil {
		t.Fatalf("initial register: %v", err)
	}

	token, ok := manager.ResumeToken(initial.Session.ID)
	if !ok {
		t.Fatal("expected resume token")
	}

	manager.DetachConnection(initial.Session.ID, time.Now().UTC())

	resumeServerConn, resumeClientConn := net.Pipe()
	defer resumeServerConn.Close()
	defer resumeClientConn.Close()

	resumed, err := manager.RegisterTunnel(context.Background(), RegisterRequest{
		Protocol:       "http",
		LocalPort:      3000,
		Conn:           resumeServerConn,
		ResumeTunnelID: initial.Session.ID,
		ResumeToken:    token,
	})
	if err != nil {
		t.Fatalf("resume register: %v", err)
	}
	if !resumed.Resumed {
		t.Fatal("expected resumed result")
	}
	if resumed.Session.ID != initial.Session.ID {
		t.Fatalf("session ID mismatch: got %q want %q", resumed.Session.ID, initial.Session.ID)
	}
	if resumed.Session.Subdomain != initial.Session.Subdomain {
		t.Fatalf("subdomain mismatch after resume: got %q want %q", resumed.Session.Subdomain, initial.Session.Subdomain)
	}
}
