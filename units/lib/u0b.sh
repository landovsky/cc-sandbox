# units/lib/u0b.sh — U0b: host keys under the `ccsbx-keys` uid, and the signer (sourced).
#
#   plan:  ccsbx-keys + ccsbx uids, the key directory (0700), the machine key, the signer unit.
#   check: as tomas: the key directory is ccsbx-keys:700, `test -r` on each key fails, the signer runs.
# Mac (`_ccsbx` + a root LaunchDaemon) is install-mac.sh, not here.
# shellcheck shell=bash

KEYS_DIR=/var/lib/ccsbx-keys
STATE_DIR=/var/lib/ccsbx
LIBEXEC=/usr/local/libexec/ccsbx
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
  note "the signer's socket is group ccsbx (hostd's gid), so hostd signs without any key access"
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
  step "U0b signer service (sign(token_claims) only)"
  put /etc/systemd/system/ccsbx-signer.service 0644 root:root < "$UNITS_DIR/systemd/ccsbx-signer.service"
  cmd systemctl daemon-reload
  cmd systemctl enable --now ccsbx-signer.service
}

u0b_check() {
  echo "-- U0b host keys"
  expect "user ccsbx-keys exists" have_user ccsbx-keys
  expect "user ccsbx exists" have_user ccsbx
  local own; own="$(stat -c '%U:%a' "$(pth $KEYS_DIR)" 2>/dev/null || echo missing)"
  [ "$own" = "ccsbx-keys:700" ] && ok "$KEYS_DIR is ccsbx-keys:700" || bad "$KEYS_DIR is $own (want ccsbx-keys:700)"
  local k
  for k in $KEY_FILES; do
    if [ -r "$(pth $KEYS_DIR/$k)" ]; then bad "$k is readable by $(id -un)"; else ok "test -r $k fails for $(id -un)"; fi
  done
  expect "ccsbx-signer is active" systemctl is-active --quiet ccsbx-signer.service
  expect "ccsbx-signer unit installed" test -f "$(pth /etc/systemd/system/ccsbx-signer.service)"
}

u0b_uninstall() {
  step "U0b undo: stop the signer, remove its unit (keys are kept unless --purge-keys)"
  cmd systemctl disable --now ccsbx-signer.service
  cmd rm -f /etc/systemd/system/ccsbx-signer.service "$LIBEXEC/ccsbx-signer"
  cmd systemctl daemon-reload
  if [ "${PURGE_KEYS:-0}" = 1 ]; then
    note "--purge-keys: the machine key and age key are DELETED. Rotate the trusted-key list first (M9)."
    cmd userdel -r ccsbx-keys
  else note "keys directory and the ccsbx-keys user stay (pass --purge-keys to delete them)"; fi
}
