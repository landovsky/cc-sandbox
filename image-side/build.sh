#!/usr/bin/env bash
# image-side/build.sh — builds the sidecar image `ccsbx-side:dev` locally for this machine's architecture.
# Why: the same Dockerfile and the same binary build as CI (.github/workflows/side-image.yml), so the image tests
# (tests/side-image-test.sh) can run before a push. Only the registry push and the second architecture are CI's.
#   image-side/build.sh [--help]      GO=/path/to/go to choose the toolchain (default $HOME/.local/go/bin/go), TAG=name:tag
set -euo pipefail
case "${1:-}" in -h|--help) sed -n '2,5p' "$0" | sed 's/^# \{0,1\}//'; exit 0;; esac
here="$(cd "$(dirname "$0")/.." && pwd)"
GO="${GO:-$HOME/.local/go/bin/go}"; TAG="${TAG:-ccsbx-side:dev}"
arch="$("$GO" env GOARCH)"
mkdir -p "$here/image-side/bin"
(cd "$here" && CGO_ENABLED=0 GOOS=linux GOARCH="$arch" "$GO" build -trimpath -buildvcs=false -ldflags "-s -w -X main.version=side-dev" -o "image-side/bin/ccsbx-linux-$arch" ./cmd/ccsbx)
docker build --build-arg "TARGETARCH=$arch" -t "$TAG" "$here/image-side"
echo "built $TAG ($arch); test it: CCSBX_TEST_IMAGE=$TAG tests/side-image-test.sh all"
