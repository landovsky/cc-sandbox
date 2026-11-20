#!/bin/bash
# tests/image-test.sh: twins for the fence image `ccsbx-fence` (plan §3 U1a), for tests/rules.toml.
# It exists so that the image's promises (pinned Claude Code, no credential in any layer, no setuid
# bits, a git shim that asks the host instead of writing) are checked against the BUILT image, not the
# Dockerfile, and so that every block twin is shown to go red: each one first builds a deliberately
# bad image from the good one and requires its checks to fail on it, then requires them to pass on
# the real image. Direct mode: results are NOT EVIDENCE. Only `docker build`, `docker run --rm` and
# read-only `docker image/history/save` are used; nothing is removed, by name or by filter (the bad
# images are untagged and stay as dangling images).
#
#   tests/image-test.sh <rule> <allow|block>
#   rules: image-no-secret image-claude-pin image-git-shim image-hardening
#   tests/image-test.sh --help
#
# Environment: CCSBX_TEST_IMAGE (default ccsbx-fence:dev; build it with image/build.sh).
set -u
case "${1:-}" in -h|--help|"") sed -n '2,16p' "$0" | sed 's/^# \{0,1\}//'; exit 0;; esac
RULE=$1 TWIN=${2:-}
IMG=${CCSBX_TEST_IMAGE:-ccsbx-fence:dev}
HERE=$(cd "$(dirname "$0")/.." && pwd)
docker image inspect "$IMG" >/dev/null 2>&1 || { echo "FAIL: image $IMG not present (image/build.sh)"; exit 1; }
fail() { echo "FAIL [$RULE/$TWIN]: $*"; exit 1; }
ok()   { echo "ok   [$RULE/$TWIN]: $*"; }
# pin NAME: a value from image/pins.env, parsed (the file is data, never sourced).
pin() { sed -n "s/^$1=//p" "$HERE/image/pins.env" | tail -1; }
CLAUDE_VERSION=$(pin CLAUDE_VERSION)
WORK=$(mktemp -d); trap 'rm -rf "$WORK"' EXIT

# build_bad <build args...> < Dockerfile-text: builds an untagged image from a Dockerfile on stdin
# (no build context) in which @IMG@ stands for the image under test; prints the image id.
build_bad() { local id; id=$(sed "s#@IMG@#$IMG#g" | docker build -q "$@" - 2>"$WORK/bb.err") || { cat "$WORK/bb.err" >&2; return 1; }; echo "$id"; }

# must_be_red <what> <image-id> <check-function>: the check must FAIL on the bad image.
must_be_red() {
  local what=$1 bad=$2 fn=$3 out
  [ -n "$bad" ] || fail "could not build the bad image for: $what"
  if out=$($fn "$bad" 2>&1); then fail "the check stayed green on a bad image ($what): the twin cannot go red"; fi
  echo "  red as it must on: $what ($(echo "$out" | head -1))"
}

# ---- image-no-secret ------------------------------------------------------------------------
# Patterns of credentials. The Claude Code binary is left out of the file scans: it carries these
# patterns as data (its own redaction rules), which is not a secret.
SECRET_RE='sk-ant-[A-Za-z0-9_-]{10,}|gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|xox[abprs]-[A-Za-z0-9-]{10,}|AKIA[0-9A-Z]{16}|-----BEGIN [A-Z ]*PRIVATE KEY-----|eyJ[A-Za-z0-9_-]{15,}\.eyJ[A-Za-z0-9_-]{15,}\.'
# The layer scans read binaries too (-a), and a library carries the PEM header as a string constant, so
# there a private key counts only with a base64 body after its header line.
SECRET_RE_BIN='sk-ant-[A-Za-z0-9_-]{10,}|gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|xox[abprs]-[A-Za-z0-9-]{10,}|AKIA[0-9A-Z]{16}|eyJ[A-Za-z0-9_-]{15,}\.eyJ[A-Za-z0-9_-]{15,}\.'
PEM_BODY_RE='-----BEGIN [A-Z ]*PRIVATE KEY-----\r?\n[A-Za-z0-9+/=]{40}'
# Whiteouts that are harmless (apt state in a base layer). Anything else is a deleted file that is
# still in its layer.
WHITEOUT_OK='^(\./)?(var/lib/apt/lists/|var/cache/apt/|var/log/apt/)'

