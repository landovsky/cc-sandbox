#!/bin/bash
# Docker-backend twins for tests/rules.toml (U1b). Direct mode: results are NOT EVIDENCE.
#
#   tests/fence-test.sh <rule> <allow|block>
#   rules: fence-flags no-internet no-dns-exfil outbox-per-gen
#   tests/fence-test.sh --help
#
# Environment (all optional):
#   CCSBX_TEST_IMAGE  image to run in the fence; needs python3 (default python:3.12-slim, a
#                     stand-in until the real fence image U1a exists). Never pulled: if it is
#                     missing the test fails rather than hitting a registry.
#   CCSBX_BIN         the ccsbx binary (default bin/ccsbx; run `make build` first)
# Each run uses a scratch state dir and removes only the containers and networks it created
# (names ccsbx-<its sid>-g1 / ccsbx-net-<its sid>-g1 / ccsbx-canary-<pid>).
set -u
case "${1:-}" in -h|--help|"") sed -n '2,15p' "$0" | sed 's/^# \{0,1\}//'; exit 0;; esac
RULE=$1 TWIN=${2:-}
IMG=${CCSBX_TEST_IMAGE:-python:3.12-slim}
BIN=${CCSBX_BIN:-bin/ccsbx}
[ -x "$BIN" ] || { echo "FAIL: $BIN missing (make build)"; exit 1; }
docker image inspect "$IMG" >/dev/null 2>&1 || { echo "FAIL: image $IMG not present locally (not pulling)"; exit 1; }

STATE=$(mktemp -d)
mkdir -p /var/tmp
PIDS=()
cleanup() {
  docker rm -f "ccsbx-canary-$$" >/dev/null 2>&1   # first: a connected peer blocks the network removal
  for p in "${PIDS[@]:-}"; do [ -n "$p" ] && kill "$p" 2>/dev/null; done
  wait 2>/dev/null   # let ccsbx run finish its own cleanup before we sweep
  for s in $(cat "$STATE/sids" 2>/dev/null); do
    docker rm -f "ccsbx-$s-g1" >/dev/null 2>&1
    docker network rm "ccsbx-net-$s-g1" >/dev/null 2>&1
    docker network inspect "ccsbx-net-$s-g1" >/dev/null 2>&1 && echo "WARN: network ccsbx-net-$s-g1 left behind" >&2
  done
  rm -rf "$STATE"
}
trap cleanup EXIT

fail() { echo "FAIL [$RULE/$TWIN]: $*"; exit 1; }
ok()   { echo "ok   [$RULE/$TWIN]: $*"; }

# new_session <label>: record a session without a pane; prints the sid.
new_session() {
  local spec; spec=$(sed "s/lid-hello-1/lid-$1-$$/;s/cc-sandbox:hello/cc-sandbox:$1-$$/;s/\"billing\":\"stub\"/\"billing\":\"stub\"/" tests/specs/hello.json)
  local sid
  sid=$(printf '%s' "$spec" | "$BIN" start --state "$STATE" --machine hp-ubuntu --launcher none --spec - --json | python3 -c 'import sys,json;print(json.load(sys.stdin)["sid"])') || return 1
  echo "$sid" >> "$STATE/sids"; echo "$sid"
}
# fence <sid> <cmd...>: run in the foreground, output on stdout.
fence() { local sid=$1; shift; "$BIN" run --state "$STATE" --machine hp-ubuntu --sid "$sid" --image "$IMG" -- "$@" 2>/dev/null; }
# fence_bg <sid> <cmd...>: run in the background and wait for its container to exist.
fence_bg() {
  local sid=$1; shift
  "$BIN" run --state "$STATE" --machine hp-ubuntu --sid "$sid" --image "$IMG" -- "$@" >/dev/null 2>&1 &
  PIDS+=("$!")
  for _ in $(seq 1 50); do docker inspect "ccsbx-$sid-g1" >/dev/null 2>&1 && return 0; sleep 0.2; done
  fail "container ccsbx-$sid-g1 never appeared"
}
PY='python3 -c'

