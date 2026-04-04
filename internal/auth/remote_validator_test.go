package auth

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRemoteValidatorValidate(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-binboi-internal-secret") != "secret" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		var body struct {
			Token string `json:"token"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body.Token != "bin_live_abcd_demo" {
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error":   "invalid_token",
				"message": "Token is invalid.",
			})
			return
		}

		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true,
			"data": map[string]any{
				"tokenId":     "tok_123",
				"prefix":      "bin_live_abcd",
				"principalId": "ws_123",
			},
		})
	}))
	defer server.Close()

	validator := NewRemoteValidator(server.URL, RemoteValidatorConfig{
		SharedSecret: "secret",
		Timeout:      time.Second,
		CacheTTL:     time.Second,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	principal, err := validator.Validate(context.Background(), "bin_live_abcd_demo")
	if err != nil {
		t.Fatalf("validate token: %v", err)
	}
	if principal.TokenID != "tok_123" {
		t.Fatalf("token ID mismatch: got %q want %q", principal.TokenID, "tok_123")
	}
	if principal.UserID != "ws_123" {
		t.Fatalf("principal ID mismatch: got %q want %q", principal.UserID, "ws_123")
	}
	if principal.Prefix != "bin_live_abcd" {
		t.Fatalf("prefix mismatch: got %q want %q", principal.Prefix, "bin_live_abcd")
	}
}

func TestRemoteValidatorRevokedToken(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error":   "revoked_token",
			"message": "Token has been revoked.",
		})
	}))
	defer server.Close()

	validator := NewRemoteValidator(server.URL, RemoteValidatorConfig{
		SharedSecret: "secret",
		Timeout:      time.Second,
		CacheTTL:     time.Second,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	_, err := validator.Validate(context.Background(), "bin_live_abcd_demo")
	if !errors.Is(err, ErrRevokedToken) {
		t.Fatalf("expected revoked token error, got %v", err)
	}
}
