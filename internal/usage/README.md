# internal/usage

This package owns lightweight usage accounting for Binboi.

It keeps hot-path counters in memory, periodically flushes durable snapshots to disk, and enforces simple plan limits such as:

- requests per period
- bytes in per period
- bytes out per period
- active tunnels per user

The current implementation is intentionally small and file-backed so the engine stays easy to audit and extend.
