package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

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
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "binboid: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	fs := flag.NewFlagSet("binboid", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	configPath := fs.String("config", "", "path to a JSON config file")
	if err := fs.Parse(os.Args[1:]); err != nil {
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

	manager := session.NewManager(cfg.Tunnel.PublicHost, cfg.Proxy.ForwardedHeader)
	server := control.NewServer(control.ServerConfig{
		HTTPAddress:       cfg.Control.ListenAddress,
		ProtocolAddress:   cfg.Control.ProtocolAddress,
		HeartbeatInterval: time.Duration(cfg.Control.HeartbeatIntervalSeconds) * time.Second,
		FlowControl:       cfg.Control.FlowControl,
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
		"environment", cfg.Service.Environment,
	)

	if err := server.Run(ctx); err != nil && err != http.ErrServerClosed {
		return err
	}

	logger.Info("binboid stopped")
	return nil
}
