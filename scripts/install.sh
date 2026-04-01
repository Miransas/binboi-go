#!/usr/bin/env sh

set -eu

echo "Installing binboi and binboid..."
go install ./cmd/binboi
go install ./cmd/binboid
echo "Installed successfully."
