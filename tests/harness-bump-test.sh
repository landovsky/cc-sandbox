#!/usr/bin/env bash
# harness-bump-test.sh — tests for bin/ccsbx-harness-bump (O6). Hermetic: a scratch git repo as the
# pins repo, a stub `gh` and a stub notify-tomas-telegram on a private PATH, fake build/gate
# commands. Nothing real is built, pushed, or messaged.
# Usage: tests/harness-bump-test.sh [--help]
set -uo pipefail
case "${1:-}" in -h|--help) sed -n '2,5p' "$0"; exit 0;; esac
here="$(cd "$(dirname "$0")/.." && pwd)"; hb="$here/bin/ccsbx-harness-bump"
tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
pass=0; fail=0
t() { local name="$1"; shift; if "$@"; then pass=$((pass+1)); echo "ok   - $name"; else fail=$((fail+1)); echo "FAIL - $name"; fi; }
mkdir -p "$tmp/bin"
printf '#!/bin/sh\necho "$@" >> %q\n' "$tmp/sent" > "$tmp/bin/notify-tomas-telegram"
printf '#!/bin/sh\necho "gh $*" >> %q\necho https://example.invalid/pr/1\n' "$tmp/gh.log" > "$tmp/bin/gh"
chmod +x "$tmp/bin/"*
export PATH="$tmp/bin:$PATH"
repo="$tmp/repo"; git init -q "$repo"; git -C "$repo" config user.email t@t; git -C "$repo" config user.name t
mkdir "$repo/etc"; echo 'claude = "1.0.0"' > "$repo/etc/pins.toml"; git -C "$repo" add -A; git -C "$repo" commit -qm init
base="$(git -C "$repo" rev-parse --abbrev-ref HEAD)"
green='echo {\"evidence\":false,\"results\":[{\"id\":\"fence-flags\",\"twin\":\"block\",\"status\":\"pass\"}]}'
red='echo {\"evidence\":false,\"results\":[{\"id\":\"fence-flags\",\"twin\":\"allow\",\"status\":\"pass\"},{\"id\":\"no-internet\",\"twin\":\"block\",\"status\":\"fail\"}]}; exit 1'
hbump() { "$hb" --repo "$repo" --build-cmd true --pr-cmd 'gh pr create --head {branch} --title "{title}" --body "{body}"' "$@"; }
reset() { rm -f "$tmp/sent" "$tmp/gh.log"; }

echo "# a weakened block must stop the bump"
reset; out="$(hbump --version 2.0.0 --gate-cmd "$red" 2>&1)"; rc=$?
t "a failing block test exits 1" test $rc -eq 1
t "no PR is opened when the suite is red" test ! -e "$tmp/gh.log"
t "the Telegram names the failing test, so the morning table says which block broke" grep -q 'T:no-internet (block)' "$tmp/sent"
t "the pin file is untouched on a red run" grep -q '1.0.0' "$repo/etc/pins.toml"
t "no bump branch is left behind on a red run" test -z "$(git -C "$repo" branch --list 'harness-bump/*')"

echo "# a green suite opens a PR from its own branch"
reset; out="$(hbump --version 2.0.0 --gate-cmd "$green" 2>&1)"; rc=$?
t "green exits 0" test $rc -eq 0
t "gh pr create runs once on the bump branch" grep -q 'gh pr create --head harness-bump/2.0.0' "$tmp/gh.log"
t "the branch moves the pin to the candidate" bash -c 'git -C "$1" show harness-bump/2.0.0:etc/pins.toml | grep -q "2.0.0"' _ "$repo"
t "the working branch is restored and its pin unchanged (nothing lands without review)" bash -c '[ "$(git -C "$1" rev-parse --abbrev-ref HEAD)" = "$2" ] && grep -q 1.0.0 "$1/etc/pins.toml"' _ "$repo" "$base"
t "the PR body says direct mode is not evidence" grep -q 'NOT EVIDENCE' "$tmp/gh.log"

echo "# broken infrastructure is never read as green"
reset; hbump --version 3.0.0 --build-cmd 'echo boom >&2; exit 5' --gate-cmd "$green" >/dev/null 2>&1; rc=$?
t "a failed candidate build exits 3 and opens no PR" bash -c '[ $1 -eq 3 ] && [ ! -e "$2" ]' _ $rc "$tmp/gh.log"
t "the build failure is reported by Telegram" grep -q 'build failed' "$tmp/sent"
reset; hbump --version 3.0.0 --gate-cmd 'echo not json' >/dev/null 2>&1; rc=$?
t "a gate that prints no JSON is red, not green" bash -c '[ $1 -eq 1 ] && [ ! -e "$2" ]' _ $rc "$tmp/gh.log"
reset; hbump --version 3.0.0 --gate-cmd 'echo {\"results\":[{\"id\":\"x\",\"twin\":\"allow\",\"status\":\"pending\"}]}' --strict >/dev/null 2>&1; rc=$?
t "--strict makes a pending twin red" test $rc -eq 1
reset; hbump --latest-cmd 'echo; exit 0' --gate-cmd "$green" >/dev/null 2>&1; rc=$?
t "a lookup that yields no version exits 3" test $rc -eq 3

echo "# a bump to the version already pinned does nothing"
reset; hbump --version 1.0.0 --gate-cmd "$red" >/dev/null 2>&1; rc=$?
t "same version as the pin: exit 0, no suite run, no message" bash -c '[ $1 -eq 0 ] && [ ! -e "$2" ]' _ $rc "$tmp/sent"

echo "# --dry-run and --help run nothing"
reset; out="$(hbump --version 2.5.0 --dry-run --build-cmd 'touch '"$tmp"'/built')"
t "--dry-run builds nothing and sends nothing" bash -c '[ ! -e "$1/built" ] && [ ! -e "$1/sent" ]' _ "$tmp"
t "--dry-run lists the PR step" grep -q 'harness-bump/2.5.0' <<<"$out"
t "--help works" bash -c '"$1" --help | grep -q Recipes' _ "$hb"
t "--no-pr stops after a green suite" bash -c '"$1" --repo "$2" --build-cmd true --version 4.0.0 --gate-cmd "$3" --no-pr --no-notify >/dev/null; [ $? -eq 0 ] && [ -z "$(git -C "$2" branch --list "harness-bump/4*")" ]' _ "$hb" "$repo" "$green"

echo "# $pass passed, $fail failed"; [ $fail -eq 0 ]
