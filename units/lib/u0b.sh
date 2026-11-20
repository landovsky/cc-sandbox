# units/lib/u0b.sh — U0b: host keys under the `ccsbx-keys` uid, and the signer (sourced).
# Why: an agent on hp must not be able to read the keys that sign session tokens, so they live under a uid tomas is not.
#
#   plan:  ccsbx-keys + ccsbx uids, the key directory (0700), the machine key, the signer unit and socket.
#   check: the key directory is ccsbx-keys:700, `test -r` on each key fails for the owner, the signer's
#          and hostd's sockets can really be connected to by their clients.
# Mac (`_ccsbx` + a root LaunchDaemon) is install-mac.sh, not here.
# shellcheck shell=bash

KEYS_DIR=/var/lib/ccsbx-keys
LAUNCH_DIR=/var/lib/ccsbx-launch               # ccsbx-launch: the launcher kubeconfig and token, 0700
STATE_DIR=/var/lib/ccsbx
SIGNER_SOCK=/run/ccsbx-signer/sign.sock       # systemd creates it (ccsbx-signer.socket): ccsbx-keys:ccsbx 0660
HOSTD_SOCK=/run/ccsbx-api/hostd.sock          # hostd makes it: ccsbx:ccsbx-clients 0660 (dir 0755)
KEY_FILES="age.key machine.key"

u0b_users() {
  step "U0b users and directories: ccsbx (hostd), ccsbx-keys (signer only), ccsbx-launch (launcher credential), ccsbx-term (no credentials), group ccsbx-clients"
  note "ccsbx-keys may already exist (R10b creates it and the age key during the day): reused, never recreated"
  if ! have_user ccsbx-keys; then
    cmd useradd --system --home-dir "$KEYS_DIR" --create-home --shell /usr/sbin/nologin ccsbx-keys
  else note "user ccsbx-keys exists"; fi
  # the signer's --allow-group default is ccsbx: the group must exist before the signer starts
  have_group ccsbx || cmd groupadd --system ccsbx
  if ! have_user ccsbx; then
    cmd useradd --system --gid ccsbx --home-dir "$STATE_DIR" --create-home --shell /usr/sbin/nologin ccsbx
  else note "user ccsbx exists"; fi
  if ! have_user ccsbx-launch; then
    cmd useradd --system --home-dir "$LAUNCH_DIR" --create-home --shell /usr/sbin/nologin ccsbx-launch
  else note "user ccsbx-launch exists"; fi
  note "ccsbx-launch holds the launcher kubeconfig and token (launcher.kubeconfig, token: 0600) and runs kubectl for the pod verbs; it is the"
  note "  signer's --keys-peer-user. It is not the signer's user (ccsbx-keys, the machine key), not hostd's (ccsbx) and not the owner"
  if ! have_user ccsbx-term; then
    cmd useradd --system --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin ccsbx-term
  else note "user ccsbx-term exists"; fi
  note "ccsbx-term has no credentials, no home, no shell and no group but its own: ccsbx-root attach drops to it to run the output filter"
  have_group ccsbx-clients || cmd groupadd --system ccsbx-clients
  note "ccsbx-clients may talk to hostd's local socket; it grants no root path"
  cmd gpasswd -a "$(real_user)" ccsbx-clients
  cmd chown ccsbx-keys:ccsbx-keys "$KEYS_DIR"
  cmd chmod 0700 "$KEYS_DIR"
  cmd chown ccsbx-launch:ccsbx-launch "$LAUNCH_DIR"
  cmd chmod 0700 "$LAUNCH_DIR"
  cmd chown ccsbx:ccsbx "$STATE_DIR"
  cmd chmod 0750 "$STATE_DIR"
  note "the signer's socket is made by systemd (ccsbx-signer.socket): ccsbx-keys:ccsbx 0660 in a 0755 dir,"
  note "so hostd (group ccsbx) can connect without any access to the key directory"
}

