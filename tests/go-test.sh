#!/bin/sh
# Unit-level twin runner for tests/rules.toml: run named Go tests.
#   tests/go-test.sh <package> <TestRegex>      e.g. tests/go-test.sh ./internal/token 'TestTokenCopied$'
# Fails if the regex matches no test (a renamed test must not turn a twin into a silent pass). When the
# regex is plain names (A$|B$), EVERY name must run and pass, so renaming one of several cannot drop it quietly.
# GO overrides the go binary (default: go on PATH, else ~/.local/go/bin/go).
case "${1:-}" in -h|--help|"") sed -n '2,6p' "$0" | sed 's/^# \{0,1\}//'; exit 0;; esac
GO=${GO:-$(command -v go || echo "$HOME/.local/go/bin/go")}
out=$("$GO" test -count=1 -run "$2" -v "$1" 2>&1); rc=$?
echo "$out" | tail -n 15
[ $rc -eq 0 ] || exit 1
echo "$out" | grep -q '^--- PASS' || { echo "FAIL: no test matched $2 in $1"; exit 1; }
# every alternative that is a plain test name must have its own PASS line
for n in $(printf '%s' "$2" | tr '|' ' ' | sed 's/\^//g; s/\$//g'); do
  case "$n" in *[!A-Za-z0-9_]*|"") continue;; esac
  echo "$out" | grep -q "^--- PASS: $n " || { echo "FAIL: $n did not run and pass in $1"; exit 1; }
done
