#!/usr/bin/env bash
# install-linux.sh — the root installer for Tom's Linux machines (I0, U0, U0b). plan | install | check | uninstall.
#
#   units/install-linux.sh plan                prints every step and the gate results; changes NOTHING; needs no root
#   (as root) units/install-linux.sh install --yes   does it (the morning gate, R10). Refuses unless every gate is met
#   units/install-linux.sh check               read-only: ok/FAIL per step; exit 0 only when the install is complete (after the reboot)
#   (as root) units/install-linux.sh uninstall --yes  restores from the backup of the last install
#
# What it does (the same code prints it in `plan` and runs it in `install`):
#   U0b  users ccsbx + ccsbx-keys, the 0700 key directory, the machine and age keys, the signer unit
#   U0   ccsbx-root (fixed verbs) as the only NOPASSWD sudo grant; the NOPASSWD grants for docker, apt-get,
#        dokku and sops removed; sops root-owned; hostd unit + firewall 7480 on the tailnet interface;
#        the accidental lxd snap and its installer socket removed; the owner out of lxd and, LAST, docker
#   then: reboot. A fresh tmux has the new groups; the old one keeps docker.
#
# GATES. `install` REFUSES (exit 3) while any is unmet; `plan` shows which are:
#   B9   the hub is a native release under a systemd user unit (not the docker container `session-bus`),
#        because leaving the docker group would otherwise orphan the live hub
#   P15  every row of the P14 inventory is done or dropped (units/p15 check)
#   bins ccsbx-root, ccsbx-hostd, ccsbx-signer, ccsbx exist in --bin-dir and match its SHA256SUMS
#   sudoers  every sudoers.d NOPASSWD line parses (units/lib/sudoers_strip.py), so nothing is guessed
#   tools    visudo, ufw, age-keygen, python3, runuser are installed
#
# Options (each also a CCSBX_* env var):
#   --yes                 really do it (install and uninstall)
#   --bin-dir DIR         built binaries + SHA256SUMS            (CCSBX_BIN_DIR, default <repo>/dist)
#   --owner USER          the human who keeps access            (CCSBX_OWNER, default the invoking user)
#   --allowed-roots A,B   ccsbx-root's workdir roots            (CCSBX_ALLOWED_ROOTS, default ~owner/git,~owner/wt)
#   --with-deploy         also the ccsbx-deploy sudoers rule (Q12 = yes, R10c)  (CCSBX_WITH_DEPLOY=1)
#   --backup DIR          uninstall from this backup, not the newest
#   --purge-keys          uninstall also deletes the keys and the ccsbx-keys user (rotate M9 first!)
#   -v, --verbose         plan prints the files it would write
# Gate overrides, for tests only: CCSBX_B9_CMD, CCSBX_P15_CMD (a command; exit 0 = met).
# Exit: 0 ok, 1 a step failed / check failed, 2 usage, 3 refused (a gate is unmet).
set -uo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
UNITS_DIR="$HERE"; REPO_DIR="$(dirname "$HERE")"

usage() { sed -n '2,/^set -uo/p' "${BASH_SOURCE[0]}" | sed '$d' | sed 's/^# \{0,1\}//'; }
case "${1:-}" in
  plan|install|check|uninstall) MODE="$1"; shift ;;
  -h|--help|help|"") usage; exit 0 ;;
  *) echo "install-linux: unknown command '$1' (plan|install|check|uninstall; --help)" >&2; exit 2 ;;
esac
YES=0; WITH_DEPLOY="${CCSBX_WITH_DEPLOY:-0}"; BIN_DIR="${CCSBX_BIN_DIR:-$REPO_DIR/dist}"
ALLOWED_ROOTS="${CCSBX_ALLOWED_ROOTS:-}"; RESTORE_OVERRIDE=""; PURGE_KEYS=0; VERBOSE=0
while [ $# -gt 0 ]; do
  case "$1" in
    -h|--help) usage; exit 0 ;;                       # on every subcommand; runs nothing
    --yes) YES=1 ;;
    --bin-dir) BIN_DIR="${2:?--bin-dir DIR}"; shift ;;
    --owner) CCSBX_OWNER="${2:?--owner USER}"; shift ;;
    --allowed-roots) ALLOWED_ROOTS="${2:?--allowed-roots A,B}"; shift ;;
    --with-deploy) WITH_DEPLOY=1 ;;
    --backup) RESTORE_OVERRIDE="${2:?--backup DIR}"; shift ;;
    --purge-keys) PURGE_KEYS=1 ;;
    -v|--verbose) VERBOSE=1 ;;
    *) echo "install-linux: unknown option '$1' (--help)" >&2; exit 2 ;;
  esac
  shift
