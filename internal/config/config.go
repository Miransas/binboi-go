package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/sardorazimov/binboi-go/pkg/api"
)

// Config describes the daemon's runtime settings.
type Config struct {
	Service       ServiceConfig       `json:"service"`
	Control       ControlConfig       `json:"control"`
	Tunnel        TunnelConfig        `json:"tunnel"`
	Proxy         ProxyConfig         `json:"proxy"`
	Auth          AuthConfig          `json:"auth"`
	Observability ObservabilityConfig `json:"observability"`
}

// ServiceConfig identifies the runtime instance.
type ServiceConfig struct {
	Name        string `json:"name"`
	Environment string `json:"environment"`
}

// ControlConfig configures the daemon's HTTP and stream control-plane listeners.
type ControlConfig struct {
	ListenAddress            string          `json:"listen_address"`
	ProtocolAddress          string          `json:"protocol_address"`
	HeartbeatIntervalSeconds int             `json:"heartbeat_interval_seconds"`
	FlowControl              api.FlowControl `json:"flow_control"`
}

// TunnelConfig holds public tunnel-facing defaults.
type TunnelConfig struct {
	PublicHost      string `json:"public_host"`
	DefaultProtocol string `json:"default_protocol"`
}

// ProxyConfig carries edge-routing defaults.
type ProxyConfig struct {
	ForwardedHeader string `json:"forwarded_header"`
}

// AuthConfig controls token validation and cache behavior for client auth.
type AuthConfig struct {
	DatabasePath                  string `json:"database_path"`
	CacheTTLSeconds               int    `json:"cache_ttl_seconds"`
	LastUsedUpdateIntervalSeconds int    `json:"last_used_update_interval_seconds"`
}

// ObservabilityConfig controls basic logging output.
type ObservabilityConfig struct {
	Level  string `json:"level"`
	Format string `json:"format"`
}

// Default returns a production-minded starter config for local development.
func Default() Config {
	return Config{
		Service: ServiceConfig{
			Name:        "binboid",
			Environment: "development",
		},
		Control: ControlConfig{
			ListenAddress:            ":8080",
			ProtocolAddress:          ":8081",
			HeartbeatIntervalSeconds: 10,
			FlowControl:              api.FlowControl{}.Normalize(),
		},
		Tunnel: TunnelConfig{
			PublicHost:      "local.binboi.test",
			DefaultProtocol: "http",
		},
		Proxy: ProxyConfig{
			ForwardedHeader: "X-Binboi-Session",
		},
		Auth: AuthConfig{
			DatabasePath:                  "./data/binboi-tokens.json",
			CacheTTLSeconds:               30,
			LastUsedUpdateIntervalSeconds: 60,
		},
		Observability: ObservabilityConfig{
			Level:  "info",
			Format: "text",
		},
	}
}

// Load reads a JSON config file. If path is empty, the defaults are returned.
func Load(path string) (Config, error) {
	cfg := Default()
	if path == "" {
		return cfg, nil
	}

	file, err := os.Open(path)
	if err != nil {
		return Config{}, fmt.Errorf("open config: %w", err)
	}
	defer file.Close()

	dec := json.NewDecoder(file)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("decode config: %w", err)
	}

	if err := Validate(cfg); err != nil {
		return Config{}, err
	}

	return cfg, nil
}

// Save writes a JSON config file to disk.
func Save(path string, cfg Config) error {
	if path == "" {
		return errors.New("config path cannot be empty")
	}
	if err := Validate(cfg); err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}

	file, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create config: %w", err)
	}
	defer file.Close()

	enc := json.NewEncoder(file)
	enc.SetIndent("", "  ")
	if err := enc.Encode(cfg); err != nil {
		return fmt.Errorf("write config: %w", err)
	}

	return nil
}

// Validate ensures the config contains the minimum required values.
func Validate(cfg Config) error {
	if cfg.Service.Name == "" {
		return errors.New("service.name is required")
	}
	if cfg.Control.ListenAddress == "" {
		return errors.New("control.listen_address is required")
	}
	if cfg.Control.ProtocolAddress == "" {
		return errors.New("control.protocol_address is required")
	}
	if cfg.Control.HeartbeatIntervalSeconds <= 0 {
		return errors.New("control.heartbeat_interval_seconds must be greater than zero")
	}
	if err := cfg.Control.FlowControl.Validate(); err != nil {
		return fmt.Errorf("control.flow_control: %w", err)
	}
	if cfg.Tunnel.PublicHost == "" {
		return errors.New("tunnel.public_host is required")
	}
	if cfg.Tunnel.DefaultProtocol == "" {
		return errors.New("tunnel.default_protocol is required")
	}
	if cfg.Auth.DatabasePath == "" {
		return errors.New("auth.database_path is required")
	}
	if cfg.Auth.CacheTTLSeconds <= 0 {
		return errors.New("auth.cache_ttl_seconds must be greater than zero")
	}
	if cfg.Auth.LastUsedUpdateIntervalSeconds <= 0 {
		return errors.New("auth.last_used_update_interval_seconds must be greater than zero")
	}
	if cfg.Observability.Level == "" {
		return errors.New("observability.level is required")
	}
	if cfg.Observability.Format == "" {
		return errors.New("observability.format is required")
	}
	return nil
}
