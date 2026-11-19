#!/usr/bin/env bash
# install-linux-test.sh — tests for units/install-linux.sh, units/p15 and units/lib/sudoers_strip.py.
# Hermetic and rootless: the installer's test hooks put every path under a temp dir
# (CCSBX_ROOT_PREFIX) and RECORD each command instead of running it (CCSBX_EXEC=record:FILE), and
# every command that could change the host (sudo, snap, lxc, apt, docker, systemctl, gpasswd …)
# is a stub on a private PATH that logs a call, which the tests assert never happens.
# Usage: tests/install-linux-test.sh [--help]
set -uo pipefail
case "${1:-}" in -h|--help) sed -n '2,7p' "$0"; exit 0;; esac
here="$(cd "$(dirname "$0")/.." && pwd)"; inst="$here/units/install-linux.sh"; p15="$here/units/p15"
strip="$here/units/lib/sudoers_strip.py"
tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
pass=0; fail=0
t() { local name="$1"; shift; if "$@"; then pass=$((pass+1)); echo "ok   - $name"; else fail=$((fail+1)); echo "FAIL - $name"; fi; }

# ---- stubs: a logged call = a test failure
bin="$tmp/bin"; mkdir -p "$bin"; calls="$tmp/stub-calls.log"; : > "$calls"
for c in sudo snap lxc apt apt-get docker systemctl gpasswd useradd userdel groupadd ufw visudo runuser pgrep; do
  printf '#!/bin/sh\necho "%s $*" >> %q\nexit 1\n' "$c" "$calls" > "$bin/$c"; chmod +x "$bin/$c"
done
export PATH="$bin:/usr/bin:/bin"

# ---- fixtures
fx() {                                   # fx NAME → a fresh fake root with sudoers.d and built binaries
  local d="$tmp/$1"; rm -rf "$d"; mkdir -p "$d/root/etc/sudoers.d" "$d/dist" "$d/rec"
  printf '%s\n' 'tomas ALL=(ALL) NOPASSWD: /usr/bin/docker, /usr/bin/apt-get, /usr/bin/dokku, /usr/local/bin/sops' > "$d/root/etc/sudoers.d/90-tomas-nopasswd"
  printf '%s\n' '# see sudoers(5)' > "$d/root/etc/sudoers.d/README"
  for b in ccsbx-root ccsbx-hostd ccsbx-signer ccsbx; do echo "bin $b" > "$d/dist/$b"; done
  (cd "$d/dist" && sha256sum ccsbx-root ccsbx-hostd ccsbx-signer ccsbx > SHA256SUMS)
  printf 'id\tstate\twhat\taction\nA5\tdone\trunners\tmoved\nB-x\tdropped\tx\tgone\n' > "$d/p15.tsv"
  echo "$d"
}
ins() {                                  # ins FXDIR ARGS… — run the installer inside the fake root, recording commands
  local d="$1"; shift
  CCSBX_ROOT_PREFIX="$d/root" CCSBX_EXEC="record:$d/rec/cmds" CCSBX_BIN_DIR="$d/dist" CCSBX_P15_FILE="$d/p15.tsv" \
  CCSBX_OWNER=tomas CCSBX_STAMP=T1 CCSBX_BACKUP_DIR=/var/backups/ccsbx-install CCSBX_B9_CMD="${B9:-true}" "$inst" "$@"
}
snapshot() { (cd "$1" && find root dist -type f -o -type l | sort | xargs -r sha256sum 2>/dev/null; cat rec/cmds 2>/dev/null); }

echo "# --help: every subcommand explains itself and runs nothing"
for sub in "" plan install check uninstall; do
  out="$("$inst" $sub --help 2>&1)"; rc=$?
  t "install-linux.sh ${sub:-(none)} --help exits 0 and mentions the gates" bash -c '[ '"$rc"' -eq 0 ] && grep -q "GATES" <<<"$1"' _ "$out"
done
t "p15 --help and p15 mark --help exit 0" bash -c '"$1" --help >/dev/null && "$1" mark --help >/dev/null' _ "$p15"
t "no help call ran a stubbed command" test ! -s "$calls"

