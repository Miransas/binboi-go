# internal/proxy

Proxy planning lives here.

The current implementation is intentionally small: it converts a normalized target into route metadata that later runtime layers can consume. This gives the rest of the repo a real shape without pretending that a full L7/L4 proxy has already been built.
