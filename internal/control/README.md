# internal/control

The daemon's HTTP and stream control plane live here.

Right now the package exposes:

- an HTTP API for health checks and session inspection
- a stream control protocol for tunnel registration and heartbeats

That gives contributors a working integration surface while leaving room for future authentication, orchestration, and transport expansion.
