#!/bin/bash
# Docker-backend twins for tests/rules.toml (U1b). Direct mode: results are NOT EVIDENCE.
#
#   tests/fence-test.sh <rule> <allow|block>
#   rules: fence-flags no-internet no-dns-exfil outbox-per-gen no-token-in-fence
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
mkdir -p /var/tmp/ccsbx-hello
PIDS=()
# Files the fence wrote (uid 10001) can't be removed by the host user: sweep them from a container.
rmstate() { docker run --rm --user 0 -v "$STATE:/s" "$IMG" sh -c 'rm -rf /s/* /s/.[!.]*' >/dev/null 2>&1; rm -rf "$STATE"; }
cleanup() {
  docker rm -f "ccsbx-fwd-$$" >/dev/null 2>&1
  docker rm -f "ccsbx-canary-$$" >/dev/null 2>&1   # first: a connected peer blocks the network removal
  for p in "${PIDS[@]:-}"; do [ -n "$p" ] && kill "$p" 2>/dev/null; done
  wait 2>/dev/null   # let ccsbx run finish its own cleanup before we sweep
  for s in $(cat "$STATE/sids" 2>/dev/null); do
    docker rm -f "ccsbx-$s-g1" >/dev/null 2>&1
    docker network rm "ccsbx-net-$s-g1" >/dev/null 2>&1
    docker network inspect "ccsbx-net-$s-g1" >/dev/null 2>&1 && echo "WARN: network ccsbx-net-$s-g1 left behind" >&2
  done
  rmstate
}
trap cleanup EXIT

fail() { echo "FAIL [$RULE/$TWIN]: $*"; exit 1; }
ok()   { echo "ok   [$RULE/$TWIN]: $*"; }

