#!/bin/sh
# Maintainer operation: release the hosted Go service.
set -eu
if [ "$#" -ne 1 ]; then
  echo 'Usage: scripts/release-controlplane.sh <image-reference>' >&2
  exit 2
fi
project_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$project_dir"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o deploy/controlplane/lambda .
cert_file=${SSL_CERT_FILE:-/etc/ssl/certs/ca-certificates.crt}
if [ ! -r "$cert_file" ]; then
  echo 'Set SSL_CERT_FILE to a readable PEM CA certificate bundle.' >&2
  exit 1
fi
cp "$cert_file" deploy/controlplane/ca-certificates.crt
go run ./cmd/release-controlplane "$1"
