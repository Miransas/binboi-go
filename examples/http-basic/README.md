# http-basic

This example runs a tiny upstream HTTP service on port `3000`.

## Run The Example Service

```bash
go run ./examples/http-basic
```

## Run The Daemon

From the repository root in another terminal:

```bash
go run ./cmd/binboid
```

## Create A Session

```bash
go run ./cmd/binboi session create \
  -name http-basic \
  -target http://127.0.0.1:3000
```

The returned session record shows the intended control-plane flow and the generated public URL placeholder.
