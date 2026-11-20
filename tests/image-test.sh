#!/bin/bash
# tests/image-test.sh: twins for the fence image `ccsbx-fence` (plan §3 U1a), for tests/rules.toml.
# It exists so that the image's promises (pinned Claude Code, no credential in any layer, a git shim
# that asks the host instead of writing) are checked against the BUILT image, not against the
# Dockerfile. Direct mode: results are NOT EVIDENCE. Only `docker run --rm`, `docker build` and
# read-only `docker image/save` are used; nothing is removed by name or by filter.
#
#   tests/image-test.sh <rule> <allow|block>
#   rules: image-no-secret image-claude-pin image-git-shim
#   tests/image-test.sh --help
#
# Environment: CCSBX_TEST_IMAGE (default ccsbx-fence:dev; build it with image/build.sh).
set -u
case "${1:-}" in -h|--help|"") sed -n '2,13p' "$0" | sed 's/^# \{0,1\}//'; exit 0;; esac
RULE=$1 TWIN=${2:-}
IMG=${CCSBX_TEST_IMAGE:-ccsbx-fence:dev}
HERE=$(cd "$(dirname "$0")/.." && pwd)
docker image inspect "$IMG" >/dev/null 2>&1 || { echo "FAIL: image $IMG not present (image/build.sh)"; exit 1; }
fail() { echo "FAIL [$RULE/$TWIN]: $*"; exit 1; }
ok()   { echo "ok   [$RULE/$TWIN]: $*"; }
. "$HERE/image/pins.env"

# ---- image-no-secret ------------------------------------------------------------------------
# Patterns of credentials. The Claude Code binary is excluded from the file scan: it carries these
# patterns as data (its own redaction rules), which is not a secret.
SECRET_RE='sk-ant-[A-Za-z0-9_-]{10,}|gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|xox[abprs]-[A-Za-z0-9-]{10,}|AKIA[0-9A-Z]{16}|-----BEGIN [A-Z ]*PRIVATE KEY-----|eyJ[A-Za-z0-9_-]{15,}\.eyJ[A-Za-z0-9_-]{15,}\.'

image_no_secret_allow() {   # the image still works as the fence: this uid, this pin, no extras
  [ "$(docker run --rm "$IMG" id -u)" = 10001 ] || fail "not uid 10001"
  v=$(docker run --rm "$IMG" claude --version); echo "$v" | grep -q "^$CLAUDE_VERSION " || fail "claude --version is not $CLAUDE_VERSION"
  ok "uid 10001, claude $CLAUDE_VERSION"
}
image_no_secret_block() {
  local env hist hits files tmp wh
  env=$(docker image inspect "$IMG" --format '{{range .Config.Env}}{{println .}}{{end}}')
  echo "$env" | grep -E -i '(KEY|TOKEN|SECRET|PASSWORD|CREDENTIAL)=' && fail "a secret-like variable in the image config"
  hist=$(docker history --no-trunc --format '{{.CreatedBy}}' "$IMG")
  echo "$hist" | grep -E -q "$SECRET_RE" && fail "a credential pattern in the build history"
  echo "$hist" | grep -E -i -q '(--build-arg|ARG) *[A-Z_]*(TOKEN|SECRET|PASSWORD|KEY)' && fail "a secret build arg in the history"
  # Every file of the final filesystem (text only; the claude binary excluded).
  hits=$(docker run --rm --user 0 "$IMG" sh -c "grep -rIlE '$SECRET_RE' / --exclude-dir=proc --exclude-dir=sys --exclude-dir=dev --exclude=claude 2>/dev/null" )
  [ -z "$hits" ] || fail "credential patterns in: $hits"
  files=$(docker run --rm --user 0 "$IMG" sh -c "find / -xdev \( -name .netrc -o -name .git-credentials -o -name id_rsa -o -name id_ed25519 -o -name .npmrc -o -name .pypirc -o -name credentials.json -o -name .env -o -name .claude.json \) 2>/dev/null")
  [ -z "$files" ] || fail "credential-like files: $files"
  # A secret added in one layer and deleted in the next is still in the layer: no whiteouts at all.
  tmp=$(mktemp -d); trap 'rm -rf "$tmp"' RETURN
  docker save "$IMG" | tar -x -C "$tmp" 2>/dev/null
  # (docker save is an OCI layout here: layers are blobs without a .tar name, so try every blob.)
  wh=$(find "$tmp" -type f -size +0 ! -name '*.json' ! -name manifest.json ! -name index.json ! -name oci-layout | while read -r l; do tar -tf "$l" 2>/dev/null | grep -E '(^|/)\.wh\.'; done | head -5)
  [ -z "$wh" ] || fail "layers contain deletions (a removed file is still in its layer): $wh"
  ok "no credential in the config, history, files or layers"
}

