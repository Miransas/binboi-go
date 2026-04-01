# internal/observability

Basic logging and telemetry scaffolding lives here.

The current setup standardizes on `log/slog` from the Go standard library so both local development and production-oriented deployments start from a sensible baseline without adding heavy dependencies.