done
export CCSBX_OWNER VERBOSE PURGE_KEYS MODE
# shellcheck source=lib/common.sh
. "$HERE/lib/common.sh"; . "$HERE/lib/u0b.sh"; . "$HERE/lib/u0.sh"

OWNER="$(real_user)"; OWNER_HOME="$(getent passwd "$OWNER" | cut -d: -f6)"; OWNER_HOME="${OWNER_HOME:-/home/$OWNER}"
[ -n "$ALLOWED_ROOTS" ] || ALLOWED_ROOTS="$OWNER_HOME/git,$OWNER_HOME/wt"
ALLOWED_ROOTS_TOML="$(printf '"%s", ' ${ALLOWED_ROOTS//,/ })"; ALLOWED_ROOTS_TOML="${ALLOWED_ROOTS_TOML%, }"
RESTORE_DIR="${RESTORE_OVERRIDE:-$(readlink "$(pth "$LATEST_LINK")" 2>/dev/null || echo "$BACKUP_DIR")}"
export BIN_DIR UNITS_DIR ALLOWED_ROOTS_TOML RESTORE_DIR

# ---------------------------------------------------------------- gates (read-only)
GATE_MSGS=(); NOTES=()
gate() { GATE_MSGS+=("$1"); }                                  # an unmet gate
as_owner() {                                                   # run a user-level command as the owner
  if [ "$(id -u)" = 0 ] && [ "$OWNER" != root ]; then runuser -u "$OWNER" -- env "XDG_RUNTIME_DIR=/run/user/$(id -u "$OWNER")" "$@"
  else "$@"; fi
}
default_b9() {
  local unit="${CCSBX_B9_UNIT:-session-bus-hub.service}" exec
  as_owner systemctl --user is-active --quiet "$unit" 2>/dev/null || { echo "user unit $unit is not active"; return 1; }
  exec="$(as_owner systemctl --user show -p ExecStart --value "$unit" 2>/dev/null)"
  case "$exec" in *docker*) echo "$unit still starts docker"; return 1;; esac
  if as_owner docker ps --filter name='^session-bus$' -q 2>/dev/null | grep -q .; then echo "docker container session-bus is still running"; return 1; fi
  return 0
}
default_p15() { "$UNITS_DIR/p15" check; }
eval_gates() {
  local m f missing=""
  if [ -n "${CCSBX_B9_CMD:-}" ]; then m="$(eval "$CCSBX_B9_CMD" 2>&1)" || gate "B9 missing: ${m:-$CCSBX_B9_CMD failed}"
  else m="$(default_b9)" || gate "B9 missing: $m (the native hub release, stream C)"; fi
  if [ -n "${CCSBX_P15_CMD:-}" ]; then m="$(eval "$CCSBX_P15_CMD" 2>&1)" || gate "P15 open: ${m:-$CCSBX_P15_CMD failed}"
  else m="$(default_p15 2>&1)" || gate "P15 open: $(head -1 <<<"$m") (run units/p15 status)"; fi
  for f in ccsbx-root ccsbx-hostd ccsbx-signer ccsbx; do [ -f "$BIN_DIR/$f" ] || missing+=" $f"; done
  [ -z "$missing" ] || gate "binaries missing in $BIN_DIR:$missing (make build)"
  if [ -z "$missing" ]; then
    if [ ! -f "$BIN_DIR/SHA256SUMS" ]; then gate "no $BIN_DIR/SHA256SUMS"
    elif ! (cd "$BIN_DIR" && sha256sum --quiet -c SHA256SUMS >/dev/null 2>&1); then gate "binaries do not match SHA256SUMS"; fi
  fi
  for f in visudo ufw age-keygen python3 runuser; do command -v "$f" >/dev/null 2>&1 || [ -n "$PREFIX" ] || gate "tool not installed: $f"; done
  # sudoers: nothing may be unparseable (a dry run of the stripper; root only, the files are 0440)
  if compgen -G "$(pth /etc/sudoers.d)/*" >/dev/null; then
    for f in "$(pth /etc/sudoers.d)"/*; do
      if [ ! -r "$f" ]; then
        [ "$(id -u)" = 0 ] && gate "cannot read $f" || NOTES+=("sudoers.d/$(basename "$f") not readable without root: verified at install")
        continue
      fi
      grep -q NOPASSWD "$f" 2>/dev/null || continue
      m="$(python3 -I "$UNITS_DIR/lib/sudoers_strip.py" "$f" 2>&1 >/dev/null)" || gate "sudoers $(basename "$f"): $m"
    done
  fi
}
print_gates() {
  echo "GATES (install refuses while any is unmet)"
  if [ "${#GATE_MSGS[@]}" -eq 0 ]; then echo "  all met"; else printf '  UNMET: %s\n' "${GATE_MSGS[@]}"; fi
  [ "${#NOTES[@]}" -eq 0 ] || printf '  note: %s\n' "${NOTES[@]}"
}

# ---------------------------------------------------------------- steps
u0_deploy_rule() {
  step "ccsbx-deploy sudoers rule (Q12 = yes, R10c): one binary, nothing else"
  put /etc/sudoers.d/.91-ccsbx-deploy.new 0440 root:root <<EOT
# ccsbx-deploy (NB5): installs only artifacts built from main by Actions, digest-pinned.
# It never installs ccsbx-root, the signer, itself, or anything that talks to ccsbx-keys.
$OWNER ALL=(root) NOPASSWD: /usr/local/sbin/ccsbx-deploy
EOT
  cmd visudo -cf /etc/sudoers.d/.91-ccsbx-deploy.new
  cmd mv -f /etc/sudoers.d/.91-ccsbx-deploy.new /etc/sudoers.d/91-ccsbx-deploy
  if [ -f "$BIN_DIR/ccsbx-deploy" ] || [ "$MODE" = plan ]; then cmd install -o root -g root -m 0755 "$BIN_DIR/ccsbx-deploy" /usr/local/sbin/ccsbx-deploy; fi
}
steps_install() {
  u0b_users; u0_binaries; u0b_keys; u0b_signer; u0_hostd
  [ "$WITH_DEPLOY" = 1 ] && u0_deploy_rule
  u0_sudoers; u0_sops; u0_lxd; u0_docker_group
  step "record the backup for uninstall"
  cmd ln -sfn "$BACKUP_DIR" "$LATEST_LINK"
}

case "$MODE" in
  plan)
    echo "install-linux plan  ($(date '+%F %T'), owner=$OWNER, bin-dir=$BIN_DIR)"
    echo "Nothing below is run by 'plan'. The same steps run under 'install --yes' as root."
    eval_gates; print_gates
    steps_install
    echo; echo "AFTER: reboot, then units/install-linux.sh check  and  bin/ccsbx-preflight --morning --post-M1"
    echo "UNDO:  as root, units/install-linux.sh uninstall --yes   (restores the newest backup under $(dirname "$BACKUP_DIR"))"
    ;;
  install)
    [ "$YES" = 1 ] || refuse "install changes the system. Read 'plan', then run it as root with --yes"
    [ "$(id -u)" = 0 ] || [ -n "$PREFIX" ] || refuse "install must run as root"
    [ "$(uname -s)" = Linux ] || refuse "this is the Linux installer (Mac: install-mac.sh)"
    eval_gates
    if [ "${#GATE_MSGS[@]}" -gt 0 ]; then print_gates >&2; refuse "${#GATE_MSGS[@]} gate(s) unmet; nothing was changed"; fi
    echo "install-linux: all gates met; backing up to $BACKUP_DIR"
    steps_install
    echo; echo "DONE. NOW: reboot. Then: units/install-linux.sh check"
    ;;
  check)
    u0b_check; u0_check
    echo
    if [ "$CHECK_FAIL" -eq 0 ]; then echo "install-linux check: complete"; exit 0; fi
    echo "install-linux check: $CHECK_FAIL FAIL"; exit 1
    ;;
  uninstall)
    [ "$YES" = 1 ] || refuse "uninstall changes the system: add --yes"
    [ "$(id -u)" = 0 ] || [ -n "$PREFIX" ] || refuse "uninstall must run as root"
    [ -d "$(pth "$RESTORE_DIR")" ] || refuse "no backup at $RESTORE_DIR (use --backup DIR)"
    u0_uninstall; u0b_uninstall
    echo; echo "DONE. docker/lxd were not reinstalled or re-granted: that is deliberate."
    ;;
esac
