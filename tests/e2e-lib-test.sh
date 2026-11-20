#!/usr/bin/env bash
# tests/e2e-lib-test.sh checks the exit traps of e2e/lib.sh against a docker stub that records its calls. It exists because
# those traps remove docker objects and scratch directories: a refused cleanup must leave its realm (and its trash list)
# alone, a clean run must leave nothing, and E2E_KEEP=1 must say which containers it left running. No real docker is used.
# Usage: tests/e2e-lib-test.sh   (--help)
case "${1:-}" in -h|--help) sed -n 2,6p "$0" | sed 's/^# \{0,1\}//'; exit 0;; esac
set -u
here=$(cd "$(dirname "$0")/.." && pwd); fail=0
bin=$(mktemp -d); calls=$bin/calls; : > "$calls"
# the stub: `docker inspect NAME` succeeds only for names listed in $bin/running; every call is recorded
cat > "$bin/docker" <<EOS
#!/bin/sh
echo "\$@" >> $calls
[ "\$1" = inspect ] && grep -qx "\$2" $bin/running 2>/dev/null
exit \$?
EOS
chmod +x "$bin/docker"; : > "$bin/running"
chk() { # name, condition result
  [ "$2" = 0 ] || { echo "FAIL $1"; fail=1; }
}
run_lib() { # runs the trap in a subshell with a fresh realm; the caller prepares $R/sids etc. via $1 (a shell snippet)
  ( export PATH="$bin:$PATH" E2E_ROOT="$R" CCSBX_BIN=/bin/true E2E_KEEP="${KEEP:-}"; unset E2E_IMAGE
    . "$here/e2e/lib.sh" >/dev/null 2>&1; e2e_init 2>/dev/null; eval "$1"; exit 0 ) 2>"$bin/err"
}
mk() { R=$(mktemp -d); }

# 1. nothing made: the realm is swept
mk; KEEP= run_lib 'true'; [ ! -d "$R" ]; chk "a run that made no docker object removes its realm" $?

# 2. a refused cleanup (a bad sid line) with trash listed: the realm and its trash list stay, and say so
mk; : > "$calls"
KEEP= run_lib 'echo "not-a-sid" >> "$E2E_ROOT/sids"; echo "/var/tmp/ccsbx-cc-AAAAAA" >> "$E2E_ROOT/trash"'
[ -d "$R" ] && [ -s "$R/trash" ]; chk "a refused cleanup leaves the realm and the trash list" $?
grep -q "left in place" "$bin/err"; chk "... and says it was left in place" $?
! grep -q 'network rm\|rm -f' "$calls"; chk "... and removed no docker object (the refusal came before any call)" $?
rm -rf "$R"

# 3. trash listed but no sid, cleanup refused for a bad trash line: still nothing is swept
mk; KEEP= run_lib 'echo "/etc" >> "$E2E_ROOT/trash"'
[ -d "$R" ]; chk "a bad trash line (not a ccsbx-cc dir) keeps the realm and removes nothing" $?
rm -rf "$R"

# 4. E2E_KEEP=1 names exactly the containers that exist, and removes nothing
mk; : > "$calls"; printf 'ccsbx-sab3cd9fgh2-g1\nccsbx-sab3cd9fgh2-g2\nccsbx-fwd-77\n' > "$bin/running"
KEEP=1 run_lib 'echo sab3cd9fgh2 >> "$E2E_ROOT/sids"; echo ccsbx-fwd-77 >> "$E2E_ROOT/extras"; echo ccsbx-fwd-99 >> "$E2E_ROOT/extras"; echo evil >> "$E2E_ROOT/sids"'
grep -q 'ccsbx-sab3cd9fgh2-g1' "$bin/err" && grep -q 'ccsbx-sab3cd9fgh2-g2' "$bin/err" && grep -q 'ccsbx-fwd-77' "$bin/err"; chk "E2E_KEEP=1 names the containers of the run that are still there" $?
! grep -q 'ccsbx-sab3cd9fgh2-g3\|ccsbx-fwd-99\|evil' "$bin/err"; chk "... and only those (not a gen that is gone, not an unrecorded or malformed name)" $?
! grep -q ' rm \|^rm \|network rm' "$calls"; chk "... and removes nothing (only read-only inspect calls)" $?
[ -d "$R" ]; chk "... and keeps the realm" $?
rm -rf "$R"; : > "$bin/running"

rm -rf "$bin"
[ $fail = 0 ] && echo "ok   e2e/lib.sh traps: refused cleanup keeps the realm, KEEP names what is left" || exit 1
