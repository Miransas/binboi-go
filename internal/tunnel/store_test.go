package tunnel

import (
	"path/filepath"
	"testing"
	"time"
)

func TestStoreUpsertFindAndUpdateStatus(t *testing.T) {
	t.Parallel()

	store := NewStore(filepath.Join(t.TempDir(), "tunnels.json"))
	if err := store.Ensure(); err != nil {
		t.Fatalf("ensure store: %v", err)
	}

	createdAt := time.Now().UTC().Add(-time.Minute)
	record := Record{
		ID:        "tun_123",
		UserID:    "user_123",
		TokenID:   "token_123",
		Subdomain: "abc123def0",
		Protocol:  "http",
		Port:      3000,
		Status:    "connected",
		PublicURL: "http://abc123def0.binboi.dev",
		CreatedAt: createdAt,
	}
	if err := store.Upsert(record); err != nil {
		t.Fatalf("upsert record: %v", err)
	}

	found, ok, err := store.FindBySubdomain(record.Subdomain)
	if err != nil {
		t.Fatalf("find by subdomain: %v", err)
	}
	if !ok {
		t.Fatal("expected subdomain record")
	}
	if found.UserID != record.UserID || found.TokenID != record.TokenID {
		t.Fatalf("record mismatch: got %#v want %#v", found, record)
	}

	lastSeen := time.Now().UTC()
	if err := store.UpdateStatus(record.ID, "disconnected", &lastSeen); err != nil {
		t.Fatalf("update status: %v", err)
	}

	updated, ok, err := store.FindBySubdomain(record.Subdomain)
	if err != nil {
		t.Fatalf("find updated record: %v", err)
	}
	if !ok {
		t.Fatal("expected updated subdomain record")
	}
	if updated.Status != "disconnected" {
		t.Fatalf("status mismatch: got %q want %q", updated.Status, "disconnected")
	}
	if updated.LastSeen == nil || !updated.LastSeen.Equal(lastSeen) {
		t.Fatal("expected last_seen to be updated")
	}
}
