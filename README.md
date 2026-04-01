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

### 2. Check health

```bash
go run ./cmd/binboi health
```

### 3. Start a live tunnel control session

```bash
go run ./cmd/binboi http 3000
```

That command connects to the daemon, sends a `register` message, receives tunnel metadata, keeps the control connection alive with heartbeat `ping`/`pong` messages, can proxy basic HTTP requests back to your local service, and now automatically retries with session resume when the control connection drops.

### 4. Generate a config file

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

- The current scaffold implements a working HTTP API, stream control protocol, config loader, logger setup, CLI commands, resumable in-memory session tracking, automatic reconnect, concurrent HTTP request forwarding, and framed body streaming.
- The forwarding layer is still intentionally modest: request and response bodies now move as framed chunks, but advanced flow control, websocket tunneling, and binary framing are still future work.
- The codebase favors standard library dependencies to keep the engine portable and easy to audit.
- Folder-level README files are included to make each major area understandable on first read.

## Current Commands

- `binboi version`
- `binboi http 3000`
- `binboi health`
- `binboi config init`
- `binboi config print-sample`
- `binboi session create`
- `binboi session list`

## Examples

- [`examples/http-basic`](./examples/http-basic): simple upstream HTTP service for local testing
- [`examples/docker-basic`](./examples/docker-basic): Docker Compose example showing the intended daemon layout

## Contributing

See [`CONTRIBUTING.md`](./CONTRIBUTING.md) for local development workflow and contribution expectations.

## License

This repository is licensed under the Apache License 2.0. See [`LICENSE`](./LICENSE).

Made by Sardor Azimov
