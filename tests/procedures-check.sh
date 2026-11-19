#!/usr/bin/env bash
# procedures-check.sh — O7's gate (`make procedures` once P1's Makefile exists): every piece the
# build plan marks "+O7" has a section in docs/runbook.md, and every section has the five fields
# When, Who, Steps, Check, Rollback. A section names the pieces it covers in a
# <!-- procedure: ID ... --> comment under its "## " heading.
# Usage: tests/procedures-check.sh [--help] [PLAN.md [RUNBOOK.md]]     Exit 0 ok, 1 gaps, 2 usage.
# Recipe: add a piece with +O7 to the plan → this goes red until its runbook section exists.
set -euo pipefail
case "${1:-}" in -h|--help) sed -n '2,7p' "$0"; exit 0;; esac
root="$(cd "$(dirname "$0")/.." && pwd)"
plan="${1:-$root/docs/cc-unified-model-build-plan.md}"; rb="${2:-$root/docs/runbook.md}"
[ -f "$plan" ] && [ -f "$rb" ] || { echo "missing $plan or $rb" >&2; exit 2; }
rc=0
# pieces marked +O7: the first cell of any table row that contains +O7
need="$(grep -F '+O7' "$plan" | grep -E '^\| ' | awk -F'|' '{gsub(/^ +| +$/,"",$2); print $2}' | grep -E '^[A-Z]+[0-9]+[a-z]?$' | sort -u)"
[ -n "$need" ] || { echo "no +O7 pieces found in $plan" >&2; exit 2; }
# covered pieces: from the procedure comments
have="$(grep -oE '<!-- procedure: [^>]*-->' "$rb" | sed -E 's/<!-- procedure: //; s/ *-->//' | tr ' ' '\n' | sort -u)"
for p in $need; do
  grep -qxF "$p" <<<"$have" || { echo "MISSING section for +O7 piece $p"; rc=1; }
done
# five fields per section (a "## N. title" heading up to the next one)
awk -v rb="$rb" '
  function flush() { if (title != "") { for (i = 1; i <= 5; i++) if (!(F[i] in seen)) { printf "SECTION \"%s\" lacks **%s:**\n", title, F[i]; bad = 1 } } }
  BEGIN { split("When Who Steps Check Rollback", F, " ") }
  /^## [0-9]+\./ { flush(); title = $0; delete seen; next }
  title != "" { for (i = 1; i <= 5; i++) if (index($0, "**" F[i] ":")) seen[F[i]] = 1 }
  END { flush(); exit bad }
' "$rb" || rc=1
[ "$rc" -eq 0 ] && echo "procedures ok: $(wc -w <<<"$need") +O7 pieces covered, every section has When/Who/Steps/Check/Rollback"
exit "$rc"
