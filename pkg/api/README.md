# pkg/api

This package contains public wire types for the daemon's control API and stream protocol.

Keeping these types small and explicit makes it easier for external clients, examples, and the private Binboi product repo to share a stable contract across both HTTP and long-lived control connections.
