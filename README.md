# binboi-go

`binboi-go` is the public Go engine repository for Binboi.

This repository contains the open tunnel, proxy, control-plane, and runtime scaffolding that powers the technical core of the product. The main Binboi dashboard, SaaS workflows, billing flows, and product-specific application code live in a separate private repository.

The goal of this repo is to be easy to read, easy to extend, and comfortable for open-source contributors to navigate.

## What This Repository Is

- A public-facing engine/core repository
- A Go-first codebase for the Binboi runtime
- A place for CLI, daemon, control API, session orchestration, tunnel/proxy scaffolding, and examples

## What This Repository Is Not

- The Binboi dashboard
- The SaaS product repository
- Billing, marketing, or product-site code

## Quick Start

### 1. Run the daemon

```bash
go run ./cmd/binboid
```

The daemon starts a lightweight control API on `:8080` by default.
The daemon also starts a JSON-over-TCP stream control listener on `:8081` by default.

### 2. Create an auth token

```bash
go run ./cmd/binboid token create -user user_123
```

The daemon returns the raw token exactly once and stores only its hash in the local token database.

### 3. Save the token in the CLI

```bash
go run ./cmd/binboi auth login -token <raw_token>
```

### 4. Check health

```bash
go run ./cmd/binboi health
```

### 5. Start a live tunnel control session

```bash
go run ./cmd/binboi http 3000
```

That command connects to the daemon, automatically includes the locally saved auth token in the `register` handshake, receives tunnel metadata including a random public subdomain, keeps the control connection alive with heartbeat `ping`/`pong` messages, can proxy basic HTTP requests and upgraded WebSocket connections back to your local service, automatically retries with session resume when the control connection drops, applies bounded per-stream flow control for safer multiplexing, and contributes per-user usage totals for limits and billing.

### 6. Generate a config file

```bash
go run ./cmd/binboi config init -path ./binboid.json
```

## Relationship To The Main Binboi Product

`binboi-go` is the open engine/core layer.

The private Binboi product repository can depend on this engine, embed it, wrap it, or integrate it as part of the broader hosted experience. Keeping the engine public makes the core runtime easier to audit, document, and contribute to while leaving product-specific concerns separate.

## Repository Structure

```text
.
├── cmd/                 # CLI and daemon entrypoints
├── docs/                # Architecture and supporting docs
├── examples/            # Runnable and deployment-oriented examples
├── internal/            # Internal engine packages
├── pkg/                 # Public importable packages
└── scripts/             # Install and developer helper scripts
```

## Development Notes

- The current scaffold implements a working HTTP API, stream control protocol, hashed token-based client auth with in-memory validation cache, random public subdomain assignment, host-based tunnel routing, in-memory usage accounting with periodic flush and plan limits, config loader, logger setup, CLI commands, resumable in-memory session tracking, automatic reconnect, concurrent HTTP request forwarding, upgraded WebSocket tunneling, framed body streaming, and bounded flow control with fair per-stream scheduling.
- The forwarding layer is still intentionally modest: HTTP and WebSocket traffic work over framed streams with backpressure and idle timeouts, but richer flow windows, binary framing, and more advanced protocol adapters are still future work.
- Daemon flow limits live under `control.flow_control` in the JSON config and control active streams, per-stream buffering, stream timeout, and idle timeout behavior.
- Client auth settings live under `auth` in the daemon config and control the token database path, validation cache TTL, and `last_used_at` persistence cadence.
- Tunnel routing settings live under `tunnel` in the daemon config and control the public host suffix plus the local tunnel registry path used for subdomain uniqueness and tunnel status tracking.
- Usage settings live under `usage` in the daemon config and control the usage database path, flush cadence, billing window, and simple per-user plan caps for requests, bytes, and active tunnels.
- The codebase favors standard library dependencies to keep the engine portable and easy to audit.
- Folder-level README files are included to make each major area understandable on first read.

## Current Commands

- `binboi version`
- `binboi auth login`
- `binboi auth logout`
- `binboi auth show`
- `binboi http 3000`
- `binboi health`
- `binboi config init`
- `binboi config print-sample`
- `binboi session create`
- `binboi session list`
- `binboid token create`
- `binboid token list`
- `binboid token revoke`

## Examples

- [`examples/http-basic`](./examples/http-basic): simple upstream HTTP service for local testing
- [`examples/docker-basic`](./examples/docker-basic): Docker Compose example showing the intended daemon layout

## Contributing

See [`CONTRIBUTING.md`](./CONTRIBUTING.md) for local development workflow and contribution expectations.

## License

This repository is licensed under the Apache License 2.0. See [`LICENSE`](./LICENSE).

Made by Sardor Azimov
