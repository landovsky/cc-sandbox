#!/bin/bash
# tests/hub-spoke-test.sh: B1 against the REAL session-bus hub (dotfiles branch n1-c, which has B10/B11:
# the live hub on :4011 does not, until Tomas merges it). It starts a throwaway hub on a spare loopback
# port from the dotfiles library (never the live hub), mints a spoke principal, and runs
# internal/hubspoke's real-hub test: hostd's spoke registers a fenced session and receives a test
# frame whose ack reaches the waiting /deliver. Direct mode: results are NOT EVIDENCE.
#
#   tests/hub-spoke-test.sh
#   CCSBX_DOTFILES=<dotfiles checkout with B10/B11> (default ~/.dotfiles/.worktrees/n1-g-hub2: B10/B11 plus
#   the M2b downlink payload and node changes; with a hub that has only B10/B11 the M2b test is skipped)
# Needs docker and the image session-bus:local; skips (exit 0, says so) when either is missing. The
# hub container is removed by its exact name, by the dotfiles library, and only the one it started.
set -u
case "${1:-}" in -h|--help) sed -n '2,11p' "$0" | sed 's/^# \{0,1\}//'; exit 0;; esac
D=${CCSBX_DOTFILES:-$HOME/.dotfiles/.worktrees/n1-g-hub2}
HERE=$(cd "$(dirname "$0")/.." && pwd)
[ -f "$D/tests/lib-sb-hub.sh" ] || { echo "skip: no session-bus test library at $D/tests/lib-sb-hub.sh"; exit 0; }
[ -f "$D/session-bus/lib/session_bus/http_spoke.ex" ] || { echo "skip: $D has no B11 (http_spoke.ex)"; exit 0; }
docker image inspect session-bus:local >/dev/null 2>&1 || { echo "skip: image session-bus:local is not present"; exit 0; }
# shellcheck disable=SC1091
source "$D/tests/lib-sb-hub.sh"
trap sb_cleanup EXIT
ADMIN="adm-$(head -c10 /dev/urandom | od -An -tx1 | tr -d ' \n')"
sb_stage || { echo "FAIL: staging the hub failed"; exit 1; }
NAME="sbccsbx-hub-$$"
sb_hub_start "$NAME" SB_SECRET="$ADMIN"
sb_hub_wait 60 || { echo "FAIL: the test hub did not come up"; sb_hub_log "$NAME" | tail -5; exit 1; }
TOK=$(curl -s -X POST -H 'content-type: application/json' -H "authorization: Bearer $ADMIN" \
  -d '{"role":"spoke","machine":"m1"}' "http://127.0.0.1:$SB_TEST_PORT/v1/principals" | jq -r .token)
[[ "$TOK" == sbp1.* ]] || { echo "FAIL: could not mint a spoke principal"; exit 1; }
cd "$HERE" || exit 1
export PATH="$PATH:$HOME/.local/go/bin"
M2B=""; grep -q principal_node "$D/session-bus/lib/session_bus/session_channel.ex" && M2B=1
CCSBX_REAL_HUB_URL="http://127.0.0.1:$SB_TEST_PORT" CCSBX_REAL_HUB_ADMIN="$ADMIN" CCSBX_REAL_HUB_SPOKE_TOKEN="$TOK" CCSBX_REAL_HUB_M2B="$M2B" \
  go test -count=1 -run RealHub -v ./internal/hubspoke ./cmd/ccsbx-hostd 2>&1 | tail -30
exit "${PIPESTATUS[0]}"
