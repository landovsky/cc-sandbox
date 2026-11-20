# units/lib/u0.sh — U0: leave docker and lxd on hp, remove the other root paths (sourced).
# Why: any NOPASSWD docker/apt/sops grant is a root shell for every agent on hp; this closes them and leaves one fixed-verb door.
#
# After this, the only way from tomas to root is `sudo -n /usr/local/sbin/ccsbx-root <verb>`
# (verbs: run stop renew gc attach exec-token; it refuses unpinned images, workdirs outside the
# allowed roots, symlink escapes and unknown flags), plus password sudo, which stays.
# Order matters: every replacement is installed and verified BEFORE the groups are removed, every
# step that can fail for a machine-specific reason (snap, sops, the lxd socket) runs BEFORE the
# sudoers files are changed, and the docker group is the LAST thing changed.
# shellcheck shell=bash

u0_binaries() {
  step "U0 binaries (root-owned, not writable by tomas)"
  for f in ccsbx-root ccsbx-hostd ccsbx-signer ccsbx; do
    [ -f "$BIN_DIR/$f" ] || note "MISSING $BIN_DIR/$f: install will refuse until it is built (a linux build of each + SHA256SUMS in --bin-dir; see --help)"
  done
  cmd install -d -o root -g root -m 0755 "$LIBEXEC" /etc/ccsbx
  cmd install -o root -g root -m 0755 "$BIN_DIR/ccsbx-root" /usr/local/sbin/ccsbx-root
  cmd install -o root -g root -m 0755 "$BIN_DIR/ccsbx-hostd" "$LIBEXEC/ccsbx-hostd"
  cmd install -o root -g root -m 0755 "$BIN_DIR/ccsbx-signer" "$LIBEXEC/ccsbx-signer"
  cmd install -o root -g root -m 0755 "$BIN_DIR/ccsbx" /usr/local/bin/ccsbx
  note "ccsbx-root reads its allowed workdir roots from /etc/ccsbx/ccsbx-root.toml (root-owned)"
  put /etc/ccsbx/ccsbx-root.toml 0644 root:root <<EOT
# written by install-linux.sh (U0). Root-owned: tomas can't widen it. Edit with sudoedit.
allowed_roots = [${ALLOWED_ROOTS_TOML}]
verbs = ["run", "stop", "renew", "gc", "attach", "exec-token"]
EOT
}

# the combined sudoers config must parse; in record mode the answer is CCSBX_TEST_VISUDO_RC (test hook)
visudo_all() {
  if [ "${EXEC%%:*}" = record ]; then rec visudo -c; return "${CCSBX_TEST_VISUDO_RC:-0}"; fi
  visudo -c >/dev/null
}

# put every sudoers file the install touched back from the first backup, and drop the new rule
sudoers_rollback() {
  local f b="$(pth "$BACKUP_DIR")/etc/sudoers.d" name rc=0
  for f in "$@"; do
    name="$(basename "$f")"
    cp -af "$b/$name" "$f" || { printf 'install-linux: CANNOT restore %s from %s: do it by hand\n' "$f" "$b" >&2; rc=1; }
  done
  rm -f "$(pth /etc/sudoers.d/90-ccsbx-root)" "$(pth /etc/sudoers.d/91-ccsbx-deploy)" || rc=1
  return $rc
}

