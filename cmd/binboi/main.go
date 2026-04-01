package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/sardorazimov/binboi-go/internal/auth"
	"github.com/sardorazimov/binboi-go/internal/config"
	"github.com/sardorazimov/binboi-go/internal/observability"
	"github.com/sardorazimov/binboi-go/pkg/api"
	"github.com/sardorazimov/binboi-go/pkg/client"
)

var (
	version = "0.1.0-dev"
	commit  = "unknown"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "binboi: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string, stdout io.Writer, stderr io.Writer) error {
	if len(args) == 0 {
		printUsage(stderr)
		return nil
	}

	switch args[0] {
	case "version":
		fmt.Fprintf(stdout, "binboi %s (%s)\n", version, commit)
		return nil
	case "http", "https", "tcp":
		return runTunnelCommand(args[0], args[1:], stdout)
	case "config":
		return runConfig(args[1:], stdout)
	case "auth":
		return runAuth(args[1:], stdout)
	case "health":
		return runHealth(args[1:], stdout)
	case "session":
		return runSession(args[1:], stdout)
	case "help", "-h", "--help":
		printUsage(stdout)
		return nil
	default:
		printUsage(stderr)
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, "binboi is the Binboi engine CLI.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  binboi version")
	fmt.Fprintln(w, "  binboi http 3000")
	fmt.Fprintln(w, "  binboi tcp 5432 -server 127.0.0.1:8081")
	fmt.Fprintln(w, "  binboi config init -path ./binboid.json")
	fmt.Fprintln(w, "  binboi config print-sample")
	fmt.Fprintln(w, "  binboi auth login -token <raw_token>")
	fmt.Fprintln(w, "  binboi auth show")
	fmt.Fprintln(w, "  binboi health -server http://127.0.0.1:8080")
	fmt.Fprintln(w, "  binboi session create -name local-http -target http://127.0.0.1:3000")
	fmt.Fprintln(w, "  binboi session list")
}

func runTunnelCommand(protocol string, args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("%s requires a local port", protocol)
	}

	localPort, err := strconv.Atoi(args[0])
	if err != nil {
		return fmt.Errorf("parse local port: %w", err)
	}

	fs := flag.NewFlagSet(protocol, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	serverAddr := fs.String("server", "127.0.0.1:8081", "daemon stream control address")
	token := fs.String("token", "", "optional auth token")
	credentialsPath := fs.String("credentials", "", "optional credentials file override")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}

	logger, err := observability.NewLogger("info", "text")
	if err != nil {
		return err
	}

	tunnelClient, err := client.NewTunnel(*serverAddr, logger)
	if err != nil {
		return err
	}

	hostname, _ := os.Hostname()
	authToken, err := resolveTunnelToken(*token, *credentialsPath)
	if err != nil {
		return err
	}

	signalCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	registerPayload := api.RegisterPayload{
		Protocol:  protocol,
		LocalPort: localPort,
		AuthToken: authToken,
		Metadata: api.ClientMetadata{
			ClientVersion: version,
			Hostname:      hostname,
			OS:            runtime.GOOS,
			Arch:          runtime.GOARCH,
		},
	}

	printed := false
	return tunnelClient.Run(signalCtx, registerPayload, func(registered api.RegisteredPayload) {
		if printed {
			return
		}
		_ = writeJSON(stdout, registered)
		printed = true
	})
}

func runAuth(args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New("auth requires a subcommand: login, logout, or show")
	}

	switch args[0] {
	case "login":
		fs := flag.NewFlagSet("auth login", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		token := fs.String("token", "", "raw auth token")
		path := fs.String("path", "", "optional credentials file path")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if strings.TrimSpace(*token) == "" {
			return errors.New("token is required")
		}
		if _, err := auth.PrefixFromRawToken(*token); err != nil {
			return err
		}

		credentialsPath, err := credentialsPathOrDefault(*path)
		if err != nil {
			return err
		}
		creds := auth.LocalCredentials{Token: strings.TrimSpace(*token)}
		if err := auth.SaveLocalCredentials(credentialsPath, creds); err != nil {
			return err
		}

		saved, err := auth.LoadLocalCredentials(credentialsPath)
		if err != nil {
			return err
		}
		return writeJSON(stdout, map[string]any{
			"path":       credentialsPath,
			"token_hint": saved.TokenHint,
			"saved_at":   saved.SavedAt,
		})
	case "logout":
		fs := flag.NewFlagSet("auth logout", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		path := fs.String("path", "", "optional credentials file path")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}

		credentialsPath, err := credentialsPathOrDefault(*path)
		if err != nil {
			return err
		}
		if err := auth.ClearLocalCredentials(credentialsPath); err != nil {
			return err
		}
		return writeJSON(stdout, map[string]any{
			"path":    credentialsPath,
			"cleared": true,
		})
	case "show":
		fs := flag.NewFlagSet("auth show", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		path := fs.String("path", "", "optional credentials file path")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}

		credentialsPath, err := credentialsPathOrDefault(*path)
		if err != nil {
			return err
		}
		creds, err := auth.LoadLocalCredentials(credentialsPath)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return writeJSON(stdout, map[string]any{
					"path":    credentialsPath,
					"present": false,
				})
			}
			return err
		}
		return writeJSON(stdout, map[string]any{
			"path":       credentialsPath,
			"present":    true,
			"token_hint": creds.TokenHint,
			"saved_at":   creds.SavedAt,
		})
	default:
		return fmt.Errorf("unknown auth subcommand %q", args[0])
	}
}

