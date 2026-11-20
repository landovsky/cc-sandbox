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
#   CCSBX_BIN (default <repo>/bin/ccsbx), E2E_S0_DIR (where `make standalone` lives, default ~/.dotfiles),
#   E2E_IMAGE (the fence image the docker steps run, default ccsbx-fence:dev; never pulled),
#   E2E_CCF / E2E_BASE_IMAGE (the client layer's ccf/ directory and the python-bearing fence image the candidate is built on),
#   E2E_DOTFILES (a dotfiles checkout with the session-bus test library lib-sb-hub.sh and a hub with B10/B11/M2b;
#   default ~/.dotfiles/.worktrees/n1-g-hub2), E2E_CCTREE (a dotfiles checkout with the remote start path, C12/C16;
#   default ~/.dotfiles/.worktrees/n2-f-c16). A leg whose piece is missing is a SKIP that says which.
#
# Docker, and only what this run made: the steps that start a fence write the sid to $E2E_ROOT/sids and the worktree
# they made to $E2E_ROOT/trash, and the exit trap hands both to tests/fence-cleanup.sh, which refuses empty or foreign
# values and removes exact names only (ccsbx-<sid>-g1, ccsbx-net-<sid>-g1, /var/tmp/ccsbx-cc-XXXXXX). Nothing here
# lists, prunes or removes containers by pattern. Background processes this run starts are in E2E_PIDS and are
# killed by pid.

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
  # a session's worktree must be strictly inside a worktree root: the scratch ones are /var/tmp/ccsbx-cc-XXXXXX
  export CCSBX_WORKTREE_ROOT=/var/tmp CCSBX_MACHINE="${CCSBX_MACHINE:-hp-ubuntu}"
  E2E_IMAGE="${E2E_IMAGE:-ccsbx-fence:dev}"; E2E_PIDS=(); E2E_WORK=""
  : > "$E2E_ROOT/sids"; : > "$E2E_ROOT/trash"; : > "$E2E_ROOT/extras"
  [ -x "$CCSBX" ] || { echo "e2e: $CCSBX missing (make build)" >&2; exit 2; }
  if [ -z "${E2E_KEEP:-}" ]; then trap e2e_cleanup EXIT; else trap e2e_keep_note EXIT; fi
}

# e2e_keep_note: the exit trap with E2E_KEEP=1. Nothing is removed, so say what is left: the realm, and by exact name every
# container of this run that is still there (read-only `docker inspect` of names built from the validated sids; no listing).
e2e_keep_note() {
  tmux -L "e2e-$$" kill-server 2>/dev/null
  local p; for p in "${E2E_PIDS[@]:-}"; do [ -n "$p" ] && kill "$p" 2>/dev/null; done
  wait 2>/dev/null
  local s g x left=""
  while IFS= read -r s; do
    [[ "$s" =~ ^s[0-9a-hjkmnp-tv-z]{10}$ ]] || continue
    for g in 1 2 3; do docker inspect "ccsbx-$s-g$g" >/dev/null 2>&1 && left="$left ccsbx-$s-g$g"; done
  done < "$E2E_ROOT/sids" 2>/dev/null
  while IFS= read -r x; do [[ "$x" =~ ^ccsbx-(fwd|canary)-[0-9]+$ ]] && docker inspect "$x" >/dev/null 2>&1 && left="$left $x"; done < "$E2E_ROOT/extras" 2>/dev/null
  echo "e2e: E2E_KEEP is set: the realm $E2E_ROOT was kept. Containers of this run still there:${left:- none}" >&2
  [ -n "$left" ] && echo "e2e: remove them yourself, by name (docker rm -f NAME), when you are done; the networks are ccsbx-net-<sid>-gN" >&2
  return 0
}