u0_sudoers() {
  step "U0 sudoers: ccsbx-root is the only NOPASSWD grant"
  note "ccsbx-root runs with a CLEAN environment: 'Defaults!/usr/local/sbin/ccsbx-root env_reset,!setenv' (the caller's env is reset and"
  note "  cannot be passed through, so no sudo -E and no VAR=x reaches it)"
  note "1. the new rule goes in a dot-named staging file (sudo ignores it), is checked, then renamed"
  put /etc/sudoers.d/.90-ccsbx-root.new 0440 root:root <<EOT
# ccsbx (U0, install-linux.sh): the only NOPASSWD rule. Fixed verbs; it validates every argument.
# ccsbx-root gets a clean environment: the caller's env is reset and cannot be passed through (no sudo -E, no VAR=x).
Defaults!/usr/local/sbin/ccsbx-root env_reset,!setenv
$(real_user) ALL=(root) NOPASSWD: /usr/local/sbin/ccsbx-root
ccsbx ALL=(root) NOPASSWD: /usr/local/sbin/ccsbx-root
EOT
  cmd visudo -cf /etc/sudoers.d/.90-ccsbx-root.new
  cmd mv -f /etc/sudoers.d/.90-ccsbx-root.new /etc/sudoers.d/90-ccsbx-root
  note "2. remove the NOPASSWD grants for docker, apt, dpkg, snap, dokku, sops from every sudoers.d file"
  note "   (units/lib/sudoers_strip.py: refuses lines it can't parse, leaves everything else alone)"
  note "3. the whole config is checked again; if it does not parse, every file changed is put back"
  case "$MODE" in
    plan) printf '     $ for f in /etc/sudoers.d/*: sudoers_strip.py f > f.new; visudo -cf f.new; mv f.new f\n     $ visudo -c   (on failure: restore from the first backup, stop)\n' ;;
    install)
      local f new changed=()
      for f in "$(pth /etc/sudoers.d)"/*; do
        [ -f "$f" ] || continue
        case "$(basename "$f")" in README|90-ccsbx-root|91-ccsbx-deploy|*.new) continue;; esac
        grep -q NOPASSWD "$f" 2>/dev/null || continue
        new="$(dirname "$f")/.$(basename "$f").new"
        python3 -I "$UNITS_DIR/lib/sudoers_strip.py" "$f" > "$new" || { rm -f "$new"; die "cannot clean $f (see above); nothing was changed in it"; }
        if cmp -s "$f" "$new"; then rm -f "$new" || die "cannot remove $new"; continue; fi
        chmod 0440 "$new" || die "cannot chmod $new"
        if [ "${EXEC%%:*}" = record ]; then rec visudo -cf "$new"
        else visudo -cf "$new" >/dev/null || { rm -f "$new"; die "cleaned $f does not validate; left as it was"; }; fi
        mv -f "$new" "$f" || die "cannot replace $f"
        changed+=("$f"); printf '     cleaned %s\n' "$f"
      done
      printf '     $ visudo -c\n'
      if ! visudo_all; then
        sudoers_rollback "${changed[@]}" || die "the combined sudoers config does not parse after the change, AND the restore failed: restore /etc/sudoers.d from $BACKUP_DIR/etc/sudoers.d by hand"
        die "the combined sudoers config does not parse after the change; restored ${#changed[@]} file(s) from the first backup and removed the new rules"
      fi ;;
  esac
}

u0_sops() {
  step "U0 sops: a root-owned binary (tomas-writable + sudo-runnable was a root path)"
  if [ "$MODE" = install ] && [ ! -e "$(pth /usr/local/bin/sops)" ]; then
    note "/usr/local/bin/sops does not exist: nothing to change"
  else
    cmd chown root:root /usr/local/bin/sops
    cmd chmod 0755 /usr/local/bin/sops
  fi
}

u0_hostd() {
  step "U0 hostd unit"
  put /etc/systemd/system/ccsbx-hostd.service 0644 root:root < "$UNITS_DIR/systemd/ccsbx-hostd.service"
  note "the bearer token for hostd's loopback API: made once (0600, ccsbx), never replaced"
  if [ "$MODE" != install ] || [ ! -e "$(pth /var/lib/ccsbx/listen.token)" ]; then
    cmd runuser -u ccsbx -- sh -c 'umask 077; head -c 32 /dev/urandom | base64 > /var/lib/ccsbx/listen.token'
  fi
  cmd systemctl daemon-reload
  cmd systemctl enable --now ccsbx-hostd.service
  note "hostd listens on 127.0.0.1:7480 only (with a token file): no firewall rule is added (least privilege)"
}

u0_lxd() {
  step "U0 lxd: remove the accidental snap and its installer socket"
  note "closes the root path of a missing-command handler turning 'lxc' into an install"
  note "each is done only if it is there: a missing lxd must not stop the install"
  if [ "$MODE" != install ] || snap list lxd >/dev/null 2>&1; then cmd snap remove --purge lxd; else note "snap lxd is not installed"; fi
  if [ "$MODE" != install ] || systemctl list-unit-files lxd-installer.socket 2>/dev/null | grep -q lxd-installer.socket; then
    cmd systemctl disable --now lxd-installer.socket
  else note "lxd-installer.socket does not exist"; fi
}

u0_groups() {
  step "U0 groups (LAST): $(real_user) leaves lxd, then docker; effective for new login sessions"
  note "running containers keep running: the daemon owns them (P14 inventory)"
  note "github-runner@N units keep docker through SupplementaryGroups=docker in their own unit:"
  note "  that is NOT changed here; it is a P15 decision (units/p15 status)"
  if in_group "$(real_user)" lxd; then cmd gpasswd -d "$(real_user)" lxd; else note "$(real_user) is not in group lxd"; fi
  if in_group "$(real_user)" docker; then cmd gpasswd -d "$(real_user)" docker; else note "$(real_user) is not in group docker"; fi
  note "NOW: sudo reboot  (a fresh tmux server has the new groups; the old one keeps docker)"
}

u0_check() {
  echo "-- U0 root paths"
  local u; u="$(real_user)"
  # the process tree check from the plan: the oldest tmux of this user
  local pid gids g leak=0
  pid="$(pgrep -u "$(id -u "$u")" -o tmux 2>/dev/null || true)"
  if [ -z "$pid" ]; then bad "no tmux of $u to inspect"; else
    gids="$(awk '/^Groups:/{for(i=2;i<=NF;i++)print $i}' "/proc/$pid/status" 2>/dev/null)"
    for g in docker lxd; do
      if have_group "$g" && grep -qx "$(getent group "$g" | cut -d: -f3)" <<<"$gids"; then leak=1; bad "tmux (pid $pid) still has group $g: reboot done?"; fi
    done
    [ "$leak" = 0 ] && ok "tmux (pid $pid) has no docker/lxd group"
  fi
  in_group "$u" docker && bad "$u is still in group docker" || ok "$u not in group docker"
  in_group "$u" lxd && bad "$u is still in group lxd" || ok "$u not in group lxd"
  local sops; sops="$(stat -c '%U:%G %a' "$(pth /usr/local/bin/sops)" 2>/dev/null || echo missing)"
  [ "$sops" = "root:root 755" ] && ok "sops is root:root 755" || bad "sops is $sops (want root:root 755)"
  local r; r="$(stat -c '%U:%G %a' "$(pth /usr/local/sbin/ccsbx-root)" 2>/dev/null || echo missing)"
  [ "$r" = "root:root 755" ] && ok "ccsbx-root is root:root 755" || bad "ccsbx-root is $r (want root:root 755)"
  if snap list lxd >/dev/null 2>&1; then bad "snap lxd is still installed"; else ok "snap lxd is gone"; fi
  if systemctl is-enabled lxd-installer.socket >/dev/null 2>&1; then bad "lxd-installer.socket is still enabled"; else ok "lxd-installer.socket not enabled"; fi
  expect "ccsbx-hostd is active" systemctl is-active --quiet ccsbx-hostd.service
  # sudo -n -l lists NOPASSWD only for ccsbx-root (and ccsbx-deploy if Q12 = yes)
  local out extra
  if out="$(sudo -n -l 2>/dev/null)"; then
    extra="$(grep NOPASSWD <<<"$out" | grep -Ev '/usr/local/sbin/ccsbx-(root|deploy)[[:space:]]*$' || true)"
    [ -z "$extra" ] && ok "sudo -n -l: NOPASSWD only for ccsbx-root/ccsbx-deploy" || bad "sudo -n -l: other NOPASSWD: $(head -1 <<<"$extra" | cut -c1-60)"
  else bad "sudo -n -l failed (needs a sudo session; run check as $u)"; fi
}

# The backup is trusted by root, so it is checked first: under the backup root, owned by root, not
# writable by group or others, and sops.meta says only "user:group NNN" with no setuid digit.
restore_dir_ok() {
  local d="$1" real own mode meta
  case "$d" in "$BACKUP_ROOT"/*) ;; *) refuse "--backup must be a directory under $BACKUP_ROOT (got $d)";; esac
  case "$d" in *..*) refuse "no .. in the backup path";; esac
  real="$(pth "$d")"
  [ -d "$real" ] || refuse "no backup at $d"
  own="$(stat -c '%u' "$real")"; mode="$(stat -c '%a' "$real")"
  [ -n "$PREFIX" ] || [ "$own" = 0 ] || refuse "$d is not owned by root"
  [ $(( 8#$mode & 8#022 )) -eq 0 ] || refuse "$d is writable by group or others (mode $mode)"
  if [ -f "$real/sops.meta" ]; then
    meta="$(cat "$real/sops.meta")"
    [[ "$meta" =~ ^[a-z_][a-z0-9_-]*:[a-z_][a-z0-9_-]*\ [0-7]{3}$ ]] || refuse "sops.meta in $d is not 'user:group NNN': $meta"
  fi
}

u0_uninstall() {
  local real f rel p
  real="$(pth "$RESTORE_DIR")"
  step "U0 undo: stop the units, take away what the install added, put back what it replaced"
  note "from the FIRST backup: $RESTORE_DIR (never overwritten, so a re-run or a failed install can't spoil it)"
  note "units are stopped and disabled BEFORE their files are removed (a disable needs the file)"
  try systemctl disable --now ccsbx-hostd.service
  try systemctl disable --now ccsbx-signer.socket
  try systemctl disable --now ccsbx-signer.service
  note "this restores the state before the FIRST install; hand edits to sudoers.d since then are lost"
  note "1. paths that did not exist before the install are removed"
  while IFS= read -r p; do
    [ -n "$p" ] || continue
    is_managed "$p" || { note "ABSENT lists $p, which is not one of ours: ignored"; continue; }
    cmd rm -f "$p"
  done < "$real/ABSENT"
  note "2. paths the install replaced come back"
  for p in "${MANAGED_PATHS[@]}"; do
    [ -e "$real$p" ] || [ -L "$real$p" ] || continue
    cmd cp -a "$RESTORE_DIR$p" "$p"
  done
  note "3. every sudoers.d file comes back as it was, then the whole config is checked"
  if [ -d "$real/etc/sudoers.d" ]; then
    for f in "$real/etc/sudoers.d"/*; do
      [ -e "$f" ] || continue
      rel="$(basename "$f")"
      case "$rel" in */*|"") continue;; esac
      cmd cp -a "$RESTORE_DIR/etc/sudoers.d/$rel" "/etc/sudoers.d/$rel"
    done
  fi
  if [ "${EXEC%%:*}" = record ]; then rec visudo -c; else cmd visudo -c; fi
  note "4. sops gets back its owner and mode from $RESTORE_DIR/sops.meta"
  if [ -f "$real/sops.meta" ]; then
    local meta; meta="$(cat "$real/sops.meta")"
    cmd chown "${meta% *}" /usr/local/bin/sops; cmd chmod "${meta#* }" /usr/local/bin/sops
  else note "no sops.meta: sops was not there, nothing to restore"; fi
  note "5. the owner's group ccsbx-clients membership"
  try gpasswd -d "$(real_user)" ccsbx-clients
  try systemctl daemon-reload
  note "NOT undone, on purpose: docker/lxd membership and the lxd snap (re-add with gpasswd -a $(real_user) docker if you want invariant 14 waived again);"
  note "  users ccsbx, ccsbx-keys and group ccsbx-clients; /var/lib/ccsbx; /etc/ccsbx and $LIBEXEC (now empty)"
  step "U0 undo: retire the backup so a later install starts from a fresh first backup"
  cmd mv "$RESTORE_DIR" "$RESTORE_DIR.undone-$(date +%Y%m%d-%H%M%S)"
}
