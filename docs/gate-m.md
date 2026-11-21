[Index](README.md)

# Gate M (#4x65): one sitting, copy and paste

What this is: the single document Tomas follows to do Gate M on hp in one sitting, in order, with the output to expect and what to do when it differs. It exists because the old list (`night-1-morning.md`) grew round by round and mixed what the fenced day on hp needs with what only pods need. Goal it serves (Tomas, 2026-11-21): **prove one real fenced day on hp before any new feature.** Anything the fenced day on hp does not need is in "LATER" at the end, with its commands, not dropped.

Written by D (cc-sandbox:#2f3e). **Nothing here has been run as root.** The expected outputs come from the code, the tests and the installer's own `plan`, not from a run on hp; where a line is a guess it says so. The installer's test suite (`tests/install-linux-test.sh`, no root) is the evidence for what the installer does.

Digests (built by Actions from main `dd26a78`, both packages public):

```
FENCE (--image):        ghcr.io/landovsky/ccsbx-fence@sha256:c0b52c95e75e26f13a06ac3acff4406d1e691c65955409e77c1847dc83518f1b
SIDECAR (--side-image): ghcr.io/landovsky/ccsbx@sha256:3374ecd87435c267a1d11982ca7f44c0364b2414e5618f918faec46f1c59bba8
```

Where: hp, in `~/git/cc-sandbox` on `main`, unless a step says "Mac". Keep ONE shell for the whole sitting (steps set variables). Keep a **second root shell** open from step 8 until the reboot in step 12.

## The steps and how long they take

| # | Step | Where | Rough time | Needs |
|---|---|---|---|---|
| 1 | The code is the code (S15) | hp | 1 min | |
| 2 | Digests and the anonymous ghcr pull (old 0/0b) | hp | 5 min | docker (still yours) |
| 3 | P15 closed | hp | 2 min | your six answers |
| 4 | Keep the `restart=no` containers | hp | 1 min | |
| 5 | Build the binaries | hp | 3 min | go |
| 6 | R-WT: one real fetch of a private repo | hp | 5 min | docker not needed, your git credentials |
| 7 | Root-only bind-mount test of `ccsbx-root` | hp | 3 min | sudo |
| 8 | Lockout guards (S9, S14) | hp | 3 min | sudo |
| 9 | Read the plan | hp | 5 min | |
| 10 | The root install | hp | 5 min | sudo |
| 11 | Seccomp profile and its docker test | hp | 5 min | sudo, docker |
| 12 | Reboot, then `check` | hp | 5 min | |
| 13 | `ccsbx-root worktree-check` smoke | hp | 3 min | sudo |
| 14 | One e2e start reaches `ccsbx-root run` | hp | 5 min | |
| 15 | future-two's ephemeral storage (each pod reserves 33 GiB) | Mac | 3 min | admin kubeconfig |

About 55 minutes if nothing fails (steps 1 to 15), plus the time to answer the P15 questions (step 3). The point of no return is step 10; steps 1 to 9 change nothing outside your home. Steps 13 and 14 are the gate's ACCEPT.

If any step fails and the text does not say what to do: stop, do not go on, and send the output to the manager. After step 10 the undo is `sudo units/install-linux.sh uninstall --yes` (restores the first backup; docker and lxd group membership are NOT re-granted on purpose).

## 1. The code is the code (S15) · 1 min

```
cd ~/git/cc-sandbox && git status --short | grep -v '^??' ; git log --oneline -1
```

Expect: no lines from `git status` (untracked `??` files are fine), then one line: the merged commit you mean to install (`dd26a78` or later). If tracked files are modified: `git stash` or commit them, then re-run. If the commit is not the one you reviewed: stop.

Why: the installer copies binaries built from this tree, as root. Later commits are fine for the install; they only matter for the sidecar digest (step 2).

## 2. Digests and the anonymous pull (old steps 0 and 0b) · 5 min

```
FENCE=ghcr.io/landovsky/ccsbx-fence@sha256:c0b52c95e75e26f13a06ac3acff4406d1e691c65955409e77c1847dc83518f1b
SIDE=ghcr.io/landovsky/ccsbx@sha256:3374ecd87435c267a1d11982ca7f44c0364b2414e5618f918faec46f1c59bba8
[[ "$FENCE" =~ ^ghcr\.io/landovsky/ccsbx-fence@sha256:[0-9a-f]{64}$ && "$SIDE" =~ ^ghcr\.io/landovsky/ccsbx@sha256:[0-9a-f]{64}$ ]] && echo "digests ok"
docker logout ghcr.io        # drops any ghcr login you use for other pulls; log in again afterwards if you need one
docker pull "$FENCE"
docker pull "$SIDE"
```

Expect: `digests ok`; `docker logout` says `Removing login credentials for ghcr.io` or `Not logged in to ghcr.io` (either is fine: the pull must be anonymous, this is the first one ever); each pull ends with `Status: Downloaded newer image for ghcr.io/landovsky/...@sha256:...` (or `Image is up to date`) and a `Digest: sha256:...` line equal to the digest you pulled.

If a pull says `unauthorized`, `denied` or `manifest unknown`:
- `unauthorized` / `denied`: the package is not public. GitHub → your profile → Packages → `ccsbx-fence` (or `ccsbx`) → Package settings → Change visibility → Public, then pull again. (Fallback: `docker login ghcr.io -u <your github user>` with a token that has `read:packages`.)
- `manifest unknown`: the digest is not in the registry (a typo, or the package was deleted). Do not guess: get the digest from the Actions run's `digest.txt` (`gh run list --repo landovsky/cc-sandbox --workflow fence-image.yml --branch main --status success --limit 1`, then `gh run download <id> --repo landovsky/cc-sandbox --name digest --dir /tmp/fence-digest`) and set `FENCE` from it. Same for `side-image.yml`.

Do not go on until both pulls work: the fence pull must work before the reboot, because docker is gone for you after it, and step 14 runs the fence from this image. The sidecar image is only pulled by the cluster; pulling it here proves the package is reachable. Note: the sidecar is the `ccsbx` binary, so its digest changes with every code merge; a digest from an older main still works, it just is not the newest build.

## 3. P15 closed · 2 min (plus your answers)

```
units/p15 status
units/p15 check; echo "exit=$?"
```

Expect at the end: `p15: every row is done or dropped` and `exit=0`. Today `check` prints `24 row(s) not done or dropped: ...` and exits 1; the install refuses until it exits 0. The six questions and the recommendations are in `docs/night-1-hitl.md` §2; once you have answered, the rows are closed with `units/p15 mark ID done "note"` (or `dropped`), by D or by you. If `check` still lists rows, close them (`units/p15 status` shows which).

## 4. Keep the `restart=no` containers · 1 min

The reboot kills every container with restart policy `no` (`messaging-hub-db-1` and the worktree `redis-*`). Per the P15 recommendation:

```
docker update --restart unless-stopped messaging-hub-db-1
docker inspect -f '{{.Name}} {{.HostConfig.RestartPolicy.Name}}' messaging-hub-db-1
```

Expect: `/messaging-hub-db-1 unless-stopped`. Skip if your P15 answer was different. The hub (`session-bus`) is the docker container and is not a Gate M dependency (decision 2026-11-21): it keeps running after the reboot (its restart policy is `always`, per the P15 inventory), but you are out of the docker group and manage it with `sudo docker` from then on.

## 5. Build the binaries · 3 min

Build now, with no agents running, so nothing can swap a file between the checksum and the copy (S5); step 6 uses `dist/ccsbx` from this build, and the install (step 10) copies the same `dist/`. If you change anything in the tree after this step, run it again before step 10. `ccsbx` is on no PATH until the install puts it in `/usr/local/bin`, which is why the steps before that call `dist/ccsbx`.

```
make dist-linux GO=$HOME/.local/go/bin/go
cat dist/SHA256SUMS
```

Expect: four lines, one each for `ccsbx`, `ccsbx-root`, `ccsbx-hostd`, `ccsbx-signer`, then `dist-linux: SHA256SUMS lists only what this run built...`. The file sits next to the binaries, so it guards against accidents only: compare the sums with a build you trust (the CI build, or the same command on the Mac) before the install. If `go` fails, the error is the go toolchain's; fix and re-run. If a `cmd/<name> does not exist` line appears, you are not on the right commit (step 1).

## 6. R-WT: one real fetch of a private repo · 5 min

What it proves: the daemon builds a worktree from `{repo, base, branch}` by fetching with YOUR credentials (nothing of them reaches the fence). **This runs BEFORE the install on purpose:** the repo form only works in DIRECT mode (see "Known gap" below), direct mode needs docker, and the install removes your docker group. `--launcher none` writes the record and builds the worktree but starts no container.

Pick a private repo you can fetch (the check that you can: `GIT_TERMINAL_PROMPT=0 git ls-remote https://github.com/<owner>/<repo>.git HEAD` prints a hash; if it asks for a password or fails, fix your git credentials first, e.g. `gh auth setup-git`, because the daemon uses exactly your configuration).

```
REPO=github.com/<owner>/<private-repo>        # e.g. one of your private repos
BASE=main                                     # a branch that exists in it
cat > /tmp/gatem-rwt.json <<EOF
{"v":1,"request_id":"lid-gatem-rwt-1","owner":"tomas","machine":"hp-ubuntu","label":"cc-sandbox:gatem-rwt",
 "realm":"production","resume":null,"harness":"claude","harness_args":["--model","sonnet"],
 "workdir":{"repo":"$REPO","base":"$BASE","branch":"gatem-rwt-smoke"},
 "profile":"e2e-default","billing":"stub",
 "msg_scope":{"manager":{"node":"cc-sandbox:2f3e"},"peers":[],"reply_window_s":86400},
 "client":{"lid":"lid-gatem-rwt-1","role":"worker"},
 "brief":{"text":"say hi"}}
EOF
mkdir -p ~/wt
dist/ccsbx start --root direct --launcher none --image "$FENCE" --worktree-root "$HOME/wt" --spec /tmp/gatem-rwt.json --json; echo "exit=$?"
```

Expect: a StartReply on stdout with a `sid` (and `not_evidence`: true, because direct mode is not evidence), then `exit=0`; it takes a few seconds to a minute (the fetch has a 120 s bound). Then:

```
ls -dt ~/wt/*gatem-rwt* | head -1
git -C "$(ls -dt ~/wt/*gatem-rwt* | head -1)" log --oneline -1
find ~/.local/state/ccsbx/clones -maxdepth 3 -name '*.git'
```

Expect: a directory like `~/wt/cc-sandbox-gatem-rwt-<6hex>` (the label slug and 6 hex digits), the last commit of the base branch, and a bare clone `.../clones/tomas/<github-owner>/<repo>.git`. **PASS** = all three exist and the commit is the tip of `$BASE`.

If it fails:
- `ccsbx start: refused: bad_spec` with `the repository or branch could not be fetched`: your credentials cannot fetch it, or the branch does not exist. Repeat the `ls-remote` check above. The text never names the repo or git's own message, on purpose.
- `bad_spec` for the worktree root: `~/wt` must exist and be a real directory (`mkdir -p ~/wt`).
- `machine_full`, `rate_capped`, `profile_not_approved`: read the code in the message; none of these is expected on a fresh state directory.
- It hangs more than two minutes: the fetch bound is 120 s, so something else is wrong; press Ctrl-C and send the output to the manager.

To repeat the step, change `request_id`, `lid` and `branch` in the spec first (the same request id answers `already_running`), or clean up below first. Git access: the daemon fetches over `https://github.com/...` with the credential helper of YOUR git configuration (no prompt is possible); an ssh-only setup works only if its `url.insteadOf` is in your global git config.

Clean up (the clone stays as the cache; only when no session uses this repo): `git -C <the clone from above> worktree remove --force <the ~/wt directory>` and `git -C <the clone> branch -D gatem-rwt-smoke`. The record it wrote is in your own direct-mode state directory (`~/.local/state/ccsbx`), not hostd's, and is harmless.

**Known gap (not a Gate M failure):** after the install your `ccsbx` talks to hostd (helper mode), and in helper mode, and in pods, the repo form answers `not_built`. Only `workdir.path` (an existing worktree under `~/git` or `~/wt`) starts there. This does not matter for C25 (the typical-day session) as long as C25 starts by `workdir.path`, which `tests/specs/e2e-hello.json` does; if C25 is changed to start by repo form it cannot run on the installed hp until a helper-mode client builds the worktree (not planned).

## 7. Root-only bind-mount test of `ccsbx-root` · 3 min

```
~/.local/go/bin/go version
sudo unshare -m env GOTOOLCHAIN=local /home/tomas/.local/go/bin/go test -tags rootmount -run TestRealMounter ./internal/rootcmd
```

Expect: `go version go1.27.x linux/amd64`, then (after the sudo password) `ok  github.com/landovsky/cc-sandbox/internal/rootcmd ...`. This builds and runs your source tree as root (its build cache goes to root's home; nothing is written into the repo). **It must PASS; if it does not, stop here** (the failure is in `ccsbx-root`'s mount code and must be fixed before it runs as root). The full path to `go` is used because go is not on sudo's PATH, and `GOTOOLCHAIN=local` stops it fetching a toolchain as root.

## 8. Lockout guards (S9, S14) · 3 min

The installer removes the NOPASSWD grants for docker, apt, dpkg, snap, dokku and sops from `/etc/sudoers.d/*`. Make sure you cannot lock yourself out:

```
sudo grep -n NOPASSWD /etc/sudoers
sudo -k; sudo -v
sudo -l
```

Expect: the first prints nothing (or only lines you know); a NOPASSWD line in `/etc/sudoers` itself would survive the installer, which cleans only `/etc/sudoers.d/*`: remove it by hand with `sudo visudo` if it is for docker, apt, dpkg, snap, dokku or sops. The second MUST ask for your password. The third shows `(ALL : ALL) ALL` (with the password), and BEFORE the install it still lists NOPASSWD lines for docker, apt-get, dokku and sops: that is what step 10 strips, so seeing them now is normal. What is NOT normal: a `NOPASSWD: ALL`, a command alias, an `env` or `sudoedit` entry in a NOPASSWD line, or a digest: the installer's stripper refuses to guess those and stops at install step [08] (exit 3, `PARTIAL INSTALL`); fix that line by hand first. If the password prompt or `(ALL : ALL) ALL` is missing: stop. **Then open a second terminal and run `sudo -s` in it; keep that root shell open until the reboot in step 12** (it does not survive the reboot; after the reboot your normal `sudo` is the way back in, and `check` in step 12 is what proves it).

## 9. Read the plan · 5 min

```
units/install-linux.sh plan --image "$FENCE" --side-image "$SIDE" | less
```

Expect: a header, then `GATES (install refuses while any is unmet)` and, if everything above is done, **no `UNMET` line**; then 10 numbered steps `[01]` to `[10]` ending with `AFTER: reboot, then units/install-linux.sh check ...` and `UNDO: ...`. Today it shows `UNMET: P15 open: 24 row(s)...` and `UNMET: binaries missing in .../dist` because steps 3 and 5 are not done; after them both are gone. `note: sudoers.d/... not readable without root: verified at install` lines are normal. Also normal: `# no --hub-url: the install does not depend on the hub` and `# B9 skipped`.

What the ten steps do, in one line each: [01] users `ccsbx`, `ccsbx-keys` (signer only), `ccsbx-launch`, `ccsbx-term`, group `ccsbx-clients`; [02] binaries, `/etc/ccsbx/ccsbx-root.toml` (verbs, roots, both images' settings), pins; [03] machine and age keys, the signer's scope; [04] signer sockets and service; [05] tmux unit, run dir, hostd unit (loopback only, no firewall rule); [06] sops root-owned; [07] the lxd snap and socket removed; [08] sudoers (one NOPASSWD rule, for `ccsbx-root`; the rest stripped, the whole config re-checked, automatic restore if it does not parse); [09] you out of lxd, then docker LAST; [10] post-install check. If the plan shows anything you do not understand, stop; add `-v` to see the files it would write.

## 10. The root install · 5 min

```
sudo units/install-linux.sh install --yes --image "$FENCE" --side-image "$SIDE"
```

(Your shell expands `$FENCE` and `$SIDE` before `sudo` runs, so use the same shell as step 2; they are not exported. Do NOT add `--hub-url` or `--hub-secret-from`: the install does not depend on the hub. Do NOT add `--revoke-doors`: there are no doors before M1. Do NOT add `--with-deploy` today: it needs `dist/ccsbx-deploy`, which `make dist-linux` does not build, and the installer would skip the copy silently.)

Expect: `install-linux: all gates met; the first backup goes to /var/backups/ccsbx-install/original`, ten numbered steps with `$ ...` command lines, and at the end `DONE. NOW: reboot. Then: units/install-linux.sh check`. `DONE` appears only after hostd is up and the signer has signed a test claim.

If it stops:
- `install-linux: REFUSED: N gate(s) unmet; nothing was changed`: nothing happened; read the `UNMET` lines (same as the plan) and fix them.
- `PARTIAL INSTALL ...`: something failed after the backup. The message names the step and the undo. Read the cause, then either fix it and re-run the same command (it is idempotent) or run `sudo units/install-linux.sh uninstall --yes`.
- `NOT DONE` with `journalctl -u ccsbx-hostd -u ccsbx-signer`: a unit crash-loops. Run that `journalctl` and send the last 30 lines to the manager before anything else.
- A sudoers message ending `restored N file(s) from the first backup`: the new rules did not parse, the files are back as they were; nothing is lost; send the message to the manager.

## 11. Seccomp profile and its docker test · 5 min

Before the reboot (it needs `/etc/ccsbx` from step 10 and docker, which this shell still has):

```
V=$(docker version --format '{{.Server.Version}}')
curl -fsSL -o default.json https://raw.githubusercontent.com/moby/moby/v$V/profiles/seccomp/default.json
ccsbx seccomp-profile --base default.json --out /tmp/seccomp.json
sudo install -o root -g root -m 0644 /tmp/seccomp.json /etc/ccsbx/seccomp.json
ccsbx seccomp-profile --verify /etc/ccsbx/seccomp.json
tests/seccomp-setuid-test.sh --base default.json --image "$FENCE" --ccsbx /usr/local/bin/ccsbx
```

Expect: `--verify` prints `ok` (exit 0) and the docker test prints `ok` lines and exits 0 (it runs the fence image and tries to create a setuid file; it must fail to). Both must pass. If the `curl` 404s, your docker server version has no tag with that name in moby: use the nearest release tag and note which. Do not reboot before this passes: `check` verifies the profile again. (`default.json` stays in `~/git/cc-sandbox`; do not commit it.)

## 12. Reboot, then `check` · 5 min

```
sudo reboot
```

After it comes back, open tmux again, `cd ~/git/cc-sandbox`, and:

```
units/install-linux.sh check; echo "exit=$?"
```

Expect: lines `ok    ...`, **no `FAIL`**, and `install-linux check: complete` with `exit=0`. Some lines start with `note` (for example files you cannot look into as a normal user, or "ccsbx-root.toml has no hub_url: this install does not depend on the hub"): those are fine. Run it once more as root for the lines it cannot test as you: `sudo units/install-linux.sh check` (it asks the signer for a token and connects to the sockets as the other users).

If a line says `FAIL`: the text names it. The common ones: `tmux (pid N) still has group docker: reboot done?` (you did not reboot, or an old tmux survived: `tmux kill-server` from a fresh login, or reboot again); `ccsbx-hostd is active` (look at `journalctl -u ccsbx-hostd`); `the seccomp profile verifies` (step 11 again). Do not go on with a FAIL: steps 13 and 14 assume a complete check. (The second root shell from step 8 died at the reboot; `sudo -v` in a normal shell is how you check you still have root.)

## 13. `ccsbx-root worktree-check` smoke · 3 min

hostd asks root this question before it accepts a remote start in helper mode. Run it the way hostd does (as the service user `ccsbx`, through sudo):

```
sudo -u ccsbx -- sudo -n /usr/local/sbin/ccsbx-root worktree-check -- /home/tomas/git/cc-sandbox; echo "exit=$?"
sudo -u ccsbx -- sudo -n /usr/local/sbin/ccsbx-root worktree-check -- /tmp; echo "exit=$?"
```

Expect, first command (the password prompt is for the outer `sudo -u ccsbx`):

```
/home/tomas/git/cc-sandbox
/home/tomas/git/cc-sandbox/.git
exit=0
```

Two lines (the resolved path, then its gitdir), exit 0. Second command: exit 2 and one fixed line on stderr that names neither the path nor the OS error. **PASS** = both. Run this BEFORE step 14: step 14's session uses `~/git/cc-sandbox` as its workdir, and a path that overlaps a live session is refused (exit 2) by design.

If the first command says the verb is not enabled (`verb ... not enabled` or similar) your `ccsbx-root.toml` has no `worktree-check` in `verbs` (an installer from before this fix): add it with `sudoedit /etc/ccsbx/ccsbx-root.toml` (the `verbs = [...]` line) or re-run the install of this commit. If it says `sudo: a password is required`: the sudoers rule for `ccsbx` is missing, run `sudo cat /etc/sudoers.d/90-ccsbx-root` and send it to the manager. Exit 3 means "could not tell" (unreadable record or config): send the stderr line.

## 14. One e2e start reaches `ccsbx-root run` · 5 min (the ACCEPT for the start path)

```
ccsbx start --spec tests/specs/e2e-hello.json --json; echo "exit=$?"
```

**Step A, hostd accepts:** expect a StartReply with a `sid` and `exit=0`. An exit 2 prints `ccsbx start: refused: <code>`:
- `image_not_pinned`: hostd's `--image` is a tag or wrong; check `systemctl cat ccsbx-hostd | grep -o -- '--image [^ ]*'` equals `$FENCE`.
- `profile_not_approved`: the profile is not approved for this machine and billing.
- `wrong_machine`: the machine name differs between the spec, hostd and `ccsbx-root.toml` (`hostname -s`, lowercase).
- `bad_spec`: for example a workdir outside `--worktree-root`.
- `rate_capped`, `machine_full`, `already_running`: expected only on a repeat; wait or stop the old session.
- `ccsbx start: ccsbx-hostd is not running or not reachable (socket /run/ccsbx-api/hostd.sock)`: `systemctl status ccsbx-hostd`; you are in group `ccsbx-clients` only after a fresh login (the reboot gave you that).

**Step B, `ccsbx-root` accepts:** the pane runs `sudo -n ccsbx-root run --tty -- <sid>` in its own tmux. If `run` refuses, it exits at once and that tmux session dies, so the refusal text is gone from the pane. Therefore:

```
SID=<the sid from the reply>
sudo ccsbx-root attach "$SID"
```

- **PASS** = attach shows the pane (the fence is running, or the pane ends with docker's own error `ccsbx-root run: ...` without `refused:`, exit 1, which still means every check passed).
- **FAIL = attach fails** (for example "no such session" / `session has ended`): the run was refused before you could attach. That failure IS the signal. Read the reason: `sudo journalctl -u ccsbx-hostd -n 50 --no-pager` (hostd logs the launch), and run the same command by hand from a real terminal to see the refusal text: `sudo -u ccsbx -- sudo -n /usr/local/sbin/ccsbx-root run --tty -- "$SID"` (it only repeats the refusal; if it starts the fence instead, press Ctrl-C and `ccsbx stop "$SID"`). The refusal lines to look for: `ccsbx-root run: refused: ` followed by `pins: ...` (the image is not an acceptable digest: steps 2 and 10), `profile <p> is not approved for machine <m> with billing <b>`, `session <sid> is for machine "<a>", this is "<b>"`, `this process is not in the host's mount namespace` (a sandboxed unit: `ccsbx-tmux` must be the host's), `seccomp profile ...` (step 11), `session <sid> has ended`, or `must run as root`.
- **Leaving the pane:** `ccsbx-root attach` removes the inner tmux's keys (no prefix, nothing bound), so there are no detach keys in the pane. Run attach inside your OUTER tmux; leave with the outer prefix and `d`, or close that window. Then `ccsbx stop "$SID"` from another window.

**Gate M is done when steps 12, 13 and 14 pass.** Then say `Arm!` in the manager tab (the milestone is NOT ARMED until you do).

## 15. future-two's ephemeral storage (each pod reserves 33 GiB) · 3 min · on the Mac

Each pod session is Guaranteed: the fence container 32 GiB (`k8s_ephemeral_mb` 32768) plus the sidecar 1 GiB, 33792 MiB = **33 GiB per pod**. hostd's `--pod-max-live` is 4, so up to **132 GiB** can be reserved on future-two at once (8 would be 264 GiB). The node must have that free, or pods stay Pending. Read-only, with the admin kubeconfig (the launcher account cannot read nodes):

```
kubectl get node future-two -o jsonpath='{.status.allocatable.ephemeral-storage}' | python3 -c "import sys; print(round(int(sys.stdin.read())/2**30,1), 'GiB allocatable')"
kubectl describe node future-two | sed -n '/Allocated resources/,/Events/p'
```

Expect: a number of GiB allocatable, and under `Allocated resources` a line `ephemeral-storage  <requests> (..%)  <limits> (..%)`. **PASS** = allocatable minus the ephemeral-storage REQUESTS already there is at least **132 GiB** (for `--pod-max-live` 4). Rule for other values: usable pods = floor(free GiB / 33).

If it is less: lower the cap on hp so hostd refuses with `machine_full` before the cluster does. On hp:

```
sudo systemctl edit ccsbx-hostd.service      # add:  [Service]  then  Environment=CCSBX_POD_MAX_LIVE=2   (floor(free/33))
sudo systemctl restart ccsbx-hostd.service
```

(Every hostd flag also reads `CCSBX_<FLAG>`; a re-run of the installer does not remove a drop-in.) Not needed for the docker-only fenced day: this only decides how many pod sessions future-two can hold, so do it now because it is one command, not because step 14 needs it.

## Known gaps (not failures)

- **Repo form in helper mode and pods is `not_built`** (step 6). Starts by `workdir.path` work everywhere.
- **The hub is still the docker container.** relay-mint and pod relays refuse cleanly (no hub secret, no `hub_url`); fenced-to-unfenced-manager messages wait for the hub spoke. After the reboot you manage the hub container with `sudo docker`.
- **No doors before M1:** `revoke_doors` is empty, so pod tokens are not revoked at the end of a generation; they expire by themselves.
- Nothing in this document starts a pod: a pod needs the launcher kubeconfig (LATER, first item).

## LATER (not needed for the fenced day on hp; kept, not dropped)

Order matters among these. Every item says what it needs.

1. **k3s launcher check 5** (hp only, as `ccsbx-keys`): needed before any pod session. The block is in `docs/night-1-morning.md` ("Step 11: launcher check 5"), verbatim from the k3s side; afterwards move the kubeconfig to `ccsbx-launch` as its "Round 3" note says (`sudo install -m 0600 -o ccsbx-launch -g ccsbx-launch /var/lib/ccsbx-keys/launcher.kubeconfig /var/lib/ccsbx-launch/launcher.kubeconfig`, then `sudo rm /var/lib/ccsbx-keys/launcher.kubeconfig`). Paste the output to the manager, who passes it to k3s.
2. **The tailnet ACL paste** (old step 11b; before any SHARED merge, because the operator needs the tag): k3s PR https://github.com/landovsky/k3s/pull/21 ("drop cluster -> :7480 grants"; the Gate M paste is only `tag:ccsbx` → hp-ubuntu tcp:4011 and hp/Mac → `tag:ccsbx` tcp:443; the `tag:ccsbx-hostd` :7480 grant comes with F4). On the Mac: merge it, `cd ~/git/k3s && git pull`, `scripts/tailscale-acl-test lint`, `scripts/tailscale-acl-test baseline save`, open https://login.tailscale.com/admin/acls, paste the whole `artifacts/tailscale-config.json`, Save, `scripts/tailscale-acl-test compare` (expect no new FAILs), report the compare output to devops:#6c44. Check afterwards: `bin/ccsbx-preflight --morning` C-R4 (`tag:ccsbx` in `tailscale status`, once the proxies exist after item 3).
3. **The SHARED merges** (old step 12, M8/M1): k3s PR https://github.com/landovsky/k3s/pull/20 (doors and hostd manifests, replicas 0). It says **DO NOT MERGE until digests set** (the door images carry an all-zero placeholder digest) and M1 later sets `replicas: 1`; merging before that leaves the Flux Kustomization not-Ready. When the k3s side says it is ready: `gh pr merge 20 --repo landovsky/k3s`, then on the Mac `flux get kustomization cc-sandbox` (expect Ready). The in-cluster hostd is F4 (Night 3).
4. **Everything green** (old step 13): `bin/ccsbx-preflight --morning --post-M1` with `CCSBX_DOORS_MODEL_URL` and `CCSBX_DOORS_EGRESS_URL` set. Needs the doors (item 3 and M1).
5. **The report** (old step 14): `bin/ccsbx-night-report --dry-run`, then `--send`.
6. **The hub's deploy gate** (Tomas ❓ 2xq4): the hub becomes a native release in the dotfiles deploy; then set `SB_NODE_CLAIMS_FILE=$HOME/.local/state/session-bus/node-claims/claims.json` in its unit, re-run the install WITH `--hub-url http://<the hub's tailnet address>:4011` (the full B9 gate and the listener rule come back), and run `sudo units/hub-probe --owner "$USER" --hub <the same url>`: expect `POST 201`, `DELETE 200`, `hub secret OK`; 401 or 403 = the secret does not match, stop. Full steps: `docs/night-1-morning.md`, "The hub's deploy gate".
7. **`--revoke-doors`** at M1 (re-run the install with the doors' owner-listener URLs).
8. **Refresh the image digests after a later push of main** (the sidecar changes with every code merge): push main, watch the two Actions runs (`fence-image`, `side-image`), read both new `digest.txt`, and re-run the install with the new values.

Background and the reasoning behind each installer step: `docs/night-1-morning.md` (kept as reference; this document is the sequence), `docs/runbook.md` §2 (rollback) and §3.
