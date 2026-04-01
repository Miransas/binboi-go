package control

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"

	"github.com/sardorazimov/binboi-go/internal/session"
	"github.com/sardorazimov/binboi-go/pkg/client"
)

func TestServerSessionLifecycle(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	manager := session.NewManager("local.binboi.test", "X-Binboi-Session")
	server := NewServer(":0", "binboid", "test", logger, manager)

	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	cli, err := client.New(ts.URL)
	if err != nil {
		t.Fatalf("create client: %v", err)
	}

	health, err := cli.Health(context.Background())
	if err != nil {
		t.Fatalf("health request: %v", err)
	}
	if health.Status != "ok" {
		t.Fatalf("health status mismatch: got %q want ok", health.Status)
	}

	session, err := cli.CreateSession(context.Background(), client.CreateSessionInput("demo", "http://127.0.0.1:3000"))
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if session.ID == "" {
		t.Fatal("expected session ID")
	}
}
