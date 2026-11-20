# units/lib/common.sh — shared by units/install-linux.sh (sourced, never run).
# Why: so `plan` (what Tomas reads) can never differ from `install` (what runs).
#
# One code path for `plan` and `install`: every step calls cmd/put/note, and in plan mode those
# only PRINT. So what `plan` shows is, by construction, what `install` runs. Read-only probes
# (id, test, grep, systemctl is-active …) run in every mode and never change anything.
#
# Test hooks (never set in real use):
#   CCSBX_ROOT_PREFIX=DIR   every absolute path the installer touches is placed under DIR
#   CCSBX_EXEC=record:FILE  cmd appends its argv to FILE instead of running it
# shellcheck shell=bash

MODE="${MODE:-plan}"                 # plan | install | check | uninstall
PREFIX="${CCSBX_ROOT_PREFIX:-}"
EXEC="${CCSBX_EXEC:-run}"
# ONE originals store, written once, before the first change, and never overwritten: a re-run or a
# half-done install must not replace the first backup with an already-changed state (review M4).
# The test hook may move it; the real path is fixed so an env var can't point root at another dir.
if [ -n "$PREFIX" ]; then BACKUP_ROOT="${CCSBX_BACKUP_DIR:-/var/backups/ccsbx-install}"; else BACKUP_ROOT=/var/backups/ccsbx-install; fi
BACKUP_DIR="$BACKUP_ROOT/original"
STEP_N=0
# the test hooks go together or not at all: a prefix without record mode would run commands on the
# real system; record mode without a prefix would write real files while only PRETENDING to run commands
if [ -n "$PREFIX" ] && [ "${EXEC%%:*}" != record ]; then
  echo "install-linux: CCSBX_ROOT_PREFIX is a test hook and needs CCSBX_EXEC=record:FILE (refusing to run commands against the real system)" >&2; exit 2
fi
if [ -z "$PREFIX" ] && [ "${EXEC%%:*}" = record ]; then
  echo "install-linux: CCSBX_EXEC=record is a test hook and needs CCSBX_ROOT_PREFIX=DIR (refusing: it would write real files and only pretend to run commands)" >&2; exit 2
fi
CHECK_FAIL=0

# Every path the installer creates or replaces. Backed up (or recorded as absent) before the first
# change; uninstall removes the absent ones and restores the others, and nothing else.
LIBEXEC=/usr/local/libexec/ccsbx
MANAGED_PATHS=(
  /etc/systemd/system/ccsbx-signer.service /etc/systemd/system/ccsbx-signer.socket /etc/systemd/system/ccsbx-signer-launch.socket
  /etc/systemd/system/ccsbx-hostd.service /etc/systemd/system/ccsbx-tmux.service
  /etc/sudoers.d/90-ccsbx-root /etc/sudoers.d/91-ccsbx-deploy /etc/ccsbx/ccsbx-root.toml /etc/ccsbx/hub-admin.secret
  /usr/local/sbin/ccsbx-root /usr/local/sbin/ccsbx-deploy /usr/local/bin/ccsbx
  "$LIBEXEC/ccsbx-hostd" "$LIBEXEC/ccsbx-signer"
)
is_managed() { local p; for p in "${MANAGED_PATHS[@]}"; do [ "$p" = "$1" ] && return 0; done; return 1; }

pth() { printf '%s%s' "$PREFIX" "$1"; }          # a path inside the (possibly faked) root

die()  {
  printf 'install-linux: %s\n' "$*" >&2
  if [ "$MODE" = install ] && [ -f "$(pth "$BACKUP_DIR")/STATE" ] && [ "$(cat "$(pth "$BACKUP_DIR")/STATE")" = in-progress ]; then
    printf 'install-linux: PARTIAL INSTALL. The first backup is %s. Undo with: units/install-linux.sh uninstall --yes\n' "$BACKUP_DIR" >&2
  fi
  exit 1
}
refuse() { printf 'install-linux: REFUSED: %s\n' "$*" >&2; exit 3; }

quote() { local a out=""; for a in "$@"; do out+="$(printf '%q' "$a") "; done; printf '%s' "${out% }"; }

step() {                              # step "title"
  STEP_N=$((STEP_N + 1))
  case "$MODE" in
    plan|install|uninstall) printf '\n[%02d] %s\n' "$STEP_N" "$*" ;;
  esac
}
note() { case "$MODE" in plan|install|uninstall) printf '     # %s\n' "$*" ;; esac; }

rec() { quote "$@" >> "${EXEC#record:}"; echo >> "${EXEC#record:}"; }   # record mode: log an argv

cmd() {                               # cmd ARGV…  — the only way a step changes the system
  case "$MODE" in
    plan) printf '     $ %s\n' "$(quote "$@")" ;;
    install|uninstall)
      printf '     $ %s\n' "$(quote "$@")"
      if [ "${EXEC%%:*}" = record ]; then rec "$@"
      else "$@" || die "step $STEP_N failed: $(quote "$@")"; fi ;;
  esac
}
# try ARGV… — an UNDO step: "already gone" is fine (a partial install has not made everything)
try() {
  case "$MODE" in
    plan) printf '     $ %s   (failure ignored)\n' "$(quote "$@")" ;;
    *)
      printf '     $ %s   (failure ignored)\n' "$(quote "$@")"
      if [ "${EXEC%%:*}" = record ]; then rec "$@"; else "$@" >/dev/null 2>&1 || printf '     # (not there, or already undone)\n'; fi ;;
  esac
}

