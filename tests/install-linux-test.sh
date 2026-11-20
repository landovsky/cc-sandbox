#!/usr/bin/env bash
# install-linux-test.sh — tests for units/install-linux.sh, units/p15 and units/lib/sudoers_strip.py.
# Why: the installer can lock Tomas out of his own machine, so its refusals and restores are proven without root.
# Hermetic and rootless: the installer's test hooks put every path under a temp dir
# (CCSBX_ROOT_PREFIX) and RECORD each command instead of running it (CCSBX_EXEC=record:FILE), and
# every command that could change the host (sudo, snap, lxc, apt, docker, systemctl, gpasswd …)
# is a stub on a private PATH that logs a call, which the tests assert never happens.
# Usage: tests/install-linux-test.sh [--help]
set -uo pipefail
case "${1:-}" in -h|--help) sed -n '2,7p' "$0"; exit 0;; esac
here="$(cd "$(dirname "$0")/.." && pwd)"; inst="$here/units/install-linux.sh"; p15="$here/units/p15"
strip="$here/units/lib/sudoers_strip.py"
tmp="$(mktemp -d)" || exit 2; [ -d "$tmp" ] || exit 2; trap 'rm -rf "$tmp"' EXIT
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
  printf 'hub-secret-value-not-printed\n' > "$d/hubsecret"
  printf 'id\tstate\twhat\taction\nA5\tdone\trunners\tmoved\nB-x\tdropped\tx\tgone\n' > "$d/p15.tsv"
  echo "$d"
}
ins() {                                  # ins FXDIR ARGS… — run the installer inside the fake root, recording commands
  local d="$1"; shift
  CCSBX_ROOT_PREFIX="$d/root" CCSBX_EXEC="record:$d/rec/cmds" CCSBX_BIN_DIR="$d/dist" CCSBX_P15_FILE="$d/p15.tsv" \
  CCSBX_SIDE_IMAGE="${SIDEIMG-ghcr.io/landovsky/ccsbx@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb}" CCSBX_HUB_SECRET_FROM="${HUBSRC-$d/hubsecret}" CCSBX_OWNER=tomas CCSBX_BACKUP_DIR=/var/backups/ccsbx-install CCSBX_B9_CMD="${B9:-true}" CCSBX_IMAGE="${IMG:-ghcr.io/landovsky/ccsbx-fence@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa}" "$inst" "$@"
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
t "the old sudoers file is backed up before it is changed" test -f "$d/root/var/backups/ccsbx-install/original/etc/sudoers.d/90-tomas-nopasswd"
t "the backup holds the ORIGINAL content" grep -q 'dokku' "$d/root/var/backups/ccsbx-install/original/etc/sudoers.d/90-tomas-nopasswd"
t "the allowed-roots file lists the owner's git root" grep -q '/git"' "$d/root/etc/ccsbx/ccsbx-root.toml"
t "the keys directory is made 0700 for ccsbx-keys" bash -c 'grep -q "chmod 0700 /var/lib/ccsbx-keys" "$1" && grep -q "chown ccsbx-keys:ccsbx-keys /var/lib/ccsbx-keys" "$1"' _ "$cmds"
t "the signer unit has no network and no key outside its own user" bash -c 'grep -q "RestrictAddressFamilies=AF_UNIX" "$1/root/etc/systemd/system/ccsbx-signer.service" && grep -q "User=ccsbx-keys" "$1/root/etc/systemd/system/ccsbx-signer.service"' _ "$d"
t "hostd's unit does not run as the key user" bash -c '! grep -q "ccsbx-keys" <(grep -E "^(User|Group)=" "$1/root/etc/systemd/system/ccsbx-hostd.service")' _ "$d"
t "no firewall rule is added: hostd is loopback-only (least privilege)" bash -c '! grep -q "ufw" "$1"' _ "$cmds"
t "the real system was not touched: the only stub calls were read-only probes (snap list)" bash -c '! grep -vE "^(snap list lxd|systemctl list-unit-files lxd-installer.socket)$" "$1"' _ "$calls"

echo "# uninstall puts the sudoers file back"
ins "$d" uninstall --yes >"$d/uout" 2>"$d/uerr"; rc=$?
t "uninstall exits 0" bash -c '[ '"$rc"' -eq 0 ] || { cat "$1" >&2; exit 1; }' _ "$d/uerr"
t "uninstall copies the backed-up sudoers files back (recorded: the hook doesn't execute)" grep -q 'cp -a /var/backups/ccsbx-install/original/etc/sudoers.d/90-tomas-nopasswd /etc/sudoers.d/90-tomas-nopasswd' "$d/rec/cmds"
t "…and the backup it copies from still holds the original (dokku) line" grep -q dokku "$d/root/var/backups/ccsbx-install/original/etc/sudoers.d/90-tomas-nopasswd"
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


echo "# review M1: hostd must be able to reach the signer's socket, and the owner hostd's"
sig="$here/units/systemd/ccsbx-signer.service"; sck="$here/units/systemd/ccsbx-signer.socket"; hd="$here/units/systemd/ccsbx-hostd.service"
t "the signer socket is owned ccsbx-keys, grouped like hostd's own group, mode 0660 (hostd can connect)" bash -c '
  g="$(sed -n "s/^Group=//p" "$3")"
  grep -q "^SocketUser=ccsbx-keys" "$2" && grep -q "^SocketGroup=$g\$" "$2" && grep -q "^SocketMode=0660" "$2" && grep -q "^DirectoryMode=0755" "$2"' _ "$sig" "$sck" "$hd"
t "the signer no longer makes its own 0750 directory that only ccsbx-keys could enter" bash -c '! grep -q "^RuntimeDirectoryMode=0750" "$1" && ! grep -q -- "--socket-group" "$1"' _ "$sig"
t "hostd is in the group it chgrps its own socket to (a process can only chgrp to a group it is in)" bash -c 'grep -q "^SupplementaryGroups=ccsbx-clients" "$1" && grep -q -- "--socket-group ccsbx-clients" "$1"' _ "$hd"
t "hostd waits for the signer socket" bash -c 'grep -q "^Requires=ccsbx-signer.socket" "$1"' _ "$hd"
d="$(fx m1)"; ins "$d" install --yes >/dev/null 2>&1
t "install writes the socket unit and enables the SOCKET (systemd creates it as root)" bash -c 'test -f "$1/root/etc/systemd/system/ccsbx-signer.socket" && grep -q "enable --now ccsbx-signer.socket" "$1/rec/cmds"' _ "$d"
t "check does NOT call it ok when the sockets are not there (the old check only looked at is-active)" bash -c 'out="$(CCSBX_ROOT_PREFIX="$1/root" CCSBX_EXEC="record:$1/x" CCSBX_OWNER=tomas "$2" check 2>&1)"; grep -q "^FAIL  signer socket" <<<"$out" && grep -q "^FAIL  hostd socket" <<<"$out"' _ "$d" "$inst"
me="$(id -un)"; mkdir -p "$d/root/run/ccsbx-api"
python3 -I -c 'import socket,sys,time
s=socket.socket(socket.AF_UNIX); s.bind(sys.argv[1]); s.listen(1); time.sleep(30)' "$d/root/run/ccsbx-api/hostd.sock" & lpid=$!
for _ in 1 2 3 4 5 6 7 8 9 10; do [ -S "$d/root/run/ccsbx-api/hostd.sock" ] && break; sleep 0.2; done
out="$(CCSBX_ROOT_PREFIX="$d/root" CCSBX_EXEC="record:$d/x" CCSBX_OWNER="$me" "$inst" check 2>&1)"
t "check actually CONNECTS to hostd's socket as the owner" grep -q "^ok    hostd socket: $me can connect" <<<"$out"
kill "$lpid" 2>/dev/null; wait "$lpid" 2>/dev/null

echo "# review M2: ccsbx-root runs inside hostd's sandbox, so /home must be visible to it"
t "hostd's ProtectHome is read-only, not yes (the allowed roots are under /home)" bash -c 'grep -q "^ProtectHome=read-only" "$1" && ! grep -q "^ProtectHome=yes" "$1"' _ "$hd"
t "the unit says which choice was made and why" grep -q 'review M2' "$hd"

echo "# review M3: under sudo HOME is /root; the P15 file must come from the owner's home"
me="$(id -un)"; myhome="$(getent passwd "$me" | cut -d: -f6)"
out="$(HOME=/root CCSBX_OWNER="$me" "$inst" plan 2>&1)"
t "plan resolves the P15 file from the owner's home, not /root, and prints it" bash -c 'grep -q "P15 status file: $2/cc-tree/cc-sandbox/nodes/4vwb/p15-status.tsv" <<<"$1" && ! grep -q "/root/cc-tree" <<<"$1"' _ "$out" "$myhome"
out="$(env -u CCSBX_P15_FILE HOME=/root SUDO_USER="$me" "$p15" check 2>&1)"
t "units/p15 itself looks in the invoking user's home when HOME is /root (it finds the real file there, or names that path)" bash -c '! grep -q "/root/" <<<"$1"' _ "$out"

echo "# review M4: one originals store, written once, never overwritten"
d="$(fx m4)"; : > "$calls"
CCSBX_TEST_VISUDO_RC=1 ins "$d" install --yes >"$d/out" 2>"$d/err"; rc=$?
t "S10: a combined sudoers config that does not parse stops the install (exit 1)" test "$rc" -eq 1
t "S10: …and the sudoers file is put back from the first backup, the new rule removed" bash -c 'grep -q dokku "$1/root/etc/sudoers.d/90-tomas-nopasswd" && [ ! -e "$1/root/etc/sudoers.d/90-ccsbx-root" ]' _ "$d"
t "M4: the failed install says it is partial and how to undo it" grep -q 'PARTIAL INSTALL' "$d/err"
t "M4: the first backup exists although the install died" test -d "$d/root/var/backups/ccsbx-install/original"
ins "$d" uninstall --yes >"$d/uout" 2>"$d/uerr"; rc=$?
t "M4: uninstall after the partial install works (it finds the first backup)" bash -c '[ '"$rc"' -eq 0 ] || { cat "$1" >&2; exit 1; }' _ "$d/uerr"
t "M4: …and restores the sudoers file from it" grep -q 'cp -a /var/backups/ccsbx-install/original/etc/sudoers.d/90-tomas-nopasswd /etc/sudoers.d/90-tomas-nopasswd' "$d/rec/cmds"
d="$(fx twice)"
ins "$d" install --yes >/dev/null 2>&1; ins "$d" install --yes >"$d/out2" 2>&1; rc=$?
t "M4: a second install run succeeds" test "$rc" -eq 0
t "M4: …says the first backup was kept" grep -q 'never overwritten' "$d/out2"
t "M4: …and the backup still holds the ORIGINAL grants (the run that stripped them did not replace it)" grep -q dokku "$d/root/var/backups/ccsbx-install/original/etc/sudoers.d/90-tomas-nopasswd"
t "M4: the backup lists what did not exist before (so uninstall can remove it)" grep -q '^/etc/sudoers.d/90-ccsbx-root$' "$d/root/var/backups/ccsbx-install/original/ABSENT"

echo "# review S1: a failed write stops the install, and nothing is changed first"
d="$(fx s1)"; mkdir -p "$d/root/var"; : > "$d/root/var/backups"
ins "$d" install --yes >"$d/out" 2>"$d/err"; rc=$?
t "S1: a backup that cannot be made aborts the install (exit 1)" test "$rc" -eq 1
t "S1: …before the sudoers file was touched" grep -q dokku "$d/root/etc/sudoers.d/90-tomas-nopasswd"
t "S1: …and nothing was recorded as run" test ! -s "$d/rec/cmds"

echo "# review S2/S3: the test hooks cannot reach a real run"
CCSBX_EXEC="record:$tmp/y" "$inst" plan >/dev/null 2>&1; t "S2: record mode without a prefix is refused (exit 2)" test $? -eq 2
CCSBX_B9_CMD=true "$inst" plan >/dev/null 2>&1; t "S3: CCSBX_B9_CMD alone (a stale env var, sudo -E) is refused (exit 3)" test $? -eq 3
CCSBX_P15_CMD=true "$inst" plan >/dev/null 2>&1; t "S3: CCSBX_P15_CMD alone is refused (exit 3)" test $? -eq 3

echo "# review S4: B9 must not pass when docker cannot be asked"
b2="$tmp/bin2"; mkdir -p "$b2"
printf '#!/bin/sh\ncase "$*" in *is-active*) exit 0;; *ExecStart*) echo /usr/bin/hub;; esac\nexit 0\n' > "$b2/systemctl"
printf '#!/bin/sh\necho "permission denied" >&2\nexit 1\n' > "$b2/docker"; chmod +x "$b2/systemctl" "$b2/docker"
out="$(PATH="$b2:$PATH" "$inst" plan 2>&1)"
t "S4: docker ps failing is an UNMET B9 (it used to pass)" bash -c 'grep -q "B9 missing: cannot ask docker" <<<"$1"' _ "$out"

echo "# review S5: every required binary must be listed in SHA256SUMS"
d="$(fx unlisted)"; grep -v ' ccsbx-root$' "$d/dist/SHA256SUMS" > "$d/s" && mv "$d/s" "$d/dist/SHA256SUMS"
ins "$d" install --yes >/dev/null 2>"$d/err"; rc=$?
t "S5: a SHA256SUMS without a line for ccsbx-root is refused (exit 3)" bash -c '[ '"$rc"' -eq 3 ] && grep -q "does not list" "$1"' _ "$d/err"

echo "# review S6: the owner and the allowed roots are validated"
for o in root ALL 'a b' '1x'; do
  d="$(fx owner)"; ins "$d" install --yes --owner "$o" >/dev/null 2>"$d/err"; rc=$?
  t "S6: --owner '$o' is refused (exit 3) and nothing is changed" bash -c '[ '"$rc"' -eq 3 ] && grep -q "owner" "$1" && [ ! -s "$2" ]' _ "$d/err" "$d/rec/cmds"
done
for r in / rel '/a"b' '/home/*' '/x/../y' '/a b'; do
  d="$(fx roots)"; ins "$d" install --yes --allowed-roots "$r" >/dev/null 2>"$d/err"; rc=$?
  t "S6: --allowed-roots '$r' is refused (exit 3)" bash -c '[ '"$rc"' -eq 3 ] && grep -q "allowed root" "$1"' _ "$d/err"
done
d="$(fx roots2)"; ins "$d" install --yes --allowed-roots /srv/a,/srv/b >/dev/null 2>&1
t "S6: two roots land in the root-owned policy file as two quoted entries" grep -q 'allowed_roots = \["/srv/a", "/srv/b"\]' "$d/root/etc/ccsbx/ccsbx-root.toml"

echo "# review S7: uninstall undoes what the runbook says"
d="$(fx un)"; echo "bin ccsbx-deploy" > "$d/dist/ccsbx-deploy"; (cd "$d/dist" && sha256sum ccsbx-root ccsbx-hostd ccsbx-signer ccsbx ccsbx-deploy > SHA256SUMS)
ins "$d" install --yes --with-deploy >/dev/null 2>"$d/err"; ins "$d" uninstall --yes >"$d/uout" 2>&1; rc=$?
t "S7: uninstall exits 0 after an install with --with-deploy" test "$rc" -eq 0
for p in /etc/sudoers.d/91-ccsbx-deploy /usr/local/sbin/ccsbx-deploy /etc/ccsbx/ccsbx-root.toml /etc/sudoers.d/90-ccsbx-root /etc/systemd/system/ccsbx-signer.socket; do
  t "S7: uninstall removes $p" grep -qx "rm -f $p" "$d/rec/cmds"
done
t "S7: uninstall removes the owner's ccsbx-clients membership, and calls no ufw (none was added)" bash -c 'grep -q "^gpasswd -d tomas ccsbx-clients" "$1" && ! grep -q ufw "$1"' _ "$d/rec/cmds"
t "S7: a unit that was never written does not stop the undo (the disable is tried, failure ignored)" grep -q 'failure ignored' "$d/uout"
t "S7: the runbook's list of what uninstall leaves matches the installer's own message" bash -c 'grep -q "NOT undone" "$1"' _ "$d/uout"

echo "# review S8: a backup dir is trusted by root, so it is checked"
d="$(fx s8)"; ins "$d" install --yes >/dev/null 2>&1
mkdir -p "$tmp/elsewhere"; ins "$d" uninstall --yes --backup "$tmp/elsewhere" >/dev/null 2>"$d/err"; rc=$?
t "S8: --backup outside /var/backups/ccsbx-install is refused (exit 3)" bash -c '[ '"$rc"' -eq 3 ] && grep -q "must be a directory under" "$1"' _ "$d/err"
b="$d/root/var/backups/ccsbx-install/evil"; mkdir -p "$b"; chmod 700 "$b"; : > "$b/ABSENT"; echo 'x:x 4755' > "$b/sops.meta"
ins "$d" uninstall --yes --backup /var/backups/ccsbx-install/evil >/dev/null 2>"$d/err"; rc=$?
t "S8: a sops.meta with a setuid mode is refused (exit 3)" bash -c '[ '"$rc"' -eq 3 ] && grep -q "sops.meta" "$1"' _ "$d/err"
chmod 777 "$b"; echo 'root:root 755' > "$b/sops.meta"
ins "$d" uninstall --yes --backup /var/backups/ccsbx-install/evil >/dev/null 2>"$d/err"; rc=$?
t "S8: a world-writable backup dir is refused (exit 3)" bash -c '[ '"$rc"' -eq 3 ] && grep -q "writable" "$1"' _ "$d/err"

echo "# review S12: steps that can fail on this machine come BEFORE the sudoers change"
d="$(fx s12)"; : > "$calls"; ins "$d" install --yes >/dev/null 2>&1; cmds="$d/rec/cmds"
t "S12: no snap remove and no lxd-installer disable when neither exists (the probes said no)" bash -c '! grep -qE "^snap remove|lxd-installer" "$1"' _ "$cmds"
t "S12: no chown of a sops that is not there" bash -c '! grep -q "/usr/local/bin/sops" "$1"' _ "$cmds"
d2="$(fx s12b)"; mkdir -p "$d2/root/usr/local/bin"; ln -s /elsewhere "$d2/root/usr/local/bin/sops"
ins "$d2" install --yes >/dev/null 2>"$d2/err"; rc=$?
t "S12: a sops that is a symlink is refused (exit 3): chown would hit its target" bash -c '[ '"$rc"' -eq 3 ] && grep -q "symlink" "$1"' _ "$d2/err"
d3="$(fx s12c)"; mkdir -p "$d3/root/usr/local/bin"; : > "$d3/root/usr/local/bin/sops"; ins "$d3" install --yes >/dev/null 2>&1
t "S12: sops (when present) is chowned BEFORE the sudoers rule is renamed into place" bash -c 'a="$(grep -n "chown root:root /usr/local/bin/sops" "$1" | cut -d: -f1)"; b="$(grep -n "mv -f /etc/sudoers.d/.90-ccsbx-root.new" "$1" | cut -d: -f1)"; [ -n "$a" ] && [ "$a" -lt "$b" ]' _ "$d3/rec/cmds"
t "S12: sops.meta was recorded in the first backup" grep -q 'root:root\|[a-z]*:[a-z]* [0-9]*' "$d3/root/var/backups/ccsbx-install/original/sops.meta"

echo "# review S13: the P15 gate fails closed on a file that proves nothing"
printf 'id\tstate\twhat\taction\n' > "$tmp/hdr.tsv"; CCSBX_P15_FILE="$tmp/hdr.tsv" "$p15" check >/dev/null 2>&1; t "S13: a header-only file fails check" test $? -eq 1
: > "$tmp/empty.tsv"; CCSBX_P15_FILE="$tmp/empty.tsv" "$p15" check >/dev/null 2>&1; t "S13: an empty file fails check" test $? -eq 1
printf 'A5\topen\tx\ty\n' > "$tmp/nohdr.tsv"; CCSBX_P15_FILE="$tmp/nohdr.tsv" "$p15" check >/dev/null 2>&1; t "S13: a file without the header fails (its first row would be skipped)" test $? -eq 1

echo "# review N3: p15 mark matches the id exactly, keeps backslashes and the file mode"
f="$tmp/n3.tsv"; printf 'id\tstate\twhat\taction\nAB\topen\tx\ty\nA5\topen\tx\ty\n' > "$f"; chmod 640 "$f"
CCSBX_P15_FILE="$f" "$p15" mark 'A.' done >/dev/null 2>&1; t "N3: id 'A.' is not a regex that matches AB (refused, exit 1)" test $? -eq 1
CCSBX_P15_FILE="$f" "$p15" mark A5 done 'path C:\new\tab' >/dev/null 2>&1
t "N3: a note with backslashes is stored as typed" grep -qF 'C:\new\tab' "$f"
t "N3: the file mode survives the rewrite" test "$(stat -c %a "$f")" = 640

echo "# review N1/N2: housekeeping"
t "N1: the signer unit's header sentence is not split by the Why line" bash -c 'l="$(grep -n "It exposes" "$1" | cut -d: -f1)"; sed -n "$((l+1))p" "$1" | grep -q "^# sign(token_claims)"' _ "$sig"
t "N2: the installer sets umask 077 before it writes anything" grep -q '^umask 077' "$inst"

echo "# sudoers_strip.py: every line shape it must keep, strip, or refuse"
kept() { printf '%s\n' "$2" > "$s/k"; local o; o="$("$strip" "$s/k" 2>/dev/null)"; t "keeps: $1" test "$o" = "$2"; }
gone() { printf '%s\n' "$2" > "$s/g"; local o; o="$("$strip" "$s/g" 2>/dev/null)"; t "strips: $1" test -z "$o"; }
refused() { printf '%s\n' "$2" > "$s/r"; "$strip" "$s/r" >/dev/null 2>&1; t "refuses (exit 3): $1" test $? -eq 3; }
N='NOPASSWD:'
kept "a comment that mentions the tag" "# tomas ALL=(ALL) $N /usr/bin/docker"
kept "a Defaults line" "Defaults env_reset"
kept "a rule with no NOPASSWD" "tomas ALL=(ALL) ALL"
kept "another user's unrelated command" "bob ALL=(ALL) $N /usr/bin/foo"
kept "a (user:group) runas" "tomas ALL=(ALL:ALL) $N /usr/bin/systemctl restart nginx"
kept "a negated command" "tomas ALL=(ALL) $N !/usr/bin/su"
kept "a host-restricted rule" "tomas hp-ubuntu=(root) $N /usr/bin/foo"
gone "only docker" "tomas ALL=(ALL) $N /usr/bin/docker"
gone "docker-compose (docker*)" "tomas ALL=(ALL) $N /usr/bin/docker-compose"
gone "apt, dpkg, snap, apt-get" "tomas ALL=(ALL) $N /usr/bin/apt, /usr/bin/dpkg, /usr/bin/snap, /usr/bin/apt-get"
gone "dokku and sops" "tomas ALL=(ALL) $N /usr/bin/dokku, /usr/local/bin/sops"
gone "a group rule with only docker" "%admin ALL=(ALL) $N /usr/bin/docker"
printf '%s\n' "tomas ALL=(ALL) $N /usr/bin/docker, /usr/bin/foo --bar" > "$s/m"
t "strips only the removable part of a mixed line and keeps the arguments of the rest" test "$("$strip" "$s/m" 2>/dev/null)" = "tomas ALL=(ALL) $N /usr/bin/foo --bar"
refused "an alias (its members are not visible)" "tomas ALL=(ALL) $N DOCKER_CMDS"
refused "env as a wrapper" "tomas ALL=(ALL) $N /usr/bin/env docker"
refused "sudoedit" "tomas ALL=(ALL) $N sudoedit /etc/x"
refused "a sha256 digest entry" "tomas ALL=(ALL) $N sha256:abcd /usr/bin/docker"
refused "two tag groups on one line" "tomas ALL=(ALL) $N /usr/bin/foo, PASSWD: /usr/bin/bar"
refused "ALL" "tomas ALL=(ALL) $N ALL"
refused "a continuation line" "tomas ALL=(ALL) $N /usr/bin/foo, \\"
refused "no command at all" "tomas ALL=(ALL) $N "

echo "# review R1/R3: hostd's real command line, DONE only when the services are up, undo order"
t "R1: hostd is started as 'ccsbx-hostd run' with loopback-only --listen and a token file (no ':7480')" bash -c 'l="$(grep "^ExecStart=" "$1")"; grep -q "ccsbx-hostd run " <<<"$l" && grep -q -- "--listen 127.0.0.1:7480" <<<"$l" && grep -q -- "--listen-token-file " <<<"$l" && grep -q -- "--local-socket /run/ccsbx-api/hostd.sock" <<<"$l" && grep -q -- "--signer /run/ccsbx-signer/sign.sock" <<<"$l" && ! grep -q -- "--listen :7480" <<<"$l"' _ "$hd"
d="$(fx r1)"; CCSBX_TEST_POSTCHECK_RC=1 ins "$d" install --yes >"$d/out" 2>"$d/err"; rc=$?
t "R1: a crash-looping hostd (post-install check fails) means exit 1, NOT DONE, and no DONE line" bash -c '[ '"$rc"' -eq 1 ] && grep -q "NOT DONE" "$1" && ! grep -q "^DONE" "$2"' _ "$d/err" "$d/out"
t "R1: …and the backup stays in-progress, so uninstall is still the documented way back" grep -qx in-progress "$d/root/var/backups/ccsbx-install/original/STATE"
d="$(fx r1ok)"; ins "$d" install --yes >"$d/out" 2>&1
t "R1: when the services are up the install prints DONE and marks the backup complete" bash -c 'grep -q "^DONE" "$1/out" && grep -qx complete "$1/root/var/backups/ccsbx-install/original/STATE"' _ "$d"
t "R1: plan shows the post-install check" bash -c 'grep -q "post-install check" <<<"$1"' _ "$("$inst" plan 2>&1)"
d="$(fx r3)"; ins "$d" install --yes >/dev/null 2>&1; ins "$d" uninstall --yes >/dev/null 2>&1
t "R3: the signer socket and service are stopped and disabled BEFORE any unit file is removed" bash -c 'c="$1"; a="$(grep -n "disable --now ccsbx-signer.socket" "$c" | head -1 | cut -d: -f1)"; b="$(grep -n "disable --now ccsbx-signer.service" "$c" | head -1 | cut -d: -f1)"; r="$(grep -n "^rm -f /etc/systemd/system/ccsbx-signer" "$c" | head -1 | cut -d: -f1)"; [ -n "$a" ] && [ -n "$b" ] && [ -n "$r" ] && [ "$a" -lt "$r" ] && [ "$b" -lt "$r" ]' _ "$d/rec/cmds"
t "R3: hostd is stopped before its unit file is removed" bash -c 'c="$1"; a="$(grep -n "disable --now ccsbx-hostd" "$c" | tail -1 | cut -d: -f1)"; r="$(grep -n "^rm -f /etc/systemd/system/ccsbx-hostd" "$c" | head -1 | cut -d: -f1)"; [ "$a" -lt "$r" ]' _ "$d/rec/cmds"

echo "# hostd's flags are exactly the ones the hostd binary accepts (unknown flags are fatal there)"
help="$tmp/hostd-help.txt"; src="fixture tests/fixtures/ccsbx-hostd-run-help.txt"
cp "$here/tests/fixtures/ccsbx-hostd-run-help.txt" "$help"
if [ -x "$here/dist/ccsbx-hostd" ] && timeout 5 "$here/dist/ccsbx-hostd" run --help >"$tmp/real-help.txt" 2>&1 && [ -s "$tmp/real-help.txt" ]; then help="$tmp/real-help.txt"; src="dist/ccsbx-hostd run --help"; fi
echo "# (checked against: $src)"
bad="$(grep '^ExecStart=' "$hd" | grep -o ' --[a-z-]*' | tr -d ' ' | sed 's/^--//' | while read -r f; do grep -qE "(^|[[:space:]])-{1,2}$f([[:space:]]|\$)" "$help" || echo "$f"; done)"
t "R1: every flag in hostd's ExecStart is a flag 'ccsbx-hostd run' has ($src)${bad:+: unknown: $bad}" test -z "$bad"
t "R1: the unit sets the tmux socket to an absolute path in /run/ccsbx and leaves --ccmsg-bin unset" bash -c 'grep -q -- "--tmux-socket /run/ccsbx/tmux.sock" "$1" && ! grep "^ExecStart=" "$1" | grep -q -- "--ccmsg-bin"' _ "$hd"
tmx="$here/units/systemd/ccsbx-tmux.service"
t "R1b: /run/ccsbx is made by the tmux unit, ccsbx-only 0700, no group, no chgrp (a group-writable tmux socket lets any owner-uid process type into fenced panes)" bash -c 'grep -q "^RuntimeDirectory=ccsbx\$" "$2" && grep -q "^RuntimeDirectoryMode=0700" "$2" && ! grep -q "chgrp" <(grep -v "^#" "$2") && ! grep -q "^RuntimeDirectory=ccsbx " "$1"' _ "$hd" "$tmx"
t "R1b: the API socket lives in its own 0755 directory and keeps --socket-group ccsbx-clients (0660)" bash -c 'grep -q "^RuntimeDirectory=ccsbx-api\$" "$1" && grep -q "ExecStartPre=+/usr/bin/chmod 0755 /run/ccsbx-api" "$1" && grep "^ExecStart=" "$1" | grep -q -- "--local-socket /run/ccsbx-api/hostd.sock --socket-group ccsbx-clients" && grep -q "^ReadWritePaths=.*/run/ccsbx-api" "$1"' _ "$hd"
d="$(fx r1b)"; ins "$d" install --yes >/dev/null 2>&1
t "R1b: no separate sudoers line for attach (the one ccsbx-root rule already covers the verb), and none for a group" bash -c 'f="$1/root/etc/sudoers.d/.90-ccsbx-root.new"; ! grep -q "attach" "$f" && ! grep -q "^%" "$f"' _ "$d"
d="$(fx tok)"; ins "$d" install --yes >/dev/null 2>&1
t "R1: the installer makes hostd's token file (0600, as ccsbx) before it starts hostd" bash -c 'a="$(grep -n "listen.token" "$1" | head -1 | cut -d: -f1)"; b="$(grep -n "enable --now ccsbx-hostd" "$1" | cut -d: -f1)"; grep -q "umask..077" "$1" && [ "$a" -lt "$b" ]' _ "$d/rec/cmds"

echo "# signer alignment (n1-b-signer 4158d88): flags, scope file, group, a real signing probe"
shelp="$here/tests/fixtures/ccsbx-signer-help.txt"; ssrc="fixture tests/fixtures/ccsbx-signer-help.txt"
if [ -x "$here/dist/ccsbx-signer" ] && timeout 5 "$here/dist/ccsbx-signer" --help >"$tmp/signer-real-help.txt" 2>&1 && [ -s "$tmp/signer-real-help.txt" ]; then shelp="$tmp/signer-real-help.txt"; ssrc="dist/ccsbx-signer --help"; fi
badf="$(grep '^ExecStart=' "$sig" | grep -o ' --[a-z-]*' | tr -d ' ' | sed 's/^--//' | while read -r f; do grep -qE "(^|[[:space:]])-{1,2}$f([[:space:]]|\$)" "$shelp" || echo "$f"; done)"
t "signer: every flag in the unit's ExecStart is a flag ccsbx-signer has ($ssrc)${badf:+: unknown: $badf}" test -z "$badf"
d="$(fx sg)"; ins "$d" install --yes --machine hp-ubuntu >/dev/null 2>&1; sc="$d/rec/cmds"
t "signer: the scope is written BY THE SIGNER (--write-scope, as ccsbx-keys); the CLI never reads machine.key" bash -c 'l="$(grep -- "--write-scope" "$1")"; grep -q "runuser -u ccsbx-keys" <<<"$l" && grep -q -- "--owners tomas" <<<"$l" && grep -q -- "--machines hp-ubuntu" <<<"$l" && ! grep -q "token pubkey\|--key-file" "$1"' _ "$sc"
t "signer: --write-scope's flags are flags ccsbx-signer has ($ssrc)" bash -c 'for f in keys write-scope owners machines pairs init; do grep -qE "(^|[[:space:]])-{1,2}$f([[:space:]]|\$)" "$1" || { echo "no --$f"; exit 1; }; done' _ "$shelp"
t "signer: the pairs come from each profile's OWN billings (dev-default api|stub, e2e-default stub), not a product" bash -c 'grep -- "--write-scope" "$1" | grep -q -- "--pairs dev-default:api\\|stub,e2e-default:stub"' _ "$sc"
t "signer: no subscription anywhere in the scope until a profile allows it" bash -c '! grep -- "--write-scope" "$1" | grep -q subscription' _ "$sc"
t "signer: the scope exists BEFORE the signer socket is enabled (else the service exits 2)" bash -c 'a="$(grep -n -- "--write-scope" "$1" | head -1 | cut -d: -f1)"; b="$(grep -n "enable --now ccsbx-signer.socket" "$1" | cut -d: -f1)"; [ -n "$a" ] && [ "$a" -lt "$b" ]' _ "$sc"
t "signer: the machine key is made (--init) before the scope is written" bash -c 'a="$(grep -n -- "ccsbx-signer --keys /var/lib/ccsbx-keys --init" "$1" | head -1 | cut -d: -f1)"; b="$(grep -n -- "--write-scope" "$1" | head -1 | cut -d: -f1)"; [ -n "$a" ] && [ "$a" -lt "$b" ]' _ "$sc"
t "signer: the unit has the reviewer's hardening (LimitCORE, ProtectProc, RestrictSUIDSGID, PrivateNetwork)" bash -c 'for k in LimitCORE=0 ProtectProc=invisible RestrictSUIDSGID=yes PrivateNetwork=yes; do grep -q "^$k" "$1" || exit 1; done' _ "$sig"
if ! getent group ccsbx >/dev/null 2>&1; then
  t "signer: group ccsbx is created BEFORE the signer starts (its --allow-group default)" bash -c 'a="$(grep -n "groupadd --system ccsbx$" "$1" | head -1 | cut -d: -f1)"; b="$(grep -n "enable --now ccsbx-signer.socket" "$1" | cut -d: -f1)"; [ -n "$a" ] && [ "$a" -lt "$b" ]' _ "$sc"
else echo "ok   - (skipped: group ccsbx exists on this host)"; fi
for bad_arg in "--pairs|bad prof" "--pairs|dev:subscription,x" "--machine|Bad_Host" "--pairs|dev:api;x"; do
  d="$(fx sgbad)"; ins "$d" install --yes "${bad_arg%%|*}" "${bad_arg#*|}" >/dev/null 2>"$d/err"; rc=$?
  t "signer: ${bad_arg%%|*} '${bad_arg#*|}' is refused (exit 3), nothing changed" bash -c '[ '"$rc"' -eq 3 ] && [ ! -s "$1" ]' _ "$d/rec/cmds"
done
t "signer: plan prints the scope it will write" bash -c 'grep -q "signer scope:    owners=tomas machines=hp-ubuntu,future-two pairs=dev-default:api|stub,e2e-default:stub" <<<"$1"' _ "$(CCSBX_OWNER=tomas CCSBX_HUB_SECRET_FROM=/etc/hostname "$inst" plan --machine hp-ubuntu 2>&1)"
# check asks the signer to sign one in-scope claim (a stub signer on a unix socket in the fake root)
d="$(fx sgchk)"; mkdir -p "$d/root/run/ccsbx-signer"
cat > "$tmp/fake-signer.py" <<'PY'
import json, socket, sys
path, mode, seen = sys.argv[1], sys.argv[2], sys.argv[3]
s = socket.socket(socket.AF_UNIX); s.bind(path); s.listen(1)
c, _ = s.accept(); d = b""
while True:                      # like the real signer: exactly one JSON value, no half-close needed
    d += c.recv(4096)
    try: json.loads(d); break
    except ValueError: continue
open(seen, "wb").write(d)
c.sendall(json.dumps({"token": "x.y.z"} if mode == "ok" else {"error": "claims outside this key's scope"}).encode()); c.close()
PY
for mode in ok refuse; do
  rm -f "$d/root/run/ccsbx-signer/sign.sock"
  python3 -I "$tmp/fake-signer.py" "$d/root/run/ccsbx-signer/sign.sock" "$mode" "$d/seen-$mode" & spid=$!
  for _ in 1 2 3 4 5 6 7 8 9 10; do [ -S "$d/root/run/ccsbx-signer/sign.sock" ] && break; sleep 0.2; done
  out="$(CCSBX_ROOT_PREFIX="$d/root" CCSBX_EXEC="record:$d/x" CCSBX_OWNER=tomas "$inst" check --machine hp-ubuntu --pairs 'dev-default:api|stub' 2>&1)"
  kill "$spid" 2>/dev/null; wait "$spid" 2>/dev/null
  if [ "$mode" = ok ]; then
    t "check: a token for the in-scope test claim is an ok line" grep -q '^ok    signer returned a token for the in-scope test claim' <<<"$out"
    t "check: the claim it sent has the owner/machine sub, a scope profile and billing, and a cnf" bash -c 'python3 -I -c "import base64,json,sys; c=json.load(open(sys.argv[1])); assert c[\"sub\"].startswith(\"tomas/hp-ubuntu/\") and c[\"billing\"]==\"api\" and c[\"profile\"]==\"dev-default\" and c[\"aud\"] and \"=\" not in c[\"cnf\"] and \"+\" not in c[\"cnf\"] and \"/\" not in c[\"cnf\"] and len(base64.urlsafe_b64decode(c[\"cnf\"]+\"=\"))==32 and \"iat\" not in c" "$1"' _ "$d/seen-ok"
  else t "check: a signer that refuses the in-scope claim is a FAIL with its reason (a connect alone is not enough)" grep -q "^FAIL  signer refused or failed the in-scope test claim: claims outside" <<<"$out"; fi
done

echo "# PR #8 N4: ccsbx-root gets a clean environment"
d="$(fx env)"; ins "$d" install --yes >/dev/null 2>&1; sf="$d/root/etc/sudoers.d/.90-ccsbx-root.new"
t "N4: the sudoers file sets env_reset,!setenv for ccsbx-root, exactly" grep -qx 'Defaults!/usr/local/sbin/ccsbx-root env_reset,!setenv' "$sf"
t "N4: …and it is a Defaults line, not an extra grant (still exactly two NOPASSWD rules)" bash -c '[ "$(grep -v "^#" "$1" | grep -c "NOPASSWD")" -eq 2 ] && ! grep -v "^#" "$1" | grep -E "^Defaults" | grep -q NOPASSWD' _ "$sf"
t "N4: the file is still validated by visudo before it is renamed into place" bash -c 'a="$(grep -n "visudo -cf /etc/sudoers.d/.90-ccsbx-root.new" "$1" | head -1 | cut -d: -f1)"; b="$(grep -n "mv -f /etc/sudoers.d/.90-ccsbx-root.new" "$1" | head -1 | cut -d: -f1)"; [ -n "$a" ] && [ "$a" -lt "$b" ]' _ "$d/rec/cmds"
t "N4: plan says ccsbx-root gets a clean environment" bash -c 'grep -q "CLEAN environment" <<<"$1" && grep -q "env_reset,!setenv" <<<"$1"' _ "$("$inst" plan 2>&1)"

echo "# ccsbx-tmux.service: the tmux server in the host mount namespace, hostd only connects"
t "tmux: the unit has no mount-namespace sandboxing and leaves NoNewPrivileges off (its panes run sudo ccsbx-root)" bash -c 'u="$(grep -v "^#" "$1")"; for k in ProtectSystem ProtectHome PrivateTmp PrivateDevices ReadOnlyPaths ReadWritePaths InaccessiblePaths RestrictNamespaces ProtectKernel ProtectControlGroups MountFlags; do ! grep -q "^$k" <<<"$u" || { echo "has $k"; exit 1; }; done; grep -q "^NoNewPrivileges=no" <<<"$u"' _ "$tmx"
t "tmux: runs as uid ccsbx, a foreground server on /run/ccsbx/tmux.sock with no config, socket 0600 (UMask 0077), LimitCORE=0" bash -c 'grep -q "^User=ccsbx" "$1" && grep -q "^ExecStart=/usr/bin/tmux -D -S /run/ccsbx/tmux.sock -f /dev/null" "$1" && grep -q "^UMask=0077" "$1" && grep -q "^LimitCORE=0" "$1"' _ "$tmx"
t "tmux: hostd requires and follows it, and no longer makes /run/ccsbx or starts a server" bash -c 'grep -q "^Requires=.*ccsbx-tmux.service" "$1" && grep -q "^After=.*ccsbx-tmux.service" "$1"' _ "$hd"
d="$(fx tmux)"; ins "$d" install --yes >/dev/null 2>&1; c="$d/rec/cmds"
t "tmux: the installer writes the unit and starts it BEFORE hostd" bash -c 'test -f "$1/root/etc/systemd/system/ccsbx-tmux.service" && a="$(grep -n "enable --now ccsbx-tmux.service" "$2" | cut -d: -f1)" && b="$(grep -n "enable --now ccsbx-hostd.service" "$2" | cut -d: -f1)" && [ "$a" -lt "$b" ]' _ "$d" "$c"
ins "$d" uninstall --yes >/dev/null 2>&1
t "tmux: …the disable comes before the rm of its file, and its file is removed (it did not exist before)" bash -c 'a="$(grep -n "disable --now ccsbx-tmux.service" "$1" | head -1 | cut -d: -f1)"; r="$(grep -n "^rm -f /etc/systemd/system/ccsbx-tmux.service" "$1" | head -1 | cut -d: -f1)"; [ -n "$a" ] && [ -n "$r" ] && [ "$a" -lt "$r" ]' _ "$c"
# check: the mount namespace and the socket
d="$(fx tmuxchk)"; mkdir -p "$d/root/run/ccsbx"
run_chk() { CCSBX_ROOT_PREFIX="$d/root" CCSBX_EXEC="record:$d/x" CCSBX_OWNER=tomas CCSBX_TEST_TMUX_PID="$1" CCSBX_TEST_HOST_NS_PID="$2" "$inst" check 2>&1; }
out="$(run_chk $$ $$)"
t "tmux check: the same mount namespace as the host is an ok line" grep -q '^ok    ccsbx-tmux is in the host mount namespace' <<<"$out"
t "tmux check: a missing socket is a FAIL (it must be ccsbx:600)" grep -q '^FAIL  the tmux socket is missing' <<<"$out"
if unshare -Urm true 2>/dev/null; then
  unshare -Urm sleep 20 & upid=$!; sleep 0.5
  if [ -r "/proc/$upid/ns/mnt" ]; then
    out="$(run_chk "$upid" $$)"
    t "tmux check: a unit in a PRIVATE mount namespace is a FAIL" grep -q '^FAIL  ccsbx-tmux is NOT in the host mount namespace' <<<"$out"
  else echo "ok   - (skipped: cannot read the private namespace)"; fi
  kill "$upid" 2>/dev/null; wait "$upid" 2>/dev/null
else echo "ok   - (skipped: unprivileged user namespaces are not available here)"; fi

echo "# Gate M review: the post-install check asks the signer to sign; check verifies the seccomp profile; tmux comment"
d="$(fx g)"; CCSBX_TEST_SIGN_RC=1 ins "$d" install --yes >"$d/out" 2>"$d/err"; rc=$?
t "G: a signer that does not sign the test claim (bad scope) means NOT DONE, even though its socket exists" bash -c '[ '"$rc"' -eq 1 ] && grep -q "did not sign" "$1" && ! grep -q "^DONE" "$2"' _ "$d/err" "$d/out"
d="$(fx gok)"; ins "$d" install --yes >"$d/out" 2>&1; t "G: …and DONE when it signs" bash -c 'grep -q "^DONE" "$1" && grep -qx sign-probe "$2"' _ "$d/out" "$d/rec/cmds"
out="$(CCSBX_ROOT_PREFIX="$d/root" CCSBX_EXEC="record:$d/x" CCSBX_OWNER=tomas "$inst" check 2>&1)"
t "G: check has a seccomp profile line (a FAIL here, nothing installed in the fake root)" grep -q 'seccomp profile verifies' <<<"$out"
t "G: the tmux unit comment no longer claims -f /dev/null gives no prefix or status line, and names the new-window cost" bash -c '! grep -q "no config file, no status line, no prefix" "$1" && grep -q "built-in defaults" "$1" && grep -q "new-window" "$1"' _ "$tmx"

echo "# helper mode (PR #12): hostd's run dir, root-owned pins, one source for the roots, image by digest, machine name"
DIG="ghcr.io/landovsky/ccsbx-fence@sha256:$(printf 'b%.0s' $(seq 64))"
t "helper: hostd's ExecStart passes --run-dir, --pins, --worktree-root, --image and --machine, all flags the binary has" bash -c 'l="$(grep "^ExecStart=" "$1")"; for f in run-dir pins worktree-root image machine; do grep -q -- "--$f " <<<"$l" || { echo "no --$f"; exit 1; }; done' _ "$hd"
t "helper: --profiles is gone from the unit (profiles are embedded; an unknown flag would be fatal)" bash -c '! grep "^ExecStart=" "$1" | grep -q -- "--profiles"' _ "$hd"
t "helper: the unit points at the ROOT-OWNED pins copy, never at the owner-writable repo" bash -c 'l="$(grep "^ExecStart=" "$1")"; grep -q -- "--pins /etc/ccsbx/pins.toml" <<<"$l" && ! grep -q "/home/" <<<"$l"' _ "$hd"
t "helper: hostd can write replies under the run dir (ReadWritePaths) and reads it through group ccsbx" grep -q '^ReadWritePaths=.*/var/lib/ccsbx-run' "$hd"
d="$(fx helper)"; IMG="$DIG" ins "$d" install --yes --allowed-roots /srv/a,/srv/b --machine hp-ubuntu >"$d/out" 2>&1; u="$d/root/etc/systemd/system/ccsbx-hostd.service"
t "helper: the installed unit has the SAME list as --worktree-root, the digest image and the machine, no placeholder left" bash -c 'grep -q -- "--worktree-root /srv/a,/srv/b --image $2 --machine hp-ubuntu" "$1" && ! grep -q "@[A-Z_]*@" "$1"' _ "$u" "$DIG"
t "helper: …and ccsbx-root.toml lists the same roots and the same machine (one source, written to both)" bash -c 'grep -q "allowed_roots = \\[\"/srv/a\", \"/srv/b\"\\]" "$1" && grep -q "^machine = \"hp-ubuntu\"" "$1"' _ "$d/root/etc/ccsbx/ccsbx-root.toml"
t "helper: pins.toml is copied to /etc/ccsbx/pins.toml unchanged; no profiles directory is made" bash -c 'cmp -s "$2/pins.toml" "$1/root/etc/ccsbx/pins.toml" && [ ! -e "$1/root/etc/ccsbx/profiles" ]' _ "$d" "$here"
t "helper: the signer pairs still come from the repo's profiles" bash -c 'grep -- "--write-scope" "$1" | grep -q -- "--pairs dev-default:api\\|stub,e2e-default:stub"' _ "$d/rec/cmds"
t "helper: the run dir exists (root 0755, as ccsbx-root makes it) before hostd starts" bash -c 'a="$(grep -n "install -d -o root -g root -m 0755 /var/lib/ccsbx-run" "$1" | cut -d: -f1)"; b="$(grep -n "enable --now ccsbx-hostd" "$1" | cut -d: -f1)"; [ -n "$a" ] && [ "$a" -lt "$b" ]' _ "$d/rec/cmds"
t "helper: pins.toml is in the backup's ABSENT list, so uninstall removes it" grep -qx /etc/ccsbx/pins.toml "$d/root/var/backups/ccsbx-install/original/ABSENT"
ins "$d" uninstall --yes >/dev/null 2>&1
t "helper: uninstall removes the pins copy" grep -qx "rm -f /etc/ccsbx/pins.toml" "$d/rec/cmds"
d="$(fx helper2)"; ins "$d" install --yes >/dev/null 2>&1
t "helper: the default allowed roots are the owner's ~/git and ~/wt, written to ccsbx-root.toml" bash -c 'grep -q "^allowed_roots = \\[\"[^\"]*/git\", \"[^\"]*/wt\"\\]\$" "$1"' _ "$d/root/etc/ccsbx/ccsbx-root.toml"
t "helper: hostd's --worktree-root is exactly ccsbx-root.toml's allowed_roots list" bash -c 'u="$1/root/etc/systemd/system/ccsbx-hostd.service"; w="$(grep -o -- "--worktree-root [^ ]*" "$u" | cut -d" " -f2)"; r="$(sed -n "s/^allowed_roots = \\[\\(.*\\)\\]\$/\\1/p" "$1/root/etc/ccsbx/ccsbx-root.toml" | tr -d "\" ")"; [ -n "$w" ] && [ "$w" = "$r" ] && grep -q "/git,.*/wt\$" <<<"$w"' _ "$d"
echo "# the image is passed BY DIGEST: hostd and ccsbx-root refuse every start of a tag (image_not_pinned)"
for bad_img in 'ccsbx-fence:dev' 'ghcr.io/landovsky/ccsbx-fence:latest' 'ghcr.io/other/ccsbx-fence@sha256:'"$(printf 'c%.0s' $(seq 64))" 'ghcr.io/landovsky/ccsbx-fence@sha256:abc' 'bad image;x'; do
  d="$(fx img)"; ins "$d" install --yes --image "$bad_img" >/dev/null 2>"$d/err"; rc=$?
  t "image: '$bad_img' is refused (exit 3), nothing changed" bash -c '[ '"$rc"' -eq 3 ] && grep -q "image" "$1" && [ ! -s "$2" ]' _ "$d/err" "$d/rec/cmds"
done
d="$(fx img0)"; CCSBX_ROOT_PREFIX="$d/root" CCSBX_EXEC="record:$d/rec/cmds" CCSBX_BIN_DIR="$d/dist" CCSBX_P15_FILE="$d/p15.tsv" CCSBX_OWNER=tomas CCSBX_B9_CMD=true "$inst" install --yes >/dev/null 2>"$d/err"; rc=$?
t "image: no image at all is refused (exit 3): there is no tag default any more" bash -c '[ '"$rc"' -eq 3 ] && grep -q "no fence image" "$1"' _ "$d/err"
t "image: the repo the installer accepts is the one pins.DefaultRepos allows (a candidate from any other repo is refused by pins.Check)" bash -c 'grep -q "DefaultRepos = \[\]string{\"ghcr.io/landovsky/ccsbx-fence\"}" "$1/internal/pins/pins.go"' _ "$here"
echo "# the machine name: lowercase, the same string for hostd and ccsbx-root"
t "machine: a mixed-case host name is lowercased by default (root lowercases its own name, so hostd must agree)" bash -c 'grep -q "tr .A-Z. .a-z." "$1"' _ "$inst"
d="$(fx mach)"; ins "$d" install --yes --machine HP-Ubuntu >/dev/null 2>"$d/err"; rc=$?
t "machine: an explicit mixed-case --machine is refused (exit 3), not passed on" test "$rc" -eq 3
out="$(CCSBX_ROOT_PREFIX="$d/root" CCSBX_EXEC="record:$d/x" CCSBX_OWNER=tomas "$inst" check 2>&1)"
t "helper: check looks at the pins file (FAIL in an empty fake root)" grep -q '^FAIL  /etc/ccsbx/pins.toml is missing' <<<"$out"
for r in '/srv/a,' ',/srv/a' '/srv/a,,/srv/b' '/srv/a/' '/srv//a'; do
  d="$(fx wr)"; ins "$d" install --yes --allowed-roots "$r" >/dev/null 2>"$d/err"; rc=$?
  t "helper: --allowed-roots '$r' (an empty entry or a trailing slash: hostd would refuse every start) is refused (exit 3)" test "$rc" -eq 3
done

echo "# allowed roots must be real paths: hostd checks them lexically, so a symlinked root refuses every start"
d="$(fx real)"; mkdir -p "$d/root/srv/real"; ln -s real "$d/root/srv/link"
ins "$d" install --yes --allowed-roots /srv/link >/dev/null 2>"$d/err"; rc=$?
t "roots: a root that is a symlink is refused (exit 3), naming it, nothing changed" bash -c '[ '"$rc"' -eq 3 ] && grep -q "is not a real path" "$1" && grep -q "/srv/link" "$1" && [ ! -s "$2" ]' _ "$d/err" "$d/rec/cmds"
mkdir -p "$d/root/srv/x"; ln -s ../x "$d/root/srv/real/inner"
ins "$d" install --yes --allowed-roots /srv/real/inner >/dev/null 2>"$d/err"; rc=$?
t "roots: a root with a symlink as its last component (relative target) is refused too" test "$rc" -eq 3
ln -s real "$d/root/srv/viadir"; mkdir -p "$d/root/srv/real/sub"
ins "$d" install --yes --allowed-roots /srv/viadir/sub >/dev/null 2>"$d/err"; rc=$?
t "roots: a root reached THROUGH a symlinked directory (/srv/viadir/sub) is refused" test "$rc" -eq 3
ins "$d" install --yes --allowed-roots /srv/real/sub >/dev/null 2>"$d/err"; rc=$?
t "roots: a real directory is accepted (exit 0)" bash -c '[ '"$rc"' -eq 0 ] || { cat "$1" >&2; exit 1; }' _ "$d/err"

echo "# ccsbx-root's verbs: status (read-only, A's n1-a-h1-r67) is listed explicitly"
d="$(fx verbs)"; ins "$d" install --yes >/dev/null 2>&1
t "verbs: the installed ccsbx-root.toml lists status explicitly, next to stop" bash -c 'grep -q "^verbs = \\[.*\"stop\", \"status\"" "$1"' _ "$d/root/etc/ccsbx/ccsbx-root.toml"
t "verbs: it lists exactly the seven docker verbs and the five pod and relay verbs (no verb is added by accident)" bash -c 'v="$(sed -n "s/^verbs = \\[\\(.*\\)\\]\$/\\1/p" "$1" | tr -d "\" " | tr "," "\\n" | sort | tr "\\n" " ")"; [ "$v" = "attach exec-token gc pod-create pod-load pod-status pod-stop relay-mint renew run status stop " ]' _ "$d/root/etc/ccsbx/ccsbx-root.toml"

echo "# round 3: ccsbx-launch, ccsbx-term, the hub secret, the keys peer (n1-a-relay b14eeb2)"
d="$(fx r3)"; ins "$d" install --yes >"$d/out" 2>"$d/err"; rc=$?; c="$d/rec/cmds"; toml="$d/root/etc/ccsbx/ccsbx-root.toml"
t "r3: the install succeeds" test "$rc" -eq 0
t "r3: ccsbx-launch is a system user with its own 0700 home, and the signer's user is not it" bash -c 'grep -q "^useradd --system --home-dir /var/lib/ccsbx-launch --create-home --shell /usr/sbin/nologin ccsbx-launch" "$1" && grep -q "^chown ccsbx-launch:ccsbx-launch /var/lib/ccsbx-launch" "$1" && grep -q "^chmod 0700 /var/lib/ccsbx-launch" "$1"' _ "$c"
t "r3: ccsbx-term has no home (/nonexistent, no create-home) and no shell, and is in no group but its own" bash -c 'l="$(grep "ccsbx-term$" "$1" | grep ^useradd)"; grep -q -- "--no-create-home" <<<"$l" && grep -q -- "--home-dir /nonexistent" <<<"$l" && grep -q -- "--shell /usr/sbin/nologin" <<<"$l" && ! grep -q -- "--groups\|-G " <<<"$l" && ! grep -q "gpasswd -a ccsbx-term\|usermod.*ccsbx-term" "$1"' _ "$c"
t "r3: the signer's unit names the keys peer ccsbx-launch for future-two only" bash -c 'grep -q -- "--keys-peer-user ccsbx-launch --keys-peer-machines future-two" "$1/root/etc/systemd/system/ccsbx-signer.service"' _ "$d"
t "r3: the signer's scope allows future-two as well (else the launcher's token is out of scope)" bash -c 'grep -- "--write-scope" "$1" | grep -q -- "--machines [a-z0-9.-]*.,future-two"' _ "$c"
t "r3: ccsbx-root.toml names launch_user, term_user, signer_user, the kubeconfig under ccsbx-launch, k8s_machine, the hub" bash -c 'for k in "launch_user = \"ccsbx-launch\"" "term_user = \"ccsbx-term\"" "signer_user = \"ccsbx-keys\"" "kubeconfig = \"/var/lib/ccsbx-launch/launcher.kubeconfig\"" "k8s_machine = \"future-two\"" "hub_url = \"http://127.0.0.1:4011\"" "hub_secret = \"/etc/ccsbx/hub-admin.secret\""; do grep -qF "$k" "$1" || { echo "missing $k"; exit 1; }; done' _ "$toml"
t "r3: the pod and relay verbs are on" bash -c 'grep "^verbs" "$1" | grep -q "pod-create.*pod-load.*pod-stop.*pod-status.*relay-mint"' _ "$toml"
t "r3: hub-admin.secret is root:root 0600 and holds the secret without a newline's worth of extras" bash -c 'grep -q "^chown root:root /etc/ccsbx/hub-admin.secret" "$1/rec/cmds" && [ "$(stat -c %a "$1/root/etc/ccsbx/hub-admin.secret")" = 600 ] && [ "$(cat "$1/root/etc/ccsbx/hub-admin.secret")" = hub-secret-value-not-printed ]' _ "$d"
printf 'first-line-secret\nsecond line must not be copied\n' > "$d/hubsecret2"; d2="$(fx r3two)"; HUBSRC="$d/hubsecret2" ins "$d2" install --yes >/dev/null 2>&1
t "r3: only the FIRST LINE of the owner's secret file is copied" bash -c '[ "$(cat "$1/root/etc/ccsbx/hub-admin.secret")" = first-line-secret ]' _ "$d2"
out="$(CCSBX_OWNER=tomas "$inst" plan 2>&1)"
t "r3: with no option the source is ~owner/.config/session-bus/secret" bash -c 'grep -q "/.config/session-bus/secret" <<<"$1"' _ "$out"
t "r3: the secret is never printed by the install" bash -c '! grep -q hub-secret-value-not-printed "$1" "$2"' _ "$d/out" "$d/err"
t "r3: the sudoers rule is unchanged: one rule for ccsbx-root covers the pod verbs for the owner and hostd" bash -c 'f="$1/root/etc/sudoers.d/.90-ccsbx-root.new"; [ "$(grep -c "^[a-z][a-z0-9_-]* ALL=(root) NOPASSWD: /usr/local/sbin/ccsbx-root\$" "$f")" = 2 ] && ! grep -q ccsbx-launch "$f"' _ "$d"
plan="$(ins "$d" plan -v 2>&1)"
t "r3: plan -v does not print the secret either" bash -c '! grep -q hub-secret-value-not-printed <<<"$1"' _ "$plan"
for bad in "/nonexistent-hub-secret"; do
  d="$(fx r3b)"; HUBSRC="$bad" ins "$d" install --yes >/dev/null 2>"$d/err"; rc=$?
  t "r3: hub secret source '${bad:-unset}' is refused (exit 3), nothing changed" bash -c '[ "$1" -eq 3 ] && [ ! -e "$2/rec/cmds" ] && [ ! -e "$2/root/etc/ccsbx" ]' _ "$rc" "$d"
done
d="$(fx r3c)"; : > "$d/empty"; HUBSRC="$d/empty" ins "$d" install --yes >/dev/null 2>&1; rc=$?
t "r3: an empty secret file is refused (exit 3)" test "$rc" -eq 3
d="$(fx r3d)"; ln -s "$d/hubsecret" "$d/hublink"; HUBSRC="$d/hublink" ins "$d" install --yes >/dev/null 2>&1; rc=$?
t "r3: a symlink as the secret source is refused (exit 3)" test "$rc" -eq 3
d="$(fx r3e)"; ins "$d" install --yes --k8s-machine "$(hostname -s | tr A-Z a-z)" >/dev/null 2>&1; rc=$?
t "r3: a k8s machine equal to this machine is refused (exit 3)" test "$rc" -eq 3
d="$(fx r3h)"; ins "$d" install --yes >/dev/null 2>&1
out="$(CCSBX_ROOT_PREFIX="$d/root" CCSBX_EXEC="record:$d/x" CCSBX_OWNER=tomas "$inst" check 2>&1)"
t "r3: check looks at ccsbx-launch, ccsbx-term, their uids, the launch dir and the hub secret" bash -c 'for k in "user ccsbx-launch exists" "user ccsbx-term exists" "five different uids\|share uid" "/var/lib/ccsbx-launch is" "hub-admin.secret is"; do grep -q "$k" <<<"$1" || { echo "no check: $k"; exit 1; }; done' _ "$out"
t "r3: the hub secret is listed as absent in the first backup, so uninstall removes it" grep -qx /etc/ccsbx/hub-admin.secret "$d/root/var/backups/ccsbx-install/original/ABSENT"

echo "# the sidecar image (#3wsr): by digest, from our repo, fail closed"
d="$(fx side)"; ins "$d" install --yes >/dev/null 2>&1; rc=$?
t "side: the digest is written to ccsbx-root.toml as k8s_side_image" bash -c '[ "$1" -eq 0 ] && grep -qx "k8s_side_image = \"ghcr.io/landovsky/ccsbx@sha256:b\{64\}\"" "$2/root/etc/ccsbx/ccsbx-root.toml"' _ "$rc" "$d"
for bad in "" "ghcr.io/landovsky/ccsbx:latest" "ghcr.io/landovsky/ccsbx@sha256:abc" "ghcr.io/landovsky/ccsbx-fence@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" "ghcr.io/other/ccsbx@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" "ghcr.io/landovsky/ccsbx@sha256:BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"; do
  d="$(fx sideb)"; SIDEIMG="$bad" ins "$d" install --yes >/dev/null 2>"$d/err"; rc=$?
  t "side: --side-image '${bad:-unset}' is refused (exit 3), nothing changed" bash -c '[ "$1" -eq 3 ] && [ ! -e "$2/rec/cmds" ] && [ ! -e "$2/root/etc/ccsbx" ]' _ "$rc" "$d"
done
d="$(fx sidec)"; ins "$d" install --yes --side-image ghcr.io/landovsky/ccsbx@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc >/dev/null 2>&1
t "side: the --side-image option overrides the environment" grep -q 'sha256:c\{64\}' "$d/root/etc/ccsbx/ccsbx-root.toml"

echo "# the test harness itself"
t "a failed mktemp -d can't turn fx into rm -rf of a top-level dir (tmp is checked)" grep -q '\[ -d "\$tmp" \] || exit 2' "$0"

echo "# $pass passed, $fail failed"
[ "$fail" -eq 0 ]