func resolveTunnelToken(explicitToken, explicitPath string) (string, error) {
	if strings.TrimSpace(explicitToken) != "" {
		return strings.TrimSpace(explicitToken), nil
	}

	credentialsPath, err := credentialsPathOrDefault(explicitPath)
	if err != nil {
		return "", err
	}

	creds, err := auth.LoadLocalCredentials(credentialsPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(creds.Token), nil
}

func credentialsPathOrDefault(path string) (string, error) {
	if strings.TrimSpace(path) != "" {
		return path, nil
	}
	return auth.DefaultLocalCredentialsPath()
}

func runConfig(args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New("config requires a subcommand: init or print-sample")
	}

	switch args[0] {
	case "init":
		fs := flag.NewFlagSet("config init", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		path := fs.String("path", "./binboid.json", "config file path")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}

		cfg := config.Default()
		if err := config.Save(*path, cfg); err != nil {
			return err
		}

		fmt.Fprintf(stdout, "wrote sample config to %s\n", *path)
		return nil
	case "print-sample":
		cfg := config.Default()
		return writeJSON(stdout, cfg)
	default:
		return fmt.Errorf("unknown config subcommand %q", args[0])
	}
}

func runHealth(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("health", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	serverURL := fs.String("server", "http://127.0.0.1:8080", "control plane base URL")
	timeout := fs.Duration("timeout", 5*time.Second, "request timeout")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cli, err := client.New(*serverURL)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	health, err := cli.Health(ctx)
	if err != nil {
		return err
	}

	return writeJSON(stdout, health)
}

func runSession(args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New("session requires a subcommand: create or list")
	}

	switch args[0] {
	case "create":
		fs := flag.NewFlagSet("session create", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		serverURL := fs.String("server", "http://127.0.0.1:8080", "control plane base URL")
		name := fs.String("name", "", "human-friendly session name")
		target := fs.String("target", "", "upstream target URL, for example http://127.0.0.1:3000")
		protocol := fs.String("protocol", "", "optional explicit protocol override")
		token := fs.String("token", "", "optional control-plane token for future auth wiring")
		timeout := fs.Duration("timeout", 5*time.Second, "request timeout")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}

		if strings.TrimSpace(*name) == "" {
			return errors.New("name is required")
		}
		if strings.TrimSpace(*target) == "" {
			return errors.New("target is required")
		}

		cli, err := client.New(*serverURL)
		if err != nil {
			return err
		}

		ctx, cancel := context.WithTimeout(context.Background(), *timeout)
		defer cancel()

		session, err := cli.CreateSession(ctx, api.CreateSessionRequest{
			Name:     *name,
			Target:   *target,
			Protocol: *protocol,
			Token:    *token,
		})
		if err != nil {
			return err
		}

		return writeJSON(stdout, session)
	case "list":
		fs := flag.NewFlagSet("session list", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		serverURL := fs.String("server", "http://127.0.0.1:8080", "control plane base URL")
		timeout := fs.Duration("timeout", 5*time.Second, "request timeout")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}

		cli, err := client.New(*serverURL)
		if err != nil {
			return err
		}

		ctx, cancel := context.WithTimeout(context.Background(), *timeout)
		defer cancel()

		sessions, err := cli.ListSessions(ctx)
		if err != nil {
			return err
		}

		return writeJSON(stdout, api.ListSessionsResponse{Sessions: sessions})
	default:
		return fmt.Errorf("unknown session subcommand %q", args[0])
	}
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