u0b_keys() {
  step "U0b machine key and age key (read only by ccsbx-keys; never by tomas or hostd)"
  note "age key: created here only if R10b didn't (skipped when $KEYS_DIR/age.key exists)"
  if [ "$MODE" != install ] || [ ! -e "$(pth $KEYS_DIR/age.key)" ]; then
    cmd runuser -u ccsbx-keys -- age-keygen -o "$KEYS_DIR/age.key"
  fi
  cmd chmod 0600 "$KEYS_DIR/age.key"
  note "machine key: made by the signer itself (--init), so no other process ever sees it"
  if [ "$MODE" != install ] || [ ! -e "$(pth $KEYS_DIR/machine.key)" ]; then
    cmd runuser -u ccsbx-keys -- "$LIBEXEC/ccsbx-signer" --keys "$KEYS_DIR" --init
  fi
  note "the signer will not serve without its scope (owners, machines, profile:billing pairs). The signer writes it itself"
  note "  (--write-scope: atomic, 0600): only that binary ever opens machine.key. Rewritten on every run to match the options"
  [ -n "$PAIRS" ] || { [ "$MODE" != install ] || die "no profile in the repo lists machine $MACHINE: the signer would have an empty scope"; }
  cmd runuser -u ccsbx-keys -- "$LIBEXEC/ccsbx-signer" --keys "$KEYS_DIR" --write-scope --owners "$(real_user)" --machines "$SCOPE_MACHINES" --pairs "$PAIRS"
  note "the printed PUBLIC key goes into config/machines/<machine>.json (M9); nothing secret is printed"
  note "the K8 kubeconfig arrives SOPS-encrypted to the age key; it is decrypted only by ccsbx-keys"
}

u0b_signer() {
  step "U0b signer socket + service (sign(token_claims) only; the socket is activated by systemd)"
  put /etc/systemd/system/ccsbx-signer.service 0644 root:root < "$UNITS_DIR/systemd/ccsbx-signer.service"
  put /etc/systemd/system/ccsbx-signer.socket 0644 root:root < "$UNITS_DIR/systemd/ccsbx-signer.socket"
  cmd systemctl daemon-reload
  cmd systemctl enable --now ccsbx-signer.socket
}

# sock_check LABEL PATH CLIENT — can CLIENT really connect? Root tests it as CLIENT (runuser); a
# user tests only as themself; otherwise the file mode and group are shown for the reader.
sock_connect_py='import socket,sys
s=socket.socket(socket.AF_UNIX); s.settimeout(3)
try: s.connect(sys.argv[1])
except Exception as e: print(e); sys.exit(1)'
sock_check() {
  local label="$1" path="$2" client="$3" real; real="$(pth "$path")"
  if [ ! -S "$real" ]; then bad "$label: $path is not a socket (is its unit active?)"; return; fi
  if [ "$(id -u)" = 0 ] && [ -z "$PREFIX" ] && have_user "$client"; then
    if runuser -u "$client" -- python3 -I -c "$sock_connect_py" "$real" >/dev/null 2>&1; then ok "$label: $client can connect to $path"
    else bad "$label: $client cannot connect to $path ($(stat -c '%U:%G %a' "$real"))"; fi
  elif [ "$client" = "$(id -un)" ]; then
    if python3 -I -c "$sock_connect_py" "$real" >/dev/null 2>&1; then ok "$label: $client can connect to $path"
    else bad "$label: $client cannot connect to $path ($(stat -c '%U:%G %a' "$real"))"; fi
  else
    note_check "$label: $path is $(stat -c '%U:%G %a' "$real"); run check as root or as $client to connect"
  fi
}
# sign_check — ask the signer for a token for one in-scope test claim and expect {"token":...}: proves the
# scope loads, the group is allowed and the key signs, not merely that the socket accepts a connection.
sign_probe_py='import base64,json,os,socket,sys
claims={"sub":sys.argv[2],"billing":sys.argv[3],"profile":sys.argv[4],"aud":["ccsbx-stub"],"cnf":base64.urlsafe_b64encode(os.urandom(32)).decode().rstrip("=")}   # base64url, no padding, 32 bytes: the signer refuses anything else
s=socket.socket(socket.AF_UNIX); s.settimeout(5)
try:
    s.connect(sys.argv[1]); s.sendall(json.dumps(claims).encode())   # one JSON value; no half-close needed
    r=json.loads(s.makefile().read())
except Exception as e: print(e); sys.exit(1)
if "token" in r: sys.exit(0)
print(r.get("error", r)); sys.exit(1)'
sign_check() {
  local real out sub billing profile; real="$(pth "$SIGNER_SOCK")"
  sub="$(real_user)/$MACHINE/scheck/g1"; profile="${PAIRS%%:*}"; billing="${PAIRS#*:}"; billing="${billing%%[|,]*}"
  if [ ! -S "$real" ]; then bad "signer signs a test claim: $SIGNER_SOCK is not a socket"; return; fi
  if [ "$(id -u)" = 0 ] && [ -z "$PREFIX" ]; then
    out="$(runuser -u ccsbx -- python3 -I -c "$sign_probe_py" "$real" "$sub" "$billing" "$profile" 2>&1)"
  elif [ -n "$PREFIX" ]; then out="$(python3 -I -c "$sign_probe_py" "$real" "$sub" "$billing" "$profile" 2>&1)"
  else note_check "signer signs a test claim: needs root (the caller must be in group ccsbx); run check as root"; return; fi
  if [ $? -eq 0 ]; then ok "signer returned a token for the in-scope test claim ($sub, $billing, $profile)"
  else bad "signer refused or failed the in-scope test claim: $(head -1 <<<"$out")"; fi
}
note_check() { printf 'note  %s\n' "$*"; }

