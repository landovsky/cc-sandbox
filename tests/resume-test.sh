#!/bin/bash
# U7 check (direct mode, NOT EVIDENCE): resume = same sid, gen+1, same home; g1's outbox is closed.
#   tests/resume-test.sh            uses CCSBX_TEST_IMAGE (default python:3.12-slim) and bin/ccsbx
#   tests/resume-test.sh --help
# A transcript is simulated by a file under the fence's home ($HOME/.claude/projects/...): the
# real "/resume lists the old transcript" needs the fence image with Claude Code (U1a pins).
set -u
case "${1:-}" in -h|--help) sed -n '2,6p' "$0" | sed 's/^# \{0,1\}//'; exit 0;; esac
IMG=${CCSBX_TEST_IMAGE:-python:3.12-slim}; BIN=${CCSBX_BIN:-bin/ccsbx}
docker image inspect "$IMG" >/dev/null 2>&1 || { echo "FAIL: image $IMG not present locally (not pulling)"; exit 1; }
mkdir -p /var/tmp/ccsbx-hello
STATE=$(mktemp -d)
# Files the fence wrote (uid 10001) can't be removed by the host user: sweep them from a container.
rmstate() { docker run --rm --user 0 -v "$STATE:/s" "$IMG" sh -c 'rm -rf /s/* /s/.[!.]*' >/dev/null 2>&1; rm -rf "$STATE"; }
trap rmstate EXIT
fail() { echo "FAIL: $*"; exit 1; }
A=("$BIN" --state "$STATE" --machine hp-ubuntu --worktree-root /var/tmp)
spec() { sed "s/lid-hello-1/$1/;s/\"resume\":null/\"resume\":$2/;s/cc-sandbox:hello/cc-sandbox:resume-$$/" tests/specs/hello.json; }
sid=$(spec rid-g1-$$ null | "${A[0]}" start "${A[@]:1}" --launcher none --spec - --json | python3 -c 'import sys,json;print(json.load(sys.stdin)["sid"])') || fail "start"
"$BIN" run "${A[@]:1}" --sid "$sid" --gen 1 --image "$IMG" -- sh -c 'mkdir -p $HOME/.claude/projects/p && echo old-transcript > $HOME/.claude/projects/p/t.jsonl; echo g1 > /run/ccsbx/out/events.jsonl' 2>/dev/null || fail "run g1"
[ -e "$STATE/s/$sid/g1/CLOSED" ] || fail "g1 outbox not closed"
rep=$(spec rid-g2-$$ "{\"sid\":\"$sid\"}" | "$BIN" start "${A[@]:1}" --launcher none --spec - --json) || fail "resume start: $rep"
echo "$rep" | python3 -c "import sys,json;d=json.load(sys.stdin);assert d['sid']=='$sid' and d['gen']==2, d" || fail "reply is not $sid gen 2: $rep"
out=$("$BIN" run "${A[@]:1}" --sid "$sid" --gen 2 --image "$IMG" -- sh -c 'cat $HOME/.claude/projects/p/t.jsonl; echo out-entries=$(ls -A /run/ccsbx/out | wc -l); cat /run/ccsbx/in/session.json' 2>/dev/null) || fail "run g2"
echo "$out" | grep -qx old-transcript || fail "g2 does not see g1's transcript: $out"
echo "$out" | grep -qx 'out-entries=0' || fail "g2's outbox is not empty: $out"
echo "$out" | grep -q '"gen": 2' || fail "in/session.json does not say gen 2: $out"
[ "$(cat "$STATE/s/$sid/g1/out/events.jsonl")" = g1 ] || fail "g1's outbox content changed"
echo "ok   resume: same sid $sid, gen 2, old transcript visible, g1 closed with its outbox intact, g2 outbox empty"
