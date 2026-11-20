#!/usr/bin/env bash
# install-linux.sh — the root installer for Tom's Linux machines (I0, U0, U0b). plan | install | check | uninstall.
# Why: U0 takes root paths away from tomas, so the morning install must be one reviewed, refusable, restorable script, not hand-typed sudo.
#
#   units/install-linux.sh plan                prints every step and the gate results; changes NOTHING; needs no root
#   (as root) units/install-linux.sh install --yes   does it (the morning gate, R10). Refuses unless every gate is met
#   units/install-linux.sh check               read-only: ok/FAIL per step; exit 0 only when the install is complete (after the reboot)
#   (as root) units/install-linux.sh uninstall --yes  restores from the FIRST backup (/var/backups/ccsbx-install/original)
#
# What it does (the same code prints it in `plan` and runs it in `install`):
#   U0b  users ccsbx + ccsbx-keys, the 0700 key directory, the machine and age keys, the signer unit
#   U0   ccsbx-root (fixed verbs) as the only NOPASSWD sudo grant; the NOPASSWD grants for docker, apt-get,
#        dokku and sops removed; sops root-owned; hostd unit (loopback only, no firewall rule);
#        the accidental lxd snap and its installer socket removed; the owner out of lxd and, LAST, docker
#   then: reboot. A fresh tmux has the new groups; the old one keeps docker.
#
# GATES. `install` REFUSES (exit 3) while any is unmet; `plan` shows which are:
#   B9   the hub is a native release under a systemd user unit (not the docker container `session-bus`),
#        because leaving the docker group would otherwise orphan the live hub
#   P15  every row of the P14 inventory is done or dropped (units/p15 check)
#   bins ccsbx-root, ccsbx-hostd, ccsbx-signer, ccsbx exist in --bin-dir and match its SHA256SUMS
#   sudoers  every sudoers.d NOPASSWD line parses (units/lib/sudoers_strip.py), so nothing is guessed
#   tools    visudo, python3, runuser (and age-keygen when there is no age key yet) are installed
#   owner    a plain login name with uid >= 1000, not root; --allowed-roots are absolute, existing, not /
#   probes   sops is not a symlink (chown would hit its target)
#
# Options (each also a CCSBX_* env var):
#   --yes                 really do it (install and uninstall)
#   --bin-dir DIR         built binaries + SHA256SUMS            (CCSBX_BIN_DIR, default <repo>/dist)
#   --owner USER          the human who keeps access            (CCSBX_OWNER, default the invoking user)
#   --allowed-roots A,B   ccsbx-root's workdir roots AND hostd's --worktree-root: one source, the same comma list written to both
#                                                              (CCSBX_ALLOWED_ROOTS, default ~owner/git,~owner/wt, the owner from SUDO_USER via getent)
#   --image REF           the fence image hostd passes to panes, BY DIGEST: ghcr.io/landovsky/ccsbx-fence@sha256:<64 hex> (CCSBX_IMAGE,
#                         required). A tag is refused: ccsbx-root and hostd refuse every start of a tag (image_not_pinned). A digest
#                         that is not in pins.toml is a candidate: only e2e-* profiles may run it. The digest comes from CI (U1a)
#   --machine NAME        the machine name the signer's scope allows   (CCSBX_MACHINE, default hostname -s)
#   --pairs P             what the signer may sign: exact profile:billing|billing,... (CCSBX_PAIRS; default: built from
#                         <repo>/profiles/*.json that list this machine, e.g. dev-default:api|stub,e2e-default:stub)
#   --side-image REF      the sidecar image of pod sessions, BY DIGEST: ghcr.io/landovsky/ccsbx@sha256:<64 hex> (CCSBX_SIDE_IMAGE, required;
#                         written to ccsbx-root.toml as k8s_side_image). A tag is refused. The digest comes from the side-image Actions run (#3wsr)
#   (rotating SB_SECRET: re-run `install --yes`: it copies the new first line again; the other steps are idempotent)
#   --revoke-doors U,V    the doors' owner-listener base URLs that take a token revoke (F2x); written as revoke_doors in ccsbx-root.toml and as
#                         hostd's --revoke-door (CCSBX_REVOKE_DOORS, default EMPTY: there are no doors before M1, and nothing is revoked without them)
#   --hub-url URL         where the hub is: http://A.B.C.D:PORT, an IPv4 literal (CCSBX_HUB_URL, REQUIRED; written to ccsbx-root.toml as hub_url). On hp
#                         today the hub listens on the tailnet address only (http://100.101.219.96:4011), not on loopback. The B9 gate checks the url
#                         against the live listeners: every listener on the port must be the owner's, and one must have that address
#   --hub-secret-from F   the owner's file holding the hub's shared secret (the hub's SB_SECRET), (CCSBX_HUB_SECRET_FROM, default
#                         ~owner/.config/session-bus/secret). Its FIRST LINE is copied to /etc/ccsbx/hub-admin.secret, root 0600, for
#                         ccsbx-root's relay mint; never printed. Must be a regular file (no symlink). Verify it at Gate M (night-1-morning.md, 8c)
#   --k8s-machine NAME    the machine pod sessions run on; the signer's keys peer (ccsbx-launch) signs only for it  (CCSBX_K8S_MACHINE, default future-two)
#   --with-deploy         also the ccsbx-deploy sudoers rule (Q12 = yes, R10c)  (CCSBX_WITH_DEPLOY=1)
#   --backup DIR          uninstall from this backup (must be under /var/backups/ccsbx-install), not the first one
#   --purge-keys          uninstall also deletes the keys and the ccsbx-keys user (rotate M9 first!)
#   -v, --verbose         plan prints the files it would write
# Gate overrides, for tests only: CCSBX_B9_CMD, CCSBX_P15_CMD (a command; exit 0 = met). They are honoured
# only together with the test hook CCSBX_ROOT_PREFIX; set alone, the installer refuses (they would be eval'd as root).
# P15's status file: CCSBX_P15_FILE, else <owner's home>/cc-tree/cc-sandbox/nodes/4vwb/p15-status.tsv (never root's $HOME).
# Exit: 0 ok, 1 a step failed / check failed, 2 usage, 3 refused (a gate is unmet).
set -uo pipefail
umask 077                 # nothing the installer writes is ever briefly world-readable
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
UNITS_DIR="$HERE"; REPO_DIR="$(dirname "$HERE")"

