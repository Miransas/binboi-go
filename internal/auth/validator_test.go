package auth

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStoreAndValidatorLifecycle(t *testing.T) {
	t.Parallel()

	store := NewStore(filepath.Join(t.TempDir(), "tokens.json"))
	if err := store.Ensure(); err != nil {
		t.Fatalf("ensure store: %v", err)
	}

	created, err := store.CreateToken(context.Background(), "user-123")
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	if created.RawToken == "" {
		t.Fatal("expected raw token")
	}
	if !strings.HasPrefix(created.RawToken, created.Token.Prefix+"_") {
		t.Fatalf("raw token prefix mismatch: got %q want prefix %q", created.RawToken, created.Token.Prefix)
	}

	contents, err := os.ReadFile(store.Path())
	if err != nil {
		t.Fatalf("read token store: %v", err)
	}
	if strings.Contains(string(contents), created.RawToken) {
		t.Fatal("raw token should never be persisted")
	}

	validator := NewValidator(store, ValidatorConfig{
		CacheTTL:               time.Second,
		LastUsedUpdateInterval: time.Millisecond,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	principal, err := validator.Validate(context.Background(), created.RawToken)
	if err != nil {
		t.Fatalf("validate token: %v", err)
	}
	if principal.UserID != "user-123" {
		t.Fatalf("user ID mismatch: got %q want %q", principal.UserID, "user-123")
	}
	if principal.TokenID != created.Token.ID {
		t.Fatalf("token ID mismatch: got %q want %q", principal.TokenID, created.Token.ID)
	}

	listed, err := store.ListTokens()
	if err != nil {
		t.Fatalf("list tokens: %v", err)
	}
	if len(listed) != 1 {
		t.Fatalf("token count mismatch: got %d want 1", len(listed))
	}
	if listed[0].LastUsedAt == nil {
		t.Fatal("expected last_used_at to be updated after validation")
	}

	if err := store.RevokeToken(created.Token.ID); err != nil {
		t.Fatalf("revoke token: %v", err)
	}

	freshValidator := NewValidator(store, ValidatorConfig{
		CacheTTL:               time.Second,
		LastUsedUpdateInterval: time.Millisecond,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	_, err = freshValidator.Validate(context.Background(), created.RawToken)
	if !errors.Is(err, ErrRevokedToken) {
		t.Fatalf("expected revoked token error, got %v", err)
	}
}
