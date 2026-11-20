#!/bin/sh
# image/build.sh: build the fence image from image/pins.env (plan §3 U1a). It exists so that the pins
# live in one file and reach the Dockerfile only as build args; run it from anywhere.
#
#   image/build.sh                       build ccsbx-fence:dev for this machine's architecture
#   image/build.sh --tag NAME            another tag
#   image/build.sh --platform linux/arm64   needs buildx with QEMU; not set up by this script
#   image/build.sh --check-pins          compare the Claude Code pins with the vendor's manifest.json
#                                        (a network read, no build) and exit 1 on a difference
#   image/build.sh --help
# Environment: CCSBX_IMAGE_TAG (default ccsbx-fence:dev); CCSBX_PINS_FILE (default image/pins.env;
# a test points it at a file with a wrong sha256 to prove the build fails). Builds only; never pushes.
set -eu
here=$(cd "$(dirname "$0")" && pwd)
tag=${CCSBX_IMAGE_TAG:-ccsbx-fence:dev}
pins=${CCSBX_PINS_FILE:-$here/pins.env}
platform=""
check=0
while [ $# -gt 0 ]; do
  case "$1" in
    -h|--help) sed -n '2,14p' "$0" | sed 's/^# \{0,1\}//'; exit 0;;
    --tag) tag=$2; shift 2;;
    --platform) platform=$2; shift 2;;
    --check-pins) check=1; shift;;
    *) echo "image/build.sh: unknown argument '$1' (--help)" >&2; exit 2;;
  esac
done
args=""
while IFS='=' read -r k v; do
  case "$k" in ''|\#*) continue;; esac
  args="$args --build-arg $k=$v"
done < "$pins"

if [ "$check" = 1 ]; then
  . "$pins"
  m=$(curl -fsSL "$CLAUDE_URL_BASE/$CLAUDE_VERSION/manifest.json")
  bad=0
  for pair in "linux-x64:$CLAUDE_SHA256_AMD64" "linux-arm64:$CLAUDE_SHA256_ARM64"; do
    plat=${pair%%:*}; want=${pair#*:}
    got=$(printf '%s' "$m" | jq -r --arg p "$plat" '.platforms[$p].checksum')
    [ "$got" = "$want" ] && echo "ok   $plat $want" || { echo "DIFF $plat pinned=$want manifest=$got"; bad=1; }
  done
  exit $bad
fi

# shellcheck disable=SC2086
exec docker build ${platform:+--platform "$platform"} $args -t "$tag" -f "$here/Dockerfile" "$here"
