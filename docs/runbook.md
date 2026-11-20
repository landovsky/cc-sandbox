[Index](README.md)

# cc-sandbox runbook: every manual procedure

> **Rule (Tomas, Q18):** every manual procedure lives here, in the same PR as the piece that
> introduces it. One section each, with five fields: **When, Who, Steps, Check, Rollback.**
> `tests/procedures-check.sh` fails when a piece marked `+O7` in the
> [build plan](cc-unified-model-build-plan.md) has no section, or a section lacks a field.
> A section says which pieces it covers in a `<!-- procedure: ID ... -->` comment.
>
> **Status of each procedure.** *live* = written against things that exist today and run as
> written. *planned* = written from the plan before the piece exists; the command shown is the
> design, and `_GAP:_` marks what the building piece must confirm. When a piece lands, it
> replaces "planned" with "live" and runs the steps once.
>
> Operational facts (topology, secrets, backups) are in [`artifacts/gitops.md`](../artifacts/gitops.md).
> **Night 1 (2026-11-19) waives invariant 14**: while tomas is in the docker group, every fence
> result is "not evidence". Nothing here is deployed by a night run; each deploy is a human gate.

| # | Procedure | Status | Covers |
|---|---|---|---|
| 1 | [Evening gate](#1-evening-gate) | live (preflight) | R1–R22, NB1 |
| 2 | [Morning gate](#2-morning-gate) | live up to the install; the rest planned | R10, NB4 |
| 3 | [hp root install: plan, install, check](#3-hp-root-install) | written, **never run** | I0, U0, U0b |
| 4 | [Night auto-start and stop](#4-night-auto-start-and-stop) | planned | NB4 |
| 5 | [Redeploy a cc-sandbox binary (ccsbx-deploy)](#5-redeploy-a-cc-sandbox-binary) | planned | NB5 |
| 6 | [Cockpit failover](#6-cockpit-failover) | planned | C13 |
| 7 | [future-two unlock](#7-future-two-unlock-after-a-reboot) | planned (the alert); the unlock itself is the k3s side's | O3 |
| 8 | [Pin promote](#8-pin-promote) | planned | H2 |
| 9 | [Roll back a live tool to its P0 tag](#9-roll-back-a-live-tool-to-its-p0-tag) | live | P0 |
| 10 | [Hub redeploy and rollback (native release)](#10-hub-redeploy-and-rollback) | planned | B9 |
| 11 | [Strict-mode flag flips (`SB_REQUIRE_AUTH`)](#11-strict-mode-flag-flips) | planned | B6b |
| 12 | [Rotate keys](#12-rotate-keys) | planned | M9, O5 |
| 13 | [Rotate the subscription setup-token](#13-rotate-the-subscription-setup-token) | planned | R5b, O5 |
| 14 | [The approval code](#14-the-approval-code) | planned | P7 |
| 15 | [Add a procedure (this document's own rule)](#15-add-a-procedure) | live | O7 |

---

## 1. Evening gate
<!-- procedure: O7 NB1 -->

- **When:** before a night starts, once. The preflight is `bin/ccsbx-preflight`.
- **Who:** Tomas, 60–90 min the first time. `R1–R22` are his steps (plan §7.2); the preflight only
  reads.
- **Steps:**
  1. Do the R-steps you can; leave the rest.
  2. `bin/ccsbx-preflight --evening` (add `-v` for full detail, `--json` for the night manager).
  3. For each failure the last lines name it: `ABORT:` means the night doesn't start, `SKIP:` means
     only the dependent pieces are parked and listed in the morning report.
  4. To start anyway, waive a check on purpose: `--waive C-R12`. A waived check shows as
     `waived`, never as pass.
  5. Type `Arm!` in the night manager's tab (R16) after the preflight is green.
- **Check:** exit 0 with `nothing failed`, or only waived rows. `bin/ccsbx-preflight --list` shows
  every check with its gates.
- **Rollback:** nothing to undo (read-only). To stand the night down: `cc-tree disarm cc-sandbox:2f3e`
  and see §4.

## 2. Morning gate
<!-- procedure: O7 R10 -->

- **When:** after night 1, before night 2. ~45 min including the reboot.
- **Who:** Tomas only (root, the SHARED merge, the reboot).
- **Steps:**
  1. Read `docs/night-1-morning.md` (the report and the `install-linux.sh plan` output).
  2. Decide the open questions in the P14 inventory (`cc-sandbox/nodes/4rcv/inventory.md` in the
     tree repo), above all the three `github-runner@N` units (they keep docker access through their
     own unit) and the memory-store backup job.
  3. `units/install-linux.sh install --yes` under sudo: see §3.
  4. Merge the M8/M1 PR (**the first SHARED go**) in `~/git/k3s`.
  5. `sudo reboot`. Open a tmux session again.
  6. `bin/ccsbx-preflight --morning --post-M1`.
- **Check:** step 6 exits 0; `units/install-linux.sh check` exits 0; in a fresh tmux
  `grep Groups /proc/$(pgrep -u $USER -o tmux)/status` has neither the docker nor the lxd gid;
  `sudo -n -l` lists NOPASSWD only for `ccsbx-root` (and `ccsbx-deploy` if Q12 = yes).
- **Rollback:** the install writes a backup of every file it changes (§3 Rollback). Until step 5
  the old paths still work. After the reboot, to get docker back for tomas:
  `sudo usermod -aG docker tomas` and log in again; that restores invariant-14-waived mode.

## 3. hp root install
<!-- procedure: I0 U0 U0b -->

- **When:** at the morning gate (R10). Never unattended and never from a night run. It **refuses**
  while B9 (the native hub) or P15 (the replacements for hp's docker users) is missing, because it
  removes tomas from the docker group and the live hub is a docker container.
- **Who:** Tomas, with sudo. `plan` and `check` need no root.
- **Steps:**
  1. `units/install-linux.sh plan`: prints every step, changes nothing. Read it.
  2. `sudo units/install-linux.sh install --yes`. It checks the refusals first, then copies every
     file it will replace to `/var/backups/ccsbx-install/original/` (once; never overwritten), and
     does the steps in the order `plan` printed. Steps that can fail on this machine come before the
     sudoers change, and the whole sudoers config is re-checked after it. It stops at the first
     failed step, says which, and says `PARTIAL INSTALL` with the undo command.
     It prints `DONE` only after hostd and the signer are up. hostd listens on loopback only
     (`127.0.0.1:7480`, with a token file), so the firewall question (S11) does not arise.
     Before step 2 do the manual checks listed in `docs/night-1-morning.md` (S5, S9, S14, S15).
     **Pins, image, machine, worktree root:** the install copies `pins.toml` to `/etc/ccsbx/pins.toml` (root-owned;
     `ccsbx-root` and hostd read it, not the repo). There is no profiles copy: profiles are compiled into the
     binaries, so hostd has no `--profiles`. The signer's pairs come from the repo's `profiles/*.json` at install.
     hostd's `--image` is a **digest** (`--image ghcr.io/landovsky/ccsbx-fence@sha256:<64 hex>` is required; a tag
     is refused by the installer because every start of a tag fails with `image_not_pinned`). A digest not in
     `pins.toml` is a candidate that only e2e profiles may run. `--machine` is the lowercased `hostname -s`, written to
     both hostd's unit and `ccsbx-root.toml`, so root never refuses a start over a mixed-case name. hostd's
     `--worktree-root` is the `--allowed-roots` value `ccsbx-root.toml` gets (default `~/git,~/wt`; the same comma list
     for both, no spaces, no empty entry, no trailing slash, and each a real path: `readlink -f` must give the root itself, because hostd checks
     roots lexically and would refuse every start under a symlinked root); its `--run-dir` is `/var/lib/ccsbx-run`.
     **Signer scope:** before it enables the signer socket the install has `ccsbx-signer --write-scope` write
     `/var/lib/ccsbx-keys/signer.scope` (0600, `ccsbx-keys`); without it the signer exits 2. The scope lists the
     owner, the machine name and the exact profile:billing pairs from `profiles/*.json` (a profile is signed
     only with a billing in its own list). Rerun `install --yes` with `--machine/--pairs` to change it.
     `check` (as root) proves it by getting a token for one in-scope test claim (api + dev-default).
     **tmux and attach:** the ccsbx tmux server runs in its own unit, `ccsbx-tmux.service` (uid `ccsbx`, no
     sandbox, so it shares the host mount namespace: its panes run `sudo ccsbx-root run`, whose binds must reach
     dockerd). hostd `Requires=` it and only connects to `/run/ccsbx/tmux.sock`; it never starts the server.
     `/run/ccsbx` is `ccsbx`-only, 0700, with the socket 0600: a group-writable tmux socket would let any process
     of the owner's uid type into fenced panes. The owner attaches with `sudo ccsbx-root attach <sid>`; the
     installer's one rule for `ccsbx-root` already covers that verb, so no separate attach line is added.
     hostd's API socket is `/run/ccsbx-api/hostd.sock`, `ccsbx:ccsbx-clients` 0660, in a 0755 directory.
     `check` (as root) confirms `ccsbx-tmux`'s mount namespace equals PID 1's and that the socket is `ccsbx:600`.
     **Known cost:** any process running as uid `ccsbx`, hostd included, can open a tmux window on
     `/run/ccsbx/tmux.sock` (`tmux -S ... new-window CMD`) and run `CMD` outside hostd's sandbox, so that
     sandbox does not contain a compromised hostd. It already has `sudo ccsbx-root` and the signer socket; the
     sandbox limits accidents. Moving the hostd-to-tmux edge behind `ccsbx-root` would close it.
     **Known limit after Gate M:** hostd runs as uid `ccsbx` (no `--ccmsg-bin`), so it cannot use the
     owner's bus sockets. Messages from a fenced session to an unfenced manager wait for the hub
     spoke over :4011 (B10/B11/M2b). Until then only ccsbx sessions are reachable by hostd.
  3. `sudo reboot` (group changes apply to new login sessions only).
  4. `units/install-linux.sh check`.
- **Check:** `check` exits 0 after the reboot and prints one `ok`/`FAIL` line per step. Before the
  install it exits non-zero and names what is still open.
- **Rollback:** `sudo units/install-linux.sh uninstall --yes`, also after a failed or repeated
  install: it always restores from the FIRST backup (`/var/backups/ccsbx-install/original/`).
  **It undoes:** the units (hostd, signer, signer socket), the sudoers rules `90-ccsbx-root` and
  `91-ccsbx-deploy`, `ccsbx-root`, `ccsbx-deploy`, `ccsbx`, `/etc/ccsbx/ccsbx-root.toml`; every
  `sudoers.d` file as it was (then `visudo -c`); sops's owner and mode;
  the owner's `ccsbx-clients` membership. The backup is then renamed `original.undone-<time>`.
  **It does NOT undo:** docker/lxd group membership (re-add with `gpasswd -a tomas docker` to
  waive invariant 14 again), the lxd snap, the users `ccsbx` and `ccsbx-keys`, the group
  `ccsbx-clients`, `/var/lib/ccsbx` (the journal) and the keys directory (`--purge-keys` deletes the
  keys and `ccsbx-keys`: rotate M9 first). `--backup DIR` must be under `/var/backups/ccsbx-install/`,
  root-owned and not group- or world-writable.
- **Seccomp profile for `ccsbx-root run` (Gate M step; fails closed without it).** `ccsbx-root run`
  refuses to start a fence unless `/etc/ccsbx/seccomp.json` is root-owned and verified. It is Docker's
  default profile with `chmod`, `fchmod`, `fchmodat`, `fchmodat2`, `open`, `openat`, `creat`, `mknod`
  and `mknodat` allowed only when the mode has no setuid or setgid bit (a setid mode falls to the
  profile's EPERM default) and `openat2` and `io_uring_*` refused. Nobody can fetch the base on the night; the commands are Gate M **step 8b** in
  `docs/night-1-morning.md`, between the install (step 8, which creates `/etc/ccsbx`) and the reboot (after which
  tomas has no docker). The docker twin uses the candidate fence image `ccsbx-fence:dev`, since there is no
  production pin yet.
  The base must be the file of the installed engine's version tag (the URL in step 8b). The last command of
  step 8b is the docker twin: it tries `chmod 4755`, `2755`, `u+s` and creating a file with mode 4755 inside
  a fence, and checks that `chmod 755` still works; it is not part of `make test`.
- **Which mode runs (U1c).** `ccsbx` and `ccsbx-hostd` take `--root auto|direct|helper` (env `CCSBX_ROOT`).
  `auto`, the default: helper if `/usr/local/sbin/ccsbx-root` exists, is a regular file owned by root and
  not writable by others, else direct. In helper mode `ccsbx run`, `stop`, `gc` and hostd's pane run
  `/usr/bin/sudo -n /usr/local/sbin/ccsbx-root <verb> <sid>`; a sudo refusal (password needed, not
  permitted, helper missing) is reported as that with exit 2 and there is no fallback to direct. Direct
  mode refuses with a clear error when docker's socket is unreachable (it is, after Gate M).
- **Real bind-mount test (Gate M).** `sudo unshare -m env GOTOOLCHAIN=local /home/tomas/.local/go/bin/go test -tags rootmount -run TestRealMounter ./internal/rootcmd` (go is not on sudo's PATH; step 7b has the `go version` precheck).
- **Known: orphaned direct-mode homes.** The node home is now keyed by owner, class and label, so
  homes made by direct mode before this change are not found again: `--resume` of such a session
  starts without its old transcript. Nothing important lives there yet; they can be deleted by hand.
- **Known limit: your own git in a worktree a fence wrote to.** The fence has the worktree read-write.
  ccsbx-root mounts the repository's own gitdir read-only, and H1 keeps the HOST's git away from
  anything the fence wrote, but your own `git commit` in that worktree trusts what it finds there:
  hooks that `core.hooksPath` (the repository's config, or your global one) or an `include.path` points
  into the worktree (Husky's `.husky/pre-commit`), include files that live in the worktree, and nested
  repositories (`sub/.git`). Before you run your own git in such a worktree, run
  `ccsbx steering-check --base <base>`: it lists changes under those paths, every in-worktree include
  file (changed or not), every nested repository (an unignored `node_modules` included), unreadable
  directories, and exits 3 if there is anything; exit 4 means it could not look everywhere (the walk cap)
  and is NOT a clean result. Do not commit until it says exit 0.
  **Exit 0 does not make your commit safe if a hook runs code the fence can edit and git ignores**:
  Husky and lint-staged call `node_modules/.bin/...`. For a worktree a fence wrote to, commit with
  `git -c core.hooksPath=/dev/null commit`, or review that code first. Command-valued config keys that
  name a script in the worktree (`filter.*.clean`, `diff.*.textconv`, `core.fsmonitor`, `core.editor`,
  `alias.*=!...`) are not read by the check either.

## 4. Night auto-start and stop
<!-- procedure: NB4 -->

- **When:** *planned.* After the morning-gate reboot, once `ccsbx-preflight --morning` is green.
  Stop: when the night should not run (sick, travelling, a bug found).
- **Who:** the tschedule job starts it; Tomas stops it.
- **Steps:**
  - *Start:* the tschedule job runs `ccsbx-preflight --morning --abort-only`, then
    `cc-tree cockpit up`, then starts the night manager. `_GAP:_ the job's name and schedule are
    NB4's to decide.`
  - *Stop:* `cc-tree disarm cc-sandbox:2f3e` (the runner goes off), then disable the tschedule job.
    The night stop rules (a piece failing 3 times is parked, doors down 30 min means hp-only work,
    a rate or spend limit parks the pieces that need it) are in plan §7.2.
- **Check:** after a reboot, `cc-tree show cc-sandbox` shows the manager `working` within 10 min.
  Morning table: `bin/ccsbx-night-report --dry-run` (add `--send` to send it).
- **Rollback:** disarm; nothing the night wrote is deployed, so a stopped night leaves only
  branches and PRs.

## 5. Redeploy a cc-sandbox binary
<!-- procedure: NB5 -->

- **When:** *planned, only if Q12 = yes.* Night 2+, to ship a binary built by CI from `main`
  without a human: `ccsbx`, `ccsbx-hostd`, the forwarder, the sidecar.
- **Who:** the night manager, through one sudoers rule for `/usr/local/sbin/ccsbx-deploy`
  (R10c). Tomas installs that rule.
- **Steps:** `sudo -n ccsbx-deploy install <name>@sha256:<digest>`. It accepts only artifacts
  built by Actions from `main`, digest-pinned and attestation-verified.
- **Check:** `sudo -n ccsbx-deploy install ccsbx-root` is **refused** (it never installs
  `ccsbx-root`, the signer, itself, or anything that talks to `ccsbx-keys`);
  `sudo -n ccsbx-deploy install <unattested>` is refused. The preflight checks both (C-R10c).
- **Rollback:** install the previous digest the same way (the previous digest is in the log the
  command prints). For `ccsbx-root` or the signer: Tomas, by hand, §3.

## 6. Cockpit failover
<!-- procedure: C13 -->

- **When:** *planned.* The cockpit (the tree view's writer) normally runs on hp. Move it when hp
  is going down, or when the cockpit machine died and you want the tree live again. While the
  cockpit is down only visibility is lost: fenced work, messages and grants carry on (plan §8).
- **Who:** Tomas, from the machine that should hold the cockpit.
- **Steps:**
  1. If the old cockpit is alive: on it, `cc-tree cockpit down`.
  2. On the new machine: `cc-tree cockpit up`. It runs `cc-tree sync --pull`, checks that the last
     cursor-ack commit of every daemon is an ancestor of HEAD (else it refuses, naming the machine
     and the commit: "that machine's last commits are not pushed". Push them there first, or
     `--wait 60` to keep pulling), takes the lease on every daemon (all or none), takes the
     daemon snapshot, then tails from the old cockpit's cursor and applies the gap at once.
     The daemons are the ones in `machines.json` at the root of the tree repo
     (`{"machines": ["hp-ubuntu", "mac"]}`), else the hosts fenced nodes ran on.
  3. If the old machine is dead and holds the lease: `cc-tree cockpit up --steal`. Use it only
     when the old cockpit really is down, since two writers means duplicate verbs.
- **Check:** `cc-tree cockpit status` on the new machine shows the lease held on every daemon
  (hp, future-two, the Mac if in); the gap's events appear; no duplicate journal ids in
  `history-*.jsonl`. Test: the cockpit-kill test (plan §8, C20).
- **Rollback:** `cc-tree cockpit down` on the new machine and `cc-tree cockpit up` on the old
  one. If `up` was never run, cc-tree behaves as it does today (standalone).

## 7. future-two unlock after a reboot
<!-- procedure: O3 O3b -->

- **When:** future-two reboots (Hetzner maintenance, a kernel update). Its disk is LUKS and waits
  for a manual unlock; until then every door and every future-two session is down. R11: no reboot
  may be planned during a night.
  **The alert for it is planned, not built.** What runs today (per k3s review 2026-11-20): the k3s
  CronJob `cc-sandbox-watchdog` (every 5 min) alerts on Pending or failing pods only, and the shared
  `k8s-event-notifier` covers node pressure and evictions. Neither checks `bootID` or names the
  unlock command, and nothing runs on hp's tschedule. So today a reboot shows up as a pile of
  Pending pods, or as the doors' `/health` failing, not as "future-two rebooted".
  `_GAP:_ O3 (the bootID check, the unlock command in the message, the 30-day setup-token warning)
  is unbuilt: until it exists, check the node by hand after any Hetzner notice.`
- **Who:** Tomas, from his phone or any machine.
- **Steps:** unlock over dropbear (port 2222) with `future-two-unlock` (k3s
  `docs/HARDWARE-INVENTORY.md`). `_GAP:_ the command is not on hp; where it lives (the Mac, a
  phone shortcut) and its exact arguments aren't recorded here: write them in once Tomas has run it
  with this doc open.` Never type the passphrase into an agent session.
- **Check:** the node is Ready (`kubectl get node future-two`, from the Mac or with the `ccsbx-ops`
  read RBAC); doors `/health` 200; `kubectl -n cc-sandbox create job --from=cronjob/cc-sandbox-watchdog
  wd-now` shows no Pending pods. Fenced sessions that were on future-two lost unpushed work: check
  their branches.
- **Rollback:** nothing to roll back. If the node doesn't return: hp-only work continues (the
  night rules say hp-only after 30 min of doors down), and `ccsbx start --backend docker` stays.

## 8. Pin promote
<!-- procedure: H2 -->

- **When:** *planned.* A new fence image, `ccsbx` binary or Claude Code version should become
  production. Weekly bump job O6 builds a candidate and opens a PR on green; promotion is manual.
- **Who:** Tomas (decision: manual promote, Q18).
- **Steps:**
  1. The candidate digest runs under `e2e-*` profiles only (it is refused under `dev-default`).
  2. Run the full suite on hp and future-two (plus the Mac when Q12 puts it in):
     `ccsbx selftest --suites all --backend docker` and `--backend k8s`.
  3. Promote: edit `pins.toml` to the new digest and merge. `_GAP:_ H2 decides whether a
     command (`ccsbx pins promote <digest>`) writes it. A promote with a red result must be refused.`
  4. On the k3s side the VAP accepts any digest in `@sha256:` form from our three repos, so no
     cluster change is needed.
- **Check:** `T:unpinned-refused` (an unpinned production digest is refused); the next production
  start uses the new digest (`ccsbx start --json` prints it).
- **Rollback:** put the previous digest back in `pins.toml` and merge. Running sessions keep their
  image until their next generation.

## 9. Roll back a live tool to its P0 tag
<!-- procedure: P0 -->

- **When:** a change to a live tool (session-bus, cc-tree/cc-flow, ccsp) misbehaves and the fix
  is not obvious. Every one of them was tagged `pre-ccsbx-2026-11-19` before any change (P0).
  Every builder change to these tools is behind a flag first: **turn the flag off before you
  roll back** (see §11).
- **Who:** Tomas. Builders never run `ccsp deploy|rollback`, `sb-deploy-hub`, or restart a live
  service (deny list).
- **Steps:**
  - **cc-tree / cc-flow** (live from the `~/.dotfiles` checkout; hp's `cc-tree` resolves to
    `~/.dotfiles/bin/cc-tree`). Restore only the tool's files, not the whole repo:
    ```
    cd ~/.dotfiles
    git rev-parse pre-ccsbx-2026-11-19                    # the tag exists
    git diff --stat pre-ccsbx-2026-11-19 -- bin/cc-tree bin/cc-pick bin/cc-tree-web bin/lib/cc_tree*.py ccf
    git checkout pre-ccsbx-2026-11-19 -- bin/cc-tree bin/cc-pick bin/cc-tree-web bin/lib/cc_tree*.py
    git commit -m "revert cc-tree to pre-ccsbx-2026-11-19"
    systemctl --user restart cc-tree-web                  # the web view
    ```
    (`ccf` exists only after C1; `git checkout` fails on a missing path: drop it from the line.)
    On the Mac: `git pull` there, no restart.
  - **session-bus hub, today's docker deploy:** `SB_BRANCH=pre-ccsbx-2026-11-19 sb-deploy-hub`
    ships the tag with `git archive` and relaunches the container (the script's `SB_BRANCH`
    override). After B9 use §10 instead. The spokes (hp hooks, the Mac's launchd unit) need no
    change.
  - **ccsp (the secret proxy):** tag `pre-ccsbx-2026-11-19` in its own repo. Follow that
    repo's own `ccsp rollback` procedure and its docs; they live in that repo and this one never
    touches it. `_GAP:_ link it here once the ccsp rollback command is confirmed with the person
    who runs it.`
- **Check:**
  - cc-tree: `cc-tree show cc-sandbox` works; `tests`: S0 (`make standalone`, stream C) is green.
  - session-bus: `sb-deploy-hub status` lists the Mac and hp; `ccq` shows live sessions; a new
    endpoint added by the change now returns 404.
  - ccsp: a normal Claude session still gets model replies.
- **Rollback:** (of this rollback) the revert is a commit (cc-tree), a redeploy (hub) or a deploy
  (ccsp), so redo the change from the branch.

## 10. Hub redeploy and rollback
<!-- procedure: B9 -->

- **When:** *planned.* After B9, the hub is a native release under a systemd user unit on hp (no
  docker). Redeploy to ship a change; roll back when the new release misbehaves.
- **Who:** Tomas. A night run builds and tests the release on a spare port and never switches the
  live hub (human gate).
- **Steps:** `sb-deploy-hub` ships `git archive` into a new release directory, then
  `systemctl --user restart` the unit; the previous release dir is kept. `--dry-run` prints the
  steps and changes nothing. `_GAP:_ exact subcommands and the release path are B9's: confirm
  against `sb-deploy-hub --help` once merged.`
- **Check:** `/health` on the hub port; `ccq` lists the Mac and hp; probe something unique to the
  new build (a new route returns 401/422, not 404), since `/health` only proves the process booted.
- **Rollback:** point the unit's `current` at the previous release dir and restart the user unit;
  the previous release is intact by design. Standalone: the hub must run with no cc-sandbox
  component (S0).

## 11. Strict-mode flag flips
<!-- procedure: B6b -->

- **When:** *planned.* Every builder change to a live tool ships with its strict mode **off**.
  Flip it only after every reader and spoke is on a version that works with it. First flag:
  `SB_REQUIRE_AUTH` (B6b, fail-closed hub auth), after B6a has put auth on every hub reader on hp
  and the Mac.
- **Who:** Tomas, at a human gate.
- **Steps:**
  1. On a test hub on a spare port: set `SB_REQUIRE_AUTH=1`; with no `SB_SECRET` the hub must
     refuse to boot; `curl <hub>/sessions` without auth must answer 401.
  2. Against that test hub, `cc-tree show`, `cc-pick`, `ccq` must still list both machines (B6a).
  3. Only then set the flag in the live unit's environment and restart (§10).
- **Check:** unauthenticated `/sessions` → 401; `ccq` on hp and the Mac still lists sessions; old
  `ccmsg` works. With the flag off, S0 is green.
- **Rollback:** unset `SB_REQUIRE_AUTH` and restart the hub. Today's behaviour returns at once.

## 12. Rotate keys
<!-- procedure: M9 O5 -->

- **When:** *planned.* Model and GitHub App keys every 90 days; machine keys yearly or on loss;
  session tokens are ≤ 12 h and need nothing; the launcher kubeconfig token every 30 days (the
  launcher renews it itself, K8). Immediately on any suspected leak.
- **Who:** Tomas for keys held in git/SOPS; the launcher for its own token.
- **Steps:** `ccsbx keys --help` lists every recipe (O5 makes that true). Machine keys follow
  **add, roll, remove**: add the new public key to `config/machines/<m>.json` (PR, ConfigMap
  `ccsbx-machines`), roll the signer onto it, then remove the old one. API key and GitHub App key:
  create the new one in the Console / GitHub, encrypt with SOPS into the k3s repo as in R5/R7,
  PR, and let Flux apply; then delete the old one at the provider. Never paste a key into an agent
  session.
- **Check:** a removed key is refused (401) within 2 min (`T:key-signs-foreign-machine`); a door's
  `/health` says `credential: apikey` (never the value); a token with 2 days left is renewed by the
  launcher.
- **Rollback:** until the old key is removed at the provider, re-add it to the list and the SOPS
  file; after removal there is no rollback, so roll forward with another new key.

## 13. Rotate the subscription setup-token
<!-- procedure: R5b -->

- **When:** *planned.* Yearly (it expires after a year; O3 is meant to warn 30 days before, `_GAP:_` unbuilt, so put the date in a calendar) and immediately on
  any suspected leak. A stolen token is a uncapped credential for the whole subscription.
- **Who:** Tomas, on the Mac (browser login).
- **Steps:**
  1. `claude setup-token` on the Mac: it prints a one-year token and saves nothing.
  2. In the k3s repo on a branch: `read -rs T; printf 'CLAUDE_CODE_OAUTH_TOKEN=%s\n' "$T" | sops -e
     --filename-override apps/cc-sandbox/secrets-cc-sandbox-model-sub.env --input-type dotenv
     --output-type dotenv /dev/stdin > apps/cc-sandbox/secrets-cc-sandbox-model-sub.env; unset T`,
     commit, PR. Only test keys go in until Q13 (k3s secrets-encryption) is done.
  3. After the merge, revoke the **old** token: claude.ai → Settings → Claude Code → the token
     list (not in the official docs; U4s checks that revocation works).
  4. Record the creation date in `artifacts/gitops.md` §5.
- **Check:** the model door's `/health` says `subscription: ok` (never the value) after one
  1-token request; `bin/ccsbx-preflight --post-M1 --only C-R5bp` is green.
- **Rollback:** the old token works until revoked: re-merge the old SOPS file if the new one is
  bad. After revocation, make a new one.

## 14. The approval code
<!-- procedure: P7 -->

- **When:** *planned.* A fenced session asks for one more egress route or one extra repo read
  (a low-tier `grant.request`). The model door sends Tomas a 6-digit code by Telegram and creates
  the request; nothing is approved without both Tomas's Tailscale identity and the code.
- **Who:** Tomas only. An agent can't: a decision from a session token or through the forwarder is
  refused (403).
- **Steps:**
  1. A ❓ card appears in the tree (only from door-originated events).
  2. Open the decision page on the door's owner listener from a tailnet device (the link is on the
     card; tailnet hostnames are not written in this public repo).
  3. Type the code from Telegram and choose approve or deny. First decision wins.
  4. Medium and high tiers are refused at solo; use the friends-tier path later.
- **Check:** card turns ✅ with `proof: header+otp`; the session gets `grant.decided`; the grant
  dies with the generation.
- **Rollback:** a grant is an overlay for one generation: end the session (`ccsbx stop`) or let
  it end. Lost code: let the request expire and ask again; there is no way to approve without it.

## 15. Add a procedure
<!-- procedure: O7 -->

- **When:** every piece marked `+O7` in the build plan adds its section in the same PR, and a
  deploy or a manual step that someone has to remember counts as a procedure.
- **Who:** the builder of the piece; the reviewer checks it.
- **Steps:**
  1. Copy a section above. Keep the five fields in this order: When, Who, Steps, Check, Rollback.
  2. Add `<!-- procedure: PIECE ... -->` under the heading with the piece ids it covers.
  3. Say *planned* or *live* in When, and mark anything unconfirmed `_GAP:_`.
  4. Add a row to the table at the top and run `tests/procedures-check.sh`.
- **Check:** `tests/procedures-check.sh` exits 0 (it reads the `+O7` pieces from the plan and each
  section's five fields). `make procedures` is the same command once P1's Makefile exists.
  `~/git/k3s/scripts/gitops-inventory.sh` lists cc-sandbox as compliant.
- **Rollback:** a doc: revert the commit.


## Messages across machines through the hub (M2b)

hostd is a client of the session-bus hub over HTTP (`--hub-url`, `--hub-token-file`: a spoke credential for
THIS machine, minted once by the hub's admin; hostd never holds the admin secret and never touches the
owner's bus sockets). It tracks its fenced sessions there (with their tree `node`, taken from the session
record, never from the fence) and polls `spoke:<machine>/ccsbx`.

**Two principals per machine, enforced by the hub.** `spoke:<machine>/ccsbx` is hostd's: it may track FENCED
entries only, join/poll only `spoke:<machine>/ccsbx`, and may not deliver; its token lives only in the
root:ccsbx 0640 file. `spoke:<machine>` is the owner's sessions' (ccmsg, the spoke service, hooks): UNFENCED
entries only (any fenced flag, attach claim or routing into the ccsbx downlink is refused, 403) and it may
not join or poll the ccsbx downlink. So an owner-uid process holding the owner's token can never act with
hostd's rights (tracking a fenced identity, reading hostd's frames), and the reverse.

- **Receiving.** A delivery that claims `fenced` is verified by hostd itself: the Ed25519 stamp (domain
  `ccsbx-stamp-v1`, 120 s skew, 10-minute nonce cache), the `to` it names must be THIS target, the sender's
  node comes from the hub's presence entry for its sid, and scope is re-checked against the target's own
  record (manager, peers, reply-to-sender). An unfenced sender is delivered as `unfenced (hub)` under the same
  scope. The typed text is rendered by hostd from the verified body, and the hub's own line must carry the
  same flattened body or the message is refused.
- **Unfenced senders** (an ordinary Mac manager using ccmsg) are heard only through THEIR MACHINE'S SPOKE
  PRINCIPAL. The shared owner secret is held by every owner session on every machine, so a message posted with
  it binds nothing and is refused. The deliver must have come in on `spoke:<machine>`, the hub must have bound
  `from.id` to an entry that same principal tracked on that machine, `from.machine` must name it (it may not
  be left out), and hostd confirms the binding against its own read of the presence list (the entry exists, is
  not fenced, is tracked by that principal). The line is typed visibly as `unfenced (hub)` and the normal
  scope check runs on the bound node. Residual: any owner-uid process on THAT one machine holds its spoke
  credential and can name another session tracked under it (the local ccmsg trust, hence the label). It can
  never claim a fenced identity (that needs a relay signature), and a node is held by ONE principal: the hub
  refuses (409) an owner entry carrying a node a fenced entry or another principal's live entry already holds,
  and hostd independently hears no sender whose node anything else holds (another hub entry or a session
  record of this machine), so an owner token of any machine cannot be heard as a fenced manager, nor make the
  real manager's node ambiguous. Owner sessions route only to `spoke:<machine>` (no named downlink), and an
  owner spoke's focus/rename/drop/read reach unfenced sessions of its own machine only; hostd's principal
  gets none of those four.
- **A node belongs to a machine** (round 5). A session's record carries the machine of its manager and peers
  (`msg_scope.manager.machine`, `msg_scope.peer_machines`: from the owner's start spec, cc-tree sends the tree
  record's Host), never from the fence. hostd hears an UNFENCED manager or peer only from that machine, and
  sends to it only if the hub's entry for the node is on that machine (otherwise `msg.dropped`; another
  machine's entry for the same node is a squatter's and neither receives a report nor makes the real one
  ambiguous); a scope that names no machine for an unfenced node reaches nobody unfenced. The relay's
  `may_reach` is `<machine>/<node>` and the hub matches both. The hub keeps a durable claim node→machine (first
  publisher, or hostd's `binds` for its session's manager/peers, which take over a claim): another machine's
  entry for a claimed node is refused (409), so the real manager is never locked out; `GET /v1/nodes` (admin)
  lists claims, `DELETE /v1/nodes/<url-encoded node>` clears one when a node moves machines on purpose, and an
  unrefreshed claim ends after 30 days (`SB_NODE_CLAIMS_FILE` keeps them across a hub restart). The shared secret
  is conflict-checked too (a node a fenced entry or a principal holds cannot be re-published with it). Known
  window: a squat BEFORE any worker bound to that node has started is accepted (nobody has said whose it is) and
  is overridden when the worker's hostd tracks; two tracks of one node at the very same moment can both pass the
  presence check (hostd's checks catch the result: a refused message, not an impersonation).
  **A 409 on your OWN node** (a manager's tracking refused, `ccmsg`/cc-tree not resolving it) means look at
  `GET /v1/nodes` (admin): another machine holds the claim. If it is a squatter that keeps renewing, it holds it
  indefinitely (the 30 days count from the last refresh): `DELETE /v1/nodes/<url-encoded node>`, then the real
  manager tracks. hostd's `binds` (its sessions' manager/peer machines, from their start specs) only cover nodes of
  the hostd principal's `trees` and may only DEFEND a claim their own machine already holds: they never create, move or
  win one (a claim comes only from a live session's own track on its machine), so a start spec naming a machine for a
  manager that is offline (a sleeping Mac) pins nothing, and the manager's track succeeds when it returns. Where a claim
  held by another machine disagrees with a daemon's bind, the hub says so in the track reply and hostd logs `hub warning:
  <node> is claimed by <machine>` once per ten minutes: the session's start spec put its manager on the wrong machine, or
  someone is squatting; fix the spec's `manager.machine` (and re-start) or clear the claim. **Residual window:** a squat
  BEFORE the manager's first track (nobody has claimed the node yet) is accepted, and a squatter that keeps renewing keeps
  the claim: an owner-uid process on any machine can lock a manager out that way (a DoS, no impersonation or leak: hostd
  hears and sends by the record's machine, and now also for FENCED senders: a fenced session labelled with a manager's node
  on another machine is refused). The remedy: `GET /v1/nodes` and `GET /v1/principals` to see who holds it, REVOKE the squatting machine's principal first (`DELETE /v1/principals/spoke:<machine>`, then re-mint that machine's own token: otherwise it re-claims at its next track), THEN `DELETE /v1/nodes/<node>`. A node whose record places it on no machine (an old record) reaches nobody: hostd refuses it as `unplaced`, and a new start refuses a `msg_scope` that lists a manager or peer without its machine (`bad_spec`). The hub's unit should set `SB_NODE_CLAIMS_FILE=$HOME/.local/state/session-bus/node-claims/claims.json` (a directory of its own, created 0700; the unit file is D's/the dev manager's to change) so a hub restart keeps the claims. `--known-machines`
  makes a start whose `msg_scope` names any other machine refuse (bad_spec) before anything is published. The claim
  table lives in memory unless `SB_NODE_CLAIMS_FILE` is set (a hub restart then reopens the first-publisher window
  until each hostd re-tracks, seconds); the file is written through an exclusive random temp, in a directory created 0700 if new (an existing directory is never chmod-ed: give the file one of its own)
  file, only when a claim appears or changes machine and every few minutes for the refresh times, and is ignored if it
  is not a regular file of the hub's user that others cannot write. A fenced manager that MOVES to another machine
  needs its workers' specs updated (their record names the old machine, so `SendRemote` ends `dropped`).
- **Fenced to unfenced.** A relay reaches an unfenced target only if (a) that target's node is in the relay's
  `may_reach`, fixed at mint by root from the session's record (the manager, then the placed peers), and (b) a SPOKE principal tracked the entry. hostd also refuses a node held
  by more than one live entry (which of them is the manager is not for it to guess): `msg.dropped`.
- **A spoke principal's deliver** may reach its own machine's sessions and any FENCED session (the receiving
  hostd verifies and scopes it); focus, rename, drop and read stay on its own machine.
- **Minting hostd's credential** (once, as the hub admin, on the hub host; the file is root-owned, 0640, group
  ccsbx): `curl -X POST -H "authorization: Bearer $ADMIN" -d '{"role":"spoke","machine":"<machine>","name":"ccsbx","trees":["cc-sandbox"]}' http://127.0.0.1:4011/v1/principals`
  (`name` is only ever `ccsbx`; the id is `spoke:<machine>/ccsbx`). The same call without `name` mints the
  owner's `spoke:<machine>`. Without `trees` the hub drops every node and nothing can be scoped or addressed;
  hostd logs a warning when its own entries come back without one. Re-minting a principal replaces ITS old
  token only; the other principal of the machine is untouched.

### Moving a machine's owner sessions onto its spoke principal (deploy gate ❓ 2xq4: NOT deployed)
What changes: today every spoke and every reader (ccmsg, ccq, cc-pick, cc-tree, sb-hook) on a machine uses the
ONE shared secret. After the switch, that machine uses its own spoke principal token (`sbp1.…`, minted with
`trees:["cc-sandbox", ...]` for the trees whose nodes its sessions publish). Nothing in the hub changes mode:
it accepts both, so machines move one at a time. The shared secret stays for the admin API (minting) and as
the default until a machine opts in.
- **The opt-in is a file**, not a code path: `~/.config/session-bus/spoke-token` (mode 600; `$SB_SPOKE_TOKEN_FILE`
  overrides). When it exists and is non-empty, `sb_secret` (shell), `sb_auth.secret()` (python) and cc-tree's
  `hub_secret()` return it instead of the shared secret; an explicit `$SB_SECRET` still wins; an empty file
  falls back to the shared secret. The websocket spoke (the Elixir client) reads `SB_SECRET` from its service
  environment, so that env value is the second half of the switch.
- **Order, per machine** (do hp, check, then the Mac): (1) on the hub host mint the spoke credential for the
  machine (above); (2) put the token in `~/.config/session-bus/spoke-token` (600); (3) readers pick it up at once:
  check `ccq` and `ccmsg --help`-less dry run (`CCMSG_DRY_RUN=1 ccmsg <node> x`) still resolve nodes; (4) change
  the spoke service's `SB_SECRET` to the token and restart it: on hp `systemctl --user edit` the spoke unit's
  Environment, then `systemctl --user restart`; on the Mac edit the LaunchAgent plist's `SB_SECRET` and
  `launchctl kickstart -k`; (5) `ccq` shows the machine's sessions again under the new principal within one
  lease; (6) hostd gets its OWN principal, `spoke:<machine>/ccsbx` (minted with `"name":"ccsbx"`, above), in its own
  file (`--hub-token-file`, root:ccsbx 0640, the ONLY place that token lives: never in the owner's
  `spoke-token`, the spoke service's `SB_SECRET` or any owner-readable file; the hub refuses the owner's token
  on the ccsbx downlink and on fenced entries, and hostd's on everything else). Re-minting one principal
  replaces only its own token: update the files that hold THAT one (the owner's `spoke-token` and the spoke
  service's `SB_SECRET` for `spoke:<machine>`; hostd's token file for the ccsbx one) and restart those that
  cache it. Order matters only for hostd: until it has its own token, run it with `--hub-token-file` unset
  (no hub traffic) rather than with the owner's token, which the hub now refuses for it.
- **What keeps working**: nodes still resolve in `ccmsg` and cc-tree deliver (an entry from a spoke principal keeps
  its node when the tree is in its `trees`; a tree not listed is dropped, so mint with every tree you use).
  Standalone (no cc-sandbox) works either way: the flag is a file in the owner's config dir.
- **Rollback**: delete `~/.config/session-bus/spoke-token`, put the shared secret back in the spoke service's
  `SB_SECRET`, restart it; the readers go back to the shared secret at once. The principals can be revoked
  (`DELETE /v1/principals/spoke:<machine>`, `.../spoke:<machine>/ccsbx`) or simply left to expire (their default
  lifetime). Rolling back the owner side never touches hostd's: it keeps its own token and file; to switch hostd's hub
  traffic off, unset `--hub-token-file` and restart it.
- **Node bound.** The hub keeps a fenced entry's `node` only for the trees the machine's spoke principal was
  minted for (`trees`), so a fence cannot publish an owner's unrelated tree node.
- **Known limit: the public key is supplied by the hub.** hostd verifies the stamp against the `pub` the hub
  sends with the message, so it trusts the hub's routing and its key bookkeeping (a compromised hub could
  substitute a key and forge a stamp for any sender whose node the hub can also fake). Scope still bounds the
  damage to messages a target would accept from that node. The end-to-end chain (the hp-signed token carrying
  the sender's key, checked by the receiver) comes with U5/M9.
- **Sending.** A fenced session's message to a node that is not on this machine is signed by hostd with a
  per-generation key (in memory only); `ccsbx-root relay-mint` registers its public half as a relay principal
  (spec: docs/relay-mint-spec.md; until it exists, sending fails with "no relay credential" and is reported as
  `undeliverable`). A target the hub does not hold, or that did not take the message, is `dropped`: the sender's
  reply says `error/dropped` and the journal records `msg.dropped` (reason and node, never the text).
- **Not wired in the unit until:** the hub has B10/B11 and the downlink payload change (dotfiles `n1-g-hub`, a
  merge that is Tomas's), the spoke credential file exists, and `relay-mint` is installed.

## The daemon on the tailnet (M2a)

`ccsbx-hostd --tailnet-listen <this machine's tailnet ip>:7480 --tailnet-host <this node's MagicDNS name>
--owner-login <you> --owner-devices <FQDNs> --peer-devices <the other machines' FQDNs>`. For example on the Mac:
`--owner-devices mac-tomas.<tailnet>.ts.net --peer-devices hp-ubuntu.<tailnet>.ts.net,future-two.<tailnet>.ts.net`
(hp, the Mac and future-two are untagged devices of the owner's login; peers are listed DEVICES, not tags, for
Night 2). `--peer-tags` is empty by default and stays so until F4 (Night 3), when an in-cluster hostd comes
through a `tag:ccsbx-hostd` egress proxy; until then no cluster source gets :7480 at all. OFF by
default, and not turned on in the unit until the tailnet ACL (R4) and `ufw allow in on tailscale0 to any port
7480` exist. At start hostd refuses: an address that is not one of THIS node's own tailnet addresses (asked of
tailscaled, so another VPN's 100.64/10 address does not pass), a short device name (use `host.<tailnet>.ts.net`),
`--owner-login` without `--owner-devices` (and the reverse), a `--peer-tags` of `tag:ccsbx`, a `--tailscale-bin`
that is not absolute or is writable by group/other, a `--whois-socket` that is not a socket of root or the
daemon's user.

**Never `tag:ccsbx` as a peer tag.** It is the cluster operator proxies' tag, and a fenced pod can reach the
egress proxy (NetPol is per pod): a peer from `tag:ccsbx` would be a fenced pod (hostd refuses it at start). hp
and the Mac stay untagged user devices; later (F4) only a `tag:ccsbx-hostd` egress proxy used by the in-cluster
hostd gets :7480 (the ACL text for that is the k3s/D owners' to change: plan §5.2 grant 3 and k3s-amendments A7
still say `tag:ccsbx`). An
in-cluster hostd (F4) behind the operator's ingress would see the proxy pod's address, not the caller's, so
WhoIs-by-address cannot classify callers there: F4 must not use this listener.

Who a caller is comes from Tailscale WhoIs for the connection's own peer address, asked of tailscaled's
LocalAPI socket (hp) or `tailscale whois --json` (the Mac; `--whois cli|localapi|auto`, `--tailscale-bin`, an
absolute path resolved once at start, logged; on the Mac the App Store build's CLI talks to the GUI's LocalAPI:
check it answers `whois` for the service uid before relying on it); nothing the caller sends counts. Before
WhoIs is asked, a request must come from a tailnet address that is not one of this machine's own (a local
process connecting to the machine's tailnet address is the machine itself, never the owner), name this daemon in
`Host` (the listen address or `--tailnet-host`; DNS rebinding is refused), carry no `Origin` and no cross-site
`Sec-Fetch-Site` (a web page in the owner's browser), and a POST must be `Content-Type: application/json` (415).
The answer is believed only if the asked address is one of the node's own, and never for a node that advertises
subnet routes. **owner** = the owner's login on a node in `--owner-devices` (a tagged node is never the owner);
**peer** = a node tagged with a `--peer-tags` tag, or a `--peer-devices` node that carries the owner's login;
anything else, and any failed WhoIs, is 403. What each class may call is an explicit table (route, method) in
`internal/daemonapi/tailnet.go`; a route added to the API is closed on the tailnet until it is classified (a test
walks the mux). An owner gets the owner operations the local socket serves except session start (M2c adds the
remote form); a peer gets info, resolve and the session list. `/v1/info` shows the caller's class as
`caller_class`.

Answers: no cache for the LocalAPI; 5 s for the CLI, a failure remembered 2 s (still an error), one question per
peer at a time, at most 8 at once. A stream or long poll on the tailnet ends after 60 s (the client reconnects
and is classed again), at most 64 connections are open at once (`--tailnet-max-conns`), idle ones close after 2 minutes.

**Owner authority is ambient, so the tailnet gives the owner little** (SEC-20, ❓ cc-sandbox:#2dsc, the manager's
default until Tomas answers): any process on a LISTED owner device, whichever user runs it, would act as the
owner there with no secret (the loopback TCP port, by contrast, refuses deliver and steal even with its token).
So over the tailnet the owner gets only `info`, `resolve`, the session list and (M2c) session start. **deliver,
prompt, replace, the cockpit lease (steal) and the journal (events, streams, cursors, the cockpit's request queue)
are local-only**: 403 `forbidden` on the tailnet even to the owner, still served on the unix socket. What that
means for a cockpit on the OTHER machine: it cannot lease, stream events or type into this machine's panes over
the tailnet; it runs against its own machine's socket until M2d gives these a per-device secret. (`attach` is
tmux, not an API route.) hostd also warns at start when `--owner-devices` lists this machine.

## Attaching to a pod session (F5)

(Round 3: three users, none able to do another's job. `ccsbx-keys` is the signer's alone (the machine key,
the age key). `ccsbx-launch` (config `launch_user`) holds the launcher kubeconfig and token in its own 0700
directory, is the signer's `--keys-peer-user`, and runs kubectl for pod-create, pod-load, pod-stop, pod-status
and attach. `ccsbx-term` (`term_user`) has no credentials: after starting kubectl on a pty as the launch user,
`ccsbx-root attach` drops to it and serves the owner's terminal through `internal/termfilter`, which drops
OSC (titles, OSC 52 clipboard, hyperlinks), DCS/APC/PM/SOS, C1 controls and the queries that make a terminal
answer, because the pod's tmux runs as the agent and its bytes must not act on the owner's terminal. No tmux
on the host. The signer refuses a keys-peer equal to its own uid; ccsbx-root refuses a launch user that is the
signer's user (`signer_user`), the caller, the owner or the service user, and a terminal user that is any of
those. Only the owner, or root by login, may attach; the service user may not. The kubeconfig and token must
be unreadable by group and others. The docker attach also sets `set-clipboard` and `allow-passthrough` off.)

`ccsbx attach <sid>` is unchanged for the owner; `ccsbx-root attach` reads the session record, and when it
says `backend: "k8s"` it execs `kubectl exec -it -c fence <pod> -- tmux attach-session -t =<sid>:` as the
launch user (`ccsbx-launch`, round 3 below) instead of tmux as the service user. Config keys in `/etc/ccsbx/ccsbx-root.toml`
(defaults in brackets): `kubectl` [/usr/local/bin/kubectl, root-owned], `kubeconfig`
[/var/lib/ccsbx-launch/launcher.kubeconfig, owned by the launch user or root, mode without group/other write,
in a directory likewise], `k8s_conn` [/etc/ccsbx/k8s-conn.json, root-owned JSON with `server`, `ca_file`,
`token_file`; the server and CA are put on kubectl's command line, so the kubeconfig cannot redirect it],
`launch_user` [ccsbx-launch, never the signer's user, the service user or the caller], `term_user` [ccsbx-term], `signer_user` [ccsbx-keys], `k8s_namespace` [cc-sandbox]. Before kubectl
is exec'd the pod's live UID (read with the keys user's token) must equal the `pod_uid` the launcher recorded
when it created the pod; otherwise: `pod_conflict`, and the owner deletes exactly the named pod. The launcher
(not built yet) is what writes `backend` and `pod_uid`. Not run against a cluster (Gate M).

## Starting a pod session (F2w)

`ccsbx-hostd --root helper --pod-machines future-two` places specs for future-two as pods. hostd writes the
record (`backend: k8s`), runs `sudo -n ccsbx-root pod-create <sid>` (one pod per generation; prints the pod's
UID), writes `pod_uid` into the record, then `pod-load <sid>`: wait for both containers (config
`k8s_ready_seconds`, default 120, max 600; exit 4 = never ready, nothing minted), mint the token in the
keys-user process through the signer socket, send `{token,key}` to the sidecar on exec stdin (the live pod's
UID must equal the recorded one first). A failed start deletes the pod by name and UID; a stop
(`Daemon.StopPod`, generation expiry) does the same through `pod-stop`. Root config keys added: `k8s_machine`
(future-two), `k8s_ready_seconds`, `k8s_side_image` (required), `k8s_cpu`, `k8s_ephemeral_mb`,
`k8s_worktree_mb`, `k8s_token_hours` (8, max 12), `k8s_audiences`, `signer_socket`. The signer needs
`--keys-peer-user ccsbx-launch --keys-peer-machines future-two`: that uid may sign only for those machines and
only with a generation key, and they are closed to hostd's uid. Not built: resume of a pod session, the
git clone inside the pod (F3c), a hostd revoke client (`Daemon.Revoke` is nil in hostd), a pod-aware
`ccsbx stop`. Not run against a cluster (Gate M).

The sidecar has a `journal` volume of its own (emptyDir, 16Mi, disk-backed, sidecar only, mounted at
`/run/ccsbx/journal`), apart from the 8Mi `side-state` that holds the ledger.

## relay-mint (M2b, D2)

`sudo -n ccsbx-root relay-mint [--after-401] -- <sid> <gen> <pub>` (service user only; spec in
`docs/relay-mint-spec.md`). Root config keys: `hub_url` (default `http://127.0.0.1:4011`, loopback only, refused
otherwise) and `hub_secret` (default `/etc/ccsbx/hub-admin.secret`, root-owned, mode 0600, directory not
group/other-writable; the installer creates it from the hub's admin secret, it is never copied off hp). The
relay's `may_reach` is the record's `msg_scope.manager`, then its `peers`, each as `<machine>/<node>` (machine from
`manager.machine` / `peer_machines`; a node with no machine is left out; at most 32; empty when none is placed);
there is no argument for it. Exit 2: refused (not
the service user, stale or ended or not running or another machine's generation, rate, bad arguments), exit
3: the hub or the secret (status code only on stderr). One audit line per mint and revoke on stderr (the first
8 hex of sha256(pub), never a token or the secret). The relay is revoked (`DELETE /v1/principals/relay:<sid>%2F<gen>`)
at the end of `run`, in `stop` and in `gc` for each reaped generation, best effort; the hub's 6 h relay
lifetime is the backstop. Not run against a real hub (Gate M).

**Pod sessions and the relay.** `relay-mint` (the verb hostd calls) stays docker-only. For a pod session
`pod-load` does it itself: as root it reads `hub_secret` into memory (before the drop; the file stays root
0600), drops to the keys user, mints the session token and generation key, registers the relay for that
key's public half (`may_reach` from the record), and sends `{"token","key","relay_token"}` in the one
side-load payload. Nothing is printed and hostd sees none of it. A load that fails after the relay was
registered revokes it at once; `pod-stop` revokes it as root before the drop. A missing or untrusted hub
secret stops `pod-load` with exit 3 before anything is dropped or created in the pod.

Pod sessions also have `pod-status` (read-only; hostd's sweep asks it to end the record of a pod the owner
already stopped) and `ccsbx stop` works on them (root's `stop` deletes the pod by name and UID). hostd flags:
`--pod-ttl` (8h: a pod session older than this is stopped by the sweep), `--pod-sweep` (1m), `--pod-load-wait`.
Installer: two new users (`ccsbx-launch` with `/var/lib/ccsbx-launch` 0700 holding the kubeconfig and token;
`ccsbx-term` with no home), signer unit `--keys-peer-user ccsbx-launch --keys-peer-machines future-two`.

**Round 4 (names for the installer).** The signer's second socket for the launch user: `--keys-peer-socket PATH`
(env `CCSBX_SIGNER_KEYS_PEER_SOCKET`; no default in the signer; the installer passes `/run/ccsbx-signer/launch.sock`,
and under systemd a second socket unit with `SocketUser=ccsbx-keys SocketGroup=ccsbx-launch SocketMode=0660`
gives `LISTEN_FDS=2`, the main socket first). Each socket takes its own class only (SO_PEERCRED still exact).
`ccsbx-root.toml` `signer_socket` (what `pod-load` connects to as the launch user) now defaults to that path.
The hub listener check: `hub_user` (the user the hub runs as; no default, the installer writes it): before the
hub admin secret is sent (relay-mint, pod-load, the revokes), every LISTEN socket on `hub_url`'s port
(`/proc/net/tcp` and `tcp6`) must be that user's, else exit 3 and nothing is sent. Residual: the hub runs as
the owner's uid, whose processes can read the hub's environment (and the secret) anyway, so the admin secret's
trust boundary is that uid; this stops hostd and every other uid from catching it on a port the hub does not
hold; a listener that appears between the check and the connect is possible and no wider than a hub restart.
The terminal filter is sound for UTF-8 terminals only (it emits only complete, valid UTF-8 without C1
controls); the session runs with `LANG=C.UTF-8`.

Systemd sockets of the signer are picked **by name**, never by order: the main socket unit sets
`FileDescriptorName=main`, the keys peer's `FileDescriptorName=launch`, and the signer refuses to start
unless `LISTEN_FDNAMES` is exactly those two names (either order) with `LISTEN_FDS=2`.

**The sidecar's command line.** `pod-create` builds the sidecar's `ccsbx side serve ...` from the record and root's
config (`internal/k8s/side.go`): `--sid --gen --label --full-id --machine --scope` (the record's `msg_scope`,
compacted, an object of at most 8 KiB) and the fixed paths (`--control /run/ccsbx/side/ctl/ctl.sock`, `--state
/run/ccsbx/side/state`, `--journal-dir /run/ccsbx/journal/s`, `--out /run/ccsbx/out`, `--in /run/ccsbx/in`,
`--fence-uid 10001`, `--listen 127.0.0.1:8080`), plus `--hub-url` from root config `k8s_hub_url` (empty until the
operator egress Service exists, then the flag is left out) and one `--door NAME=URL` per entry of `k8s_doors`.
Every value is checked again before it goes in the pod spec, and the pod contract holds the sidecar to
`ccsbx side serve`. No secret is in the spec: the relay token travels in the one load. The `relay-cred` volume is gone.