# scan_image <img>: 0 if clean; else prints why and returns 1. Every sub-check fails LOUDLY: a tool
# error is a failure, never "no hits".
scan_image() {
  local img=$1 env hist out rc tmp l n=0 skipped=0 base_ids sum
  env=$(docker image inspect "$img" --format '{{range .Config.Env}}{{println .}}{{end}}') || { echo "inspect failed"; return 1; }
  echo "$env" | grep -Eiq '(KEY|TOKEN|SECRET|PASSWORD|CREDENTIAL|PAT)=' && { echo "a secret-like variable in the image config"; return 1; }
  hist=$(docker history --no-trunc --format '{{.CreatedBy}}' "$img") || { echo "history failed"; return 1; }
  # The final stage has no ARG but three public pins: any other ARG line, or the `|N NAME=value` prefix docker adds to a RUN
  # that saw build args, means a build arg reached this image's history. The allowed ones are SNAPSHOT (a date) and the two
  # python package versions (image/pins.env): they show as `ARG NAME` and one `|3 NAME=... NAME=... NAME=...` prefix.
  echo "$hist" | sed -E 's/(^|[[:space:]])ARG (SNAPSHOT(=[0-9TZ]+)?|PYTHON3_MINIMAL_VERSION(=[0-9A-Za-z.+~-]+)?|LIBPYTHON3_STDLIB_VERSION(=[0-9A-Za-z.+~-]+)?)([[:space:]]|$)/ /; s/^RUN \|3 ((SNAPSHOT=[0-9TZ]+|PYTHON3_MINIMAL_VERSION=[0-9A-Za-z.+~-]+|LIBPYTHON3_STDLIB_VERSION=[0-9A-Za-z.+~-]+) ){3}/RUN /' | grep -Eq '(^|[[:space:]])ARG [A-Za-z_]|^RUN \|[0-9]+ ' && { echo "a build arg in the image history"; return 1; }
  echo "$hist" | grep -Eq "$SECRET_RE" && { echo "a credential pattern in the build history"; return 1; }
  # The final filesystem (text files; the claude binary excluded). rc: 0 hits, 1 none, 2+ error.
  out=$(docker run --rm --user 0 "$img" sh -c "grep -rIlE '$SECRET_RE' / --exclude-dir=proc --exclude-dir=sys --exclude-dir=dev --exclude=claude; echo \"grep-rc=\$?\"") || { echo "the file scan could not run"; return 1; }
  rc=$(echo "$out" | sed -n 's/^grep-rc=//p')
  [ "$rc" = 1 ] || { echo "the file scan found credential patterns or failed (rc=$rc): $(echo "$out" | head -3)"; return 1; }
  out=$(docker run --rm --user 0 "$img" sh -c 'find / -xdev \( -name .netrc -o -name .git-credentials -o -name id_rsa -o -name id_ed25519 -o -name .npmrc -o -name .pypirc -o -name credentials.json -o -name .env -o -name .claude.json \) -print; echo "find-rc=$?"') || { echo "the file-name scan could not run"; return 1; }
  [ "$(echo "$out" | sed -n 's/^find-rc=//p')" = 0 ] && [ "$(echo "$out" | grep -vc '^find-rc=')" = 0 ] || { echo "credential-like files: $(echo "$out" | head -3)"; return 1; }
  # EVERY layer, not only the final filesystem: a secret overwritten later is still in its layer.
  # The Debian base layer is pinned by digest (its content is not ours to judge: a library there carries
  # sample strings such as AWS's documented example key). ONLY the layer whose diff id is the pinned
  # BASE_DIFF_ID is skipped; the image's bottom layer must be exactly that one, or the base changed
  # and the pin must be reviewed. Every layer WE added is scanned.
  base_ids=$(pin BASE_DIFF_ID)
  case "$base_ids" in sha256:[0-9a-f]*) ;; *) echo "BASE_DIFF_ID is not pinned in image/pins.env"; return 1;; esac
  [ "$(docker image inspect "$img" --format '{{index .RootFS.Layers 0}}')" = "$base_ids" ] || { echo "the image's bottom layer is not the pinned base (BASE_DIFF_ID)"; return 1; }
  tmp=$(mktemp -d -p "$WORK"); docker save "$img" 2>/dev/null | tar -x -C "$tmp" || { echo "docker save failed"; return 1; }
  while IFS= read -r l; do
    tar -tf "$l" >/dev/null 2>&1 || continue          # config, manifests: not layers
    sum="sha256:$(gzip -dcf "$l" | sha256sum | cut -d' ' -f1)"  # the diff id is the hash of the UNCOMPRESSED layer
    if echo "$base_ids" | grep -Fxq "$sum"; then skipped=$((skipped + 1)); continue; fi
    n=$((n + 1))
    tar -xOf "$l" --exclude='*usr/local/bin/claude' > "$tmp/content" 2>/dev/null || { echo "could not read layer $l"; return 1; }
    grep -aEq "$SECRET_RE_BIN" "$tmp/content"; rc=$?
    if [ "$rc" = 1 ]; then grep -aPzq -e "$PEM_BODY_RE" "$tmp/content"; rc=$?; fi
    [ "$rc" = 1 ] || { echo "a credential pattern in layer $(basename "$l") (grep rc=$rc; first 8 chars of up to 3 hits: $(grep -aEo "$SECRET_RE_BIN" "$tmp/content" | head -3 | cut -c1-8 | tr '\n' ' '); archive paths: $(tar -tf "$l" | head -2 | tr '\n' ' '))"; return 1; }
    # An exact whiteout check: only names outside the harmless list count.
    tar -tf "$l" 2>/dev/null | grep -E '(^|/)\.wh\.' | grep -Ev "$WHITEOUT_OK" | head -3 > "$tmp/wh"
    [ -s "$tmp/wh" ] && { echo "layer $(basename "$l") deletes files (they are still in an earlier layer): $(head -1 "$tmp/wh")"; return 1; }
  done < <(find "$tmp" -type f -size +0 ! -name '*.json' ! -name content ! -name wh ! -name oci-layout)
  [ "$skipped" -ge 1 ] || { echo "no layer was recognised as the base image's: the base-skip is not working"; return 1; }
  [ "$n" -ge 2 ] || { echo "the layer scan saw $n added layers: it did not run"; return 1; }
  return 0
}