# e2e_cleanup: the exit trap. Kills this run's tmux server and background pids, then (only if a step made docker
# objects or worktrees) lets tests/fence-cleanup.sh remove exactly those; its guard refuses anything that does not look
# like this run's own scratch realm, in which case the realm is left for a human, never swept wider.
e2e_cleanup() {
  tmux -L "e2e-$$" kill-server 2>/dev/null
  local p; for p in "${E2E_PIDS[@]:-}"; do [ -n "$p" ] && kill "$p" 2>/dev/null; done
  wait 2>/dev/null
  if [ -s "$E2E_ROOT/sids" ] || [ -s "$E2E_ROOT/trash" ]; then
    local STATE="$E2E_ROOT" IMG="$E2E_IMAGE" PIDS=()
    local CLEANUP_EXTRA; CLEANUP_EXTRA="$(tr '\n' ' ' < "$E2E_ROOT/extras" 2>/dev/null)"   # containers a step made beside the fence
    # shellcheck disable=SC1091
    . "$REPO/tests/fence-cleanup.sh"
    # fence-cleanup.sh knows generation 1; a resume made g2/g3 of the same validated sids: their exact names too
    if cleanup_guard 2>/dev/null; then
      local s g
      while IFS= read -r s; do
        for g in 2 3; do docker rm -f "ccsbx-$s-g$g" >/dev/null 2>&1; docker network rm "ccsbx-net-$s-g$g" >/dev/null 2>&1; done
      done < "$E2E_ROOT/sids"
    fi
    cleanup || echo "e2e: the scratch realm $E2E_ROOT was left in place (cleanup refused; see above)" >&2
  fi
  # only a realm that never held a docker object or a worktree is swept here; a refused cleanup leaves its realm (and its
  # trash list) for a human, never rm -rf over files only the fence's uid can remove
  [ -d "$E2E_ROOT" ] && [ ! -s "$E2E_ROOT/sids" ] && [ ! -s "$E2E_ROOT/trash" ] && [ ! -s "$E2E_ROOT/extras" ] && rm -rf "$E2E_ROOT"
  return 0
}

# e2e_work: a scratch worktree for a session (made once per script, world-writable so the fence's uid can write it).
e2e_work() {
  [ -n "$E2E_WORK" ] && return 0
  E2E_WORK="$(mktemp -d /var/tmp/ccsbx-cc-XXXXXX)"; chmod 0777 "$E2E_WORK"; echo "$E2E_WORK" >> "$E2E_ROOT/trash"
}

# e2e_sid REPLY_JSON: the sid out of a start reply, recorded for the cleanup (only a sid of the shape mintSid makes is kept)
e2e_sid() {
  local sid; sid="$(jget sid <<<"$1" 2>/dev/null)" || return 1
  [[ "$sid" =~ ^s[0-9a-hjkmnp-tv-z]{10}$ ]] || return 1
  echo "$sid" >> "$E2E_ROOT/sids"; echo "$sid"
}

# e2e_wait SECONDS cmd...: poll cmd every 0.2 s until it succeeds or the time is up
e2e_wait() { local t=$(( $1 * 5 )); shift; while [ $t -gt 0 ]; do "$@" >/dev/null 2>&1 && return 0; sleep 0.2; t=$((t-1)); done; return 1; }

# e2e_port: a free loopback port (the spare-port rule: nothing here ever uses a live service's port)
e2e_port() { python3 -c 'import socket;s=socket.socket();s.bind(("127.0.0.1",0));print(s.getsockname()[1])'; }

