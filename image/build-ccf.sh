#!/bin/sh
# image/build-ccf.sh: build the CANDIDATE image (fence image + the client layer at /opt/ccf) from a ccf tar (plan §3 C3, H2).
# It exists so the layer is delivered as a digest and checked on the way in; run it from anywhere.
#
#   image/build-ccf.sh --ccf-tar PATH [--base IMAGE] [--tag TAG]
#       PATH   a tar made by dotfiles ccf/ccf-pack: exactly the six layer files at its root, nothing else. A candidate.json beside it
#              is a CONSISTENCY check only (same producer as the tar); pass --digest sha256:HEX to pin the tar to a digest you trust
#       BASE   the fence image (default ccsbx-fence:dev); a local tag or a digest reference
#       TAG    default ccsbx-fence:ccf-<first 12 hex of the tar's sha256>
#   prints:  candidate <tag> <image id> ccf sha256:<tar digest>      on success; exit 1 and the reason otherwise
#   image/build-ccf.sh --help
# Checks after the build: the label carries the tar's digest, /opt/ccf has exactly the layer's files (+ .digest), every file is
# root-owned and not writable by the fence, no setuid/setgid file and no credential pattern (the same regex as tests/image-test.sh)
# is under /opt/ccf, and python3 -I can run the hook. Builds only; never pushes. The result is a candidate:
# `ccsbx pin-check --image <tag> --profile e2e-default` says so, and under dev-default it is refused.
set -eu
here=$(cd "$(dirname "$0")" && pwd)
base=${CCSBX_IMAGE:-ccsbx-fence:dev}; tar=""; tag=""; want_dig=""
while [ $# -gt 0 ]; do
  case "$1" in
    -h|--help) sed -n '2,13p' "$0" | sed 's/^# \{0,1\}//'; exit 0;;
    --ccf-tar) tar=$2; shift 2;;
    --base) base=$2; shift 2;;
    --tag) tag=$2; shift 2;;
    --digest) want_dig=${2#sha256:}; shift 2;;
    *) echo "image/build-ccf.sh: unknown argument '$1' (--help)" >&2; exit 2;;
  esac
done
[ -f "$tar" ] || { echo "image/build-ccf.sh: --ccf-tar PATH is required and must be a file" >&2; exit 2; }
dig=$(sha256sum "$tar" | cut -d' ' -f1)
[ -z "$want_dig" ] || [ "$want_dig" = "$dig" ] || { echo "image/build-ccf.sh: the tar is sha256:$dig, not the pinned sha256:$want_dig: not building" >&2; exit 1; }
cj=$(dirname "$tar")/candidate.json
if [ -f "$cj" ]; then
  want=$(sed -n 's/.*"digest":"sha256:\([0-9a-f]*\)".*/\1/p' "$cj" | head -1)
  [ "$want" = "$dig" ] || { echo "image/build-ccf.sh: $cj says sha256:$want, the tar is sha256:$dig: not building" >&2; exit 1; }
fi
# EXACTLY the six files, all regular and at the root: an extra json.py or sitecustomize.py would shadow the stdlib, a link or a
# directory has no place in the layer
expect="ccf-hook ccf-mcp ccf-statusline ccfreq.py managed-mcp.json managed-settings.json"
got=$(tar -tf "$tar" | sed 's#^\./##' | LC_ALL=C sort | tr '\n' ' ' | sed 's/ $//')
[ "$got" = "$expect" ] || { echo "image/build-ccf.sh: the tar holds '$got', want exactly '$expect'" >&2; exit 1; }
tar -tvf "$tar" | cut -c1 | grep -qvx -- '-' && { echo "image/build-ccf.sh: the tar has an entry that is not a regular file" >&2; exit 1; }
SECRET_RE=$(sed -n "s/^SECRET_RE='\(.*\)'\$/\1/p" "$here/../tests/image-test.sh")
[ -n "$SECRET_RE" ] || { echo "image/build-ccf.sh: cannot read SECRET_RE from tests/image-test.sh" >&2; exit 1; }
tag=${tag:-ccsbx-fence:ccf-$(printf '%s' "$dig" | cut -c1-12)}
ctx=$(mktemp -d); trap 'rm -rf "$ctx"' EXIT
cp "$tar" "$ctx/ccf.tar"; cp "$here/ccf.Dockerfile" "$ctx/Dockerfile"
existed=0; docker image inspect "$tag" >/dev/null 2>&1 && existed=1
# a refusal after the build removes the tag this run made (never one that existed before it): a refused layer must not linger as a candidate
refuse() { echo "image/build-ccf.sh: $*" >&2; [ "$existed" = 1 ] || docker rmi "$tag" >/dev/null 2>&1; exit 1; }
docker build --build-arg "BASE_IMAGE=$base" --build-arg "CCF_DIGEST=sha256:$dig" -t "$tag" "$ctx" >/dev/null
lab=$(docker inspect -f '{{ index .Config.Labels "org.ccsbx.ccf.digest" }}' "$tag")
[ "$lab" = "sha256:$dig" ] || refuse "the image's label is '$lab', not sha256:$dig"
# --user 0 and no --read-only: the scans must see the files as they are, and the writability probe must not pass only because the
# runtime root is read-only (the fence's own container is, which would hide a writable layer)
o=$(docker run --rm --user 10001 "$tag" sh -c 'ls -A /opt/ccf | tr "\n" " "; echo; touch /opt/ccf/x 2>&1 && echo WRITABLE; find /opt/ccf -not -user root | wc -l; find /opt/ccf -perm /022 | wc -l; printf "{}" | CCF_OUT=/tmp/o CCF_IN=/tmp/i /usr/bin/python3 -I /opt/ccf/ccf-hook Stop >/dev/null 2>&1; echo "hook=$?"') \
  || refuse "the checks could not run"
echo "$o" | grep -q WRITABLE && refuse "/opt/ccf is writable by the fence"
echo "$o" | grep -q 'ccf-hook' || refuse "/opt/ccf has no ccf-hook: $o"
[ "$(echo "$o" | sed -n 3p)" = 0 ] && [ "$(echo "$o" | sed -n 4p)" = 0 ] || refuse "a file in /opt/ccf is not root-owned or is group/other-writable"
sc=$(docker run --rm --user 0 "$tag" sh -c "find /opt/ccf -perm /6000 | wc -l; grep -rIlE '$SECRET_RE' /opt/ccf | wc -l") || refuse "the scans could not run"
[ "$(echo "$sc" | sed -n 1p)" = 0 ] || refuse "a setuid/setgid file under /opt/ccf"
[ "$(echo "$sc" | sed -n 2p)" = 0 ] || refuse "a credential pattern in a file under /opt/ccf"
echo "$o" | sed -n 1p | grep -qx '.digest ccf-hook ccf-mcp ccf-statusline ccfreq.py managed-mcp.json managed-settings.json ' || refuse "/opt/ccf does not hold exactly the layer: $(echo "$o" | sed -n 1p)"
echo "$o" | grep -q '^hook=0$' || refuse "ccf-hook does not run in the image (python3?): $o"
echo "candidate $tag $(docker inspect -f '{{.Id}}' "$tag") ccf sha256:$dig"
