# binboi-go Architecture

## Overview

`binboi-go` is the public engine/core repository for Binboi.

The current scaffold is intentionally modest: it implements a working control plane and a clean package layout while leaving deeper tunnel and proxy mechanics ready for incremental build-out.

## Package Roles

### Entrypoints

- `cmd/binboi`: operator CLI for local workflows and control-plane requests
- `cmd/binboid`: daemon entrypoint that loads config, starts logging, and serves the control plane

### Internal Packages

- `internal/auth`: token generation, hashed token storage, validation cache, local CLI credential helpers
- `internal/config`: runtime configuration defaults, loading, saving, validation
- `internal/control`: HTTP control API plus the stream protocol server for register and heartbeat flows
- `internal/session`: orchestration layer that validates requests and stores session state
- `internal/transport`: target parsing and protocol inference
- `internal/proxy`: route metadata planning
- `internal/tunnel`: public tunnel descriptor generation, random subdomain assignment, and durable tunnel record storage
- `internal/usage`: per-user usage accounting, plan-limit enforcement, and periodic durable flush
- `internal/observability`: logging setup

### Public Packages

- `pkg/api`: shared HTTP and stream protocol message types
- `pkg/client`: external client for the HTTP API and stream control protocol

## Current Data Flow

1. `binboid` starts with config defaults or a JSON config file.
2. The daemon creates a logger, an in-memory session manager, a token store, and a tunnel registry store.
3. The control plane exposes `GET /healthz`, `GET/POST /v1/sessions`, and a JSON-over-TCP stream listener.
4. `binboid token create -user ...` generates a random raw token once, stores only its SHA-256 hash and metadata in the token database, and exposes the raw token for operator distribution.
5. `binboi auth login -token ...` stores the raw token locally with restricted file permissions so tunnel commands can reuse it automatically.
6. `binboi http 3000` connects to the stream listener, sends a `register` message with the token, and the daemon validates it through the in-memory auth cache backed by the token database.
7. On first registration the daemon creates a tunnel record, allocates a unique random subdomain under the configured public host, persists the tunnel metadata, and returns a `registered` response containing the `public_url` and `subdomain`.
8. The client then sends `ping` heartbeats and the daemon responds with `pong` while updating the in-memory session record and durable tunnel status.
9. Usage reservations are checked before a new tunnel or forwarded request is admitted, which lets the daemon enforce simple active-tunnel and request-volume limits without writing to disk on the hot path.
10. Incoming HTTP requests are routed from the `Host` header through in-memory host and subdomain indexes, then converted into framed `request_*` protocol messages, forwarded to the connected client, proxied to the local service, and returned as framed `response_*` messages.
11. The server tracks multiple in-flight requests per tunnel by request ID while the client processes request messages concurrently.
12. One fair outbound dispatcher per tunnel multiplexes stream frames in round-robin order so large bodies and upgraded streams do not monopolize the connection.
13. Flow control limits bound active streams and per-stream buffered chunks, which lets backpressure propagate naturally when either side slows down.
14. Completed requests update per-user counters for request count, bytes in, bytes out, and active tunnels in memory; a background flush loop periodically writes those snapshots to the usage database.
15. HTTP upgrade requests such as WebSocket handshakes are detected from the forwarded headers, proxied to the local upstream, and then switched into long-lived bidirectional raw byte streams while keeping the same request ID as the stream ID.
16. Cancellation is propagated with request-scoped cancel messages so user disconnects, idle streams, or timeouts can abort local upstream work.
17. When a control connection drops, the client retries with exponential backoff and attempts to resume the same tunnel using a resumable session identity.
18. Session creation requests are normalized through `transport`, planned through `proxy`, and surfaced through `tunnel`.

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
- token scope models and daemon-to-database persistence beyond the current local file store
- custom domains and reserved subdomain claims on top of the current random-subdomain router
- richer flow windows and weighted multiplex scheduling on top of the current framed stream transport
- long-lived upgraded stream resume semantics across reconnects
- richer resume semantics such as expiry windows and request cancellation

## Design Principles

- Go-first and dependency-light
- small public API surface
- explicit package responsibilities
- easy local development
- documentation that matches the current reality

Made by Sardor Azimov
