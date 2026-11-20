#!/usr/bin/env bash
# night-report-test.sh — tests for bin/ccsbx-night-report (NB3). Why: the report is the only
# thing Tomas reads in the morning, so its table must stay right. Hermetic: a fixture tree.md and a
# fixture --limits string, a stub notify-tomas-telegram on a private PATH, nothing real is read.
# Usage: tests/night-report-test.sh [--help]
set -uo pipefail
case "${1:-}" in -h|--help) sed -n '2,4p' "$0"; exit 0;; esac
here="$(cd "$(dirname "$0")/.." && pwd)"; nr="$here/bin/ccsbx-night-report"
tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
pass=0; fail=0
t() { local name="$1"; shift; if "$@"; then pass=$((pass+1)); echo "ok   - $name"; else fail=$((fail+1)); echo "FAIL - $name"; fi; }
mkdir -p "$tmp/bin"; printf '#!/bin/sh\necho "$@" > %q\n' "$tmp/sent" > "$tmp/bin/notify-tomas-telegram"; chmod +x "$tmp/bin/notify-tomas-telegram"
cat > "$tmp/tree.md" <<'T'
# cc-sandbox
- [ ] Phased build  <!-- n:2f3e -->
  - [ ] 🔨 N1-D Ops stream  <!-- n:34r5 -->
    - [x] P14 inventory  <!-- n:4rcv -->
      Outcome:
        ACCEPT: everything listed
        RAN docker ps -a; SAW 20 containers all listed
    - [ ] U0b-script (write only; Tomas installs in the morning)  <!-- n:43s5 -->
      Outcome:
        BLOCKED (night 1): B9 missing, install refuses
    - [ ] NB3 report  <!-- n:5c4y -->
      Outcome:
        RESULT: red the table is 72 wide
    - [ ] A very long piece title that must not wrap the row  <!-- n:aaaa -->
      Outcome:
        WHY: a reason that is much longer than the forty-odd columns the why column has left over
  - [ ] ⏳ N1-R Review  <!-- n:4d3g -->
  - [ ] Unrelated milestone child  <!-- n:zzzz -->
T
run() { PATH="$tmp/bin:$PATH" "$nr" --tree-md "$tmp/tree.md" --limits "hp: 7d 51% →Fri 09:00" --baseline "$tmp/base.json" "$@"; }

echo "# the table, as Tomas reads it on a phone"
out="$(run --dry-run)"
t "--dry-run prints a table no wider than 70 columns" bash -c 'awk "length(\$0)>70{exit 1}" <<<"$1"' _ "$out"
t "a node ticked [x] shows done with test ok (cc-tree done already ran its check)" grep -qE '^P14 +done +ok' <<<"$out"
t "a BLOCKED: line makes the piece blocked and shows why, so the morning list says what waits" grep -qE '^U0b-script +blocked .*B9 missing' <<<"$out"
t "RESULT: red shows as a red test" grep -qE '^NB3 +open +red' <<<"$out"
t "an over-long why is clipped with an ellipsis, never wrapped" grep -qE '^A +open .*…$' <<<"$out"
t "the stream node itself isn't a row when it has children" bash -c '! grep -q "^N1-D" <<<"$1"' _ "$out"
t "a stream with no children is its own row" grep -qE '^N1-R ' <<<"$out"
t "nodes outside the N1-* streams are left out" bash -c '! grep -q Unrelated <<<"$1"' _ "$out"
t "the summary counts done and blocked" grep -q '1/5 done; blocked 1' <<<"$out"
t "night 1 footer says invariant 14 waived, not evidence" grep -q 'invariant 14 waived' <<<"$out"
out3="$(run --dry-run --no-waived-14)"
t "--no-waived-14 drops that footer" bash -c '! grep -q "invariant 14" <<<"$1"' _ "$out3"

echo "# window and spend"
t "window shows used % and reset" grep -q 'window 7d 51%.*resets Fri 09:00' <<<"$out"
t "without a baseline the spend line says so" grep -q 'spend n/a' <<<"$out"
run --snapshot >/dev/null
out2="$(PATH="$tmp/bin:$PATH" "$nr" --tree-md "$tmp/tree.md" --limits "hp: 7d 55% →Fri 09:00" --baseline "$tmp/base.json" --dry-run)"
t "after --snapshot the spend line is the change since the night's start" grep -q 'spend +4 pts' <<<"$out2"

echo "# sending"
run --send >/dev/null
t "--send goes to the devops bot with --pre" grep -q '^devops --pre ' "$tmp/sent"
run --send --dry-run >/dev/null 2>&1; rc=$?
t "--send and --dry-run together are refused (exit 2)" test "$rc" -eq 2
t "a missing tree.md exits 2" bash -c '"$1" --tree-md /nonexistent >/dev/null 2>&1; [ $? -eq 2 ]' _ "$nr"
t "--help runs nothing and exits 0" bash -c '"$1" --help >/dev/null' _ "$nr"
echo "# $pass passed, $fail failed"; [ "$fail" -eq 0 ]