image_no_secret_allow() {
  [ "$(docker run --rm "$IMG" id -u)" = 10001 ] || fail "not uid 10001"
  scan_image "$IMG" || fail "the real image is not clean"
  ok "uid 10001; no credential in the config, history, files or any layer"
}
image_no_secret_block() {
  local id
  # The key is generated inside the build, so the pattern is never literal in the history: only the
  # file and layer scans can find it, not the history check.
  id=$(build_bad <<'EOF'
FROM @IMG@
USER 0
RUN k=$(printf "gh%s%s" p_ "$(printf 'a1B2%.0s' 1 2 3 4 5 6 7 8 9)"); printf %s "$k" > /home/fence/leak
USER 10001
EOF
); must_be_red "a key in a file" "$id" scan_image
  id=$(build_bad <<'EOF'
FROM @IMG@
USER 0
RUN k=$(printf "gh%s%s" p_ "$(printf 'a1B2%.0s' 1 2 3 4 5 6 7 8 9)"); printf %s "$k" > /home/fence/leak
RUN printf clean > /home/fence/leak
USER 10001
EOF
); must_be_red "a key overwritten in a later layer (only the per-layer scan sees it)" "$id" scan_image
  id=$(build_bad <<'EOF'
FROM @IMG@
USER 0
RUN { printf -- '-----BEGIN %s-----\n' 'OPENSSH PRIVATE KEY'; printf 'A%.0s' $(seq 1 64); printf '\n'; } > /home/fence/k
RUN printf clean > /home/fence/k
USER 10001
EOF
); must_be_red "a PEM private key overwritten in a later layer" "$id" scan_image
  id=$(build_bad <<'EOF'
FROM @IMG@
USER 0
RUN echo hi > /home/fence/gone
RUN rm /home/fence/gone
USER 10001
EOF
); must_be_red "a deleted file (whiteout)" "$id" scan_image
  id=$(build_bad --build-arg GH_PAT=canary-value-123 <<'EOF'
FROM @IMG@
ARG GH_PAT
RUN echo "$GH_PAT" > /dev/null
EOF
); must_be_red "a generic secret build arg" "$id" scan_image
  id=$(build_bad <<'EOF'
FROM @IMG@
USER 0
RUN touch /home/fence/.netrc
USER 10001
EOF
); must_be_red "a .netrc" "$id" scan_image
  id=$(build_bad <<'EOF'
FROM @IMG@
ENV API_TOKEN=x
EOF
); must_be_red "an ENV token" "$id" scan_image
  scan_image "$IMG" > "$WORK/s.out" 2>&1 || fail "the real image is not clean: $(cat "$WORK/s.out")"
  ok "every check went red on its bad image, and the real image is clean"
}