echo "# plan: shows every step, changes nothing (the I0 accept line)"
d="$(fx plan)"; before="$(snapshot "$d")"
out="$(ins "$d" plan -v 2>&1)"; rc=$?
t "plan exits 0" test "$rc" -eq 0
t "plan prints the docker group removal, and it is the last step before the backup record" bash -c 'grep -q "gpasswd -d tomas docker" <<<"$1" && [ "$(grep -n "gpasswd -d tomas docker" <<<"$1" | cut -d: -f1)" -gt "$(grep -n "visudo -cf" <<<"$1" | tail -1 | cut -d: -f1)" ]' _ "$out"
t "plan prints ccsbx-root as the NOPASSWD rule" grep -q 'NOPASSWD: /usr/local/sbin/ccsbx-root' <<<"$out"
t "plan changed no file and recorded no command" test "$before" = "$(snapshot "$d")"
t "plan called no stubbed command (no sudo, snap, lxc, apt, docker, systemctl …)" test ! -s "$calls"
dry="$("$inst" plan 2>&1)"
t "the real plan (no test hooks) also prints the step list and calls no stub" bash -c 'grep -q "^\[09\]" <<<"$1"' _ "$dry"
t "…its only calls were read-only probes of the hub (systemctl --user is-active/show, docker ps)" bash -c '! grep -vE "^(systemctl --user (is-active|show)|docker ps)" "$1"' _ "$calls"
: > "$calls"

echo "# install refuses until B9 and P15 are met (U0 removes the owner from docker)"
d="$(fx noyes)"; before="$(snapshot "$d")"
ins "$d" install >/dev/null 2>"$tmp/err"; rc=$?
t "install without --yes is refused (exit 3)" test "$rc" -eq 3
d="$(fx b9)"; before="$(snapshot "$d")"
B9='echo "hub is still the docker container"; false' ins "$d" install --yes >/dev/null 2>"$tmp/err"; rc=$?
t "B9 missing → exit 3, and the message names B9" bash -c '[ '"$rc"' -eq 3 ] && grep -q "B9 missing" "$1"' _ "$tmp/err"
t "B9 missing → nothing recorded, nothing written, no backup made" test "$before" = "$(snapshot "$d")"
d="$(fx p15)"; printf 'id\tstate\twhat\taction\nA5\topen\trunners\tundecided\n' > "$d/p15.tsv"
ins "$d" install --yes >/dev/null 2>"$tmp/err"; rc=$?
t "P15 open → exit 3 naming P15 and the open row" bash -c '[ '"$rc"' -eq 3 ] && grep -q "P15 open" "$1" && grep -q A5 "$1"' _ "$tmp/err"
t "P15 open → nothing recorded" test ! -s "$d/rec/cmds"
d="$(fx nobin)"; rm "$d/dist/ccsbx-root"
ins "$d" install --yes >/dev/null 2>"$tmp/err"; rc=$?
t "a missing binary → exit 3" bash -c '[ '"$rc"' -eq 3 ] && grep -q "binaries missing" "$1"' _ "$tmp/err"
d="$(fx tamper)"; echo tampered > "$d/dist/ccsbx-hostd"
ins "$d" install --yes >/dev/null 2>"$tmp/err"; rc=$?
t "a binary that doesn't match SHA256SUMS → exit 3" bash -c '[ '"$rc"' -eq 3 ] && grep -q "SHA256SUMS" "$1"' _ "$tmp/err"
d="$(fx badsudo)"; echo 'tomas ALL=(ALL) NOPASSWD: ALL' > "$d/root/etc/sudoers.d/90-tomas-nopasswd"
ins "$d" install --yes >/dev/null 2>"$tmp/err"; rc=$?
t "a NOPASSWD: ALL line the stripper won't guess at → exit 3, nothing changed" bash -c '[ '"$rc"' -eq 3 ] && grep -q "sudoers 90-tomas-nopasswd" "$1" && [ ! -s "$2" ]' _ "$tmp/err" "$d/rec/cmds"

