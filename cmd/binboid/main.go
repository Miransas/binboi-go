package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"

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
	server := control.NewServer(cfg.Control.ListenAddress, cfg.Service.Name, version, logger, manager)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	logger.Info("starting binboid",
		"version", version,
		"commit", commit,
		"addr", cfg.Control.ListenAddress,
		"public_host", cfg.Tunnel.PublicHost,
		"environment", cfg.Service.Environment,
	)

	if err := server.Run(ctx); err != nil && err != http.ErrServerClosed {
		return err
	}

	logger.Info("binboid stopped")
	return nil
}