# ---- image-claude-pin -----------------------------------------------------------------------
image_claude_pin_allow() { local v; v=$(docker run --rm "$IMG" claude --version); echo "$v" | grep -q "^$CLAUDE_VERSION " && ok "claude --version = $CLAUDE_VERSION" || fail "wrong version: $v"; }
image_claude_pin_block() {   # a wrong sha256 must stop the build, not produce an image
  local bad log; bad=$(mktemp -p "$WORK"); log=$(mktemp -p "$WORK")
  sed 's/^CLAUDE_SHA256_AMD64=.*/CLAUDE_SHA256_AMD64=0000000000000000000000000000000000000000000000000000000000000000/;s/^CLAUDE_SHA256_ARM64=.*/CLAUDE_SHA256_ARM64=0000000000000000000000000000000000000000000000000000000000000000/' "$HERE/image/pins.env" > "$bad"
  if CCSBX_PINS_FILE=$bad CCSBX_IMAGE_TAG=ccsbx-fence-badpin:test "$HERE/image/build.sh" >"$log" 2>&1; then fail "the build succeeded with a wrong sha256"; fi
  grep -q "FAILED\|did NOT match\|computed checksums" "$log" || { tail -5 "$log"; fail "the build failed, but not on the checksum"; }
  ok "a wrong sha256 fails the build at sha256sum -c"
}

