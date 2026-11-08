#!/bin/bash
# Build OpenLinkHub from a source tree inside a Debian container (SteamOS has no C toolchain).
# Usage: build-openlinkhub.sh <source-dir> <output-binary>
set -euo pipefail
SRC=$(realpath "$1"); OUT=$(realpath -m "$2")
podman run --rm -e CGO_CFLAGS_ALLOW=-fno-strict-overflow -v "$SRC":/src:Z -v "$(dirname "$OUT")":/out:Z -v olh-gocache:/root/.cache -v olh-gomod:/go/pkg/mod \
  docker.io/library/golang:1.26.10-bookworm bash -c '
    set -e
    command -v pkg-config >/dev/null && dpkg -s libpipewire-0.3-dev >/dev/null 2>&1 || \
      (apt-get update -qq && apt-get install -y -qq libudev-dev libpipewire-0.3-dev pkg-config >/dev/null)
    cd /src && go build -buildvcs=false -o /out/'"$(basename "$OUT")"' .'
echo "Built $OUT"