# e2e_candidate: the CANDIDATE image (fence image + the client layer at /opt/ccf, plan C3/H2), built once and cached by the
# layer's digest. Sets E2E_CAND to its tag and returns 0, or returns 1 with E2E_CAND_WHY saying which piece is missing.
#   E2E_CCF       a dotfiles ccf/ directory (default: .worktrees/n1-b-ccf/ccf, else .worktrees/n1-g-cc/ccf)
#   E2E_BASE_IMAGE  the fence image to build on (default $E2E_IMAGE); it must have python3 (image/pins.env PYTHON3_MINIMAL_VERSION)
# It runs `docker build` (an image per layer digest stays as a cache; nothing here removes images) and never pulls.
e2e_candidate() {
  E2E_CAND=""; E2E_CAND_WHY=""
  local ccf="${E2E_CCF:-}" base="${E2E_BASE_IMAGE:-$E2E_IMAGE}"
  [ -n "$ccf" ] || for ccf in "$HOME/.dotfiles/.worktrees/n1-b-ccf/ccf" "$HOME/.dotfiles/.worktrees/n1-g-cc/ccf"; do [ -x "$ccf/ccf-pack" ] && break; done
  [ -x "$ccf/ccf-pack" ] || { E2E_CAND_WHY="no dotfiles ccf/ directory with ccf-pack (set E2E_CCF; the layer is on dotfiles n1-g-cc / n1-b-ccf, not on main)"; return 1; }
  E2E_CCF_USED="$ccf"
  docker image inspect "$base" >/dev/null 2>&1 || { E2E_CAND_WHY="the base image $base is not present"; return 1; }
  docker run --rm "$base" python3 -c 'import json' >/dev/null 2>&1 || { E2E_CAND_WHY="the base image $base has no python3 with json (rebuild it with image/build.sh from pins that have PYTHON3_MINIMAL_VERSION)"; return 1; }
  local dist="$E2E_ROOT/ccf-dist"; mkdir -p "$dist"
  "$ccf/ccf-pack" --out "$dist" >/dev/null 2>&1 || { E2E_CAND_WHY="ccf-pack failed"; return 1; }
  local tar; tar="$dist/$(sed -n 's/.*"tar":"\([^"]*\)".*/\1/p' "$dist/candidate.json" 2>/dev/null | head -1)"   # the one ccf-pack just wrote, not a stale one
  [ -n "$tar" ] || { E2E_CAND_WHY="ccf-pack made no tar"; return 1; }
  local out; out="$("$REPO/image/build-ccf.sh" --ccf-tar "$tar" --base "$base" 2>&1)" || { E2E_CAND_WHY="build-ccf.sh: $(echo "$out" | tail -2 | tr '\n' ' ')"; return 1; }
  E2E_CAND="$(echo "$out" | awk '/^candidate /{print $2}' | tail -1)"
  [ -n "$E2E_CAND" ] || { E2E_CAND_WHY="build-ccf.sh printed no candidate"; return 1; }
}

have_image() { docker image inspect "$E2E_IMAGE" >/dev/null 2>&1; }

# jget FIELD: a top-level or dotted field from JSON on stdin (python, no jq dependency)
jget() { python3 -c 'import json,sys; d=json.load(sys.stdin)
for k in sys.argv[1].split("."): d=d[k]
print(d)' "$1"; }

# spec NAME LID [sed-expr]: a LaunchSpec for the e2e realm, from the checked-in hello spec
spec() { e2e_work; sed -e 's/"realm":"production"/"realm":"e2e"/' -e 's/"dev-default"/"e2e-default"/' \
  -e "s/lid-hello-1/$2/" -e "s/cc-sandbox:hello/$1/" -e "s#/var/tmp/ccsbx-hello#$E2E_WORK#" ${3:+-e "$3"} "$REPO/tests/specs/hello.json"; }

have_stub() { "$CCSBX" stub --help >/dev/null 2>&1; }   # C18, stream B: not on main until merged

# e2e_fwd SID: the forwarder with the scripted stub door (C18) behind it, as a peer on the session's network under the
# alias "fwd", fed its token and generation key on STDIN: the fence holds neither, and reaches the model only here. The
# container is named ccsbx-fwd-$$ (the shape fence-cleanup.sh accepts), recorded in $E2E_ROOT/extras for the exit trap.
# Needs the session's container to be up (its network exists). Returns 1 with the reason on stderr.
e2e_fwd() {
  local sid="$1" net="ccsbx-net-$1-g1" bin; bin="$(readlink -f "$CCSBX")"
  "$CCSBX" token mint --key-file "$E2E_ROOT/dev.key" --sub "tomas/hp-ubuntu/$sid/g1" --label e2e --billing stub --profile e2e-default \
    --aud ccsbx-stub --fwd-secrets > "$E2E_ROOT/secrets.json" 2>"$E2E_ROOT/mint.err" || { cat "$E2E_ROOT/mint.err" >&2; return 1; }
  chmod 0600 "$E2E_ROOT/dev.key" "$E2E_ROOT/secrets.json"
  "$CCSBX" token pubkey --key-file "$E2E_ROOT/dev.key" --owners tomas --machines hp-ubuntu --profiles 'e2e-*' --billings stub > "$E2E_ROOT/keyring" || return 1
  echo "ccsbx-fwd-$$" >> "$E2E_ROOT/extras"
  docker run --rm -i --name "ccsbx-fwd-$$" --network "$net" --network-alias fwd -v "$bin:/ccsbx:ro" -v "$E2E_ROOT/keyring:/keyring:ro" "$E2E_IMAGE" sh -c \
    '/ccsbx stub --listen 127.0.0.1:8081 --keyring /keyring --state /tmp/stub-state.json & exec /ccsbx fwd --listen :8080 --door stub=http://127.0.0.1:8081' \
    < "$E2E_ROOT/secrets.json" > "$E2E_ROOT/fwd.log" 2>&1 &
  E2E_PIDS+=("$!")
  e2e_wait 20 grep -q "serving :8080" "$E2E_ROOT/fwd.log"
}
