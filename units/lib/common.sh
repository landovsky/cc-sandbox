# units/lib/common.sh — shared by units/install-linux.sh (sourced, never run).
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
STAMP="${CCSBX_STAMP:-$(date +%Y%m%d-%H%M%S)}"
BACKUP_DIR="${CCSBX_BACKUP_DIR:-/var/backups/ccsbx-install}/$STAMP"
LATEST_LINK="$(dirname "$BACKUP_DIR")/latest"
STEP_N=0
if [ -n "$PREFIX" ] && [ "${EXEC%%:*}" != record ]; then
  echo "install-linux: CCSBX_ROOT_PREFIX is a test hook and needs CCSBX_EXEC=record:FILE (refusing to run commands against the real system)" >&2; exit 2
fi
CHECK_FAIL=0

pth() { printf '%s%s' "$PREFIX" "$1"; }          # a path inside the (possibly faked) root

die()  { printf 'install-linux: %s\n' "$*" >&2; exit 1; }
refuse() { printf 'install-linux: REFUSED: %s\n' "$*" >&2; exit 3; }

quote() { local a out=""; for a in "$@"; do out+="$(printf '%q' "$a") "; done; printf '%s' "${out% }"; }

step() {                              # step "title"
  STEP_N=$((STEP_N + 1))
  case "$MODE" in
    plan|install|uninstall) printf '\n[%02d] %s\n' "$STEP_N" "$*" ;;
  esac
}
note() { case "$MODE" in plan|install|uninstall) printf '     # %s\n' "$*" ;; esac; }

cmd() {                               # cmd ARGV…  — the only way a step changes the system
  case "$MODE" in
    plan) printf '     $ %s\n' "$(quote "$@")" ;;
    install|uninstall)
      printf '     $ %s\n' "$(quote "$@")"
      if [ "${EXEC%%:*}" = record ]; then quote "$@" >> "${EXEC#record:}"; echo >> "${EXEC#record:}"
      else "$@" || die "step $STEP_N failed: $(quote "$@")"; fi ;;
  esac
}

# put PATH MODE OWNER:GROUP < content   — write a file (atomically, backing up what it replaces)
put() {
  local path="$1" mode="$2" own="$3" real tmp; real="$(pth "$path")"
  local body; body="$(cat)"
  case "$MODE" in
    plan)
      printf '     write %s  (mode %s, %s, %d lines)\n' "$path" "$mode" "$own" "$(printf '%s\n' "$body" | wc -l)"
      if [ "${VERBOSE:-0}" = 1 ]; then printf '%s\n' "$body" | sed 's/^/       | /'; fi ;;
    install|uninstall)
      printf '     write %s  (mode %s, %s)\n' "$path" "$mode" "$own"
      backup "$path"
      mkdir -p "$(dirname "$real")"
      tmp="$(mktemp "$real.XXXXXX")"
      printf '%s\n' "$body" > "$tmp"
      chmod "$mode" "$tmp" 2>/dev/null || true
      if [ "${EXEC%%:*}" != record ]; then chown "$own" "$tmp"; fi
      mv -f "$tmp" "$real"
      [ "${EXEC%%:*}" = record ] && { quote chown "$own" "$path"; echo; } >> "${EXEC#record:}" ;;
  esac
}

# backup PATH — copy what is there now, so uninstall can put it back. Records "absent" too.
backup() {
  local path="$1" real; real="$(pth "$path")"
  [ "$MODE" = install ] || return 0
  mkdir -p "$(pth "$BACKUP_DIR")"
  if [ -e "$real" ] || [ -L "$real" ]; then
    mkdir -p "$(pth "$BACKUP_DIR")$(dirname "$path")"
    cp -a "$real" "$(pth "$BACKUP_DIR")$path"
  else
    printf '%s\n' "$path" >> "$(pth "$BACKUP_DIR")/ABSENT"
  fi
}

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