case "$RULE/$TWIN" in
fence-flags/allow)
  # The fence must still do ordinary work: run as 10001, write /tmp and out/, read in/ and managed settings.
  sid=$(new_session ff) || fail "start"
  out=$(fence "$sid" sh -c 'id -u; touch /tmp/w && echo tmp; touch /run/ccsbx/out/x && echo out; cat /run/ccsbx/in/session.json >/dev/null && echo in; test -f /etc/claude-code/managed-settings.json && echo managed') || fail "run failed: $out"
  for want in 10001 tmp out in managed; do echo "$out" | grep -qx "$want" || fail "missing '$want' in: $out"; done
  ok "uid 10001, /tmp and out/ writable, in/ and managed settings readable";;

fence-flags/block)
  sid=$(new_session fb) || fail "start"
  out=$(fence "$sid" sh -c 'touch /x 2>&1; touch /run/ccsbx/in/x 2>&1; touch /etc/claude-code/x 2>&1; grep -E "^(CapEff|NoNewPrivs)" /proc/self/status; env') || fail "run failed"
  echo "$out" | grep -c 'Read-only file system' | grep -qx 3 || fail "root, in/ and managed are not all read-only: $out"
  echo "$out" | grep -q 'CapEff:.0000000000000000' || fail "capabilities not dropped"
  echo "$out" | grep -q 'NoNewPrivs:.1' || fail "no-new-privileges not set"
  for v in USER SSH_AUTH_SOCK ANTHROPIC_API_KEY GITHUB_TOKEN DISPLAY XDG_RUNTIME_DIR; do echo "$out" | grep -q "^$v=" && fail "host env $v leaked"; done
  # Live container, as docker reports it (a new session: a generation runs only once).
  sid=$(new_session fb2) || fail "start 2"
  fence_bg "$sid" sleep 30
  ins=$(docker inspect "ccsbx-$sid-g1" --format '{{.HostConfig.ReadonlyRootfs}} {{.HostConfig.PidsLimit}} {{.HostConfig.Memory}} {{.HostConfig.Privileged}} {{.Config.User}} {{json .HostConfig.CapDrop}} {{json .HostConfig.SecurityOpt}} {{.HostConfig.NetworkMode}}')
  echo "$ins" | grep -q '^true 512 4294967296 false 10001:10001 \["ALL"\] \["no-new-privileges"\] ccsbx-net-' || fail "inspect: $ins"
  ok "read-only mounts, no caps, no-new-privs, no host env; inspect: $ins";;

no-internet/allow)
  # The fence can reach a peer on its own internal network (the forwarder will be one).
  sid=$(new_session ni) || fail "start"
  fence_bg "$sid" sleep 40
  net=ccsbx-net-$sid-g1
  docker run -d --name "ccsbx-canary-$$" "$IMG" python3 -m http.server 8000 >/dev/null || fail "peer start"
  docker network connect "$net" "ccsbx-canary-$$" || fail "connect peer"
  ip=$(docker inspect "ccsbx-canary-$$" --format "{{(index .NetworkSettings.Networks \"$net\").IPAddress}}")
  sleep 1
  docker exec "ccsbx-$sid-g1" $PY "import socket;socket.create_connection(('$ip',8000),3).close();print('reached')" 2>&1 | grep -q reached || fail "peer $ip:8000 not reachable from the fence"
  ok "fence reaches a peer on its own network ($ip)";;

no-internet/block)
  sid=$(new_session nib) || fail "start"
  out=$(fence "$sid" $PY "
import socket
for host in ('1.1.1.1','8.8.8.8','172.17.0.1','100.100.100.100'):
    try:
        socket.create_connection((host,443),3).close(); print('REACHED',host)
    except OSError as e: print('blocked',host)
print('default-route', any(l.split()[1]=='00000000' for l in open('/proc/net/route').read().splitlines()[1:]))
") || fail "run failed"
  echo "$out" | grep -q REACHED && fail "reached the outside: $out"
  echo "$out" | grep -q 'default-route False' || fail "a default route exists: $out"
  ok "no route out, nothing reachable: $(echo $out | tr '\n' ' ')";;

no-dns-exfil/allow)
  # Names on the fence's own network still resolve through docker's embedded DNS: the forwarder is addressed as 'fwd'.
  sid=$(new_session nd) || fail "start"
  fence_bg "$sid" sleep 40
  net=ccsbx-net-$sid-g1
  docker run -d --name "ccsbx-canary-$$" "$IMG" python3 -m http.server 8000 >/dev/null || fail "peer start"
  docker network connect --alias fwd "$net" "ccsbx-canary-$$" || fail "connect peer"
  sleep 1
  docker exec "ccsbx-$sid-g1" $PY "import socket;print('resolved',socket.gethostbyname('fwd'))" 2>&1 | grep -q resolved || fail "name 'fwd' does not resolve on the fence network"
  ok "peer name resolves on the fence network";;

