[Index](README.md)

# Night 1: the morning bundle (for Tomas)

> Written by stream N1-D (cc-sandbox:#34r5). **Nothing here has been run as root.** The install
> scripts are in a PR (security paths: sudoers, a root-owned wrapper, groups, a reboot). Read the
> PR, then the plan output below. Procedures: [runbook](runbook.md) §2 and §3.

## What you do (≈45 min)

1. **Read the install PR** (`units/install-linux.sh`, `units/lib/*`, `units/p15`) and the plan below.
   `units/install-linux.sh plan` prints it fresh and changes nothing.
2. **The install will refuse today. That is intended.** `plan` shows which gates are unmet, as of
   the time it was run (the output below):
   - **B9**: the hub is still the docker container `session-bus`. Stream C builds the native release;
     the unit name is assumed to be `session-bus-hub.service` (override with `CCSBX_B9_UNIT`).
   - **P15**: 24 of 38 rows of the P14 inventory are still open. `units/p15 status` lists them;
     `units/p15 mark ID done|dropped "note"` closes one. The rows that need *your* decision:
     - **A5 the three `github-runner@N` units**: they keep docker access through their own unit even
       after you leave the docker group. Dedicated uid, or move CI to k3s ARC runners?
     - **the reboot stops every container with restart policy `no`** (`messaging-hub-db-1`, five
       `redis-*`): `docker update --restart unless-stopped <name>` before the install, or let them go.
     - **the memory-store backup job** (`docker exec`) breaks silently at the reboot.
   - **binaries**: `ccsbx-root`, `ccsbx-hostd`, `ccsbx-signer`, `ccsbx` are not built yet (stream A).
     The installer wants them in `dist/` with a `SHA256SUMS`.
3. Merge the SHARED PR (M8/M1), `sudo units/install-linux.sh install --yes`, reboot, then
   `units/install-linux.sh check` and `bin/ccsbx-preflight --morning --post-M1`.
4. Rollback: `sudo units/install-linux.sh uninstall --yes` (restores the backup it took).

## Already true on hp (read-only probes tonight)

- `ccsbx-keys` exists and `/var/lib/ccsbx-keys/age.key` is unreadable to tomas (R10b is done).
- `docker-ce` and `containerd.io` are on hold (R17).
- `/usr/local/bin/sops` is owned by tomas, mode 0775: a tomas-writable binary that sudo can run.
- `/etc/sudoers.d/90-tomas-nopasswd` is root-only (0440): its content could not be read tonight.
  The installer parses it as root and refuses if a NOPASSWD line isn't one of docker, apt-get,
  dokku, sops (it won't guess).

## `units/install-linux.sh plan` as of 2026-11-20 00:0x (owner tomas)

```
install-linux plan  (owner=tomas)
Nothing below is run by 'plan'. The same steps run under 'install --yes' as root.
GATES (install refuses while any is unmet)
  UNMET: B9 missing: user unit session-bus-hub.service is not active (the native hub release, stream C)
  UNMET: P15 open: 24 row(s) not done or dropped: A5 B-session-bus B-messaging-hub-db-1 B-redis-5ctq B-redis-4s6w … (run units/p15 status)
  UNMET: binaries missing in <repo>/dist: ccsbx-root ccsbx-hostd ccsbx-signer ccsbx (make build)
  note: sudoers.d/90-tomas-nopasswd not readable without root: verified at install
  note: sudoers.d/README not readable without root: verified at install

[01] U0b users and directories: ccsbx (hostd), ccsbx-keys (keys), group ccsbx-clients
     # ccsbx-keys may already exist (R10b creates it and the age key during the day): reused, never recreated
     # user ccsbx-keys exists
     $ useradd --system --home-dir /var/lib/ccsbx --create-home --shell /usr/sbin/nologin ccsbx
     $ groupadd --system ccsbx-clients
     # ccsbx-clients may talk to hostd's local socket; it grants no root path
     $ gpasswd -a tomas ccsbx-clients
     $ chown ccsbx-keys:ccsbx-keys /var/lib/ccsbx-keys
     $ chmod 0700 /var/lib/ccsbx-keys
     $ chown ccsbx:ccsbx /var/lib/ccsbx
     $ chmod 0750 /var/lib/ccsbx
     # the signer's socket is group ccsbx (hostd's gid), so hostd signs without any key access

[02] U0 binaries (root-owned, not writable by tomas)
     # MISSING <repo>/dist/ccsbx-root: install will refuse until it is built (make build)
     # MISSING <repo>/dist/ccsbx-hostd: install will refuse until it is built (make build)
     # MISSING <repo>/dist/ccsbx-signer: install will refuse until it is built (make build)
     # MISSING <repo>/dist/ccsbx: install will refuse until it is built (make build)
     $ install -d -o root -g root -m 0755 /usr/local/libexec/ccsbx /etc/ccsbx
     $ install -o root -g root -m 0755 <repo>/dist/ccsbx-root /usr/local/sbin/ccsbx-root
     $ install -o root -g root -m 0755 <repo>/dist/ccsbx-hostd /usr/local/libexec/ccsbx/ccsbx-hostd
     $ install -o root -g root -m 0755 <repo>/dist/ccsbx-signer /usr/local/libexec/ccsbx/ccsbx-signer
     $ install -o root -g root -m 0755 <repo>/dist/ccsbx /usr/local/bin/ccsbx
     # ccsbx-root reads its allowed workdir roots from /etc/ccsbx/ccsbx-root.toml (root-owned)
     write /etc/ccsbx/ccsbx-root.toml  (mode 0644, root:root, 3 lines)

[03] U0b machine key and age key (read only by ccsbx-keys; never by tomas or hostd)
     # age key: created here only if R10b didn't (skipped when /var/lib/ccsbx-keys/age.key exists)
     $ runuser -u ccsbx-keys -- age-keygen -o /var/lib/ccsbx-keys/age.key
     $ chmod 0600 /var/lib/ccsbx-keys/age.key
     # machine key: made by the signer itself (--init), so no other process ever sees it
     $ runuser -u ccsbx-keys -- /usr/local/libexec/ccsbx/ccsbx-signer --keys /var/lib/ccsbx-keys --init
     # the printed PUBLIC key goes into config/machines/<machine>.json (M9); nothing secret is printed
     # the K8 kubeconfig arrives SOPS-encrypted to the age key; it is decrypted only by ccsbx-keys

[04] U0b signer service (sign(token_claims) only)
     write /etc/systemd/system/ccsbx-signer.service  (mode 0644, root:root, 35 lines)
     $ systemctl daemon-reload
     $ systemctl enable --now ccsbx-signer.service

[05] U0 hostd unit and firewall
     write /etc/systemd/system/ccsbx-hostd.service  (mode 0644, root:root, 34 lines)
     $ systemctl daemon-reload
     $ systemctl enable --now ccsbx-hostd.service
     # 7480 only on the tailnet interface; the Mac and future-two reach hostd there
     $ ufw allow in on tailscale0 to any port 7480 proto tcp comment ccsbx\ hostd

[06] U0 sudoers: ccsbx-root is the only NOPASSWD grant
     # 1. the new rule goes in a dot-named staging file (sudo ignores it), is checked, then renamed
     write /etc/sudoers.d/.90-ccsbx-root.new  (mode 0440, root:root, 3 lines)
     $ visudo -cf /etc/sudoers.d/.90-ccsbx-root.new
     $ mv -f /etc/sudoers.d/.90-ccsbx-root.new /etc/sudoers.d/90-ccsbx-root
     # 2. remove the NOPASSWD grants for docker, apt-get, dokku, sops from every sudoers.d file
     #    (units/lib/sudoers_strip.py: refuses lines it can't parse, leaves everything else alone)
     $ for f in /etc/sudoers.d/*: sudoers_strip.py f > f.new; visudo -cf f.new; mv f.new f

[07] U0 sops: a root-owned binary (tomas-writable + sudo-runnable was a root path)
     $ chown root:root /usr/local/bin/sops
     $ chmod 0755 /usr/local/bin/sops

[08] U0 lxd: remove the accidental snap and its installer socket
     # closes the root path of a missing-command handler turning 'lxc' into an install
     $ snap remove lxd
     $ systemctl disable --now lxd-installer.socket
     $ gpasswd -d tomas lxd

[09] U0 docker group (LAST): tomas leaves it; effective for new login sessions
     # running containers keep running: the daemon owns them (P14 inventory)
     # github-runner@N units keep docker through SupplementaryGroups=docker in their own unit:
     #   that is NOT changed here; it is a P15 decision (units/p15 status)
     $ gpasswd -d tomas docker
     # NOW: sudo reboot  (a fresh tmux server has the new groups; the old one keeps docker)

[10] record the backup for uninstall
     $ ln -sfn /var/backups/ccsbx-install/20261120-000431 /var/backups/ccsbx-install/latest

AFTER: reboot, then units/install-linux.sh check  and  bin/ccsbx-preflight --morning --post-M1
UNDO:  as root, units/install-linux.sh uninstall --yes   (restores the newest backup under /var/backups/ccsbx-install)
```
