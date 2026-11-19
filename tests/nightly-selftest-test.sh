#!/usr/bin/env bash
# nightly-selftest-test.sh — tests for bin/ccsbx-nightly-selftest (P6). Hermetic: fake legs print
# canned selftest JSON; a stub notify-tomas-telegram on a private PATH; results go to a tmpdir.
# Usage: tests/nightly-selftest-test.sh [--help]
set -uo pipefail
case "${1:-}" in -h|--help) sed -n '2,4p' "$0"; exit 0;; esac
here="$(cd "$(dirname "$0")/.." && pwd)"; ns="$here/bin/ccsbx-nightly-selftest"
tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
pass=0; fail=0
t() { local name="$1"; shift; if "$@"; then pass=$((pass+1)); echo "ok   - $name"; else fail=$((fail+1)); echo "FAIL - $name"; fi; }
mkdir -p "$tmp/bin"; printf '#!/bin/sh\nshift; shift; printf "%%s\\n" "$1" > %q\n' "$tmp/sent" > "$tmp/bin/notify-tomas-telegram"; chmod +x "$tmp/bin/notify-tomas-telegram"
export PATH="$tmp/bin:$PATH"
G='echo {\"evidence\":false,\"results\":[{\"id\":\"a\",\"twin\":\"allow\",\"status\":\"pass\"},{\"id\":\"b\",\"twin\":\"block\",\"status\":\"pending\"}]}'
R='echo {\"evidence\":false,\"results\":[{\"id\":\"a\",\"twin\":\"allow\",\"status\":\"pass\"},{\"id\":\"no-internet\",\"twin\":\"block\",\"status\":\"fail\"}]}; exit 1'
# every default leg replaced, so nothing real runs
legs=(--leg "unit=$G" --leg "fence@docker=$G" --leg "doors=$G" --leg "xmachine=$G" --leg "live-hi=skip:needs the model door")
run() { "$ns" --out "$tmp/out" "${legs[@]}" "$@"; }

echo "# a quiet night"
out="$(run)"; rc=$?
t "all legs green or skipped exits 0" test $rc -eq 0
t "a skipped leg shows skip with its reason, never ok" grep -qE 'live-hi +skip' <<<"$out"
t "the table says ALL GREEN" grep -q 'ALL GREEN' <<<"$out"
t "direct mode is flagged not evidence (invariant 14 waived)" grep -q 'NOT EVIDENCE' <<<"$out"
t "the night's JSON is kept, plus latest-hp.json for --last" test -e "$tmp/out/latest-hp.json"

echo "# a forced red test must reach the morning table by name"
out="$(run --leg "doors=$R" --send)"; rc=$?
t "a red twin exits 1" test $rc -eq 1
t "the table names the failing test and twin" grep -q '! T:no-internet (block)' <<<"$out"
t "the leg row says red and RED closes the table" bash -c 'grep -qE "doors +red" <<<"$1" && grep -q "^RED" <<<"$1"' _ "$out"
t "--send posts that same table to Telegram" grep -q 'T:no-internet (block)' "$tmp/sent"
t "no table line is wider than 70 columns (phone, --pre)" bash -c '! awk "length(\$0)>70{f=1} END{exit !f}" <<<"$1"' _ "$out"
t "--last reprints the red night without running anything" bash -c '"$1" --out "$2" --last | grep -q "T:no-internet (block)"' _ "$ns" "$tmp/out"

echo "# broken infrastructure is red, never green"
out="$(run --leg 'unit=echo oops; exit 3')"; rc=$?
t "a leg that prints no JSON is broken and exits 1" bash -c '[ $1 -eq 1 ] && grep -qE "unit +broken" <<<"$2"' _ $rc "$out"
out="$(run --leg 'unit=sleep 5' --timeout 1)"; rc=$?
t "a hung leg is cut by --timeout and is broken" bash -c '[ $1 -eq 1 ] && grep -q "timeout after 1s" <<<"$2"' _ $rc "$out"

echo "# sets, --help and --dry-run"
t "--dry-run runs nothing and lists the plan's legs" bash -c 'o="$("$1" --dry-run)"; grep -q "xmachine" <<<"$o" && grep -q "nothing was run" <<<"$o"' _ "$ns"
t "set k8s has the fence@k8s leg (plan: hp -> future-two 02:45)" bash -c '"$1" --set k8s --dry-run | grep -q "fence@k8s"' _ "$ns"
t "an unknown set exits 2" bash -c '"$1" --set nope >/dev/null 2>&1; [ $? -eq 2 ]' _ "$ns"
t "--help works" bash -c '"$1" --help | grep -q Recipes' _ "$ns"

echo "# $pass passed, $fail failed"; [ $fail -eq 0 ]