no-dns-exfil/block)
  # A canary resolver sits on the fence's network and logs every query it gets. The fence looks up
  # unique names for external domains; the canary's log must stay empty and the lookups must fail.
  sid=$(new_session ndb) || fail "start"
  fence_bg "$sid" sleep 40
  net=ccsbx-net-$sid-g1
  docker run -d --name "ccsbx-canary-$$" "$IMG" python3 -u -c "
import socket
s=socket.socket(socket.AF_INET,socket.SOCK_DGRAM); s.bind(('0.0.0.0',53))
while True:
    d,a=s.recvfrom(512); print('QUERY',a,d,flush=True)
" >/dev/null || fail "canary start"
  docker network connect "$net" "ccsbx-canary-$$" || fail "connect canary"
  sleep 1
  res=$(docker exec "ccsbx-$sid-g1" $PY "
import socket
for n in ('exfil-$$.canary.invalid','secret-data-$$.example.com'):
    try: print('RESOLVED',n,socket.gethostbyname(n))
    except OSError: print('failed',n)
") || fail "exec failed"
  echo "$res" | grep -q RESOLVED && fail "external name resolved: $res"
  log=$(docker logs "ccsbx-canary-$$" 2>&1)
  [ -z "$log" ] || fail "canary saw queries: $log"
  grep -q 'nameserver 127.0.0.1\|nameserver 127.0.0.11' <(docker exec "ccsbx-$sid-g1" cat /etc/resolv.conf) || fail "resolv.conf unexpected: $(docker exec ccsbx-$sid-g1 cat /etc/resolv.conf)"
  ok "lookups failed, canary log empty";;

outbox-per-gen/allow)
  sid=$(new_session og) || fail "start"
  fence "$sid" sh -c 'echo hook > /run/ccsbx/out/events.jsonl' || fail "run"
  [ "$(cat "$STATE/s/$sid/g1/out/events.jsonl" 2>/dev/null)" = hook ] || fail "the host does not see the fence's outbox write"
  [ -e "$STATE/s/$sid/g1/CLOSED" ] || fail "generation not marked closed after exit"
  ok "write in the fence appears in g1/out on the host; g1 closed at exit";;

outbox-per-gen/block)
  # One outbox per generation, never reused: the same (sid, gen) can't be run twice, a new session's
  # out/ starts empty, and the fence sees only its own generation's out/ and in/.
  sid=$(new_session ob) || fail "start"
  fence "$sid" sh -c 'echo secret > /run/ccsbx/out/events.jsonl' || fail "first run"
  fence "$sid" true && fail "the same generation ran twice, sharing its outbox"
  [ "$(cat "$STATE/s/$sid/g1/out/events.jsonl")" = secret ] || fail "g1's outbox was disturbed by the refused rerun"
  sid2=$(new_session ob2) || fail "start 2"
  seen=$(fence "$sid2" sh -c 'ls -A /run/ccsbx/out | wc -l; ls /run/ccsbx; find / -xdev -name events.jsonl 2>/dev/null | grep -v "^/run/ccsbx/out" | wc -l') || fail "run 2"
  echo "$seen" | head -1 | grep -qx 0 || fail "a new session's out/ is not empty: $seen"
  echo "$seen" | tail -1 | grep -qx 0 || fail "another outbox is visible in the fence: $seen"
  ok "per-generation outbox, rerun refused, new session starts empty";;

*) echo "unknown rule/twin: $RULE/$TWIN"; exit 2;;
esac
