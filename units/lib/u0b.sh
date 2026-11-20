# units/lib/u0b.sh — U0b: host keys under the `ccsbx-keys` uid, and the signer (sourced).
# Why: an agent on hp must not be able to read the keys that sign session tokens, so they live under a uid tomas is not.
#
#   plan:  ccsbx-keys + ccsbx uids, the key directory (0700), the machine key, the signer unit and socket.
#   check: the key directory is ccsbx-keys:700, `test -r` on each key fails for the owner, the signer's
#          and hostd's sockets can really be connected to by their clients.
# Mac (`_ccsbx` + a root LaunchDaemon) is install-mac.sh, not here.
# shellcheck shell=bash

KEYS_DIR=/var/lib/ccsbx-keys
STATE_DIR=/var/lib/ccsbx
SIGNER_SOCK=/run/ccsbx-signer/sign.sock       # systemd creates it (ccsbx-signer.socket): ccsbx-keys:ccsbx 0660
HOSTD_SOCK=/run/ccsbx/hostd.sock              # hostd makes it: ccsbx:ccsbx-clients 0660
KEY_FILES="age.key machine.key"

u0b_users() {
  step "U0b users and directories: ccsbx (hostd), ccsbx-keys (keys), group ccsbx-clients"
  note "ccsbx-keys may already exist (R10b creates it and the age key during the day): reused, never recreated"
  if ! have_user ccsbx-keys; then
    cmd useradd --system --home-dir "$KEYS_DIR" --create-home --shell /usr/sbin/nologin ccsbx-keys
  else note "user ccsbx-keys exists"; fi
  if ! have_user ccsbx; then
    cmd useradd --system --home-dir "$STATE_DIR" --create-home --shell /usr/sbin/nologin ccsbx
  else note "user ccsbx exists"; fi
  have_group ccsbx-clients || cmd groupadd --system ccsbx-clients
  note "ccsbx-clients may talk to hostd's local socket; it grants no root path"
  cmd gpasswd -a "$(real_user)" ccsbx-clients
  cmd chown ccsbx-keys:ccsbx-keys "$KEYS_DIR"
  cmd chmod 0700 "$KEYS_DIR"
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
note_check() { printf 'note  %s\n' "$*"; }

u0b_check() {
  echo "-- U0b host keys"
  expect "user ccsbx-keys exists" have_user ccsbx-keys
  expect "user ccsbx exists" have_user ccsbx
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