u0b_check() {
  echo "-- U0b host keys"
  expect "user ccsbx-keys exists" have_user ccsbx-keys
  expect "user ccsbx exists" have_user ccsbx
  expect "user ccsbx-launch exists" have_user ccsbx-launch
  expect "user ccsbx-term exists" have_user ccsbx-term
  local uids; uids="$(for u in ccsbx ccsbx-keys ccsbx-launch ccsbx-term "$(real_user)"; do id -u "$u" 2>/dev/null; done | sort | uniq -d)"
  [ -z "$uids" ] && ok "the five users (owner, ccsbx, ccsbx-keys, ccsbx-launch, ccsbx-term) have five different uids" || bad "two of the users share uid $uids"
  local lown; lown="$(stat -c '%U:%a' "$(pth $LAUNCH_DIR)" 2>/dev/null || echo missing)"
  [ "$lown" = "ccsbx-launch:700" ] && ok "$LAUNCH_DIR is ccsbx-launch:700" || bad "$LAUNCH_DIR is $lown (want ccsbx-launch:700)"
  local kf
  for kf in launcher.kubeconfig token; do
    if [ -e "$(pth $LAUNCH_DIR/$kf)" ]; then
      lown="$(stat -c '%U:%a' "$(pth $LAUNCH_DIR/$kf)")"
      [ "$lown" = "ccsbx-launch:600" ] && ok "$LAUNCH_DIR/$kf is ccsbx-launch:600" || bad "$LAUNCH_DIR/$kf is $lown (want ccsbx-launch:600)"
    else note_check "$LAUNCH_DIR/$kf is not there yet (k3s launcher check 5 puts it there)"; fi
  done
  if have_user ccsbx-term && [ -z "$PREFIX" ]; then
    local tg; tg="$(id -nG ccsbx-term)"
    [ "$tg" = "ccsbx-term" ] && ok "ccsbx-term has no group but its own" || bad "ccsbx-term is in groups: $tg"
    [ "$(getent passwd ccsbx-term | cut -d: -f7)" = /usr/sbin/nologin ] && ok "ccsbx-term has no shell" || bad "ccsbx-term has a shell"
  fi
  local hs; hs="$(stat -c '%U:%G %a' "$(pth /etc/ccsbx/hub-admin.secret)" 2>/dev/null || echo missing)"
  [ "$hs" = "root:root 600" ] && ok "/etc/ccsbx/hub-admin.secret is root:root 600" || bad "/etc/ccsbx/hub-admin.secret is $hs (want root:root 600)"
  local own; own="$(stat -c '%U:%a' "$(pth $KEYS_DIR)" 2>/dev/null || echo missing)"
  [ "$own" = "ccsbx-keys:700" ] && ok "$KEYS_DIR is ccsbx-keys:700" || bad "$KEYS_DIR is $own (want ccsbx-keys:700)"
  local k
  for k in $KEY_FILES; do
    if [ "$(id -u)" = 0 ] && [ -z "$PREFIX" ]; then note_check "$k: readability is not tested as root (it always reads); run check as $(real_user)"
    elif [ -r "$(pth $KEYS_DIR/$k)" ]; then bad "$k is readable by $(id -un)"
    else ok "test -r $k fails for $(id -un)"; fi
  done
  expect "ccsbx-signer.socket is active" systemctl is-active --quiet ccsbx-signer.socket
  expect "ccsbx-signer unit installed" test -f "$(pth /etc/systemd/system/ccsbx-signer.service)"
  sock_check "signer socket" "$SIGNER_SOCK" ccsbx
  sock_check "hostd socket" "$HOSTD_SOCK" "$(real_user)"
  sign_check
}

u0b_uninstall() {
  step "U0b undo: stop the signer, remove its units (keys are kept unless --purge-keys)"
  note "the signer's units were stopped and disabled in the U0 undo step, before any unit file was removed"
  note "the unit files are removed or restored in the files step below"
  if [ "${PURGE_KEYS:-0}" = 1 ]; then
    note "--purge-keys: the machine key and age key are DELETED. Rotate the trusted-key list first (M9)."
    try userdel -r ccsbx-keys
  else note "keys directory and the ccsbx-keys user stay (pass --purge-keys to delete them)"; fi
  note "users ccsbx and ccsbx-clients, and /var/lib/ccsbx (the journal), stay: they hold no root path"
}
