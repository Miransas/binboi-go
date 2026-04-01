#!/usr/bin/env sh

set -eu

echo "Formatting..."
go fmt ./...

echo "Vetting..."
go vet ./...

echo "Testing..."
go test ./...

echo "Building..."
go build ./...

echo "Development checks passed."
