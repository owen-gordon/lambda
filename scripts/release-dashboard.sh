#!/bin/sh
set -eu
if [ "$#" -ne 1 ]; then
  echo 'Usage: scripts/release-dashboard.sh <image-reference>' >&2
  exit 2
fi
project_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$project_dir"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o deploy/dashboard/dashboard ./cmd/dashboard
go run ./cmd/release-dashboard "$1"
