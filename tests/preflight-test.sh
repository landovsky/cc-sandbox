#!/usr/bin/env bash
# preflight-test.sh — tests for bin/ccsbx-preflight (NB1). Why: the evening gate must never
# touch anything real or install anything, so that is proven here. Hermetic: every external command the
# preflight may call is a stub on a private PATH, so nothing real (kubectl, sudo, gh, tailscale,
# a Telegram) is touched, and a stub for sudo/snap/lxc/apt records any call so we can prove
# the evening gate never makes one.
# Usage: tests/preflight-test.sh [--help]      Needs python3; strace is optional (that test skips).
set -uo pipefail
case "${1:-}" in -h|--help) sed -n '2,6p' "$0"; exit 0;; esac
here="$(cd "$(dirname "$0")/.." && pwd)"
pf="$here/bin/ccsbx-preflight"
tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
bin="$tmp/bin"; mkdir -p "$bin"; log="$tmp/calls.log"; : > "$log"
pass=0; fail=0
ok()  { pass=$((pass+1)); echo "ok   - $1"; }
bad() { fail=$((fail+1)); echo "FAIL - $1  ($2)"; }
t() { local name="$1"; shift; if "$@"; then ok "$name"; else bad "$name" "$*"; fi; }

# --- stubs. Forbidden ones record themselves, then fail loudly.
for c in sudo snap lxc apt apt-get dokku; do
  printf '#!/bin/sh\necho "%s $*" >> %q\nexit 99\n' "$c" "$log" > "$bin/$c"; chmod +x "$bin/$c"
done
cat > "$bin/cc-tree" <<'S'
#!/bin/sh
case "$1" in
  show) for p in "65s2:Q2" "42yx:Q3" "4wws:Q4" "2tdw:Q6" "6qgt:Q7" "53vd:Q8" "2bqd:Q9" "3y55:Q10" "24wy:Q12"; do
          id=${p%%:*}; q=${p##*:}
          m="[x]"; case " $FAKE_OPEN " in *" $id "*) m="[ ]";; esac
          echo "  - $m ❓ $q a decision  #$id"; done ;;
  limits) echo "5h 12%  7d 47%" ;;
  trees) echo "e2e" ;;
  armed) echo '{"armed": true}' ;;
esac
S
printf '#!/bin/sh\necho "kubectl $*" >> %q\nexit 1\n' "$log" > "$bin/kubectl"
printf '#!/bin/sh\necho "gh $*" >> %q\necho "gh: Not Found (HTTP 404)" >&2\nexit 1\n' "$log" > "$bin/gh"
printf '#!/bin/sh\necho "tailscale $*" >> %q\necho "{}"\n' "$log" > "$bin/tailscale"
printf '#!/bin/sh\necho "tg $*" >> %q\n' "$log" > "$bin/notify-tomas-telegram"
printf '#!/bin/sh\necho "ccq $*" >> %q\necho x\n' "$log" > "$bin/ccq"
chmod +x "$bin"/*
export PATH="$bin:/usr/bin:/bin"
cfg="$tmp/p.toml"; sed "s|~/git/k3s|$tmp/k3s|; s|~/git/cc-sandbox\"|$here\"|" "$here/etc/preflight.toml" > "$cfg"
mkdir -p "$tmp/k3s"; git -C "$tmp/k3s" init -q
run() { "$pf" --config "$cfg" "$@"; }

echo "# --help and --list: the gate table must be readable with nothing to run it against"
out="$(run --help 2>&1)"; rc=$?
t "--help exits 0 and lists C-R8 with its gates and effect" bash -c '[ '"$rc"' -eq 0 ] && grep -q "C-R8 .*evening.*skip.*testbed" <<<"$1"' _ "$out"
t "--help made no external call (stubs untouched)" test ! -s "$log"
if command -v strace >/dev/null; then
  s="$(strace -f -e trace=connect "$pf" --help 2>&1 >/dev/null | grep -c 'connect(')"
  t "strace -f -e trace=connect --help shows no connect" test "$s" = 0
else echo "skip - strace missing"; fi
t "--list prints every check, one per line" test "$(run --list | wc -l)" -ge 25

echo "# usage errors"
run >/dev/null 2>&1; t "no gate flag exits 2 so a typo can't look like a green night" test $? -eq 2
run --only C-NOPE >/dev/null 2>&1; t "an unknown check id exits 2" test $? -eq 2

echo "# C-R1: an unanswered ❓ blocks the night and is named"
out="$(FAKE_OPEN="6qgt" run --only C-R1)"; rc=$?
t "an open Q7 fails C-R1 naming Q7 #6qgt" bash -c '[ '"$rc"' -eq 1 ] && grep -q "Q7 #6qgt" <<<"$1"' _ "$out"
out="$(run --only C-R1)"; rc=$?
t "all night-blocking ❓s answered passes" test "$rc" -eq 0

echo "# the evening gate with the testbed missing"
out="$(run --evening)"; rc=$?
t "--evening exits 1 and names C-R8 (testbed missing) on the SKIP line" bash -c '[ '"$rc"' -eq 1 ] && grep -q "^SKIP:.*C-R8" <<<"$1"' _ "$out"
t "every table line is at most 70 columns" bash -c 'awk "length(\$0)>70{exit 1}" <<<"$1"' _ "$out"
t "the table has one header row" test "$(grep -c '^check ' <<<"$out")" -eq 1
t "the evening gate never calls sudo, snap, lxc, apt, dokku" bash -c '! grep -E "^(sudo|snap|lxc|apt|apt-get|dokku) " "$1"' _ "$log"
t "the evening gate never runs kubectl write verbs" bash -c '! grep -E "^kubectl .*(apply|delete|edit|patch|rollout)" "$1"' _ "$log"

echo "# waiving, abort-only and json"
run --only C-R8 --waive C-R8 >/dev/null; t "a waived failure doesn't fail the run" test $? -eq 0
run --only C-R8 --abort-only >/dev/null; t "--abort-only ignores a skip-class failure" test $? -eq 0
run --only C-R8 --json > "$tmp/j.json"; python3 -I -c 'import json,sys; d=json.load(open(sys.argv[1])); assert d["skip"]==["C-R8"] and d["results"][0]["state"]=="FAIL", d' "$tmp/j.json"
t "--json is parseable and lists C-R8 under skip" test $? -eq 0

echo "# telegram"
: > "$log"; run --only C-R6 --telegram >/dev/null
t "--telegram sends the table with --pre to the devops bot" grep -q '^tg devops --pre' "$log"

echo "# a crashing check doesn't kill the run"
bad_cfg="$tmp/bad.toml"; sed 's/^night_blocking = .*/night_blocking = { Q2 = 5 }/' "$cfg" > "$bad_cfg"
"$pf" --config "$bad_cfg" --only C-R1,C-R6 >/dev/null 2>&1; rc=$?
t "bad config in one check still runs the rest and exits 0/1, not a traceback" test "$rc" -le 1

echo "# $pass passed, $fail failed"
[ "$fail" -eq 0 ]
