# binboi-go Architecture

## Overview

`binboi-go` is the public engine/core repository for Binboi.

The current scaffold is intentionally modest: it implements a working control plane and a clean package layout while leaving deeper tunnel and proxy mechanics ready for incremental build-out.

## Package Roles

### Entrypoints

- `cmd/binboi`: operator CLI for local workflows and control-plane requests
- `cmd/binboid`: daemon entrypoint that loads config, starts logging, and serves the control plane

### Internal Packages

- `internal/config`: runtime configuration defaults, loading, saving, validation
- `internal/control`: HTTP control API plus the stream protocol server for register and heartbeat flows
- `internal/session`: orchestration layer that validates requests and stores session state
- `internal/transport`: target parsing and protocol inference
- `internal/proxy`: route metadata planning
- `internal/tunnel`: public tunnel descriptor generation
- `internal/observability`: logging setup

### Public Packages

- `pkg/api`: shared HTTP and stream protocol message types
- `pkg/client`: external client for the HTTP API and stream control protocol

## Current Data Flow

1. `binboid` starts with config defaults or a JSON config file.
2. The daemon creates a logger and an in-memory session manager.
3. The control plane exposes `GET /healthz`, `GET/POST /v1/sessions`, and a JSON-over-TCP stream listener.
4. `binboi http 3000` connects to the stream listener, sends a `register` message, and receives a `registered` response with tunnel metadata.
5. The client then sends `ping` heartbeats and the daemon responds with `pong` while updating the in-memory session record.
6. Incoming HTTP requests are converted into `request` protocol messages, forwarded to the connected client, proxied to the local service, and returned as `response` messages.
7. The server tracks multiple in-flight requests per tunnel by request ID while the client processes request messages concurrently.
8. Request and response bodies are transported as framed streams using `*_start`, `*_body`, and `*_end` messages instead of whole buffered blobs.
9. Cancellation is propagated with request-scoped cancel messages so user disconnects or timeouts can abort local upstream work.
10. When a control connection drops, the client retries with exponential backoff and attempts to resume the same tunnel using a resumable session identity.
11. Session creation requests are normalized through `transport`, planned through `proxy`, and surfaced through `tunnel`.

## Why The Implementation Is Intentionally Small

This repository is meant to be public, navigable, and credible.

That means:

- implemented pieces should be real and runnable
- unfinished areas should be clearly scaffolded instead of faked
- package boundaries should be obvious early

The tunnel and proxy layers therefore expose realistic integration points without claiming that a full production transport plane is already finished.

## Near-Term Extension Points

- persistent session storage
- authentication on the control API
- real tunnel lifecycle workers
- proxy transport adapters
- metrics and tracing
- richer client ergonomics
- smarter multiplex scheduling and flow control on top of the current framed stream transport
- richer resume semantics such as expiry windows and request cancellation

## Design Principles

- Go-first and dependency-light
- small public API surface
- explicit package responsibilities
- easy local development
- documentation that matches the current reality

Made by Sardor Azimov