echo "# install with every gate met (against the fake root, commands recorded)"
d="$(fx ok)"; : > "$calls"
ins "$d" install --yes >"$d/out" 2>"$d/err"; rc=$?
t "install exits 0" bash -c '[ '"$rc"' -eq 0 ] || { cat "$1" >&2; exit 1; }' _ "$d/err"
cmds="$d/rec/cmds"
t "the owner leaves docker as the LAST group change" bash -c 'l="$(grep -n "^gpasswd" "$1" | tail -1)"; grep -q "gpasswd -d tomas docker" <<<"$l"' _ "$cmds"
t "the replacement rule is validated by visudo before it is renamed into place" bash -c 'a="$(grep -n "visudo -cf /etc/sudoers.d/.90-ccsbx-root.new" "$1" | head -1 | cut -d: -f1)"; b="$(grep -n "mv -f /etc/sudoers.d/.90-ccsbx-root.new" "$1" | head -1 | cut -d: -f1)"; [ -n "$a" ] && [ "$a" -lt "$b" ]' _ "$cmds"
t "docker group removal comes after the sudoers rule and after hostd is enabled" bash -c 'g="$(grep -n "gpasswd -d tomas docker" "$1" | cut -d: -f1)"; s="$(grep -n "mv -f /etc/sudoers.d/.90-ccsbx-root.new" "$1" | cut -d: -f1)"; h="$(grep -n "enable --now ccsbx-hostd" "$1" | cut -d: -f1)"; [ "$g" -gt "$s" ] && [ "$g" -gt "$h" ]' _ "$cmds"
t "the new sudoers file grants only ccsbx-root, to the owner and to ccsbx" bash -c 'f="$1/root/etc/sudoers.d/.90-ccsbx-root.new"; [ "$(grep -c "^[a-z].*NOPASSWD" "$f")" -eq 2 ] && ! grep "^[a-z].*NOPASSWD" "$f" | grep -qv "/usr/local/sbin/ccsbx-root$"' _ "$d"
t "the old file lost docker, apt-get, dokku and sops (nothing else was in it)" bash -c '! grep -qE "docker|apt-get|dokku|sops" "$1/root/etc/sudoers.d/90-tomas-nopasswd"' _ "$d"
t "the old sudoers file is backed up before it is changed" test -f "$d/root/var/backups/ccsbx-install/T1/etc/sudoers.d/90-tomas-nopasswd"
t "the backup holds the ORIGINAL content" grep -q 'dokku' "$d/root/var/backups/ccsbx-install/T1/etc/sudoers.d/90-tomas-nopasswd"
t "the allowed-roots file lists the owner's git root" grep -q '/git"' "$d/root/etc/ccsbx/ccsbx-root.toml"
t "the keys directory is made 0700 for ccsbx-keys" bash -c 'grep -q "chmod 0700 /var/lib/ccsbx-keys" "$1" && grep -q "chown ccsbx-keys:ccsbx-keys /var/lib/ccsbx-keys" "$1"' _ "$cmds"
t "the signer unit has no network and no key outside its own user" bash -c 'grep -q "RestrictAddressFamilies=AF_UNIX" "$1/root/etc/systemd/system/ccsbx-signer.service" && grep -q "User=ccsbx-keys" "$1/root/etc/systemd/system/ccsbx-signer.service"' _ "$d"
t "hostd's unit does not run as the key user" bash -c '! grep -q "ccsbx-keys" <(grep -E "^(User|Group)=" "$1/root/etc/systemd/system/ccsbx-hostd.service")' _ "$d"
t "the firewall rule is only on the tailnet interface" grep -q 'ufw allow in on tailscale0 to any port 7480' "$cmds"
t "the real system was not touched: the only stub calls were read-only probes (snap list)" bash -c '! grep -vE "^snap list lxd$" "$1"' _ "$calls"

