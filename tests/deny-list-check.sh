#!/usr/bin/env bash
# deny-list-check.sh — print every rule in .claude/settings.json permissions.deny, grouped by tool,
# and fail if a required category has no rule. Deny rules are speed bumps, not the fence (invariant 3).
# Usage: tests/deny-list-check.sh [--help] [settings.json]   (default: <repo>/.claude/settings.json)
# Recipe: probe for real with
#   claude -p --permission-mode bypassPermissions 'run: sudo -n true'   (expect: denied)
set -euo pipefail
case "${1:-}" in -h|--help) sed -n '2,6p' "$0"; exit 0;; esac
f="${1:-$(cd "$(dirname "$0")/.." && pwd)/.claude/settings.json}"
command -v jq >/dev/null || { echo "jq required" >&2; exit 2; }
jq -e '.permissions.deny|type=="array" and length>0' "$f" >/dev/null || { echo "FAIL: no permissions.deny in $f" >&2; exit 1; }
jq -r '.permissions.deny[]' "$f" | sort | awk '{print}'
echo "--- $(jq '.permissions.deny|length' "$f") rules in $f"
rc=0
need() { jq -e --arg r "$1" '.permissions.deny|index($r)' "$f" >/dev/null || { echo "MISSING: $1"; rc=1; }; }
for r in 'Bash(sudo *)' 'Bash(su *)' 'Bash(lxc *)' 'Bash(snap *)' 'Bash(apt *)' 'Bash(apt-get *)' 'Bash(dpkg *)' 'Bash(kubectl *)' 'Bash(helm *)' 'Bash(flux *)' 'Bash(pmset *)' \
  'Bash(systemctl restart*)' 'Bash(systemctl --user restart*)' 'Bash(git push --force*)' 'Bash(git push * main)' 'Bash(git push * master)' 'Bash(sb-deploy-hub)' 'Bash(cc-tree arm *)' \
  'Bash(wget *)' 'Bash(curl *https://*)' 'Bash(docker run *--privileged*)' 'Read(~/.ssh/**)' 'Read(~/.config/gh/**)' 'Read(~/.aws/**)' 'Read(~/.kube/**)' 'Read(**/.env)' 'Read(~/.claude/.credentials.json)'; do need "$r"; done
# the allowed exception must stay open
jq -e '.permissions.deny|map(select(test("^Bash\\(systemctl (--user )?status")))|length==0' "$f" >/dev/null || { echo "FAIL: systemctl status is denied"; rc=1; }
exit $rc
