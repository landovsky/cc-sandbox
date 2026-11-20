#!/bin/sh
# image/build-ccf.sh: build the CANDIDATE image (fence image + the client layer at /opt/ccf) from a ccf tar (plan §3 C3, H2).
# It exists so the layer is delivered as a digest and checked on the way in; run it from anywhere.
#
#   image/build-ccf.sh --ccf-tar PATH [--base IMAGE] [--tag TAG]
#       PATH   a tar made by dotfiles ccf/ccf-pack (with its candidate.json beside it, whose digest must match the tar)
#       BASE   the fence image (default ccsbx-fence:dev); a local tag or a digest reference
#       TAG    default ccsbx-fence:ccf-<first 12 hex of the tar's sha256>
#   prints:  candidate <tag> <image id> ccf sha256:<tar digest>      on success; exit 1 and the reason otherwise
#   image/build-ccf.sh --help
# Checks after the build: the label carries the tar's digest, /opt/ccf has the layer's files, every file is root-owned and
# not writable by the fence (touch /opt/ccf/x fails), and python3 can run the hook. Builds only; never pushes. The result is a
# candidate: `ccsbx pin-check --image <tag> --profile e2e-default` says so, and under dev-default it is refused.
set -eu
here=$(cd "$(dirname "$0")" && pwd)
base=${CCSBX_IMAGE:-ccsbx-fence:dev}; tar=""; tag=""
while [ $# -gt 0 ]; do
  case "$1" in
    -h|--help) sed -n '2,13p' "$0" | sed 's/^# \{0,1\}//'; exit 0;;
    --ccf-tar) tar=$2; shift 2;;
    --base) base=$2; shift 2;;
    --tag) tag=$2; shift 2;;
    *) echo "image/build-ccf.sh: unknown argument '$1' (--help)" >&2; exit 2;;
  esac
done
[ -f "$tar" ] || { echo "image/build-ccf.sh: --ccf-tar PATH is required and must be a file" >&2; exit 2; }
dig=$(sha256sum "$tar" | cut -d' ' -f1)
cj=$(dirname "$tar")/candidate.json
if [ -f "$cj" ]; then
  want=$(sed -n 's/.*"digest":"sha256:\([0-9a-f]*\)".*/\1/p' "$cj" | head -1)
  [ "$want" = "$dig" ] || { echo "image/build-ccf.sh: $cj says sha256:$want, the tar is sha256:$dig: not building" >&2; exit 1; }
fi
for f in ccf-hook ccf-statusline ccf-mcp ccfreq.py managed-settings.json managed-mcp.json; do
  tar -tf "$tar" | grep -qx "$f" || { echo "image/build-ccf.sh: the tar has no $f at its root" >&2; exit 1; }
done
tag=${tag:-ccsbx-fence:ccf-$(printf '%s' "$dig" | cut -c1-12)}
ctx=$(mktemp -d); trap 'rm -rf "$ctx"' EXIT
cp "$tar" "$ctx/ccf.tar"; cp "$here/ccf.Dockerfile" "$ctx/Dockerfile"
docker build --build-arg "BASE_IMAGE=$base" --build-arg CCF_TAR=ccf.tar --build-arg "CCF_DIGEST=sha256:$dig" -t "$tag" "$ctx" >/dev/null
lab=$(docker inspect -f '{{ index .Config.Labels "org.ccsbx.ccf.digest" }}' "$tag")
[ "$lab" = "sha256:$dig" ] || { echo "image/build-ccf.sh: the image's label is '$lab', not sha256:$dig" >&2; exit 1; }
o=$(docker run --rm "$tag" sh -c 'ls /opt/ccf | tr "\n" " "; echo; touch /opt/ccf/x 2>&1 && echo WRITABLE; find /opt/ccf -not -user root | wc -l; find /opt/ccf -perm /022 | wc -l; printf "{}" | CCF_OUT=/tmp/o CCF_IN=/tmp/i /opt/ccf/ccf-hook Stop >/dev/null 2>&1; echo "hook=$?"') \
  || { echo "image/build-ccf.sh: the checks could not run" >&2; exit 1; }
echo "$o" | grep -q WRITABLE && { echo "image/build-ccf.sh: /opt/ccf is writable by the fence" >&2; exit 1; }
echo "$o" | grep -q 'ccf-hook' || { echo "image/build-ccf.sh: /opt/ccf has no ccf-hook: $o" >&2; exit 1; }
[ "$(echo "$o" | sed -n 3p)" = 0 ] && [ "$(echo "$o" | sed -n 4p)" = 0 ] || { echo "image/build-ccf.sh: a file in /opt/ccf is not root-owned or is group/other-writable" >&2; exit 1; }
echo "$o" | grep -q '^hook=0$' || { echo "image/build-ccf.sh: ccf-hook does not run in the image (python3?): $o" >&2; exit 1; }
echo "candidate $tag $(docker inspect -f '{{.Id}}' "$tag") ccf sha256:$dig"