# new_session <label>: record a session without a pane; prints the sid.
new_session() {
  local spec; spec=$(sed "s/lid-hello-1/lid-$1-$$/;s/cc-sandbox:hello/cc-sandbox:$1-$$/;s/\"billing\":\"stub\"/\"billing\":\"stub\"/" tests/specs/hello.json)
  local sid
  sid=$(printf '%s' "$spec" | "$BIN" start --state "$STATE" --machine hp-ubuntu --worktree-root /var/tmp --launcher none --spec - --json | python3 -c 'import sys,json;print(json.load(sys.stdin)["sid"])') || return 1
  echo "$sid" >> "$STATE/sids"; echo "$sid"
}
# fence <sid> <cmd...>: run in the foreground, output on stdout.
fence() { local sid=$1; shift; "$BIN" run --state "$STATE" --machine hp-ubuntu --worktree-root /var/tmp --sid "$sid" --image "$IMG" -- "$@" 2>/dev/null; }
# fence_bg <sid> <cmd...>: run in the background and wait for its container to exist.
fence_bg() {
  local sid=$1; shift
  "$BIN" run --state "$STATE" --machine hp-ubuntu --worktree-root /var/tmp --sid "$sid" --image "$IMG" -- "$@" >/dev/null 2>&1 &
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
  # Public addresses, the default bridge, AND the host's own address on the fence's internal bridge:
  # that one is on-link, so "no default route" doesn't cover it. Every port a host service listens
  # on at the wildcard address is probed there, plus the ones the platform will use (22, 7480, 4011).
  sid=$(new_session nib) || fail "start"
  fence_bg "$sid" sleep 40
  net=ccsbx-net-$sid-g1
  # the host's address on the bridge is the first address of the subnet (with inhibit_ipv4 docker
  # reports no gateway, but a host that did have an address would have this one)
  subnet=$(docker network inspect "$net" --format '{{(index .IPAM.Config 0).Subnet}}')
  gw=$(python3 -c "import ipaddress,sys;print(next(ipaddress.ip_network(sys.argv[1]).hosts()))" "$subnet") || fail "no subnet for $net"
  ports=$( { ss -ltnH 2>/dev/null | awk '{print $4}' | grep -E '^(0\.0\.0\.0|\[::\]|\*):[0-9]+$' | sed 's/.*://' ; echo 22; echo 7480; echo 4011; } | sort -un | tr '\n' ',')
  out=$(docker exec "ccsbx-$sid-g1" $PY "
import socket
for host in ('1.1.1.1','8.8.8.8','172.17.0.1','100.100.100.100'):
    try:
        socket.create_connection((host,443),3).close(); print('REACHED',host)
    except OSError as e: print('blocked',host)
reached=[]
for port in [int(p) for p in '$ports'.split(',') if p]:
    try:
        socket.create_connection(('$gw',port),1.5).close(); reached.append(port)
    except OSError: pass
print('gateway-$gw-reachable-ports', reached)
print('default-route', any(l.split()[1]=='00000000' for l in open('/proc/net/route').read().splitlines()[1:]))
") || fail "exec failed"
  echo "$out" | grep -q REACHED && fail "reached the outside: $out"
  echo "$out" | grep -q 'default-route False' || fail "a default route exists: $out"
  echo "$out" | grep -q "reachable-ports \[\]" || fail "the host answers on the fence's own bridge address: $out"
  ok "no route out, host's bridge address $gw answers on none of: $ports";;

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
  # The canary can't see a mutation that removes --dns (docker's internal networks don't forward at
  # all on a fixed engine), so assert the flag and the engine version directly.
  dns=$(docker inspect "ccsbx-$sid-g1" --format '{{json .HostConfig.Dns}}')
  [ "$dns" = '["127.0.0.1"]' ] || fail "the fence's --dns is $dns, want [\"127.0.0.1\"] (no resolver)"
  major=$(docker version --format '{{.Server.Version}}' | cut -d. -f1)
  [ "${major:-0}" -ge 26 ] || fail "docker engine $major.x < 26: CVE-2024-29018 let internal networks forward DNS queries out"
  ok "lookups failed, canary log empty, --dns 127.0.0.1, engine >= 26";;

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

no-token-in-fence/allow|no-token-in-fence/block)
  # The forwarder (with a loopback stub door behind it, local mode) runs as a peer on the fence's
  # own network under the alias 'fwd', fed its token and key on STDIN. The fence holds neither.
  sid=$(new_session nt) || fail "start"
  fence_bg "$sid" sleep 60
  net=ccsbx-net-$sid-g1
  [ "$RULE" = no-token-in-fence ] || fail "internal"
  BINABS=$(readlink -f "$BIN")
  "$BIN" token mint --key-file "$STATE/dev.key" --sub "tomas/hp-ubuntu/$sid/g1" --label nt --billing stub --profile e2e-test \
      --aud ccsbx-model --fwd-secrets > "$STATE/secrets.json" || fail "mint"
  chmod 0600 "$STATE/dev.key"
  "$BIN" token pubkey --key-file "$STATE/dev.key" --owners tomas --machines hp-ubuntu > "$STATE/keyring" || fail "pubkey"
  docker run --rm -i --name "ccsbx-fwd-$$" --network "$net" --network-alias fwd \
      -v "$BINABS:/ccsbx:ro" -v "$STATE/keyring:/keyring:ro" "$IMG" sh -c \
      '/ccsbx stub --listen 127.0.0.1:8081 --keyring /keyring & exec /ccsbx fwd --listen :8080 --door model=http://127.0.0.1:8081' \
      < "$STATE/secrets.json" >"$STATE/fwd.log" 2>&1 &
  PIDS+=("$!")
  call="import urllib.request,json
r=urllib.request.Request('http://fwd:8080/model/v1/messages',data=json.dumps({'model':'m','messages':[{'role':'user','content':'E2E:hi'}]}).encode(),headers={'content-type':'application/json'})
print(urllib.request.urlopen(r,timeout=5).read().decode())"
  got=""
  for _ in $(seq 1 25); do got=$(docker exec "ccsbx-$sid-g1" $PY "$call" 2>&1) && break; sleep 0.4; done
  if [ "$TWIN" = allow ]; then
    echo "$got" | grep -q STUB-OK || fail "the fence could not use the model door through fwd: $got | fwd.log: $(cat "$STATE/fwd.log")"
    ok "no-credential call through fwd reached the stub door: STUB-OK"
  else
    echo "$got" | grep -q STUB-OK || fail "precondition: fwd path not working, the scan would be vacuous: $got"
    tok=$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["token"])' "$STATE/secrets.json")
    key=$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["key"])' "$STATE/secrets.json")
    # Everything the fence can read: every process's environ and cmdline, its env, and every
    # readable regular file on EVERY mount the fence has (read from /proc/mounts: /tmp, the home,
    # /run/ccsbx/*, the workdir are separate mounts that `find / -xdev` would skip); plus how docker
    # describes the container.
    scan='cat /proc/[0-9]*/environ /proc/[0-9]*/cmdline 2>/dev/null; env
      while read -r _ mp fst _; do
        case "$fst" in proc|sysfs|devpts|devtmpfs|cgroup|cgroup2|mqueue|securityfs|debugfs|tracefs|bpf|pstore) continue;; esac
        find "$mp" -xdev -type f -readable -size -2M -exec cat {} + 2>/dev/null
      done < /proc/mounts'
    dump_now() { { docker exec "ccsbx-$sid-g1" sh -c "$scan"; docker inspect "ccsbx-$sid-g1"; } | tr '\0' '\n'; }
    dump=$(dump_now)
    [ -n "$dump" ] || fail "scan produced nothing"
    echo "$dump" | grep -qF "$sid" || fail "scan control failed: the fence's own sid is not in the dump, so the scan sees too little"
    sig=${tok##*.}; pay=$(echo "$tok" | cut -d. -f2)
    for needle in "$sig" "$pay" "$key" "Bearer ey"; do
      echo "$dump" | grep -qF -- "$needle" && fail "secret material visible in the fence (needle ${needle:0:12}…)"
    done
    # Positive controls (the scan must be able to fail): plant one piece of the token ONLY on each
    # separate mount the agent writes to, re-run the same scan, and require every piece to show up.
    # A scan that skips a mount goes red here.
    docker exec "ccsbx-$sid-g1" sh -c "echo $sig > /tmp/planted" || fail "control: cannot write /tmp in the fence"
    docker exec "ccsbx-$sid-g1" sh -c "echo $pay > \"\$HOME/planted\"" || fail "control: cannot write the fence's home"
    d2=$(dump_now)
    echo "$d2" | grep -qF "$sig" || fail "control: the token planted in /tmp was not found, the scan misses that mount"
    echo "$d2" | grep -qF "$pay" || fail "control: the token planted in the home was not found, the scan misses that mount"
    docker exec "ccsbx-$sid-g1" sh -c 'rm -f /tmp/planted "$HOME/planted"'
    # fwd's own process is not in the fence's pid namespace.
    docker exec "ccsbx-$sid-g1" sh -c "grep -l '[-][-]door' /proc/[0-9]*/cmdline 2>/dev/null" | grep -q . && fail "the forwarder process is visible from the fence"
    ok "after a successful model call, no token, key or Bearer header anywhere the fence can read ($(echo "$dump" | wc -l) lines scanned, control found)"
  fi;;

*) echo "unknown rule/twin: $RULE/$TWIN"; exit 2;;
esac
