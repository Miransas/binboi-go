# internal/auth

This package owns Binboi's token-based client authentication layer.

It currently provides:

- secure token generation with one-time raw token return
- hash-only persistence
- a versioned on-disk token database
- token validation with an in-memory cache
- local CLI credential storage helpers

The goal is to keep handshake authentication real and secure without pulling in
heavier infrastructure before the rest of the product auth stack exists.
