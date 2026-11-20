#!/usr/bin/env bash
# tests/fence-cleanup-test.sh checks that the fence tests' cleanup refuses empty or foreign values
# before any docker call. It exists because that cleanup removes containers and networks, and an
# empty sid or STATE must never turn into a broad removal. docker is a stub that records its calls.
# Usage: tests/fence-cleanup-test.sh [--help]
case "${1:-}" in -h|--help) sed -n 2,5p "$0" | sed 's/^# \{0,1\}//'; exit 0;; esac
set -u
here=$(cd "$(dirname "$0")" && pwd)
bin=$(mktemp -d); calls=$bin/calls
printf '#!/bin/sh\necho "$@" >> %s\nexit 0\n' "$calls" > "$bin/docker"; chmod +x "$bin/docker"
export PATH="$bin:$PATH" IMG=img; PIDS=()
. "$here/fence-cleanup.sh"
fail=0
check() { # name, expect refuse|ok
  : > "$calls"; ( cleanup ) >/dev/null 2>&1; rc=$?
  if [ "$2" = refuse ]; then
    { [ $rc -ne 0 ] && [ ! -s "$calls" ]; } || { echo "FAIL $1: rc=$rc docker calls: $(cat "$calls")"; fail=1; }
  else
    { [ $rc -eq 0 ] && [ -s "$calls" ]; } || { echo "FAIL $1 (allow twin): rc=$rc"; fail=1; }
  fi
}
STATE=$(mktemp -d); printf 'sab3cd9fgh2\n' > "$STATE/sids"
check "a valid run cleans its own objects" ok
STATE=$(mktemp -d); printf '\n' > "$STATE/sids";                check "empty sid line" refuse
STATE=$(mktemp -d); printf 'sab3cd9fgh2\n\n' > "$STATE/sids";    check "empty line after a sid" refuse
STATE=$(mktemp -d); printf 'ab3cd9fgh2\n' > "$STATE/sids";       check "sid without the s prefix" refuse
STATE=$(mktemp -d); printf 'sab3cd9fgh2 x\n' > "$STATE/sids";    check "sid with trailing junk" refuse
STATE=$(mktemp -d); printf '*\n' > "$STATE/sids";                check "glob as sid" refuse
STATE="";                                                         check "empty STATE" refuse
STATE=/;                                                          check "STATE is /" refuse
STATE=$HOME;                                                      check "STATE is the home" refuse
STATE=$(mktemp -d); rmdir "$STATE";                               check "STATE does not exist" refuse
rm -rf "$bin"; [ $fail = 0 ] && echo "ok fence-cleanup: refuses empty/foreign values before any docker call" || exit 1
