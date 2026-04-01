# internal/control

The daemon's HTTP control plane lives here.

Right now the package exposes a small, real API for health checks and session creation/listing. That gives contributors a working integration surface while leaving room for future authentication, orchestration, and transport expansion.