# put PATH MODE OWNER:GROUP < content   — write a file atomically. What it replaces was already
# saved by backup_originals; every write is checked, so a full disk stops the install.
put() {
  local path="$1" mode="$2" own="$3" real tmp; real="$(pth "$path")"
  local body; body="$(cat)"
  case "$MODE" in
    plan)
      printf '     write %s  (mode %s, %s, %d lines)\n' "$path" "$mode" "$own" "$(printf '%s\n' "$body" | wc -l)"
      if [ "${VERBOSE:-0}" = 1 ]; then printf '%s\n' "$body" | sed 's/^/       | /'; fi ;;
    install|uninstall)
      printf '     write %s  (mode %s, %s)\n' "$path" "$mode" "$own"
      mkdir -p "$(dirname "$real")" || die "cannot create $(dirname "$real")"
      tmp="$(mktemp "$real.XXXXXX")" || die "cannot create a temp file next to $real"
      [ -n "$tmp" ] || die "mktemp returned nothing for $real"
      printf '%s\n' "$body" > "$tmp" || die "cannot write $tmp"
      chmod "$mode" "$tmp" || die "cannot chmod $tmp"
      if [ "${EXEC%%:*}" != record ]; then chown "$own" "$tmp" || die "cannot chown $tmp"; else rec chown "$own" "$path"; fi
      mv -f "$tmp" "$real" || die "cannot move $tmp to $real" ;;
  esac
}

# backup_originals — BEFORE the first change, copy every file the install will replace (and all of
# sudoers.d, and sops's owner and mode), and list the paths that do not exist yet. Built in a temp
# dir, verified, then renamed into place in one step. An existing backup is never touched.
backup_originals() {
  local final tmpd p f rel
  final="$(pth "$BACKUP_DIR")"
  if [ -d "$final" ]; then
    note "the first backup $BACKUP_DIR already exists: kept as it is, never overwritten"
    return 0
  fi
  umask 077
  mkdir -p "$(dirname "$final")" || die "cannot create $(dirname "$final")"
  chmod 0700 "$(dirname "$final")" || die "cannot chmod $(dirname "$final")"
  tmpd="$(mktemp -d "$final.XXXXXX")" || die "cannot create a backup directory in $(dirname "$final")"
  [ -n "$tmpd" ] && [ -d "$tmpd" ] || die "backup directory was not created"
  : > "$tmpd/ABSENT" || die "cannot write the backup"
  for p in "${MANAGED_PATHS[@]}"; do
    if [ -e "$(pth "$p")" ] || [ -L "$(pth "$p")" ]; then
      mkdir -p "$tmpd$(dirname "$p")" || die "cannot back up $p"
      cp -a "$(pth "$p")" "$tmpd$p" || die "cannot back up $p"
      cmp -s "$(pth "$p")" "$tmpd$p" || [ -L "$(pth "$p")" ] || die "backup of $p differs from the original"
    else
      printf '%s\n' "$p" >> "$tmpd/ABSENT" || die "cannot write the backup"
    fi
  done
  mkdir -p "$tmpd/etc/sudoers.d" || die "cannot back up sudoers.d"
  if compgen -G "$(pth /etc/sudoers.d)/*" >/dev/null; then
    for f in "$(pth /etc/sudoers.d)"/*; do
      [ -e "$f" ] || continue
      cp -a "$f" "$tmpd/etc/sudoers.d/" || die "cannot back up $f"
      cmp -s "$f" "$tmpd/etc/sudoers.d/$(basename "$f")" || die "backup of $f differs from the original"
    done
  fi
  if [ -e "$(pth /usr/local/bin/sops)" ]; then
    stat -c '%U:%G %a' "$(pth /usr/local/bin/sops)" > "$tmpd/sops.meta" || die "cannot record sops's owner"
  fi
  echo in-progress > "$tmpd/STATE" || die "cannot write the backup"
  mv -T "$tmpd" "$final" || die "cannot move the backup into place at $final"
  note "backup of every file the install replaces: $BACKUP_DIR (the first state; later runs never overwrite it)"
}
backup_state() { [ "$MODE" = install ] && printf '%s\n' "$1" > "$(pth "$BACKUP_DIR")/STATE"; }

# --- read-only probes (safe in plan mode) ---
have_user()  { id "$1" >/dev/null 2>&1; }
have_group() { getent group "$1" >/dev/null 2>&1; }
in_group()   { id -nG "$1" 2>/dev/null | tr ' ' '\n' | grep -qx "$2"; }
real_user()  { printf '%s' "${CCSBX_OWNER:-${SUDO_USER:-$(id -un)}}"; }

# --- check mode: ok/FAIL lines ---
ok()   { printf 'ok    %s\n' "$*"; }
bad()  { printf 'FAIL  %s\n' "$*"; CHECK_FAIL=$((CHECK_FAIL + 1)); }
expect() {                            # expect "what" command…  (a read-only probe that must succeed)
  local what="$1"; shift
  if "$@" >/dev/null 2>&1; then ok "$what"; else bad "$what"; fi
}
