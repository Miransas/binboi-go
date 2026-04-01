package config

import (
	"path/filepath"
	"testing"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	t.Parallel()

	cfg := Default()
	cfg.Tunnel.PublicHost = "public.binboi.dev"

	path := filepath.Join(t.TempDir(), "binboid.json")
	if err := Save(path, cfg); err != nil {
		t.Fatalf("save config: %v", err)
	}

	got, err := Load(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	if got.Tunnel.PublicHost != cfg.Tunnel.PublicHost {
		t.Fatalf("public host mismatch: got %q want %q", got.Tunnel.PublicHost, cfg.Tunnel.PublicHost)
	}
}
