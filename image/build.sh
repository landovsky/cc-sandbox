#!/bin/sh
# image/build.sh: build the fence image from image/pins.env (plan §3 U1a). It exists so that the pins
# live in one file and reach the Dockerfile only as build args; run it from anywhere.
#
#   image/build.sh                       build ccsbx-fence:dev for this machine's architecture
#   image/build.sh --tag NAME            another tag
#   image/build.sh --platform linux/arm64   needs buildx with QEMU; not set up by this script
#   image/build.sh --check-pins          compare the Claude Code pins with the release bucket's
#                                        manifest.json and check the apt snapshot exists (network
#                                        reads, no build); exit 1 on a difference. It detects drift
#                                        of the numbers, NOT tampering at the vendor (same bucket),
#                                        and it does not check the BASE digest.
#   image/build.sh --help
# Environment: CCSBX_IMAGE_TAG (default ccsbx-fence:dev); CCSBX_PINS_FILE (default image/pins.env;
# a TEST override: a warning is printed when it is set). Builds only; never pushes.
set -eu
here=$(cd "$(dirname "$0")" && pwd)
tag=${CCSBX_IMAGE_TAG:-ccsbx-fence:dev}
pins=${CCSBX_PINS_FILE:-$here/pins.env}
[ -n "${CCSBX_PINS_FILE:-}" ] && echo "image/build.sh: WARNING: pins come from CCSBX_PINS_FILE=$pins, not image/pins.env" >&2
platform=""
check=0
while [ $# -gt 0 ]; do
  case "$1" in
    -h|--help) sed -n '2,16p' "$0" | sed 's/^# \{0,1\}//'; exit 0;;
    --tag) tag=$2; shift 2;;
    --platform) platform=$2; shift 2;;
    --check-pins) check=1; shift;;
    *) echo "image/build.sh: unknown argument '$1' (--help)" >&2; exit 2;;
  esac
done

# pin NAME: the value of NAME in the pins file (parsed, never sourced: the file is data).
pin() { sed -n "s/^$1=//p" "$pins" | tail -1; }

args=""
# `|| [ -n "$k" ]`: a last line without a trailing newline is still read.
while IFS='=' read -r k v || [ -n "$k" ]; do
  case "$k" in ''|\#*) continue;; esac
  args="$args --build-arg $k=$v"
done < "$pins"

if [ "$check" = 1 ]; then
  base=$(pin CLAUDE_URL_BASE); ver=$(pin CLAUDE_VERSION)
  m=$(curl -fsSL --proto '=https' --tlsv1.2 "$base/$ver/manifest.json")
  bad=0
  for pair in "linux-x64:$(pin CLAUDE_SHA256_AMD64)" "linux-arm64:$(pin CLAUDE_SHA256_ARM64)"; do
    plat=${pair%%:*}; want=${pair#*:}
    got=$(printf '%s' "$m" | jq -r --arg p "$plat" '.platforms[$p].checksum')
    [ "$got" = "$want" ] && echo "ok   $plat $want" || { echo "DIFF $plat pinned=$want manifest=$got"; bad=1; }
  done
  snap=$(pin SNAPSHOT)
  if curl -fsSI "http://snapshot.debian.org/archive/debian/$snap/dists/bookworm/Release" >/dev/null 2>&1; then
    echo "ok   apt snapshot $snap exists"
  else
    echo "DIFF apt snapshot $snap is not reachable"; bad=1
  fi
  exit $bad
fi

# shellcheck disable=SC2086
exec docker build ${platform:+--platform "$platform"} $args -t "$tag" -f "$here/Dockerfile" "$here"
