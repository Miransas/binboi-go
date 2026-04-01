# docker-basic

This example shows a lightweight Docker Compose setup for local experimentation.

It runs:

- `binboid` from the current repository using the Go toolchain image
- a tiny echo upstream service

## Start The Stack

```bash
docker compose -f ./examples/docker-basic/compose.yaml up
```

## Create A Session

From the repository root:

```bash
go run ./cmd/binboi session create \
  -server http://127.0.0.1:8080 \
  -name docker-echo \
  -target http://echo:5678
```

The example is intentionally simple and aimed at demonstrating the intended control-plane shape rather than shipping a full container distribution story.
