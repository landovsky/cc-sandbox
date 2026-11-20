#!/bin/bash
# tests/xmachine-mac-test.sh: T:xmachine-msg, the real thing: a fenced worker on hp messages a manager on
# the Mac (cc-tree stopped on both), signed, through the hub, and it is typed in the Mac's pane. It exists
# because only two real machines prove the tailnet path and the Mac's daemon; hp alone is covered by
# cmd/ccsbx-hostd's real-hub test (two logical machines on one hub). SKIPPED until the Mac has a daemon:
# it needs CCSBX_MAC_HOST (ssh name of the Mac), a running ccsbx-hostd there on the same hub, and a fenced
# session with a manager node on each side. Direct mode: results are NOT EVIDENCE.
#
#   tests/xmachine-mac-test.sh            prints "skip" and exits 0 until CCSBX_MAC_HOST is set
set -u
case "${1:-}" in -h|--help) sed -n '2,10p' "$0" | sed 's/^# \{0,1\}//'; exit 0;; esac
[ -n "${CCSBX_MAC_HOST:-}" ] || { echo "skip: T:xmachine-msg needs the Mac (set CCSBX_MAC_HOST); hp-only coverage: tests/hub-spoke-test.sh"; exit 0; }
echo "FAIL: T:xmachine-msg against a real Mac is not written yet (the Mac's daemon, M2a, is not here); remove this line when it is" >&2
exit 1
