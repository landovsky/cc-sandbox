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

## Gate M, step by step (exact commands)

Run on hp in `~/git/cc-sandbox` unless a step says otherwise. Order matters: the reboot is step 9.

| # | Step | Command |
|---|---|---|
| 1 | Read the install PR and the plan | `gh pr view 2 --web`, then `units/install-linux.sh plan` (as of the morning; this file's copy is from last night) |
| 2 | Push main (builders never push it; it is ahead of origin) | `git -C ~/git/cc-sandbox push origin main` |
| 3 | Merge the install PR, then get its files | `gh pr merge 2 --squash` (or review in the browser), then `git pull --ff-only` in `~/git/cc-sandbox` |
| 4 | P15: decide, then close the rows | `units/p15 status`; decisions are in #4vwb's Outcome; `units/p15 mark ID done\|dropped "note"`; `units/p15 check` must exit 0 |
| 5 | Keep the restart=no containers (table above) | `docker update --restart unless-stopped messaging-hub-db-1` (and any other you want kept) |
| 6 | B9: the hub is native | stream C's release is live and `systemctl --user is-active session-bus-hub.service` says `active` (unit name assumed; `CCSBX_B9_UNIT` overrides). The install refuses otherwise |
| 7 | Build the binaries the installer wants | `ccsbx-root`, `ccsbx-hostd`, `ccsbx-signer`, `ccsbx` for linux/amd64 in `dist/` with a `SHA256SUMS` (`make dist` does not yet produce them: `_GAP:_` stream A) |
| 8 | The root install (sudo, password) | `sudo units/install-linux.sh install --yes` (add `--with-deploy` only if Q12 = yes) |
| 9 | Reboot | `sudo reboot`, then open tmux again |
| 10 | The install is complete | `units/install-linux.sh check` (exit 0) |
| 11 | k3s launcher check 5: hp only, as `ccsbx-keys`, launcher-scoped | the block below, verbatim from the k3s side |
| 12 | The first SHARED go (M8/M1) | merge the k3s PR `_GAP:_ number from the k3s side`: `gh pr merge <n> --repo landovsky/k3s`; then `flux get kustomization cc-sandbox` on the Mac |
| 13 | Everything green | `bin/ccsbx-preflight --morning --post-M1` (set `CCSBX_DOORS_MODEL_URL` and `CCSBX_DOORS_EGRESS_URL` first) |
| 14 | The report | `bin/ccsbx-night-report --dry-run`, then `--send` |

If a step fails: runbook §2 (rollback), and `sudo units/install-linux.sh uninstall --yes` undoes steps 8 to 9.

**Right before step 8, by hand (the installer does not check these; the PR #2 review accepted them as manual steps):**
1. **S5, binaries:** build `dist/` immediately before the install, with no agents running, so nothing
   can swap a file between the checksum check and the copy.
2. **S9, sudoers itself:** `sudo grep -n NOPASSWD /etc/sudoers` once. The installer cleans only
   `/etc/sudoers.d/*`; a NOPASSWD line in `/etc/sudoers` itself would survive.
3. **S14, lockout guard:** `sudo -k; sudo -v` must ask for your password; `sudo -l` must show
   `(ALL : ALL) ALL` without NOPASSWD; keep a **second root shell open** until `check` passes.
4. **S15, the code is the code you read:** `git status` is clean and `git rev-parse HEAD` is the merged
   commit of the install PR.

Step 8 in detail (changed after the PR #2 review):
- the first thing `install` does, once every gate is met, is copy every file it will replace to
  `/var/backups/ccsbx-install/original/`. That backup is made once and never overwritten, so a second
  run, or a run after a failure, cannot spoil it. `uninstall` always restores from it;
- the steps that can fail on this machine (sops, the lxd snap and socket) run BEFORE the sudoers
  files are changed. After the change the whole sudoers config is checked; if it does not parse, the
  files are put back and the install stops;
- if it dies it says `PARTIAL INSTALL` and the undo command. Fix the cause and re-run `install --yes`,
  or run `uninstall --yes`;
- the signer's socket is made by systemd (`ccsbx-signer.socket`); `check` after the reboot connects
  to the signer's and hostd's sockets, so a `FAIL` there is a real finding, not a formality;
- P15's file is read from the owner's home, not root's (`plan` prints the path).
- `install` prints `DONE` only after hostd and the signer are really up (it waits up to 10 s). A
  crash-looping hostd gives `NOT DONE` and the undo hint, not a false DONE;
- known limit after the gate: hostd has no `--ccmsg-bin` (uid `ccsbx` cannot use the owner's bus sockets), so fenced to unfenced-manager messages wait for the hub spoke (B10/B11/M2b);
- the signer will not start without its scope file; the install writes `/var/lib/ccsbx-keys/signer.scope`
  (one line: owners = the owner, machines = `hostname -s`, profiles = the names in `profiles/*.json`, billings =
  `api,subscription,stub`; override with `--machine`, `--profiles`, `--billings`; `plan` prints it). `check` asks the
  signer for a token for one in-scope test claim, so it needs root (the caller must be in group `ccsbx`);
- hostd listens on loopback only (`127.0.0.1:7480` with a token file), so no firewall rule is added and no firewall gate is needed (S11).

### Step 11: launcher check 5 (verbatim from the k3s side, 2026-11-20)

```bash
# k3s launcher check 5 (from K3s manager a09b4c, 2026-11-20 ~00:40). HP ONLY, run by Tomas.
# Proves: ccsbx-keys can decrypt the file; the launcher RBAC and self-renewal work from hp.
# (The admin half, which creates test pods, already passed on the Mac.)
# Our age key is /var/lib/ccsbx-keys/age.key, not sops's default path, hence SOPS_AGE_KEY_FILE.
git -C ~/git/k3s pull --ff-only
install -m 644 ~/git/k3s/apps/cc-sandbox/launcher.kubeconfig.sops.yaml /tmp/ccsbx-launcher.sops.yaml
sudo -u ccsbx-keys bash -c '
  umask 077; cd ~
  SOPS_AGE_KEY_FILE=/var/lib/ccsbx-keys/age.key sops -d /tmp/ccsbx-launcher.sops.yaml > ~/launcher.kubeconfig || exit 1
  K="kubectl --kubeconfig $HOME/launcher.kubeconfig -n cc-sandbox"
  $K auth whoami                                                   # expect system:serviceaccount:cc-sandbox:cc-sandbox-launcher
  echo "create pods: $($K auth can-i create pods)"                 # yes
  echo "exec:        $($K auth can-i create pods --subresource=exec)"  # yes
  echo "secrets:     $($K auth can-i get secrets)"                 # no
  echo "pvc:         $($K auth can-i list persistentvolumeclaims)" # no
  echo "default ns:  $($K auth can-i list pods -n default)"        # no
  echo "doors ns:    $($K auth can-i list pods -n cc-sandbox-doors)"   # no
  $K get secrets 2>&1 | tail -1                                    # expect Forbidden
  $K create token cc-sandbox-launcher --duration=10m --bound-object-kind Secret --bound-object-name cc-sandbox-launcher-anchor >/dev/null && echo "renew: OK"
'
rm /tmp/ccsbx-launcher.sops.yaml
# PASS = whoami is the launcher SA, yes/yes/no/no/no/no, Forbidden, renew OK. Paste the output to the K3s manager (a09b4c), who closes the item.
# Afterwards ~/launcher.kubeconfig stays in ccsbx-keys's home (0600); per the runbook it moves where the daemon reads it, or is deleted.
```

Notes from N1-D (checked on hp, read-only):
- `ccsbx-keys` exists; its home is `/var/lib/ccsbx-keys`, so `~/launcher.kubeconfig` lands at
  `/var/lib/ccsbx-keys/launcher.kubeconfig` (0600 by the `umask 077`). That is inside the key directory, which is where the
  daemon side is meant to read keys from. **The runbook does not name a path for the kubeconfig yet**
  (the last comment line in the block says "per the runbook"; it isn't there): decide the final path when
  hostd/launcher code lands, and fix runbook §12.
- **The directory is mode 0750 today, not 0700** (it was made with `useradd --create-home` in R10b). Nobody else is in group
  `ccsbx-keys`, so tomas can't read it, but the installer's U0b step does `chmod 0700` to close the group bit.
- The installer's U0b step is on PR #2 (`units/lib/u0b.sh`), not on main yet.


## The reboot and the containers with restart policy `no`

The install ends with a reboot. These containers have restart policy `no`, so **they stay down
after it** (`docker ps -a` on 2026-11-20):

| Container | What it is | Recommendation |
|---|---|---|
| `messaging-hub-db-1` | postgres :54329 for messaging-hub dev | `docker update --restart unless-stopped messaging-hub-db-1` before the install |
| `redis-5ctq`, `redis-4s6w`, `redis-644w`, `redis-6y56`, `redis-zl` | per-worktree test redis | drop with their worktrees, or `docker update --restart unless-stopped` |
| `redis-zl-review`, `link-shortener-pg` | already exited | `docker rm` |

After the group change only password sudo can start them (`sudo docker start <name>`).

## Already true on hp (read-only probes tonight)

- `ccsbx-keys` exists and `/var/lib/ccsbx-keys/age.key` is unreadable to tomas (R10b is done).
- `docker-ce` and `containerd.io` are on hold (R17).
- `/usr/local/bin/sops` is owned by tomas, mode 0775: a tomas-writable binary that sudo can run.
- `/etc/sudoers.d/90-tomas-nopasswd` is root-only (0440): its content could not be read tonight.
  The installer parses it as root and refuses if a NOPASSWD line isn't one of docker, apt-get,
  dokku, sops (it won't guess).

## `units/install-linux.sh plan` as of 2026-11-20 (owner tomas, after the signer alignment)

```
install-linux plan  (owner=tomas, bin-dir=<repo>/dist)
Nothing below is run by 'plan'. The same steps run under 'install --yes' as root.
GATES (install refuses while any is unmet)
  UNMET: B9 missing: user unit session-bus-hub.service is not active (the native hub release, stream C)
  UNMET: P15 open: 24 row(s) not done or dropped: A5 B-session-bus B-messaging-hub-db-1 B-redis-5ctq B-redis-4s6w … (run units/p15 status)
  UNMET: binaries missing in <repo>/dist: ccsbx-root ccsbx-hostd ccsbx-signer ccsbx (a linux build of each + SHA256SUMS in --bin-dir; see --help)
  note: sudoers.d/90-tomas-nopasswd not readable without root: verified at install
  note: sudoers.d/README not readable without root: verified at install
  P15 status file: /home/tomas/cc-tree/cc-sandbox/nodes/4vwb/p15-status.tsv
  allowed roots:   "/home/tomas/git", "/home/tomas/wt"
  signer scope:    owners=tomas machines=hp-ubuntu profiles=dev-default,e2e-default billings=api,subscription,stub

[01] U0b users and directories: ccsbx (hostd), ccsbx-keys (keys), group ccsbx-clients
     # ccsbx-keys may already exist (R10b creates it and the age key during the day): reused, never recreated
     # user ccsbx-keys exists
     $ groupadd --system ccsbx
     $ useradd --system --gid ccsbx --home-dir /var/lib/ccsbx --create-home --shell /usr/sbin/nologin ccsbx
     $ groupadd --system ccsbx-clients
     # ccsbx-clients may talk to hostd's local socket; it grants no root path
     $ gpasswd -a tomas ccsbx-clients
     $ chown ccsbx-keys:ccsbx-keys /var/lib/ccsbx-keys
     $ chmod 0700 /var/lib/ccsbx-keys
     $ chown ccsbx:ccsbx /var/lib/ccsbx
     $ chmod 0750 /var/lib/ccsbx
     # the signer's socket is made by systemd (ccsbx-signer.socket): ccsbx-keys:ccsbx 0660 in a 0755 dir,
     # so hostd (group ccsbx) can connect without any access to the key directory

[02] U0 binaries (root-owned, not writable by tomas)
     # MISSING <repo>/dist/ccsbx-root: install will refuse until it is built (a linux build of each + SHA256SUMS in --bin-dir; see --help)
     # MISSING <repo>/dist/ccsbx-hostd: install will refuse until it is built (a linux build of each + SHA256SUMS in --bin-dir; see --help)
     # MISSING <repo>/dist/ccsbx-signer: install will refuse until it is built (a linux build of each + SHA256SUMS in --bin-dir; see --help)
     # MISSING <repo>/dist/ccsbx: install will refuse until it is built (a linux build of each + SHA256SUMS in --bin-dir; see --help)
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
     # the signer will not serve without its scope: ONE keyring line (owners, machines, profiles, billings), written by
     #   'ccsbx token pubkey' as ccsbx-keys, 0600, from the install's own values; rewritten on every run so it matches the options
     $ runuser -u ccsbx-keys -- sh -c f=\$1\;\ shift\;\ umask\ 077\;\ \"\$@\"\ \>\ \"\$f.tmp\"\ \&\&\ mv\ \"\$f.tmp\"\ \"\$f\" sh /var/lib/ccsbx-keys/signer.scope /usr/local/bin/ccsbx token pubkey --key-file /var/lib/ccsbx-keys/machine.key --owners tomas --machines hp-ubuntu --profiles dev-default\,e2e-default --billings api\,subscription\,stub
     # the printed PUBLIC key goes into config/machines/<machine>.json (M9); nothing secret is printed
     # the K8 kubeconfig arrives SOPS-encrypted to the age key; it is decrypted only by ccsbx-keys

[04] U0b signer socket + service (sign(token_claims) only; the socket is activated by systemd)
     write /etc/systemd/system/ccsbx-signer.service  (mode 0644, root:root, 34 lines)
     write /etc/systemd/system/ccsbx-signer.socket  (mode 0644, root:root, 16 lines)
     $ systemctl daemon-reload
     $ systemctl enable --now ccsbx-signer.socket

[05] U0 hostd unit
     write /etc/systemd/system/ccsbx-hostd.service  (mode 0644, root:root, 54 lines)
     # the bearer token for hostd's loopback API: made once (0600, ccsbx), never replaced
     $ runuser -u ccsbx -- sh -c umask\ 077\;\ head\ -c\ 32\ /dev/urandom\ \|\ base64\ \>\ /var/lib/ccsbx/listen.token
     $ systemctl daemon-reload
     $ systemctl enable --now ccsbx-hostd.service
     # hostd listens on 127.0.0.1:7480 only (with a token file): no firewall rule is added (least privilege)

[06] U0 sops: a root-owned binary (tomas-writable + sudo-runnable was a root path)
     $ chown root:root /usr/local/bin/sops
     $ chmod 0755 /usr/local/bin/sops

[07] U0 lxd: remove the accidental snap and its installer socket
     # closes the root path of a missing-command handler turning 'lxc' into an install
     # each is done only if it is there: a missing lxd must not stop the install
     $ snap remove --purge lxd
     $ systemctl disable --now lxd-installer.socket

[08] U0 sudoers: ccsbx-root is the only NOPASSWD grant
     # 1. the new rule goes in a dot-named staging file (sudo ignores it), is checked, then renamed
     write /etc/sudoers.d/.90-ccsbx-root.new  (mode 0440, root:root, 3 lines)
     $ visudo -cf /etc/sudoers.d/.90-ccsbx-root.new
     $ mv -f /etc/sudoers.d/.90-ccsbx-root.new /etc/sudoers.d/90-ccsbx-root
     # 2. remove the NOPASSWD grants for docker, apt, dpkg, snap, dokku, sops from every sudoers.d file
     #    (units/lib/sudoers_strip.py: refuses lines it can't parse, leaves everything else alone)
     # 3. the whole config is checked again; if it does not parse, every file changed is put back
     $ for f in /etc/sudoers.d/*: sudoers_strip.py f > f.new; visudo -cf f.new; mv f.new f
     $ visudo -c   (on failure: restore from the first backup, stop)

[09] U0 groups (LAST): tomas leaves lxd, then docker; effective for new login sessions
     # running containers keep running: the daemon owns them (P14 inventory)
     # github-runner@N units keep docker through SupplementaryGroups=docker in their own unit:
     #   that is NOT changed here; it is a P15 decision (units/p15 status)
     $ gpasswd -d tomas lxd
     $ gpasswd -d tomas docker
     # NOW: sudo reboot  (a fresh tmux server has the new groups; the old one keeps docker)

[10] post-install check: hostd and the signer must be up before DONE
     # waits up to 10 s for ccsbx-hostd and ccsbx-signer.socket to be active and their sockets to exist; otherwise install stops, no DONE

AFTER: reboot, then units/install-linux.sh check  and  bin/ccsbx-preflight --morning --post-M1
UNDO:  as root, units/install-linux.sh uninstall --yes   (restores the first backup, /var/backups/ccsbx-install/original; a failed install is undone the same way)
```