# ---- image-git-shim -----------------------------------------------------------------------
# shim_run <image> <git args...>: runs the shim as uid 10001 in a container with a throwaway repo
# (a configured remote "origin", a branch "work", one commit) and a throwaway outbox; a background
# loop plays the host (answers each request). Prints rc=<n>, the shim's output, then REQ:<json> lines.
shim_run() {
  local img=$1; shift
  docker run --rm -e CCSBX_GIT_TIMEOUT=5 -e HOME=/tmp "$img" sh -c '
    R=/usr/lib/git-core/git
    mkdir -p /tmp/o/requests /tmp/i/replies /tmp/w && cd /tmp/w
    $R init -q -b work . && $R remote add origin https://example.invalid/r.git
    echo one > f.txt && $R add f.txt && GIT_AUTHOR_NAME=a GIT_AUTHOR_EMAIL=a@a GIT_COMMITTER_NAME=a GIT_COMMITTER_EMAIL=a@a $R commit -q -m "first commit"
    ln -s . log; mkdir -p /tmp/gd && cp -r .git/. /tmp/gd/
    export CCSBX_OUT=/tmp/o CCSBX_IN=/tmp/i
    ( for n in $(seq 1 40); do
        for f in /tmp/o/requests/*.json; do [ -f "$f" ] || continue; r=$(basename "$f" .json)
          [ -f /tmp/i/replies/$r.json ] || { echo "{\"status\":\"queued\",\"result\":{\"seq\":1}}" > /tmp/i/replies/.$r.tmp; mv /tmp/i/replies/.$r.tmp /tmp/i/replies/$r.json; }
        done; sleep 0.1; done ) &
    git "$@" >/tmp/out.txt 2>&1; rc=$?; sleep 0.3
    echo "rc=$rc"; cat /tmp/out.txt; for f in /tmp/o/requests/*.json; do [ -f "$f" ] && { printf "REQ:"; cat "$f"; }; done; kill %1 2>/dev/null; exit 0' sh "$@" 2>&1
}
# refused <image> <args...>: the shim must exit 1 and put NO request in the outbox.
refused() { local img=$1 o; shift; o=$(shim_run "$img" "$@"); echo "$o" | grep -q '^rc=1$' && ! echo "$o" | grep -q '^REQ:\|REQ:{' || { echo "not refused: git $* -> $(echo "$o" | head -3 | tr '\n' ' ')"; return 1; }; }
# shim_refusals <image>: every case that must be refused.
shim_refusals() {
  local img=$1 v
  refused "$img" push --force origin main || return 1
  refused "$img" push origin +main || return 1          # a force push in a refspec
  refused "$img" push origin :main || return 1          # a delete in a refspec
  refused "$img" push origin :feature || return 1
  refused "$img" push https://evil.example/x.git main || return 1   # a URL as the remote
  refused "$img" push upstream main || return 1          # not a configured remote
  refused "$img" push origin refs/heads/x:refs/heads/main || return 1  # the plan forbids pushing main
  refused "$img" push origin main || return 1            # main as the destination
  refused "$img" push origin work:heads/main || return 1             # git expands it to refs/heads/main
  refused "$img" push origin work:refs/heads//main || return 1       # an empty component
  refused "$img" push origin work:refs/heads/./main || return 1
  refused "$img" push origin work:refs/heads/x/../main || return 1
  refused "$img" push origin work:refs/heads/-x || return 1
  refused "$img" push origin work:refs/tags/v1 || return 1
  refused "$img" push -u origin main || return 1
  refused "$img" push origin ../x || return 1
  refused "$img" -c diff.external=/tmp/x diff || return 1
  refused "$img" -c core.pager=/tmp/x log || return 1
  refused "$img" -c a.textconv=/tmp/x log || return 1
  refused "$img" diff --outpu=/tmp/pwned || return 1       # abbreviated --output
  refused "$img" diff --ou /tmp/pwned || return 1
  refused "$img" push origin work extra || return 1
  refused "$img" push -f || return 1
  refused "$img" -C log commit -m x || return 1          # an option's value taken for the verb
  refused "$img" -c a=b commit -m x || return 1
  for v in add checkout reset config remote clean rebase update-ref tag branch stash; do refused "$img" $v x || return 1; done
  refused "$img" commit || return 1
  refused "$img" commit -m a -m b || return 1
  refused "$img" commit -m x --amend || return 1
  refused "$img" reflog expire --all || return 1
  refused "$img" reflog delete HEAD@{0} || return 1
  refused "$img" log --output=/tmp/pwned || return 1
  refused "$img" diff --output /tmp/pwned || return 1
  refused "$img" grep -O sh x || return 1
  return 0
}
# shim_allowed <image>: the legitimate cases, with their effects checked.
shim_allowed() {
  local img=$1 o msg='a"b\c
line2 -x	tab '$'\x01''end' got
  o=$(shim_run "$img" commit -m 'hello world' -a)
  echo "$o" | grep -q '^rc=0$' || { echo "commit failed: $o"; return 1; }
  echo "$o" | grep -q 'REQ:{"type":"git.commit","message":"hello world","all":true}' || { echo "commit request is wrong: $o"; return 1; }
  # a hostile message must arrive as exactly one JSON line that round-trips
  o=$(shim_run "$img" commit -m "$msg")
  req=$(echo "$o" | sed -n 's/^REQ://p')
  [ "$(echo "$req" | wc -l)" = 1 ] || { echo "the request is not one line: $o"; return 1; }
  got=$(echo "$req" | jq -j .message 2>/dev/null) || { echo "the request is not valid JSON: $req"; return 1; }
  [ "$got" = "$msg" ] || { echo "the message did not round-trip: [$got]"; return 1; }
  o=$(shim_run "$img" push origin main:feature)
  echo "$o" | grep -q 'REQ:{"type":"git.push","remote":"origin","refspec":"main:feature","set_upstream":false}' || { echo "push request is wrong: $o"; return 1; }
  o=$(shim_run "$img" push origin work); echo "$o" | grep -q '"refspec":"work"' || { echo "push work: $o"; return 1; }
  o=$(shim_run "$img" push -u origin work); echo "$o" | grep -q '"set_upstream":true' || { echo "push -u: $o"; return 1; }
  o=$(shim_run "$img" push --set-upstream origin work:refs/heads/feature/x); echo "$o" | grep -q '"set_upstream":true' || { echo "push --set-upstream: $o"; return 1; }
  # reads pass through, with the real output
  o=$(shim_run "$img" log --oneline); echo "$o" | grep -q 'first commit' || { echo "log: $o"; return 1; }
  o=$(shim_run "$img" status --short); echo "$o" | grep -q '^rc=0$' || { echo "status: $o"; return 1; }
  o=$(shim_run "$img" -C /tmp/w log --oneline); echo "$o" | grep -q 'first commit' || { echo "-C dir log: $o"; return 1; }
  o=$(shim_run "$img" --git-dir /tmp/gd log --oneline); echo "$o" | grep -q 'first commit' || { echo "--git-dir log: $o"; return 1; }
  o=$(shim_run "$img" -c core.quotepath=off diff --stat); echo "$o" | grep -q '^rc=0$' || { echo "-c diff: $o"; return 1; }
  o=$(shim_run "$img" show --stat HEAD); echo "$o" | grep -q 'f.txt' || { echo "show: $o"; return 1; }
  o=$(shim_run "$img" reflog); echo "$o" | grep -q '^rc=0$' || { echo "reflog: $o"; return 1; }
  o=$(shim_run "$img" --version); echo "$o" | grep -q 'git version' || { echo "--version: $o"; return 1; }
  return 0
}
image_git_shim_allow() { local o; o=$(shim_allowed "$IMG") || fail "$o"; ok "commit (hostile message round-trips) and push become requests; reads, -C/-c/--git-dir pass through"; }
image_git_shim_block() {
  local id
  # a bad image whose "git" writes a request for anything and says yes
  id=$(build_bad <<'EOF'
FROM @IMG@
USER 0
RUN printf '#!/bin/sh\necho "{\\"type\\":\\"git.push\\"}" > $CCSBX_OUT/requests/x.json\nexit 0\n' > /usr/local/bin/git
USER 10001
EOF
)
  must_be_red "a git that accepts everything" "$id" shim_refusals
  shim_refusals "$IMG" > "$WORK/r.out" 2>&1 || fail "the real shim let something through: $(cat "$WORK/r.out")"
  ok "every refusal (+refspec, :ref, URL remote, main, -C log, writes, --output...) went red on a bad shim and holds on the real one"
}

# ---- image-hardening ------------------------------------------------------------------------
# hardening_check <image>: 0 if the image has no setid file, no sudo, an empty /opt/ccf that the fence
# uid cannot write, the shim as the only git on PATH, and an explicit PATH.
hardening_check() {
  local img=$1 o
  o=$(docker run --rm --user 0 "$img" sh -c 'find / -xdev -type f -perm /6000 -print 2>&1; echo "rc=$?"') || { echo "scan could not run"; return 1; }
  [ "$(echo "$o" | grep -vc '^rc=')" = 0 ] && echo "$o" | grep -q '^rc=0$' || { echo "setuid/setgid files: $(echo "$o" | head -3 | tr '\n' ' ')"; return 1; }
  o=$(docker run --rm "$img" sh -c 'command -v sudo doas su 2>/dev/null | grep -E "sudo|doas"; ls -A /opt/ccf | wc -l; touch /opt/ccf/x 2>&1 && echo WRITABLE; command -v git; ls /usr/bin/git 2>&1; echo "$PATH"')
  echo "$o" | grep -qE 'sudo|doas' && { echo "a privilege tool is installed"; return 1; }
  echo "$o" | grep -q '^0$' || { echo "/opt/ccf is not empty"; return 1; }
  echo "$o" | grep -q WRITABLE && { echo "/opt/ccf is writable by the fence uid"; return 1; }
  echo "$o" | grep -q '^/usr/local/bin/git$' || { echo "git on PATH is not the shim"; return 1; }
  echo "$o" | grep -q 'No such file' || { echo "the real git is still at /usr/bin/git"; return 1; }
  echo "$o" | grep -q '^/usr/local/bin:/usr/bin:/bin$' || { echo "PATH is not set explicitly with the shim first"; return 1; }
  # the client layer (/opt/ccf, plan C3) is stdlib-only Python 3 and the fence has no other interpreter: python3 must be there at the
  # PINNED version (image/pins.env), able to `import json` (python3-minimal alone cannot), and must not be a way to a privilege
  o=$(docker run --rm "$img" sh -c 'python3 --version 2>&1; python3 -c "import json,os,sys,time; print(\"stdlib-ok\")" 2>&1; dpkg-query -W -f="\${Version}\n" python3-minimal libpython3.11-stdlib 2>&1; ls -l /usr/bin/python3 2>&1 | cut -c1-10') || { echo "python check could not run"; return 1; }
  echo "$o" | grep -q '^Python 3\.11\.' || { echo "python3 is not in the image or is not 3.11: $(echo "$o" | head -1)"; return 1; }
  echo "$o" | grep -qx 'stdlib-ok' || { echo "python3 cannot import the stdlib the client layer needs (json, os, sys, time): $(echo "$o" | sed -n 2p)"; return 1; }
  echo "$o" | grep -qx "$(pin PYTHON3_MINIMAL_VERSION)" || { echo "python3-minimal is not the pinned $(pin PYTHON3_MINIMAL_VERSION)"; return 1; }
  echo "$o" | grep -qx "$(pin LIBPYTHON3_STDLIB_VERSION)" || { echo "libpython3.11-stdlib is not the pinned $(pin LIBPYTHON3_STDLIB_VERSION)"; return 1; }
  return 0
}
image_hardening_allow() { local o; o=$(hardening_check "$IMG") || fail "$o"; ok "no setid file, no sudo, empty read-only /opt/ccf, the shim is the only git on an explicit PATH, python3 at its pinned version with the stdlib the client layer needs"; }
image_hardening_block() {
  local id
  id=$(build_bad <<'EOF'
FROM @IMG@
USER 0
RUN chmod u+s /bin/su
USER 10001
EOF
); must_be_red "a setuid su" "$id" hardening_check
  id=$(build_bad <<'EOF'
FROM @IMG@
USER 0
RUN touch /opt/ccf/x
USER 10001
EOF
); must_be_red "a file in /opt/ccf" "$id" hardening_check
  id=$(build_bad <<'EOF'
FROM @IMG@
USER 0
RUN cp /usr/lib/git-core/git /usr/bin/git
USER 10001
EOF
); must_be_red "the real git back on PATH" "$id" hardening_check
  id=$(build_bad <<'EOF'
FROM @IMG@
ENV PATH=/usr/bin:/usr/local/bin
EOF
); must_be_red "a PATH without the shim first" "$id" hardening_check
  id=$(build_bad <<'EOF'
FROM @IMG@
USER 0
RUN rm -f /usr/bin/python3 /usr/bin/python3.11
USER 10001
EOF
); must_be_red "no python3 for the client layer" "$id" hardening_check
  hardening_check "$IMG" > "$WORK/h.out" 2>&1 || fail "the real image failed: $(cat "$WORK/h.out")"
  ok "each hardening check went red on its bad image and the real image passes"
}

fn=$(echo "${RULE}_${TWIN}" | tr '-' '_')
type "$fn" >/dev/null 2>&1 || { echo "unknown rule/twin: $RULE $TWIN"; exit 2; }
"$fn"
