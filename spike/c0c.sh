#!/usr/bin/env bash
# C0c spike: hook parity between the host and the fence (plan §3 C0c, ccf client layer C3).
#
#   spike/c0c.sh fence [OUT]     run Claude Code in a fence-flag container (uid 10001, read-only, no
#                                caps, internal network) against a local fake API; write the hook
#                                payload FIELD NAMES to OUT (default spike/fields-fence.txt)
#   spike/c0c.sh host  [OUT]     the same on the host; REFUSES where root-owned managed settings
#                                exist (they pin the model base URL to the live proxy, so a host run
#                                could not reach the fake API, or would use the real login)
#   spike/c0c.sh diff            diff fields-host.txt fields-fence.txt  (empty = parity)
#   spike/c0c.sh --help
# Needs: docker with the image CCSBX_IMAGE (default python:3.12-slim, present locally, never pulled),
# and the host's claude binary CCSBX_CLAUDE_BIN (default ~/.local/bin/claude, resolved). It removes only
# what it created (names carry its pid). Payload VALUES are never recorded, only field names and types.
set -uo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
case "${1:-}" in -h|--help|"") sed -n '2,15p' "$0" | sed 's/^# \{0,1\}//'; exit 0;; esac
IMG=${CCSBX_IMAGE:-python:3.12-slim}
CLAUDE=${CCSBX_CLAUDE_BIN:-$(readlink -f "$HOME/.local/bin/claude")}
MANAGED=/etc/claude-code/managed-settings.json
PORT=8099
PROMPT="run the true command then say done"

settings() { # $1 = the dump command prefix
  python3 - "$1" <<'PY'
import json, sys
c = sys.argv[1]
ev = ["SessionStart", "UserPromptSubmit", "PreToolUse", "PostToolUse", "Stop", "SubagentStop", "Notification", "PreCompact", "SessionEnd"]
print(json.dumps({"skipDangerousModePermissionPrompt": True,
  "hooks": {e: [{"hooks": [{"type": "command", "command": f"{c} {e}"}]}] for e in ev},
  "statusLine": {"type": "command", "command": f"{c} statusLine"}}))
PY
}

case "$1" in
fence)
  OUT=${2:-$HERE/fields-fence.txt}
  [ -x "$CLAUDE" ] || { echo "no claude binary at $CLAUDE" >&2; exit 1; }
  docker image inspect "$IMG" >/dev/null 2>&1 || { echo "image $IMG not present (not pulling)" >&2; exit 1; }
  W=$(mktemp -d); mkdir -p "$W/seed/.claude" "$W/out" "$W/work"; chmod 777 "$W/out" "$W/work"
  settings "python3 /spike/c0c-dump.py" > "$W/seed/.claude/settings.json"
  echo '{"hasCompletedOnboarding":true,"numStartups":3,"projects":{"/work":{"hasTrustDialogAccepted":true}}}' > "$W/seed/.claude.json"
  chmod -R a+rX "$W/seed"
  NET=ccsbx-net-c0c-$$; CT=ccsbx-c0c-$$
  docker network create --internal "$NET" >/dev/null || exit 1
  docker run --rm --name "$CT" --user 10001:10001 --read-only --cap-drop ALL --security-opt no-new-privileges \
    --pids-limit 512 --memory 2g --tmpfs /tmp:rw,exec,size=256m --tmpfs /home/agent:rw,exec,size=128m,uid=10001,gid=10001 --network "$NET" \
    -v "$CLAUDE:/usr/local/bin/claude:ro" -v "$HERE:/spike:ro" -v "$W/seed:/seed:ro" -v "$W/out:/out" -v "$W/work:/work" -w /work \
    -e HOME=/home/agent -e LANG=C.UTF-8 -e C0C_OUT=/out/fields.txt -e DISABLE_AUTOUPDATER=1 -e CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1 \
    -e ANTHROPIC_BASE_URL=http://127.0.0.1:$PORT -e ANTHROPIC_AUTH_TOKEN=c0c-placeholder \
    "$IMG" sh -c "cp -r /seed/. /home/agent/ && (python3 -u /spike/c0c-stub.py 127.0.0.1 $PORT >/tmp/stub.log 2>&1 &) && sleep 1 && claude -p '$PROMPT' --dangerously-skip-permissions --output-format json >/out/claude.json 2>/out/claude.err; echo claude-exit=\$?"
  docker network rm "$NET" >/dev/null 2>&1
  [ -s "$W/out/fields.txt" ] || { echo "no payloads recorded; claude said:"; head -c 600 "$W/out/claude.err" "$W/out/claude.json"; exit 1; }
  sort -u "$W/out/fields.txt" > "$OUT"
  echo "wrote $OUT ($(wc -l < "$OUT") field rows; events: $(cut -f1 "$OUT" | sort -u | tr '\n' ' '))"
  echo "reply: $(python3 -c 'import json,sys;print(json.load(open(sys.argv[1])).get("result"))' "$W/out/claude.json" 2>/dev/null)"
  ;;
host)
  OUT=${2:-$HERE/fields-host.txt}
  if [ -e "$MANAGED" ] && [ "${CCSBX_C0C_ALLOW_MANAGED:-}" != 1 ]; then
    echo "refused: root-owned managed settings exist on this machine. They pin the model base URL to the live proxy," >&2
    echo "so a host run could not reach the fake API, and one that did would use the real login. Run this arm on a machine" >&2
    echo "without them, or deliberately with Tomas's own login: CCSBX_C0C_ALLOW_MANAGED=1 spike/c0c.sh host (one real turn is billed)." >&2
    exit 3
  fi
  W=$(mktemp -d); mkdir -p "$W/home/.claude" "$W/work"
  settings "python3 $HERE/c0c-dump.py" > "$W/home/.claude/settings.json"
  echo '{"hasCompletedOnboarding":true,"numStartups":3}' > "$W/home/.claude.json"
  ENVV=()
  if [ "${CCSBX_C0C_ALLOW_MANAGED:-}" != 1 ]; then
    python3 -u "$HERE/c0c-stub.py" 127.0.0.1 $PORT >/dev/null 2>&1 & SP=$!; sleep 1
    ENVV=(ANTHROPIC_BASE_URL=http://127.0.0.1:$PORT ANTHROPIC_AUTH_TOKEN=c0c-placeholder)
  fi
  (cd "$W/work" && env -i HOME="$W/home" PATH=/usr/bin:/bin LANG=C.UTF-8 C0C_OUT="$W/fields.txt" DISABLE_AUTOUPDATER=1 "${ENVV[@]}" \
     "$CLAUDE" -p "$PROMPT" --dangerously-skip-permissions --output-format json >"$W/claude.json" 2>"$W/claude.err")
  [ -n "${SP:-}" ] && kill "$SP" 2>/dev/null
  [ -s "$W/fields.txt" ] || { echo "no payloads recorded"; head -c 600 "$W/claude.err"; exit 1; }
  sort -u "$W/fields.txt" > "$OUT"; echo "wrote $OUT ($(wc -l < "$OUT") rows)"
  ;;
diff) diff <(grep -v "^#" "$HERE/fields-host.txt") <(grep -v "^#" "$HERE/fields-fence.txt") && echo "parity: the host and the fence send the same hook payload fields" ;;
*) echo "unknown arm $1 (--help)" >&2; exit 64 ;;
esac
