package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/sardorazimov/binboi-go/internal/auth"
	"github.com/sardorazimov/binboi-go/internal/config"
	"github.com/sardorazimov/binboi-go/internal/control"
	"github.com/sardorazimov/binboi-go/internal/observability"
	"github.com/sardorazimov/binboi-go/internal/session"
)

var (
	version = "0.1.0-dev"
	commit  = "unknown"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "binboid: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string, stdout io.Writer, stderr io.Writer) error {
	if len(args) > 0 {
		switch args[0] {
		case "token":
			return runToken(args[1:], stdout)
		case "help", "-h", "--help":
			printUsage(stdout)
			return nil
		}
	}

	return runDaemon(args)
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, "binboid is the Binboi engine daemon.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  binboid -config ./binboid.json")
	fmt.Fprintln(w, "  binboid token create -user user_123")
	fmt.Fprintln(w, "  binboid token list")
	fmt.Fprintln(w, "  binboid token revoke -id <token_id>")
}

func runDaemon(args []string) error {
	fs := flag.NewFlagSet("binboid", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	configPath := fs.String("config", "", "path to a JSON config file")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}

	logger, err := observability.NewLogger(cfg.Observability.Level, cfg.Observability.Format)
	if err != nil {
		return err
	}

	tokenStore := auth.NewStore(cfg.Auth.DatabasePath)
	if err := tokenStore.Ensure(); err != nil {
		return fmt.Errorf("initialize token store: %w", err)
	}

	tokenValidator := auth.NewValidator(tokenStore, auth.ValidatorConfig{
		CacheTTL:               time.Duration(cfg.Auth.CacheTTLSeconds) * time.Second,
		LastUsedUpdateInterval: time.Duration(cfg.Auth.LastUsedUpdateIntervalSeconds) * time.Second,
	}, logger)

	manager := session.NewManager(cfg.Tunnel.PublicHost, cfg.Proxy.ForwardedHeader)
	server := control.NewServer(control.ServerConfig{
		HTTPAddress:       cfg.Control.ListenAddress,
		ProtocolAddress:   cfg.Control.ProtocolAddress,
		HeartbeatInterval: time.Duration(cfg.Control.HeartbeatIntervalSeconds) * time.Second,
		FlowControl:       cfg.Control.FlowControl,
		AuthValidator:     tokenValidator,
		Name:              cfg.Service.Name,
		Version:           version,
	}, logger, manager)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	logger.Info("starting binboid",
		"version", version,
		"commit", commit,
		"http_addr", cfg.Control.ListenAddress,
		"protocol_addr", cfg.Control.ProtocolAddress,
		"heartbeat_interval", time.Duration(cfg.Control.HeartbeatIntervalSeconds)*time.Second,
		"max_concurrent_streams", cfg.Control.FlowControl.Normalize().MaxConcurrentStreams,
		"buffered_bytes_per_stream", cfg.Control.FlowControl.Normalize().BufferedBytesPerStream,
		"stream_timeout", cfg.Control.FlowControl.Normalize().StreamTimeout(),
		"stream_idle_timeout", cfg.Control.FlowControl.Normalize().StreamIdleTimeout(),
		"public_host", cfg.Tunnel.PublicHost,
		"auth_database_path", cfg.Auth.DatabasePath,
		"auth_cache_ttl", time.Duration(cfg.Auth.CacheTTLSeconds)*time.Second,
		"auth_touch_interval", time.Duration(cfg.Auth.LastUsedUpdateIntervalSeconds)*time.Second,
		"environment", cfg.Service.Environment,
	)

	if err := server.Run(ctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}

	logger.Info("binboid stopped")
	return nil
}

func runToken(args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New("token requires a subcommand: create, list, or revoke")
	}

	switch args[0] {
	case "create":
		fs := flag.NewFlagSet("token create", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		configPath := fs.String("config", "", "optional config file path")
		dbPath := fs.String("db", "", "optional token database path override")
		userID := fs.String("user", "", "user identifier for the token owner")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *userID == "" {
			return errors.New("user is required")
		}

		store, err := authStoreForCommand(*configPath, *dbPath)
		if err != nil {
			return err
		}

		created, err := store.CreateToken(context.Background(), *userID)
		if err != nil {
			return err
		}
		return writeJSON(stdout, created)
	case "list":
		fs := flag.NewFlagSet("token list", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		configPath := fs.String("config", "", "optional config file path")
		dbPath := fs.String("db", "", "optional token database path override")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}

		store, err := authStoreForCommand(*configPath, *dbPath)
		if err != nil {
			return err
		}

		tokens, err := store.ListTokens()
		if err != nil {
			return err
		}
		return writeJSON(stdout, map[string]any{"tokens": tokens})
	case "revoke":
		fs := flag.NewFlagSet("token revoke", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		configPath := fs.String("config", "", "optional config file path")
		dbPath := fs.String("db", "", "optional token database path override")
		tokenID := fs.String("id", "", "token identifier to revoke")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *tokenID == "" {
			return errors.New("id is required")
		}

		store, err := authStoreForCommand(*configPath, *dbPath)
		if err != nil {
			return err
		}
		if err := store.RevokeToken(*tokenID); err != nil {
			return err
		}
		return writeJSON(stdout, map[string]any{
			"id":      *tokenID,
			"revoked": true,
		})
	default:
		return fmt.Errorf("unknown token subcommand %q", args[0])
	}
}

func authStoreForCommand(configPath, overridePath string) (*auth.Store, error) {
	path := overridePath
	if path == "" {
		cfg, err := loadConfigForTokenCommand(configPath)
		if err != nil {
			return nil, err
		}
		path = cfg.Auth.DatabasePath
	}

	store := auth.NewStore(path)
	if err := store.Ensure(); err != nil {
		return nil, fmt.Errorf("initialize token store: %w", err)
	}
	return store, nil
}

func loadConfigForTokenCommand(configPath string) (config.Config, error) {
	if configPath == "" {
		return config.Default(), nil
	}
	return config.Load(configPath)
}

func writeJSON(w io.Writer, value any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(value)
}
