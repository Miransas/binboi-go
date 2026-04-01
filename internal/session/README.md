# internal/session

Session lifecycle coordination lives here.

The manager validates control-plane requests, normalizes targets, prepares route metadata, and returns session records. Sessions are currently stored in memory so the daemon remains easy to understand while the public API shape settles.
