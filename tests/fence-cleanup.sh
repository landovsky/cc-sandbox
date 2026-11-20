# tests/fence-cleanup.sh is the cleanup that tests/fence-test.sh runs on exit, in a file of its own so
# tests/fence-cleanup-test.sh can run it against a stub docker. It exists because a cleanup that removes
# docker objects must never act on an empty or foreign value: it removes only the exact names this
# run created, and refuses before ANY docker call if its inputs look wrong. Source it; it runs nothing.
#
# Needs: STATE (the mktemp -d this run created), IMG, PIDS (array), $$; $STATE/sids lists this run's sids.

TRASH_RE='^/var/tmp/ccsbx-cc-[A-Za-z0-9]{6}$' # the exact shape of a scratch dir a test made: a glob's ? would also match / and ..
SID_RE='^s[0-9a-hjkmnp-tv-z]{10}$' # what mintSid in internal/start makes: "s" + 10 base32 characters

# cleanup_guard prints why and returns 1 if the cleanup must not touch docker.
cleanup_guard() {
  [ -n "${$:-}" ] || { echo "cleanup refused: empty pid" >&2; return 1; }
  [ -n "${STATE:-}" ] || { echo "cleanup refused: STATE is empty" >&2; return 1; }
  case "$STATE" in
    "${TMPDIR:-/tmp}"/tmp.??????*) ;;
    *) echo "cleanup refused: STATE=$STATE is not a mktemp path of this run" >&2; return 1;;
  esac
  [ -d "$STATE" ] || { echo "cleanup refused: STATE=$STATE does not exist" >&2; return 1; }
  local s x
  while IFS= read -r s; do
    [[ "$s" =~ $SID_RE ]] || { echo "cleanup refused: bad sid line '$s'" >&2; return 1; }
  done < <(cat "$STATE/sids" 2>/dev/null)
  while IFS= read -r s; do
    [[ "$s" =~ $TRASH_RE ]] || { echo "cleanup refused: bad trash line '$s'" >&2; return 1; }
  done < <(cat "$STATE/trash" 2>/dev/null)
  for x in ${CLEANUP_EXTRA:-}; do
    [[ "$x" =~ ^ccsbx-(fwd|canary)-[0-9]+$ ]] || { echo "cleanup refused: bad extra container '$x'" >&2; return 1; }
  done
  return 0
}

# Files the fence wrote (uid 10001) can't be removed by the host user: sweep them from a container.
rmstate() {
  # Dirs a test made outside STATE (listed in $STATE/trash): only /var/tmp/ccsbx-cc-<6 chars>, nothing else.
  local d
  for d in $(cat "$STATE/trash" 2>/dev/null); do
    if [[ "$d" =~ $TRASH_RE ]]; then docker run --rm --user 0 -v "$d:/s" "$IMG" sh -c 'rm -rf /s/* /s/.[!.]*' >/dev/null 2>&1; rm -rf "$d"; fi
  done
  docker run --rm --user 0 -v "$STATE:/s" "$IMG" sh -c 'rm -rf /s/* /s/.[!.]*' >/dev/null 2>&1; rm -rf "$STATE"
}

cleanup() {
  cleanup_guard || return 1
  # CLEANUP_EXTRA: containers only the calling test creates (fence-test: its forwarder and canary), first:
  # a connected peer blocks the network removal. gc-test and resume-test set none.
  local x
  for x in ${CLEANUP_EXTRA:-}; do docker rm -f "$x" >/dev/null 2>&1; done
  for p in "${PIDS[@]:-}"; do [ -n "$p" ] && kill "$p" 2>/dev/null; done
  wait 2>/dev/null   # let ccsbx run finish its own cleanup before we sweep
  for s in $(cat "$STATE/sids" 2>/dev/null); do
    docker rm -f "ccsbx-$s-g1" >/dev/null 2>&1
    docker network rm "ccsbx-net-$s-g1" >/dev/null 2>&1
    docker network inspect "ccsbx-net-$s-g1" >/dev/null 2>&1 && echo "WARN: network ccsbx-net-$s-g1 left behind" >&2
  done
  rmstate
}
