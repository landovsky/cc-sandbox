[Index](README.md)

# Night 1: the morning bundle (for Tomas)

> **The Gate M sequence to follow is now [gate-m.md](gate-m.md)** (one sitting, copy and paste, expected output per step, new digests). This page is kept as the background and reference for each installer step; where the two differ, gate-m.md wins.

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

Run on hp in `~/git/cc-sandbox` unless a step says otherwise. Order matters: the reboot is step 9. **Gate M is
2026-11-22.**

**Prerequisite: U1c (the helper-mode client, built by stream A) must be merged first.** Until it lands nothing
on hp can start a fence after the gate: hostd and `ccsbx start` only know `--root direct`, which needs the docker
group this install removes. Merge U1c before step 8, or accept that window explicitly and make U1c the first
night-2 piece.

| # | Step | Command |
|---|---|---|
| 0 | **The fence image digest** (the install needs it; Tomas has pushed `main`, so G's Actions workflow `fence-image` has built the image: U1a/R19) | in `~/git/cc-sandbox`, in the shell you will use for steps 8 and 8b: `RUN=$(gh run list --repo landovsky/cc-sandbox --workflow fence-image.yml --branch main --status success --limit 1 --json databaseId -q '.[0].databaseId')`; `gh run download "$RUN" --repo landovsky/cc-sandbox --name digest --dir /tmp/fence-digest`; `DIGEST=$(cat /tmp/fence-digest/digest.txt)`; `[[ "$DIGEST" =~ ^ghcr\.io/landovsky/ccsbx-fence@sha256:[0-9a-f]{64}$ ]] && echo "digest ok: $DIGEST"`. If the run is missing or red there is no digest and the install refuses. The workflow file (`fence-image.yml`), the artifact (`digest`) and the file (`digest.txt`) are confirmed by G; the job summary of the same run also shows the digest **The earlier fence digest (`ghcr.io/landovsky/ccsbx-fence@sha256:c815f83c…`, built from main `2945647`) is STALE: it is to be re-pinned after the push of main `0883a50`** (the CCF-IN layer changes the fence image). Do NOT pass it: run the commands above after that push and use the new run's `digest.txt`; write the new value here once it is known. |
| 0b | **The sidecar image digest** (the install needs it too: `k8s_side_image`; the `side-image` workflow, #3wsr, builds it from the same push of `main`) | same shell as step 0: `RUN=$(gh run list --repo landovsky/cc-sandbox --workflow side-image.yml --branch main --status success --limit 1 --json databaseId -q '.[0].databaseId')`; `gh run download "$RUN" --repo landovsky/cc-sandbox --name digest --dir /tmp/side-digest`; `SIDE=$(cat /tmp/side-digest/digest.txt)`; `[[ "$SIDE" =~ ^ghcr\.io/landovsky/ccsbx@sha256:[0-9a-f]{64}$ ]] && echo "side digest ok: $SIDE"` | the same refusal as step 0: no run or a red run means no digest, and the install refuses (`--side-image` is required, a tag is refused). Also give `$SIDE` to the k3s manifests (the manager does) **The earlier sidecar digest (`ghcr.io/landovsky/ccsbx@sha256:8806715a…`, built from main `2945647`) is STALE: to be re-pinned after the next push of main** (the sidecar is the `ccsbx` binary, so it changes with every code merge). Do NOT pass it: use the new run's `digest.txt`; write the new value here once it is known. |
| 1 | Read the install PR and the plan | `gh pr view 2 --web`, then run `plan` **from the PR branch** (`git switch n1-d-root`, then back to `main`) or after step 3: on `main` the `units/` directory is empty until the PR is merged. This file's copy is from last night |
| 2 | Push main (builders never push it; it is ahead of origin) | `git -C ~/git/cc-sandbox push origin main` |
| 3 | Merge the install PR, then get its files | `gh pr merge 2 --squash` (or review in the browser), then `git pull --ff-only` in `~/git/cc-sandbox` |
| 4 | P15: decide, then close the rows | `units/p15 status`; decisions are in #4vwb's Outcome; `units/p15 mark ID done\|dropped "note"`; `units/p15 check` must exit 0 |
| 5 | Keep the restart=no containers (table above) | `docker update --restart unless-stopped messaging-hub-db-1` (and any other you want kept) |
| 6 | The hub is NOT a Gate M dependency (decision 2026-11-21) | nothing to do. The hub on hp is still the docker container (uid 0 behind docker-proxy); moving it to a native release is part of the dotfiles deploy gate (Tomas ❓ 2xq4), not of this install. The install is run WITHOUT `--hub-url`: B9 (native hub + listener rule) is skipped with a note, no hub secret is read or written, and `hub_url`, `hub_user`, `hub_secret` stay unset in `ccsbx-root.toml`, so relay-mint and pod relays refuse cleanly (the sidecar starts without a hub url; its bus answers `relay_unavailable`). Consequence you accept: after the reboot you are out of the docker group, so the hub container keeps running but you manage it with `sudo docker` (P15 answers). With `--hub-url` (at the deploy gate) the full B9 gate returns: the native user unit active, and `units/lib/hub_listen.py` (ccsbx-root's rule) checks the url against the live listeners. **MERGE ORDER: the installer writes new `ccsbx-root.toml` keys, so merge it only together with, or after, A's `ccsbx-root` change** |
| 7 | Build the binaries the installer wants | `make dist-linux GO=$HOME/.local/go/bin/go` builds `ccsbx-root`, `ccsbx-hostd`, `ccsbx-signer`, `ccsbx` for linux/amd64 into `dist/` with a `SHA256SUMS` that lists exactly them. Compare the checksums with a build you trust (CI or a second machine) before step 8: the file sits next to the binaries |
| 7b | Root-only test of `ccsbx-root`'s bind mount (**PR #8 must be merged first**) | from `~/git/cc-sandbox`: `~/.local/go/bin/go version` first (go is not on sudo's PATH, so the full path is used and `GOTOOLCHAIN=local` stops it fetching a toolchain), then `sudo unshare -m env GOTOOLCHAIN=local /home/tomas/.local/go/bin/go test -tags rootmount -run TestRealMounter ./internal/rootcmd`. It must **PASS**; if it does not, stop here |
| 8 | The root install (sudo, password) | `sudo units/install-linux.sh install --yes --image "$DIGEST" --side-image "$SIDE"` (`$DIGEST` is the value from step 0 and `$SIDE` the one from step 0b, written to `ccsbx-root.toml` as `k8s_side_image`; `--hub-url` and `--hub-secret-from` are NOT passed: the install does not depend on the hub (step 6); `sudo` keeps your shell's expansion of `$DIGEST`). The install makes three more users: `ccsbx-launch` (holds the launcher kubeconfig and token under `/var/lib/ccsbx-launch`, 0700; the signer's keys peer for `future-two`) and `ccsbx-term` (no credentials, no home, no shell; runs the attach output filter), while `ccsbx-keys` stays the signer's alone. Without `--hub-url` no hub secret is written; at the hub's deploy gate the install is re-run with `--hub-url` and copies the FIRST LINE of `~/.config/session-bus/secret` to `/etc/ccsbx/hub-admin.secret` (root 0600, never printed). `--revoke-doors` is NOT passed at Gate M: there are no doors before M1, so `revoke_doors` stays empty and hostd's `--revoke-door` is `""` (nothing is revoked; pod tokens expire by themselves). Pass it when the doors exist and re-run the install. The sudoers rule is unchanged: the pod verbs are ccsbx-root verbs, covered by the same one rule (add `--with-deploy` only if Q12 = yes). **The image is passed by digest**: the CI-built candidate from U1a; a tag is refused, because hostd and `ccsbx-root` refuse every start of a tag (`image_not_pinned`). A digest not in `pins.toml` is a candidate: only e2e profiles may run it, which is what step 10b uses |
| 8c | MOVED to the hub's deploy gate (below): the hub secret check needs the native hub | see "The hub's deploy gate" | not a Gate M step |
| 8b | Seccomp profile for `ccsbx-root run` (**before the reboot**: it needs `/etc/ccsbx` from step 8 and docker, which the old session still has) | from `~/git/cc-sandbox`, in this order: `V=$(docker version --format '{{.Server.Version}}')`; `curl -fsSL -o default.json https://raw.githubusercontent.com/moby/moby/v$V/profiles/seccomp/default.json`; `ccsbx seccomp-profile --base default.json --out /tmp/seccomp.json`; `sudo install -o root -g root -m 0644 /tmp/seccomp.json /etc/ccsbx/seccomp.json`; `ccsbx seccomp-profile --verify /etc/ccsbx/seccomp.json`; `tests/seccomp-setuid-test.sh --base default.json --image ccsbx-fence:dev` (docker; the candidate image, since there is no production pin yet). The verify and the docker test must both pass. Last, still before the reboot (docker is gone after it): `docker pull "$DIGEST"`, so step 10b's pane can really run the fence. **If the pull fails** with `unauthorized` or `denied`: the ghcr package is private by default. Either make it public (the repo's Packages → `ccsbx-fence` → Package settings → Change visibility → Public; the image holds no secrets), or run `docker login ghcr.io -u <your github user>` with a token that has `read:packages`, then pull again. Do not reboot before the pull works `check` verifies the profile again |
| 9 | Reboot | `sudo reboot`, then open tmux again |
| 10 | The install is complete | `units/install-linux.sh check` (exit 0) |
| 10b | **One e2e start reaches `ccsbx-root run`** (the Gate M ACCEPT for the start path; U1c is merged) | from `~/git/cc-sandbox`: `ccsbx start --spec tests/specs/e2e-hello.json --json`. **Step 1 of the proof, hostd accepts:** exit 0 and a StartReply with a `sid`. Exit 2 prints `ccsbx start: refused: <code>` with one of `image_not_pinned` (the image is a tag, or hostd's `--image` is wrong), `profile_not_approved`, `wrong_machine` (the machine name differs between the spec, hostd and `ccsbx-root.toml`), `bad_spec` (for example a workdir outside `--worktree-root`), `rate_capped`, `machine_full`, `already_running`; or `ccsbx start: ccsbx-hostd is not running or not reachable (socket /run/ccsbx-api/hostd.sock)`. **Step 2, ccsbx-root accepts:** `sudo ccsbx-root attach <sid>` shows the pane. **PASS** = the pane has no line starting `ccsbx-root run: refused:`; it runs the fence, or ends with docker's own error (`ccsbx-root run: ...` without `refused:`, exit 1), which means every check passed. **FAIL** = `ccsbx-root run: refused: ` followed by `pins: ...` (the image is not an acceptable digest), `profile <p> is not approved for machine <m> with billing <b>`, `session <sid> is for machine "<a>", this is "<b>"`, `this process is not in the host's mount namespace` (a sandboxed unit: `ccsbx-tmux` must be the host's), `seccomp profile ...` (step 8b), `session <sid> has ended`, or `must run as root` |
| 11 | k3s launcher check 5: hp only, as `ccsbx-keys`, launcher-scoped | the block below, verbatim from the k3s side. **Then hand the kubeconfig to the launch user** (see "Notes", round 3): the block leaves it in `ccsbx-keys`'s home |
| 11b | **Paste the tailnet ACL** (Tomas, admin console; before step 12, because the operator needs the tag to create proxy devices) | k3s PR https://github.com/landovsky/k3s/pull/21 (head 4f3bde0, SHARED). On the Mac: (1) merge it, then `cd ~/git/k3s && git pull`; (2) `scripts/tailscale-acl-test lint`; (3) `scripts/tailscale-acl-test baseline save`; (4) open https://login.tailscale.com/admin/acls, paste the whole `artifacts/tailscale-config.json` (with the TODO Tomas A7 parts on the first A7 paste), Save; (5) `scripts/tailscale-acl-test compare`, expect no new FAILs (C2 is now an expected deny); (6) report the compare output to devops:#6c44 (the Gate M paste is only `tag:ccsbx` → hp-ubuntu tcp:4011 and hp/Mac → `tag:ccsbx` tcp:443; the `tag:ccsbx-hostd` :7480 grant comes with F4, Night 3) | `bin/ccsbx-preflight --morning` C-R4 passes (`tag:ccsbx` in `tailscale status`, once the proxies exist after step 12) |
| 12 | The first SHARED go (M8/M1) | merge the k3s PR `_GAP:_ number from the k3s side`: `gh pr merge <n> --repo landovsky/k3s`; then `flux get kustomization cc-sandbox` on the Mac |
| 13 | Everything green | `bin/ccsbx-preflight --morning --post-M1` (set `CCSBX_DOORS_MODEL_URL` and `CCSBX_DOORS_EGRESS_URL` first) |
| 14 | The report | `bin/ccsbx-night-report --dry-run`, then `--send` |

If a step fails: runbook §2 (rollback), and `sudo units/install-linux.sh uninstall --yes` undoes steps 8 to 9 (the seccomp file in step 8b stays: it is harmless).

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
- the signer will not start without its scope file; the install has `ccsbx-signer --write-scope` write
  `/var/lib/ccsbx-keys/signer.scope` (owners = the owner, machines = `hostname -s`, **pairs** built from
  `profiles/*.json`: each profile's own billings, today `dev-default:api|stub,e2e-default:stub`, so no
  `subscription` until a profile allows it; override with `--machine` and `--pairs`; `plan` prints it).
  Only the signer opens `machine.key`. `check` asks the signer, as uid `ccsbx`, for a token for one in-scope
  test claim (api + dev-default), so it needs root;
- `ccsbx-root` runs with a clean environment: the sudoers file has `Defaults!/usr/local/sbin/ccsbx-root env_reset,!setenv`, so `sudo -E` and `VAR=x` do not reach it;
- the ccsbx tmux server is its own unit, `ccsbx-tmux.service` (uid `ccsbx`, no sandbox, host mount namespace), started before hostd; hostd only connects to `/run/ccsbx/tmux.sock`. `check` (as root) confirms the namespace and that the socket is `ccsbx:600`;
- `install` prints DONE only after the signer has signed a test claim, not only after its socket exists;
- helper mode: the install copies `profiles/*.json` to `/etc/ccsbx/profiles/` and `pins.toml` to `/etc/ccsbx/pins.toml`
  (root:root 0644, dir 0755). hostd (`--profiles`, `--pins`) and the signer's scope use THESE copies, never the
  owner-writable repo; `pins.toml` is empty today, so only e2e candidate images run. hostd also gets `--run-dir
  /var/lib/ccsbx-run` (made here, root 0755), `--worktree-root` and `--image ccsbx-fence:dev`. The default
  allowed roots stay `~/git,~/wt`: `ccsbx-root.toml`'s `allowed_roots` and hostd's `--worktree-root` are one value.
  hostd gets exactly that list (`--worktree-root /home/tomas/git,/home/tomas/wt`): one bad or empty entry stops every
  start, so the installer refuses empty entries, trailing slashes and `~`;
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
- **Round 3 (three users): after the block, move the kubeconfig to its final place.** `ccsbx-keys` is the signer's alone, so the
  launcher kubeconfig belongs to `ccsbx-launch`: `sudo install -m 0600 -o ccsbx-launch -g ccsbx-launch /var/lib/ccsbx-keys/launcher.kubeconfig /var/lib/ccsbx-launch/launcher.kubeconfig`,
  then `sudo rm /var/lib/ccsbx-keys/launcher.kubeconfig`. That is the path `ccsbx-root.toml` names (`kubeconfig`). `check` reports the
  file's owner and mode (ccsbx-launch:600), and says "not there yet" before this step. The launcher token file (`/var/lib/ccsbx-launch/token`, 0600) is
  checked the same way. The signer has a second socket for the launcher, `/run/ccsbx-signer/launch.sock` (`ccsbx-keys:ccsbx-launch 0660`, unit `ccsbx-signer-launch.socket`; the main socket stays hostd's, `ccsbx-keys:ccsbx`); the signer unit passes `--keys-peer-socket /run/ccsbx-signer/launch.sock`, and `ccsbx-root.toml` has `signer_socket` for it and `hub_user` (the owner, for the hub listener check). `check` connects to it as `ccsbx-launch`.
- **Not an installer step (dotfiles deploy gate, Tomas ❓ 2xq4):** the hub unit gets `SB_NODE_CLAIMS_FILE=$HOME/.local/state/session-bus/node-claims/claims.json`.
  The hub creates that directory itself, mode 0700, so the node claims survive a hub restart. The hub unit lives in dotfiles: do it with that deploy, and do not read or print the unit.
- The installer's U0b step is on PR #2 (`units/lib/u0b.sh`), not on main yet.


## The hub's deploy gate (later, Tomas ❓ 2xq4; NOT Gate M)

The hub on hp becomes a native release under the owner's user unit in the dotfiles deploy. These steps belong to that gate, after it:
1. The hub unit gets `SB_NODE_CLAIMS_FILE=$HOME/.local/state/session-bus/node-claims/claims.json` (the hub creates the directory, 0700). Do not read or print the hub unit.
2. Re-run the install WITH the hub: `sudo units/install-linux.sh install --yes --image "$DIGEST" --side-image "$SIDE" --hub-url http://100.101.219.96:4011` (re-read the url from `ss -ltnH 'sport = :4011'`; add `--hub-secret-from FILE` only if the secret is not in `~/.config/session-bus/secret`). It now enforces B9 and the listener rule, writes `hub_url`, `hub_user` and `hub_secret`, and copies the first line of the secret to `/etc/ccsbx/hub-admin.secret`. The other steps are idempotent. (If SB_SECRET is ever rotated: the same re-run.)
3. **Step 8c, the hub secret is the right one:** `sudo units/hub-probe --owner "$USER" --hub http://100.101.219.96:4011`. It checks that EVERY listener on the port (tcp and tcp6) is yours and that one has the url's address (a wildcard listener covers only this host's own addresses; canonical dotted quad), then makes one admin call with the root-only secret (the Bearer goes to curl on stdin, never in argv) and deletes the test principal by its exact id (POST `{role: relay, sid: gatem-probe, gen: 1, pub: 43 x A, may_reach: []}`, DELETE `/v1/principals/relay%3Agatem-probe%2F1`, confirmed by G). Pass: `POST 201`, `DELETE 200`, `hub secret OK` (a second DELETE gives 404; re-running is harmless). **401 or 403: the secret does not match, STOP**: fix `~/.config/session-bus/secret`, re-run the install.

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

## `units/install-linux.sh plan` as of 2026-11-20 (owner tomas, after the gate fixes: image by digest, machine name, no --profiles)

```
install-linux plan  (owner=tomas, bin-dir=<repo>/dist)
Nothing below is run by 'plan'. The same steps run under 'install --yes' as root.
GATES (install refuses while any is unmet)
  UNMET: no fence image: pass --image ghcr.io/landovsky/ccsbx-fence@sha256:<64 hex> (the CI-built candidate, U1a); a tag is refused
  UNMET: B9 missing: user unit session-bus-hub.service is not active (the native hub release, stream C)
  UNMET: P15 open: 24 row(s) not done or dropped: A5 B-session-bus B-messaging-hub-db-1 B-redis-5ctq B-redis-4s6w … (run units/p15 status)
  UNMET: binaries missing in <repo>/dist: ccsbx-root ccsbx-hostd ccsbx-signer ccsbx (a linux build of each + SHA256SUMS in --bin-dir; see --help)
  note: sudoers.d/90-tomas-nopasswd not readable without root: verified at install
  note: sudoers.d/README not readable without root: verified at install
  P15 status file: /home/tomas/cc-tree/cc-sandbox/nodes/4vwb/p15-status.tsv
  allowed roots:   "/home/tomas/git", "/home/tomas/wt"
  signer scope:    owners=tomas machines=hp-ubuntu pairs=dev-default:api|stub,e2e-default:stub

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
     write /etc/ccsbx/ccsbx-root.toml  (mode 0644, root:root, 4 lines)
     # a root-owned copy of the pins: ccsbx-root and hostd read THIS, never the owner-writable repo
     write /etc/ccsbx/pins.toml  (mode 0644, root:root, 8 lines)
     # pins.toml is empty today: only e2e candidate images run, as expected, until U1a is pinned

[03] U0b machine key and age key (read only by ccsbx-keys; never by tomas or hostd)
     # age key: created here only if R10b didn't (skipped when /var/lib/ccsbx-keys/age.key exists)
     $ runuser -u ccsbx-keys -- age-keygen -o /var/lib/ccsbx-keys/age.key
     $ chmod 0600 /var/lib/ccsbx-keys/age.key
     # machine key: made by the signer itself (--init), so no other process ever sees it
     $ runuser -u ccsbx-keys -- /usr/local/libexec/ccsbx/ccsbx-signer --keys /var/lib/ccsbx-keys --init
     # the signer will not serve without its scope (owners, machines, profile:billing pairs). The signer writes it itself
     #   (--write-scope: atomic, 0600): only that binary ever opens machine.key. Rewritten on every run to match the options
     $ runuser -u ccsbx-keys -- /usr/local/libexec/ccsbx/ccsbx-signer --keys /var/lib/ccsbx-keys --write-scope --owners tomas --machines hp-ubuntu --pairs dev-default:api\|stub\,e2e-default:stub
     # the printed PUBLIC key goes into config/machines/<machine>.json (M9); nothing secret is printed
     # the K8 kubeconfig arrives SOPS-encrypted to the age key; it is decrypted only by ccsbx-keys

[04] U0b signer socket + service (sign(token_claims) only; the socket is activated by systemd)
     write /etc/systemd/system/ccsbx-signer.service  (mode 0644, root:root, 38 lines)
     write /etc/systemd/system/ccsbx-signer.socket  (mode 0644, root:root, 16 lines)
     $ systemctl daemon-reload
     $ systemctl enable --now ccsbx-signer.socket

[05] U0 hostd unit
     # the tmux server is its own unit (no sandbox: its panes run sudo ccsbx-root and need the host mount namespace); hostd only connects
     write /etc/systemd/system/ccsbx-tmux.service  (mode 0644, root:root, 35 lines)
     # ccsbx-root's run directory (it makes it itself, root-owned 0755; made here so hostd's unit can bind it read-write)
     $ install -d -o root -g root -m 0755 /var/lib/ccsbx-run
     # hostd's unit gets the same roots as ccsbx-root (/home/tomas/git,/home/tomas/wt, as --worktree-root) and the fence image (, by digest) and machine name (hp-ubuntu)
     write /etc/systemd/system/ccsbx-hostd.service  (mode 0644, root:root, 59 lines)
     # the bearer token for hostd's loopback API: made once (0600, ccsbx), never replaced
     $ runuser -u ccsbx -- sh -c umask\ 077\;\ head\ -c\ 32\ /dev/urandom\ \|\ base64\ \>\ /var/lib/ccsbx/listen.token
     $ systemctl daemon-reload
     $ systemctl enable --now ccsbx-tmux.service
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
     # ccsbx-root runs with a CLEAN environment: 'Defaults!/usr/local/sbin/ccsbx-root env_reset,!setenv' (the caller's env is reset and
     #   cannot be passed through, so no sudo -E and no VAR=x reaches it)
     # 1. the new rule goes in a dot-named staging file (sudo ignores it), is checked, then renamed
     write /etc/sudoers.d/.90-ccsbx-root.new  (mode 0440, root:root, 5 lines)
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
