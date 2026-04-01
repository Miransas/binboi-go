# Contributing to binboi-go

Thanks for taking the time to contribute.

## Philosophy

This repository is the public engine/core for Binboi. Contributions should keep the codebase:

- readable
- modular
- production-minded
- honest about what is implemented versus still planned

## Local Development

```bash
./scripts/dev.sh
```

That script formats, vets, tests, and builds the repository.

## Project Layout

- `cmd/` contains runnable entrypoints
- `internal/` contains engine implementation details
- `pkg/` contains public packages intended for reuse
- `examples/` shows intended usage patterns
- `docs/` explains architecture and technical direction

## Contribution Guidelines

- Prefer small, reviewable pull requests
- Add or update docs when package boundaries change
- Keep package comments and README files current
- Avoid introducing unnecessary dependencies
- Do not add dashboard or SaaS product code here

## Before Opening A PR

Please make sure the following all pass:

```bash
go fmt ./...
go vet ./...
go test ./...
go build ./...
```

## Reporting Issues

When opening an issue, include:

- the use case
- expected behavior
- actual behavior
- reproduction steps
- environment details when relevant

## Attribution

Created and maintained by Sardor Azimov.
