#!/bin/sh
# Unit-level twin runner for tests/rules.toml: run named Go tests.
#   tests/go-test.sh <package> <TestRegex>      e.g. tests/go-test.sh ./internal/token 'TestTokenCopied$'
# Fails if the regex matches no test (a renamed test must not turn a twin into a silent pass).
# GO overrides the go binary (default: go on PATH, else ~/.local/go/bin/go).
case "${1:-}" in -h|--help|"") sed -n '2,6p' "$0" | sed 's/^# \{0,1\}//'; exit 0;; esac
GO=${GO:-$(command -v go || echo "$HOME/.local/go/bin/go")}
out=$("$GO" test -count=1 -run "$2" -v "$1" 2>&1); rc=$?
echo "$out" | tail -n 15
[ $rc -eq 0 ] || exit 1
echo "$out" | grep -q '^--- PASS' || { echo "FAIL: no test matched $2 in $1"; exit 1; }
