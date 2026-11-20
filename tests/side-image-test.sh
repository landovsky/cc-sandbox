#!/bin/bash
# tests/side-image-test.sh: twins for the sidecar image `ccsbx` (cc-sandbox:#3wsr), run by CI on the PUSHED digest.
# Why: the sidecar holds the pod's only credential, so what the image contains is the promise: one static binary,
# no shell, uid/gid 10002, no setuid bit, serving on a read-only root with no network. These checks read the BUILT
# image, and each block twin first builds a deliberately bad image from the good one and requires its check to go red.
# Direct mode: results are NOT EVIDENCE. Only `docker build`, `docker run --rm` and read-only `docker image/save` are
# used; nothing is removed (the bad images are untagged and stay as dangling images).
#
#   tests/side-image-test.sh <check|all> [allow|block]
#   checks: side-user side-no-shell side-no-setuid side-runs   (all = every check, allow then block)
#   tests/side-image-test.sh --help
#
# Environment: CCSBX_TEST_IMAGE (default ccsbx-side:dev; build it with image-side/build.sh).
set -u
case "${1:-}" in -h|--help|"") sed -n '2,15p' "$0" | sed 's/^# \{0,1\}//'; exit 0;; esac
RULE=$1 TWIN=${2:-}
IMG=${CCSBX_TEST_IMAGE:-ccsbx-side:dev}
docker image inspect "$IMG" >/dev/null 2>&1 || { echo "FAIL: image $IMG not present (image-side/build.sh)"; exit 1; }
WORK=$(mktemp -d); trap 'rm -rf "$WORK"' EXIT
fail() { echo "FAIL [$RULE/$TWIN]: $*"; exit 1; }
ok()   { echo "ok   [$RULE/$TWIN]: $*"; }

# layer_entries IMAGE: "mode name" for every entry of every layer, from `docker save` (no container is created)
layer_entries() {
  docker save "$1" > "$WORK/save.tar" || return 1
  python3 -I - "$WORK/save.tar" <<'PY'
import sys, tarfile, io
outer = tarfile.open(sys.argv[1])
layers = 0
for m in outer.getmembers():
    if not m.isfile() or m.size == 0: continue
    data = outer.extractfile(m).read()
    try: inner = tarfile.open(fileobj=io.BytesIO(data))
    except Exception: continue          # a config or manifest json, not a layer
    for e in inner.getmembers():
        if e.name.startswith(".wh."): continue
        print("%04o %s" % (e.mode & 0o7777, e.name.lstrip("./")))
        layers += 1
if layers == 0:
    sys.stderr.write("cannot read any layer from `docker save` (an OCI layout with compressed layers, or the containerd image store?): this is a limit of the test, not a finding about the image\n")
    sys.exit(3)
PY
}

check_user()    { local u e; u=$(docker image inspect -f '{{.Config.User}}' "$1") || return 1
                  [ "$u" = "10002:10002" ] || { echo "image user is '$u', want 10002:10002"; return 1; }
                  e=$(docker image inspect -f '{{json .Config.Entrypoint}}' "$1")
                  [ "$e" = '["/usr/local/bin/ccsbx","side"]' ] || { echo "entrypoint is $e"; return 1; }; }
check_noshell() { local l; l=$(layer_entries "$1") || return 1
                  echo "$l" | grep -Ev ' (usr|usr/local|usr/local/bin)/?$' | grep -qv ' usr/local/bin/ccsbx$' \
                    && { echo "the image holds more than the binary: $(echo "$l" | grep -Ev ' (usr|usr/local|usr/local/bin)/?$' | grep -v ' usr/local/bin/ccsbx$' | head -3 | tr '\n' ';')"; return 1; }
                  echo "$l" | grep -q ' usr/local/bin/ccsbx$' || { echo "no /usr/local/bin/ccsbx"; return 1; }
                  if docker run --rm --entrypoint /bin/sh "$1" -c true >/dev/null 2>&1; then echo "/bin/sh runs"; return 1; fi; }
