# internal/config

Configuration loading and validation for the daemon and related tools.

The initial scaffold uses JSON to keep the repository dependency-light and straightforward to audit. The control configuration now includes both the HTTP API listener and the stream protocol listener used by long-lived CLI tunnel sessions.
