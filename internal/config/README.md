# internal/config

Configuration loading and validation for the daemon and related tools.

The initial scaffold uses JSON to keep the repository dependency-light and straightforward to audit. The package is designed so more advanced config sources can be layered in later without changing the rest of the engine shape.
