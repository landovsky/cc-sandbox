#!/bin/sh
# Twin runner for rules.toml: tests/twin-go.sh <package> <TestName>
# Runs exactly that Go test and fails unless it ran AND passed. A bare `go test -run X` exits 0
# when X matches nothing, which would turn a renamed test into a green rule.
#   GO=/path/to/go tests/twin-go.sh ./internal/settings TestDenyUnderBypassBlock
#   tests/twin-go.sh --help
case "${1:-}" in -h|--help|"")
  sed -n '2,6p' "$0" | sed 's/^# \{0,1\}//'; exit 0;; esac
GO="${GO:-$HOME/.local/go/bin/go}"
out=$("$GO" test -count=1 -v -run "^$2\$" "$1" 2>&1); rc=$?
printf '%s\n' "$out" | tail -15
[ $rc -eq 0 ] || exit 1
printf '%s\n' "$out" | grep -q -- "^--- PASS: $2 " || { echo "twin-go: $2 did not run (no PASS line)"; exit 1; }
