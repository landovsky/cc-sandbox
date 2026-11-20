#!/bin/bash
# U1b N2 twins (direct mode, NOT EVIDENCE): a SIGKILLed `ccsbx run` leaves its container and network
# behind; `ccsbx gc` reaps them, and leaves a live run alone.
#   tests/gc-test.sh allow|block      (allow: a live run is untouched; block: the killed run's leftovers go)
#   tests/gc-test.sh --help
# Uses CCSBX_TEST_IMAGE (default python:3.12-slim, never pulled) and CCSBX_BIN (default bin/ccsbx).
set -u
case "${1:-}" in -h|--help|"") sed -n '2,7p' "$0" | sed 's/^# \{0,1\}//'; exit 0;; esac
MODE=$1; IMG=${CCSBX_TEST_IMAGE:-python:3.12-slim}; BIN=${CCSBX_BIN:-bin/ccsbx}
export CCSBX_ROOT=direct   # these twins are DIRECT-mode checks: never let the mode resolve to helper (auto) on a machine that has ccsbx-root
docker image inspect "$IMG" >/dev/null 2>&1 || { echo "FAIL: image $IMG not present locally (not pulling)"; exit 1; }
mkdir -p /var/tmp/ccsbx-hello; STATE=$(mktemp -d); PIDS=()
. "$(dirname "$0")/fence-cleanup.sh"   # cleanup: exact names only, refuses empty or foreign values
trap cleanup EXIT
fail() { echo "FAIL [gc/$MODE]: $*"; exit 1; }
C=(--state "$STATE" --machine hp-ubuntu --worktree-root /var/tmp)
mk() { sed "s/lid-hello-1/lid-gc-$1-$$/;s/cc-sandbox:hello/cc-sandbox:gc-$1-$$/" tests/specs/hello.json | "$BIN" start "${C[@]}" --launcher none --spec - --json | python3 -c 'import sys,json;print(json.load(sys.stdin)["sid"])'; }
live=$(mk live) || fail start; dead=$(mk dead) || fail start2; printf '%s\n%s\n' "$live" "$dead" > "$STATE/sids"
"$BIN" run "${C[@]}" --sid "$live" --image "$IMG" -- sleep 60 >/dev/null 2>&1 & LIVEPID=$!; PIDS+=("$LIVEPID")
"$BIN" run "${C[@]}" --sid "$dead" --image "$IMG" -- sleep 60 >/dev/null 2>&1 & DEADPID=$!
for _ in $(seq 1 50); do docker inspect "ccsbx-$live-g1" >/dev/null 2>&1 && docker inspect "ccsbx-$dead-g1" >/dev/null 2>&1 && break; sleep 0.2; done
docker inspect "ccsbx-$dead-g1" >/dev/null 2>&1 || fail "the second fence never started"
kill -9 "$DEADPID"; wait "$DEADPID" 2>/dev/null; sleep 1
docker inspect "ccsbx-$dead-g1" >/dev/null 2>&1 || fail "precondition: SIGKILL should have left the container behind"
dry=$("$BIN" gc --state "$STATE" --dry-run) || fail "dry-run failed"
echo "$dry" | grep -q "ccsbx-$dead-g1" || fail "dry-run does not list the killed run's container: $dry"
echo "$dry" | grep -q "ccsbx-$live-g1" && fail "dry-run lists a LIVE run: $dry"
docker inspect "ccsbx-$dead-g1" >/dev/null 2>&1 || fail "dry-run removed something"
"$BIN" gc --state "$STATE" || fail "gc failed"
if [ "$MODE" = allow ]; then
  docker inspect "ccsbx-$live-g1" >/dev/null 2>&1 && docker network inspect "ccsbx-net-$live-g1" >/dev/null 2>&1 || fail "gc removed a live run's container or network"
  kill -0 "$LIVEPID" 2>/dev/null || fail "the live ccsbx run died"
  echo "ok   [gc/allow]: the live run, its container and network are untouched"
else
  docker inspect "ccsbx-$dead-g1" >/dev/null 2>&1 && fail "the killed run's container survived gc"
  docker network inspect "ccsbx-net-$dead-g1" >/dev/null 2>&1 && fail "the killed run's network survived gc"
  [ -e "$STATE/s/$dead/g1/CLOSED" ] || fail "the generation was not marked CLOSED"
  echo "ok   [gc/block]: the killed run's container and network are gone, its generation is closed"
fi
