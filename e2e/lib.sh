# e2e/lib.sh — shared by every e2e/0N-* script (plan §3 C19). Source it; it runs nothing.
#
# TAP lines are printed WITHOUT numbers; e2e/run numbers the whole stream and prints the plan.
#   ok   <desc>              pass
#   bad  <desc> [detail]     fail ("not ok")
#   skip <desc> <reason>     "ok ... # SKIP reason": the piece it needs is not built yet; never green evidence
#   check <desc> cmd...      ok if cmd exits 0, else bad
#
# The realm: every script runs in a scratch e2e realm under $E2E_ROOT and NEVER in the live one.
#   CC_TREE_REPO   $E2E_ROOT/cc-tree-e2e   (R22: a scratch tree repo, never pushed; `e2e_init` refuses
#                                           a path outside $E2E_ROOT or equal to the live default)
#   CCSBX_STATE    $E2E_ROOT/state         daemon state; CCSBX_TMUX_SOCKET e2e-<pid>, never "ccsbx"
#   CCSBX_ROOT     direct                  night-1 mode: results are not evidence (invariant 14 waived)
# Tunables (env): E2E_ROOT (default: a fresh mktemp -d, removed on exit; set it to keep the realm),
#   CCSBX_BIN (default <repo>/bin/ccsbx), E2E_S0_DIR (where `make standalone` lives, default ~/.dotfiles).

E2E_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO="$(cd "$E2E_DIR/.." && pwd)"
CCSBX="${CCSBX_BIN:-$REPO/bin/ccsbx}"

ok()   { echo "ok - $1"; }
bad()  { echo "not ok - $1"; [ -n "${2:-}" ] && echo "# ${2//$'\n'/$'\n'# }"; return 0; }
skip() { echo "ok - $1 # SKIP $2"; }
check() { local d="$1"; shift; local out; if out="$("$@" 2>&1)"; then ok "$d"; else bad "$d" "$out"; fi; }

e2e_init() {
  E2E_ROOT="${E2E_ROOT:-$(mktemp -d)}"; export E2E_ROOT
  mkdir -p "$E2E_ROOT"
  export CC_TREE_REPO="$E2E_ROOT/cc-tree-e2e"
  case "$CC_TREE_REPO" in "$E2E_ROOT"/*) ;; *) echo "e2e: CC_TREE_REPO outside the scratch realm" >&2; exit 2;; esac
  [ "$CC_TREE_REPO" != "$HOME/cc-tree" ] || { echo "e2e: refusing the live tree repo" >&2; exit 2; }
  export CCSBX_STATE="$E2E_ROOT/state" CCSBX_ROOT=direct CCSBX_TMUX_SOCKET="e2e-$$"
  export CCSBX_PROFILES="$REPO/profiles" CCSBX_PINS="$E2E_ROOT/no-pins.toml"
  [ -x "$CCSBX" ] || { echo "e2e: $CCSBX missing (make build)" >&2; exit 2; }
  if [ -z "${E2E_KEEP:-}" ]; then trap 'tmux -L "e2e-$$" kill-server 2>/dev/null; rm -rf "$E2E_ROOT"' EXIT; fi
}

# jget FIELD: a top-level or dotted field from JSON on stdin (python, no jq dependency)
jget() { python3 -c 'import json,sys; d=json.load(sys.stdin)
for k in sys.argv[1].split("."): d=d[k]
print(d)' "$1"; }

# spec NAME LID [sed-expr]: a LaunchSpec for the e2e realm, from the checked-in hello spec
spec() { sed -e 's/"realm":"production"/"realm":"e2e"/' -e 's/"dev-default"/"e2e-default"/' \
  -e "s/lid-hello-1/$2/" -e "s/cc-sandbox:hello/$1/" ${3:+-e "$3"} "$REPO/tests/specs/hello.json"; }

have_stub() { "$CCSBX" stub --help >/dev/null 2>&1; }   # C18, stream B: not on main until merged