echo "# uninstall puts the sudoers file back"
ins "$d" uninstall --yes >"$d/uout" 2>"$d/uerr"; rc=$?
t "uninstall exits 0" bash -c '[ '"$rc"' -eq 0 ] || { cat "$1" >&2; exit 1; }' _ "$d/uerr"
t "uninstall copies the backed-up sudoers files back (recorded: the hook doesn't execute)" grep -q 'cp -a /var/backups/ccsbx-install/T1/etc/sudoers.d/. /etc/sudoers.d/' "$d/rec/cmds"
t "…and the backup it copies from still holds the original (dokku) line" grep -q dokku "$d/root/var/backups/ccsbx-install/T1/etc/sudoers.d/90-tomas-nopasswd"
t "uninstall without a backup is refused" bash -c 'CCSBX_ROOT_PREFIX="$1/nope" CCSBX_EXEC="record:$1/x" CCSBX_BACKUP_DIR=/nowhere CCSBX_OWNER=tomas "$2" uninstall --yes >/dev/null 2>&1; [ $? -eq 3 ]' _ "$d" "$inst"

echo "# check: read-only, and fails on a machine that was never installed"
: > "$calls"
out="$(CCSBX_ROOT_PREFIX="$tmp/empty" CCSBX_EXEC="record:$tmp/x" CCSBX_OWNER=tomas "$inst" check 2>&1)"; rc=$?
t "check exits 1 before the install and says what is open" bash -c '[ '"$rc"' -eq 1 ] && grep -q "^FAIL" <<<"$1"' _ "$out"
t "check calls the sudo stub only to read (sudo -n -l), never to change" bash -c '! grep -E "^(sudo|snap|systemctl) " "$1" | grep -vE "^(sudo -n -l|snap list|systemctl (is-active|is-enabled))"' _ "$calls"

echo "# the test hook can't be pointed at the real system"
CCSBX_ROOT_PREFIX="$tmp/z" "$inst" plan >/dev/null 2>&1; t "a prefix without record mode is refused (exit 2)" test $? -eq 2

echo "# sudoers_strip.py: removes only the four grants, guesses at nothing"
s="$tmp/s"; mkdir -p "$s"
printf '%s\n' 'Defaults env_reset' 'tomas ALL=(ALL) NOPASSWD: /usr/bin/docker, /usr/bin/systemctl restart nginx' 'bob ALL=(ALL) NOPASSWD: /usr/bin/foo' > "$s/a"
out="$("$strip" "$s/a" 2>/dev/null)"
t "a mixed line keeps the other command" grep -q 'tomas ALL=(ALL) NOPASSWD: /usr/bin/systemctl restart nginx$' <<<"$out"
t "another user's rule and Defaults are untouched" bash -c 'grep -q "^bob ALL=(ALL) NOPASSWD: /usr/bin/foo$" <<<"$1" && grep -q "^Defaults env_reset$" <<<"$1"' _ "$out"
printf '%s\n' 'tomas ALL=(ALL) NOPASSWD: ALL' > "$s/b"; "$strip" "$s/b" >/dev/null 2>&1; t "NOPASSWD: ALL → exit 3" test $? -eq 3
printf '%s\n' 'tomas ALL=(ALL) NOPASSWD: /usr/bin/docker, \' '  /usr/bin/foo' > "$s/c"; "$strip" "$s/c" >/dev/null 2>&1; t "a continuation line → exit 3" test $? -eq 3
printf '%s\n' '%admin ALL=(ALL) NOPASSWD: /usr/bin/docker' > "$s/d"; out="$("$strip" "$s/d" 2>/dev/null)"; t "a group rule granting only docker is dropped" test -z "$out"

echo "# p15: the gate itself"
f="$tmp/p.tsv"; printf 'id\tstate\twhat\taction\nA\topen\tx\ty\nB\tdone\tx\ty\n' > "$f"
CCSBX_P15_FILE="$f" "$p15" check >/dev/null; t "an open row fails check" test $? -eq 1
CCSBX_P15_FILE="$f" "$p15" mark A dropped "removed" >/dev/null
CCSBX_P15_FILE="$f" "$p15" check >/dev/null; t "marking it dropped makes check pass" test $? -eq 0
CCSBX_P15_FILE="$f" "$p15" mark NOPE done >/dev/null 2>&1; t "marking an unknown row is refused" test $? -eq 1
CCSBX_P15_FILE="$tmp/none" "$p15" check >/dev/null 2>&1; t "a missing status file fails check (the install stays refused)" test $? -eq 1

echo "# $pass passed, $fail failed"
[ "$fail" -eq 0 ]
