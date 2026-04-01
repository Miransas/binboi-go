package session

import (
	"context"
	"testing"

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