check_setuid()  { local l; l=$(layer_entries "$1") || return 1
                  echo "$l" | awk '{ if (substr($1,1,1) ~ /[2-7]/) bad=bad" "$2 } END { if (bad) { print "setuid/setgid/sticky bit on:" bad; exit 1 } }'; }
# check_runs: the pod's real conditions. A read-only root, no network, no capabilities, uid 10002, and ONLY the side-state
# volume writable (a tmpfs at the path the pod mounts). `side serve` must still be up after 4 s (timeout ends it: rc 124).
# An image that needs its own root fs to be writable (e.g. a control socket path outside the volume) exits at once.
check_runs()    { local rc out
                  out=$(timeout 4 docker run --rm --read-only --network none --user 10002:10002 --cap-drop ALL \
                        --tmpfs /run/ccsbx/side:uid=10002,gid=10002,mode=0777 --entrypoint /usr/local/bin/ccsbx \
                        "$1" side serve --door m=http://127.0.0.1:9 2>&1); rc=$?
                  [ "$rc" -eq 124 ] || { echo "side serve did not stay up on a read-only root (exit $rc): $(echo "$out" | head -1)"; return 1; }; }

# build_bad <docker build args...> < Dockerfile text, @IMG@ standing for the image under test; prints the image id
build_bad() { local id; id=$(sed "s#@IMG@#$IMG#g" | docker build -q "$@" - 2>"$WORK/bb.err") || { cat "$WORK/bb.err" >&2; return 1; }; echo "$id"; }
must_be_red() { local what=$1 bad=$2 fn=$3 out
  [ -n "$bad" ] || fail "could not build the bad image for: $what"
  if out=$($fn "$bad" 2>&1); then fail "the check stayed green on a bad image ($what): the twin cannot go red"; fi
  echo "  red as it must on: $what ($(echo "$out" | head -1))"; }
must_be_green() { local out; out=$($2 "$IMG" 2>&1) || fail "$1: $out"; ok "$1"; }

run_check() {   # run_check NAME TWIN
  local n=$1 t=$2 bad
  TWIN=$t
  case "$n/$t" in
    side-user/allow)       must_be_green "runs as 10002:10002, entrypoint is 'ccsbx side'" check_user ;;
    side-user/block)       bad=$(printf 'FROM @IMG@\nUSER 0\n' | build_bad); must_be_red "USER 0" "$bad" check_user ;;
    side-no-shell/allow)   must_be_green "the image is the binary alone and has no /bin/sh" check_noshell ;;
    side-no-shell/block)   bad=$(printf 'FROM @IMG@\nCOPY --from=@IMG@ /usr/local/bin/ccsbx /bin/sh\n' | build_bad); must_be_red "a /bin/sh added" "$bad" check_noshell ;;
    side-no-setuid/allow)  must_be_green "no setuid, setgid or sticky entry" check_setuid ;;
    side-no-setuid/block)  bad=$(printf 'FROM @IMG@\nCOPY --chmod=4755 --from=@IMG@ /usr/local/bin/ccsbx /usr/local/bin/x\n' | build_bad); must_be_red "a setuid file" "$bad" check_setuid ;;
    side-runs/allow)       must_be_green "side serve stays up as 10002 on a read-only root (only the side-state volume writable), no network, no capabilities" check_runs ;;
    side-runs/block)       bad=$(printf 'FROM @IMG@\nENV CCSBX_SIDE_CONTROL=/ctl/ctl.sock\n' | build_bad); must_be_red "a control socket on the image's own root fs" "$bad" check_runs ;;
    *) fail "unknown check/twin '$n/$t' (side-user side-no-shell side-no-setuid side-runs; allow|block)" ;;
  esac
}
if [ "$RULE" = all ]; then
  for c in side-user side-no-shell side-no-setuid side-runs; do for t in allow block; do RULE=$c; run_check $c $t; done; done
else run_check "$RULE" "$TWIN"; fi
