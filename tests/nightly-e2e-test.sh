#!/usr/bin/env bash
# nightly-e2e-test.sh — tests for bin/ccsbx-nightly-e2e (C21). Hermetic: canned TAP from fake run
# commands, a private --out dir, a stub notify-tomas-telegram on a private PATH. Nothing real runs.
# Usage: tests/nightly-e2e-test.sh [--help]
set -uo pipefail
case "${1:-}" in -h|--help) sed -n '2,4p' "$0"; exit 0;; esac
here="$(cd "$(dirname "$0")/.." && pwd)"; ne="$here/bin/ccsbx-nightly-e2e"
tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
pass=0; fail=0
t() { local name="$1"; shift; if "$@"; then pass=$((pass+1)); echo "ok   - $name"; else fail=$((fail+1)); echo "FAIL - $name"; fi; }
mkdir -p "$tmp/bin"; printf '#!/bin/sh\nshift; shift; printf "%%s\\n" "$1" > %q\n' "$tmp/sent" > "$tmp/bin/notify-tomas-telegram"; chmod +x "$tmp/bin/notify-tomas-telegram"
export PATH="$tmp/bin:$PATH"
GREEN="printf '# 00-standalone\nok 1 - a\nok 2 - b\n# 01-launch\nok 3 - c\n1..3\n'"
SKIPS="printf '# 00-standalone\nok 1 - a\n# 05-approval\nok 2 - x # SKIP needs P7\n1..2\n'"
RED="printf '# 00-standalone\nok 1 - a\n# 03-verbs\nnot ok 2 - an outcome for A leaked under B\n1..2\n'; exit 1"
run() { "$ne" --out "$tmp/out" "$@"; }

echo "# three green nights is the bar; a red night resets it"
for n in 1 2; do out="$(run --run-cmd "$GREEN")"; done
t "after two green nights the streak reads 2/3" grep -q 'green streak 2/3' <<<"$out"
out="$(run --run-cmd "$GREEN" --send)"; rc=$?
t "the third green night reaches 3/3 and exits 0" bash -c '[ $1 -eq 0 ] && grep -q "streak 3/3" <<<"$2"' _ $rc "$out"
t "--send posts the same table to Telegram" grep -q 'streak 3/3' "$tmp/sent"
out="$(run --run-cmd "$RED")"; rc=$?
t "a failing line exits 1 and the table names it, so the morning message says what broke" bash -c '[ $1 -eq 1 ] && grep -q "! 03-verbs: an outcome for A leaked under B" <<<"$2"' _ $rc "$out"
t "RED closes a red night" grep -q '^RED' <<<"$out"
out="$(run --run-cmd "$GREEN")"
t "the streak restarts at 1 after a red night (the next green night is night one again)" grep -q 'streak 1/3' <<<"$out"

echo "# a skip is not a pass"
out="$(run --run-cmd "$SKIPS")"; rc=$?
t "skips exit 0 but the verdict says 'with skips' and counts them" bash -c '[ $1 -eq 0 ] && grep -q "GREEN, with skips: 1 ok, 1 skipped" <<<"$2"' _ $rc "$out"
t "the streak line carries the skip warning, so three skipped nights are not read as three real passes" grep -q '1 skips: not built yet' <<<"$out"

echo "# broken infrastructure is red"
out="$(run --run-cmd 'echo oops >&2; exit 3')"; rc=$?
t "a run that prints no TAP is red, not green" bash -c '[ $1 -eq 1 ] && grep -q "^RED" <<<"$2" && grep -q "exited 3" <<<"$2"' _ $rc "$out"
out="$(run --run-cmd 'sleep 5' --timeout 1)"; rc=$?
t "a hung run is cut by --timeout and is red" bash -c '[ $1 -eq 1 ] && grep -q "timeout after 1s" <<<"$2"' _ $rc "$out"

echo "# output shape"
out="$(run --run-cmd "$RED")"
t "no table line is wider than 70 columns (phone, --pre)" bash -c '! awk "length(\$0)>70{f=1} END{exit !f}" <<<"$1"' _ "$out"
t "direct mode is flagged not evidence" grep -q 'NOT EVIDENCE' <<<"$out"
t "--last reprints without running" bash -c '"$1" --out "$2" --last | grep -q "leaked under B"' _ "$ne" "$tmp/out"
t "--dry-run runs and writes nothing" bash -c 'n=$(wc -l < "$1/out/e2e-history.jsonl"); "$2" --out "$1/out" --dry-run --run-cmd "echo hi >> $1/ran" >/dev/null; [ ! -e "$1/ran" ] && [ "$n" = "$(wc -l < "$1/out/e2e-history.jsonl")" ]' _ "$tmp" "$ne"
t "--help works" bash -c '"$1" --help | grep -q Recipes' _ "$ne"

echo "# $pass passed, $fail failed"; [ $fail -eq 0 ]
