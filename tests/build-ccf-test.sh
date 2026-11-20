#!/usr/bin/env bash
# tests/build-ccf-test.sh checks image/build-ccf.sh, the gate a client-layer candidate passes before H2 may promote it. It exists because
# the layer comes from another repo and becomes executable code in every fence: the tar must hold exactly the six layer files as plain
# files, match a pinned digest when one is given, carry no credential pattern and no setid file, and the image built from it must hold
# nothing else, root-owned and unwritable for the fence's uid. Synthetic tars, the base image of your choice; nothing is pulled.
#   tests/build-ccf-test.sh [allow|block]    (both by default; --help)
#   CCSBX_TEST_IMAGE   a fence image with python3 (default ccsbx-fence:dev; skips, exit 0, saying so, when it has none)
case "${1:-}" in -h|--help) sed -n 2,8p "$0" | sed 's/^# \{0,1\}//'; exit 0;; esac
set -u
mode=${1:-both}; here=$(cd "$(dirname "$0")/.." && pwd); BASE=${CCSBX_TEST_IMAGE:-ccsbx-fence:dev}
docker image inspect "$BASE" >/dev/null 2>&1 || { echo "skip: image $BASE is not present"; exit 0; }
docker run --rm "$BASE" python3 -c 'import json' >/dev/null 2>&1 || { echo "skip: $BASE has no python3 with json (rebuild it with image/build.sh)"; exit 0; }
W=$(mktemp -d); trap 'rm -rf "$W"' EXIT; fail=0; n=0
mk() { # NAME [extra-file-content-cmd]: a good layer in $W/NAME; the caller edits it, then packs
  d=$W/$1; mkdir -p "$d"
  for f in ccf-hook ccf-mcp ccf-statusline; do printf '#!/usr/bin/python3 -I\nimport sys\nsys.exit(0)\n' > "$d/$f"; chmod 0755 "$d/$f"; done
  printf '# shared\n' > "$d/ccfreq.py"; printf '{}\n' > "$d/managed-settings.json"; printf '{}\n' > "$d/managed-mcp.json"
}
pack() { tar --sort=name --owner=0 --group=0 --numeric-owner --mtime='2026-01-01 00:00:00Z' -C "$W/$1" -cf "$W/$1.tar" $(ls -A "$W/$1"); }
tag() { echo "ccsbx-fence:ccf-test-$$-$1"; }
expect() { # name, want ok|refuse, then the build-ccf.sh args after the tar
  local name=$1 want=$2; shift 2; n=$((n+1))
  case "$mode:$want" in allow:refuse|block:ok) return;; esac
  out=$("$here/image/build-ccf.sh" --ccf-tar "$W/$name.tar" --base "$BASE" --tag "$(tag $name)" "$@" 2>&1); rc=$?
  if [ "$want" = ok ]; then
    [ $rc -eq 0 ] && echo "$out" | grep -q '^candidate ' || { echo "FAIL $name: wanted a candidate, got rc=$rc: $(echo "$out" | tail -2)"; fail=1; return; }
  else
    [ $rc -ne 0 ] || { echo "FAIL $name: a bad layer was built: $out"; fail=1; return; }
    docker image inspect "$(tag $name)" >/dev/null 2>&1 && { echo "FAIL $name: a refused layer left an image behind"; fail=1; return; }
    echo "  red as it must on: $name ($(echo "$out" | tail -1 | cut -c1-110))"
  fi
}
mk good; pack good; expect good ok
mk extra; printf 'x=1\n' > "$W/extra/sitecustomize.py"; pack extra; expect extra refuse
mk missing; rm "$W/missing/ccf-mcp"; pack missing; expect missing refuse
mk link; ln -s ccf-hook "$W/link/ccf-extra"; rm "$W/link/ccf-statusline"; ln -s ccf-hook "$W/link/ccf-statusline"; pack link; expect link refuse
mk secret; printf '#!/usr/bin/python3 -I\n# ghp_%s\n' "$(head -c 40 /dev/zero | tr '\0' a)" > "$W/secret/ccf-hook"; pack secret; expect secret refuse
mk wrongdig; pack wrongdig; expect wrongdig refuse --digest sha256:0000000000000000000000000000000000000000000000000000000000000000
mk gooddig; pack gooddig; expect gooddig ok --digest "sha256:$(sha256sum "$W/gooddig.tar" | cut -d' ' -f1)"
mk nohook; printf '#!/usr/bin/python3 -I\nimport sys\nsys.exit(3)\n' > "$W/nohook/ccf-hook"; pack nohook; expect nohook refuse
for t in good gooddig; do docker rmi "$(tag $t)" >/dev/null 2>&1; done   # the images this test built, by exact tag
[ $fail = 0 ] && echo "ok   build-ccf [$mode]: a layer with an extra file, a link, a missing file, a credential, a wrong pinned digest or a hook that fails is refused (and leaves no image); a good one is built" || exit 1
