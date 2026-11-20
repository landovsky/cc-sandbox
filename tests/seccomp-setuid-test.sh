#!/bin/sh
# seccomp-setuid-test.sh — the docker twin of the setid seccomp rule: a fence cannot set a setuid or
# setgid bit, and an ordinary chmod still works. It needs docker, so it is NOT part of `make test`;
# it runs at Gate M on hp (nothing here removes anything: containers are `docker run --rm`).
#   tests/seccomp-setuid-test.sh --base DEFAULT.json --image IMAGE [--ccsbx bin/ccsbx]
#   --base   docker's default seccomp profile for the installed docker version
#   --image  a pinned fence image (needs sh, chmod, touch)
# Exit 0: both twins hold. Exit 1: a twin failed. Exit 64: usage.
set -u
BASE=""; IMAGE=""; CCSBX="${CCSBX:-bin/ccsbx}"
while [ $# -gt 0 ]; do
  case "$1" in
    -h|--help) sed -n '2,9p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    --base) BASE="${2:?--base FILE}"; shift ;;
    --image) IMAGE="${2:?--image REF}"; shift ;;
    --ccsbx) CCSBX="${2:?--ccsbx BIN}"; shift ;;
    *) echo "seccomp-setuid-test: unknown argument $1 (--help)" >&2; exit 64 ;;
  esac
  shift
done
[ -n "$BASE" ] && [ -n "$IMAGE" ] || { echo "seccomp-setuid-test: --base and --image are required (--help)" >&2; exit 64; }
PROFILE="$(mktemp)"
"$CCSBX" seccomp-profile --base "$BASE" --out "$PROFILE" || exit 1
run() { docker run --rm --user 10001:10001 --read-only --cap-drop ALL --security-opt no-new-privileges --security-opt "seccomp=$PROFILE" --tmpfs /w:rw,mode=1777 "$IMAGE" sh -c "$1"; }
fail=0
# allow: an ordinary mode change works
run 'touch /w/a && chmod 755 /w/a && test "$(stat -c %a /w/a)" = 755' || { echo "FAIL allow: chmod 755 did not work"; fail=1; }
# block: setuid and setgid are refused, by chmod(2) and by the file's own descriptor
run 'touch /w/b; chmod 4755 /w/b 2>/dev/null; test "$(stat -c %a /w/b)" != 4755 && chmod 2755 /w/b 2>/dev/null; test "$(stat -c %a /w/b)" != 2755' || { echo "FAIL block: a setid bit was set"; fail=1; }
run 'touch /w/c; chmod u+s /w/c 2>/dev/null; test -u /w/c && exit 1; exit 0' || { echo "FAIL block: chmod u+s worked"; fail=1; }
[ "$fail" = 0 ] && echo "ok: the fence cannot set setuid/setgid bits"
exit "$fail"
