# units/lib/u0.sh — U0: leave docker and lxd on hp, remove the other root paths (sourced).
#
# After this, the only way from tomas to root is `sudo -n /usr/local/sbin/ccsbx-root <verb>`
# (verbs: run stop renew gc attach exec-token; it refuses unpinned images, workdirs outside the
# allowed roots, symlink escapes and unknown flags), plus password sudo, which stays.
# Order matters: every replacement is installed and verified BEFORE the groups are removed,
# and the docker group is the LAST thing changed.
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

u0_sudoers() {
  step "U0 sudoers: ccsbx-root is the only NOPASSWD grant"
  note "1. the new rule goes in a dot-named staging file (sudo ignores it), is checked, then renamed"
  put /etc/sudoers.d/.90-ccsbx-root.new 0440 root:root <<EOT
# ccsbx (U0, install-linux.sh): the only NOPASSWD rule. Fixed verbs; it validates every argument.
$(real_user) ALL=(root) NOPASSWD: /usr/local/sbin/ccsbx-root
ccsbx ALL=(root) NOPASSWD: /usr/local/sbin/ccsbx-root
EOT
  cmd visudo -cf /etc/sudoers.d/.90-ccsbx-root.new
  cmd mv -f /etc/sudoers.d/.90-ccsbx-root.new /etc/sudoers.d/90-ccsbx-root
  note "2. remove the NOPASSWD grants for docker, apt-get, dokku, sops from every sudoers.d file"
  note "   (units/lib/sudoers_strip.py: refuses lines it can't parse, leaves everything else alone)"
  case "$MODE" in
    plan) printf '     $ for f in /etc/sudoers.d/*: sudoers_strip.py f > f.new; visudo -cf f.new; mv f.new f\n' ;;
    install)
      local f new
      for f in "$(pth /etc/sudoers.d)"/*; do
        [ -f "$f" ] || continue
        case "$(basename "$f")" in README|90-ccsbx-root|*.new) continue;; esac
        grep -q NOPASSWD "$f" 2>/dev/null || continue
        new="$(dirname "$f")/.$(basename "$f").new"
        backup "/etc/sudoers.d/$(basename "$f")"
        python3 -I "$UNITS_DIR/lib/sudoers_strip.py" "$f" > "$new" || die "cannot clean $f (see above); nothing was changed in it"
        if cmp -s "$f" "$new"; then rm -f "$new"; continue; fi
        chmod 0440 "$new"
        if [ "${EXEC%%:*}" = record ]; then quote visudo -cf "$new" >> "${EXEC#record:}"; echo >> "${EXEC#record:}"
        else visudo -cf "$new" >/dev/null || die "cleaned $f does not validate; left as it was"; fi
        mv -f "$new" "$f"; printf '     cleaned %s\n' "$f"
      done ;;
  esac
}

u0_sops() {
  step "U0 sops: a root-owned binary (tomas-writable + sudo-runnable was a root path)"
  if [ "$MODE" = install ] && [ -e "$(pth /usr/local/bin/sops)" ]; then
    mkdir -p "$(pth "$BACKUP_DIR")"
    stat -c '%U:%G %a' "$(pth /usr/local/bin/sops)" > "$(pth "$BACKUP_DIR")/sops.meta"
  fi
  cmd chown root:root /usr/local/bin/sops
  cmd chmod 0755 /usr/local/bin/sops
}

u0_hostd() {
  step "U0 hostd unit and firewall"
  put /etc/systemd/system/ccsbx-hostd.service 0644 root:root < "$UNITS_DIR/systemd/ccsbx-hostd.service"
  cmd systemctl daemon-reload
  cmd systemctl enable --now ccsbx-hostd.service
  note "7480 only on the tailnet interface; the Mac and future-two reach hostd there"
  cmd ufw allow in on tailscale0 to any port 7480 proto tcp comment "ccsbx hostd"
}

u0_lxd() {
  step "U0 lxd: remove the accidental snap and its installer socket"
  note "closes the root path of a missing-command handler turning 'lxc' into an install"
  if [ "$MODE" != install ] || snap list lxd >/dev/null 2>&1; then cmd snap remove lxd; fi
  cmd systemctl disable --now lxd-installer.socket
  if in_group "$(real_user)" lxd; then cmd gpasswd -d "$(real_user)" lxd; else note "$(real_user) is not in group lxd"; fi
}

u0_docker_group() {
  step "U0 docker group (LAST): $(real_user) leaves it; effective for new login sessions"
  note "running containers keep running: the daemon owns them (P14 inventory)"
  note "github-runner@N units keep docker through SupplementaryGroups=docker in their own unit:"
  note "  that is NOT changed here; it is a P15 decision (units/p15 status)"
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

u0_uninstall() {
  step "U0 undo: put the sudoers files, sops mode and groups back from the backup"
  note "backup: $BACKUP_DIR (the newest one unless --backup DIR). Docker/lxd are not reinstalled."
  cmd rm -f /etc/sudoers.d/90-ccsbx-root
  cmd systemctl disable --now ccsbx-hostd.service
  cmd rm -f /etc/systemd/system/ccsbx-hostd.service /usr/local/sbin/ccsbx-root /usr/local/bin/ccsbx "$LIBEXEC/ccsbx-hostd"
  note "every sudoers.d file the install changed comes back from $RESTORE_DIR"
  if [ "$MODE" = plan ] || [ -d "$(pth "$RESTORE_DIR")/etc/sudoers.d" ]; then
    cmd cp -a "$RESTORE_DIR/etc/sudoers.d/." /etc/sudoers.d/
  fi
  note "sops gets back the owner and mode recorded in $RESTORE_DIR/sops.meta (it was the owner's, 0775)"
  local meta="" ; [ -f "$(pth "$RESTORE_DIR")/sops.meta" ] && meta="$(cat "$(pth "$RESTORE_DIR")/sops.meta")"
  if [ -n "$meta" ]; then cmd chown "${meta% *}" /usr/local/bin/sops; cmd chmod "${meta#* }" /usr/local/bin/sops
  elif [ "$MODE" = plan ]; then cmd chown "$(real_user):$(real_user)" /usr/local/bin/sops; cmd chmod 0775 /usr/local/bin/sops; fi
  note "re-add the groups only if you want invariant 14 waived again: gpasswd -a $(real_user) docker"
}