# ---- image-claude-pin ---------------------------------------------------------------------
image_claude_pin_allow() { v=$(docker run --rm "$IMG" claude --version); echo "$v" | grep -q "^$CLAUDE_VERSION " && ok "claude --version = $CLAUDE_VERSION" || fail "wrong version"; }
image_claude_pin_block() {   # a wrong sha256 must stop the build, not produce an image
  local bad; bad=$(mktemp); trap 'rm -f "$bad"' RETURN
  sed 's/^CLAUDE_SHA256_AMD64=.*/CLAUDE_SHA256_AMD64=0000000000000000000000000000000000000000000000000000000000000000/;s/^CLAUDE_SHA256_ARM64=.*/CLAUDE_SHA256_ARM64=0000000000000000000000000000000000000000000000000000000000000000/' "$HERE/image/pins.env" > "$bad"
  if CCSBX_PINS_FILE=$bad CCSBX_IMAGE_TAG=ccsbx-fence-badpin:test "$HERE/image/build.sh" >/tmp/image-badpin.$$ 2>&1; then rm -f /tmp/image-badpin.$$; fail "the build succeeded with a wrong sha256"; fi
  grep -q "FAILED\|did NOT match\|computed checksums" /tmp/image-badpin.$$ || { cat /tmp/image-badpin.$$ | tail -5; rm -f /tmp/image-badpin.$$; fail "the build failed, but not on the checksum"; }
  rm -f /tmp/image-badpin.$$
  ok "a wrong sha256 fails the build at sha256sum -c"
}

# ---- image-git-shim -----------------------------------------------------------------------
# Runs the shim in a container with a throwaway outbox/inbox; a background loop plays the host.
shim_run() {   # shim_run <git args...>: prints "rc=<n>" and the request file (if any)
  docker run --rm -e CCSBX_GIT_TIMEOUT=5 "$IMG" sh -c '
    mkdir -p /tmp/o/requests /tmp/i/replies; export CCSBX_OUT=/tmp/o CCSBX_IN=/tmp/i
    ( for n in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20; do
        f=$(ls /tmp/o/requests/*.json 2>/dev/null | head -1)
        [ -n "$f" ] && { r=$(basename "$f" .json); echo "{\"status\":\"queued\",\"result\":{\"seq\":1}}" > /tmp/i/replies/$r.json; break; }
        sleep 0.2; done ) &
    git "$@" >/tmp/out.txt 2>/tmp/err.txt; rc=$?; wait
    echo "rc=$rc"; cat /tmp/out.txt /tmp/err.txt; for f in /tmp/o/requests/*.json; do [ -f "$f" ] && cat "$f"; done' sh "$@"
}
image_git_shim_allow() {
  local o
  o=$(shim_run commit -m "hello world" -a)
  echo "$o" | grep -q '^rc=0' || fail "commit did not succeed: $o"
  echo "$o" | grep -q '"type":"git.commit"' && echo "$o" | grep -q '"message":"hello world"' && echo "$o" | grep -q '"all":true' || fail "commit request is wrong: $o"
  o=$(shim_run push origin main)
  echo "$o" | grep -q '"type":"git.push"' && echo "$o" | grep -q '"remote":"origin"' || fail "push request is wrong: $o"
  o=$(docker run --rm "$IMG" sh -c "git --version; git rev-parse --is-inside-work-tree 2>&1 | head -1"); echo "$o" | grep -q "git version" || fail "a read verb did not pass through"
  ok "commit and push become requests with the host's answer; reads pass through"
}
image_git_shim_block() {
  local o
  o=$(shim_run push --force origin main); echo "$o" | grep -q '^rc=1' && ! echo "$o" | grep -q '"type"' || fail "push --force reached the outbox: $o"
  for v in add checkout reset config remote clean rebase "update-ref"; do
    o=$(shim_run $v x); echo "$o" | grep -q '^rc=1' && ! echo "$o" | grep -q '"type"' || fail "git $v was not refused: $o"
  done
  o=$(shim_run commit); echo "$o" | grep -q '^rc=1' || fail "commit without -m was not refused: $o"
  ok "force-push, writes to the repo and message-less commits are refused, nothing reaches the outbox"
}

fn=$(echo "${RULE}_${TWIN}" | tr '-' '_')
type "$fn" >/dev/null 2>&1 || { echo "unknown rule/twin: $RULE $TWIN"; exit 2; }
"$fn"