usage() { sed -n '2,/^set -uo/p' "${BASH_SOURCE[0]}" | sed '$d' | sed 's/^# \{0,1\}//'; }
case "${1:-}" in
  plan|install|check|uninstall) MODE="$1"; shift ;;
  -h|--help|help|"") usage; exit 0 ;;
  *) echo "install-linux: unknown command '$1' (plan|install|check|uninstall; --help)" >&2; exit 2 ;;
esac
YES=0; WITH_DEPLOY="${CCSBX_WITH_DEPLOY:-0}"; BIN_DIR="${CCSBX_BIN_DIR:-$REPO_DIR/dist}"
MACHINE="${CCSBX_MACHINE:-}"; PAIRS="${CCSBX_PAIRS:-}"; IMAGE="${CCSBX_IMAGE:-}"; SIDE_IMAGE="${CCSBX_SIDE_IMAGE:-}"; HUB_URL="${CCSBX_HUB_URL:-}"; REVOKE_DOORS="${CCSBX_REVOKE_DOORS:-}"
HUB_SECRET_FROM="${CCSBX_HUB_SECRET_FROM:-}"; K8S_MACHINE="${CCSBX_K8S_MACHINE:-future-two}"
ALLOWED_ROOTS="${CCSBX_ALLOWED_ROOTS:-}"; RESTORE_OVERRIDE=""; PURGE_KEYS=0; VERBOSE=0
while [ $# -gt 0 ]; do
  case "$1" in
    -h|--help) usage; exit 0 ;;                       # on every subcommand; runs nothing
    --yes) YES=1 ;;
    --bin-dir) BIN_DIR="${2:?--bin-dir DIR}"; shift ;;
    --owner) CCSBX_OWNER="${2:?--owner USER}"; shift ;;
    --allowed-roots) ALLOWED_ROOTS="${2:?--allowed-roots A,B}"; shift ;;
    --machine) MACHINE="${2:?--machine NAME}"; shift ;;
    --pairs) PAIRS="${2:?--pairs P}"; shift ;;
    --image) IMAGE="${2:?--image REF}"; shift ;;
    --side-image) SIDE_IMAGE="${2:?--side-image REF}"; shift ;;
    --revoke-doors) REVOKE_DOORS="${2:?--revoke-doors URL,URL}"; shift ;;
    --hub-url) HUB_URL="${2:?--hub-url http://A.B.C.D:PORT}"; shift ;;
    --hub-secret-from) HUB_SECRET_FROM="${2:?--hub-secret-from FILE}"; shift ;;
    --k8s-machine) K8S_MACHINE="${2:?--k8s-machine NAME}"; shift ;;
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
# the signer's scope (what the machine key may sign): owner, machine, profiles, billings
[ -n "$MACHINE" ] || MACHINE="$(hostname -s 2>/dev/null | tr 'A-Z' 'a-z')"   # lowercase: root lowercases its own, so a mixed-case name would make it refuse every start
# the pairs come from the profiles themselves: each profile that lists this machine, with ITS billings (a flat
# profiles x billings product would let a profile be signed with a billing it does not allow)
pairs_from() {   # pairs_from DIR — name:billing|billing,... of every profile in DIR that lists this machine
  compgen -G "$1/*.json" >/dev/null || return 0
  python3 -I - "$MACHINE" "$1"/*.json <<'PY' 2>/dev/null
import json, sys
machine, out = sys.argv[1], []
for f in sys.argv[2:]:
    p = json.load(open(f))
    if machine in p.get("machines", []) and p.get("billing"):
        out.append(p["name"] + ":" + "|".join(p["billing"]))
print(",".join(out))
PY
}
# the signer's pairs come from the repo's profiles at install (the repo is reviewed and merged; profiles are embedded elsewhere)
[ -n "$PAIRS" ] || PAIRS="$(pairs_from "$REPO_DIR/profiles")"
SCOPE_ERRORS=()
[[ "$K8S_MACHINE" =~ ^[a-z0-9][a-z0-9.-]*$ ]] && [ "$K8S_MACHINE" != "$MACHINE" ] || SCOPE_ERRORS+=("k8s machine '$K8S_MACHINE' must be a plain host name other than this machine (--k8s-machine)")
SCOPE_MACHINES="$MACHINE,$K8S_MACHINE"
[ -n "$HUB_SECRET_FROM" ] || HUB_SECRET_FROM="$OWNER_HOME/.config/session-bus/secret"
[[ "$MACHINE" =~ ^[a-z0-9][a-z0-9.-]*$ ]] || SCOPE_ERRORS+=("machine name '$MACHINE' is not a plain host name (--machine)")
_b='(api|subscription|stub)'
[[ "$PAIRS" =~ ^[A-Za-z0-9._-]+:$_b(\|$_b)*(,[A-Za-z0-9._-]+:$_b(\|$_b)*)*$ ]] || SCOPE_ERRORS+=("pairs '$PAIRS' is empty or not name:billing|billing,... (--pairs; default: profiles/*.json that list machine $MACHINE)")
# split on commas only (no globbing, no word splitting); each root must be a plain absolute path
ROOT_ERRORS=(); ALLOWED_ROOTS_TOML=""
IFS=, read -r -a _roots <<<"$ALLOWED_ROOTS"
for _r in "${_roots[@]}"; do
  if [[ "$_r" =~ ^/[A-Za-z0-9._@+/-]+$ ]] && [ "$_r" != / ] && [[ "$_r" != *..* ]] && [[ "$_r" != */ ]] && [[ "$_r" != *//* ]]; then
    # a root must be a real directory whose realpath is itself: hostd checks roots lexically, so a symlinked root
    # (or one with a symlink in its path) would make it refuse every start
    _p="$(pth "$_r")"; _base=""; [ -z "$PREFIX" ] || _base="$(readlink -f "$PREFIX")"
    if [ -z "$PREFIX" ] || [ -e "$_p" ]; then
      if [ ! -d "$_p" ]; then ROOT_ERRORS+=("allowed root $_r does not exist")
      elif [ "$(readlink -f "$_p")" != "$_base$_r" ]; then
        ROOT_ERRORS+=("allowed root $_r is not a real path (readlink -f gives $(readlink -f "$_p")): a symlink, or a symlink in its path; give the real directory")
      fi
    fi
    ALLOWED_ROOTS_TOML+="\"$_r\", "
  else ROOT_ERRORS+=("allowed root '$_r' is not a plain absolute path (no /, .., quotes, globs or spaces)"); fi
done
ALLOWED_ROOTS_TOML="${ALLOWED_ROOTS_TOML%, }"
# hostd --worktree-root takes the same comma list as allowed_roots (no spaces, no empty entry, no trailing slash, no ~)
[[ "$ALLOWED_ROOTS" =~ (^,|,,|,$) ]] && ROOT_ERRORS+=("allowed roots '$ALLOWED_ROOTS' has an empty entry")
WORKTREE_ROOT="$(IFS=,; echo "${_roots[*]}")"
# the repo list is pins.DefaultRepos (internal/pins/pins.go); a test keeps the two in step
if [ -z "$IMAGE" ]; then ROOT_ERRORS+=("no fence image: pass --image ghcr.io/landovsky/ccsbx-fence@sha256:<64 hex> (the CI-built candidate, U1a); a tag is refused")
elif [[ "$IMAGE" != *@sha256:* ]]; then ROOT_ERRORS+=("image '$IMAGE' is a tag: hostd and ccsbx-root refuse every start of a tag (image_not_pinned); use ghcr.io/landovsky/ccsbx-fence@sha256:<64 hex>")
elif ! [[ "$IMAGE" =~ ^ghcr\.io/landovsky/ccsbx-fence@sha256:[0-9a-f]{64}$ ]]; then ROOT_ERRORS+=("image '$IMAGE' is not ghcr.io/landovsky/ccsbx-fence@sha256:<64 hex> (the only repo a candidate may come from)"); fi
# the hub's address: an IPv4 literal, with a port (no host names: nothing to resolve or trick)
[[ "$HUB_URL" =~ ^http://((25[0-5]|2[0-4][0-9]|1[0-9][0-9]|[1-9]?[0-9])\.){3}(25[0-5]|2[0-4][0-9]|1[0-9][0-9]|[1-9]?[0-9]):[1-9][0-9]{0,4}$ ]] || ROOT_ERRORS+=("no hub url: pass --hub-url http://A.B.C.D:PORT (a canonical IPv4 literal, no leading zeros; on hp today http://100.101.219.96:4011, the hub is on the tailnet address, not loopback); got '${HUB_URL}'")
# the doors' revoke URLs: empty, or a comma list of plain base URLs (they land in a systemd ExecStart and a TOML list, so no quotes, spaces or paths)
REVOKE_DOORS_TOML=""
if [ -n "$REVOKE_DOORS" ]; then
  IFS=, read -r -a _rd <<<"$REVOKE_DOORS"
  for _u in "${_rd[@]}"; do
    if [[ "$_u" =~ ^https?://[A-Za-z0-9._-]+(:[1-9][0-9]{0,4})?$ ]]; then REVOKE_DOORS_TOML+="\"$_u\", "
    else ROOT_ERRORS+=("revoke door '$_u' is not a plain http(s)://host[:port] base URL (--revoke-doors)"); fi
  done
  REVOKE_DOORS_TOML="${REVOKE_DOORS_TOML%, }"
fi
# the sidecar image: by digest from our own repo only; ccsbx-root's pod-create refuses a config without it
if [ -z "$SIDE_IMAGE" ]; then ROOT_ERRORS+=("no sidecar image: pass --side-image ghcr.io/landovsky/ccsbx@sha256:<64 hex> (the digest of the side-image Actions run, #3wsr); a tag is refused")
elif ! [[ "$SIDE_IMAGE" =~ ^ghcr\.io/landovsky/ccsbx@sha256:[0-9a-f]{64}$ ]]; then ROOT_ERRORS+=("sidecar image '$SIDE_IMAGE' is not ghcr.io/landovsky/ccsbx@sha256:<64 hex> (a tag, or another repo, is refused)"); fi
# the root-owned pins copy: ccsbx-root and hostd read it, never the owner-writable repo (profiles are embedded in the binaries)
MANAGED_PATHS+=(/etc/ccsbx/pins.toml)
[ -f "$REPO_DIR/pins.toml" ] || ROOT_ERRORS+=("no $REPO_DIR/pins.toml to install as /etc/ccsbx/pins.toml")
RESTORE_DIR="${RESTORE_OVERRIDE:-$BACKUP_DIR}"
export OWNER MACHINE PAIRS IMAGE SIDE_IMAGE HUB_URL REVOKE_DOORS REVOKE_DOORS_TOML WORKTREE_ROOT BIN_DIR UNITS_DIR ALLOWED_ROOTS_TOML RESTORE_DIR

# ---------------------------------------------------------------- gates (read-only)
GATE_MSGS=(); NOTES=()
gate() { GATE_MSGS+=("$1"); }                                  # an unmet gate
# The hub secret is read ONCE, AS THE OWNER (so a root-only file is unreadable even through a swapped link), with O_NOFOLLOW on the
# last component, regular files only, at most 4 KiB, and a first line ccsbx-root's own rule accepts (1..512 printable, no space).
# That one buffer is validated and later written; root never opens the owner's path. A link in a parent directory is the owner's
# to make and can only lead to what the owner can already read. In record mode the call is recorded and made as the current user.
hub_secret_py='import os, re, stat, sys
try:
    fd = os.open(sys.argv[1], os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK | os.O_CLOEXEC)
except OSError as e:
    sys.stderr.write("cannot open it (a link in the last component is refused): %s\n" % e.strerror); sys.exit(3)
st = os.fstat(fd)
if not stat.S_ISREG(st.st_mode): sys.stderr.write("not a regular file\n"); sys.exit(3)
if st.st_size > 4096: sys.stderr.write("larger than 4 KiB\n"); sys.exit(3)
b = os.read(fd, 4097).split(b"\n", 1)[0].rstrip(b"\r")
if not re.fullmatch(rb"[\x21-\x7e]{1,512}", b): sys.stderr.write("the first line is empty, longer than 512 bytes, or has a space or control character\n"); sys.exit(3)
sys.stdout.write(b.decode("ascii"))'
HUB_SECRET=""
load_hub_secret() {
  local out rc err
  err="$(mktemp)" || die "cannot create a temp file"
  out="$(as_owner python3 -I -c "$hub_secret_py" "$HUB_SECRET_FROM" 2>"$err")"; rc=$?
  if [ "$rc" -ne 0 ]; then gate "hub secret file $HUB_SECRET_FROM: $(head -1 "$err") (pass --hub-secret-from FILE; ccsbx-root's relay mint needs the hub's secret)"; HUB_SECRET=""
  else HUB_SECRET="$out"; fi
  rm -f "$err"
}
as_owner() {                                                   # run a user-level command as the owner
  if [ "$(id -u)" = 0 ] && [ "$OWNER" != root ]; then runuser -u "$OWNER" -- env "XDG_RUNTIME_DIR=/run/user/$(id -u "$OWNER")" "$@"
  else "$@"; fi
}
default_b9() {
  local unit="${CCSBX_B9_UNIT:-session-bus-hub.service}" exec
  as_owner systemctl --user is-active --quiet "$unit" 2>/dev/null || { echo "user unit $unit is not active"; return 1; }
  exec="$(as_owner systemctl --user show -p ExecStart --value "$unit" 2>/dev/null)"
  case "$exec" in *docker*) echo "$unit still starts docker"; return 1;; esac
  if command -v docker >/dev/null 2>&1; then
    local ps
    ps="$(as_owner docker ps --filter name='^session-bus$' -q 2>&1)" || { echo "cannot ask docker whether session-bus runs (docker ps failed): $(head -1 <<<"$ps")"; return 1; }
    [ -z "$ps" ] || { echo "docker container session-bus is still running"; return 1; }
  fi
  # the hub's address must be where the owner's hub really listens (every listener the owner's, one of them at HUB_URL's address)
  if [[ "$HUB_URL" =~ ^http://((25[0-5]|2[0-4][0-9]|1[0-9][0-9]|[1-9]?[0-9])\.){3}(25[0-5]|2[0-4][0-9]|1[0-9][0-9]|[1-9]?[0-9]):[1-9][0-9]{0,4}$ ]]; then
    local why; why="$(python3 -I "$UNITS_DIR/lib/hub_listen.py" "$HUB_URL" "$(id -u "$OWNER")")" || { echo "--hub-url $HUB_URL does not match the live hub: $why"; return 1; }
  fi
  return 0
}
P15_FILE="${CCSBX_P15_FILE:-$OWNER_HOME/cc-tree/cc-sandbox/nodes/4vwb/p15-status.tsv}"   # never $HOME: under sudo that is /root
default_p15() { as_owner env CCSBX_P15_FILE="$P15_FILE" "$UNITS_DIR/p15" check; }
eval_gates() {
  local m f missing="" e unlisted
  load_hub_secret
  local sp; while IFS= read -r sp; do [ -z "$sp" ] || gate "$sp"; done < <(svc_users_check)
  if [ -z "$PREFIX" ] && [ -n "${CCSBX_B9_CMD:-}${CCSBX_P15_CMD:-}" ]; then
    refuse "CCSBX_B9_CMD / CCSBX_P15_CMD are test overrides and are set: unset them (sudo -E would pass them to root)"
  fi
  # the owner keeps access, so it must be a real login: root, ALL or a typo would put the rule on the wrong account
  case "$OWNER" in ""|root|ALL|*[!a-z0-9_-]*|[!a-z_]*) gate "owner '$OWNER' is not a plain login name (run as sudo from the owner's shell, or --owner USER)";; esac
  if [ -z "$PREFIX" ]; then
    if ! id -u "$OWNER" >/dev/null 2>&1; then gate "owner '$OWNER' does not exist"
    elif [ "$(id -u "$OWNER")" -lt 1000 ]; then gate "owner '$OWNER' is a system account (uid < 1000)"; fi
  fi
  for e in "${ROOT_ERRORS[@]}" "${SCOPE_ERRORS[@]}"; do gate "$e"; done
  if [ -n "${CCSBX_B9_CMD:-}" ]; then m="$(eval "$CCSBX_B9_CMD" 2>&1)" || gate "B9 missing: ${m:-$CCSBX_B9_CMD failed}"
  else m="$(default_b9)" || gate "B9 missing: $m (the native hub release, stream C)"; fi
  if [ -n "${CCSBX_P15_CMD:-}" ]; then m="$(eval "$CCSBX_P15_CMD" 2>&1)" || gate "P15 open: ${m:-$CCSBX_P15_CMD failed}"
  else m="$(default_p15 2>&1)" || gate "P15 open: $(head -1 <<<"$m") (run units/p15 status)"; fi
  for f in ccsbx-root ccsbx-hostd ccsbx-signer ccsbx; do [ -f "$BIN_DIR/$f" ] || missing+=" $f"; done
  [ -z "$missing" ] || gate "binaries missing in $BIN_DIR:$missing (a linux build of each + SHA256SUMS in --bin-dir; see --help)"
  if [ -z "$missing" ]; then
    if [ ! -f "$BIN_DIR/SHA256SUMS" ]; then gate "no $BIN_DIR/SHA256SUMS"
    elif unlisted="$(for f in ccsbx-root ccsbx-hostd ccsbx-signer ccsbx; do grep -Eq "^[0-9a-f]{64} [ *]${f}\$" "$BIN_DIR/SHA256SUMS" || printf ' %s' "$f"; done)"; [ -n "$unlisted" ]; then
      gate "SHA256SUMS does not list:$unlisted (an unlisted file would pass unchecked)"
    elif ! (cd "$BIN_DIR" && sha256sum --quiet -c SHA256SUMS >/dev/null 2>&1); then gate "binaries do not match SHA256SUMS"; fi
  fi
  local tools="visudo python3 runuser"; [ -e "$(pth /var/lib/ccsbx-keys/age.key)" ] || tools+=" age-keygen"
  for f in $tools; do command -v "$f" >/dev/null 2>&1 || [ -n "$PREFIX" ] || gate "tool not installed: $f"; done
  if [ -L "$(pth /usr/local/bin/sops)" ]; then gate "/usr/local/bin/sops is a symlink: chown would change its target; replace it with the real file first"; fi
  if [ "$WITH_DEPLOY" = 1 ]; then
    [ -f "$BIN_DIR/ccsbx-deploy" ] && grep -Eq "^[0-9a-f]{64} [ *]ccsbx-deploy\$" "$BIN_DIR/SHA256SUMS" 2>/dev/null && (cd "$BIN_DIR" && grep -E " [ *]ccsbx-deploy\$" SHA256SUMS | sha256sum --quiet -c - >/dev/null 2>&1) \
      || gate "--with-deploy: ccsbx-deploy is missing in $BIN_DIR, not listed in SHA256SUMS, or does not match it"
  fi
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
# DONE only when the services really came up: `enable --now` of a Type=simple unit returns 0 even for a
# crash-looping daemon (review R1). In record mode CCSBX_TEST_POSTCHECK_RC stands in for the answer.
post_install_check() {
  step "post-install check: hostd and the signer must be up before DONE"
  local i ready=1
  if [ "$MODE" = plan ]; then note "waits up to 10 s for ccsbx-hostd, ccsbx-signer.socket and ccsbx-signer-launch.socket to be active and their sockets to exist; otherwise install stops, no DONE"; return 0; fi
  if [ "${EXEC%%:*}" = record ]; then rec post-install-check; ready="${CCSBX_TEST_POSTCHECK_RC:-0}"
  else
    for i in 1 2 3 4 5 6 7 8 9 10; do
      if systemctl is-active --quiet ccsbx-tmux.service && systemctl is-active --quiet ccsbx-hostd.service && systemctl is-active --quiet ccsbx-signer.socket \
         && systemctl is-active --quiet ccsbx-signer-launch.socket && [ -S "$SIGNER_SOCK" ] && [ -S "$LAUNCH_SOCK" ] && [ -S "$HOSTD_SOCK" ]; then ready=0; break; fi
      sleep 1
    done
  fi
  [ "$ready" = 0 ] || die "ccsbx-hostd or the signer is not up (a crash loop?): see 'journalctl -u ccsbx-hostd -u ccsbx-signer'. NOT DONE"
  # the signer is socket-activated: the socket file exists even if the service exits 2 on a bad scope, so ask it to sign
  if [ "${EXEC%%:*}" = record ]; then rec sign-probe; [ "${CCSBX_TEST_SIGN_RC:-0}" = 0 ] || die "the signer did not sign the in-scope test claim (bad scope or key?): see 'journalctl -u ccsbx-signer'. NOT DONE"
  else
    CHECK_FAIL=0; sign_check
    [ "$CHECK_FAIL" -eq 0 ] || die "the signer did not sign the in-scope test claim (see above; 'journalctl -u ccsbx-signer'). NOT DONE"
  fi
  note "up, and the signer signs"
}
steps_install() {
  if [ "$MODE" = install ]; then step "back up every file the install replaces (once; never overwritten)"; backup_originals; fi
  # the read happened in eval_gates (as the owner); in record mode it is logged here, once the gates and the backup have passed, so a refusal records nothing
  if [ "$MODE" = install ] && [ "${EXEC%%:*}" = record ]; then rec runuser -u "$OWNER" -- python3 -I -c "$hub_secret_py" "$HUB_SECRET_FROM"; fi
  # everything that can fail for a machine-specific reason comes BEFORE sudoers is changed
  u0b_users; u0_binaries; u0b_keys; u0b_signer; u0_hostd
  if [ "$WITH_DEPLOY" = 1 ]; then u0_deploy_rule; fi
  u0_sops; u0_lxd
  u0_sudoers; u0_groups
  post_install_check
  if [ "$MODE" = install ]; then step "mark the install complete"; backup_state complete || die "cannot mark the backup complete"; fi
}

case "$MODE" in
  plan)
    echo "install-linux plan  ($(date '+%F %T'), owner=$OWNER, bin-dir=$BIN_DIR)"
    echo "Nothing below is run by 'plan'. The same steps run under 'install --yes' as root."
    eval_gates; print_gates
    echo "  P15 status file: $P15_FILE"; echo "  allowed roots:   ${ALLOWED_ROOTS_TOML:-(none)}"
    echo "  signer scope:    owners=$OWNER machines=$SCOPE_MACHINES pairs=$PAIRS"
    steps_install
    echo; echo "AFTER: reboot, then units/install-linux.sh check  and  bin/ccsbx-preflight --morning --post-M1"
    echo "UNDO:  as root, units/install-linux.sh uninstall --yes   (restores the first backup, $BACKUP_DIR; a failed install is undone the same way)"
    ;;
  install)
    [ "$YES" = 1 ] || refuse "install changes the system. Read 'plan', then run it as root with --yes"
    [ "$(id -u)" = 0 ] || [ -n "$PREFIX" ] || refuse "install must run as root"
    [ "$(uname -s)" = Linux ] || refuse "this is the Linux installer (Mac: install-mac.sh)"
    eval_gates
    if [ "${#GATE_MSGS[@]}" -gt 0 ]; then print_gates >&2; refuse "${#GATE_MSGS[@]} gate(s) unmet; nothing was changed"; fi
    echo "install-linux: all gates met; the first backup goes to $BACKUP_DIR"
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
    restore_dir_ok "$RESTORE_DIR"
    u0_uninstall; u0b_uninstall
    echo; echo "DONE. docker/lxd were not reinstalled or re-granted: that is deliberate."
    ;;
esac
